package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func closeTestRun(t *testing.T, ledger *Store, runID string) CloseResult {
	t.Helper()
	exitCode := 0
	timedOut := false
	result, err := ledger.CloseRun(context.Background(), runID, CloseRunParams{
		EventID:  runID + "_terminal",
		EndedAt:  testEpoch.Add(3 * time.Second),
		Outcome:  OutcomeSuccess,
		ExitCode: &exitCode,
		TimedOut: &timedOut,
		Output:   OutputCounts{StdoutBytes: 12, StderrBytes: 2, StdoutLines: 2, StderrLines: 1},
	})
	if err != nil {
		t.Fatalf("CloseRun() error = %v", err)
	}
	return result
}

func TestProjectReceiptIsDeterministicAndSelfChecking(t *testing.T) {
	ledger := openTestStore(t)
	ledger.now = func() time.Time { return testEpoch.Add(time.Second) }
	startTestRun(t, ledger, "run_receipt")
	closeTestRun(t, ledger, "run_receipt")

	first, err := ledger.ProjectReceipt(context.Background(), "run_receipt")
	if err != nil {
		t.Fatalf("ProjectReceipt() error = %v", err)
	}
	second, err := ledger.ProjectReceipt(context.Background(), "run_receipt")
	if err != nil {
		t.Fatalf("second ProjectReceipt() error = %v", err)
	}
	if string(first.Bytes()) != string(second.Bytes()) || first.DigestSHA256 != second.DigestSHA256 {
		t.Fatalf("receipt projection is not deterministic")
	}
	if len(first.JSON) == 0 || first.JSON[len(first.JSON)-1] != '\n' {
		t.Fatalf("receipt is not newline terminated")
	}

	var envelope struct {
		Schema        string          `json:"schema"`
		Receipt       json.RawMessage `json:"receipt"`
		ReceiptSHA256 string          `json:"receipt_sha256"`
	}
	if err := json.Unmarshal(first.JSON, &envelope); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if envelope.Schema != receiptSchema {
		t.Fatalf("schema = %q", envelope.Schema)
	}
	digest := sha256.Sum256(envelope.Receipt)
	if got := hex.EncodeToString(digest[:]); got != envelope.ReceiptSHA256 || got != first.DigestSHA256 {
		t.Fatalf("digest = %q envelope=%q returned=%q", got, envelope.ReceiptSHA256, first.DigestSHA256)
	}
	var body struct {
		TrustModel string `json:"trust_model"`
	}
	if err := json.Unmarshal(envelope.Receipt, &body); err != nil {
		t.Fatalf("decode receipt member: %v", err)
	}
	if body.TrustModel != "producer-controlled" {
		t.Fatalf("trust_model = %q", body.TrustModel)
	}

	copy := first.Bytes()
	copy[0] = 'X'
	if first.JSON[0] == 'X' {
		t.Fatalf("Bytes() did not return a defensive copy")
	}
}

func TestProjectReceiptRejectsOpenOrInconsistentRun(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_open")
	if _, err := ledger.ProjectReceipt(context.Background(), "run_open"); !errors.Is(err, ErrOpen) {
		t.Fatalf("ProjectReceipt(open) error = %v, want ErrOpen", err)
	}

	startTestRun(t, ledger, "run_tamper")
	closeTestRun(t, ledger, "run_tamper")
	if _, err := ledger.db.Exec(`DROP TRIGGER events_no_update`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := ledger.db.Exec(`UPDATE events SET payload = ? WHERE id = 'run_tamper_started'`, []byte(`{"ok":false}`)); err != nil {
		t.Fatalf("tamper payload: %v", err)
	}
	if _, err := ledger.ProjectReceipt(context.Background(), "run_tamper"); err == nil {
		t.Fatalf("ProjectReceipt() accepted inconsistent payload")
	}
}

func TestReceiptAndDoctorRejectTerminalOutcomeContradiction(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_contradiction")
	closed := closeTestRun(t, ledger, "run_contradiction")
	if _, err := ledger.db.Exec(`DROP TRIGGER events_no_update`); err != nil {
		t.Fatal(err)
	}
	contradictory := CloseRunParams{
		EventID:  closed.TerminalEvent.ID,
		EndedAt:  *closed.Run.EndedAt,
		Outcome:  OutcomeFailure,
		ExitCode: closed.Run.ExitCode,
		TimedOut: closed.Run.TimedOut,
		Output:   closed.Run.Output,
	}
	payload, err := canonicalTerminalPayload(contradictory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`UPDATE events SET payload = ?, payload_sha256 = ? WHERE id = ?`,
		payload, payloadDigest(payload), closed.TerminalEvent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ProjectReceipt(context.Background(), "run_contradiction"); err == nil {
		t.Fatal("ProjectReceipt() accepted a terminal outcome contradiction")
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Consistent {
		t.Fatal("Doctor() accepted a terminal outcome contradiction")
	}
}

func TestReceiptAndDoctorReportMalformedClosedRunWithoutPanicking(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_missing_end")
	closeTestRun(t, ledger, "run_missing_end")
	if _, err := ledger.db.Exec(`DROP TRIGGER runs_no_terminal_mutation_after_close`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`UPDATE runs SET ended_at = NULL WHERE id = 'run_missing_end'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`PRAGMA ignore_check_constraints = OFF`); err != nil {
		t.Fatal(err)
	}
	recreated := false
	for _, statement := range schemaStatements {
		if strings.HasPrefix(strings.TrimSpace(statement), `CREATE TRIGGER runs_no_terminal_mutation_after_close`) {
			if _, err := ledger.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			recreated = true
			break
		}
	}
	if !recreated {
		t.Fatal("terminal-mutation trigger definition not found")
	}

	if _, err := ledger.ProjectReceipt(context.Background(), "run_missing_end"); err == nil ||
		!strings.Contains(err.Error(), "closed run has no ended_at") {
		t.Fatalf("ProjectReceipt() error = %v", err)
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if report.Consistent {
		t.Fatalf("Doctor() accepted malformed closed run: %#v", report)
	}
	found := false
	for _, check := range report.Checks {
		if check.Name == "run terminal records" && !check.OK && strings.Contains(check.Detail, "closed run has no ended_at") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Doctor() did not report malformed closed run: %#v", report)
	}
}
