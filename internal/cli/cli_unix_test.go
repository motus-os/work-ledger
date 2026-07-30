//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestReadCommandsWorkWithNonWritableState(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	database := filepath.Join(stateDir, store.DatabaseFilename)
	command := append([]string{"wrap", "--"}, helperCommand(t, "success")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, command); code != 0 {
		t.Fatalf("wrap exit = %d", code)
	}
	runID := recordedRuns(t, stateDir)[0].ID
	var stdout, stderr bytes.Buffer
	if code := runCLIWithInput(t, cwd, stateDir, "Read-only handoff remains inspectable.\n", &stdout, &stderr, []string{
		"finding", "add", "--run", runID, "--file", "-", "--json",
	}); code != 0 {
		t.Fatalf("finding add exit = %d, stderr=%q", code, stderr.String())
	}
	var finding store.Finding
	if err := json.Unmarshal(stdout.Bytes(), &finding); err != nil {
		t.Fatalf("decode finding: %v", err)
	}
	databaseBefore, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
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

	tests := []struct {
		name    string
		command []string
		check   func(*testing.T, []byte)
	}{
		{
			name:    "run list",
			command: []string{"run", "list", "--json"},
			check: func(t *testing.T, output []byte) {
				var runs []store.Run
				if err := json.Unmarshal(output, &runs); err != nil || len(runs) != 1 || runs[0].ID != runID {
					t.Fatalf("runs = %#v, %v", runs, err)
				}
			},
		},
		{
			name:    "finding list",
			command: []string{"finding", "list", "--json"},
			check: func(t *testing.T, output []byte) {
				var findings []store.Finding
				if err := json.Unmarshal(output, &findings); err != nil || len(findings) != 1 ||
					findings[0].ID != finding.ID {
					t.Fatalf("findings = %#v, %v", findings, err)
				}
			},
		},
		{
			name:    "finding show",
			command: []string{"finding", "show", finding.ID, "--json"},
			check: func(t *testing.T, output []byte) {
				var view findingView
				if err := json.Unmarshal(output, &view); err != nil ||
					view.Finding.ID != finding.ID || view.OriginRun.ID != runID {
					t.Fatalf("finding view = %#v, %v", view, err)
				}
			},
		},
		{
			name:    "run receipt",
			command: []string{"run", "receipt", runID},
			check: func(t *testing.T, output []byte) {
				var receipt map[string]any
				if err := json.Unmarshal(output, &receipt); err != nil ||
					receipt["schema"] != "motus.work-receipt.v1" {
					t.Fatalf("receipt = %#v, %v", receipt, err)
				}
			},
		},
		{
			name:    "doctor",
			command: []string{"doctor", "--json"},
			check: func(t *testing.T, output []byte) {
				var report store.DoctorReport
				if err := json.Unmarshal(output, &report); err != nil || !report.Consistent {
					t.Fatalf("doctor = %#v, %v", report, err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, test.command); code != 0 {
				t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q", stderr.String())
			}
			test.check(t, stdout.Bytes())
		})
	}
	databaseAfter, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(databaseBefore, databaseAfter) {
		t.Fatal("read commands changed the non-writable database")
	}
}
