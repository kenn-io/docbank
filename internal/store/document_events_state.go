package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

const maxDocumentEventDiagnosticBytes = document.MaxDocumentEventsEncodedBytes

var (
	// ErrDocumentEventInputsChanged means retained authority changed after a
	// target was captured, so its derived result must be retried.
	ErrDocumentEventInputsChanged = errors.New("document event inputs changed while deriving")
)

const (
	// DocumentEventsDeriverDescriptor names every adapter participating in the
	// initial derivation recipe. Store owns this identity so rebuild receipt
	// creation can install the recipe without importing processing.
	DocumentEventsDeriverDescriptor = "docbank-document-events:f10-metadata+content-version+provenance-binding+actor-evidence:v2"
	// DocumentEventsDeriverFingerprint is the SHA-256 of
	// DocumentEventsDeriverDescriptor, pinned independently by tests.
	DocumentEventsDeriverFingerprint = "a52b7a303d578b028e6001f7dd84c9598776c62f325c095d6a5bdfcc3a6fcbc3"
)

// DocumentEventState is the durable recipe and publication fence for the
// rebuildable event projection.
type DocumentEventState struct {
	ContractVersion    string
	DeriverFingerprint string
	InputEpoch         int64
	PublicationEpoch   int64
	UpdatedAt          string
}

// DocumentEventStateRow reads the durable event state. A missing singleton is
// represented by the effective initial epoch without making a pristine vault
// non-empty.
func (s *Store) DocumentEventStateRow(
	ctx context.Context,
	fingerprint string,
) (DocumentEventState, error) {
	if err := validateCatalogSHA256(fingerprint, "document event deriver fingerprint"); err != nil {
		return DocumentEventState{}, err
	}
	return effectiveDocumentEventState(ctx, s.db, fingerprint)
}

func effectiveDocumentEventState(
	ctx context.Context,
	q metadataQuerier,
	fingerprint string,
) (DocumentEventState, error) {
	state, exists, err := storedDocumentEventState(ctx, q)
	if err != nil {
		return DocumentEventState{}, err
	}
	if !exists {
		return DocumentEventState{
			ContractVersion:    document.DocumentEventsContractV1,
			DeriverFingerprint: fingerprint,
			InputEpoch:         1,
			PublicationEpoch:   1,
		}, nil
	}
	if state.DeriverFingerprint != fingerprint {
		state.DeriverFingerprint = fingerprint
		state.InputEpoch++
	}
	return state, nil
}

func storedDocumentEventState(
	ctx context.Context,
	q metadataQuerier,
) (DocumentEventState, bool, error) {
	var state DocumentEventState
	err := q.QueryRowContext(ctx, `SELECT contract_version,deriver_fingerprint,
		input_epoch,publication_epoch,updated_at FROM document_event_state WHERE singleton=1`).
		Scan(&state.ContractVersion, &state.DeriverFingerprint, &state.InputEpoch,
			&state.PublicationEpoch, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentEventState{}, false, nil
	}
	if err != nil {
		return DocumentEventState{}, false, fmt.Errorf("reading document event state: %w", err)
	}
	if state.ContractVersion != document.DocumentEventsContractV1 {
		return DocumentEventState{}, false, fmt.Errorf("unsupported document event contract %q", state.ContractVersion)
	}
	if err := validateCatalogSHA256(state.DeriverFingerprint, "stored document event deriver fingerprint"); err != nil {
		return DocumentEventState{}, false, err
	}
	return state, true, nil
}

// EnsureDocumentEventRecipe installs the active recipe when retained versions
// exist and advances the input epoch once when that recipe changes.
func (s *Store) EnsureDocumentEventRecipe(ctx context.Context, fingerprint string) error {
	if err := validateCatalogSHA256(fingerprint, "document event deriver fingerprint"); err != nil {
		return err
	}
	state, exists, err := storedDocumentEventState(ctx, s.db)
	if err != nil || (exists && state.DeriverFingerprint == fingerprint) {
		return err
	}
	if !exists {
		var retained bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions)`).Scan(&retained); err != nil {
			return fmt.Errorf("checking document event targets: %w", err)
		}
		if !retained {
			return nil
		}
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		state, exists, err := storedDocumentEventStateTx(ctx, tx)
		if err != nil {
			return err
		}
		if !exists {
			var retained bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions)`).Scan(&retained); err != nil {
				return fmt.Errorf("checking document event targets: %w", err)
			}
			if !retained {
				return nil
			}
			return insertDocumentEventStateTx(ctx, tx, fingerprint, 1)
		}
		if state.DeriverFingerprint == fingerprint {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE document_event_state SET
			deriver_fingerprint=?,input_epoch=input_epoch+1,updated_at=? WHERE singleton=1`,
			fingerprint, nowRFC3339())
		if err != nil {
			return fmt.Errorf("updating document event recipe: %w", err)
		}
		return nil
	})
}

func storedDocumentEventStateTx(
	ctx context.Context,
	tx *sql.Tx,
) (DocumentEventState, bool, error) {
	return storedDocumentEventState(ctx, tx)
}

func insertDocumentEventStateTx(
	ctx context.Context,
	tx *sql.Tx,
	fingerprint string,
	inputEpoch int64,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO document_event_state(
		singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at
	) VALUES(1,?,?,?,?,?)`, document.DocumentEventsContractV1, fingerprint, inputEpoch, 1,
		nowRFC3339())
	if err != nil {
		return fmt.Errorf("recording document event state: %w", err)
	}
	return nil
}

