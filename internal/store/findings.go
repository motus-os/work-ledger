package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxFindingSummaryBytes = 512
	maxFindingTextBytes    = 4 << 10
	maxFindingQueryTerms   = 64
)

var bidiControl = unicode.Properties["Bidi_Control"]

const findingColumns = `f.id, f.origin_run_id, f.payload, f.payload_sha256, f.recorded_at,
    c.id, c.finding_id, c.disposition, c.resolving_run_id,
    c.payload, c.payload_sha256, c.recorded_at`

// AddFinding appends authored context connected to a closed run. Repeating an
// identical request with the same ID is idempotent.
func (s *Store) AddFinding(ctx context.Context, params AddFindingParams) (Finding, error) {
	if err := validateOpaque("finding ID", params.ID, 255); err != nil {
		return Finding{}, err
	}
	if err := validateOpaque("origin run ID", params.OriginRunID, 255); err != nil {
		return Finding{}, err
	}
	payload, err := canonicalFindingPayload(params.Content)
	if err != nil {
		return Finding{}, err
	}
	recordedAt := s.now()
	var result Finding
	err = s.write(ctx, func(tx *sql.Tx) error {
		existing, err := getFinding(ctx, tx, params.ID)
		if err == nil {
			if existing.OriginRunID != params.OriginRunID || existing.Content != params.Content {
				return fmt.Errorf("%w: finding ID %q already has different content", ErrConflict, params.ID)
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		run, err := getRun(ctx, tx, params.OriginRunID)
		if err != nil {
			return err
		}
		if run.State != RunClosed {
			return fmt.Errorf("%w: finding origin run %q is open", ErrOpen, params.OriginRunID)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO findings (
            id, origin_run_id, payload, payload_sha256, recorded_at
        ) VALUES (?, ?, ?, ?, ?)`,
			params.ID, params.OriginRunID, payload, payloadDigest(payload), formatTime(recordedAt),
		)
		if err != nil {
			return fmt.Errorf("insert finding: %w", err)
		}
		result, err = getFinding(ctx, tx, params.ID)
		return err
	})
	if err != nil {
		return Finding{}, err
	}
	return result, nil
}

// CloseFinding appends one immutable resolved or dismissed closure. Repeating
// an identical request with the same ID is idempotent.
func (s *Store) CloseFinding(ctx context.Context, findingID string, params CloseFindingParams) (Finding, error) {
	if err := validateOpaque("finding ID", findingID, 255); err != nil {
		return Finding{}, err
	}
	if err := validateOpaque("finding closure ID", params.ID, 255); err != nil {
		return Finding{}, err
	}
	if err := validateFindingDisposition(params.Disposition, params.ResolvingRunID); err != nil {
		return Finding{}, err
	}
	payload, err := canonicalFindingClosurePayload(params.Content)
	if err != nil {
		return Finding{}, err
	}
	recordedAt := s.now()
	var result Finding
	err = s.write(ctx, func(tx *sql.Tx) error {
		finding, err := getFinding(ctx, tx, findingID)
		if err != nil {
			return err
		}
		if finding.Closure != nil {
			closure := finding.Closure
			if closure.ID != params.ID || closure.Disposition != params.Disposition ||
				closure.ResolvingRunID != params.ResolvingRunID ||
				closure.Content != params.Content {
				return fmt.Errorf("%w: finding %q is already closed differently", ErrConflict, findingID)
			}
			result = finding
			return nil
		}
		if params.Disposition == DispositionResolved {
			run, err := getRun(ctx, tx, params.ResolvingRunID)
			if err != nil {
				return err
			}
			if run.State != RunClosed || run.Outcome != OutcomeSuccess {
				return fmt.Errorf("%w: resolving run %q must be closed with outcome success", ErrInvalid, params.ResolvingRunID)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO finding_closures (
            id, finding_id, disposition, resolving_run_id,
            payload, payload_sha256, recorded_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			params.ID, findingID, params.Disposition,
			optionalStringDatabaseValue(params.ResolvingRunID),
			payload, payloadDigest(payload), formatTime(recordedAt),
		)
		if err != nil {
			return fmt.Errorf("insert finding closure: %w", err)
		}
		result, err = getFinding(ctx, tx, findingID)
		return err
	})
	if err != nil {
		return Finding{}, err
	}
	return result, nil
}

// GetFinding returns one finding and its optional immutable closure.
func (s *Store) GetFinding(ctx context.Context, findingID string) (Finding, error) {
	if err := validateOpaque("finding ID", findingID, 255); err != nil {
		return Finding{}, err
	}
	return getFinding(ctx, s.db, findingID)
}

// ListFindings returns deterministic results after applying the optional
// state and query filters. Unqueried results are newest-first. Queried results
// are ordered by lexical relevance, then recency.
func (s *Store) ListFindings(ctx context.Context, options ListFindingsOptions) ([]Finding, error) {
	if err := ValidateListFindingsOptions(options); err != nil {
		return nil, err
	}
	if options.Limit == 0 {
		options.Limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+findingColumns+`
        FROM findings f
        LEFT JOIN finding_closures c ON c.finding_id = f.id
        ORDER BY f.recorded_at DESC, f.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	defer rows.Close()
	query, err := newFindingQuery(options.Query)
	if err != nil {
		return nil, err
	}
	findings := make([]Finding, 0)
	matches := make([]findingMatch, 0)
	skipped := 0
	for rows.Next() {
		finding, err := scanFinding(rows)
		if err != nil {
			return nil, fmt.Errorf("scan listed finding: %w", err)
		}
		if options.State != "" && finding.State != options.State {
			continue
		}
		if query.empty() {
			if skipped < options.Offset {
				skipped++
				continue
			}
			if len(findings) == options.Limit {
				break
			}
			findings = append(findings, finding)
			continue
		}
		match := query.match(finding)
		if !match.matched() {
			continue
		}
		matches = append(matches, findingMatch{finding: finding, score: match})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	if query.empty() {
		return findings, nil
	}
	sort.Slice(matches, func(left, right int) bool {
		a, b := matches[left], matches[right]
		if a.score.exact != b.score.exact {
			return a.score.exact
		}
		if a.score.matchedTerms != b.score.matchedTerms {
			return a.score.matchedTerms > b.score.matchedTerms
		}
		if !a.finding.RecordedAt.Equal(b.finding.RecordedAt) {
			return a.finding.RecordedAt.After(b.finding.RecordedAt)
		}
		return a.finding.ID < b.finding.ID
	})
	if options.Offset >= len(matches) {
		return findings, nil
	}
	end := min(options.Offset+options.Limit, len(matches))
	findings = make([]Finding, 0, end-options.Offset)
	for _, match := range matches[options.Offset:end] {
		findings = append(findings, match.finding)
	}
	return findings, nil
}

// ValidateListFindingsOptions validates filters without reading the Store.
func ValidateListFindingsOptions(options ListFindingsOptions) error {
	if options.Limit < 0 || options.Limit > 1000 {
		return fmt.Errorf("%w: list limit must be between 1 and 1000", ErrInvalid)
	}
	if options.Offset < 0 {
		return fmt.Errorf("%w: list offset cannot be negative", ErrInvalid)
	}
	if options.State != "" && options.State != FindingOpen &&
		options.State != FindingResolved && options.State != FindingDismissed {
		return fmt.Errorf("%w: invalid finding state %q", ErrInvalid, options.State)
	}
	if err := validateQuery(options.Query); err != nil {
		return err
	}
	if _, err := newFindingQuery(options.Query); err != nil {
		return err
	}
	return nil
}

func getFinding(ctx context.Context, q sqlReadWriter, findingID string) (Finding, error) {
	finding, err := scanFinding(q.QueryRowContext(ctx, `SELECT `+findingColumns+`
        FROM findings f
        LEFT JOIN finding_closures c ON c.finding_id = f.id
        WHERE f.id = ?`, findingID))
	if errors.Is(err, sql.ErrNoRows) {
		return Finding{}, fmt.Errorf("%w: finding %q", ErrNotFound, findingID)
	}
	if err != nil {
		return Finding{}, fmt.Errorf("read finding %q: %w", findingID, err)
	}
	return finding, nil
}

func scanFinding(scanner rowScanner) (Finding, error) {
	var finding Finding
	var payload, digest []byte
	var recordedAt string
	var closureID, closureFindingID, disposition, resolvingRunID, closureRecordedAt sql.NullString
	var closurePayload, closureDigest []byte
	if err := scanner.Scan(
		&finding.ID, &finding.OriginRunID, &payload, &digest, &recordedAt,
		&closureID, &closureFindingID, &disposition, &resolvingRunID,
		&closurePayload, &closureDigest, &closureRecordedAt,
	); err != nil {
		return Finding{}, err
	}
	if !bytes.Equal(digest, payloadDigest(payload)) {
		return Finding{}, fmt.Errorf("finding %q payload hash mismatch", finding.ID)
	}
	canonical, err := canonicalBoundedJSON(payload, "finding")
	if err != nil {
		return Finding{}, fmt.Errorf("finding %q payload: %w", finding.ID, err)
	}
	if !bytes.Equal(payload, canonical) {
		return Finding{}, fmt.Errorf("finding %q payload is not canonical JSON", finding.ID)
	}
	if err := decodeExactJSON(payload, &finding.Content); err != nil {
		return Finding{}, fmt.Errorf("finding %q payload: %w", finding.ID, err)
	}
	if err := validateFindingContent(finding.Content); err != nil {
		return Finding{}, fmt.Errorf("finding %q payload: %w", finding.ID, err)
	}
	err = nil
	if finding.RecordedAt, err = parseTime(recordedAt); err != nil {
		return Finding{}, err
	}
	finding.PayloadSHA256 = hex.EncodeToString(digest)
	finding.State = FindingOpen
	if closureID.Valid {
		if !closureFindingID.Valid || !disposition.Valid || !closureRecordedAt.Valid {
			return Finding{}, fmt.Errorf("finding %q has an incomplete closure", finding.ID)
		}
		if closureFindingID.String != finding.ID {
			return Finding{}, fmt.Errorf("finding %q closure points to %q", finding.ID, closureFindingID.String)
		}
		if !bytes.Equal(closureDigest, payloadDigest(closurePayload)) {
			return Finding{}, fmt.Errorf("finding closure %q payload hash mismatch", closureID.String)
		}
		canonical, err := canonicalBoundedJSON(closurePayload, "finding closure")
		if err != nil {
			return Finding{}, fmt.Errorf("finding closure %q payload: %w", closureID.String, err)
		}
		if !bytes.Equal(closurePayload, canonical) {
			return Finding{}, fmt.Errorf("finding closure %q payload is not canonical JSON", closureID.String)
		}
		closure := &FindingClosure{
			ID:             closureID.String,
			FindingID:      closureFindingID.String,
			Disposition:    FindingDisposition(disposition.String),
			ResolvingRunID: resolvingRunID.String,
			PayloadSHA256:  hex.EncodeToString(closureDigest),
		}
		if err := decodeExactJSON(closurePayload, &closure.Content); err != nil {
			return Finding{}, fmt.Errorf("finding closure %q payload: %w", closure.ID, err)
		}
		if err := validateFindingClosureContent(closure.Content); err != nil {
			return Finding{}, fmt.Errorf("finding closure %q payload: %w", closure.ID, err)
		}
		if err := validateFindingDisposition(closure.Disposition, closure.ResolvingRunID); err != nil {
			return Finding{}, fmt.Errorf("finding closure %q: %w", closure.ID, err)
		}
		if closure.RecordedAt, err = parseTime(closureRecordedAt.String); err != nil {
			return Finding{}, err
		}
		finding.State = FindingState(closure.Disposition)
		finding.Closure = closure
	}
	return finding, nil
}

func canonicalFindingPayload(content FindingContent) ([]byte, error) {
	if err := validateFindingContent(content); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("encode finding: %w", err)
	}
	return canonicalBoundedJSON(raw, "finding")
}

func canonicalFindingClosurePayload(content FindingClosureContent) ([]byte, error) {
	if err := validateFindingClosureContent(content); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("encode finding closure: %w", err)
	}
	return canonicalBoundedJSON(raw, "finding closure")
}

func canonicalBoundedJSON(raw []byte, name string) ([]byte, error) {
	return canonicalJSONNamed(raw, MaxFindingPayloadBytes, name)
}

// ParseFindingContentJSON rejects duplicate or unknown fields and returns a
// validated finding payload. It is used by the CLI's file/stdin boundary.
func ParseFindingContentJSON(raw []byte) (FindingContent, error) {
	canonical, err := canonicalBoundedJSON(raw, "finding")
	if err != nil {
		return FindingContent{}, err
	}
	var content FindingContent
	if err := decodeExactJSON(canonical, &content); err != nil {
		return FindingContent{}, err
	}
	if err := validateFindingContent(content); err != nil {
		return FindingContent{}, err
	}
	return content, nil
}

// ParseFindingClosureContentJSON rejects duplicate or unknown fields and
// returns a validated closure payload.
func ParseFindingClosureContentJSON(raw []byte) (FindingClosureContent, error) {
	canonical, err := canonicalBoundedJSON(raw, "finding closure")
	if err != nil {
		return FindingClosureContent{}, err
	}
	var content FindingClosureContent
	if err := decodeExactJSON(canonical, &content); err != nil {
		return FindingClosureContent{}, err
	}
	if err := validateFindingClosureContent(content); err != nil {
		return FindingClosureContent{}, err
	}
	return content, nil
}

func decodeExactJSON(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: invalid JSON: %v", ErrInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errorsNewTrailingJSON
		}
		return fmt.Errorf("%w: invalid JSON: %v", ErrInvalid, err)
	}
	return nil
}

func validateFindingContent(content FindingContent) error {
	if err := validateFindingText("summary", content.Summary, maxFindingSummaryBytes, true, false); err != nil {
		return err
	}
	if err := validateFindingText("hypothesis", content.Hypothesis, maxFindingTextBytes, false, true); err != nil {
		return err
	}
	return validateFindingText("next_step", content.NextStep, maxFindingTextBytes, false, true)
}

func validateFindingClosureContent(content FindingClosureContent) error {
	return validateFindingText("note", content.Note, maxFindingTextBytes, true, true)
}

func validateFindingText(name, value string, maxBytes int, required, multiline bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%w: %s is required", ErrInvalid, name)
		}
		return nil
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalid, name, maxBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s must be valid UTF-8", ErrInvalid, name)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s must not start or end with whitespace", ErrInvalid, name)
	}
	for _, character := range value {
		if multiline && character == '\n' {
			continue
		}
		if unicode.IsControl(character) || unicode.In(character, bidiControl, unicode.Zl, unicode.Zp) {
			return fmt.Errorf("%w: %s contains a control or formatting character", ErrInvalid, name)
		}
	}
	return nil
}

