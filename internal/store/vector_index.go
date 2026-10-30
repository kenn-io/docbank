package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
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
	ByteSize               int // Declared serialized size, populated on reads; writes derive it from Bytes.
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

type vectorIndexHeadCandidate struct {
	setID, profile, vectorSpace        string
	contentVersion, binding, inputKind sql.NullString
}

func captureVectorIndexSourceTx(ctx context.Context, tx *sql.Tx, vectorSpaceID string) (VectorIndexSource, error) {
	return captureVectorIndexSourceObservedTx(ctx, tx, vectorSpaceID, nil)
}

func captureVectorIndexSourceObservedTx(ctx context.Context, tx *sql.Tx, vectorSpaceID string, observe vectorIndexSourceObserver) (_ VectorIndexSource, retErr error) {
	var candidates []vectorIndexHeadCandidate
	var values *semanticSnapshotValueGuard
	var err error
	if observe != nil {
		values = &semanticSnapshotValueGuard{compatible: true}
		candidates, err = loadVectorIndexHeadCandidatesObservedTx(ctx, tx, vectorSpaceID, true, values)
	} else {
		candidates, err = loadVectorIndexHeadCandidates(ctx, tx, vectorSpaceID)
	}
	if err != nil {
		return VectorIndexSource{}, err
	}

	source := VectorIndexSource{VectorSpaceID: vectorSpaceID}
	// Only immutable descriptor decoding is reused within this transaction.
	// Each member's inputs, vector manifest and live eligibility are reloaded.
	spaces := make(map[string]EmbeddingVectorSpaceRecord)
	for _, candidate := range candidates {
		set, err := loadEmbeddingSetWithVectorSpacesObservedTx(ctx, tx, candidate.setID, spaces, values)
		if err != nil {
			return VectorIndexSource{}, fmt.Errorf("loading vector index member %s: %w", candidate.setID, err)
		}
		eligible, live, err := embeddingSetEligibilityTx(ctx, tx, set, observe != nil)
		if err != nil {
			return VectorIndexSource{}, err
		}
		if observe != nil {
			values.include(live.textCompatible)
			if err := observe(ctx, candidate, set, live, values.compatible); err != nil {
				return VectorIndexSource{}, err
			}
		}
		if !eligible || set.ProcessingProfileFingerprint != candidate.profile ||
			set.VectorSpace.ID != candidate.vectorSpace || candidate.vectorSpace != vectorSpaceID {
			continue
		}
		source.Members = append(source.Members, VectorIndexMember{
			EmbeddingSetID: set.ID, VectorSetID: set.VectorSet.ID,
			PayloadBlobHash: set.VectorSet.PayloadBlobHash, PayloadSize: set.VectorSet.PayloadSize,
		})
	}
	if len(source.Members) == 0 {
		return VectorIndexSource{}, ErrNotFound
	}
	source.ManifestChecksum = vectorIndexSourceChecksum(source.Members)
	return source, nil
}

func loadVectorIndexHeadCandidates(ctx context.Context, tx *sql.Tx, vectorSpaceID string) ([]vectorIndexHeadCandidate, error) {
	return loadVectorIndexHeadCandidatesWithKey(ctx, tx, vectorSpaceID, false)
}

func loadVectorIndexHeadCandidatesWithKey(ctx context.Context, tx *sql.Tx, vectorSpaceID string, withHeadKey bool) ([]vectorIndexHeadCandidate, error) {
	return loadVectorIndexHeadCandidatesObservedTx(ctx, tx, vectorSpaceID, withHeadKey, nil)
}

