package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"go.kenn.io/docbank/document"
)

// DocumentEventCoverage reports the derivation state for one selected version
// population. Rebuild coverage selects every retained content version; the
// HTTP report selects only current files.
type DocumentEventCoverage struct {
	Selected             int64
	Indexed              int64
	Pending              int64
	Failed               int64
	Unavailable          int64
	MissingMetadata      int64
	InvalidDates         int64
	UnboundProvenance    int64
	OperationalFallbacks int64
	ContractVersion      string
	DeriverFingerprint   string
	InputEpoch           int64
	PublicationEpoch     int64
}

// DocumentEventCoverageReport counts current file heads and browser-safe
// diagnostics. It reads effective initial state without creating the singleton.
func (s *Store) DocumentEventCoverageReport(ctx context.Context) (DocumentEventCoverage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DocumentEventCoverage{}, fmt.Errorf("starting document event coverage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, err := effectiveDocumentEventState(ctx, tx, DocumentEventsDeriverFingerprint)
	if err != nil {
		return DocumentEventCoverage{}, err
	}
	coverage := DocumentEventCoverage{
		ContractVersion: state.ContractVersion, DeriverFingerprint: state.DeriverFingerprint,
		InputEpoch: state.InputEpoch, PublicationEpoch: state.PublicationEpoch,
	}
	const freshIndexed = `a.input_epoch=? AND a.input_revision=COALESCE(d.revision,0)
		AND a.state='indexed' AND h.input_epoch=a.input_epoch
		AND g.inputs_sha256=a.inputs_sha256 AND g.contract_version=?
		AND g.deriver_fingerprint=?`
	query := `SELECT count(*),
		COALESCE(sum(CASE WHEN ` + freshIndexed + ` THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN a.input_epoch=? AND a.input_revision=COALESCE(d.revision,0)
			AND a.state='failed' THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN a.input_epoch=? AND a.input_revision=COALESCE(d.revision,0)
			AND a.state='unavailable' THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN NOT EXISTS (
			SELECT 1 FROM source_metadata_heads smh JOIN source_metadata_generations smg
			ON smg.generation_id=smh.generation_id WHERE smh.source_sha256=cv.blob_hash
			AND smg.contract_version=?
		) THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN NOT EXISTS (
			SELECT 1 FROM provenance_version_bindings pvb
			WHERE pvb.content_version_id=cv.version_id
		) THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN ` + freshIndexed + ` AND EXISTS (
			SELECT 1 FROM json_each(CAST(g.canonical_json AS TEXT),'$.primaries') dep
			JOIN json_each(CAST(g.canonical_json AS TEXT),'$.events') de
			ON json_extract(de.value,'$.event_id')=json_extract(dep.value,'$.event_id')
			WHERE json_extract(dep.value,'$.scope_class')='vault'
			AND json_extract(dep.value,'$.disclosure')='safe'
			AND json_extract(de.value,'$.date_kind') IN ('imported','vault_recorded')
		) THEN 1 ELSE 0 END),0)
		FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id
		LEFT JOIN document_event_dirty d ON d.content_version_id=cv.version_id
		LEFT JOIN document_event_attempts a ON a.content_version_id=cv.version_id
		LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id
		LEFT JOIN document_event_generations g ON g.generation_id=h.generation_id
		WHERE n.kind='file'`
	err = tx.QueryRowContext(ctx, query,
		state.InputEpoch, state.ContractVersion, state.DeriverFingerprint,
		state.InputEpoch, state.InputEpoch,
		document.SourceMetadataContractV1,
		state.InputEpoch, state.ContractVersion, state.DeriverFingerprint,
	).Scan(&coverage.Selected, &coverage.Indexed, &coverage.Failed, &coverage.Unavailable,
		&coverage.MissingMetadata, &coverage.UnboundProvenance, &coverage.OperationalFallbacks)
	if err != nil {
		return DocumentEventCoverage{}, fmt.Errorf("counting document event coverage: %w", err)
	}
	coverage.Pending = coverage.Selected - coverage.Indexed - coverage.Failed - coverage.Unavailable
	if coverage.Pending < 0 {
		return DocumentEventCoverage{}, fmt.Errorf("document event coverage counters: %w", ErrDocumentEventsCorrupt)
	}
	if err := countSafeInvalidDateDiagnostics(ctx, tx, state, &coverage); err != nil {
		return DocumentEventCoverage{}, err
	}
	if err := tx.Commit(); err != nil {
		return DocumentEventCoverage{}, fmt.Errorf("closing document event coverage: %w", err)
	}
	return coverage, nil
}

func countSafeInvalidDateDiagnostics(
	ctx context.Context, tx *sql.Tx, state DocumentEventState, coverage *DocumentEventCoverage,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT a.diagnostic_json
		FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id
		JOIN document_event_attempts a ON a.content_version_id=cv.version_id
		LEFT JOIN document_event_dirty d ON d.content_version_id=cv.version_id
		LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id
		LEFT JOIN document_event_generations g ON g.generation_id=h.generation_id
		WHERE n.kind='file' AND a.input_epoch=?
		AND a.input_revision=COALESCE(d.revision,0) AND (
			a.state IN ('failed','unavailable') OR (a.state='indexed'
			AND h.input_epoch=a.input_epoch AND g.inputs_sha256=a.inputs_sha256
			AND g.contract_version=? AND g.deriver_fingerprint=?))`,
		state.InputEpoch, state.ContractVersion, state.DeriverFingerprint)
	if err != nil {
		return fmt.Errorf("listing document event diagnostics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("scanning document event diagnostics: %w", err)
		}
		canonical, err := validateDocumentEventDiagnostics(raw)
		if err != nil {
			return fmt.Errorf("stored document event diagnostics: %w: %w", err, ErrDocumentEventsCorrupt)
		}
		var diagnostics []document.DocumentEventDiagnosticV1
		if err := json.Unmarshal(canonical, &diagnostics); err != nil {
			return fmt.Errorf("decoding document event diagnostics: %w: %w", err, ErrDocumentEventsCorrupt)
		}
		for _, diagnostic := range diagnostics {
			if diagnostic.Code != "date_unparseable" {
				continue
			}
			if coverage.InvalidDates == math.MaxInt64 {
				return fmt.Errorf("document event diagnostic count overflow: %w", ErrDocumentEventsCorrupt)
			}
			coverage.InvalidDates++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing document event diagnostics: %w", err)
	}
	if err := rows.Close(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("closing document event diagnostics: %w", err)
	}
	return nil
}

// DocumentEventRebuildCoverage counts terminal state over every retained
// version, including historical versions that are not current file heads.
func (s *Store) DocumentEventRebuildCoverage(ctx context.Context) (DocumentEventCoverage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DocumentEventCoverage{}, fmt.Errorf("starting document event rebuild coverage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, err := effectiveDocumentEventState(ctx, tx, DocumentEventsDeriverFingerprint)
	if err != nil {
		return DocumentEventCoverage{}, err
	}
	coverage := DocumentEventCoverage{
		ContractVersion: state.ContractVersion, DeriverFingerprint: state.DeriverFingerprint,
		InputEpoch: state.InputEpoch, PublicationEpoch: state.PublicationEpoch,
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*),
		COALESCE(sum(CASE WHEN a.input_epoch=? AND
			a.input_revision=COALESCE(d.revision,0) AND a.state='indexed' AND
			h.input_epoch=a.input_epoch AND g.inputs_sha256=a.inputs_sha256
			THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN a.input_epoch=? AND
			a.input_revision=COALESCE(d.revision,0) AND a.state='failed'
			THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN a.input_epoch=? AND
			a.input_revision=COALESCE(d.revision,0) AND a.state='unavailable'
			THEN 1 ELSE 0 END),0)
		FROM content_versions cv
		LEFT JOIN document_event_dirty d ON d.content_version_id=cv.version_id
		LEFT JOIN document_event_attempts a ON a.content_version_id=cv.version_id
		LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id
		LEFT JOIN document_event_generations g ON g.generation_id=h.generation_id`,
		state.InputEpoch, state.InputEpoch, state.InputEpoch,
	).Scan(&coverage.Selected, &coverage.Indexed, &coverage.Failed, &coverage.Unavailable)
	if err != nil {
		return DocumentEventCoverage{}, fmt.Errorf("counting document event rebuild coverage: %w", err)
	}
	coverage.Pending = coverage.Selected - coverage.Indexed - coverage.Failed - coverage.Unavailable
	if err := tx.Commit(); err != nil {
		return DocumentEventCoverage{}, fmt.Errorf("closing document event rebuild coverage: %w", err)
	}
	return coverage, nil
}
