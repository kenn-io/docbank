package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"time"

	"go.kenn.io/kit/packstore"
)

var (
	ErrVectorIndexBuildInProgress = errors.New("vector index build is already in progress")
	ErrVectorIndexBuildFenced     = errors.New("vector index build is fenced")
	ErrVectorIndexLeaseFenced     = errors.New("vector index reader lease is fenced")
	ErrVectorIndexSourceStale     = errors.New("vector index source membership is stale")
	ErrVectorSetUnavailable       = errors.New("vector set payload is unavailable")
)

const vectorIndexSourceDomain = "docbank-vector-index-source/v1\x00"

type VectorIndexMember struct {
	EmbeddingSetID  string
	VectorSetID     string
	PayloadBlobHash string
	PayloadSize     int64
}

type VectorIndexSource struct {
	VectorSpaceID    string
	ManifestChecksum string
	Members          []VectorIndexMember
}

type VectorIndexBuildClaim struct {
	VectorSpaceID          string
	SourceManifestChecksum string
	Owner                  string
	FencingToken           int64
	ExpiresAt              time.Time
}

type VectorIndexGenerationRecord struct {
	ID                     string
	VectorSpaceID          string
	SourceManifestChecksum string
	IndexManifestChecksum  string
	Bytes                  []byte
	RowCount               int
	BuiltAt                string
}

type VectorIndexReaderLease struct {
	ID           string
	FencingToken int64
	ExpiresAt    time.Time
	Generation   VectorIndexGenerationRecord
}

type VectorIndexUnavailableSet struct {
	EmbeddingSetID  string
	VectorSetID     string
	PayloadBlobHash string
}

type VectorIndexUnavailableCoverage struct {
	VectorSpaceID               string
	SourceManifestChecksum      string
	Missing                     []VectorIndexUnavailableSet
	ExternalReembeddingRequired bool
}

func (s *Store) CaptureVectorIndexSource(ctx context.Context, vectorSpaceID string) (VectorIndexSource, error) {
	if err := validateCatalogSHA256(vectorSpaceID, "vector index vector-space ID"); err != nil {
		return VectorIndexSource{}, err
	}
	var source VectorIndexSource
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		source, err = captureVectorIndexSourceTx(ctx, tx, vectorSpaceID)
		return err
	})
	return source, err
}

func captureVectorIndexSourceTx(ctx context.Context, tx *sql.Tx, vectorSpaceID string) (_ VectorIndexSource, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT es.embedding_set_id,es.vector_set_id,
		vs.payload_blob_hash,vs.payload_size
		FROM embedding_heads eh
		JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id
		 AND es.content_version_id=eh.content_version_id
		 AND es.binding_id=eh.binding_id AND es.input_kind=eh.input_kind
		 AND es.vector_space_id=eh.vector_space_id
		 AND es.profile_fingerprint=eh.profile_fingerprint
		JOIN embedding_vector_sets vs ON vs.vector_set_id=es.vector_set_id
		 AND vs.vector_space_id=es.vector_space_id
		JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
		 AND eig.source_version_id=es.content_version_id
		 AND eig.profile_fingerprint=es.profile_fingerprint
		JOIN content_versions cv ON cv.version_id=es.content_version_id
		JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
		 AND n.trashed_at IS NULL
		WHERE eh.vector_space_id=?
		 AND (eig.attachment_id IS NULL OR EXISTS(
		   SELECT 1 FROM rendition_heads rh
		   WHERE rh.content_version_id=es.content_version_id
		     AND rh.profile_fingerprint=es.profile_fingerprint
		     AND rh.attachment_id=eig.attachment_id
		 ))
		ORDER BY es.embedding_set_id`, vectorSpaceID)
	if err != nil {
		return VectorIndexSource{}, fmt.Errorf("listing vector index logical membership: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()

	source := VectorIndexSource{VectorSpaceID: vectorSpaceID}
	for rows.Next() {
		var member VectorIndexMember
		if err := rows.Scan(&member.EmbeddingSetID, &member.VectorSetID,
			&member.PayloadBlobHash, &member.PayloadSize); err != nil {
			return VectorIndexSource{}, err
		}
		source.Members = append(source.Members, member)
	}
	if err := rows.Err(); err != nil {
		return VectorIndexSource{}, err
	}
	if len(source.Members) == 0 {
		return VectorIndexSource{}, ErrNotFound
	}
	source.ManifestChecksum = vectorIndexSourceChecksum(source.Members)
	return source, nil
}

func vectorIndexSourceChecksum(members []VectorIndexMember) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, vectorIndexSourceDomain)
	for _, member := range members {
		for _, value := range []string{member.EmbeddingSetID, member.VectorSetID,
			member.PayloadBlobHash, strconv.FormatInt(member.PayloadSize, 10)} {
			_, _ = io.WriteString(hash, strconv.Itoa(len(value)))
			_, _ = io.WriteString(hash, ":")
			_, _ = io.WriteString(hash, value)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (s *Store) ListVectorIndexSpaces(ctx context.Context) (_ []string, retErr error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT vector_space_id FROM embedding_heads ORDER BY vector_space_id`)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var spaces []string
	for rows.Next() {
		var space string
		if err := rows.Scan(&space); err != nil {
			return nil, err
		}
		spaces = append(spaces, space)
	}
	return spaces, rows.Err()
}

