package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

// DocumentEventGeneration is one immutable, canonical event projection for an
// exact retained content version and input manifest.
type DocumentEventGeneration struct {
	GenerationID       string
	ContentVersionID   string
	ContractVersion    string
	DeriverFingerprint string
	InputsSHA256       string
	DocumentKind       string
	CanonicalJSON      []byte
	Checksum           string
	EventCount         int
	CreatedAt          string
}

// DocumentEventView binds an active event generation to its exact version and
// publication fence in one read snapshot.
type DocumentEventView struct {
	Version     ContentVersion
	Generation  DocumentEventGeneration
	Events      document.DocumentEventsV1
	InputEpoch  int64
	PublishedAt string
}

// DocumentEventTarget captures the immutable version identity and the
// publication fence observed by the derivation worker.
type DocumentEventTarget struct {
	ContentVersionID string
	BlobHash         string
	MIMEType         string
	RecordedAt       string
	NodeID           int64
	Size             int64
	InputEpoch       int64
	InputRevision    int64
}

// ErrDocumentEventsCorrupt means generation identity, canonical bytes, or
// recorded event metadata disagree.
var ErrDocumentEventsCorrupt = errors.New("timeline index generation does not match its recorded evidence")

// PublishDocumentEvents validates canonical bytes and atomically records the
// immutable generation and the selected head.
func (s *Store) PublishDocumentEvents(
	ctx context.Context,
	target DocumentEventTarget,
	deriverFingerprint, inputsSHA256 string,
	canonical []byte,
) (DocumentEventGeneration, error) {
	if err := validateCatalogSHA256(deriverFingerprint, "document event deriver fingerprint"); err != nil {
		return DocumentEventGeneration{}, err
	}
	if err := validateCatalogSHA256(inputsSHA256, "document event inputs digest"); err != nil {
		return DocumentEventGeneration{}, err
	}
	if target.InputEpoch <= 0 {
		return DocumentEventGeneration{}, errors.New("document event target input epoch must be positive")
	}
	record, checksum, err := document.DecodeDocumentEventsV1(canonical)
	if err != nil {
		return DocumentEventGeneration{}, fmt.Errorf("validating document events: %w", err)
	}
	if record.VaultUID != s.VaultID() {
		return DocumentEventGeneration{}, errors.New("document event record belongs to a different vault")
	}
	if record.ContentVersionID != target.ContentVersionID {
		return DocumentEventGeneration{}, errors.New("document event record belongs to a different content version")
	}
	if record.DocumentKind != DocumentEventKindForMIMEType(target.MIMEType) {
		return DocumentEventGeneration{}, errors.New("document event record does not match the immutable content kind")
	}
	generationID := document.DocumentEventGenerationID(s.VaultID(), target.ContentVersionID,
		record.ContractVersion, deriverFingerprint, inputsSHA256)

	var generation DocumentEventGeneration
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var publishErr error
		generation, publishErr = s.publishDocumentEventsTx(ctx, tx, target, deriverFingerprint,
			inputsSHA256, generationID, checksum, canonical, record)
		return publishErr
	})
	return generation, err
}

// invalidateDocumentEventsForVersionsTx is the transaction seam for callers
// that change event inputs. Input changes, head invalidation, and dirty-state
// publication must commit or roll back together.
func invalidateDocumentEventsForVersionsTx(
	ctx context.Context, tx *sql.Tx, versionIDs []string,
) (int64, error) {
	if err := invalidateDocumentPeopleForVersionsTx(ctx, tx, versionIDs); err != nil {
		return 0, fmt.Errorf("invalidating document people: %w", err)
	}
	seen := make(map[string]struct{}, len(versionIDs))
	var invalidated int64
	for _, versionID := range versionIDs {
		if _, duplicate := seen[versionID]; duplicate {
			continue
		}
		seen[versionID] = struct{}{}
		result, err := tx.ExecContext(ctx,
			`DELETE FROM document_event_heads WHERE content_version_id=?`, versionID)
		if err != nil {
			return 0, fmt.Errorf("invalidating document events for version %s: %w", versionID, err)
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("counting invalidated document events for version %s: %w", versionID, err)
		}
		invalidated += removed

		var retained bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM content_versions WHERE version_id=?)`, versionID,
		).Scan(&retained); err != nil {
			return 0, fmt.Errorf("checking document event version %s: %w", versionID, err)
		}
		if retained {
			if err := markDocumentEventDirtyTx(ctx, tx, versionID, "evidence changed"); err != nil {
				return 0, fmt.Errorf("marking document events for version %s dirty: %w", versionID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_event_generations
			WHERE content_version_id=? AND NOT EXISTS(SELECT 1 FROM document_event_heads h
			WHERE h.generation_id=document_event_generations.generation_id)`, versionID); err != nil {
			return 0, fmt.Errorf("collecting unreferenced document event generations: %w", err)
		}
	}

	return invalidated, nil
}

