package store

import (
	"context"
	"strings"
	"testing"
)

func TestDoctorCleanAndTamperedStore(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_doctor")
	closeTestRun(t, ledger, "run_doctor")

	report, err := ledger.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if !report.Consistent || len(report.Checks) != 8 {
		t.Fatalf("Doctor() = %#v", report)
	}
	if !strings.Contains(report.Scope, "producer-controlled") {
		t.Fatalf("Doctor scope = %q", report.Scope)
	}

	if _, err := ledger.db.Exec(`DROP TRIGGER events_no_update`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := ledger.db.Exec(`UPDATE events SET payload = ? WHERE id = 'run_doctor_started'`, []byte(`{"ok":false}`)); err != nil {
		t.Fatalf("tamper payload: %v", err)
	}
	report, err = ledger.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor(tampered) error = %v", err)
	}
	if report.Consistent {
		t.Fatalf("Doctor() reported tampered store consistent: %#v", report)
	}
	failed := make(map[string]bool)
	for _, check := range report.Checks {
		if !check.OK {
			failed[check.Name] = true
		}
	}
	if !failed["schema"] || !failed["event payload digests"] || !failed["run terminal records"] {
		t.Fatalf("Doctor() failed checks = %#v", failed)
	}
}

func TestDoctorRejectsLookalikeRequiredTrigger(t *testing.T) {
	ledger := openTestStore(t)
	if _, err := ledger.db.Exec(`DROP TRIGGER runs_no_delete`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := ledger.db.Exec(`CREATE TRIGGER runs_no_delete BEFORE DELETE ON runs BEGIN SELECT 1; END`); err != nil {
		t.Fatalf("create lookalike trigger: %v", err)
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if report.Consistent {
		t.Fatalf("Doctor() accepted altered required trigger")
	}
	for _, check := range report.Checks {
		if check.Name == "schema" && !check.OK && strings.Contains(check.Detail, "runs_no_delete") {
			return
		}
	}
	t.Fatalf("Doctor() did not identify altered trigger: %#v", report.Checks)
}

func TestDoctorRejectsUnexpectedSchemaObject(t *testing.T) {
	ledger := openTestStore(t)
	if _, err := ledger.db.Exec(`CREATE TABLE unexpected(value TEXT) STRICT`); err != nil {
		t.Fatal(err)
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Consistent {
		t.Fatalf("Doctor() accepted an unexpected table")
	}
}

func TestDoctorIsNotAuthenticationAgainstDatabaseOwner(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_owner_boundary")
	if _, err := ledger.db.Exec(`DROP TRIGGER runs_no_identity_mutation`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`DROP TRIGGER events_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`UPDATE runs SET producer = 'rewritten-by-owner' WHERE id = 'run_owner_boundary'`); err != nil {
		t.Fatal(err)
	}
	rewritten, err := getRun(context.Background(), ledger.db, "run_owner_boundary")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := canonicalStartPayload(StartRunParams{
		ID: rewritten.ID, EventID: rewritten.StartEventID, StartedAt: rewritten.StartedAt,
		Source: rewritten.Source, Producer: rewritten.Producer,
		ExecutableBasename: rewritten.ExecutableBasename, ArgumentCount: rewritten.ArgumentCount,
		Git: rewritten.Git,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`UPDATE events SET payload = ?, payload_sha256 = ? WHERE id = ?`,
		payload, payloadDigest(payload), rewritten.StartEventID); err != nil {
		t.Fatal(err)
	}
	triggerSQL := make(map[string]string)
	for _, statement := range schemaStatements {
		for _, name := range []string{"runs_no_identity_mutation", "events_no_update"} {
			if strings.Contains(statement, "CREATE TRIGGER "+name) {
				triggerSQL[name] = statement
			}
		}
	}
	if len(triggerSQL) != 2 {
		t.Fatal("required trigger definition not found")
	}
	for _, name := range []string{"runs_no_identity_mutation", "events_no_update"} {
		if _, err := ledger.db.Exec(triggerSQL[name]); err != nil {
			t.Fatal(err)
		}
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Consistent {
		t.Fatalf("Doctor() = %#v; local consistency should not be presented as owner-resistant authentication", report)
	}
	run, err := getRun(context.Background(), ledger.db, "run_owner_boundary")
	if err != nil || run.Producer != "rewritten-by-owner" {
		t.Fatalf("rewritten run = %#v, %v", run, err)
	}
}
