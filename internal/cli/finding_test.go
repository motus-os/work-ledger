package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motus-os/work-ledger/internal/store"
)

func runCLIWithInput(t *testing.T, cwd, stateDir, input string, stdout, stderr io.Writer, command []string) int {
	t.Helper()
	environment := testEnvironment(cwd, stdout, stderr)
	environment.Stdin = strings.NewReader(input)
	arguments := append([]string{"--state-dir", stateDir}, command...)
	t.Setenv("MOTUS_TEST_HELPER", "1")
	return Run(context.Background(), arguments, environment)
}

func recordedRuns(t *testing.T, stateDir string) []store.Run {
	t.Helper()
	ledger, err := store.OpenReadOnly(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	runs, err := ledger.ListRuns(context.Background(), store.ListRunsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestFindingCLIEndToEnd(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	failedCommand := append([]string{"wrap", "--"}, helperCommand(t, "failure")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, failedCommand); code != 7 {
		t.Fatalf("failed wrap exit = %d", code)
	}
	runs := recordedRuns(t, stateDir)
	if len(runs) != 1 || runs[0].Outcome != store.OutcomeFailure {
		t.Fatalf("failed runs = %#v", runs)
	}
	originRunID := runs[0].ID
	ledger, err := store.OpenReadOnly(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	receiptBefore, err := ledger.ProjectReceipt(context.Background(), originRunID)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Close()

	contentPath := filepath.Join(cwd, "finding input.json")
	content := `{"summary":"Retry used stale state","hypothesis":"The cache survived cleanup.","next_step":"Reproduce from a clean cache."}`
	if err := os.WriteFile(contentPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "add", "--run", originRunID, "--file", contentPath, "--format", "json",
	}); code != 0 {
		t.Fatalf("finding add exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "Recorded finding_") ||
		!strings.Contains(stdout.String(), "Summary: Retry used stale state") || stderr.Len() != 0 {
		t.Fatalf("finding add stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "list", "--query", "STALE", "--json",
	}); code != 0 {
		t.Fatalf("finding list exit = %d, stderr=%q", code, stderr.String())
	}
	var findings []store.Finding
	if err := json.Unmarshal(stdout.Bytes(), &findings); err != nil || len(findings) != 1 {
		t.Fatalf("finding list = %#v, %v; output=%q", findings, err, stdout.String())
	}
	findingID := findings[0].ID
	if findings[0].OriginRunID != originRunID || findings[0].State != store.FindingOpen {
		t.Fatalf("finding = %#v", findings[0])
	}

	ledger, err = store.Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.AddFinding(context.Background(), store.AddFindingParams{
		ID:          "finding_newer_reordered_terms",
		OriginRunID: originRunID,
		Content: store.FindingContent{
			Summary:  "Used cache after retry",
			NextStep: "Inspect the cache before another attempt.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "list", "--query", "retry used", "--json",
	}); code != 0 {
		t.Fatalf("ranked JSON list exit = %d, stderr=%q", code, stderr.String())
	}
	findings = nil
	if err := json.Unmarshal(stdout.Bytes(), &findings); err != nil || len(findings) != 2 {
		t.Fatalf("ranked JSON list = %#v, %v; output=%q", findings, err, stdout.String())
	}
	if findings[0].ID != findingID || findings[1].ID != "finding_newer_reordered_terms" {
		t.Fatalf("ranked JSON IDs = %q, %q", findings[0].ID, findings[1].ID)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "list", "--query", "retry used",
	}); code != 0 {
		t.Fatalf("ranked human list exit = %d, stderr=%q", code, stderr.String())
	}
	firstPosition := strings.Index(stdout.String(), findingID)
	secondPosition := strings.Index(stdout.String(), "finding_newer_reordered_terms")
	if firstPosition < 0 || secondPosition < 0 || firstPosition >= secondPosition {
		t.Fatalf("ranked human list order = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "show", "--json", findingID,
	}); code != 0 {
		t.Fatalf("finding show exit = %d, stderr=%q", code, stderr.String())
	}
	var view findingView
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil ||
		view.Finding.ID != findingID || view.OriginRun.ID != originRunID || view.OriginRun.Outcome != store.OutcomeFailure {
		t.Fatalf("finding view = %#v, %v", view, err)
	}

	successCommand := append([]string{"wrap", "--"}, helperCommand(t, "success")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, successCommand); code != 0 {
		t.Fatalf("successful wrap exit = %d", code)
	}
	runs = recordedRuns(t, stateDir)
	if len(runs) != 2 || runs[0].Outcome != store.OutcomeSuccess {
		t.Fatalf("runs after fix = %#v", runs)
	}
	resolvingRunID := runs[0].ID

	stdout.Reset()
	stderr.Reset()
	if code := runCLIWithInput(t, cwd, stateDir, "Clean cache confirmed the fix.\n", &stdout, &stderr, []string{
		"finding", "close", findingID, "--disposition", "resolved",
		"--run", resolvingRunID, "--file", "-", "--json",
	}); code != 0 {
		t.Fatalf("finding close exit = %d, stderr=%q", code, stderr.String())
	}
	var resolved store.Finding
	if err := json.Unmarshal(stdout.Bytes(), &resolved); err != nil ||
		resolved.State != store.FindingResolved || resolved.Closure == nil ||
		resolved.Closure.ResolvingRunID != resolvingRunID {
		t.Fatalf("resolved finding = %#v, %v", resolved, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "show", findingID,
	}); code != 0 {
		t.Fatalf("human finding show exit = %d, stderr=%q", code, stderr.String())
	}
	for _, want := range []string{
		"State: resolved", "Origin run: " + originRunID + " (failure,",
		"Resolving run: " + resolvingRunID + " (success,", "Closure note: Clean cache confirmed the fix.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("finding show missing %q: %q", want, stdout.String())
		}
	}

	ledger, err = store.OpenReadOnly(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	receiptAfter, err := ledger.ProjectReceipt(context.Background(), originRunID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ledger.Doctor(context.Background())
	ledger.Close()
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor() = %#v, %v", report, err)
	}
	if string(receiptBefore.Bytes()) != string(receiptAfter.Bytes()) {
		t.Fatalf("finding workflow changed the origin receipt")
	}
}

func TestFindingCLIDismissalAndEmptyStore(t *testing.T) {
	cwd := t.TempDir()
	missing := filepath.Join(cwd, "missing")
	var stdout, stderr bytes.Buffer
	if code := runCLI(t, context.Background(), cwd, missing, &stdout, &stderr, []string{
		"finding", "list", "--json",
	}); code != 1 || stdout.Len() != 0 ||
		!strings.Contains(stderr.String(), "no Motus ledger found in") ||
		!strings.Contains(stderr.String(), "--state-dir or MOTUS_STATE_DIR") {
		t.Fatalf("missing finding ledger = code %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("finding list created store: %v", err)
	}

	stateDir := filepath.Join(cwd, "state")
	command := append([]string{"wrap", "--"}, helperCommand(t, "success")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, command); code != 0 {
		t.Fatal(code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "list",
	}); code != 0 || stdout.String() != "No findings recorded.\n" || stderr.Len() != 0 {
		t.Fatalf("empty finding list = code %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "list", "--json",
	}); code != 0 || stdout.String() != "[]\n" || stderr.Len() != 0 {
		t.Fatalf("empty finding JSON = code %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	runID := recordedRuns(t, stateDir)[0].ID
	stdout.Reset()
	stderr.Reset()
	if code := runCLIWithInput(t, cwd, stateDir, "No longer relevant.\n", &stdout, &stderr, []string{
		"finding", "add", "--run", runID, "--file", "-", "--json",
	}); code != 0 {
		t.Fatalf("text finding add exit = %d, stderr=%q", code, stderr.String())
	}
	var finding store.Finding
	if err := json.Unmarshal(stdout.Bytes(), &finding); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLIWithInput(t, cwd, stateDir, "The underlying task was retired.\n", &stdout, &stderr, []string{
		"finding", "close", finding.ID, "--disposition", "dismissed", "--file", "-",
	}); code != 0 || !strings.Contains(stdout.String(), "(dismissed)") {
		t.Fatalf("dismiss exit = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "list", "--query", "does not exist",
	}); code != 0 || stdout.String() != "No findings matched \"does not exist\".\n" || stderr.Len() != 0 {
		t.Fatalf("no matching findings = code %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestFindingCLIRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	command := append([]string{"wrap", "--"}, helperCommand(t, "success")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, command); code != 0 {
		t.Fatal(code)
	}
	runID := recordedRuns(t, stateDir)[0].ID

	tests := []struct {
		name   string
		input  string
		format string
	}{
		{name: "duplicate JSON field", input: `{"summary":"one","summary":"two"}`, format: "json"},
		{name: "unknown JSON field", input: `{"summary":"one","confidence":"high"}`, format: "json"},
		{name: "trailing JSON", input: `{"summary":"one"} {}`, format: "json"},
		{name: "unpaired surrogate", input: `{"summary":"bad\ud800"}`, format: "json"},
		{name: "multiline text summary", input: "one\ntwo\n", format: "text"},
		{name: "terminal control", input: "bad\x1b[31m\n", format: "text"},
		{name: "invalid format", input: "one\n", format: "yaml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runCLIWithInput(t, cwd, stateDir, test.input, &stdout, &stderr, []string{
				"finding", "add", "--run", runID, "--file", "-", "--format", test.format,
			})
			if code != 2 || !strings.Contains(stderr.String(), "Try 'motus --help'.") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}

	var stdout, stderr bytes.Buffer
	oversized := strings.Repeat("x", store.MaxFindingPayloadBytes+1)
	if code := runCLIWithInput(t, cwd, stateDir, oversized, &stdout, &stderr, []string{
		"finding", "add", "--run", runID, "--file", "-",
	}); code != 2 || !strings.Contains(stderr.String(), "exceeds") {
		t.Fatalf("oversized exit=%d stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "list", "--limit", "0",
	}); code != 2 || !strings.Contains(stderr.String(), `invalid --limit "0"`) {
		t.Fatalf("zero limit exit=%d stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI(t, context.Background(), cwd, stateDir, &stdout, &stderr, []string{
		"finding", "add", "--run", runID, "--file", filepath.Join(cwd, "missing"),
	}); code != 1 || !strings.Contains(stderr.String(), "open finding input") {
		t.Fatalf("missing file exit=%d stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLIWithInput(t, cwd, stateDir, "summary\n", &stdout, &stderr, []string{
		"finding", "add", "--run", runID, "--run", runID, "--file", "-",
	}); code != 2 || !strings.Contains(stderr.String(), "--run may be specified only once") {
		t.Fatalf("duplicate flag exit=%d stderr=%q", code, stderr.String())
	}
}

func TestFindingCommandsAppearInHelpWithoutCreatingState(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--state-dir", stateDir, "--help"}, testEnvironment(cwd, &stdout, &stderr)); code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"finding add", "finding list", "finding show", "finding close"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("root help missing %q: %q", want, stdout.String())
		}
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("help created state: %v", err)
	}
}

func TestFindingHumanOutputFailureIsReported(t *testing.T) {
	cwd := t.TempDir()
	stateDir := filepath.Join(cwd, "state")
	command := append([]string{"wrap", "--"}, helperCommand(t, "success")...)
	if code := runCLI(t, context.Background(), cwd, stateDir, io.Discard, io.Discard, command); code != 0 {
		t.Fatal(code)
	}
	runID := recordedRuns(t, stateDir)[0].ID
	writer := &failingWriter{limit: 1}
	var stderr bytes.Buffer
	if code := runCLIWithInput(t, cwd, stateDir, "Output failure test.\n", writer, &stderr, []string{
		"finding", "add", "--run", runID, "--file", "-",
	}); code != 1 || !strings.Contains(stderr.String(), "write output") {
		t.Fatalf("output failure exit=%d stderr=%q", code, stderr.String())
	}
}
