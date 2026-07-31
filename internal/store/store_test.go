package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testEpoch = time.Date(2026, 7, 21, 12, 0, 0, 123456789, time.UTC)

func TestSQLiteDSNInstallsBusyTimeoutBeforeTruncateJournalMode(t *testing.T) {
	dsn := sqliteDSN(filepath.Join(t.TempDir(), DatabaseFilename), false, defaultBusyTimeout)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pragmas := parsed.Query()["_pragma"]
	if len(pragmas) < 2 {
		t.Fatalf("pragmas = %q", pragmas)
	}
	if got, want := pragmas[0], "busy_timeout("+strconv.FormatInt(defaultBusyTimeout.Milliseconds(), 10)+")"; got != want {
		t.Fatalf("first pragma = %q, want %q", got, want)
	}
	if got, want := pragmas[1], "journal_mode(TRUNCATE)"; got != want {
		t.Fatalf("second pragma = %q, want %q", got, want)
	}
}

func TestValidationDirectoryStaysOutsideStateEvenWhenTMPDIRPointsInside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TMPDIR controls os.TempDir on POSIX")
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", stateDir)
	before, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	validationDir, err := createValidationDirectory(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	inside, err := pathWithin(validationDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if inside {
		t.Fatalf("validation directory %q is inside state directory %q", validationDir, stateDir)
	}
	if err := os.RemoveAll(validationDir); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) ||
		before.Mode() != after.Mode() ||
		before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("validation directory fallback changed state directory metadata: before=%#v after=%#v",
			before, after)
	}
}

