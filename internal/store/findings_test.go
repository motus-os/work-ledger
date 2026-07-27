package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func addTestFinding(t *testing.T, ledger *Store, id, runID, summary string) Finding {
	t.Helper()
	return addTestFindingContent(t, ledger, id, runID, FindingContent{
		Summary:    summary,
		Hypothesis: "The cached state may be stale.",
		NextStep:   "Reproduce with a clean cache.",
	})
}

func addTestFindingContent(t *testing.T, ledger *Store, id, runID string, content FindingContent) Finding {
	t.Helper()
	finding, err := ledger.AddFinding(context.Background(), AddFindingParams{
		ID:          id,
		OriginRunID: runID,
		Content:     content,
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

func TestFindingMultiTermSearchRankingAndPagination(t *testing.T) {
	ledger := openTestStore(t)
	for _, runID := range []string{
		"run_exact", "run_all_terms", "run_partial", "run_generic",
		"run_tie_alpha", "run_tie_beta", "run_filtered",
	} {
		startTestRun(t, ledger, runID)
		closeTestRun(t, ledger, runID)
	}

	fixtures := []struct {
		at      time.Duration
		id      string
		runID   string
		content FindingContent
	}{
		{
			at:    10 * time.Second,
			id:    "finding_exact",
			runID: "run_exact",
			content: FindingContent{
				Summary:    "Repository history review used a shallow checkout",
				Hypothesis: "The local clone did not contain the remote history.",
				NextStep:   "Fetch the remote before counting commits.",
			},
		},
		{
			at:    11 * time.Second,
			id:    "finding_all_terms",
			runID: "run_all_terms",
			content: FindingContent{
				Summary:    "History count was incomplete",
				Hypothesis: "The repository checkout was shallow.",
				NextStep:   "Compare the local result with the remote.",
			},
		},
		{
			at:    12 * time.Second,
			id:    "finding_partial",
			runID: "run_partial",
			content: FindingContent{
				Summary:  "Repository branch protection review",
				NextStep: "Inspect the protected branch settings.",
			},
		},
		{
			at:    13 * time.Second,
			id:    "finding_generic_run",
			runID: "run_generic",
			content: FindingContent{
				Summary:  "Run list output changed",
				NextStep: "Compare the documented output.",
			},
		},
		{
			at:      14 * time.Second,
			id:      "finding_tie_alpha",
			runID:   "run_tie_alpha",
			content: FindingContent{Summary: "Tieword alpha"},
		},
		{
			at:      14 * time.Second,
			id:      "finding_tie_beta",
			runID:   "run_tie_beta",
			content: FindingContent{Summary: "Tieword beta"},
		},
		{
			at:      15 * time.Second,
			id:      "finding_filtered",
			runID:   "run_filtered",
			content: FindingContent{Summary: "History review of the repository was retired"},
		},
	}
	for _, fixture := range fixtures {
		ledger.now = func() time.Time { return testEpoch.Add(fixture.at) }
		addTestFindingContent(t, ledger, fixture.id, fixture.runID, fixture.content)
	}
	ledger.now = func() time.Time { return testEpoch.Add(16 * time.Second) }
	if _, err := ledger.CloseFinding(context.Background(), "finding_filtered", CloseFindingParams{
		ID:          "closure_filtered",
		Disposition: DispositionDismissed,
		Content:     FindingClosureContent{Note: "This test fixture is intentionally closed."},
	}); err != nil {
		t.Fatal(err)
	}

	rankingTests := []struct {
		query string
		want  []string
	}{
		{
			query: "repository history",
			want:  []string{"finding_exact", "finding_filtered", "finding_all_terms", "finding_partial"},
		},
		{
			query: "history, repository",
			want:  []string{"finding_filtered", "finding_all_terms", "finding_exact", "finding_partial"},
		},
		{
			query: "repository repository history",
			want:  []string{"finding_filtered", "finding_all_terms", "finding_exact", "finding_partial"},
		},
	}
	for _, test := range rankingTests {
		got, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: test.query})
		if err != nil {
			t.Fatalf("ListFindings(%q) error = %v", test.query, err)
		}
		if ids := findingIDs(got); !reflect.DeepEqual(ids, test.want) {
			t.Fatalf("ListFindings(%q) IDs = %v, want %v", test.query, ids, test.want)
		}
	}

	page, err := ledger.ListFindings(context.Background(), ListFindingsOptions{
		Query: "repository history", Limit: 2, Offset: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := findingIDs(page), []string{"finding_filtered", "finding_all_terms"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked page IDs = %v, want %v", got, want)
	}

	openOnly, err := ledger.ListFindings(context.Background(), ListFindingsOptions{
		Query: "repository history", State: FindingOpen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := findingIDs(openOnly), []string{"finding_exact", "finding_all_terms", "finding_partial"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("open ranked IDs = %v, want %v", got, want)
	}

	tied, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: "tieword"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := findingIDs(tied), []string{"finding_tie_alpha", "finding_tie_beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tie IDs = %v, want %v", got, want)
	}

	genericRun, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := findingIDs(genericRun), []string{"finding_generic_run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("generic run query IDs = %v, want %v", got, want)
	}
	genericRunPrefix, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: "run_"})
	if err != nil {
		t.Fatal(err)
	}
	if len(genericRunPrefix) != 0 {
		t.Fatalf("generic run prefix query IDs = %v, want none", findingIDs(genericRunPrefix))
	}
	for _, query := range []string{"find", "_"} {
		found, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: query, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != 0 {
			t.Fatalf("generic identifier query %q IDs = %v, want none", query, findingIDs(found))
		}
	}
	shortRun, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: "ru", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := findingIDs(shortRun), []string{"finding_generic_run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("short run query IDs = %v, want authored-text match %v", got, want)
	}

	identifier, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: "run_all_terms"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := findingIDs(identifier), []string{"finding_all_terms"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("identifier query IDs = %v, want %v", got, want)
	}

	punctuation, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: "---"})
	if err != nil {
		t.Fatal(err)
	}
	if len(punctuation) != 0 {
		t.Fatalf("punctuation-only query returned %v", findingIDs(punctuation))
	}
}