// BumpDocumentEventInputEpoch invalidates every terminal attempt for an
// explicit rebuild. Installing a changed recipe and advancing the epoch are a
// single change, so callers never double-bump.
func (s *Store) BumpDocumentEventInputEpoch(
	ctx context.Context,
	fingerprint string,
) (int64, error) {
	if err := validateCatalogSHA256(fingerprint, "document event deriver fingerprint"); err != nil {
		return 0, err
	}
	var epoch int64
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		epoch, err = bumpDocumentEventInputEpochTx(ctx, tx, fingerprint)
		return err
	})
	return epoch, err
}

func bumpDocumentEventInputEpochTx(
	ctx context.Context,
	tx *sql.Tx,
	fingerprint string,
) (int64, error) {
	state, exists, err := storedDocumentEventStateTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if !exists {
		if err := insertDocumentEventStateTx(ctx, tx, fingerprint, 2); err != nil {
			return 0, err
		}
		return 2, nil
	}
	epoch := state.InputEpoch + 1
	_, err = tx.ExecContext(ctx, `UPDATE document_event_state SET
		deriver_fingerprint=?,input_epoch=?,updated_at=? WHERE singleton=1`,
		fingerprint, epoch, nowRFC3339())
	if err != nil {
		return 0, fmt.Errorf("advancing document event input epoch: %w", err)
	}
	return epoch, nil
}

func markDocumentEventDirtyTx(ctx context.Context, tx *sql.Tx, id, reason string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO document_event_dirty(content_version_id,revision,reason)
		VALUES(?,1,?) ON CONFLICT(content_version_id) DO UPDATE SET
		revision=revision+1,reason=excluded.reason`, id, reason)
	return err
}

// RecordDocumentEventAttempt stores a deterministic failed or unavailable
// outcome. Indexed attempts are reserved for PublishDocumentEvents, which can
// prove the matching generation and head in the same transaction.
func (s *Store) RecordDocumentEventAttempt(
	ctx context.Context,
	target DocumentEventTarget,
	inputsSHA256, state string,
	diagnostics []byte,
) error {
	if err := validateCatalogSHA256(inputsSHA256, "document event inputs digest"); err != nil {
		return err
	}
	if state != "failed" && state != "unavailable" {
		return errors.New("document event attempt state must be failed or unavailable")
	}
	canonicalDiagnostics, err := validateDocumentEventDiagnostics(diagnostics)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := s.validateDocumentEventFenceTx(ctx, tx, target,
			DocumentEventsDeriverFingerprint, inputsSHA256, true); err != nil {
			return err
		}
		return recordDocumentEventAttemptTx(ctx, tx, target, inputsSHA256, state,
			canonicalDiagnostics)
	})
}

func validateDocumentEventDiagnostics(raw []byte) ([]byte, error) {
	if len(raw) > maxDocumentEventDiagnosticBytes {
		return nil, fmt.Errorf("document event diagnostics exceed %d bytes", maxDocumentEventDiagnosticBytes)
	}
	canonical := jsontext.Value(raw).Clone()
	if canonical.Kind() != '[' {
		return nil, errors.New("document event diagnostics must be a JSON array")
	}
	if err := canonical.Format(jsontext.ReorderRawObjects(true), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true)); err != nil || !bytes.Equal(canonical, raw) {
		return nil, errors.New("document event diagnostics must be canonical JSON")
	}
	return canonical, nil
}

func (s *Store) validateDocumentEventFenceTx(
	ctx context.Context,
	tx *sql.Tx,
	target DocumentEventTarget,
	expectedFingerprint, inputsSHA256 string,
	allowInvalidEvidence bool,
) error {
	if target.InputEpoch <= 0 || target.InputRevision < 0 {
		return ErrDocumentEventInputsChanged
	}
	if err := validateDocumentEventTargetTx(ctx, tx, target); err != nil {
		return fmt.Errorf("%w: %w", ErrDocumentEventInputsChanged, err)
	}
	state, exists, err := storedDocumentEventStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if !exists {
		if expectedFingerprint == "" || target.InputEpoch != 1 {
			return ErrDocumentEventInputsChanged
		}
		if err := insertDocumentEventStateTx(ctx, tx, expectedFingerprint, 1); err != nil {
			return err
		}
		state = DocumentEventState{
			DeriverFingerprint: expectedFingerprint,
			InputEpoch:         1,
		}
	}
	if target.InputEpoch != state.InputEpoch ||
		(expectedFingerprint != "" && expectedFingerprint != state.DeriverFingerprint) {
		return ErrDocumentEventInputsChanged
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision
		FROM document_event_dirty WHERE content_version_id=?),0)`,
		target.ContentVersionID).Scan(&revision); err != nil {
		return fmt.Errorf("reading document event dirty revision: %w", err)
	}
	if revision != target.InputRevision {
		return ErrDocumentEventInputsChanged
	}
	snapshot, evidenceErr := s.loadDocumentEventEvidenceTx(ctx, tx, target, false)
	if snapshot.InputsSHA256 == "" {
		if evidenceErr != nil {
			return evidenceErr
		}
		return ErrDocumentEventInputsChanged
	}
	if snapshot.InputsSHA256 != inputsSHA256 {
		return ErrDocumentEventInputsChanged
	}
	if evidenceErr != nil && (!allowInvalidEvidence ||
		(!errors.Is(evidenceErr, ErrSourceMetadataCorrupt) &&
			!errors.Is(evidenceErr, ErrDocumentEventEvidenceUnavailable))) {
		return evidenceErr
	}
	return nil
}