func setJournalModeForTest(t *testing.T, database, mode string) {
	t.Helper()
	u := sqliteFileURL(database, runtime.GOOS == "windows")
	q := u.Query()
	q.Set("mode", "rw")
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(defaultBusyTimeout.Milliseconds(), 10)+")")
	q.Set("_dqs", "false")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRow(`PRAGMA journal_mode = ` + mode).Scan(&got); err != nil {
		_ = db.Close()
		t.Fatalf("set journal mode %s: %v", mode, err)
	}
	if !strings.EqualFold(got, mode) {
		_ = db.Close()
		t.Fatalf("journal mode = %q, want %q", got, mode)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func setApplicationIDForTest(t *testing.T, database string, applicationID uint32) {
	t.Helper()
	db, err := sql.Open("sqlite3",
		sqliteDSNWithJournalMode(database, false, defaultBusyTimeout, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA application_id = ` + strconv.FormatUint(uint64(applicationID), 10)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var got uint32
	if err := db.QueryRow(`PRAGMA application_id`).Scan(&got); err != nil || got != applicationID {
		_ = db.Close()
		t.Fatalf("application_id = %d, %v; want %d", got, err, applicationID)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

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

func TestOpenReadOnlyWorksWithNonWritableState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode semantics")
	}
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	writable, err := Open(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_nonwritable_read"
	startTestRun(t, writable, runID)
	closeTestRun(t, writable, runID)
	if _, err := writable.AddFinding(ctx, AddFindingParams{
		ID:          "finding_nonwritable_read",
		OriginRunID: runID,
		Content:     FindingContent{Summary: "Read-only state remains useful."},
	}); err != nil {
		t.Fatal(err)
	}
	var writableJournalMode string
	if err := writable.db.QueryRow(`PRAGMA journal_mode`).Scan(&writableJournalMode); err != nil ||
		!strings.EqualFold(writableJournalMode, "truncate") {
		t.Fatalf("writable journal mode = %q, %v", writableJournalMode, err)
	}
	receiptBefore, err := writable.ProjectReceipt(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	journal := database + "-journal"
	journalInfo, err := os.Lstat(journal)
	if err != nil || !journalInfo.Mode().IsRegular() || journalInfo.Size() != 0 {
		t.Fatalf("clean TRUNCATE journal = %#v, %v", journalInfo, err)
	}
	contentsBefore, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	infoBefore, err := os.Stat(database)
	if err != nil {
		t.Fatal(err)
	}
	entriesBefore, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(database, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(journal, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(stateDir, 0o700)
		_ = os.Chmod(database, 0o600)
		_ = os.Chmod(journal, 0o600)
	})

	for name, opener := range map[string]func(context.Context, string) (*Store, error){
		"OpenReadOnly": OpenReadOnly,
		"OpenForRead":  OpenForRead,
	} {
		t.Run(name, func(t *testing.T) {
			ledger, err := opener(ctx, stateDir)
			if err != nil {
				t.Fatalf("%s() error = %v", name, err)
			}
			runs, err := ledger.ListRuns(ctx, ListRunsOptions{})
			if err != nil || len(runs) != 1 || runs[0].ID != runID {
				_ = ledger.Close()
				t.Fatalf("ListRuns() = %#v, %v", runs, err)
			}
			finding, err := ledger.GetFinding(ctx, "finding_nonwritable_read")
			if err != nil || finding.OriginRunID != runID {
				_ = ledger.Close()
				t.Fatalf("GetFinding() = %#v, %v", finding, err)
			}
			receiptAfter, err := ledger.ProjectReceipt(ctx, runID)
			if err != nil || !bytes.Equal(receiptBefore.Bytes(), receiptAfter.Bytes()) {
				_ = ledger.Close()
				t.Fatalf("ProjectReceipt() changed: %v", err)
			}
			report, err := ledger.Doctor(ctx)
			if err != nil || !report.Consistent {
				_ = ledger.Close()
				t.Fatalf("Doctor() = %#v, %v", report, err)
			}
			if err := ledger.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	contentsAfter, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	infoAfter, err := os.Stat(database)
	if err != nil {
		t.Fatal(err)
	}
	entriesAfter, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contentsBefore, contentsAfter) {
		t.Fatal("read-only opens changed the database bytes")
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) || infoAfter.Size() != infoBefore.Size() {
		t.Fatalf("read-only opens changed database metadata: before=%#v after=%#v", infoBefore, infoAfter)
	}
	if got, want := directoryEntryNames(entriesAfter), directoryEntryNames(entriesBefore); !slices.Equal(got, want) {
		t.Fatalf("state files after read-only opens = %q, want %q", got, want)
	}
}

func TestOpenRejectsEmptyRollbackJournalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	ledger, err := Open(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	journal := database + "-journal"
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, journal); err != nil {
		t.Fatal(err)
	}

	for name, opener := range map[string]func(context.Context, string) (*Store, error){
		"Open":         Open,
		"OpenForRead":  OpenForRead,
		"OpenReadOnly": OpenReadOnly,
	} {
		t.Run(name, func(t *testing.T) {
			opened, err := opener(ctx, stateDir)
			if opened != nil {
				_ = opened.Close()
			}
			if !errors.Is(err, ErrUnsafeStatePath) {
				t.Fatalf("%s(empty rollback journal symlink) error = %v, want ErrUnsafeStatePath",
					name, err)
			}
		})
	}
}

func directoryEntryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	sort.Strings(names)
	return names
}

func TestOpenForReadMigratesWALWithoutChangingRecords(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	writable, err := Open(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_wal_upgrade"
	startTestRun(t, writable, runID)
	closeTestRun(t, writable, runID)
	if _, err := writable.AddFinding(ctx, AddFindingParams{
		ID:          "finding_wal_upgrade",
		OriginRunID: runID,
		Content:     FindingContent{Summary: "Preserve this finding."},
	}); err != nil {
		t.Fatal(err)
	}
	receiptBefore, err := writable.ProjectReceipt(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	setJournalModeForTest(t, database, "WAL")
	requiresMigration, err := requiresJournalMigration(database)
	if err != nil || !requiresMigration {
		t.Fatalf("requiresJournalMigration(WAL) = %t, %v", requiresMigration, err)
	}
	if _, err := OpenReadOnly(ctx, stateDir); !errors.Is(err, errJournalMigrationRequired) {
		t.Fatalf("OpenReadOnly(WAL) error = %v, want journal migration required", err)
	}

	ledger, err := OpenForRead(ctx, stateDir)
	if err != nil {
		t.Fatalf("OpenForRead(WAL) error = %v", err)
	}
	defer ledger.Close()
	receiptAfter, err := ledger.ProjectReceipt(ctx, runID)
	if err != nil || !bytes.Equal(receiptBefore.Bytes(), receiptAfter.Bytes()) {
		t.Fatalf("receipt after WAL migration changed: %v", err)
	}
	finding, err := ledger.GetFinding(ctx, "finding_wal_upgrade")
	if err != nil || finding.Content.Summary != "Preserve this finding." {
		t.Fatalf("finding after WAL migration = %#v, %v", finding, err)
	}
	var journalMode string
	if err := ledger.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil ||
		!strings.EqualFold(journalMode, "delete") {
		t.Fatalf("journal mode after migration = %q, %v", journalMode, err)
	}
	report, err := ledger.Doctor(ctx)
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor() after WAL migration = %#v, %v", report, err)
	}
	requiresMigration, err = requiresJournalMigration(database)
	if err != nil || requiresMigration {
		t.Fatalf("requiresJournalMigration(after) = %t, %v", requiresMigration, err)
	}
	recognized, err := hasMotusApplicationID(database)
	if err != nil || !recognized {
		t.Fatalf("application identity after WAL migration = %t, %v", recognized, err)
	}
}

func TestWALMigrationCommitsIdentityBeforeReturning(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	ledger, err := Open(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_unmarked_wal_upgrade"
	startTestRun(t, ledger, runID)
	closeTestRun(t, ledger, runID)
	receiptBefore, err := ledger.ProjectReceipt(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	setApplicationIDForTest(t, database, 0)
	setJournalModeForTest(t, database, "WAL")

	wal, err := sql.Open("sqlite3",
		sqliteDSNWithJournalMode(database, false, defaultBusyTimeout, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ensureMotusIdentity(ctx, wal); err != nil {
		t.Fatal(err)
	}
	if err := checkpointMotusIdentity(ctx, wal, database); err != nil {
		t.Fatal(err)
	}
	header, err := inspectSQLiteHeader(database)
	if err != nil || !header.valid ||
		header.writeVersion != 2 || header.readVersion != 2 ||
		header.applicationID != motusApplicationID {
		t.Fatalf("header before WAL-to-rollback transition = %#v, %v", header, err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	if err := migrateWALJournal(ctx, stateDir, database); err != nil {
		t.Fatalf("migrateWALJournal(unmarked WAL) = %v", err)
	}
	header, err = inspectSQLiteHeader(database)
	if err != nil || !header.valid ||
		header.writeVersion != 1 || header.readVersion != 1 ||
		header.applicationID != motusApplicationID {
		t.Fatalf("header immediately after WAL migration = %#v, %v", header, err)
	}
	ledger, err = OpenReadOnly(ctx, stateDir)
	if err != nil {
		t.Fatalf("OpenReadOnly(immediately migrated ledger) = %v", err)
	}
	defer ledger.Close()
	receiptAfter, err := ledger.ProjectReceipt(ctx, runID)
	if err != nil || !bytes.Equal(receiptBefore.Bytes(), receiptAfter.Bytes()) {
		t.Fatalf("receipt after identity-first WAL migration changed: %v", err)
	}
}

func TestCurrentVersionRemigratesLedgerAfterOlderVersionReenablesWAL(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	ledger, err := Open(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_repeated_wal_upgrade"
	startTestRun(t, ledger, runID)
	closeTestRun(t, ledger, runID)
	receiptBefore, err := ledger.ProjectReceipt(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	setApplicationIDForTest(t, database, 0)
	setJournalModeForTest(t, database, "WAL")
	firstCurrentOpen, err := OpenForRead(ctx, stateDir)
	if err != nil {
		t.Fatalf("first current open = %v", err)
	}
	if err := firstCurrentOpen.Close(); err != nil {
		t.Fatal(err)
	}

	// Versions through v0.1.4 request WAL on every writable open. Re-enabling
	// WAL here emulates opening this already migrated ledger with that binary.
	setJournalModeForTest(t, database, "WAL")
	if required, err := requiresJournalMigration(database); err != nil || !required {
		t.Fatalf("older-version reopen requires migration = %t, %v", required, err)
	}
	secondCurrentOpen, err := OpenForRead(ctx, stateDir)
	if err != nil {
		t.Fatalf("second current open after older-version reopen = %v", err)
	}
	defer secondCurrentOpen.Close()
	receiptAfter, err := secondCurrentOpen.ProjectReceipt(ctx, runID)
	if err != nil || !bytes.Equal(receiptBefore.Bytes(), receiptAfter.Bytes()) {
		t.Fatalf("receipt after repeated WAL migration changed: %v", err)
	}
	if required, err := requiresJournalMigration(database); err != nil || required {
		t.Fatalf("second current open left WAL migration required = %t, %v", required, err)
	}
}

func TestWALMigrationRejectsUnknownSchemaWithoutChangingJournalMode(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(stateDir, DatabaseFilename)
	file, err := os.OpenFile(database, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3",
		sqliteDSNWithJournalMode(database, false, defaultBusyTimeout, "WAL"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sentinel(value TEXT)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	contentsBefore, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenForRead(ctx, stateDir); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("OpenForRead(unknown WAL schema) error = %v, want ErrInvalidSchema", err)
	}
	contentsAfter, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contentsBefore, contentsAfter) {
		t.Fatal("failed WAL validation changed the database bytes")
	}
	requiresMigration, err := requiresJournalMigration(database)
	if err != nil || !requiresMigration {
		t.Fatalf("failed WAL validation changed journal mode: requires=%t err=%v", requiresMigration, err)
	}
	recognized, err := hasMotusApplicationID(database)
	if err != nil || recognized {
		t.Fatalf("failed WAL validation changed application identity = %t, %v", recognized, err)
	}
}

func TestOpenForReadAddsIdentityOnlyAfterValidatingCurrentSchema(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	ledger, err := Open(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_identity_upgrade"
	startTestRun(t, ledger, runID)
	closeTestRun(t, ledger, runID)
	receiptBefore, err := ledger.ProjectReceipt(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`PRAGMA application_id = 0`); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(ctx, stateDir); !errors.Is(err, errIdentityMigrationRequired) {
		t.Fatalf("OpenReadOnly(unmarked current schema) error = %v", err)
	}
	ledger, err = OpenForRead(ctx, stateDir)
	if err != nil {
		t.Fatalf("OpenForRead(unmarked current schema) = %v", err)
	}
	defer ledger.Close()
	receiptAfter, err := ledger.ProjectReceipt(ctx, runID)
	if err != nil || !bytes.Equal(receiptBefore.Bytes(), receiptAfter.Bytes()) {
		t.Fatalf("identity migration changed receipt: %v", err)
	}
	recognized, err := hasMotusApplicationID(database)
	if err != nil || !recognized {
		t.Fatalf("identity after migration = %t, %v", recognized, err)
	}
}

func TestOpenForReadExplainsReadOnlyWALMigration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode semantics")
	}
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	ledger, err := Open(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	setJournalModeForTest(t, database, "WAL")
	if err := os.Chmod(database, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(stateDir, 0o700)
		_ = os.Chmod(database, 0o600)
	})
	if _, err := OpenForRead(ctx, stateDir); !errors.Is(err, errJournalMigrationRequired) ||
		!strings.Contains(err.Error(), "needs exclusive writable access") {
		t.Fatalf("OpenForRead(read-only WAL) error = %v", err)
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
