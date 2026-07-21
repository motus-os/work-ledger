package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
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
