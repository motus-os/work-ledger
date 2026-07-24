//go:build unix

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motus-os/work-ledger/internal/store"
)

func TestDisplayCommandUsesPOSIXShellQuoting(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	values := []string{
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"single'quote",
		"space separated",
		`path\segment`,
	}
	arguments := append([]string{"printf", strings.Repeat("%s\\n", len(values))}, values...)
	command := displayCommandForPlatform("linux", arguments...)
	output, err := exec.Command("sh", "-c", command).Output()
	if err != nil {
		t.Fatalf("execute displayed command: %v; command=%q", err, command)
	}
	want := []byte(strings.Join(values, "\n") + "\n")
	if !bytes.Equal(output, want) {
		t.Fatalf("displayed command output = %q, want %q; command=%q", output, want, command)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("displayed command executed command substitution: %v", err)
	}
}

func TestDisplayCommandUsesPowerShellLiteralArguments(t *testing.T) {
	command := displayCommandForPlatform("windows",
		`C:\Program Files\Motus\motus.exe`, "--state-dir",
		`C:\state\$(touch owned)\%TEMP%\`+"`whoami`"+`\it's`,
	)
	want := `& 'C:\Program Files\Motus\motus.exe' '--state-dir' 'C:\state\$(touch owned)\%TEMP%\` +
		"`whoami`" + `\it''s'`
	if command != want {
		t.Fatalf("PowerShell command = %q, want %q", command, want)
	}
}

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
