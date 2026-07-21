package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const receiptSchema = "motus.work-receipt.v1"

type receiptEnvelope struct {
	Schema        string      `json:"schema"`
	Receipt       receiptBody `json:"receipt"`
	ReceiptSHA256 string      `json:"receipt_sha256"`
}

type receiptBody struct {
	TrustModel string         `json:"trust_model"`
	Run        receiptRun     `json:"run"`
	Events     []receiptEvent `json:"events"`
}

type receiptRun struct {
	ID                 string       `json:"id"`
	State              RunState     `json:"state"`
	StartedAt          string       `json:"started_at"`
	EndedAt            string       `json:"ended_at"`
	CreatedAt          string       `json:"created_at"`
	UpdatedAt          string       `json:"updated_at"`
	Source             string       `json:"source"`
	Producer           string       `json:"producer"`
	ExecutableBasename string       `json:"executable_basename"`
	ArgumentCount      int          `json:"argument_count"`
	Git                GitMetadata  `json:"git"`
	Outcome            Outcome      `json:"outcome"`
	ExitCode           *int         `json:"exit_code,omitempty"`
	Signal             *string      `json:"signal,omitempty"`
	TimedOut           *bool        `json:"timed_out,omitempty"`
	Output             OutputCounts `json:"output"`
	StartEventID       string       `json:"start_event_id"`
	TerminalEventID    string       `json:"terminal_event_id"`
}