func loadVectorIndexHeadCandidatesObservedTx(ctx context.Context, tx *sql.Tx, vectorSpaceID string, withHeadKey bool, values *semanticSnapshotValueGuard) (_ []vectorIndexHeadCandidate, retErr error) {
	query := `SELECT embedding_set_id,profile_fingerprint,vector_space_id
		FROM embedding_heads WHERE vector_space_id=? ORDER BY embedding_set_id`
	if withHeadKey {
		query = `SELECT embedding_set_id,profile_fingerprint,vector_space_id,
 content_version_id,binding_id,input_kind FROM embedding_heads WHERE vector_space_id=? ORDER BY embedding_set_id`
	}
	const observedQuery = `SELECT embedding_set_id,profile_fingerprint,vector_space_id,
 content_version_id,binding_id,input_kind,
 CASE WHEN embedding_set_id=CAST(embedding_set_id AS TEXT)
 AND content_version_id=CAST(content_version_id AS TEXT)
 AND profile_fingerprint=CAST(profile_fingerprint AS TEXT)
 AND binding_id=CAST(binding_id AS TEXT)
 AND input_kind=CAST(input_kind AS TEXT)
 AND vector_space_id=CAST(vector_space_id AS TEXT) THEN 1 ELSE 0 END
 FROM embedding_heads WHERE vector_space_id=? ORDER BY embedding_set_id`
	if values != nil {
		query = observedQuery
	}
	rows, err := tx.QueryContext(ctx, query, vectorSpaceID)
	if err != nil {
		return nil, fmt.Errorf("listing vector index logical membership: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var candidates []vectorIndexHeadCandidate
	for rows.Next() {
		var candidate vectorIndexHeadCandidate
		var scanErr error
		if values != nil {
			var compatible bool
			scanErr = rows.Scan(&candidate.setID, &candidate.profile, &candidate.vectorSpace, &candidate.contentVersion, &candidate.binding, &candidate.inputKind, &compatible)
			values.include(compatible)
		} else if withHeadKey {
			scanErr = rows.Scan(&candidate.setID, &candidate.profile, &candidate.vectorSpace, &candidate.contentVersion, &candidate.binding, &candidate.inputKind)
		} else {
			scanErr = rows.Scan(&candidate.setID, &candidate.profile, &candidate.vectorSpace)
		}
		if scanErr != nil {
			return nil, scanErr
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func vectorIndexSourceChecksum(members []VectorIndexMember) string {
	hash := sha256.New()
	buffer := bufio.NewWriterSize(hash, 32<<10)
	var lengthScratch [20]byte
	var payloadScratch [20]byte
	writeLength := func(length int) {
		prefix := strconv.AppendInt(lengthScratch[:0], int64(length), 10)
		_, _ = buffer.Write(prefix)
		_ = buffer.WriteByte(':')
	}
	_, _ = buffer.WriteString(vectorIndexSourceDomain)
	for _, member := range members {
		for _, value := range [...]string{member.EmbeddingSetID, member.VectorSetID, member.PayloadBlobHash} {
			writeLength(len(value))
			_, _ = buffer.WriteString(value)
		}
		payloadSize := strconv.AppendInt(payloadScratch[:0], member.PayloadSize, 10)
		writeLength(len(payloadSize))
		_, _ = buffer.Write(payloadSize)
	}
	_ = buffer.Flush()
	return hex.EncodeToString(hash.Sum(nil))
}

func (s *Store) ListVectorIndexSpaces(ctx context.Context) (_ []string, retErr error) {
	rows, err := s.db.QueryContext(ctx, `SELECT vector_space_id FROM embedding_heads UNION SELECT vector_space_id FROM vector_index_heads ORDER BY vector_space_id`)
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

// RetireEmptyVectorIndexHead rechecks eligibility in the deletion transaction
// so a concurrent publication cannot lose a head for live membership.
func (s *Store) RetireEmptyVectorIndexHead(ctx context.Context, vectorSpaceID string, at time.Time) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := captureVectorIndexSourceTx(ctx, tx, vectorSpaceID)
		if err == nil {
			return ErrVectorIndexSourceStale
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM vector_index_heads WHERE vector_space_id=?`, vectorSpaceID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE vector_index_build_jobs SET lease_expires_at=? WHERE vector_space_id=?`, at.UTC().Format(timestampLayout), vectorSpaceID)
		return err
	})
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
		current, err := captureVectorIndexSourceTx(ctx, tx, vectorSpaceID)
		if errors.Is(err, ErrNotFound) || err == nil && current.ManifestChecksum != sourceChecksum {
			return ErrVectorIndexSourceStale
		}
		if err != nil {
			return err
		}
		var token int64
		var rawExpiry, priorSource string
		err = tx.QueryRowContext(ctx, `SELECT fencing_token,lease_expires_at,source_manifest_checksum
			FROM vector_index_build_jobs WHERE vector_space_id=?`, vectorSpaceID).Scan(&token, &rawExpiry, &priorSource)
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
			if priorExpiry.After(at) && priorSource == sourceChecksum {
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

// AbandonVectorIndexBuild expires only this attempt, preserving its fencing
// history so cleanup from an older attempt cannot affect a newer owner.
func (s *Store) AbandonVectorIndexBuild(ctx context.Context, claim VectorIndexBuildClaim, at time.Time) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE vector_index_build_jobs SET lease_expires_at=?
            WHERE vector_space_id=? AND source_manifest_checksum=? AND owner=? AND fencing_token=?`,
			at.UTC().Format(timestampLayout), claim.VectorSpaceID, claim.SourceManifestChecksum, claim.Owner, claim.FencingToken)
		return err
	})
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
	return validateVectorIndexGenerationMetadata(record, len(record.Bytes))
}

func validateVectorIndexGenerationMetadata(record VectorIndexGenerationRecord, byteSize int) error {
	for value, subject := range map[string]string{record.ID: "vector index generation ID",
		record.VectorSpaceID:          "vector index generation vector-space ID",
		record.SourceManifestChecksum: "vector index source manifest",
		record.IndexManifestChecksum:  "vector index embedded manifest"} {
		if err := validateCatalogSHA256(value, subject); err != nil {
			return err
		}
	}
	if byteSize < 1 || byteSize > 512<<20 || record.RowCount < 1 || record.RowCount > 1_000_000 {
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
		// Candidate bytes were validated before publication. Only read their
		// immutable source metadata while holding mutation admission.
		var space, source string
		err := tx.QueryRowContext(ctx, `SELECT vector_space_id,source_manifest_checksum
			FROM vector_index_generations WHERE generation_id=?`, generationID).Scan(&space, &source)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if space != claim.VectorSpaceID || source != claim.SourceManifestChecksum {
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
		_, err = tx.ExecContext(ctx, `UPDATE vector_index_build_jobs SET lease_expires_at=?
			WHERE vector_space_id=? AND fencing_token=?`, at.UTC().Format(timestampLayout), claim.VectorSpaceID, claim.FencingToken)
		return err
	})
}

func loadVectorIndexGenerationTx(ctx context.Context, query metadataQuerier, generationID string) (VectorIndexGenerationRecord, error) {
	return loadVectorIndexGenerationRecordTx(ctx, query, generationID, false)
}

// The metadata path deliberately does not select generation_bytes. Both paths
// share shape, identity and publication validation.
func loadVectorIndexGenerationRecordTx(ctx context.Context, query metadataQuerier, generationID string, metadataOnly bool) (VectorIndexGenerationRecord, error) {
	var record VectorIndexGenerationRecord
	columns := "generation_id,vector_space_id,source_manifest_checksum,index_manifest_checksum,"
	destinations := []any{&record.ID, &record.VectorSpaceID, &record.SourceManifestChecksum, &record.IndexManifestChecksum}
	if !metadataOnly {
		columns += "generation_bytes,"
		destinations = append(destinations, &record.Bytes)
	}
	columns += "byte_size,row_count,built_at"
	destinations = append(destinations, &record.ByteSize, &record.RowCount, &record.BuiltAt)
	err := query.QueryRowContext(ctx, "SELECT "+columns+" FROM vector_index_generations WHERE generation_id=?", generationID).Scan(destinations...)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	if err != nil {
		return record, err
	}
	if record.ID != generationID {
		return VectorIndexGenerationRecord{}, errors.New("vector index generation identity is corrupt")
	}
	if !metadataOnly && record.ByteSize != len(record.Bytes) {
		return VectorIndexGenerationRecord{}, errors.New("vector index generation byte size is corrupt")
	}
	return record, validateVectorIndexGenerationMetadata(record, record.ByteSize)
}

// VectorIndexHead is the publication metadata needed by an idle worker scan.
// Generation bytes are immutable and validated before this head is published.
type VectorIndexHead struct {
	GenerationID           string
	SourceManifestChecksum string
}

func (s *Store) ActiveVectorIndexHead(ctx context.Context, vectorSpaceID string) (VectorIndexHead, error) {
	var head VectorIndexHead
	err := s.db.QueryRowContext(ctx, `SELECT generation_id,source_manifest_checksum
		FROM vector_index_heads WHERE vector_space_id=?`, vectorSpaceID).Scan(&head.GenerationID, &head.SourceManifestChecksum)
	if errors.Is(err, sql.ErrNoRows) {
		return head, ErrNotFound
	}
	return head, err
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

func (s *Store) ReadVectorIndexVectorSet(ctx context.Context, reader RenditionBlobReader, member VectorIndexMember) ([]byte, error) {
	if err := validateCatalogSHA256(member.VectorSetID, "vector index vector-set ID"); err != nil {
		return nil, err
	}
	if err := validateCatalogSHA256(member.PayloadBlobHash, "vector index payload blob hash"); err != nil {
		return nil, err
	}
	if member.PayloadSize < 1 || member.PayloadSize > 64<<20 {
		return nil, errors.New("vector index payload size exceeds bounds")
	}
	data, err := readEmbeddingArtifact(ctx, reader, importedProcessingBlob{hash: member.PayloadBlobHash, size: member.PayloadSize})
	if isMissingVectorPayload(err) {
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
	return s.acquireVectorIndexGeneration(ctx, vectorSpaceID, owner, at, duration, false)
}

// AcquireVectorIndexGenerationHead leases the current generation and validates
// its metadata and live source membership without reading its serialized bytes.
func (s *Store) AcquireVectorIndexGenerationHead(ctx context.Context, vectorSpaceID, owner string,
	at time.Time, duration time.Duration,
) (VectorIndexReaderLease, error) {
	return s.acquireVectorIndexGeneration(ctx, vectorSpaceID, owner, at, duration, true)
}

func (s *Store) acquireVectorIndexGeneration(ctx context.Context, vectorSpaceID, owner string,
	at time.Time, duration time.Duration, metadataOnly bool,
) (VectorIndexReaderLease, error) {
	lease, err := newVectorIndexReaderLease(vectorSpaceID, owner, at, duration)
	if err != nil {
		return VectorIndexReaderLease{}, err
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return s.acquireVectorIndexGenerationTx(ctx, tx, vectorSpaceID, owner, &lease, metadataOnly, nil)
	})
	return lease, err
}

func newVectorIndexReaderLease(vectorSpaceID, owner string, at time.Time, duration time.Duration) (VectorIndexReaderLease, error) {
	if err := validateCatalogSHA256(vectorSpaceID, "vector index vector-space ID"); err != nil {
		return VectorIndexReaderLease{}, err
	}
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
	return VectorIndexReaderLease{ID: leaseID, FencingToken: 1, ExpiresAt: at.UTC().Add(duration)}, nil
}

// acquireVectorIndexGenerationTx always validates the active generation against
// a complete fresh source before inserting its lease. An optional collector is
// local to this transaction and may supply semantic counts and ANN identities.
func (s *Store) acquireVectorIndexGenerationTx(ctx context.Context, tx *sql.Tx, vectorSpaceID, owner string,
	lease *VectorIndexReaderLease, metadataOnly bool, collector *originalSemanticSnapshot,
) error {
	var generationID, headSource string
	if err := tx.QueryRowContext(ctx, `SELECT generation_id,source_manifest_checksum FROM vector_index_heads
		WHERE vector_space_id=?`, vectorSpaceID).Scan(&generationID, &headSource); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var err error
	lease.Generation, err = loadVectorIndexGenerationRecordTx(ctx, tx, generationID, metadataOnly)
	if err != nil {
		return err
	}
	if lease.Generation.VectorSpaceID != vectorSpaceID {
		return errors.New("vector index head does not match generation")
	}
	if lease.Generation.SourceManifestChecksum != headSource {
		return ErrVectorIndexSourceStale
	}
	var current VectorIndexSource
	if metadataOnly {
		current, err = s.captureSemanticSearchSourceTx(ctx, tx, vectorSpaceID, collector)
	} else {
		current, err = captureVectorIndexSourceTx(ctx, tx, vectorSpaceID)
	}
	if err != nil {
		return err
	}
	if current.ManifestChecksum != lease.Generation.SourceManifestChecksum {
		return ErrVectorIndexSourceStale
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO vector_index_reader_leases(
		lease_id,generation_id,owner,fencing_token,lease_expires_at) VALUES(?,?,?,?,?)`,
		lease.ID, generationID, owner, lease.FencingToken, lease.ExpiresAt.Format(timestampLayout))
	return err
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
			  AND NOT EXISTS(SELECT 1 FROM vector_index_reader_leases l WHERE l.generation_id=vector_index_generations.generation_id)
			  AND NOT EXISTS(SELECT 1 FROM vector_index_build_jobs j
			      WHERE j.vector_space_id=vector_index_generations.vector_space_id
			        AND j.source_manifest_checksum=vector_index_generations.source_manifest_checksum
			        AND j.lease_expires_at>?)`, at.UTC().Format(timestampLayout))
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
