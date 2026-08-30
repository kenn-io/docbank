package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/packstore"
)

// SourceMetadataGeneration is immutable local evidence extracted from one
// exact original blob by one versioned extractor.
type SourceMetadataGeneration struct {
	GenerationID         string
	SourceSHA256         string
	ContractVersion      string
	ExtractorFingerprint string
	CanonicalJSON        []byte
	Checksum             string
	CreatedAt            string
}

// SourceMetadataTarget is one retained original missing a generation from the
// current extractor.
type SourceMetadataTarget struct {
	SourceSHA256 string
	Size         int64
}

// SourceMetadataAttachmentFacts are attachment-scoped facts joined at read
// time. They deliberately are not copied into content-derived evidence.
type SourceMetadataAttachmentFacts struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	Filename         string `json:"filename,omitempty"`
	Extension        string `json:"extension,omitempty"`
	Path             string `json:"path,omitempty"`
	SourcePath       string `json:"source_path,omitempty"`
	IngestedAt       string `json:"ingested_at,omitempty"`
	FilesystemMTime  string `json:"filesystem_mtime,omitempty"`
}

// SourceMetadataView combines immutable byte-derived evidence with the
// attachment facts appropriate to the requested content version.
type SourceMetadataView struct {
	Generation SourceMetadataGeneration
	Metadata   document.SourceMetadataV1
	Attachment SourceMetadataAttachmentFacts
}

// NodeSourceMetadataView binds a node, its path, and any active source
// metadata to one read snapshot.
type NodeSourceMetadataView struct {
	Node           Node
	Path           string
	SourceMetadata *SourceMetadataView
}