func (s *Store) ClaimVectorIndexBuild(ctx context.Context, vectorSpaceID, sourceChecksum,
	owner string, at time.Time, lease time.Duration,
) (VectorIndexBuildClaim, bool, error) {
	if err := validateCatalogSHA256(vectorSpaceID, "vector index vector-space ID"); err != nil {
		return VectorIndexBuildClaim{}, false, err
	}
	if err := validateCatalogSHA256(sourceChecksum, "vector index source manifest"); err != nil {
		return VectorIndexBuildClaim{}, false, err
	}
	if err := validateEmbeddingCatalogText(owner, "vector index build owner"); err != nil {
		return VectorIndexBuildClaim{}, false, err
	}
	if at.IsZero() || lease <= 0 {
		return VectorIndexBuildClaim{}, false, errors.New("vector index build lease is invalid")
	}
	at, expires := at.UTC(), at.UTC().Add(lease)
	claim := VectorIndexBuildClaim{VectorSpaceID: vectorSpaceID,
		SourceManifestChecksum: sourceChecksum, Owner: owner, ExpiresAt: expires}
	claimed := false
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var token int64
		var rawExpiry string
		err := tx.QueryRowContext(ctx, `SELECT fencing_token,lease_expires_at
			FROM vector_index_build_jobs WHERE vector_space_id=?`, vectorSpaceID).Scan(&token, &rawExpiry)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			claim.FencingToken = 1
			_, err = tx.ExecContext(ctx, `INSERT INTO vector_index_build_jobs(
				vector_space_id,source_manifest_checksum,owner,fencing_token,lease_expires_at)
				VALUES(?,?,?,?,?)`, vectorSpaceID, sourceChecksum, owner, claim.FencingToken,
				expires.Format(timestampLayout))
		case err != nil:
			return err
		default:
			priorExpiry, parseErr := time.Parse(timestampLayout, rawExpiry)
			if parseErr != nil {
				return errors.New("vector index build lease timestamp is corrupt")
			}
			if priorExpiry.After(at) {
				return ErrVectorIndexBuildInProgress
			}
			claim.FencingToken = token + 1
			_, err = tx.ExecContext(ctx, `UPDATE vector_index_build_jobs SET
				source_manifest_checksum=?,owner=?,fencing_token=?,lease_expires_at=?
				WHERE vector_space_id=? AND fencing_token=?`, sourceChecksum, owner,
				claim.FencingToken, expires.Format(timestampLayout), vectorSpaceID, token)
		}
		if err != nil {
			return err
		}
		claimed = true
		return nil
	})
	if errors.Is(err, ErrVectorIndexBuildInProgress) {
		return VectorIndexBuildClaim{}, false, nil
	}
	return claim, claimed, err
}

