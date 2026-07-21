package store

import (
	"encoding/json"
	"errors"
	"time"
)

const (
	// SchemaVersion is the only schema version understood by this package.
	SchemaVersion = 1
	// DatabaseFilename is the fixed database name beneath a Store state directory.
	DatabaseFilename = "ledger.db"
	// MaxEventPayloadBytes bounds canonical event payload storage.
	MaxEventPayloadBytes = 1 << 20
)

var (
	ErrNotFound        = errors.New("store: not found")
	ErrConflict        = errors.New("store: idempotency conflict")
	ErrOpen            = errors.New("store: run is open")
	ErrReadOnly        = errors.New("store: read-only")
	ErrBusy            = errors.New("store: sqlite busy retry exhausted")
	ErrInvalid         = errors.New("store: invalid argument")
	ErrInvalidSchema   = errors.New("store: invalid schema")
	ErrUnsafeStatePath = errors.New("store: unsafe state path")
)

type RunState string

const (
	RunOpen   RunState = "open"
	RunClosed RunState = "closed"
)

type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomeAborted Outcome = "aborted"
)

// GitMetadata is immutable start metadata. Empty strings and a nil Dirty value
// represent unavailable metadata rather than inferred values.
type GitMetadata struct {
	Repository string `json:"repository,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Dirty      *bool  `json:"dirty,omitempty"`
}

// OutputCounts records counts only; raw output is never stored by this package.
type OutputCounts struct {
	StdoutBytes int64 `json:"stdout_bytes"`
	StderrBytes int64 `json:"stderr_bytes"`
	StdoutLines int64 `json:"stdout_lines"`
	StderrLines int64 `json:"stderr_lines"`
}

// Run is the durable run projection.
type Run struct {
	ID                 string       `json:"id"`
	State              RunState     `json:"state"`
	StartedAt          time.Time    `json:"started_at"`
	EndedAt            *time.Time   `json:"ended_at,omitempty"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
	Source             string       `json:"source"`
	Producer           string       `json:"producer"`
	ExecutableBasename string       `json:"executable_basename"`
	ArgumentCount      int          `json:"argument_count"`
	Git                GitMetadata  `json:"git"`
	Outcome            Outcome      `json:"outcome,omitempty"`
	ExitCode           *int         `json:"exit_code,omitempty"`
	Signal             *string      `json:"signal,omitempty"`
	TimedOut           *bool        `json:"timed_out,omitempty"`
	Output             OutputCounts `json:"output"`
	StartEventID       string       `json:"start_event_id"`
	TerminalEventID    string       `json:"terminal_event_id,omitempty"`
}

// Event is an immutable, append-only event. Payload is canonical JSON and
// PayloadSHA256 is the lowercase hexadecimal SHA-256 digest of those exact
// payload bytes.
type Event struct {
	ID            string          `json:"id"`
	RunID         string          `json:"run_id"`
	Sequence      int64           `json:"sequence"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	PayloadSHA256 string          `json:"payload_sha256"`
	RecordedAt    time.Time       `json:"recorded_at"`
	Terminal      bool            `json:"terminal"`
}

type StartRunParams struct {
	ID                 string
	EventID            string
	StartedAt          time.Time
	Source             string
	Producer           string
	ExecutableBasename string
	ArgumentCount      int
	Git                GitMetadata
}

type CloseRunParams struct {
	// EventID is the idempotency key for the atomically appended terminal event.
	EventID  string
	EndedAt  time.Time
	Outcome  Outcome
	ExitCode *int
	Signal   *string
	TimedOut *bool
	Output   OutputCounts
}

type CloseResult struct {
	Run           Run   `json:"run"`
	TerminalEvent Event `json:"terminal_event"`
}

type Snapshot struct {
	Run    Run     `json:"run"`
	Events []Event `json:"events"`
}

// ListRunsOptions controls deterministic newest-first listing. Limit defaults
// to 100 and cannot exceed 1000.
type ListRunsOptions struct {
	State   RunState
	Outcome Outcome
	Limit   int
	Offset  int
}

// Receipt contains a deterministic JSON envelope and the lowercase
// hexadecimal SHA-256 digest of the envelope's receipt member. It is a
// producer-controlled process record, not an attestation. JSON is returned as
// a defensive copy by Bytes.
type Receipt struct {
	JSON         []byte
	DigestSHA256 string
}

func (r Receipt) Bytes() []byte {
	return append([]byte(nil), r.JSON...)
}

type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// DoctorReport reports local consistency only. It does not establish
// authenticity or independence from the producer controlling the database.
type DoctorReport struct {
	Scope      string        `json:"scope"`
	Consistent bool          `json:"consistent"`
	Checks     []DoctorCheck `json:"checks"`
}
