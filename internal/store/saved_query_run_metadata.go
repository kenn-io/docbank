package store

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
)

type metadataSavedQueryRun struct {
	Photos                   bool    `json:"photos,omitzero" db:"photos"`
	Type                     string  `json:"type"`
	RunID                    string  `json:"run_id" db:"run_id"`
	SavedQueryID             string  `json:"saved_query_id" db:"saved_query_id"`
	SavedQueryRevision       int64   `json:"saved_query_revision" db:"saved_query_revision"`
	QueryFingerprint         string  `json:"query_fingerprint" db:"query_fingerprint"`
	SnapshotID               string  `json:"snapshot_id" db:"snapshot_id"`
	MemberHash               string  `json:"member_hash" db:"member_hash"`
	Total                    int64   `json:"total" db:"total"`
	TotalBytes               int64   `json:"total_bytes" db:"total_bytes"`
	RanAt                    string  `json:"ran_at" db:"ran_at"`
	ExpiresAt                string  `json:"expires_at" db:"expires_at"`
	PreviousRunID            *string `json:"previous_run_id" db:"previous_run_id"`
	PreviousMemberHash       *string `json:"previous_member_hash" db:"previous_member_hash"`
	PreviousTotal            *int64  `json:"previous_total" db:"previous_total"`
	PreviousQueryFingerprint *string `json:"previous_query_fingerprint" db:"previous_query_fingerprint"`
}

func normalizeSavedQueryRunMetadata(raw jsontext.Value) (jsontext.Value, error) {
	fields, err := decodeMetadataFields(raw)
	if err != nil {
		return nil, err
	}
	if _, exists := fields["photos"]; exists {
		return raw, nil
	}
	fields["photos"] = jsontext.Value("false")
	return json.Marshal(fields, json.Deterministic(true))
}

func savedQueryRunProjection(layout metadataSourceLayout) string {
	if layout.schemaVersion < photoSavedQueryRunsStorageSchemaVersion {
		return `SELECT
			run_id,saved_query_id,saved_query_revision,query_fingerprint,snapshot_id,
			member_hash,total,total_bytes,ran_at,expires_at,previous_run_id,
			previous_member_hash,previous_total,previous_query_fingerprint,0 AS photos
			FROM saved_query_runs `
	}
	return savedQueryRunSelect
}

func exportSavedQueryRuns(ctx context.Context, q metadataQuerier, write metadataWrite, layout metadataSourceLayout) error {
	rows, err := q.QueryContext(ctx, savedQueryRunProjection(layout)+`
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
		Photos: run.Photos, Type: metadataSavedQueryRunType, RunID: run.RunID, SavedQueryID: run.SavedQueryID,
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

var savedQueryRunImportMetadata = newMetadataTable(metadataTable[metadataSavedQueryRun]{
	record: metadataSavedQueryRun{Type: metadataSavedQueryRunType}, table: "saved_query_runs",
	validate: validateSavedQueryRunMetadataRecord})

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
		Photos: record.Photos, RunID: record.RunID, SavedQueryID: record.SavedQueryID,
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

func validateSavedQueryRunMetadataState(ctx context.Context, q metadataQuerier, layout metadataSourceLayout) error {
	if err := exportSavedQueryRuns(ctx, q, func(any) error { return nil }, layout); err != nil {
		return err
	}
	projection := savedQueryRunProjection(layout)
	var invalid bool
	if err := q.QueryRowContext(ctx, `WITH runs AS (`+projection+`) SELECT EXISTS(
		SELECT 1 FROM runs r
		LEFT JOIN saved_queries q ON q.id=r.saved_query_id
		WHERE q.id IS NULL OR q.revision<r.saved_query_revision
	)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validating saved query run definition relations: %w", err)
	}
	if invalid {
		return errors.New("saved query run references an absent definition or future revision")
	}
	if err := q.QueryRowContext(ctx, `WITH runs AS (`+projection+`) SELECT EXISTS(
		SELECT 1 FROM runs r
		LEFT JOIN runs p ON p.run_id=r.previous_run_id
		WHERE r.previous_run_id IS NOT NULL AND (
			p.run_id IS NULL OR p.saved_query_id<>r.saved_query_id OR p.photos<>r.photos OR
			p.member_hash<>r.previous_member_hash OR p.total<>r.previous_total OR
			p.query_fingerprint<>r.previous_query_fingerprint
		)
	)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validating saved query run previous receipt relations: %w", err)
	}
	if invalid {
		return errors.New("saved query run previous receipt does not match durable authority")
	}
	if err := q.QueryRowContext(ctx, `WITH RECURSIVE runs AS (`+projection+`), chain(run_id) AS (
		SELECT run_id FROM runs WHERE previous_run_id IS NULL
		UNION ALL
		SELECT r.run_id FROM runs r JOIN chain ON r.previous_run_id=chain.run_id
	)
	SELECT EXISTS(
		SELECT saved_query_id FROM runs
		GROUP BY saved_query_id,photos
		HAVING SUM(CASE WHEN previous_run_id IS NULL THEN 1 ELSE 0 END)<>1
	) OR EXISTS(
		SELECT previous_run_id FROM runs WHERE previous_run_id IS NOT NULL
		GROUP BY previous_run_id HAVING COUNT(*)<>1
	) OR (SELECT COUNT(*) FROM chain)<>(SELECT COUNT(*) FROM runs)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validating saved query run comparison chain: %w", err)
	}
	if invalid {
		return errors.New("saved query run comparison chain is disconnected or branched")
	}
	return nil
}