// publishDocumentEventsTx is the transaction seam used by the derivation
// worker once it has rechecked the target's epoch, revision, and input digest.
func (s *Store) publishDocumentEventsTx(
	ctx context.Context,
	tx *sql.Tx,
	target DocumentEventTarget,
	deriverFingerprint, inputsSHA256, generationID, checksum string,
	canonical []byte,
	record document.DocumentEventsV1,
) (DocumentEventGeneration, error) {
	if err := s.validateDocumentEventFenceTx(ctx, tx, target, deriverFingerprint,
		inputsSHA256, false); err != nil {
		return DocumentEventGeneration{}, err
	}
	diagnostics, err := json.Marshal(record.Diagnostics, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	if err != nil {
		return DocumentEventGeneration{}, fmt.Errorf("encoding document event diagnostics: %w", err)
	}
	diagnostics, err = validateDocumentEventDiagnostics(diagnostics)
	if err != nil {
		return DocumentEventGeneration{}, err
	}

	generation := DocumentEventGeneration{
		GenerationID: generationID, ContentVersionID: target.ContentVersionID,
		ContractVersion: record.ContractVersion, DeriverFingerprint: deriverFingerprint,
		InputsSHA256: inputsSHA256, DocumentKind: string(record.DocumentKind),
		CanonicalJSON: append([]byte(nil), canonical...),
		Checksum:      checksum, EventCount: len(record.Events), CreatedAt: nowRFC3339(),
	}
	replayed, err := checkEventGenerationReplay(ctx, tx, generationID, checksum, canonical)
	if err != nil {
		return DocumentEventGeneration{}, err
	}
	if replayed {
		generation, err = readDocumentEventGeneration(ctx, tx, generationID)
		if err != nil {
			return DocumentEventGeneration{}, err
		}
	} else if err := insertDocumentEventGeneration(ctx, tx, generation, record); err != nil {
		return DocumentEventGeneration{}, err
	}
	if err := checkDocumentEventGeneration(s.VaultID(), generation, record); err != nil {
		return DocumentEventGeneration{}, err
	}

	changed, err := documentEventHeadChanged(ctx, tx, target.ContentVersionID,
		generation.GenerationID, target.InputEpoch)
	if err != nil {
		return DocumentEventGeneration{}, err
	}
	if changed {
		publishedAt := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `INSERT INTO document_event_heads(
			content_version_id,generation_id,input_epoch,published_at
		) VALUES(?,?,?,?) ON CONFLICT(content_version_id) DO UPDATE SET
			generation_id=excluded.generation_id,input_epoch=excluded.input_epoch,
			published_at=excluded.published_at`, target.ContentVersionID,
			generation.GenerationID, target.InputEpoch, publishedAt); err != nil {
			return DocumentEventGeneration{}, fmt.Errorf("selecting document event generation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE document_event_state SET
			publication_epoch=publication_epoch+1,updated_at=? WHERE singleton=1`, publishedAt); err != nil {
			return DocumentEventGeneration{}, fmt.Errorf("advancing document event publication epoch: %w", err)
		}
	}
	if err := recordDocumentEventAttemptTx(ctx, tx, target, inputsSHA256, "indexed", diagnostics); err != nil {
		return DocumentEventGeneration{}, err
	}
	return generation, nil
}

func validateDocumentEventTargetTx(ctx context.Context, tx *sql.Tx, target DocumentEventTarget) error {
	version, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=?`,
		target.ContentVersionID))
	if err != nil {
		return fmt.Errorf("document event target %q: %w", target.ContentVersionID, err)
	}
	if !documentEventTargetMatchesVersion(target, version) {
		return fmt.Errorf("document event target %q no longer matches the retained content version",
			target.ContentVersionID)
	}
	return nil
}

func checkEventGenerationReplay(
	ctx context.Context,
	tx *sql.Tx,
	id, checksum string,
	canonicalJSON []byte,
) (bool, error) {
	var storedChecksum string
	var storedJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT checksum,canonical_json
		FROM document_event_generations WHERE generation_id=?`, id).
		Scan(&storedChecksum, &storedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if storedChecksum != checksum || !bytes.Equal(storedJSON, canonicalJSON) {
		return false, fmt.Errorf("generation %s stored checksum %s, derived %s: %w",
			id, storedChecksum, checksum, ErrDocumentEventsCorrupt)
	}
	return true, nil
}

func insertDocumentEventGeneration(
	ctx context.Context,
	tx *sql.Tx,
	generation DocumentEventGeneration,
	record document.DocumentEventsV1,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO document_event_generations(
		generation_id,content_version_id,contract_version,deriver_fingerprint,inputs_sha256,
		document_kind,canonical_json,checksum,event_count,created_at
	) VALUES(?,?,?,?,?,?,?,?,?,?)`, generation.GenerationID, generation.ContentVersionID,
		generation.ContractVersion, generation.DeriverFingerprint, generation.InputsSHA256,
		generation.DocumentKind, generation.CanonicalJSON,
		generation.Checksum, generation.EventCount, generation.CreatedAt); err != nil {
		return fmt.Errorf("recording document event generation: %w", err)
	}
	for _, event := range record.Events {
		if _, err := tx.ExecContext(ctx, `INSERT INTO document_events(
			generation_id,event_id,source_key,date_kind,source_kind_raw,date_value,raw_value,
			precision,fraction_digits,timezone_kind,zone_text,offset_seconds,axis_key,utc_key,
			claim_basis,parse_confidence,evidence_kind,evidence_id,evidence_sha256,
			evidence_locator,sensitive
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, generation.GenerationID,
			event.EventID, event.SourceKey, event.DateKind, event.SourceKindRaw, event.DateValue,
			event.RawValue, event.Precision, event.FractionDigits, event.TimezoneKind, event.ZoneText,
			nullableDocumentEventOffset(event.OffsetSeconds), event.AxisKey,
			nullableString(event.UTCKey), event.ClaimBasis, event.ParseConfidence,
			event.EvidenceKind, event.EvidenceID, event.EvidenceSHA256, []byte(event.EvidenceLocator),
			documentEventBool(event.Sensitive)); err != nil {
			return fmt.Errorf("recording document event %s: %w", event.EventID, err)
		}
		for _, actor := range event.Actors {
			if _, err := tx.ExecContext(ctx, `INSERT INTO document_event_actors(
				generation_id,event_id,role,ordinal,actor_key,display_name,address,claim_json,
				evidence_kind,evidence_id,sensitive
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, generation.GenerationID, event.EventID, actor.Role,
				actor.Ordinal, actor.ActorKey, actor.DisplayName, actor.Address, []byte(actor.Claim),
				actor.EvidenceKind, actor.EvidenceID, documentEventBool(actor.Sensitive)); err != nil {
				return fmt.Errorf("recording document event %s actor: %w", event.EventID, err)
			}
		}
	}
	for _, primary := range record.Primaries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO document_event_primaries(
			generation_id,scope_class,disclosure,event_id,rule_id,reason
		) VALUES(?,?,?,?,?,?)`, generation.GenerationID, primary.ScopeClass,
			primary.Disclosure, primary.EventID, primary.RuleID, primary.Reason); err != nil {
			return fmt.Errorf("recording document event primary: %w", err)
		}
	}
	return nil
}

func nullableDocumentEventOffset(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func documentEventBool(value bool) int {
	if value {
		return 1
	}
	return 0
}

func documentEventHeadChanged(
	ctx context.Context,
	tx *sql.Tx,
	versionID, generationID string,
	inputEpoch int64,
) (bool, error) {
	var storedGeneration string
	var storedEpoch int64
	err := tx.QueryRowContext(ctx, `SELECT generation_id,input_epoch FROM document_event_heads
		WHERE content_version_id=?`, versionID).Scan(&storedGeneration, &storedEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading document event head: %w", err)
	}
	return storedGeneration != generationID || storedEpoch != inputEpoch, nil
}

// DocumentEventsForVersion returns the selected canonical event record and
// validates its canonical bytes and recorded identity.
func (s *Store) DocumentEventsForVersion(
	ctx context.Context,
	versionID string,
) (DocumentEventView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DocumentEventView{}, fmt.Errorf("starting document event snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var view DocumentEventView
	err = tx.QueryRowContext(ctx, `SELECT g.generation_id,g.content_version_id,
		g.contract_version,g.deriver_fingerprint,g.inputs_sha256,g.document_kind,
		g.canonical_json,g.checksum,g.event_count,g.created_at,
		h.input_epoch,h.published_at
		FROM document_event_heads h JOIN document_event_generations g
		ON g.generation_id=h.generation_id WHERE h.content_version_id=?`, versionID).
		Scan(&view.Generation.GenerationID, &view.Generation.ContentVersionID,
			&view.Generation.ContractVersion, &view.Generation.DeriverFingerprint,
			&view.Generation.InputsSHA256, &view.Generation.DocumentKind,
			&view.Generation.CanonicalJSON,
			&view.Generation.Checksum, &view.Generation.EventCount,
			&view.Generation.CreatedAt, &view.InputEpoch, &view.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentEventView{}, fmt.Errorf("document events for version %q: %w", versionID, ErrNotFound)
	}
	if err != nil {
		return DocumentEventView{}, fmt.Errorf("reading document event generation: %w", err)
	}
	view.Version, err = scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=?`, versionID))
	if err != nil {
		return DocumentEventView{}, fmt.Errorf("reading document event version: %w", err)
	}
	view.Events, _, err = document.DecodeDocumentEventsV1(view.Generation.CanonicalJSON)
	if err != nil {
		return DocumentEventView{}, documentEventCorruption(view.Generation.GenerationID, err)
	}
	if view.Version.ID != versionID || view.Generation.ContentVersionID != versionID ||
		view.Events.ContentVersionID != versionID {
		return DocumentEventView{}, documentEventCorruption(view.Generation.GenerationID, nil)
	}
	if err := checkDocumentEventGeneration(s.VaultID(), view.Generation, view.Events); err != nil {
		return DocumentEventView{}, err
	}
	if err := tx.Commit(); err != nil {
		return DocumentEventView{}, fmt.Errorf("closing document event snapshot: %w", err)
	}
	view.Generation.CanonicalJSON = append([]byte(nil), view.Generation.CanonicalJSON...)
	return view, nil
}

