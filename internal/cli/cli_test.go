package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/motus-os/work-ledger/internal/buildinfo"
	"github.com/motus-os/work-ledger/internal/store"
)

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("MOTUS_TEST_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(98)
	}
	arguments := os.Args[separator+1:]
	switch arguments[0] {
	case "success":
		_, _ = os.Stdout.WriteString("hello\n")
		_, _ = os.Stderr.WriteString("warning\n")
		os.Exit(0)
	case "private-output":
		if len(arguments) != 4 {
			os.Exit(95)
		}
		_, _ = os.Stdout.WriteString(arguments[1])
		_, _ = os.Stderr.WriteString(arguments[2])
		os.Exit(0)
	case "failure":
		_, _ = os.Stderr.WriteString("failed")
		os.Exit(7)
	case "large":
		_, _ = io.CopyN(os.Stdout, strings.NewReader(strings.Repeat("x", 1<<20)), 1<<20)
		os.Exit(0)
	case "stdin":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		os.Exit(0)
	case "sleep":
		if len(arguments) > 1 {
			_ = os.WriteFile(arguments[1], []byte("ready\n"), 0o600)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "signal":
		process, _ := os.FindProcess(os.Getpid())
		_ = process.Signal(os.Interrupt)
		time.Sleep(time.Second)
		os.Exit(96)
	default:
		os.Exit(97)
	}
}

func testEnvironment(cwd string, stdout, stderr io.Writer) Environment {
	return Environment{
		WorkingDirectory: cwd,
		Getenv:           func(string) string { return "" },
		ProgramName:      "motus",
		Stdin:            strings.NewReader(""),
		Stdout:           stdout,
		Stderr:           stderr,
	}
}

func helperCommand(t *testing.T, mode string, extra ...string) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments := []string{executable, "-test.run=TestCLIHelperProcess", "--", mode}
	return append(arguments, extra...)
}

func runCLI(t *testing.T, ctx context.Context, cwd, stateDir string, stdout, stderr io.Writer, command []string) int {
	t.Helper()
	arguments := []string{"--state-dir", stateDir}
	arguments = append(arguments, command...)
	t.Setenv("MOTUS_TEST_HELPER", "1")
	return Run(ctx, arguments, testEnvironment(cwd, stdout, stderr))
}

