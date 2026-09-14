package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"mime"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

// ErrDocumentEventEvidenceUnavailable means the complete evidence authority
// was captured and hashed, but bounded derivation inputs cannot represent it.
// Callers may persist an unavailable terminal attempt when the returned
// snapshot carries a nonempty InputsSHA256.
var ErrDocumentEventEvidenceUnavailable = errors.New("document event evidence exceeds derivation bounds")

// BoundProvenanceEventInput is the immutable provenance evidence joined
// through an exact content-version binding.
type BoundProvenanceEventInput struct {
	Binding         ProvenanceVersionBinding
	ClaimIdentity   string
	OriginalMTime   *string
	IngestStartedAt string
	EvidenceSHA256  string
}

// DocumentEventEvidenceSnapshot is one transactionally consistent manifest
// of every retained input used to derive an exact version's event record.
type DocumentEventEvidenceSnapshot struct {
	VaultUID           string
	Target             DocumentEventTarget
	MetadataGeneration SourceMetadataGeneration
	Metadata           document.SourceMetadataV1
	Bindings           []BoundProvenanceEventInput
	InputsSHA256       string
}

type documentEventMetadataManifest struct {
	Checksum             string `json:"checksum"`
	ContractVersion      string `json:"contract_version"`
	ExtractorFingerprint string `json:"extractor_fingerprint"`
	GenerationID         string `json:"generation_id"`
	Present              bool   `json:"present"`
	SourceSHA256         string `json:"source_sha256"`
}

type documentEventBindingManifest struct {
	BasisRef             string `json:"basis_ref"`
	ClaimIdentity        string `json:"claim_identity"`
	ContentVersionID     string `json:"content_version_id"`
	EvidenceSHA256       string `json:"evidence_sha256"`
	IngestID             string `json:"ingest_id"`
	IngestStartedAt      string `json:"ingest_started_at"`
	ObservedAt           string `json:"observed_at"`
	OriginalMTime        string `json:"original_mtime"`
	OriginalMTimePresent bool   `json:"original_mtime_present"`
	ProvenanceIdentity   string `json:"provenance_identity"`
}

type documentEventManifestHeader struct {
	DeriverDescriptor  string                        `json:"deriver_descriptor"`
	DeriverFingerprint string                        `json:"deriver_fingerprint"`
	DocumentKind       document.DocumentKind         `json:"document_kind"`
	Metadata           documentEventMetadataManifest `json:"metadata"`
	Target             documentEventManifestTarget   `json:"target"`
	VaultUID           string                        `json:"vault_uid"`
}

type documentEventManifestTrailer struct {
	BindingCount int64 `json:"binding_count"`
}

type documentEventManifestTarget struct {
	BlobHash         string `json:"blob_hash"`
	ContentVersionID string `json:"content_version_id"`
	MIMEType         string `json:"mime_type"`
	NodeID           int64  `json:"node_id"`
	RecordedAt       string `json:"recorded_at"`
	Size             int64  `json:"size"`
}

