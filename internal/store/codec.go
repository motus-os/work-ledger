package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const timestampLayout = "2006-01-02T15:04:05.000000000Z"

func formatTime(value time.Time) string {
	return value.UTC().Round(0).Format(timestampLayout)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(timestampLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	return parsed, nil
}

func normalizedRequiredTime(name string, value time.Time) (time.Time, error) {
	if value.IsZero() {
		return time.Time{}, fmt.Errorf("%w: %s is required", ErrInvalid, name)
	}
	normalized := value.UTC().Round(0)
	if normalized.Year() < 1 || normalized.Year() > 9999 {
		return time.Time{}, fmt.Errorf("%w: %s year is outside 0001..9999", ErrInvalid, name)
	}
	return normalized, nil
}

func validateOpaque(name, value string, maxBytes int) error {
	if value == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalid, name, maxBytes)
	}
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: %s must be valid UTF-8 without NUL", ErrInvalid, name)
	}
	return nil
}

func validateOptional(name, value string, maxBytes int) error {
	if value == "" {
		return nil
	}
	return validateOpaque(name, value, maxBytes)
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	return canonicalJSONNamed(raw, MaxEventPayloadBytes, "event")
}

func canonicalJSONNamed(raw json.RawMessage, maxBytes int, name string) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: %s payload is required", ErrInvalid, name)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: %s payload must be valid UTF-8", ErrInvalid, name)
	}
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("%w: %s payload exceeds %d bytes", ErrInvalid, name, maxBytes)
	}
	if err := validateJSONUnicodeEscapes(raw); err != nil {
		return nil, fmt.Errorf("%w: invalid %s JSON: %v", ErrInvalid, name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid %s JSON: %v", ErrInvalid, name, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			err = errorsNewTrailingJSON
		}
		return nil, fmt.Errorf("%w: invalid %s JSON: %v", ErrInvalid, name, err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize %s JSON: %w", name, err)
	}
	if len(canonical) > maxBytes {
		return nil, fmt.Errorf("%w: canonical %s payload exceeds %d bytes", ErrInvalid, name, maxBytes)
	}
	return canonical, nil
}

func validateJSONUnicodeEscapes(raw []byte) error {
	inString := false
	for index := 0; index < len(raw); index++ {
		switch raw[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(raw) {
				continue
			}
			if raw[index+1] != 'u' {
				index++
				continue
			}
			unit, ok := parseJSONHexQuad(raw, index+2)
			if !ok {
				return fmt.Errorf("invalid Unicode escape")
			}
			switch {
			case unit >= 0xd800 && unit <= 0xdbff:
				if index+11 >= len(raw) || raw[index+6] != '\\' || raw[index+7] != 'u' {
					return fmt.Errorf("unpaired high surrogate escape")
				}
				low, ok := parseJSONHexQuad(raw, index+8)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("unpaired high surrogate escape")
				}
				index += 11
			case unit >= 0xdc00 && unit <= 0xdfff:
				return fmt.Errorf("unpaired low surrogate escape")
			default:
				index += 5
			}
		}
	}
	return nil
}

