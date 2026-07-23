package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func addTestFinding(t *testing.T, ledger *Store, id, runID, summary string) Finding {
	t.Helper()
	finding, err := ledger.AddFinding(context.Background(), AddFindingParams{
		ID:          id,
		OriginRunID: runID,
		Content: FindingContent{
			Summary:    summary,
			Hypothesis: "The cached state may be stale.",
			NextStep:   "Reproduce with a clean cache.",
		},
	})
	if err != nil {
		t.Fatalf("AddFinding() error = %v", err)
	}
	return finding
}

func TestFindingLifecycleAndReceiptIsolation(t *testing.T) {
	ledger := openTestStore(t)
	ledger.now = func() time.Time { return testEpoch.Add(4 * time.Second) }
	startTestRun(t, ledger, "run_origin")
	closeTestRun(t, ledger, "run_origin")

	receiptBefore, err := ledger.ProjectReceipt(context.Background(), "run_origin")
	if err != nil {
		t.Fatal(err)
	}
	finding := addTestFinding(t, ledger, "finding_01", "run_origin", "Retry used stale state")
	if finding.State != FindingOpen || finding.Closure != nil || finding.PayloadSHA256 == "" {
		t.Fatalf("AddFinding() = %#v", finding)
	}
	idempotent := addTestFinding(t, ledger, "finding_01", "run_origin", "Retry used stale state")
	if idempotent.ID != finding.ID || !idempotent.RecordedAt.Equal(finding.RecordedAt) {
		t.Fatalf("idempotent AddFinding() changed finding: %#v", idempotent)
	}
	if _, err := ledger.AddFinding(context.Background(), AddFindingParams{
		ID: "finding_01", OriginRunID: "run_origin", Content: FindingContent{Summary: "Different"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting AddFinding() error = %v, want ErrConflict", err)
	}

	ledger.now = func() time.Time { return testEpoch.Add(5 * time.Second) }
	closed, err := ledger.CloseFinding(context.Background(), finding.ID, CloseFindingParams{
		ID:             "closure_01",
		Disposition:    DispositionResolved,
		ResolvingRunID: "run_origin",
		Content:        FindingClosureContent{Note: "A clean cache made the retry deterministic."},
	})
	if err != nil {
		t.Fatalf("CloseFinding() error = %v", err)
	}
	if closed.State != FindingResolved || closed.Closure == nil ||
		closed.Closure.ResolvingRunID != "run_origin" || closed.Closure.PayloadSHA256 == "" {
		t.Fatalf("CloseFinding() = %#v", closed)
	}
	closedAgain, err := ledger.CloseFinding(context.Background(), finding.ID, CloseFindingParams{
		ID:             "closure_01",
		Disposition:    DispositionResolved,
		ResolvingRunID: "run_origin",
		Content:        FindingClosureContent{Note: "A clean cache made the retry deterministic."},
	})
	if err != nil || closedAgain.Closure.ID != "closure_01" {
		t.Fatalf("idempotent CloseFinding() = %#v, %v", closedAgain, err)
	}
	if _, err := ledger.CloseFinding(context.Background(), finding.ID, CloseFindingParams{
		ID: "closure_other", Disposition: DispositionDismissed,
		Content: FindingClosureContent{Note: "No longer relevant."},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting CloseFinding() error = %v, want ErrConflict", err)
	}

	receiptAfter, err := ledger.ProjectReceipt(context.Background(), "run_origin")
	if err != nil {
		t.Fatal(err)
	}
	if string(receiptBefore.Bytes()) != string(receiptAfter.Bytes()) ||
		receiptBefore.DigestSHA256 != receiptAfter.DigestSHA256 {
		t.Fatalf("finding records changed the run receipt")
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor() = %#v, %v", report, err)
	}
}

func TestFindingDismissalAndDeterministicListing(t *testing.T) {
	ledger := openTestStore(t)
	for _, runID := range []string{"run_one", "run_two", "run_three"} {
		startTestRun(t, ledger, runID)
		closeTestRun(t, ledger, runID)
	}

	ledger.now = func() time.Time { return testEpoch.Add(10 * time.Second) }
	addTestFinding(t, ledger, "finding_one", "run_one", "Cache mismatch on Café build")
	ledger.now = func() time.Time { return testEpoch.Add(11 * time.Second) }
	addTestFinding(t, ledger, "finding_two", "run_two", "Windows path mismatch")
	ledger.now = func() time.Time { return testEpoch.Add(12 * time.Second) }
	addTestFinding(t, ledger, "finding_three", "run_three", "Flaky test was environmental")
	ledger.now = func() time.Time { return testEpoch.Add(13 * time.Second) }
	dismissed, err := ledger.CloseFinding(context.Background(), "finding_three", CloseFindingParams{
		ID:          "closure_three",
		Disposition: DispositionDismissed,
		Content:     FindingClosureContent{Note: "The failure came from a retired runner."},
	})
	if err != nil || dismissed.State != FindingDismissed || dismissed.Closure.ResolvingRunID != "" {
		t.Fatalf("dismissed finding = %#v, %v", dismissed, err)
	}

	all, err := ledger.ListFindings(context.Background(), ListFindingsOptions{})
	if err != nil || len(all) != 3 || all[0].ID != "finding_three" || all[2].ID != "finding_one" {
		t.Fatalf("ListFindings() = %#v, %v", all, err)
	}
	open, err := ledger.ListFindings(context.Background(), ListFindingsOptions{State: FindingOpen})
	if err != nil || len(open) != 2 {
		t.Fatalf("open findings = %#v, %v", open, err)
	}
	queried, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: "CAFÉ"})
	if err != nil || len(queried) != 1 || queried[0].ID != "finding_one" {
		t.Fatalf("queried findings = %#v, %v", queried, err)
	}
	closureQuery, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: "retired runner"})
	if err != nil || len(closureQuery) != 1 || closureQuery[0].ID != "finding_three" {
		t.Fatalf("closure query = %#v, %v", closureQuery, err)
	}
	page, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 || page[0].ID != "finding_two" {
		t.Fatalf("finding page = %#v, %v", page, err)
	}
	got, err := ledger.GetFinding(context.Background(), "finding_three")
	if err != nil || got.State != FindingDismissed || got.Closure.Content.Note == "" {
		t.Fatalf("GetFinding() = %#v, %v", got, err)
	}
}

