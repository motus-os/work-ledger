package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const doctorScope = "local consistency only; producer-controlled records are not independently authenticated"

// Doctor checks the current database structure and internal consistency. A
// successful report does not prove that a database owner never altered the
// database or that recorded claims are true.
func (s *Store) Doctor(ctx context.Context) (DoctorReport, error) {
	if s == nil || s.db == nil {
		return DoctorReport{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	report := DoctorReport{Scope: doctorScope, Consistent: true}
	add := func(name string, err error, success string) {
		check := DoctorCheck{Name: name, OK: err == nil, Detail: success}
		if err != nil {
			check.Detail = err.Error()
			report.Consistent = false
		}
		report.Checks = append(report.Checks, check)
	}

	add("state path", verifyStatePath(s.stateDir, s.database), "state directory and database paths pass local safety checks")
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DoctorReport{}, fmt.Errorf("begin doctor snapshot: %w", err)
	}
	defer tx.Rollback()

	add("schema", validateSchema(ctx, tx), "schema version and required objects are present")
	add("sqlite integrity", checkSQLiteIntegrity(ctx, tx), "SQLite integrity_check returned ok")
	add("foreign keys", checkForeignKeys(ctx, tx), "no foreign-key violations")
	add("metadata", checkMetadata(ctx, tx), "metadata keys and values are valid")
	add("event payload digests", checkEventPayloadDigests(ctx, tx), "all event payload hashes match their stored bytes")
	add("run event sequences", checkRunSequences(ctx, tx), "event sequences are contiguous")
	add("run terminal records", checkRunTerminalRecords(ctx, tx), "terminal events match their run projections")

	if err := tx.Commit(); err != nil {
		return DoctorReport{}, fmt.Errorf("commit doctor snapshot: %w", err)
	}
	return report, nil
}

func checkSQLiteIntegrity(ctx context.Context, q sqlReadWriter) error {
	rows, err := q.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("run SQLite integrity_check: %w", err)
	}
	defer rows.Close()
	var failures []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("read SQLite integrity_check: %w", err)
		}
		if result != "ok" {
			failures = append(failures, result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read SQLite integrity_check: %w", err)
	}
	if len(failures) != 0 {
		return fmt.Errorf("SQLite integrity_check failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func checkForeignKeys(ctx context.Context, q sqlReadWriter) error {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("run foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID any
		var parent string
		var foreignKeyID int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			return fmt.Errorf("read foreign_key_check: %w", err)
		}
		return fmt.Errorf("foreign-key violation in table %q referencing %q", table, parent)
	}
	return rows.Err()
}

func checkMetadata(ctx context.Context, q sqlReadWriter) error {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM metadata ORDER BY key`)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return fmt.Errorf("scan metadata: %w", err)
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	if len(values) != 2 || values["schema_version"] != "1" {
		return fmt.Errorf("metadata rows are incomplete or unexpected")
	}
	if _, err := parseTime(values["created_at"]); err != nil {
		return fmt.Errorf("metadata created_at: %w", err)
	}
	return nil
}

func checkEventPayloadDigests(ctx context.Context, q sqlReadWriter) error {
	rows, err := q.QueryContext(ctx, `SELECT id, payload, payload_sha256 FROM events ORDER BY run_id, sequence`)
	if err != nil {
		return fmt.Errorf("read event payloads: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var payload, stored []byte
		if err := rows.Scan(&id, &payload, &stored); err != nil {
			return fmt.Errorf("scan event payload: %w", err)
		}
		expected := payloadDigest(payload)
		if !bytes.Equal(stored, expected) {
			return fmt.Errorf("event %q payload hash mismatch (stored %s, computed %s)",
				id, hex.EncodeToString(stored), hex.EncodeToString(expected))
		}
	}
	return rows.Err()
}

func checkRunSequences(ctx context.Context, q sqlReadWriter) error {
	rows, err := q.QueryContext(ctx, `SELECT run_id, sequence FROM events ORDER BY run_id, sequence`)
	if err != nil {
		return fmt.Errorf("read event sequences: %w", err)
	}
	defer rows.Close()
	next := make(map[string]int64)
	for rows.Next() {
		var runID string
		var sequence int64
		if err := rows.Scan(&runID, &sequence); err != nil {
			return fmt.Errorf("scan event sequence: %w", err)
		}
		want := next[runID] + 1
		if sequence != want {
			return fmt.Errorf("run %q sequence %d follows %d", runID, sequence, next[runID])
		}
		next[runID] = sequence
	}
	return rows.Err()
}

func checkRunTerminalRecords(ctx context.Context, q sqlReadWriter) error {
	rows, err := q.QueryContext(ctx, `SELECT id FROM runs ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return fmt.Errorf("scan run ID: %w", err)
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close run list: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	sort.Strings(runIDs)
	for _, runID := range runIDs {
		run, err := getRun(ctx, q, runID)
		if err != nil {
			return err
		}
		events, err := listEvents(ctx, q, runID)
		if err != nil {
			return err
		}
		if err := validateSnapshotConsistency(Snapshot{Run: run, Events: events}); err != nil {
			return fmt.Errorf("run %q: %w", runID, err)
		}
	}
	return nil
}
