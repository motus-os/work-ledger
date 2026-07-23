package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func getRun(ctx context.Context, q sqlReadWriter, runID string) (Run, error) {
	run, err := scanRun(q.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	if err != nil {
		return Run{}, fmt.Errorf("read run %q: %w", runID, err)
	}
	return run, nil
}

// GetRun returns one durable run projection.
func (s *Store) GetRun(ctx context.Context, runID string) (Run, error) {
	if err := validateOpaque("run ID", runID, 255); err != nil {
		return Run{}, err
	}
	return getRun(ctx, s.db, runID)
}

func getEventByID(ctx context.Context, q sqlReadWriter, eventID string) (Event, error) {
	event, err := scanEvent(q.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, fmt.Errorf("%w: event %q", ErrNotFound, eventID)
	}
	if err != nil {
		return Event{}, fmt.Errorf("read event %q: %w", eventID, err)
	}
	return event, nil
}

func (s *Store) Snapshot(ctx context.Context, runID string) (Snapshot, error) {
	if err := validateOpaque("run ID", runID, 255); err != nil {
		return Snapshot{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin snapshot: %w", err)
	}
	defer tx.Rollback()
	run, err := getRun(ctx, tx, runID)
	if err != nil {
		return Snapshot{}, err
	}
	events, err := listEvents(ctx, tx, runID)
	if err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("commit read snapshot: %w", err)
	}
	return Snapshot{Run: run, Events: events}, nil
}

func listEvents(ctx context.Context, q sqlReadWriter, runID string) ([]Event, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE run_id = ? ORDER BY sequence`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list events for run %q: %w", runID, err)
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event for run %q: %w", runID, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events for run %q: %w", runID, err)
	}
	return events, nil
}

func (s *Store) ListRuns(ctx context.Context, options ListRunsOptions) ([]Run, error) {
	if options.Limit == 0 {
		options.Limit = 100
	}
	if options.Limit < 0 || options.Limit > 1000 {
		return nil, fmt.Errorf("%w: list limit must be between 1 and 1000", ErrInvalid)
	}
	if options.Offset < 0 {
		return nil, fmt.Errorf("%w: list offset cannot be negative", ErrInvalid)
	}
	if options.State != "" && options.State != RunOpen && options.State != RunClosed {
		return nil, fmt.Errorf("%w: invalid run state %q", ErrInvalid, options.State)
	}
	if options.Outcome != "" {
		switch options.Outcome {
		case OutcomeSuccess, OutcomeFailure, OutcomeAborted:
		default:
			return nil, fmt.Errorf("%w: invalid outcome %q", ErrInvalid, options.Outcome)
		}
	}
	query := `SELECT ` + runColumns + ` FROM runs`
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 4)
	if options.State != "" {
		conditions = append(conditions, `state = ?`)
		args = append(args, options.State)
	}
	if options.Outcome != "" {
		conditions = append(conditions, `outcome = ?`)
		args = append(args, options.Outcome)
	}
	if len(conditions) != 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY started_at DESC, id ASC LIMIT ? OFFSET ?`
	args = append(args, options.Limit, options.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	runs := make([]Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan listed run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return runs, nil
}