type receiptEvent struct {
	ID            string          `json:"id"`
	Sequence      int64           `json:"sequence"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	PayloadSHA256 string          `json:"payload_sha256"`
	RecordedAt    string          `json:"recorded_at"`
	Terminal      bool            `json:"terminal"`
}

// ProjectReceipt returns a deterministic, self-checking projection of a
// closed run. The digest establishes only that the receipt member matches the
// bytes projected into this envelope. It does not establish who produced the
// underlying record or whether the recorded claims are true.
func (s *Store) ProjectReceipt(ctx context.Context, runID string) (Receipt, error) {
	snapshot, err := s.Snapshot(ctx, runID)
	if err != nil {
		return Receipt{}, err
	}
	if snapshot.Run.State != RunClosed {
		return Receipt{}, fmt.Errorf("%w: run %q", ErrOpen, runID)
	}
	if err := validateSnapshotConsistency(snapshot); err != nil {
		return Receipt{}, fmt.Errorf("project receipt: %w", err)
	}

	body := receiptBody{
		TrustModel: "producer-controlled",
		Run:        projectReceiptRun(snapshot.Run),
		Events:     make([]receiptEvent, 0, len(snapshot.Events)),
	}
	for _, event := range snapshot.Events {
		body.Events = append(body.Events, receiptEvent{
			ID:            event.ID,
			Sequence:      event.Sequence,
			Kind:          event.Kind,
			Payload:       append(json.RawMessage(nil), event.Payload...),
			PayloadSHA256: event.PayloadSHA256,
			RecordedAt:    formatTime(event.RecordedAt),
			Terminal:      event.Terminal,
		})
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return Receipt{}, fmt.Errorf("encode receipt member: %w", err)
	}
	digest := sha256.Sum256(bodyJSON)
	digestHex := hex.EncodeToString(digest[:])
	envelopeJSON, err := json.Marshal(receiptEnvelope{
		Schema:        receiptSchema,
		Receipt:       body,
		ReceiptSHA256: digestHex,
	})
	if err != nil {
		return Receipt{}, fmt.Errorf("encode receipt envelope: %w", err)
	}
	envelopeJSON = append(envelopeJSON, '\n')
	return Receipt{JSON: envelopeJSON, DigestSHA256: digestHex}, nil
}

func projectReceiptRun(run Run) receiptRun {
	endedAt := ""
	if run.EndedAt != nil {
		endedAt = formatTime(*run.EndedAt)
	}
	return receiptRun{
		ID:                 run.ID,
		State:              run.State,
		StartedAt:          formatTime(run.StartedAt),
		EndedAt:            endedAt,
		CreatedAt:          formatTime(run.CreatedAt),
		UpdatedAt:          formatTime(run.UpdatedAt),
		Source:             run.Source,
		Producer:           run.Producer,
		ExecutableBasename: run.ExecutableBasename,
		ArgumentCount:      run.ArgumentCount,
		Git:                run.Git,
		Outcome:            run.Outcome,
		ExitCode:           cloneInt(run.ExitCode),
		Signal:             cloneString(run.Signal),
		TimedOut:           cloneBool(run.TimedOut),
		Output:             run.Output,
		StartEventID:       run.StartEventID,
		TerminalEventID:    run.TerminalEventID,
	}
}

func validateSnapshotConsistency(snapshot Snapshot) error {
	if err := validateRunProjection(snapshot.Run); err != nil {
		return err
	}
	wantEvents := 1
	if snapshot.Run.State == RunClosed {
		wantEvents = 2
	}
	if len(snapshot.Events) != wantEvents {
		return fmt.Errorf("run has %d lifecycle events, want %d", len(snapshot.Events), wantEvents)
	}
	for index, event := range snapshot.Events {
		wantSequence := int64(index + 1)
		if event.RunID != snapshot.Run.ID {
			return fmt.Errorf("event %q belongs to run %q", event.ID, event.RunID)
		}
		if event.Sequence != wantSequence {
			return fmt.Errorf("event %q has sequence %d, want %d", event.ID, event.Sequence, wantSequence)
		}
		if got := payloadDigestHex(event.Payload); got != event.PayloadSHA256 {
			return fmt.Errorf("event %q payload digest mismatch", event.ID)
		}
		if event.Terminal != (index == len(snapshot.Events)-1 && snapshot.Run.State == RunClosed) {
			return fmt.Errorf("event %q terminal placement is inconsistent", event.ID)
		}
	}
	start := snapshot.Events[0]
	if start.ID != snapshot.Run.StartEventID || start.Kind != startEventKind || start.Terminal {
		return fmt.Errorf("start event does not match run")
	}
	expectedStart, err := canonicalStartPayload(StartRunParams{
		ID: snapshot.Run.ID, EventID: start.ID, StartedAt: snapshot.Run.StartedAt,
		Source: snapshot.Run.Source, Producer: snapshot.Run.Producer,
		ExecutableBasename: snapshot.Run.ExecutableBasename, ArgumentCount: snapshot.Run.ArgumentCount,
		Git: snapshot.Run.Git,
	})
	if err != nil {
		return err
	}
	if !bytes.Equal(expectedStart, start.Payload) {
		return fmt.Errorf("start event payload does not match run")
	}
	if snapshot.Run.State == RunClosed {
		terminal := snapshot.Events[len(snapshot.Events)-1]
		if terminal.ID != snapshot.Run.TerminalEventID || terminal.Kind != terminalEventKind {
			return fmt.Errorf("terminal event does not match closed run")
		}
		params := CloseRunParams{
			EventID:  terminal.ID,
			EndedAt:  *snapshot.Run.EndedAt,
			Outcome:  snapshot.Run.Outcome,
			ExitCode: snapshot.Run.ExitCode,
			Signal:   snapshot.Run.Signal,
			TimedOut: snapshot.Run.TimedOut,
			Output:   snapshot.Run.Output,
		}
		expected, err := canonicalTerminalPayload(params)
		if err != nil {
			return err
		}
		if !bytes.Equal(expected, terminal.Payload) {
			return fmt.Errorf("terminal event payload does not match closed run")
		}
	}
	return nil
}

func validateRunProjection(run Run) error {
	start := StartRunParams{
		ID:                 run.ID,
		EventID:            run.StartEventID,
		StartedAt:          run.StartedAt,
		Source:             run.Source,
		Producer:           run.Producer,
		ExecutableBasename: run.ExecutableBasename,
		ArgumentCount:      run.ArgumentCount,
		Git:                run.Git,
	}
	if err := validateStartRun(start); err != nil {
		return fmt.Errorf("run start metadata: %w", err)
	}
	if run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() || run.UpdatedAt.Before(run.CreatedAt) {
		return fmt.Errorf("run timestamps are inconsistent")
	}
	switch run.State {
	case RunOpen:
		if run.EndedAt != nil || run.Outcome != "" || run.ExitCode != nil || run.Signal != nil ||
			run.TimedOut != nil || run.Output != (OutputCounts{}) || run.TerminalEventID != "" {
			return fmt.Errorf("open run contains terminal metadata")
		}
	case RunClosed:
		if run.EndedAt == nil {
			return fmt.Errorf("closed run has no ended_at")
		}
		if run.EndedAt.Before(run.StartedAt) {
			return fmt.Errorf("closed run ended_at precedes started_at")
		}
		params := CloseRunParams{
			EventID:  run.TerminalEventID,
			EndedAt:  *run.EndedAt,
			Outcome:  run.Outcome,
			ExitCode: run.ExitCode,
			Signal:   run.Signal,
			TimedOut: run.TimedOut,
			Output:   run.Output,
		}
		if err := validateClose(params); err != nil {
			return fmt.Errorf("closed run terminal metadata: %w", err)
		}
	default:
		return fmt.Errorf("run has invalid state %q", run.State)
	}
	return nil
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
