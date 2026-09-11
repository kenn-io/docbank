package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/query"
)

// ErrInvalidSavedQueryRun identifies an invalid durable run receipt or list request.
var ErrInvalidSavedQueryRun = errors.New("invalid saved query run")

// SavedQueryRunComparison summarizes the change from the immediately previous run.
type SavedQueryRunComparison struct {
	HashChanged       bool  `json:"hash_changed"`
	TotalDelta        int64 `json:"total_delta"`
	DefinitionChanged bool  `json:"definition_changed"`
}

// SavedQueryRun is durable evidence that one saved definition was materialized.
// Snapshot rows remain daemon-local and are not reconstructed from this receipt.
type SavedQueryRun struct {
	RunID                    string                  `json:"run_id"`
	SavedQueryID             string                  `json:"saved_query_id"`
	SavedQueryRevision       int64                   `json:"saved_query_revision"`
	QueryFingerprint         string                  `json:"query_fingerprint"`
	SnapshotID               string                  `json:"snapshot_id"`
	MemberHash               string                  `json:"member_hash"`
	Total                    int64                   `json:"total"`
	TotalBytes               int64                   `json:"total_bytes"`
	RanAt                    time.Time               `json:"ran_at"`
	ExpiresAt                time.Time               `json:"expires_at"`
	PreviousRunID            string                  `json:"previous_run_id,omitempty"`
	PreviousMemberHash       string                  `json:"previous_member_hash,omitempty"`
	PreviousTotal            int64                   `json:"previous_total,omitempty"`
	PreviousQueryFingerprint string                  `json:"previous_query_fingerprint,omitempty"`
	Comparison               SavedQueryRunComparison `json:"comparison"`
}

