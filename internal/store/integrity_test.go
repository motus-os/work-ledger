package store

import "testing"

func TestDatabaseGuardsRejectDirectMutation(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_guard")
	closeTestRun(t, ledger, "run_guard")

	attempts := []struct {
		name string
		stmt string
	}{
		{"delete run", `DELETE FROM runs WHERE id = 'run_guard'`},
		{"delete event", `DELETE FROM events WHERE id = 'run_guard_started'`},
		{"update event", `UPDATE events SET payload = '{}' WHERE id = 'run_guard_started'`},
		{"update identity", `UPDATE runs SET producer = 'changed' WHERE id = 'run_guard'`},
		{"update terminal", `UPDATE runs SET outcome = 'failure' WHERE id = 'run_guard'`},
		{"delete metadata", `DELETE FROM metadata WHERE key = 'schema_version'`},
		{"update metadata", `UPDATE metadata SET value = '2' WHERE key = 'schema_version'`},
	}
	for _, attempt := range attempts {
		t.Run(attempt.name, func(t *testing.T) {
			if _, err := ledger.db.Exec(attempt.stmt); err == nil {
				t.Fatalf("statement unexpectedly succeeded: %s", attempt.stmt)
			}
		})
	}
}

func TestEventConstraintsRejectGapsAndDuplicateTerminal(t *testing.T) {
	ledger := openTestStore(t)
	startTestRun(t, ledger, "run_constraints")
	payload := []byte(`{}`)
	if _, err := ledger.db.Exec(`INSERT INTO events
		(id, run_id, sequence, kind, payload, payload_sha256, recorded_at, terminal)
		VALUES ('gap', 'run_constraints', 3, 'run.terminal', ?, ?, '2026-07-21T12:00:00.000000000Z', 1)`,
		payload, payloadDigest(payload)); err == nil {
		t.Fatalf("out-of-sequence insert succeeded")
	}
	if _, err := ledger.db.Exec(`INSERT INTO events
		(id, run_id, sequence, kind, payload, payload_sha256, recorded_at, terminal)
		VALUES ('fake_terminal', 'run_constraints', 2, 'run.terminal', ?, ?, '2026-07-21T12:00:00.000000000Z', 1)`,
		payload, payloadDigest(payload)); err != nil {
		t.Fatalf("initial direct terminal insert: %v", err)
	}
	if _, err := ledger.db.Exec(`UPDATE runs SET
		state = 'closed', ended_at = '2026-07-21T12:00:01.000000000Z',
		updated_at = '2026-07-21T12:00:01.000000000Z', outcome = 'success',
		exit_code = 1, timed_out = 0, terminal_event_id = 'fake_terminal'
		WHERE id = 'run_constraints'`); err == nil {
		t.Fatalf("contradictory successful close succeeded")
	}
	if _, err := ledger.db.Exec(`INSERT INTO events
		(id, run_id, sequence, kind, payload, payload_sha256, recorded_at, terminal)
		VALUES ('after_terminal', 'run_constraints', 3, 'run.terminal', ?, ?, '2026-07-21T12:00:00.000000000Z', 1)`,
		payload, payloadDigest(payload)); err == nil {
		t.Fatalf("insert after terminal succeeded")
	}
}