func TestFindingIdentifierFragmentSearchCompatibility(t *testing.T) {
	ledger := openTestStore(t)
	for _, runID := range []string{
		"run_22222222aaaabbbb",
		"run_44444444ccccdddd",
	} {
		startTestRun(t, ledger, runID)
		closeTestRun(t, ledger, runID)
	}
	addTestFindingContent(t, ledger,
		"finding_11111111aaaabbbb",
		"run_22222222aaaabbbb",
		FindingContent{Summary: "Identifier compatibility fixture"},
	)
	if _, err := ledger.CloseFinding(context.Background(), "finding_11111111aaaabbbb", CloseFindingParams{
		ID:             "closure_33333333bbbbaaaa",
		Disposition:    DispositionResolved,
		ResolvingRunID: "run_44444444ccccdddd",
		Content:        FindingClosureContent{Note: "Resolved for identifier search coverage."},
	}); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{
		"11111111aaaabbbb",
		"22222222aaaabbbb",
		"33333333bbbbaaaa",
		"44444444ccccdddd",
	} {
		found, err := ledger.ListFindings(context.Background(), ListFindingsOptions{Query: query})
		if err != nil {
			t.Fatalf("ListFindings(%q) error = %v", query, err)
		}
		if got, want := findingIDs(found), []string{"finding_11111111aaaabbbb"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ListFindings(%q) IDs = %v, want %v", query, got, want)
		}
	}
}

func findingIDs(findings []Finding) []string {
	ids := make([]string, len(findings))
	for index, finding := range findings {
		ids[index] = finding.ID
	}
	return ids
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
		{Query: uniqueFindingQueryTerms(65)},
	} {
		if _, err := ledger.ListFindings(context.Background(), options); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ListFindings(%#v) error = %v, want ErrInvalid", options, err)
		}
	}
	if _, err := ledger.ListFindings(context.Background(), ListFindingsOptions{
		Query: uniqueFindingQueryTerms(64),
	}); err != nil {
		t.Fatalf("ListFindings(64 unique terms) error = %v", err)
	}
	if _, err := ledger.GetFinding(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFinding(missing) error = %v, want ErrNotFound", err)
	}
}