func TestFindingValidationAndRunRequirements(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_open_finding")
	if _, err := ledger.AddFinding(context.Background(), AddFindingParams{
		ID: "finding_open", OriginRunID: "run_open_finding", Content: FindingContent{Summary: "Open run"},
	}); !errors.Is(err, ErrOpen) {
		t.Fatalf("finding on open run error = %v, want ErrOpen", err)
	}
	startTestRun(t, ledger, "run_closed_finding")
	closeTestRun(t, ledger, "run_closed_finding")

	tests := []struct {
		name    string
		content FindingContent
	}{
		{name: "empty summary", content: FindingContent{}},
		{name: "leading whitespace", content: FindingContent{Summary: " leading"}},
		{name: "multiline summary", content: FindingContent{Summary: "one\ntwo"}},
		{name: "terminal control", content: FindingContent{Summary: "bad\x1b[31m"}},
		{name: "bidi formatting", content: FindingContent{Summary: "bad\u202etext"}},
		{name: "oversized summary", content: FindingContent{Summary: strings.Repeat("x", maxFindingSummaryBytes+1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ledger.AddFinding(context.Background(), AddFindingParams{
				ID:          "finding_invalid_" + strings.ReplaceAll(test.name, " ", "_"),
				OriginRunID: "run_closed_finding", Content: test.content,
			}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("AddFinding() error = %v, want ErrInvalid", err)
			}
		})
	}

	addTestFinding(t, ledger, "finding_close_rules", "run_closed_finding", "Closure rules")
	if _, err := ledger.CloseFinding(context.Background(), "finding_close_rules", CloseFindingParams{
		ID: "closure_missing_run", Disposition: DispositionResolved,
		Content: FindingClosureContent{Note: "Missing run."},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("resolved without run error = %v, want ErrInvalid", err)
	}
	if _, err := ledger.CloseFinding(context.Background(), "finding_close_rules", CloseFindingParams{
		ID: "closure_dismissed_run", Disposition: DispositionDismissed, ResolvingRunID: "run_closed_finding",
		Content: FindingClosureContent{Note: "Dismissed."},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dismissed with run error = %v, want ErrInvalid", err)
	}

	startTestRun(t, ledger, "run_failed_resolution")
	exitCode := 1
	if _, err := ledger.CloseRun(context.Background(), "run_failed_resolution", CloseRunParams{
		EventID: "run_failed_resolution_terminal", EndedAt: testEpoch.Add(3 * time.Second),
		Outcome: OutcomeFailure, ExitCode: &exitCode,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CloseFinding(context.Background(), "finding_close_rules", CloseFindingParams{
		ID: "closure_failed_run", Disposition: DispositionResolved, ResolvingRunID: "run_failed_resolution",
		Content: FindingClosureContent{Note: "Still failing."},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("failed resolving run error = %v, want ErrInvalid", err)
	}

	for _, options := range []ListFindingsOptions{
		{Limit: -1}, {Limit: 1001}, {Offset: -1}, {State: "unknown"}, {Query: "bad\nquery"},
	} {
		if _, err := ledger.ListFindings(context.Background(), options); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ListFindings(%#v) error = %v, want ErrInvalid", options, err)
		}
	}
	if _, err := ledger.GetFinding(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFinding(missing) error = %v, want ErrNotFound", err)
	}
}

func TestDoctorRejectsMalformedFindingPayloadsAndRunLinks(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_doctor_finding")
	closeTestRun(t, ledger, "run_doctor_finding")
	addTestFinding(t, ledger, "finding_doctor", "run_doctor_finding", "Doctor checks findings")

	if _, err := ledger.db.Exec(`DROP TRIGGER findings_no_update`); err != nil {
		t.Fatal(err)
	}
	malformed := []byte(`{"summary":"Doctor checks findings","unknown":true}`)
	if _, err := ledger.db.Exec(`UPDATE findings SET payload = ?, payload_sha256 = ? WHERE id = 'finding_doctor'`,
		malformed, payloadDigest(malformed)); err != nil {
		t.Fatal(err)
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Consistent {
		t.Fatalf("Doctor() accepted malformed finding payload")
	}
	failed := make(map[string]bool)
	for _, check := range report.Checks {
		if !check.OK {
			failed[check.Name] = true
		}
	}
	if !failed["schema"] || !failed["finding records"] {
		t.Fatalf("Doctor() failed checks = %#v", failed)
	}
}

func TestDoctorRejectsNonCanonicalFindingAndClosurePayloads(t *testing.T) {
	t.Run("finding duplicate key", func(t *testing.T) {
		ledger := openTestStore(t)
		startTestRun(t, ledger, "run_duplicate_finding")
		closeTestRun(t, ledger, "run_duplicate_finding")
		payload := []byte(`{"summary":"first","summary":"second"}`)
		if _, err := ledger.db.Exec(`INSERT INTO findings
			(id, origin_run_id, payload, payload_sha256, recorded_at)
			VALUES ('finding_duplicate', 'run_duplicate_finding', ?, ?, '2026-07-21T12:00:04.000000000Z')`,
			payload, payloadDigest(payload)); err != nil {
			t.Fatal(err)
		}
		report, err := ledger.Doctor(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if report.Consistent || !doctorCheckFailed(report, "finding records") {
			t.Fatalf("Doctor() accepted duplicate-key finding: %#v", report)
		}
		if _, err := ledger.ListFindings(context.Background(), ListFindingsOptions{}); err == nil {
			t.Fatalf("ListFindings() accepted duplicate-key finding")
		}
	})

	t.Run("closure duplicate key", func(t *testing.T) {
		ledger := openTestStore(t)
		startTestRun(t, ledger, "run_duplicate_closure")
		closeTestRun(t, ledger, "run_duplicate_closure")
		addTestFinding(t, ledger, "finding_duplicate_closure", "run_duplicate_closure", "Closure duplicate")
		payload := []byte(`{"note":"first","note":"second"}`)
		if _, err := ledger.db.Exec(`INSERT INTO finding_closures
			(id, finding_id, disposition, resolving_run_id, payload, payload_sha256, recorded_at)
			VALUES ('closure_duplicate', 'finding_duplicate_closure', 'dismissed', NULL, ?, ?,
			'2026-07-21T12:00:05.000000000Z')`, payload, payloadDigest(payload)); err != nil {
			t.Fatal(err)
		}
		report, err := ledger.Doctor(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if report.Consistent || !doctorCheckFailed(report, "finding records") {
			t.Fatalf("Doctor() accepted duplicate-key closure: %#v", report)
		}
	})
}

func doctorCheckFailed(report DoctorReport, name string) bool {
	for _, check := range report.Checks {
		if check.Name == name {
			return !check.OK
		}
	}
	return false
}

func TestFindingJSONRoundTripHasNoImplicitClaims(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_json_finding")
	closeTestRun(t, ledger, "run_json_finding")
	finding := addTestFinding(t, ledger, "finding_json", "run_json_finding", "One explicit record")
	raw, err := json.Marshal(finding)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"approved", "verified", "attested", "proof"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("finding JSON contains implicit trust claim %q: %s", forbidden, text)
		}
	}
}

func TestFindingJSONParsersRejectAmbiguousObjects(t *testing.T) {
	for _, raw := range []string{
		`{"summary":"one","summary":"two"}`,
		`{"summary":"one","unknown":true}`,
		`{"summary":"one"} {}`,
		`[]`,
		`null`,
	} {
		if _, err := ParseFindingContentJSON([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ParseFindingContentJSON(%q) error = %v, want ErrInvalid", raw, err)
		}
	}
	for _, raw := range []string{
		`{"note":"one","note":"two"}`,
		`{"note":"one","unknown":true}`,
		`{"note":"one"} {}`,
	} {
		if _, err := ParseFindingClosureContentJSON([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ParseFindingClosureContentJSON(%q) error = %v, want ErrInvalid", raw, err)
		}
	}
}

func TestFindingTextAllowsOrdinaryUnicodeButRejectsBidiControls(t *testing.T) {
	content := FindingContent{
		Summary:    "Agent fixed the cache 👩‍💻",
		Hypothesis: "مرحبا بالعالم",
	}
	if _, err := canonicalFindingPayload(content); err != nil {
		t.Fatalf("ordinary Unicode rejected: %v", err)
	}
	content.Summary = "safe\u202etxt"
	if _, err := canonicalFindingPayload(content); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bidi control error = %v, want ErrInvalid", err)
	}
}

func TestFindingConstraintsRejectInvalidDirectLinks(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_open_direct")
	startTestRun(t, ledger, "run_success_direct")
	closeTestRun(t, ledger, "run_success_direct")
	startTestRun(t, ledger, "run_failure_direct")
	exitCode := 1
	if _, err := ledger.CloseRun(context.Background(), "run_failure_direct", CloseRunParams{
		EventID: "run_failure_direct_terminal", EndedAt: testEpoch.Add(3 * time.Second),
		Outcome: OutcomeFailure, ExitCode: &exitCode,
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := canonicalFindingPayload(FindingContent{Summary: "Direct constraint"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`INSERT INTO findings
		(id, origin_run_id, payload, payload_sha256, recorded_at)
		VALUES ('finding_open_direct', 'run_open_direct', ?, ?, '2026-07-21T12:00:04.000000000Z')`,
		payload, payloadDigest(payload)); err == nil {
		t.Fatalf("direct finding on open run succeeded")
	}
	addTestFinding(t, ledger, "finding_direct", "run_success_direct", "Direct closure constraint")
	closurePayload, err := canonicalFindingClosurePayload(FindingClosureContent{Note: "Direct closure."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`INSERT INTO finding_closures
		(id, finding_id, disposition, resolving_run_id, payload, payload_sha256, recorded_at)
		VALUES ('closure_failed_direct', 'finding_direct', 'resolved', 'run_failure_direct', ?, ?,
		'2026-07-21T12:00:05.000000000Z')`, closurePayload, payloadDigest(closurePayload)); err == nil {
		t.Fatalf("direct resolution with failed run succeeded")
	}
	if _, err := ledger.db.Exec(`INSERT INTO finding_closures
		(id, finding_id, disposition, resolving_run_id, payload, payload_sha256, recorded_at)
		VALUES ('closure_dismissed_direct', 'finding_direct', 'dismissed', 'run_success_direct', ?, ?,
		'2026-07-21T12:00:05.000000000Z')`, closurePayload, payloadDigest(closurePayload)); err == nil {
		t.Fatalf("direct dismissal with resolving run succeeded")
	}
}

func TestConcurrentFindingClosureProducesOneTerminalRecord(t *testing.T) {
	first := openTestStore(t)
	startTestRun(t, first, "run_concurrent_finding")
	closeTestRun(t, first, "run_concurrent_finding")
	addTestFinding(t, first, "finding_concurrent", "run_concurrent_finding", "Concurrent close")
	second, err := Open(context.Background(), first.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	stores := []*Store{first, second}
	errorsSeen := make([]error, len(stores))
	var wait sync.WaitGroup
	for index, ledger := range stores {
		wait.Add(1)
		go func(index int, ledger *Store) {
			defer wait.Done()
			_, errorsSeen[index] = ledger.CloseFinding(context.Background(), "finding_concurrent", CloseFindingParams{
				ID:          "closure_concurrent_" + string(rune('a'+index)),
				Disposition: DispositionDismissed,
				Content:     FindingClosureContent{Note: "Only one closure may win."},
			})
		}(index, ledger)
	}
	wait.Wait()
	successes, conflicts := 0, 0
	for _, err := range errorsSeen {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent closure error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent closures: successes=%d conflicts=%d errors=%v", successes, conflicts, errorsSeen)
	}
	var count int
	if err := first.db.QueryRow(`SELECT COUNT(*) FROM finding_closures WHERE finding_id = 'finding_concurrent'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("finding closure count = %d, %v", count, err)
	}
}

func TestDoctorBoundaryAlsoAppliesToFindings(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_finding_owner_boundary")
	closeTestRun(t, ledger, "run_finding_owner_boundary")
	addTestFinding(t, ledger, "finding_owner_boundary", "run_finding_owner_boundary", "Original finding")
	if _, err := ledger.db.Exec(`DROP TRIGGER findings_no_update`); err != nil {
		t.Fatal(err)
	}
	rewritten, err := canonicalFindingPayload(FindingContent{Summary: "Rewritten by database owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`UPDATE findings SET payload = ?, payload_sha256 = ? WHERE id = 'finding_owner_boundary'`,
		rewritten, payloadDigest(rewritten)); err != nil {
		t.Fatal(err)
	}
	var triggerSQL string
	for _, statement := range schemaStatements {
		if strings.Contains(statement, "CREATE TRIGGER findings_no_update") {
			triggerSQL = statement
			break
		}
	}
	if triggerSQL == "" {
		t.Fatal("findings_no_update definition not found")
	}
	if _, err := ledger.db.Exec(triggerSQL); err != nil {
		t.Fatal(err)
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor() = %#v, %v; local consistency is not owner-resistant authentication", report, err)
	}
	finding, err := ledger.GetFinding(context.Background(), "finding_owner_boundary")
	if err != nil || finding.Content.Summary != "Rewritten by database owner" {
		t.Fatalf("rewritten finding = %#v, %v", finding, err)
	}
}