// ListSavedQueryRuns returns the newest bounded receipt page for one definition.
func (s *Store) ListSavedQueryRuns(
	ctx context.Context, id string, limit int,
) ([]SavedQueryRun, error) {
	if err := validateUUIDv4(id); err != nil {
		return nil, fmt.Errorf("%w: saved query ID: %w", ErrInvalidSavedQueryRun, err)
	}
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("%w: limit must be from 1 through 100", ErrInvalidSavedQueryRun)
	}
	rows, err := s.db.QueryContext(ctx, `WITH RECURSIVE history(
		run_id,saved_query_id,saved_query_revision,query_fingerprint,snapshot_id,
		member_hash,total,total_bytes,ran_at,expires_at,previous_run_id,
		previous_member_hash,previous_total,previous_query_fingerprint,depth
	) AS (
		SELECT r.run_id,r.saved_query_id,r.saved_query_revision,r.query_fingerprint,r.snapshot_id,
			r.member_hash,r.total,r.total_bytes,r.ran_at,r.expires_at,r.previous_run_id,
			r.previous_member_hash,r.previous_total,r.previous_query_fingerprint,0
		FROM saved_query_runs r WHERE r.saved_query_id=? AND NOT EXISTS(
			SELECT 1 FROM saved_query_runs successor WHERE successor.previous_run_id=r.run_id
		)
		UNION ALL
		SELECT p.run_id,p.saved_query_id,p.saved_query_revision,p.query_fingerprint,p.snapshot_id,
			p.member_hash,p.total,p.total_bytes,p.ran_at,p.expires_at,p.previous_run_id,
			p.previous_member_hash,p.previous_total,p.previous_query_fingerprint,h.depth+1
		FROM saved_query_runs p JOIN history h ON p.run_id=h.previous_run_id
	)
	SELECT run_id,saved_query_id,saved_query_revision,query_fingerprint,snapshot_id,
		member_hash,total,total_bytes,ran_at,expires_at,previous_run_id,
		previous_member_hash,previous_total,previous_query_fingerprint
	FROM history ORDER BY depth LIMIT ?`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("listing saved query runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]SavedQueryRun, 0)
	for rows.Next() {
		run, scanErr := scanSavedQueryRun(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing saved query runs: %w", err)
	}
	return result, nil
}

const savedQueryRunSelect = `SELECT
	run_id,saved_query_id,saved_query_revision,query_fingerprint,snapshot_id,
	member_hash,total,total_bytes,ran_at,expires_at,previous_run_id,
	previous_member_hash,previous_total,previous_query_fingerprint
	FROM saved_query_runs `

func insertSavedQueryRunTx(
	ctx context.Context, tx *sql.Tx, savedQueryID string, expectedRevision int64,
	projection SnapshotProjection, page SnapshotPage,
) (SavedQueryRun, error) {
	root, err := (queryResolver{q: tx}).Resolve(ctx, query.ReferenceSaved, savedQueryID, true)
	if err != nil || root.Dependency.Revision != expectedRevision {
		return SavedQueryRun{}, fmt.Errorf("saved query %s changed before run receipt: %w", savedQueryID, ErrStaleRevision)
	}
	rootFingerprint, err := query.Fingerprint(*root.Query)
	if err != nil {
		return SavedQueryRun{}, err
	}
	if rootFingerprint != projection.QueryFingerprint {
		return SavedQueryRun{}, fmt.Errorf("saved query %s definition changed before run receipt: %w", savedQueryID, ErrStaleRevision)
	}
	if err := recheckSavedQueryRunDependencies(ctx, tx, projection.Dependencies); err != nil {
		return SavedQueryRun{}, err
	}
	runID, err := newUUIDv4()
	if err != nil {
		return SavedQueryRun{}, fmt.Errorf("allocating saved query run ID: %w", err)
	}
	run := SavedQueryRun{
		RunID: runID, SavedQueryID: savedQueryID, SavedQueryRevision: expectedRevision,
		QueryFingerprint: projection.QueryFingerprint, SnapshotID: page.SnapshotID,
		MemberHash: projection.MemberHash, Total: projection.Total, TotalBytes: projection.TotalBytes,
		RanAt: page.CreatedAt.UTC(), ExpiresAt: page.ExpiresAt.UTC(),
	}
	var previousRunID, previousHash, previousFingerprint sql.NullString
	var previousTotal sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT r.run_id,r.member_hash,r.total,r.query_fingerprint
		FROM saved_query_runs r WHERE r.saved_query_id=? AND NOT EXISTS(
			SELECT 1 FROM saved_query_runs successor WHERE successor.previous_run_id=r.run_id
		) LIMIT 1`,
		savedQueryID).Scan(&previousRunID, &previousHash, &previousTotal, &previousFingerprint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SavedQueryRun{}, fmt.Errorf("selecting previous saved query run: %w", err)
	}
	if err == nil {
		run.PreviousRunID = previousRunID.String
		run.PreviousMemberHash = previousHash.String
		run.PreviousTotal = previousTotal.Int64
		run.PreviousQueryFingerprint = previousFingerprint.String
		run.Comparison = SavedQueryRunComparison{
			HashChanged:       run.MemberHash != run.PreviousMemberHash,
			TotalDelta:        run.Total - run.PreviousTotal,
			DefinitionChanged: run.QueryFingerprint != run.PreviousQueryFingerprint,
		}
	}
	if err := validateSavedQueryRun(run); err != nil {
		return SavedQueryRun{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO saved_query_runs(
		run_id,saved_query_id,saved_query_revision,query_fingerprint,snapshot_id,
		member_hash,total,total_bytes,ran_at,expires_at,previous_run_id,
		previous_member_hash,previous_total,previous_query_fingerprint
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.RunID, run.SavedQueryID,
		run.SavedQueryRevision, run.QueryFingerprint, run.SnapshotID, run.MemberHash,
		run.Total, run.TotalBytes, formatSavedQueryRunTime(run.RanAt),
		formatSavedQueryRunTime(run.ExpiresAt), nullableString(run.PreviousRunID),
		nullableString(run.PreviousMemberHash), nullablePreviousTotal(run),
		nullableString(run.PreviousQueryFingerprint))
	if err != nil {
		return SavedQueryRun{}, fmt.Errorf("inserting saved query run: %w", err)
	}
	return run, nil
}

func recheckSavedQueryRunDependencies(ctx context.Context, tx *sql.Tx, dependencies []query.Dependency) error {
	resolver := queryResolver{q: tx}
	for _, observed := range dependencies {
		current, err := resolver.Resolve(ctx, observed.Kind, observed.ID, true)
		if err != nil || current.Dependency != observed {
			return fmt.Errorf("saved query dependency %s %s changed before run receipt: %w",
				observed.Kind, observed.ID, ErrStaleRevision)
		}
	}
	return nil
}

func scanSavedQueryRun(row interface{ Scan(dest ...any) error }) (SavedQueryRun, error) {
	var run SavedQueryRun
	var ranAt, expiresAt string
	var previousRunID, previousHash, previousFingerprint sql.NullString
	var previousTotal sql.NullInt64
	if err := row.Scan(&run.RunID, &run.SavedQueryID, &run.SavedQueryRevision,
		&run.QueryFingerprint, &run.SnapshotID, &run.MemberHash, &run.Total,
		&run.TotalBytes, &ranAt, &expiresAt, &previousRunID, &previousHash,
		&previousTotal, &previousFingerprint); err != nil {
		return SavedQueryRun{}, fmt.Errorf("scanning saved query run: %w", err)
	}
	var err error
	run.RanAt, err = time.Parse(timestampLayout, ranAt)
	if err != nil {
		return SavedQueryRun{}, fmt.Errorf("scanning saved query run ran_at: %w", err)
	}
	run.ExpiresAt, err = time.Parse(timestampLayout, expiresAt)
	if err != nil {
		return SavedQueryRun{}, fmt.Errorf("scanning saved query run expires_at: %w", err)
	}
	previousCount := 0
	for _, present := range []bool{
		previousRunID.Valid, previousHash.Valid, previousTotal.Valid, previousFingerprint.Valid,
	} {
		if present {
			previousCount++
		}
	}
	if previousCount != 0 && previousCount != 4 {
		return SavedQueryRun{}, fmt.Errorf("scanning saved query run: %w: previous fields must be present together", ErrInvalidSavedQueryRun)
	}
	if previousCount == 4 {
		run.PreviousRunID = previousRunID.String
		run.PreviousMemberHash = previousHash.String
		run.PreviousTotal = previousTotal.Int64
		run.PreviousQueryFingerprint = previousFingerprint.String
		run.Comparison = SavedQueryRunComparison{
			HashChanged:       run.MemberHash != run.PreviousMemberHash,
			TotalDelta:        run.Total - run.PreviousTotal,
			DefinitionChanged: run.QueryFingerprint != run.PreviousQueryFingerprint,
		}
	}
	if err := validateSavedQueryRun(run); err != nil {
		return SavedQueryRun{}, fmt.Errorf("scanning saved query run: %w", err)
	}
	return run, nil
}

func validateSavedQueryRun(run SavedQueryRun) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidSavedQueryRun, fmt.Sprintf(format, args...))
	}
	if err := validateUUIDv4(run.RunID); err != nil {
		return invalid("run ID: %v", err)
	}
	if err := validateUUIDv4(run.SavedQueryID); err != nil {
		return invalid("saved query ID: %v", err)
	}
	if run.SavedQueryRevision < 1 || run.Total < 0 || run.TotalBytes < 0 {
		return invalid("negative count or invalid revision")
	}
	if err := validateSavedQueryRunFingerprint(run.QueryFingerprint, "query fingerprint"); err != nil {
		return invalid("%v", err)
	}
	if len(run.SnapshotID) != snapshotIDBytes*2 || strings.ToLower(run.SnapshotID) != run.SnapshotID {
		return invalid("snapshot ID must be %d lowercase hexadecimal characters", snapshotIDBytes*2)
	}
	if _, err := hex.DecodeString(run.SnapshotID); err != nil {
		return invalid("snapshot ID must be %d lowercase hexadecimal characters", snapshotIDBytes*2)
	}
	if err := validateCatalogSHA256(run.MemberHash, "member hash"); err != nil {
		return invalid("%v", err)
	}
	if run.RanAt.IsZero() || run.ExpiresAt.Before(run.RanAt) {
		return invalid("invalid run timestamps")
	}
	hasPrevious := run.PreviousRunID != ""
	if hasPrevious != (run.PreviousMemberHash != "") || hasPrevious != (run.PreviousQueryFingerprint != "") {
		return invalid("previous run fields must be present together")
	}
	if hasPrevious {
		if err := validateUUIDv4(run.PreviousRunID); err != nil || run.PreviousRunID == run.RunID {
			return invalid("invalid previous run ID")
		}
		if err := validateCatalogSHA256(run.PreviousMemberHash, "previous member hash"); err != nil {
			return invalid("%v", err)
		}
		if err := validateSavedQueryRunFingerprint(run.PreviousQueryFingerprint, "previous query fingerprint"); err != nil {
			return invalid("%v", err)
		}
		if run.PreviousTotal < 0 {
			return invalid("previous total must not be negative")
		}
	}
	return nil
}

func validateSavedQueryRunFingerprint(value, subject string) error {
	if !strings.HasPrefix(value, "sha256:") {
		return fmt.Errorf("%s must use sha256", subject)
	}
	return validateCatalogSHA256(strings.TrimPrefix(value, "sha256:"), subject)
}

func formatSavedQueryRunTime(value time.Time) string { return value.UTC().Format(timestampLayout) }

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullablePreviousTotal(run SavedQueryRun) any {
	if run.PreviousRunID == "" {
		return nil
	}
	return run.PreviousTotal
}