func requireVectorIndexBuildClaimTx(ctx context.Context, tx *sql.Tx, claim VectorIndexBuildClaim, at time.Time) error {
	var source, owner, rawExpiry string
	var token int64
	err := tx.QueryRowContext(ctx, `SELECT source_manifest_checksum,owner,fencing_token,lease_expires_at
		FROM vector_index_build_jobs WHERE vector_space_id=?`, claim.VectorSpaceID).Scan(
		&source, &owner, &token, &rawExpiry)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrVectorIndexBuildFenced
	}
	if err != nil {
		return err
	}
	expires, err := time.Parse(timestampLayout, rawExpiry)
	if err != nil || !expires.After(at.UTC()) || source != claim.SourceManifestChecksum ||
		owner != claim.Owner || token != claim.FencingToken {
		return ErrVectorIndexBuildFenced
	}
	return nil
}

func validateVectorIndexGenerationRecord(record VectorIndexGenerationRecord) error {
	for value, subject := range map[string]string{record.ID: "vector index generation ID",
		record.VectorSpaceID:          "vector index generation vector-space ID",
		record.SourceManifestChecksum: "vector index source manifest",
		record.IndexManifestChecksum:  "vector index embedded manifest"} {
		if err := validateCatalogSHA256(value, subject); err != nil {
			return err
		}
	}
	if len(record.Bytes) < 1 || len(record.Bytes) > 512<<20 || record.RowCount < 1 || record.RowCount > 1_000_000 {
		return errors.New("vector index generation shape exceeds bounds")
	}
	return validateMetadataTime("vector index built_at", record.BuiltAt)
}

