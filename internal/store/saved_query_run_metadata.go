package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type metadataSavedQueryRun struct {
	Type                     string  `json:"type"`
	RunID                    string  `json:"run_id"`
	SavedQueryID             string  `json:"saved_query_id"`
	SavedQueryRevision       int64   `json:"saved_query_revision"`
	QueryFingerprint         string  `json:"query_fingerprint"`
	SnapshotID               string  `json:"snapshot_id"`
	MemberHash               string  `json:"member_hash"`
	Total                    int64   `json:"total"`
	TotalBytes               int64   `json:"total_bytes"`
	RanAt                    string  `json:"ran_at"`
	ExpiresAt                string  `json:"expires_at"`
	PreviousRunID            *string `json:"previous_run_id"`
	PreviousMemberHash       *string `json:"previous_member_hash"`
	PreviousTotal            *int64  `json:"previous_total"`
	PreviousQueryFingerprint *string `json:"previous_query_fingerprint"`
}

func exportSavedQueryRuns(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, savedQueryRunSelect+`
		ORDER BY saved_query_id,ran_at,run_id`)
	if err != nil {
		return fmt.Errorf("exporting saved query runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		run, scanErr := scanSavedQueryRun(rows)
		if scanErr != nil {
			return fmt.Errorf("validating saved query run metadata for export: %w", scanErr)
		}
		record := savedQueryRunMetadata(run)
		if err := validateSavedQueryRunMetadataRecord(record); err != nil {
			return fmt.Errorf("validating saved query run metadata for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("saved query run", rows)
}

func savedQueryRunMetadata(run SavedQueryRun) metadataSavedQueryRun {
	record := metadataSavedQueryRun{
		Type: metadataSavedQueryRunType, RunID: run.RunID, SavedQueryID: run.SavedQueryID,
		SavedQueryRevision: run.SavedQueryRevision, QueryFingerprint: run.QueryFingerprint,
		SnapshotID: run.SnapshotID, MemberHash: run.MemberHash, Total: run.Total,
		TotalBytes: run.TotalBytes, RanAt: formatSavedQueryRunTime(run.RanAt),
		ExpiresAt: formatSavedQueryRunTime(run.ExpiresAt),
	}
	if run.PreviousRunID != "" {
		record.PreviousRunID = new(run.PreviousRunID)
		record.PreviousMemberHash = new(run.PreviousMemberHash)
		record.PreviousTotal = new(run.PreviousTotal)
		record.PreviousQueryFingerprint = new(run.PreviousQueryFingerprint)
	}
	return record
}

func importSavedQueryRunMetadata(ctx context.Context, tx *sql.Tx, record metadataSavedQueryRun) error {
	if err := validateSavedQueryRunMetadataRecord(record); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO saved_query_runs(
		run_id,saved_query_id,saved_query_revision,query_fingerprint,snapshot_id,
		member_hash,total,total_bytes,ran_at,expires_at,previous_run_id,
		previous_member_hash,previous_total,previous_query_fingerprint
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.RunID, record.SavedQueryID,
		record.SavedQueryRevision, record.QueryFingerprint, record.SnapshotID,
		record.MemberHash, record.Total, record.TotalBytes, record.RanAt,
		record.ExpiresAt, record.PreviousRunID, record.PreviousMemberHash,
		record.PreviousTotal, record.PreviousQueryFingerprint)
	return err
}

func validateSavedQueryRunMetadataRecord(record metadataSavedQueryRun) error {
	if record.Type != metadataSavedQueryRunType {
		return errors.New("invalid saved query run record")
	}
	ranAt, err := time.Parse(timestampLayout, record.RanAt)
	if err != nil || formatSavedQueryRunTime(ranAt) != record.RanAt {
		return errors.New("saved query run ran_at is not canonical")
	}
	expiresAt, err := time.Parse(timestampLayout, record.ExpiresAt)
	if err != nil || formatSavedQueryRunTime(expiresAt) != record.ExpiresAt {
		return errors.New("saved query run expires_at is not canonical")
	}
	run := SavedQueryRun{
		RunID: record.RunID, SavedQueryID: record.SavedQueryID,
		SavedQueryRevision: record.SavedQueryRevision,
		QueryFingerprint:   record.QueryFingerprint, SnapshotID: record.SnapshotID,
		MemberHash: record.MemberHash, Total: record.Total, TotalBytes: record.TotalBytes,
		RanAt: ranAt, ExpiresAt: expiresAt,
	}
	previousCount := 0
	for _, present := range []bool{
		record.PreviousRunID != nil, record.PreviousMemberHash != nil,
		record.PreviousTotal != nil, record.PreviousQueryFingerprint != nil,
	} {
		if present {
			previousCount++
		}
	}
	if previousCount != 0 && previousCount != 4 {
		return errors.New("saved query run previous fields must be null or present together")
	}
	if previousCount == 4 {
		run.PreviousRunID = *record.PreviousRunID
		run.PreviousMemberHash = *record.PreviousMemberHash
		run.PreviousTotal = *record.PreviousTotal
		run.PreviousQueryFingerprint = *record.PreviousQueryFingerprint
	}
	return validateSavedQueryRun(run)
}

func validateSavedQueryRunMetadataState(ctx context.Context, q metadataQuerier) error {
	if err := exportSavedQueryRuns(ctx, q, func(any) error { return nil }); err != nil {
		return err
	}
	var invalid bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM saved_query_runs r
		LEFT JOIN saved_queries q ON q.id=r.saved_query_id
		WHERE q.id IS NULL OR q.revision<r.saved_query_revision
	)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validating saved query run definition relations: %w", err)
	}
	if invalid {
		return errors.New("saved query run references an absent definition or future revision")
	}
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM saved_query_runs r
		LEFT JOIN saved_query_runs p ON p.run_id=r.previous_run_id
		WHERE r.previous_run_id IS NOT NULL AND (
			p.run_id IS NULL OR p.saved_query_id<>r.saved_query_id OR
			p.member_hash<>r.previous_member_hash OR p.total<>r.previous_total OR
			p.query_fingerprint<>r.previous_query_fingerprint
		)
	)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validating saved query run previous receipt relations: %w", err)
	}
	if invalid {
		return errors.New("saved query run previous receipt does not match durable authority")
	}
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT saved_query_id FROM saved_query_runs
		GROUP BY saved_query_id
		HAVING SUM(CASE WHEN previous_run_id IS NULL THEN 1 ELSE 0 END)<>1
	) OR EXISTS(
		SELECT previous_run_id FROM saved_query_runs WHERE previous_run_id IS NOT NULL
		GROUP BY previous_run_id HAVING COUNT(*)<>1
	)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validating saved query run comparison chain: %w", err)
	}
	if invalid {
		return errors.New("saved query run comparison chain is disconnected or branched")
	}
	return nil
}