func uniqueFindingQueryTerms(count int) string {
	terms := make([]string, count)
	for index := range terms {
		terms[index] = fmt.Sprintf("term%d", index)
	}
	return strings.Join(terms, " ")
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

func BenchmarkListFindingsQuery10K(b *testing.B) {
	ctx := context.Background()
	ledger, err := Open(ctx, filepath.Join(b.TempDir(), "state"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := ledger.Close(); err != nil {
			b.Error(err)
		}
	})
	dirty := false
	if _, err := ledger.StartRun(ctx, StartRunParams{
		ID: "run_benchmark", EventID: "run_benchmark_started", StartedAt: testEpoch,
		Source: "benchmark", Producer: "motus", ExecutableBasename: "test",
		Git: GitMetadata{Repository: "/work/project", Commit: strings.Repeat("a", 40), Dirty: &dirty},
	}); err != nil {
		b.Fatal(err)
	}
	exitCode := 0
	if _, err := ledger.CloseRun(ctx, "run_benchmark", CloseRunParams{
		EventID: "run_benchmark_terminal", EndedAt: testEpoch.Add(time.Second),
		Outcome: OutcomeSuccess, ExitCode: &exitCode,
	}); err != nil {
		b.Fatal(err)
	}
	tx, err := ledger.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	for index := range 10_000 {
		content := FindingContent{
			Summary:  fmt.Sprintf("Dependency update %05d", index),
			NextStep: "Run the focused checks before the full suite.",
		}
		switch {
		case index%100 == 0:
			content.Summary = fmt.Sprintf("Repository history review %05d", index)
			content.Hypothesis = "The clone was shallow."
			content.NextStep = "Fetch the remote history before counting commits."
		case index%10 == 0:
			content.Summary = fmt.Sprintf("Repository branch review %05d", index)
		}
		payload, err := canonicalFindingPayload(content)
		if err != nil {
			b.Fatal(err)
		}
		id := fmt.Sprintf("finding_benchmark_%05d", index)
		if _, err := tx.ExecContext(ctx, `INSERT INTO findings (
            id, origin_run_id, payload, payload_sha256, recorded_at
        ) VALUES (?, ?, ?, ?, ?)`,
			id, "run_benchmark", payload, payloadDigest(payload),
			formatTime(testEpoch.Add(time.Duration(index)*time.Millisecond)),
		); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		query string
	}{
		{name: "exact_phrase", query: "repository history"},
		{name: "reordered_terms", query: "history repository"},
		{name: "specific_record", query: "dependency update 09999"},
		{name: "no_match", query: "absent phrase"},
		{name: "maximum_terms", query: uniqueFindingQueryTerms(64)},
	} {
		b.Run(test.name, func(b *testing.B) {
			for range b.N {
				if _, err := ledger.ListFindings(ctx, ListFindingsOptions{
					Query: test.query,
					Limit: 20,
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkFindingQueryMatchMaximumContent(b *testing.B) {
	query, err := newFindingQuery(uniqueFindingQueryTerms(64))
	if err != nil {
		b.Fatal(err)
	}
	finding := Finding{
		ID:          "finding_abcdef1234567890",
		OriginRunID: "run_abcdef1234567890",
		Content: FindingContent{
			Summary:    strings.Repeat("s", maxFindingSummaryBytes),
			Hypothesis: strings.Repeat("h", maxFindingTextBytes),
			NextStep:   strings.Repeat("n", maxFindingTextBytes),
		},
		Closure: &FindingClosure{
			ID:             "closure_abcdef1234567890",
			ResolvingRunID: "run_1234567890abcdef",
			Content: FindingClosureContent{
				Note: strings.Repeat("c", maxFindingTextBytes),
			},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	var result findingMatchScore
	for range b.N {
		result = query.match(finding)
	}
	if result.matched() {
		b.Fatal("unexpected benchmark match")
	}
}