func validateFindingDisposition(disposition FindingDisposition, resolvingRunID string) error {
	switch disposition {
	case DispositionResolved:
		if err := validateOpaque("resolving run ID", resolvingRunID, 255); err != nil {
			return err
		}
	case DispositionDismissed:
		if resolvingRunID != "" {
			return fmt.Errorf("%w: dismissed finding must not have a resolving run", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: invalid finding disposition %q", ErrInvalid, disposition)
	}
	return nil
}

func validateQuery(query string) error {
	if len(query) > maxFindingTextBytes {
		return fmt.Errorf("%w: query exceeds %d bytes", ErrInvalid, maxFindingTextBytes)
	}
	if !utf8.ValidString(query) || strings.IndexFunc(query, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, bidiControl, unicode.Zl, unicode.Zp)
	}) >= 0 {
		return fmt.Errorf("%w: query must be valid UTF-8 without control or formatting characters", ErrInvalid)
	}
	return nil
}

type findingQuery struct {
	full  string
	terms []string
}

type findingMatch struct {
	finding Finding
	score   findingMatchScore
}

type findingMatchScore struct {
	exact        bool
	matchedTerms int
}

func newFindingQuery(query string) (findingQuery, error) {
	lower := strings.ToLower(query)
	rawTerms := strings.FieldsFunc(lower, func(character rune) bool {
		return character != '_' && !unicode.IsLetter(character) && !unicode.IsNumber(character)
	})
	terms := make([]string, 0, len(rawTerms))
	seen := make(map[string]struct{}, len(rawTerms))
	for _, term := range rawTerms {
		if _, exists := seen[term]; exists {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
		if len(terms) > maxFindingQueryTerms {
			return findingQuery{}, fmt.Errorf("%w: query exceeds %d unique terms", ErrInvalid, maxFindingQueryTerms)
		}
	}
	return findingQuery{full: lower, terms: terms}, nil
}

func (q findingQuery) empty() bool {
	return q.full == ""
}

func (q findingQuery) match(finding Finding) findingMatchScore {
	authored := lowerValues(findingAuthoredValues(finding))
	identifiers := lowerValues(findingIdentifierValues(finding))
	exact := containsNormalized(authored, q.full)
	if identifierQueryAllowed(q.full) {
		exact = exact || containsNormalized(identifiers, q.full)
	}
	matchedTerms := 0
	for _, term := range q.terms {
		matched := containsNormalized(authored, term)
		if isFindingIdentifierTerm(term) {
			matched = matched || containsNormalized(identifiers, term)
		}
		if matched {
			matchedTerms++
		}
	}
	return findingMatchScore{exact: exact, matchedTerms: matchedTerms}
}

func (score findingMatchScore) matched() bool {
	return score.exact || score.matchedTerms > 0
}

func findingAuthoredValues(finding Finding) []string {
	values := []string{
		finding.Content.Summary,
		finding.Content.Hypothesis,
		finding.Content.NextStep,
	}
	if finding.Closure != nil {
		values = append(values, finding.Closure.Content.Note)
	}
	return values
}

func findingIdentifierValues(finding Finding) []string {
	values := []string{
		finding.ID,
		finding.OriginRunID,
	}
	if finding.Closure != nil {
		values = append(values,
			finding.Closure.ID,
			finding.Closure.ResolvingRunID,
		)
	}
	return values
}

func lowerValues(values []string) []string {
	lowered := make([]string, len(values))
	for index, value := range values {
		lowered[index] = strings.ToLower(value)
	}
	return lowered
}

func containsNormalized(values []string, lowerQuery string) bool {
	for _, value := range values {
		if strings.Contains(value, lowerQuery) {
			return true
		}
	}
	return false
}

func isFindingIdentifierTerm(term string) bool {
	return hasSpecificIdentifierPrefix(term) || isHexIdentifierFragment(term)
}

func identifierQueryAllowed(query string) bool {
	return hasSpecificIdentifierPrefix(query) || isHexIdentifierFragment(query)
}

func hasSpecificIdentifierPrefix(term string) bool {
	for _, prefix := range []string{"finding_", "closure_", "run_"} {
		if strings.HasPrefix(term, prefix) && len(term)-len(prefix) >= 8 {
			return true
		}
	}
	return false
}

func isHexIdentifierFragment(term string) bool {
	if len(term) < 8 {
		return false
	}
	for _, character := range term {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