func recordDocumentEventAttemptTx(
	ctx context.Context,
	tx *sql.Tx,
	target DocumentEventTarget,
	inputsSHA256, state string,
	diagnostics []byte,
) error {
	var storedEpoch, storedRevision int64
	var storedInputs, storedState string
	var storedDiagnostics []byte
	err := tx.QueryRowContext(ctx, `SELECT input_epoch,input_revision,inputs_sha256,state,
		diagnostic_json FROM document_event_attempts WHERE content_version_id=?`,
		target.ContentVersionID).Scan(&storedEpoch, &storedRevision, &storedInputs,
		&storedState, &storedDiagnostics)
	if err == nil && storedEpoch == target.InputEpoch && storedRevision == target.InputRevision &&
		storedInputs == inputsSHA256 && storedState == state && bytes.Equal(storedDiagnostics, diagnostics) {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading document event attempt: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO document_event_attempts(
		content_version_id,input_epoch,input_revision,inputs_sha256,state,diagnostic_json,attempted_at
	) VALUES(?,?,?,?,?,?,?) ON CONFLICT(content_version_id) DO UPDATE SET
		input_epoch=excluded.input_epoch,input_revision=excluded.input_revision,
		inputs_sha256=excluded.inputs_sha256,state=excluded.state,
		diagnostic_json=excluded.diagnostic_json,attempted_at=excluded.attempted_at`,
		target.ContentVersionID, target.InputEpoch, target.InputRevision, inputsSHA256,
		state, diagnostics, nowRFC3339())
	if err != nil {
		return fmt.Errorf("recording document event attempt: %w", err)
	}
	return nil
}

// MissingDocumentEventTargetsAfter returns retained versions whose terminal
// attempt does not match the requested recipe epoch and exact dirty revision.
func (s *Store) MissingDocumentEventTargetsAfter(
	ctx context.Context,
	fingerprint, afterVersionID string,
	limit int,
) ([]DocumentEventTarget, error) {
	if err := validateCatalogSHA256(fingerprint, "document event deriver fingerprint"); err != nil {
		return nil, err
	}
	if afterVersionID != "" {
		if err := validateUUIDv4(afterVersionID); err != nil {
			return nil, errors.New("document event backfill cursor must be a UUID")
		}
	}
	if limit < 1 || limit > 1000 {
		return nil, errors.New("document event backfill limit must be between 1 and 1000")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("starting document event target snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := effectiveDocumentEventState(ctx, tx, fingerprint)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT cv.version_id,cv.node_id,cv.blob_hash,
		cv.size,COALESCE(cv.mime_type,''),cv.recorded_at,?,COALESCE(d.revision,0)
		FROM content_versions cv JOIN nodes n ON n.id=cv.node_id
		LEFT JOIN document_event_dirty d ON d.content_version_id=cv.version_id
		LEFT JOIN document_event_attempts a ON a.content_version_id=cv.version_id
		LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id
		LEFT JOIN document_event_generations g ON g.generation_id=h.generation_id
		WHERE cv.version_id>? AND (a.content_version_id IS NULL OR a.input_epoch<?
			OR a.input_revision<>COALESCE(d.revision,0)
			OR a.state NOT IN ('indexed','failed','unavailable')
			OR (a.state='indexed' AND (h.content_version_id IS NULL
				OR h.input_epoch<>a.input_epoch
				OR COALESCE(g.inputs_sha256,'')<>a.inputs_sha256)))
		ORDER BY cv.version_id LIMIT ?`, state.InputEpoch, afterVersionID, state.InputEpoch, limit)
	if err != nil {
		return nil, fmt.Errorf("listing document event targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	targets := make([]DocumentEventTarget, 0)
	for rows.Next() {
		var target DocumentEventTarget
		if err := rows.Scan(&target.ContentVersionID, &target.NodeID, &target.BlobHash,
			&target.Size, &target.MIMEType, &target.RecordedAt, &target.InputEpoch,
			&target.InputRevision); err != nil {
			return nil, fmt.Errorf("scanning document event target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing document event targets: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing document event targets: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("closing document event target snapshot: %w", err)
	}
	return targets, nil
}