// LoadDocumentEventEvidence captures every input in one read transaction.
// Captured semantic corruption or bounded-input exhaustion returns a populated
// snapshot and digest with a distinguishable error so a worker can persist a
// fenced terminal attempt. Transactional capture failures return no digest.
func (s *Store) LoadDocumentEventEvidence(
	ctx context.Context,
	target DocumentEventTarget,
) (DocumentEventEvidenceSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DocumentEventEvidenceSnapshot{}, fmt.Errorf("starting document event evidence snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	snapshot, semanticErr := s.loadDocumentEventEvidenceTx(ctx, tx, target, true)
	if semanticErr != nil && snapshot.InputsSHA256 == "" {
		return DocumentEventEvidenceSnapshot{}, semanticErr
	}
	if err := tx.Commit(); err != nil {
		return DocumentEventEvidenceSnapshot{}, fmt.Errorf("closing document event evidence snapshot: %w", err)
	}
	return snapshot, semanticErr
}

func (s *Store) loadDocumentEventEvidenceTx(
	ctx context.Context,
	tx *sql.Tx,
	target DocumentEventTarget,
	decodeMetadata bool,
) (DocumentEventEvidenceSnapshot, error) {
	version, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=?`,
		target.ContentVersionID))
	if err != nil {
		return DocumentEventEvidenceSnapshot{}, fmt.Errorf("reading document event evidence version: %w", err)
	}
	if !documentEventTargetMatchesVersion(target, version) {
		return DocumentEventEvidenceSnapshot{}, fmt.Errorf("%w: retained content version does not match target", ErrDocumentEventInputsChanged)
	}
	var vaultUID string
	if err := tx.QueryRowContext(ctx, `SELECT vault_uid FROM vault_metadata WHERE singleton=1`).Scan(&vaultUID); err != nil {
		return DocumentEventEvidenceSnapshot{}, fmt.Errorf("reading document event evidence vault: %w", err)
	}

	snapshot := DocumentEventEvidenceSnapshot{
		VaultUID: vaultUID, Target: target, Bindings: []BoundProvenanceEventInput{},
	}
	metadataManifest, semanticErr, err := loadDocumentEventMetadataEvidence(ctx, tx, target.BlobHash, &snapshot, decodeMetadata)
	if err != nil {
		return DocumentEventEvidenceSnapshot{}, err
	}
	manifestHash := sha256.New()
	header := documentEventManifestHeader{
		VaultUID: vaultUID,
		Target: documentEventManifestTarget{
			ContentVersionID: target.ContentVersionID, NodeID: target.NodeID,
			BlobHash: target.BlobHash, Size: target.Size, MIMEType: target.MIMEType,
			RecordedAt: target.RecordedAt,
		},
		DocumentKind:       DocumentEventKindForMIMEType(target.MIMEType),
		Metadata:           metadataManifest,
		DeriverDescriptor:  DocumentEventsDeriverDescriptor,
		DeriverFingerprint: DocumentEventsDeriverFingerprint,
	}
	if err := writeDocumentEventManifestFrame(manifestHash, header); err != nil {
		return DocumentEventEvidenceSnapshot{}, err
	}
	bindingCount, boundErr, err := loadBoundDocumentEventEvidence(
		ctx, tx, target.ContentVersionID, &snapshot, manifestHash,
	)
	if err != nil {
		return DocumentEventEvidenceSnapshot{}, err
	}
	if err := writeDocumentEventManifestFrame(manifestHash, documentEventManifestTrailer{
		BindingCount: bindingCount,
	}); err != nil {
		return DocumentEventEvidenceSnapshot{}, err
	}
	snapshot.InputsSHA256 = hex.EncodeToString(manifestHash.Sum(nil))
	return snapshot, errors.Join(semanticErr, boundErr)
}

func loadDocumentEventMetadataEvidence(
	ctx context.Context,
	tx *sql.Tx,
	blobHash string,
	snapshot *DocumentEventEvidenceSnapshot,
	decodeMetadata bool,
) (documentEventMetadataManifest, error, error) {
	var generation SourceMetadataGeneration
	err := tx.QueryRowContext(ctx, `SELECT g.generation_id,g.source_sha256,g.contract_version,
		g.extractor_fingerprint,g.checksum,g.created_at
		FROM source_metadata_heads h JOIN source_metadata_generations g
		ON g.generation_id=h.generation_id WHERE h.source_sha256=?`, blobHash).Scan(
		&generation.GenerationID, &generation.SourceSHA256, &generation.ContractVersion,
		&generation.ExtractorFingerprint, &generation.Checksum, &generation.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return documentEventMetadataManifest{Present: false}, nil, nil
	}
	if err != nil {
		return documentEventMetadataManifest{}, nil, fmt.Errorf("reading document event metadata identity: %w", err)
	}
	manifest := documentEventMetadataManifest{
		Present: true, GenerationID: generation.GenerationID,
		SourceSHA256: generation.SourceSHA256, ContractVersion: generation.ContractVersion,
		ExtractorFingerprint: generation.ExtractorFingerprint, Checksum: generation.Checksum,
	}
	snapshot.MetadataGeneration = generation
	// The stored generation and checksum identify immutable source bytes. A
	// publication fence compares this identity without rereading or decoding
	// the payload while the write transaction is held.
	if !decodeMetadata {
		return manifest, nil, nil
	}
	var byteLength int64
	if err := tx.QueryRowContext(ctx, `SELECT length(CAST(canonical_json AS BLOB))
		FROM source_metadata_generations WHERE generation_id=?`, generation.GenerationID).Scan(&byteLength); err != nil {
		return documentEventMetadataManifest{}, nil, fmt.Errorf("reading source metadata length: %w", err)
	}
	if byteLength > document.MaxSourceMetadataEncodedBytes {
		return manifest, fmt.Errorf("source metadata generation %s exceeds the canonical byte bound: %w",
			generation.GenerationID, ErrDocumentEventEvidenceUnavailable), nil
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT CAST(canonical_json AS BLOB)
		FROM source_metadata_generations WHERE generation_id=?`, generation.GenerationID).Scan(&raw); err != nil {
		return documentEventMetadataManifest{}, nil, fmt.Errorf("reading source metadata evidence: %w", err)
	}
	snapshot.MetadataGeneration.CanonicalJSON = raw
	metadata, checksum, decodeErr := document.DecodeSourceMetadataV1(raw)
	if decodeErr != nil || checksum != generation.Checksum {
		return manifest, fmt.Errorf("source metadata generation %s: %w",
			generation.GenerationID, ErrSourceMetadataCorrupt), nil
	}
	snapshot.Metadata = metadata
	return manifest, nil, nil
}