func parseJSONHexQuad(raw []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, character := range raw[start : start+4] {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value |= uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

var errorsNewTrailingJSON = fmt.Errorf("trailing JSON value")

func decodeJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		switch token.(type) {
		case nil, bool, string, json.Number:
			return token, nil
		default:
			return nil, fmt.Errorf("unsupported JSON token %T", token)
		}
	}
	switch delim {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key is %T", keyToken)
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate object key %q", key)
			}
			value, err := decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if closing != json.Delim('}') {
			return nil, fmt.Errorf("unexpected object terminator %v", closing)
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if closing != json.Delim(']') {
			return nil, fmt.Errorf("unexpected array terminator %v", closing)
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func payloadDigest(payload []byte) []byte {
	digest := sha256.Sum256(payload)
	return digest[:]
}

func payloadDigestHex(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

const runColumns = `id, state, started_at, ended_at, created_at, updated_at,
    source, producer, executable_basename, argument_count,
	    git_repository, git_commit, git_dirty,
    outcome, exit_code, signal, timed_out,
	    stdout_bytes, stderr_bytes, stdout_lines, stderr_lines, start_event_id, terminal_event_id`

type rowScanner interface {
	Scan(...any) error
}

func scanRun(scanner rowScanner) (Run, error) {
	var run Run
	var startedAt, createdAt, updatedAt string
	var endedAt, gitRepository, gitCommit sql.NullString
	var gitDirty, exitCode, timedOut sql.NullInt64
	var outcome, signal, terminalEventID sql.NullString
	if err := scanner.Scan(
		&run.ID, &run.State, &startedAt, &endedAt, &createdAt, &updatedAt,
		&run.Source, &run.Producer, &run.ExecutableBasename, &run.ArgumentCount,
		&gitRepository, &gitCommit, &gitDirty,
		&outcome, &exitCode, &signal, &timedOut,
		&run.Output.StdoutBytes, &run.Output.StderrBytes, &run.Output.StdoutLines, &run.Output.StderrLines,
		&run.StartEventID, &terminalEventID,
	); err != nil {
		return Run{}, err
	}
	var err error
	if run.StartedAt, err = parseTime(startedAt); err != nil {
		return Run{}, err
	}
	if run.CreatedAt, err = parseTime(createdAt); err != nil {
		return Run{}, err
	}
	if run.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Run{}, err
	}
	if endedAt.Valid {
		value, err := parseTime(endedAt.String)
		if err != nil {
			return Run{}, err
		}
		run.EndedAt = &value
	}
	run.Git.Repository = gitRepository.String
	run.Git.Commit = gitCommit.String
	if gitDirty.Valid {
		value, err := databaseBool("git_dirty", gitDirty.Int64)
		if err != nil {
			return Run{}, err
		}
		run.Git.Dirty = &value
	}
	if outcome.Valid {
		run.Outcome = Outcome(outcome.String)
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		if int64(value) != exitCode.Int64 {
			return Run{}, fmt.Errorf("exit_code %d overflows int", exitCode.Int64)
		}
		run.ExitCode = &value
	}
	if signal.Valid {
		value := signal.String
		run.Signal = &value
	}
	if timedOut.Valid {
		value, err := databaseBool("timed_out", timedOut.Int64)
		if err != nil {
			return Run{}, err
		}
		run.TimedOut = &value
	}
	run.TerminalEventID = terminalEventID.String
	return run, nil
}

func databaseBool(name string, value int64) (bool, error) {
	switch value {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("%s has non-boolean value %d", name, value)
	}
}

const eventColumns = `id, run_id, sequence, kind, payload, payload_sha256, recorded_at, terminal`

func scanEvent(scanner rowScanner) (Event, error) {
	var event Event
	var payload, digest []byte
	var recordedAt string
	var terminal int64
	if err := scanner.Scan(
		&event.ID, &event.RunID, &event.Sequence, &event.Kind,
		&payload, &digest, &recordedAt, &terminal,
	); err != nil {
		return Event{}, err
	}
	var err error
	if event.RecordedAt, err = parseTime(recordedAt); err != nil {
		return Event{}, err
	}
	event.Terminal, err = databaseBool("terminal", terminal)
	if err != nil {
		return Event{}, err
	}
	event.Payload = append(json.RawMessage(nil), payload...)
	event.PayloadSHA256 = hex.EncodeToString(digest)
	return event, nil
}

func boolDatabaseValue(value *bool) any {
	if value == nil {
		return nil
	}
	if *value {
		return int64(1)
	}
	return int64(0)
}

func intDatabaseValue(value *int) any {
	if value == nil {
		return nil
	}
	return int64(*value)
}

func stringDatabaseValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalStringDatabaseValue(value string) any {
	if value == "" {
		return nil
	}
	return value
}