func readDocumentEventGeneration(
	ctx context.Context,
	tx *sql.Tx,
	generationID string,
) (DocumentEventGeneration, error) {
	var generation DocumentEventGeneration
	err := tx.QueryRowContext(ctx, `SELECT generation_id,content_version_id,contract_version,
		deriver_fingerprint,inputs_sha256,document_kind,canonical_json,
		checksum,event_count,created_at FROM document_event_generations WHERE generation_id=?`,
		generationID).Scan(&generation.GenerationID, &generation.ContentVersionID,
		&generation.ContractVersion, &generation.DeriverFingerprint, &generation.InputsSHA256,
		&generation.DocumentKind, &generation.CanonicalJSON,
		&generation.Checksum, &generation.EventCount, &generation.CreatedAt)
	if err != nil {
		return DocumentEventGeneration{}, fmt.Errorf("reading document event generation: %w", err)
	}
	return generation, nil
}

func checkDocumentEventGeneration(
	vaultID string,
	generation DocumentEventGeneration,
	record document.DocumentEventsV1,
) error {
	_, checksum, err := document.DecodeDocumentEventsV1(generation.CanonicalJSON)
	if err != nil {
		return documentEventCorruption(generation.GenerationID, err)
	}
	wantID := document.DocumentEventGenerationID(vaultID, record.ContentVersionID,
		record.ContractVersion, generation.DeriverFingerprint, generation.InputsSHA256)
	if record.VaultUID != vaultID || generation.GenerationID != wantID ||
		generation.ContentVersionID != record.ContentVersionID ||
		generation.ContractVersion != record.ContractVersion ||
		generation.DocumentKind != string(record.DocumentKind) ||
		generation.Checksum != checksum || generation.EventCount != len(record.Events) {
		return documentEventCorruption(generation.GenerationID, nil)
	}
	return nil
}

func documentEventCorruption(generationID string, cause error) error {
	if cause == nil {
		return fmt.Errorf("generation %s: %w", generationID, ErrDocumentEventsCorrupt)
	}
	return fmt.Errorf("generation %s: %w: %w", generationID, cause, ErrDocumentEventsCorrupt)
}
