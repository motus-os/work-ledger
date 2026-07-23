package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCanonicalJSONValidation(t *testing.T) {
	tests := []struct {
		name    string
		payload json.RawMessage
		want    string
		valid   bool
	}{
		{"object order", json.RawMessage(`{"z":1,"a":2}`), `{"a":2,"z":1}`, true},
		{"nested", json.RawMessage(` [ 3, {"b":false,"a":null} ] `), `[3,{"a":null,"b":false}]`, true},
		{"duplicate key", json.RawMessage(`{"a":1,"a":2}`), "", false},
		{"trailing", json.RawMessage(`{} {}`), "", false},
		{"valid surrogate pair", json.RawMessage(`{"value":"\ud83d\udc69"}`), `{"value":"👩"}`, true},
		{"escaped surrogate text", json.RawMessage(`{"value":"\\ud800"}`), `{"value":"\\ud800"}`, true},
		{"unpaired high surrogate", json.RawMessage(`{"value":"\ud800"}`), "", false},
		{"unpaired low surrogate", json.RawMessage(`{"value":"\udc00"}`), "", false},
		{"high surrogate followed by non-low", json.RawMessage(`{"value":"\ud800\u0041"}`), "", false},
		{"empty", nil, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := canonicalJSON(test.payload)
			if test.valid {
				if err != nil || string(got) != test.want {
					t.Fatalf("canonicalJSON() = %q, %v; want %q", got, err, test.want)
				}
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatalf("canonicalJSON() error = %v, want ErrInvalid", err)
			}
		})
	}
	invalidUTF8 := json.RawMessage{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}
	if _, err := canonicalJSON(invalidUTF8); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid UTF-8 error = %v, want ErrInvalid", err)
	}
	tooLarge := json.RawMessage(`"` + strings.Repeat("x", MaxEventPayloadBytes) + `"`)
	if _, err := canonicalJSON(tooLarge); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize canonicalJSON() error = %v, want ErrInvalid", err)
	}
}

func TestInputValidationAndConflicts(t *testing.T) {
	ledger := openTestStore(t)
	ctx := context.Background()
	valid := StartRunParams{
		ID: "run_validation", EventID: "run_validation_started", StartedAt: testEpoch, Source: "test", Producer: "test",
		ExecutableBasename: "true",
	}
	if _, err := ledger.StartRun(ctx, valid); err != nil {
		t.Fatalf("StartRun(valid) error = %v", err)
	}
	changed := valid
	changed.Producer = "other"
	if _, err := ledger.StartRun(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("StartRun(conflict) error = %v, want ErrConflict", err)
	}
	collidingEvent := valid
	collidingEvent.ID = "run_event_collision"
	if _, err := ledger.StartRun(ctx, collidingEvent); !errors.Is(err, ErrConflict) {
		t.Fatalf("StartRun(event collision) error = %v, want ErrConflict", err)
	}
	if _, err := ledger.CloseRun(ctx, valid.ID, CloseRunParams{
		EventID: "terminal_bad_time", EndedAt: testEpoch.Add(-time.Second), Outcome: OutcomeFailure,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("CloseRun(early) error = %v, want ErrInvalid", err)
	}
	if _, err := ledger.ListRuns(ctx, ListRunsOptions{Limit: 1001}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ListRuns(limit) error = %v, want ErrInvalid", err)
	}
}

func TestCloseRunRejectsContradictoryTerminalMetadata(t *testing.T) {
	zero, one, negative := 0, 1, -1
	falseValue, trueValue := false, true
	signal := "terminated"
	tests := []struct {
		name   string
		params CloseRunParams
	}{
		{name: "success without exit code", params: CloseRunParams{Outcome: OutcomeSuccess}},
		{name: "success with nonzero exit", params: CloseRunParams{Outcome: OutcomeSuccess, ExitCode: &one}},
		{name: "success with signal", params: CloseRunParams{Outcome: OutcomeSuccess, ExitCode: &zero, Signal: &signal}},
		{name: "success with timeout", params: CloseRunParams{Outcome: OutcomeSuccess, ExitCode: &zero, TimedOut: &trueValue}},
		{name: "failure with timeout", params: CloseRunParams{Outcome: OutcomeFailure, ExitCode: &one, TimedOut: &trueValue}},
		{name: "negative exit", params: CloseRunParams{Outcome: OutcomeFailure, ExitCode: &negative}},
		{name: "too many stdout lines", params: CloseRunParams{Outcome: OutcomeFailure, ExitCode: &one, TimedOut: &falseValue, Output: OutputCounts{StdoutBytes: 1, StdoutLines: 2}}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ledger := openTestStore(t)
			run := startTestRun(t, ledger, fmt.Sprintf("run_terminal_validation_%d", index))
			test.params.EventID = fmt.Sprintf("event_terminal_validation_%d", index)
			test.params.EndedAt = testEpoch.Add(time.Second)
			if _, err := ledger.CloseRun(context.Background(), run.ID, test.params); !errors.Is(err, ErrInvalid) {
				t.Fatalf("CloseRun() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestStatePathSafety(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode and symlink semantics")
	}
	ctx := context.Background()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, state); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("Open(world-readable state) error = %v, want ErrUnsafeStatePath", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger, err := Open(ctx, state)
	if err != nil {
		t.Fatalf("Open(private state) error = %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(state, DatabaseFilename), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, state); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("Open(world-readable db) error = %v, want ErrUnsafeStatePath", err)
	}

	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, link); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("Open(symlink state) error = %v, want ErrUnsafeStatePath", err)
	}

	ancestorTarget := filepath.Join(root, "ancestor-target")
	if err := os.Mkdir(ancestorTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	ancestorLink := filepath.Join(root, "ancestor-link")
	if err := os.Symlink(ancestorTarget, ancestorLink); err != nil {
		t.Fatal(err)
	}
	throughAncestor := filepath.Join(ancestorLink, "child")
	throughLedger, err := Open(ctx, throughAncestor)
	if err != nil {
		t.Fatalf("Open(ancestor symlink) error = %v", err)
	}
	defer throughLedger.Close()
	resolved, err := filepath.EvalSymlinks(throughAncestor)
	if err != nil {
		t.Fatal(err)
	}
	if throughLedger.stateDir != resolved {
		t.Fatalf("stateDir = %q, want physical path %q", throughLedger.stateDir, resolved)
	}
}

func TestExistingVersionZeroDatabaseWithTablesIsRejected(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	ledger, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`DROP TRIGGER metadata_no_update; DROP TRIGGER metadata_no_delete; DELETE FROM metadata; PRAGMA user_version=0`); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), stateDir); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("Open(version-zero nonempty) error = %v, want ErrInvalidSchema", err)
	}
}
