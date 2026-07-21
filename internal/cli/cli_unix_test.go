//go:build unix

package cli

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/motus-os/work-ledger/internal/store"
)

func TestWrapPreservesExternalSignal(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	command := append([]string{"wrap", "--"}, helperCommand(t, "signal")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, command); code != 130 {
		t.Fatalf("wrap exit = %d, want 130", code)
	}
	ledger, err := store.OpenReadOnly(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	runs, err := ledger.ListRuns(context.Background(), store.ListRunsOptions{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns() = %#v, %v", runs, err)
	}
	if runs[0].Outcome != store.OutcomeFailure || runs[0].Signal == nil || *runs[0].Signal != "interrupt" {
		t.Fatalf("signaled run = %#v", runs[0])
	}
}