func (s *Store) StageVectorIndexGeneration(ctx context.Context, claim VectorIndexBuildClaim,
	record VectorIndexGenerationRecord, at time.Time,
) error {
	if err := validateVectorIndexGenerationRecord(record); err != nil {
		return err
	}
	if record.VectorSpaceID != claim.VectorSpaceID || record.SourceManifestChecksum != claim.SourceManifestChecksum {
		return ErrVectorIndexBuildFenced
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := requireVectorIndexBuildClaimTx(ctx, tx, claim, at); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO vector_index_generations(
			generation_id,vector_space_id,source_manifest_checksum,index_manifest_checksum,
			generation_bytes,byte_size,row_count,built_at) VALUES(?,?,?,?,?,?,?,?)`, record.ID,
			record.VectorSpaceID, record.SourceManifestChecksum, record.IndexManifestChecksum,
			record.Bytes, len(record.Bytes), record.RowCount, record.BuiltAt)
		if err != nil {
			return err
		}
		inserted, _ := result.RowsAffected()
		if inserted != 0 {
			return nil
		}
		stored, err := loadVectorIndexGenerationTx(ctx, tx, record.ID)
		if err != nil {
			return err
		}
		if stored.ID != record.ID || stored.VectorSpaceID != record.VectorSpaceID ||
			stored.SourceManifestChecksum != record.SourceManifestChecksum ||
			stored.IndexManifestChecksum != record.IndexManifestChecksum || stored.RowCount != record.RowCount ||
			!bytes.Equal(stored.Bytes, record.Bytes) {
			return errors.New("vector index generation identity names different bytes")
		}
		return nil
	})
}

func (s *Store) PublishVectorIndexGeneration(ctx context.Context, claim VectorIndexBuildClaim,
	generationID string, at time.Time,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := requireVectorIndexBuildClaimTx(ctx, tx, claim, at); err != nil {
			return err
		}
		record, err := loadVectorIndexGenerationTx(ctx, tx, generationID)
		if err != nil {
			return err
		}
		if record.VectorSpaceID != claim.VectorSpaceID || record.SourceManifestChecksum != claim.SourceManifestChecksum {
			return ErrVectorIndexBuildFenced
		}
		current, err := captureVectorIndexSourceTx(ctx, tx, claim.VectorSpaceID)
		if errors.Is(err, ErrNotFound) || err == nil && current.ManifestChecksum != claim.SourceManifestChecksum {
			return ErrVectorIndexSourceStale
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vector_index_heads(
			vector_space_id,generation_id,source_manifest_checksum) VALUES(?,?,?)
			ON CONFLICT(vector_space_id) DO UPDATE SET generation_id=excluded.generation_id,
			source_manifest_checksum=excluded.source_manifest_checksum`, claim.VectorSpaceID,
			generationID, claim.SourceManifestChecksum); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM vector_index_build_jobs
			WHERE vector_space_id=? AND fencing_token=?`, claim.VectorSpaceID, claim.FencingToken)
		return err
	})
}

func loadVectorIndexGenerationTx(ctx context.Context, query metadataQuerier, generationID string) (VectorIndexGenerationRecord, error) {
	var record VectorIndexGenerationRecord
	var size int
	err := query.QueryRowContext(ctx, `SELECT generation_id,vector_space_id,source_manifest_checksum,
		index_manifest_checksum,generation_bytes,byte_size,row_count,built_at
		FROM vector_index_generations WHERE generation_id=?`, generationID).Scan(&record.ID,
		&record.VectorSpaceID, &record.SourceManifestChecksum, &record.IndexManifestChecksum,
		&record.Bytes, &size, &record.RowCount, &record.BuiltAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	if err != nil {
		return record, err
	}
	if size != len(record.Bytes) {
		return VectorIndexGenerationRecord{}, errors.New("vector index generation byte size is corrupt")
	}
	return record, validateVectorIndexGenerationRecord(record)
}

func (s *Store) ActiveVectorIndexGeneration(ctx context.Context, vectorSpaceID string) (VectorIndexGenerationRecord, error) {
	var generationID, source string
	err := s.db.QueryRowContext(ctx, `SELECT generation_id,source_manifest_checksum
		FROM vector_index_heads WHERE vector_space_id=?`, vectorSpaceID).Scan(&generationID, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return VectorIndexGenerationRecord{}, ErrNotFound
	}
	if err != nil {
		return VectorIndexGenerationRecord{}, err
	}
	record, err := loadVectorIndexGenerationTx(ctx, s.db, generationID)
	if err == nil && (record.VectorSpaceID != vectorSpaceID || record.SourceManifestChecksum != source) {
		return VectorIndexGenerationRecord{}, errors.New("vector index head does not match generation")
	}
	return record, err
}

func (s *Store) LoadVectorIndexGeneration(ctx context.Context, generationID string) (VectorIndexGenerationRecord, error) {
	return loadVectorIndexGenerationTx(ctx, s.db, generationID)
}

func (s *Store) ReadVectorIndexVectorSet(ctx context.Context, member VectorIndexMember) ([]byte, error) {
	if err := validateCatalogSHA256(member.VectorSetID, "vector index vector-set ID"); err != nil {
		return nil, err
	}
	if err := validateCatalogSHA256(member.PayloadBlobHash, "vector index payload blob hash"); err != nil {
		return nil, err
	}
	if member.PayloadSize < 1 || member.PayloadSize > 64<<20 {
		return nil, errors.New("vector index payload size exceeds bounds")
	}
	layout, err := packstore.NewLayout(
		filepath.Join(filepath.Dir(s.path), "blobs"),
		packstore.LayoutOptions{Staging: packstore.StagingStoreDirectory, StagingDir: "tmp"},
	)
	if err != nil {
		return nil, fmt.Errorf("opening vector index payload layout: %w", err)
	}
	backend, err := packstore.NewFilesystemBackend(layout, packstore.FilesystemBackendOptions{})
	if err != nil {
		return nil, fmt.Errorf("opening vector index payload reader: %w", err)
	}
	defer func() { _ = backend.Close() }()
	data, err := readCatalogEmbeddingArtifact(ctx, s, backend, member.PayloadBlobHash, member.PayloadSize)
	if errors.Is(err, errEmbeddingArtifactUnavailable) || errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: vector set %s blob %s", ErrVectorSetUnavailable,
			member.VectorSetID, member.PayloadBlobHash)
	}
	if err != nil {
		return nil, fmt.Errorf("reading vector set %s blob %s: %w",
			member.VectorSetID, member.PayloadBlobHash, err)
	}
	return data, nil
}

func (s *Store) AcquireVectorIndexGeneration(ctx context.Context, vectorSpaceID, owner string,
	at time.Time, duration time.Duration,
) (VectorIndexReaderLease, error) {
	if err := validateEmbeddingCatalogText(owner, "vector index reader owner"); err != nil {
		return VectorIndexReaderLease{}, err
	}
	if at.IsZero() || duration <= 0 {
		return VectorIndexReaderLease{}, errors.New("vector index reader lease is invalid")
	}
	leaseID, err := newUUIDv4()
	if err != nil {
		return VectorIndexReaderLease{}, err
	}
	lease := VectorIndexReaderLease{ID: leaseID, FencingToken: 1, ExpiresAt: at.UTC().Add(duration)}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var generationID string
		if err := tx.QueryRowContext(ctx, `SELECT generation_id FROM vector_index_heads
			WHERE vector_space_id=?`, vectorSpaceID).Scan(&generationID); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var err error
		lease.Generation, err = loadVectorIndexGenerationTx(ctx, tx, generationID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO vector_index_reader_leases(
			lease_id,generation_id,owner,fencing_token,lease_expires_at) VALUES(?,?,?,?,?)`,
			lease.ID, generationID, owner, lease.FencingToken, lease.ExpiresAt.Format(timestampLayout))
		return err
	})
	return lease, err
}