func sourceMetadataGenerationID(source, contract, fingerprint, checksum string) string {
	h := sha256.New()
	for _, value := range []string{source, contract, fingerprint, checksum} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PublishSourceMetadata validates and atomically publishes one immutable
// generation. Retrying the identical publication is idempotent.
func (s *Store) PublishSourceMetadata(
	ctx context.Context, sourceSHA256, extractorFingerprint string, canonical []byte,
) (SourceMetadataGeneration, error) {
	parsed, err := packstore.ParseHash(sourceSHA256)
	if err != nil || parsed.String() != sourceSHA256 {
		return SourceMetadataGeneration{}, errors.New("source metadata SHA-256 identity is invalid")
	}
	fingerprint, fingerprintErr := packstore.ParseHash(extractorFingerprint)
	if fingerprintErr != nil || fingerprint.String() != extractorFingerprint {
		return SourceMetadataGeneration{}, errors.New("source metadata extractor fingerprint is invalid")
	}
	metadata, checksum, err := document.DecodeSourceMetadataV1(canonical)
	if err != nil {
		return SourceMetadataGeneration{}, fmt.Errorf("validating source metadata: %w", err)
	}
	generation := SourceMetadataGeneration{
		SourceSHA256: sourceSHA256, ContractVersion: metadata.ContractVersion,
		ExtractorFingerprint: extractorFingerprint, CanonicalJSON: append([]byte(nil), canonical...),
		Checksum: checksum, CreatedAt: nowRFC3339(),
	}
	generation.GenerationID = sourceMetadataGenerationID(sourceSHA256, metadata.ContractVersion,
		extractorFingerprint, checksum)
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO source_metadata_generations(
			generation_id,source_sha256,contract_version,extractor_fingerprint,canonical_json,checksum,created_at
		) VALUES(?,?,?,?,?,?,?) ON CONFLICT(source_sha256,contract_version,extractor_fingerprint) DO NOTHING`,
			generation.GenerationID, sourceSHA256, metadata.ContractVersion, extractorFingerprint,
			canonical, checksum, generation.CreatedAt); execErr != nil {
			return fmt.Errorf("recording source metadata: %w", execErr)
		}
		var stored SourceMetadataGeneration
		if scanErr := tx.QueryRowContext(ctx, `SELECT generation_id,source_sha256,contract_version,
			extractor_fingerprint,canonical_json,checksum,created_at FROM source_metadata_generations
			WHERE source_sha256=? AND contract_version=? AND extractor_fingerprint=?`,
			sourceSHA256, metadata.ContractVersion, extractorFingerprint).Scan(
			&stored.GenerationID, &stored.SourceSHA256, &stored.ContractVersion,
			&stored.ExtractorFingerprint, &stored.CanonicalJSON, &stored.Checksum, &stored.CreatedAt); scanErr != nil {
			return fmt.Errorf("reading source metadata generation: %w", scanErr)
		}
		if stored.GenerationID != generation.GenerationID || stored.Checksum != checksum ||
			!bytes.Equal(stored.CanonicalJSON, canonical) {
			return errors.New("source metadata extractor identity already has different evidence")
		}
		generation = stored
		_, execErr := tx.ExecContext(ctx, `INSERT INTO source_metadata_heads(source_sha256,generation_id,published_at)
			VALUES(?,?,?) ON CONFLICT(source_sha256) DO UPDATE SET
			generation_id=excluded.generation_id,published_at=excluded.published_at`,
			sourceSHA256, generation.GenerationID, nowRFC3339())
		return execErr
	})
	return generation, err
}

// ActiveSourceMetadata returns the selected generation for one retained blob.
func (s *Store) ActiveSourceMetadata(ctx context.Context, sourceSHA256 string) (SourceMetadataGeneration, document.SourceMetadataV1, error) {
	return activeSourceMetadata(ctx, s.db, sourceSHA256)
}

func activeSourceMetadata(
	ctx context.Context, q metadataQuerier, sourceSHA256 string,
) (SourceMetadataGeneration, document.SourceMetadataV1, error) {
	var generation SourceMetadataGeneration
	err := q.QueryRowContext(ctx, `SELECT g.generation_id,g.source_sha256,g.contract_version,
		g.extractor_fingerprint,g.canonical_json,g.checksum,g.created_at
		FROM source_metadata_heads h JOIN source_metadata_generations g ON g.generation_id=h.generation_id
		WHERE h.source_sha256=?`, sourceSHA256).Scan(&generation.GenerationID, &generation.SourceSHA256,
		&generation.ContractVersion, &generation.ExtractorFingerprint, &generation.CanonicalJSON,
		&generation.Checksum, &generation.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceMetadataGeneration{}, document.SourceMetadataV1{}, ErrNotFound
	}
	if err != nil {
		return SourceMetadataGeneration{}, document.SourceMetadataV1{}, fmt.Errorf("reading active source metadata: %w", err)
	}
	metadata, checksum, err := document.DecodeSourceMetadataV1(generation.CanonicalJSON)
	if err != nil || checksum != generation.Checksum {
		return SourceMetadataGeneration{}, document.SourceMetadataV1{}, errors.New("stored source metadata failed canonical checksum validation")
	}
	return generation, metadata, nil
}

// MissingSourceMetadataTargets returns a deterministic, resumable batch of
// retained originals not processed by the named extractor.
func (s *Store) MissingSourceMetadataTargets(ctx context.Context, fingerprint string, limit int) ([]SourceMetadataTarget, error) {
	return s.MissingSourceMetadataTargetsAfter(ctx, fingerprint, "", limit)
}

// MissingSourceMetadataTargetsAfter returns the next ordered page after one
// SHA-256 cursor so permanently failing originals cannot pin the first batch.
func (s *Store) MissingSourceMetadataTargetsAfter(
	ctx context.Context, fingerprint, afterSHA256 string, limit int,
) ([]SourceMetadataTarget, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("source metadata backfill limit must be between 1 and 1000")
	}
	if afterSHA256 != "" {
		if err := validateCatalogSHA256(afterSHA256, "source metadata backfill cursor"); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT b.hash,b.size FROM blobs b
		JOIN content_versions v ON v.blob_hash=b.hash
		LEFT JOIN source_metadata_generations g ON g.source_sha256=b.hash
			AND g.contract_version=? AND g.extractor_fingerprint=?
		WHERE b.hash>? AND g.generation_id IS NULL ORDER BY b.hash LIMIT ?`,
		document.SourceMetadataContractV1, fingerprint, afterSHA256, limit)
	if err != nil {
		return nil, fmt.Errorf("listing missing source metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var targets []SourceMetadataTarget
	for rows.Next() {
		var target SourceMetadataTarget
		if err := rows.Scan(&target.SourceSHA256, &target.Size); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// ContentVersionSourceMetadata joins active evidence with attachment facts for
// an authenticated content-version detail read.
func (s *Store) ContentVersionSourceMetadata(ctx context.Context, versionID string) (SourceMetadataView, error) {
	if err := validateUUIDv4(versionID); err != nil {
		return SourceMetadataView{}, fmt.Errorf("content version %q: %w", versionID, ErrNotFound)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SourceMetadataView{}, fmt.Errorf("starting source-metadata snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	version, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id = ?`, versionID))
	if err != nil {
		return SourceMetadataView{}, fmt.Errorf("content version %q: %w", versionID, err)
	}
	node, err := nodeByIDTx(tx, version.NodeID)
	if err != nil {
		return SourceMetadataView{}, err
	}
	view, err := sourceMetadataViewForVersion(ctx, tx, version, node)
	if err != nil {
		return SourceMetadataView{}, err
	}
	if err := tx.Commit(); err != nil {
		return SourceMetadataView{}, fmt.Errorf("closing source-metadata snapshot: %w", err)
	}
	return view, nil
}

func sourceMetadataViewForVersion(
	ctx context.Context, q metadataQuerier, version ContentVersion, node Node,
) (SourceMetadataView, error) {
	generation, metadata, err := activeSourceMetadata(ctx, q, version.BlobHash)
	if err != nil {
		return SourceMetadataView{}, err
	}
	facts := SourceMetadataAttachmentFacts{NodeID: node.ID, ContentVersionID: version.ID}
	if version.ID == node.CurrentVersionID && node.TrashedAt == nil {
		facts.Filename = node.Name
		facts.Extension = strings.ToLower(filepath.Ext(node.Name))
		facts.Path, err = pathOf(ctx, q, node.ID)
		if err != nil {
			return SourceMetadataView{}, err
		}
		err = q.QueryRowContext(ctx, `SELECT p.original_path,COALESCE(p.original_mtime,''),i.started_at
			FROM provenance p JOIN ingests i ON i.id=p.ingest_id WHERE p.node_id=?
			AND NOT EXISTS(SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
			ORDER BY i.started_at DESC,p.identity DESC LIMIT 1`, node.ID).Scan(
			&facts.SourcePath, &facts.FilesystemMTime, &facts.IngestedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return SourceMetadataView{}, fmt.Errorf("reading source-metadata provenance: %w", err)
		}
	}
	return SourceMetadataView{Generation: generation, Metadata: metadata, Attachment: facts}, nil
}

// NodeSourceMetadataViewByID returns one node detail from a single read snapshot.
func (s *Store) NodeSourceMetadataViewByID(ctx context.Context, id int64) (NodeSourceMetadataView, error) {
	return s.nodeSourceMetadataView(ctx, func(tx *sql.Tx) (Node, error) {
		return nodeByIDTx(tx, id)
	})
}

// NodeSourceMetadataViewByPath resolves a live path and returns one node detail
// from a single read snapshot.
func (s *Store) NodeSourceMetadataViewByPath(ctx context.Context, path string) (NodeSourceMetadataView, error) {
	return s.nodeSourceMetadataView(ctx, func(tx *sql.Tx) (Node, error) {
		return nodeByPath(ctx, tx, s.rootID, path)
	})
}

func (s *Store) nodeSourceMetadataView(
	ctx context.Context, resolve func(*sql.Tx) (Node, error),
) (NodeSourceMetadataView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return NodeSourceMetadataView{}, fmt.Errorf("starting node source-metadata snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	node, err := resolve(tx)
	if err != nil {
		return NodeSourceMetadataView{}, err
	}
	nodeView, err := nodeViewForNode(ctx, tx, node)
	if err != nil {
		return NodeSourceMetadataView{}, err
	}
	view := NodeSourceMetadataView{Node: nodeView.Node, Path: nodeView.Path}
	if !node.IsDir() && node.CurrentVersionID != "" {
		version, versionErr := scanContentVersion(tx.QueryRowContext(ctx,
			`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id = ? AND node_id = ?`,
			node.CurrentVersionID, node.ID))
		if versionErr != nil {
			return NodeSourceMetadataView{}, versionErr
		}
		metadata, metadataErr := sourceMetadataViewForVersion(ctx, tx, version, node)
		if metadataErr == nil {
			view.SourceMetadata = &metadata
		} else if !errors.Is(metadataErr, ErrNotFound) {
			return NodeSourceMetadataView{}, metadataErr
		}
	}
	if err := tx.Commit(); err != nil {
		return NodeSourceMetadataView{}, fmt.Errorf("closing node source-metadata snapshot: %w", err)
	}
	return view, nil
}
