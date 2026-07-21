package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const terminalEventKind = "run.terminal"
const startEventKind = "run.started"

func (s *Store) StartRun(ctx context.Context, params StartRunParams) (Run, error) {
	if err := validateStartRun(params); err != nil {
		return Run{}, err
	}
	startedAt, _ := normalizedRequiredTime("started_at", params.StartedAt)
	params.StartedAt = startedAt
	createdAt := s.now()
	payload, err := canonicalStartPayload(params)
	if err != nil {
		return Run{}, err
	}
	var result Run
	err = s.write(ctx, func(tx *sql.Tx) error {
		existing, err := getRun(ctx, tx, params.ID)
		if err == nil {
			if !sameStart(existing, params) || existing.StartEventID != params.EventID {
				return fmt.Errorf("%w: run ID %q already has different start metadata", ErrConflict, params.ID)
			}
			event, err := getEventByID(ctx, tx, params.EventID)
			if err != nil {
				return fmt.Errorf("existing run start event: %w", err)
			}
			if event.RunID != params.ID || event.Sequence != 1 || event.Kind != startEventKind ||
				event.Terminal || !bytes.Equal(event.Payload, payload) {
				return fmt.Errorf("%w: start event %q differs", ErrConflict, params.EventID)
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if _, err := getEventByID(ctx, tx, params.EventID); err == nil {
			return fmt.Errorf("%w: event ID %q already exists", ErrConflict, params.EventID)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO runs (
                id, state, started_at, ended_at, created_at, updated_at,
                source, producer, executable_basename, argument_count,
	                git_repository, git_commit, git_dirty,
                outcome, exit_code, signal, timed_out,
	                stdout_bytes, stderr_bytes, stdout_lines, stderr_lines, start_event_id, terminal_event_id
	            ) VALUES (?, 'open', ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	                      NULL, NULL, NULL, NULL, 0, 0, 0, 0, ?, NULL)`,
			params.ID, formatTime(params.StartedAt), formatTime(createdAt), formatTime(createdAt),
			params.Source, params.Producer, params.ExecutableBasename, params.ArgumentCount,
			optionalStringDatabaseValue(params.Git.Repository),
			optionalStringDatabaseValue(params.Git.Commit),
			boolDatabaseValue(params.Git.Dirty),
			params.EventID,
		)
		if err != nil {
			return fmt.Errorf("insert run: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO events (
	                id, run_id, sequence, kind, payload, payload_sha256, recorded_at, terminal
	            ) VALUES (?, ?, 1, ?, ?, ?, ?, 0)`,
			params.EventID, params.ID, startEventKind, payload, payloadDigest(payload), formatTime(createdAt),
		)
		if err != nil {
			return fmt.Errorf("append start event: %w", err)
		}
		result, err = getRun(ctx, tx, params.ID)
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return result, nil
}

func validateStartRun(params StartRunParams) error {
	if err := validateOpaque("run ID", params.ID, 255); err != nil {
		return err
	}
	if err := validateOpaque("start event ID", params.EventID, 255); err != nil {
		return err
	}
	if _, err := normalizedRequiredTime("started_at", params.StartedAt); err != nil {
		return err
	}
	if err := validateOpaque("source", params.Source, 255); err != nil {
		return err
	}
	if err := validateOpaque("producer", params.Producer, 255); err != nil {
		return err
	}
	if err := validateOpaque("executable_basename", params.ExecutableBasename, 255); err != nil {
		return err
	}
	if params.ExecutableBasename == "." || params.ExecutableBasename == ".." ||
		strings.ContainsAny(params.ExecutableBasename, `/\`) {
		return fmt.Errorf("%w: executable_basename must not contain a path", ErrInvalid)
	}
	if params.ArgumentCount < 0 {
		return fmt.Errorf("%w: argument_count cannot be negative", ErrInvalid)
	}
	if err := validateOptional("git repository", params.Git.Repository, 4096); err != nil {
		return err
	}
	if err := validateOptional("git commit", params.Git.Commit, 255); err != nil {
		return err
	}
	return nil
}

type startPayload struct {
	ArgumentCount      int         `json:"argument_count"`
	ExecutableBasename string      `json:"executable_basename"`
	Git                GitMetadata `json:"git"`
	Producer           string      `json:"producer"`
	Source             string      `json:"source"`
	StartedAt          string      `json:"started_at"`
}

func canonicalStartPayload(params StartRunParams) ([]byte, error) {
	raw, err := json.Marshal(startPayload{
		ArgumentCount:      params.ArgumentCount,
		ExecutableBasename: params.ExecutableBasename,
		Git:                params.Git,
		Producer:           params.Producer,
		Source:             params.Source,
		StartedAt:          formatTime(params.StartedAt),
	})
	if err != nil {
		return nil, fmt.Errorf("encode start event: %w", err)
	}
	return canonicalJSON(raw)
}

func sameStart(run Run, params StartRunParams) bool {
	return run.ID == params.ID &&
		run.StartedAt.Equal(params.StartedAt) &&
		run.Source == params.Source &&
		run.Producer == params.Producer &&
		run.ExecutableBasename == params.ExecutableBasename &&
		run.ArgumentCount == params.ArgumentCount &&
		run.Git.Repository == params.Git.Repository &&
		run.Git.Commit == params.Git.Commit &&
		equalBoolPointer(run.Git.Dirty, params.Git.Dirty)
}

func (s *Store) CloseRun(ctx context.Context, runID string, params CloseRunParams) (CloseResult, error) {
	if err := validateOpaque("run ID", runID, 255); err != nil {
		return CloseResult{}, err
	}
	if err := validateClose(params); err != nil {
		return CloseResult{}, err
	}
	endedAt, _ := normalizedRequiredTime("ended_at", params.EndedAt)
	params.EndedAt = endedAt
	payload, err := canonicalTerminalPayload(params)
	if err != nil {
		return CloseResult{}, err
	}
	recordedAt := s.now()
	var result CloseResult
	err = s.write(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if params.EndedAt.Before(run.StartedAt) {
			return fmt.Errorf("%w: ended_at precedes started_at", ErrInvalid)
		}
		if run.State == RunClosed {
			if !sameClose(run, params) || run.TerminalEventID != params.EventID {
				return fmt.Errorf("%w: run %q is already closed differently", ErrConflict, runID)
			}
			event, err := getEventByID(ctx, tx, params.EventID)
			if err != nil {
				return fmt.Errorf("closed run terminal event: %w", err)
			}
			if event.RunID != runID || !event.Terminal || event.Kind != terminalEventKind ||
				!bytes.Equal(event.Payload, payload) {
				return fmt.Errorf("%w: terminal event %q differs", ErrConflict, params.EventID)
			}
			result = CloseResult{Run: run, TerminalEvent: event}
			return nil
		}
		if _, err := getEventByID(ctx, tx, params.EventID); err == nil {
			return fmt.Errorf("%w: event ID %q already exists", ErrConflict, params.EventID)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		var sequence int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(sequence), 0) + 1 FROM events WHERE run_id = ?`, runID,
		).Scan(&sequence); err != nil {
			return fmt.Errorf("allocate terminal sequence: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO events (
                id, run_id, sequence, kind, payload, payload_sha256, recorded_at, terminal
            ) VALUES (?, ?, ?, ?, ?, ?, ?, 1)`,
			params.EventID, runID, sequence, terminalEventKind, payload, payloadDigest(payload), formatTime(recordedAt),
		)
		if err != nil {
			return fmt.Errorf("append terminal event: %w", err)
		}
		updateResult, err := tx.ExecContext(ctx, `UPDATE runs SET
                state = 'closed', ended_at = ?, updated_at = ?, outcome = ?,
                exit_code = ?, signal = ?, timed_out = ?,
                stdout_bytes = ?, stderr_bytes = ?, stdout_lines = ?, stderr_lines = ?,
                terminal_event_id = ?
             WHERE id = ? AND state = 'open'`,
			formatTime(params.EndedAt), formatTime(recordedAt), params.Outcome,
			intDatabaseValue(params.ExitCode), stringDatabaseValue(params.Signal), boolDatabaseValue(params.TimedOut),
			params.Output.StdoutBytes, params.Output.StderrBytes,
			params.Output.StdoutLines, params.Output.StderrLines,
			params.EventID, runID,
		)
		if err != nil {
			return fmt.Errorf("close run: %w", err)
		}
		if affected, err := updateResult.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return fmt.Errorf("close run rows affected: %w", err)
			}
			return fmt.Errorf("%w: run %q changed while closing", ErrConflict, runID)
		}
		closedRun, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		terminalEvent, err := getEventByID(ctx, tx, params.EventID)
		if err != nil {
			return err
		}
		result = CloseResult{Run: closedRun, TerminalEvent: terminalEvent}
		return nil
	})
	if err != nil {
		return CloseResult{}, err
	}
	return result, nil
}

func validateClose(params CloseRunParams) error {
	if err := validateOpaque("terminal event ID", params.EventID, 255); err != nil {
		return err
	}
	if _, err := normalizedRequiredTime("ended_at", params.EndedAt); err != nil {
		return err
	}
	switch params.Outcome {
	case OutcomeSuccess, OutcomeFailure, OutcomeAborted:
	default:
		return fmt.Errorf("%w: invalid terminal outcome %q", ErrInvalid, params.Outcome)
	}
	if params.Signal != nil {
		if err := validateOpaque("signal", *params.Signal, 255); err != nil {
			return err
		}
	}
	if params.Output.StdoutBytes < 0 || params.Output.StderrBytes < 0 ||
		params.Output.StdoutLines < 0 || params.Output.StderrLines < 0 {
		return fmt.Errorf("%w: output counts cannot be negative", ErrInvalid)
	}
	if params.Output.StdoutLines > params.Output.StdoutBytes ||
		params.Output.StderrLines > params.Output.StderrBytes {
		return fmt.Errorf("%w: output newline counts cannot exceed byte counts", ErrInvalid)
	}
	if params.ExitCode != nil && *params.ExitCode < 0 {
		return fmt.Errorf("%w: exit_code cannot be negative", ErrInvalid)
	}
	if params.Outcome == OutcomeSuccess &&
		(params.ExitCode == nil || *params.ExitCode != 0 || params.Signal != nil ||
			(params.TimedOut != nil && *params.TimedOut)) {
		return fmt.Errorf("%w: success requires exit_code 0 without a signal or timeout", ErrInvalid)
	}
	if params.TimedOut != nil && *params.TimedOut && params.Outcome != OutcomeAborted {
		return fmt.Errorf("%w: a timed-out run must be aborted", ErrInvalid)
	}
	return nil
}

type terminalPayload struct {
	EndedAt  string       `json:"ended_at"`
	ExitCode *int         `json:"exit_code,omitempty"`
	Outcome  Outcome      `json:"outcome"`
	Output   OutputCounts `json:"output"`
	Signal   *string      `json:"signal,omitempty"`
	TimedOut *bool        `json:"timed_out,omitempty"`
}

func canonicalTerminalPayload(params CloseRunParams) ([]byte, error) {
	raw, err := json.Marshal(terminalPayload{
		EndedAt:  formatTime(params.EndedAt),
		ExitCode: params.ExitCode,
		Outcome:  params.Outcome,
		Output:   params.Output,
		Signal:   params.Signal,
		TimedOut: params.TimedOut,
	})
	if err != nil {
		return nil, fmt.Errorf("encode terminal event: %w", err)
	}
	return canonicalJSON(raw)
}

func sameClose(run Run, params CloseRunParams) bool {
	return run.EndedAt != nil && run.EndedAt.Equal(params.EndedAt) &&
		run.Outcome == params.Outcome &&
		equalIntPointer(run.ExitCode, params.ExitCode) &&
		equalStringPointer(run.Signal, params.Signal) &&
		equalBoolPointer(run.TimedOut, params.TimedOut) &&
		run.Output == params.Output
}

func equalBoolPointer(left, right *bool) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func equalIntPointer(left, right *int) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func equalStringPointer(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}