func (s *Store) ReleaseVectorIndexGeneration(ctx context.Context, leaseID string, fencingToken int64, at time.Time) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var token int64
		var rawExpiry string
		err := tx.QueryRowContext(ctx, `SELECT fencing_token,lease_expires_at
			FROM vector_index_reader_leases WHERE lease_id=?`, leaseID).Scan(&token, &rawExpiry)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrVectorIndexLeaseFenced
		}
		if err != nil {
			return err
		}
		expires, parseErr := time.Parse(timestampLayout, rawExpiry)
		if parseErr != nil || token != fencingToken || !expires.After(at.UTC()) {
			_, _ = tx.ExecContext(ctx, `DELETE FROM vector_index_reader_leases WHERE lease_id=?`, leaseID)
			return ErrVectorIndexLeaseFenced
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM vector_index_reader_leases
			WHERE lease_id=? AND fencing_token=?`, leaseID, fencingToken)
		return err
	})
}

func (s *Store) ReclaimVectorIndexGenerations(ctx context.Context, at time.Time) (int, error) {
	removed := 0
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM vector_index_reader_leases
			WHERE lease_expires_at<=?`, at.UTC().Format(timestampLayout)); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM vector_index_generations
			WHERE NOT EXISTS(SELECT 1 FROM vector_index_heads h WHERE h.generation_id=vector_index_generations.generation_id)
			  AND NOT EXISTS(SELECT 1 FROM vector_index_reader_leases l WHERE l.generation_id=vector_index_generations.generation_id)`)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		removed = int(count)
		return err
	})
	return removed, err
}

func (s *Store) ReplaceVectorIndexUnavailableCoverage(ctx context.Context, coverage []VectorIndexUnavailableCoverage) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM vector_index_unavailable_coverage`); err != nil {
			return err
		}
		for _, item := range coverage {
			if !item.ExternalReembeddingRequired {
				return errors.New("unavailable vector coverage must require external re-embedding")
			}
			for _, missing := range item.Missing {
				if _, err := tx.ExecContext(ctx, `INSERT INTO vector_index_unavailable_coverage(
					vector_space_id,source_manifest_checksum,embedding_set_id,vector_set_id,
					payload_blob_hash,external_reembedding_required) VALUES(?,?,?,?,?,1)`,
					item.VectorSpaceID, item.SourceManifestChecksum, missing.EmbeddingSetID,
					missing.VectorSetID, missing.PayloadBlobHash); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
