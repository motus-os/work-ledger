package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testEpoch = time.Date(2026, 7, 21, 12, 0, 0, 123456789, time.UTC)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	ledger, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ledger.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return ledger
}

func downgradeStoreToLegacySchema(t *testing.T, ledger *Store) {
	t.Helper()
	additions := schemaAdditionsSince(legacySchemaVersion)
	for index := len(additions) - 1; index >= 0; index-- {
		fields := strings.Fields(additions[index])
		if len(fields) < 3 {
			t.Fatalf("invalid schema addition %q", additions[index])
		}
		if _, err := ledger.db.Exec(`DROP ` + fields[1] + ` ` + fields[2]); err != nil {
			t.Fatalf("drop schema addition %s: %v", fields[2], err)
		}
	}
	if _, err := ledger.db.Exec(`DROP TRIGGER metadata_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`UPDATE metadata SET value = '1' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(schemaStatementByName("metadata_no_update")); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := validateSchemaVersion(context.Background(), ledger.db, legacySchemaVersion,
		schemaStatementsForVersion(legacySchemaVersion)); err != nil {
		t.Fatalf("legacy schema fixture = %v", err)
	}
}

func createLegacyStoreWithRun(t *testing.T) (string, string, []byte) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	ledger, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_legacy_upgrade"
	startTestRun(t, ledger, runID)
	closeTestRun(t, ledger, runID)
	receipt, err := ledger.ProjectReceipt(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	downgradeStoreToLegacySchema(t, ledger)
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	return stateDir, runID, receipt.Bytes()
}

func assertLegacySchemaNotPartiallyMigrated(t *testing.T, database string) {
	t.Helper()
	db, err := sql.Open("sqlite3", sqliteDSN(database, true, defaultBusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var userVersion int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil || userVersion != legacySchemaVersion {
		t.Fatalf("failed migration changed user_version = %d, %v", userVersion, err)
	}
	var findingTables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('findings', 'finding_closures')`).Scan(&findingTables); err != nil || findingTables != 0 {
		t.Fatalf("failed migration created finding tables = %d, %v", findingTables, err)
	}
}

func startTestRun(t *testing.T, ledger *Store, id string) Run {
	t.Helper()
	dirty := true
	run, err := ledger.StartRun(context.Background(), StartRunParams{
		ID:                 id,
		EventID:            id + "_started",
		StartedAt:          testEpoch,
		Source:             "motus-wrap",
		Producer:           "motus",
		ExecutableBasename: "printf",
		ArgumentCount:      2,
		Git: GitMetadata{
			Repository: "/work/project",
			Commit:     "0123456789abcdef0123456789abcdef01234567",
			Dirty:      &dirty,
		},
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	return run
}

func TestLifecycleRoundTripAndIdempotency(t *testing.T) {
	ledger := openTestStore(t)
	ledger.now = func() time.Time { return testEpoch.Add(time.Second) }

	started := startTestRun(t, ledger, "run_01")
	if started.State != RunOpen || started.ID != "run_01" {
		t.Fatalf("StartRun() = %#v", started)
	}
	startedAgain := startTestRun(t, ledger, "run_01")
	if startedAgain.ID != started.ID || !startedAgain.CreatedAt.Equal(started.CreatedAt) {
		t.Fatalf("idempotent StartRun() changed run: first=%#v second=%#v", started, startedAgain)
	}

	exitCode := 0
	timedOut := false
	closeParams := CloseRunParams{
		EventID:  "event_terminal_01",
		EndedAt:  testEpoch.Add(2 * time.Second),
		Outcome:  OutcomeSuccess,
		ExitCode: &exitCode,
		TimedOut: &timedOut,
		Output:   OutputCounts{StdoutBytes: 6, StdoutLines: 1},
	}
	closed, err := ledger.CloseRun(context.Background(), started.ID, closeParams)
	if err != nil {
		t.Fatalf("CloseRun() error = %v", err)
	}
	if closed.Run.State != RunClosed || closed.Run.Outcome != OutcomeSuccess ||
		closed.TerminalEvent.Sequence != 2 || !closed.TerminalEvent.Terminal {
		t.Fatalf("CloseRun() = %#v", closed)
	}
	closedAgain, err := ledger.CloseRun(context.Background(), started.ID, closeParams)
	if err != nil || closedAgain.TerminalEvent.ID != closed.TerminalEvent.ID {
		t.Fatalf("idempotent CloseRun() = %#v, %v", closedAgain, err)
	}

	snapshot, err := ledger.Snapshot(context.Background(), started.ID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(snapshot.Events) != 2 || snapshot.Events[0].ID != "run_01_started" || snapshot.Events[1].ID != "event_terminal_01" {
		t.Fatalf("Snapshot() events = %#v", snapshot.Events)
	}
	runs, err := ledger.ListRuns(context.Background(), ListRunsOptions{})
	if err != nil || len(runs) != 1 || runs[0].ID != started.ID {
		t.Fatalf("ListRuns() = %#v, %v", runs, err)
	}
}

func TestOpenReadOnlyDoesNotCreateAndCannotWrite(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenReadOnly(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenReadOnly(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenReadOnly created state directory: %v", err)
	}
	if _, err := OpenForRead(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenForRead(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenForRead created state directory: %v", err)
	}

	stateDir := filepath.Join(t.TempDir(), "state")
	writable, err := Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	readOnly, err := OpenReadOnly(ctx, stateDir)
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	defer readOnly.Close()
	_, err = readOnly.StartRun(ctx, StartRunParams{
		ID: "run_ro", EventID: "run_ro_started", StartedAt: testEpoch, Source: "test", Producer: "test",
		ExecutableBasename: "true",
	})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only StartRun() error = %v, want ErrReadOnly", err)
	}
	forRead, err := OpenForRead(ctx, stateDir)
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	defer forRead.Close()
	_, err = forRead.StartRun(ctx, StartRunParams{
		ID: "run_for_read", EventID: "run_for_read_started", StartedAt: testEpoch, Source: "test", Producer: "test",
		ExecutableBasename: "true",
	})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("OpenForRead current schema StartRun() error = %v, want ErrReadOnly", err)
	}
}

func TestOpenForReadMigratesLegacySchemaWithoutChangingReceipts(t *testing.T) {
	ctx := context.Background()
	stateDir, runID, receiptBefore := createLegacyStoreWithRun(t)
	if _, err := OpenReadOnly(ctx, stateDir); !errors.Is(err, errSchemaMigrationRequired) {
		t.Fatalf("OpenReadOnly(legacy) error = %v, want migration required", err)
	}

	ledger, err := OpenForRead(ctx, stateDir)
	if err != nil {
		t.Fatalf("OpenForRead(legacy) error = %v", err)
	}
	defer ledger.Close()
	receiptAfter, err := ledger.ProjectReceipt(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(receiptBefore, receiptAfter.Bytes()) {
		t.Fatal("schema migration changed the existing run receipt")
	}
	var userVersion int
	if err := ledger.db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil || userVersion != SchemaVersion {
		t.Fatalf("user_version = %d, %v", userVersion, err)
	}
	var metadataVersion string
	if err := ledger.db.QueryRow(`SELECT value FROM metadata WHERE key = 'schema_version'`).Scan(&metadataVersion); err != nil || metadataVersion != strconv.Itoa(SchemaVersion) {
		t.Fatalf("metadata schema version = %q, %v", metadataVersion, err)
	}
	report, err := ledger.Doctor(ctx)
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor() after migration = %#v, %v", report, err)
	}
}

func TestConcurrentLegacyMigrationIsSerialized(t *testing.T) {
	ctx := context.Background()
	stateDir, _, _ := createLegacyStoreWithRun(t)
	errorsSeen := make([]error, 2)
	var wait sync.WaitGroup
	for index := range errorsSeen {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			ledger, err := OpenForRead(ctx, stateDir)
			if err == nil {
				err = ledger.Close()
			}
			errorsSeen[index] = err
		}(index)
	}
	wait.Wait()
	for index, err := range errorsSeen {
		if err != nil {
			t.Fatalf("OpenForRead[%d] error = %v", index, err)
		}
	}
	ledger, err := OpenReadOnly(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	report, err := ledger.Doctor(ctx)
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor() after concurrent migration = %#v, %v", report, err)
	}
}

func TestLegacyMigrationRejectsAlteredSchemaWithoutPartialChanges(t *testing.T) {
	ctx := context.Background()
	stateDir, _, _ := createLegacyStoreWithRun(t)
	database := filepath.Join(stateDir, DatabaseFilename)
	db, err := sql.Open("sqlite3", sqliteDSN(database, false, defaultBusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER runs_no_delete;
		CREATE TRIGGER runs_no_delete BEFORE DELETE ON runs BEGIN SELECT 1; END`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenForRead(ctx, stateDir); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("OpenForRead(altered legacy schema) error = %v, want ErrInvalidSchema", err)
	}
	assertLegacySchemaNotPartiallyMigrated(t, database)
}

func TestLegacyMigrationRejectsCrossTypeNameCollision(t *testing.T) {
	ctx := context.Background()
	stateDir, _, _ := createLegacyStoreWithRun(t)
	database := filepath.Join(stateDir, DatabaseFilename)
	db, err := sql.Open("sqlite3", sqliteDSN(database, false, defaultBusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE runs_no_delete(extra TEXT) STRICT`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenForRead(ctx, stateDir); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("OpenForRead(colliding legacy schema) error = %v, want ErrInvalidSchema", err)
	}
	assertLegacySchemaNotPartiallyMigrated(t, database)
}

func TestSQLiteDSNUsesValidWindowsFileURLs(t *testing.T) {
	tests := []struct {
		name     string
		database string
		want     string
	}{
		{name: "drive", database: `C:\Users\Example\project state\ledger.db`, want: `file:C:/Users/Example/project%20state/ledger.db`},
		{name: "reserved characters", database: `C:\Users\Example\state #1%\ledger?.db`, want: `file:C:/Users/Example/state%20%231%25/ledger%3F.db`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := sqliteFileURL(test.database, true).String()
			if got != test.want {
				t.Fatalf("sqliteFileURL() = %q, want %q", got, test.want)
			}
			parsed, err := url.Parse(got)
			if err != nil || parsed.Scheme != "file" {
				t.Fatalf("parse %q = %#v, %v", got, parsed, err)
			}
		})
	}
}