func loadBoundDocumentEventEvidence(
	ctx context.Context,
	tx *sql.Tx,
	versionID string,
	snapshot *DocumentEventEvidenceSnapshot,
	manifestHash hash.Hash,
) (int64, error, error) {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE resolved_binding AS (
		SELECT provenance_identity,content_version_id,observed_at,basis_ref,
			provenance_identity AS claim_identity
		FROM provenance_version_bindings WHERE content_version_id=?
		UNION ALL
		SELECT b.provenance_identity,b.content_version_id,b.observed_at,b.basis_ref,p.identity
		FROM resolved_binding b JOIN provenance p ON p.supersedes=b.claim_identity
	) SELECT b.provenance_identity,b.content_version_id,b.observed_at,b.basis_ref,
		claim.original_mtime,original.ingest_id,i.started_at,b.claim_identity
		FROM resolved_binding b JOIN provenance original ON original.identity=b.provenance_identity
		JOIN ingests i ON i.id=original.ingest_id
		JOIN provenance claim ON claim.identity=b.claim_identity
		WHERE NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=b.claim_identity)
		ORDER BY b.provenance_identity,b.claim_identity`, versionID)
	if err != nil {
		return 0, nil, fmt.Errorf("reading bound document event evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var bindingCount int64
	retainedEventCount := 1 // the content-version adapter always emits one event
	retainBindings := true
	var boundErr error
	for rows.Next() {
		var input BoundProvenanceEventInput
		var originalMTime sql.NullString
		var ingestID string
		if err := rows.Scan(&input.Binding.ProvenanceIdentity, &input.Binding.ContentVersionID,
			&input.Binding.ObservedAt, &input.Binding.BasisRef, &originalMTime, &ingestID,
			&input.IngestStartedAt, &input.ClaimIdentity); err != nil {
			return 0, nil, fmt.Errorf("scanning bound document event evidence: %w", err)
		}
		if originalMTime.Valid {
			value := originalMTime.String
			input.OriginalMTime = &value
		}
		manifest := documentEventBindingManifest{
			ProvenanceIdentity: input.Binding.ProvenanceIdentity,
			ClaimIdentity:      input.ClaimIdentity,
			ContentVersionID:   input.Binding.ContentVersionID,
			ObservedAt:         input.Binding.ObservedAt, BasisRef: input.Binding.BasisRef,
			OriginalMTimePresent: originalMTime.Valid, OriginalMTime: originalMTime.String,
			IngestID: ingestID, IngestStartedAt: input.IngestStartedAt,
		}
		evidenceBytes, err := canonical.Marshal(manifest)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding bound provenance evidence: %w", err)
		}
		sum := sha256.Sum256(evidenceBytes)
		input.EvidenceSHA256 = hex.EncodeToString(sum[:])
		manifest.EvidenceSHA256 = input.EvidenceSHA256
		if err := writeDocumentEventManifestFrame(manifestHash, manifest); err != nil {
			return 0, nil, err
		}
		bindingCount++
		eventCount := 1
		if input.OriginalMTime != nil {
			eventCount++
		}
		if retainBindings && retainedEventCount+eventCount <= document.MaxDocumentEvents {
			snapshot.Bindings = append(snapshot.Bindings, input)
			retainedEventCount += eventCount
		} else {
			retainBindings = false
			boundErr = ErrDocumentEventEvidenceUnavailable
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("reading bound document event evidence: %w", err)
	}
	return bindingCount, boundErr, nil
}

func writeDocumentEventManifestFrame(destination hash.Hash, value any) error {
	encoded, err := canonical.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding document event evidence manifest: %w", err)
	}
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(encoded)))
	_, _ = destination.Write(size[:])
	_, _ = destination.Write(encoded)
	return nil
}

func documentEventTargetMatchesVersion(target DocumentEventTarget, version ContentVersion) bool {
	return version.ID == target.ContentVersionID && version.NodeID == target.NodeID &&
		version.BlobHash == target.BlobHash && version.Size == target.Size &&
		version.MimeType == target.MIMEType && version.RecordedAt == target.RecordedAt
}

// DocumentEventKindForMIMEType returns the immutable document kind used by
// evidence manifests, derivation, and publication validation.
func DocumentEventKindForMIMEType(mimeType string) document.DocumentKind {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return "other"
	}
	switch {
	case mediaType == "message/rfc822":
		return "email"
	case mediaType == "text/calendar":
		return "calendar"
	case strings.HasPrefix(mediaType, "image/"):
		return "image"
	case strings.HasPrefix(mediaType, "audio/"), strings.HasPrefix(mediaType, "video/"):
		return "audio_video"
	default:
		return "other"
	}
}