func TestWrapListReceiptDoctorEndToEnd(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	var stdout, stderr bytes.Buffer
	command := append([]string{"wrap", "--"}, helperCommand(t, "success")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, command); code != 0 {
		t.Fatalf("wrap exit = %d, stderr=%q", code, stderr.String())
	}
	if stdout.String() != "hello\n" {
		t.Fatalf("wrap stdout = %q", stdout.String())
	}
	wantNext := "\nNext: " + displayCommand("motus", "--state-dir", stateDir, "run", "receipt") + " run_"
	if !strings.HasPrefix(stderr.String(), "warning\nmotus: recorded run_") ||
		!strings.Contains(stderr.String(), wantNext) {
		t.Fatalf("wrap stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{"run", "list", "--json"}); code != 0 {
		t.Fatalf("list exit = %d, stderr=%q", code, stderr.String())
	}
	var runs []store.Run
	if err := json.Unmarshal(stdout.Bytes(), &runs); err != nil {
		t.Fatalf("decode list: %v; output=%q", err, stdout.String())
	}
	if len(runs) != 1 || runs[0].State != store.RunClosed || runs[0].Outcome != store.OutcomeSuccess {
		t.Fatalf("runs = %#v", runs)
	}
	if runs[0].Output.StdoutBytes != 6 || runs[0].Output.StdoutLines != 1 ||
		runs[0].Output.StderrBytes != 8 || runs[0].Output.StderrLines != 1 {
		t.Fatalf("output counts = %#v", runs[0].Output)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{"run", "receipt", runs[0].ID}); code != 0 {
		t.Fatalf("receipt exit = %d, stderr=%q", code, stderr.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if envelope["schema"] != "motus.work-receipt.v1" {
		t.Fatalf("receipt schema = %#v", envelope["schema"])
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{"doctor", "--json"}); code != 0 {
		t.Fatalf("doctor exit = %d, stderr=%q output=%q", code, stderr.String(), stdout.String())
	}
	var report store.DoctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || !report.Consistent {
		t.Fatalf("doctor report = %#v, %v", report, err)
	}
}

func TestWrapForwardsStdinAndPrintsAResolvableNextCommand(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state with space")
	var stdout, stderr bytes.Buffer
	command := append([]string{"wrap", "--"}, helperCommand(t, "stdin")...)
	environment := testEnvironment(cwd, &stdout, &stderr)
	environment.ProgramName = filepath.Join(cwd, "bin with space", "motus")
	environment.Stdin = strings.NewReader("input stays out of the ledger\n")
	t.Setenv("MOTUS_TEST_HELPER", "1")
	arguments := append([]string{"--state-dir", stateDir}, command...)
	if code := Run(context.Background(), arguments, environment); code != 0 {
		t.Fatalf("wrap exit = %d, stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "input stays out of the ledger\n" {
		t.Fatalf("wrap stdout = %q", got)
	}
	wantPrefix := "Next: " + strconv.Quote(environment.ProgramName) +
		" --state-dir " + strconv.Quote(stateDir) + " run receipt run_"
	if !strings.Contains(stderr.String(), wantPrefix) {
		t.Fatalf("wrap next instruction = %q, want prefix %q", stderr.String(), wantPrefix)
	}
	if err := filepath.WalkDir(stateDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte("input stays out of the ledger")) {
			t.Fatalf("stdin content persisted in %s", entry.Name())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWrapDoesNotPersistArgumentsEnvironmentOrRawStreams(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	argumentSecret := "argument-canary-value-92731"
	stdoutSecret := "stdout-canary-value-48026"
	stderrSecret := "stderr-canary-value-19374"
	environmentSecret := "environment-canary-value-64051"
	t.Setenv("MOTUS_TEST_PRIVATE_VALUE", environmentSecret)
	command := append([]string{"wrap", "--"}, helperCommand(t, "private-output", stdoutSecret, stderrSecret, argumentSecret)...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, command); code != 0 {
		t.Fatalf("wrap exit = %d", code)
	}
	err := filepath.WalkDir(stateDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{argumentSecret, stdoutSecret, stderrSecret, environmentSecret} {
			if bytes.Contains(content, []byte(secret)) {
				t.Fatalf("private process data persisted in %s", entry.Name())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFirstWrapDoesNotCountItsOwnStateAsGitDirty(t *testing.T) {
	cwd := t.TempDir()
	commands := [][]string{
		{"git", "init", "--quiet", cwd},
		{"git", "-C", cwd, "config", "user.email", "test@example.invalid"},
		{"git", "-C", cwd, "config", "user.name", "Test"},
	}
	if err := os.WriteFile(filepath.Join(cwd, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands = append(commands,
		[]string{"git", "-C", cwd, "add", "tracked.txt"},
		[]string{"git", "-C", cwd, "commit", "--quiet", "-m", "initial"},
	)
	for _, arguments := range commands {
		if output, err := exec.Command(arguments[0], arguments[1:]...).CombinedOutput(); err != nil {
			t.Skipf("git setup unavailable: %v: %s", err, output)
		}
	}
	stateDir := filepath.Join(cwd, ".motus")
	command := append([]string{"wrap", "--"}, helperCommand(t, "success")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, command); code != 0 {
		t.Fatalf("wrap exit = %d", code)
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
	if runs[0].Git.Dirty == nil || *runs[0].Git.Dirty {
		t.Fatalf("stored Git dirty state = %#v", runs[0].Git.Dirty)
	}
}

func TestWrapPreservesNonzeroExitAndClosesRun(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	var stderr bytes.Buffer
	command := append([]string{"wrap", "--"}, helperCommand(t, "failure")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, &stderr, command); code != 7 {
		t.Fatalf("wrap exit = %d, want 7; stderr=%q", code, stderr.String())
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
	if runs[0].State != store.RunClosed || runs[0].Outcome != store.OutcomeFailure ||
		runs[0].ExitCode == nil || *runs[0].ExitCode != 7 {
		t.Fatalf("failed run = %#v", runs[0])
	}
}

func TestRunListFiltersAndPagination(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	for _, mode := range []string{"success", "failure"} {
		command := append([]string{"wrap", "--"}, helperCommand(t, mode)...)
		code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, command)
		if mode == "success" && code != 0 || mode == "failure" && code != 7 {
			t.Fatalf("wrap %s exit = %d", mode, code)
		}
	}
	var stdout, stderr bytes.Buffer
	code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr,
		[]string{"run", "list", "--json", "--outcome", "failure", "--limit", "1", "--offset", "0"})
	if code != 0 {
		t.Fatalf("filtered list exit = %d, stderr=%q", code, stderr.String())
	}
	var runs []store.Run
	if err := json.Unmarshal(stdout.Bytes(), &runs); err != nil || len(runs) != 1 || runs[0].Outcome != store.OutcomeFailure {
		t.Fatalf("filtered runs = %#v, %v", runs, err)
	}
	stdout.Reset()
	stderr.Reset()
	code = runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr,
		[]string{"run", "list", "--state", "invalid"})
	if code != 2 || !strings.Contains(stderr.String(), "invalid run state") {
		t.Fatalf("invalid filter = code %d, stderr=%q", code, stderr.String())
	}
}

func TestRunListEscapesStoredTerminalControls(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	ledger, err := store.Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	_, err = ledger.StartRun(context.Background(), store.StartRunParams{
		ID: "run_control", EventID: "event_control_start", StartedAt: startedAt,
		Source: "test", Producer: "test", ExecutableBasename: "\x1b[31mred\x1b[0m",
	})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	_, err = ledger.CloseRun(context.Background(), "run_control", store.CloseRunParams{
		EventID: "event_control_terminal", EndedAt: startedAt.Add(time.Second),
		Outcome: store.OutcomeSuccess, ExitCode: &exitCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{"run", "list"}); code != 0 {
		t.Fatalf("run list exit = %d, stderr=%q", code, stderr.String())
	}
	if strings.ContainsRune(stdout.String(), '\x1b') || !strings.Contains(stdout.String(), `\x1b[31mred\x1b[0m`) {
		t.Fatalf("unsafe or unreadable command column = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{"run", "list", "--json"}); code != 0 {
		t.Fatalf("JSON run list exit = %d, stderr=%q", code, stderr.String())
	}
	var runs []store.Run
	if err := json.Unmarshal(stdout.Bytes(), &runs); err != nil || len(runs) != 1 ||
		runs[0].ExecutableBasename != "\x1b[31mred\x1b[0m" {
		t.Fatalf("JSON command value = %#v, %v", runs, err)
	}
}

type failingWriter struct {
	written int
	limit   int
}

func (w *failingWriter) Write(payload []byte) (int, error) {
	if w.written >= w.limit {
		return 0, errors.New("intentional writer failure")
	}
	remaining := w.limit - w.written
	if remaining > len(payload) {
		remaining = len(payload)
	}
	w.written += remaining
	if remaining < len(payload) {
		return remaining, errors.New("intentional writer failure")
	}
	return remaining, nil
}

func TestWrapDrainsOutputAfterWriterFailure(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	writer := &failingWriter{limit: 1024}
	command := append([]string{"wrap", "--"}, helperCommand(t, "large")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, writer, io.Discard, command); code != internalFailureExitCode {
		t.Fatalf("wrap exit = %d, want %d", code, internalFailureExitCode)
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
	if runs[0].State != store.RunClosed || runs[0].Outcome != store.OutcomeFailure || runs[0].Output.StdoutBytes != 1<<20 {
		t.Fatalf("writer-failure run = %#v", runs[0])
	}
}

func TestWrapCancellationStillClosesRun(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	readyPath := filepath.Join(cwd, "child-ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := append([]string{"wrap", "--"}, helperCommand(t, "sleep", readyPath)...)
	arguments := append([]string{"--state-dir", stateDir}, command...)
	t.Setenv("MOTUS_TEST_HELPER", "1")
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, arguments, testEnvironment(cwd, io.Discard, io.Discard))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not report ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 130 {
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
	if runs[0].State != store.RunClosed || runs[0].Outcome != store.OutcomeAborted || runs[0].TimedOut == nil || *runs[0].TimedOut {
		t.Fatalf("canceled run = %#v", runs[0])
	}
}

func TestListMissingStoreAndUsage(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "missing")
	var stdout, stderr bytes.Buffer
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{"run", "list", "--json"}); code != 0 || stdout.String() != "[]\n" {
		t.Fatalf("empty list = code %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("list created store: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"wrap", "echo"}, testEnvironment(cwd, &stdout, &stderr)); code != 2 {
		t.Fatalf("invalid wrap exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "wrap requires --") {
		t.Fatalf("usage stderr = %q", stderr.String())
	}
}

func TestHelpAndVersionDoNotCreateState(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	tests := []struct {
		name      string
		arguments []string
		contains  string
	}{
		{name: "root empty", contains: "Motus Work Ledger"},
		{name: "root flag", arguments: []string{"--help"}, contains: "motus [--state-dir PATH] COMMAND"},
		{name: "help command", arguments: []string{"help"}, contains: "Commands:"},
		{name: "wrap", arguments: []string{"wrap", "--help"}, contains: "wrap -- COMMAND [ARG ...]"},
		{name: "run", arguments: []string{"run", "--help"}, contains: "run receipt RUN_ID"},
		{name: "receipt", arguments: []string{"run", "receipt", "--help"}, contains: "run receipt RUN_ID"},
		{name: "doctor", arguments: []string{"doctor", "--help"}, contains: "current SQLite structure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			arguments := append([]string{"--state-dir", stateDir}, test.arguments...)
			if code := Run(context.Background(), arguments, testEnvironment(cwd, &stdout, &stderr)); code != 0 {
				t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.contains) || stderr.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}

	oldVersion, oldCommit, oldDate := buildinfo.Version, buildinfo.Commit, buildinfo.Date
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.Date = oldVersion, oldCommit, oldDate
	})
	buildinfo.Version = "0.1.0"
	buildinfo.Commit = "0123456789abcdef"
	buildinfo.Date = "2026-07-21T12:00:00Z"
	for _, arguments := range [][]string{{"--version"}, {"version"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), arguments, testEnvironment(cwd, &stdout, &stderr)); code != 0 {
			t.Fatalf("version exit = %d", code)
		}
		want := "motus 0.1.0 commit 0123456789abcdef built 2026-07-21T12:00:00Z\n"
		if stdout.String() != want || stderr.Len() != 0 {
			t.Fatalf("version stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	}

	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("help or version created state: %v", err)
	}
}
