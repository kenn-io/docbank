package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"
)

// IndexProjectionKind identifies one rebuildable serving projection.
type IndexProjectionKind string

const (
	IndexProjectionLexical IndexProjectionKind = "lexical"
	IndexProjectionVector  IndexProjectionKind = "vector"
)

// IndexProjectionStateRecord is the durable operational state for one
// projection. It is rebuildable and intentionally excluded from metadata
// backup authority.
type IndexProjectionStateRecord struct {
	Kind                   IndexProjectionKind
	Key                    string
	SourceChecksum         string
	AuthorizationChecksum  string
	SourceWatermark        uint64
	AuthorizationWatermark uint64
	IndexWatermark         uint64
	GenerationID           string
	ExpectedCount          uint64
	IndexedCount           uint64
	UnavailableCount       uint64
	FailureReason          string
	UpdatedAt              string
}

// LocalIndexAuthorizationChecksum identifies rebuild work that uses only
// vault-local immutable authority and makes no provider call.
func LocalIndexAuthorizationChecksum() string {
	digest := sha256.Sum256([]byte("docbank/local-index-authorization/v1"))
	return hex.EncodeToString(digest[:])
}

// ObserveIndexProjection records an exact source and authorization view,
// advancing only the watermark whose checksum changed.
func (s *Store) ObserveIndexProjection(
	ctx context.Context, kind IndexProjectionKind, key, sourceChecksum, authorizationChecksum string,
) (IndexProjectionStateRecord, error) {
	var state IndexProjectionStateRecord
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		state, err = observeIndexProjectionTx(
			ctx, tx, kind, key, sourceChecksum, authorizationChecksum,
		)
		return err
	})
	return state, err
}

func observeIndexProjectionTx(
	ctx context.Context, tx *sql.Tx, kind IndexProjectionKind, key, sourceChecksum, authorizationChecksum string,
) (IndexProjectionStateRecord, error) {
	if err := validateIndexProjectionIdentity(kind, key, sourceChecksum, authorizationChecksum); err != nil {
		return IndexProjectionStateRecord{}, err
	}
	state, err := indexProjectionStateTx(ctx, tx, kind, key)
	if errors.Is(err, ErrNotFound) {
		now := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO index_projection_state(
				projection_kind,projection_key,source_checksum,authorization_checksum,
				source_watermark,authorization_watermark,updated_at
			) VALUES(?,?,?,?,1,1,?)`, kind, key, sourceChecksum, authorizationChecksum, now,
		); err != nil {
			return IndexProjectionStateRecord{}, fmt.Errorf("recording index projection observation: %w", err)
		}
		return indexProjectionStateTx(ctx, tx, kind, key)
	}
	if err != nil {
		return IndexProjectionStateRecord{}, err
	}
	sourceWatermark, authorizationWatermark := state.SourceWatermark, state.AuthorizationWatermark
	if state.SourceChecksum != sourceChecksum {
		if sourceWatermark == math.MaxInt64 {
			return IndexProjectionStateRecord{}, errors.New("index source watermark exhausted")
		}
		sourceWatermark++
	}
	if state.AuthorizationChecksum != authorizationChecksum {
		if authorizationWatermark == math.MaxInt64 {
			return IndexProjectionStateRecord{}, errors.New("index authorization watermark exhausted")
		}
		authorizationWatermark++
	}
	if state.SourceChecksum == sourceChecksum && state.AuthorizationChecksum == authorizationChecksum {
		return state, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE index_projection_state
		SET source_checksum=?,authorization_checksum=?,source_watermark=?,
		    authorization_watermark=?,updated_at=?
		WHERE projection_kind=? AND projection_key=?`,
		sourceChecksum, authorizationChecksum, sourceWatermark, authorizationWatermark,
		nowRFC3339(), kind, key,
	); err != nil {
		return IndexProjectionStateRecord{}, fmt.Errorf("advancing index projection observation: %w", err)
	}
	return indexProjectionStateTx(ctx, tx, kind, key)
}

// IndexProjectionState reads the last durable observation and publication.
func (s *Store) IndexProjectionState(
	ctx context.Context, kind IndexProjectionKind, key string,
) (IndexProjectionStateRecord, error) {
	var state IndexProjectionStateRecord
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		state, err = indexProjectionStateTx(ctx, tx, kind, key)
		return err
	})
	return state, err
}

func indexProjectionStateTx(
	ctx context.Context, q metadataQuerier, kind IndexProjectionKind, key string,
) (IndexProjectionStateRecord, error) {
	var (
		state                                                   IndexProjectionStateRecord
		sourceWatermark, authorizationWatermark, indexWatermark int64
		expectedCount, indexedCount, unavailableCount           int64
	)
	err := q.QueryRowContext(ctx, `
		SELECT source_checksum,authorization_checksum,source_watermark,
		       authorization_watermark,index_watermark,generation_id,
		       expected_count,indexed_count,unavailable_count,failure_reason,updated_at
		FROM index_projection_state WHERE projection_kind=? AND projection_key=?`, kind, key,
	).Scan(&state.SourceChecksum, &state.AuthorizationChecksum, &sourceWatermark,
		&authorizationWatermark, &indexWatermark, &state.GenerationID,
		&expectedCount, &indexedCount, &unavailableCount, &state.FailureReason, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return IndexProjectionStateRecord{}, ErrNotFound
	}
	if err != nil {
		return IndexProjectionStateRecord{}, fmt.Errorf("reading index projection state: %w", err)
	}
	if sourceWatermark <= 0 || authorizationWatermark <= 0 || indexWatermark < 0 ||
		expectedCount < 0 || indexedCount < 0 || unavailableCount < 0 ||
		indexedCount+unavailableCount > expectedCount {
		return IndexProjectionStateRecord{}, errors.New("index projection state is corrupt")
	}
	state.Kind, state.Key = kind, key
	state.SourceWatermark = uint64(sourceWatermark)
	state.AuthorizationWatermark = uint64(authorizationWatermark)
	state.IndexWatermark = uint64(indexWatermark)
	state.ExpectedCount = uint64(expectedCount)
	state.IndexedCount = uint64(indexedCount)
	state.UnavailableCount = uint64(unavailableCount)
	return state, nil
}

func validateIndexProjectionIdentity(
	kind IndexProjectionKind, key, sourceChecksum, authorizationChecksum string,
) error {
	switch kind {
	case IndexProjectionLexical, IndexProjectionVector:
	default:
		return fmt.Errorf("invalid index projection kind %q", kind)
	}
	if len(key) > 256 {
		return errors.New("index projection key is too long")
	}
	if err := validateCatalogSHA256(sourceChecksum, "index source checksum"); err != nil {
		return err
	}
	return validateCatalogSHA256(authorizationChecksum, "index authorization checksum")
}

// LexicalIndexSource is an exact immutable build manifest captured for a
// lexical rebuild. The rows remain private so callers cannot alter authority.
type LexicalIndexSource struct {
	SourceChecksum         string
	AuthorizationChecksum  string
	SourceWatermark        uint64
	AuthorizationWatermark uint64
	Documents              uint64
	Bytes                  uint64
	rows                   []lexicalManifestRow
	buildIDs               []string
}

// LexicalIndexCandidate names an unreachable, validated-on-commit generation.
type LexicalIndexCandidate struct {
	Source     LexicalIndexSource
	Generation LexicalGeneration
}

// LexicalIndexProjection is an atomic view of current lexical authority and
// its serving generation.
type LexicalIndexProjection struct {
	Source     LexicalIndexSource
	State      IndexProjectionStateRecord
	Generation LexicalGeneration
	Serving    bool
}

// VectorIndexProjection is an atomic catalog view for one vector space.
type VectorIndexProjection struct {
	Source      VectorIndexSource
	State       IndexProjectionStateRecord
	Generation  VectorIndexGenerationRecord
	Serving     bool
	Documents   uint64
	Bytes       uint64
	Unavailable uint64
}

// InspectVectorIndexProjection observes current vector membership and adopts
// a structurally valid matching head. The vector worker performs the deeper
// encoded-generation validation before the coordinator reports it queryable.
func (s *Store) InspectVectorIndexProjection(
	ctx context.Context, vectorSpaceID string,
) (VectorIndexProjection, error) {
	var projection VectorIndexProjection
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		source, err := captureVectorIndexSourceTx(ctx, tx, vectorSpaceID)
		if err != nil {
			return err
		}
		projection.Source = source
		projection.Documents = uint64(len(source.Members))
		for _, member := range source.Members {
			if member.PayloadSize < 0 || uint64(member.PayloadSize) > math.MaxUint64-projection.Bytes {
				return errors.New("vector index source byte count overflow")
			}
			projection.Bytes += uint64(member.PayloadSize)
		}
		projection.State, err = observeIndexProjectionTx(
			ctx, tx, IndexProjectionVector, vectorSpaceID,
			source.ManifestChecksum, LocalIndexAuthorizationChecksum(),
		)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM vector_index_unavailable_coverage
			WHERE vector_space_id=? AND source_manifest_checksum=?`,
			vectorSpaceID, source.ManifestChecksum,
		).Scan(&projection.Unavailable); err != nil {
			return fmt.Errorf("reading unavailable vector coverage: %w", err)
		}
		var generationID, headSource string
		err = tx.QueryRowContext(ctx, `SELECT generation_id,source_manifest_checksum
			FROM vector_index_heads WHERE vector_space_id=?`, vectorSpaceID,
		).Scan(&generationID, &headSource)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		projection.Generation, err = loadVectorIndexGenerationTx(ctx, tx, generationID)
		if err != nil {
			return err
		}
		projection.Serving = true
		if headSource != source.ManifestChecksum ||
			projection.Generation.SourceManifestChecksum != source.ManifestChecksum ||
			projection.Generation.VectorSpaceID != vectorSpaceID {
			return nil
		}
		if projection.State.GenerationID == generationID &&
			projection.State.IndexWatermark == projection.State.SourceWatermark &&
			projection.State.ExpectedCount == projection.Documents &&
			projection.State.IndexedCount == projection.Documents &&
			projection.State.UnavailableCount == projection.Unavailable {
			return nil
		}
		indexed := projection.Documents
		if projection.Unavailable > indexed {
			return errors.New("vector unavailable coverage exceeds source membership")
		}
		indexed -= projection.Unavailable
		if _, err := tx.ExecContext(ctx, `UPDATE index_projection_state SET
			index_watermark=source_watermark,generation_id=?,expected_count=?,indexed_count=?,
			unavailable_count=?,failure_reason='',updated_at=?
			WHERE projection_kind=? AND projection_key=?`,
			generationID, projection.Documents, indexed, projection.Unavailable, nowRFC3339(),
			IndexProjectionVector, vectorSpaceID,
		); err != nil {
			return fmt.Errorf("adopting verified vector projection state: %w", err)
		}
		projection.State, err = indexProjectionStateTx(
			ctx, tx, IndexProjectionVector, vectorSpaceID,
		)
		return err
	})
	return projection, err
}

// InspectLexicalIndexProjection validates the serving head against current
// immutable authority. A valid pre-watermark head is adopted without a
// rebuild, which makes daemon restart and first upgrade observation cheap.
func (s *Store) InspectLexicalIndexProjection(ctx context.Context) (LexicalIndexProjection, error) {
	var projection LexicalIndexProjection
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		source, err := captureLexicalIndexSourceTx(ctx, tx)
		if err != nil {
			return err
		}
		projection.Source = source
		projection.State, err = indexProjectionStateTx(ctx, tx, IndexProjectionLexical, "")
		if err != nil {
			return err
		}
		var generationID string
		err = tx.QueryRowContext(ctx,
			`SELECT generation_id FROM rendition_lexical_heads WHERE singleton=1`,
		).Scan(&generationID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading lexical index head: %w", err)
		}
		generation, err := loadAndValidateLexicalGenerationTx(ctx, tx, generationID)
		if err != nil {
			return err
		}
		expected := LexicalGeneration{
			SegmentCount: len(source.rows), ManifestDigest: lexicalManifestDigest(source.rows),
			BuildCount: len(source.buildIDs), BuildDigest: lexicalBuildDigest(source.buildIDs),
		}
		if generation.SegmentCount != expected.SegmentCount ||
			generation.ManifestDigest != expected.ManifestDigest ||
			generation.BuildCount != expected.BuildCount || generation.BuildDigest != expected.BuildDigest {
			return nil
		}
		projection.Generation, projection.Serving = generation, true
		if projection.State.GenerationID == generation.ID &&
			projection.State.IndexWatermark == source.SourceWatermark &&
			projection.State.ExpectedCount == source.Documents &&
			projection.State.IndexedCount == source.Documents && projection.State.UnavailableCount == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE index_projection_state
			SET index_watermark=source_watermark,generation_id=?,expected_count=?,indexed_count=?,
			    unavailable_count=0,failure_reason='',updated_at=?
			WHERE projection_kind=? AND projection_key=?`,
			generation.ID, source.Documents, source.Documents, nowRFC3339(),
			IndexProjectionLexical, "",
		); err != nil {
			return fmt.Errorf("adopting verified lexical projection state: %w", err)
		}
		projection.State, err = indexProjectionStateTx(ctx, tx, IndexProjectionLexical, "")
		return err
	})
	return projection, err
}

// CaptureLexicalIndexSource observes the exact immutable lexical catalog.
func (s *Store) CaptureLexicalIndexSource(ctx context.Context) (LexicalIndexSource, error) {
	var source LexicalIndexSource
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		source, err = captureLexicalIndexSourceTx(ctx, tx)
		return err
	})
	return source, err
}

func captureLexicalIndexSourceTx(ctx context.Context, tx *sql.Tx) (LexicalIndexSource, error) {
	rows, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
	if err != nil {
		return LexicalIndexSource{}, err
	}
	buildIDs, err := lexicalCatalogBuildIDsTx(ctx, tx)
	if err != nil {
		return LexicalIndexSource{}, err
	}
	checksum := lexicalReplacementGenerationID(rows, buildIDs)
	authorization := LocalIndexAuthorizationChecksum()
	state, err := observeIndexProjectionTx(
		ctx, tx, IndexProjectionLexical, "", checksum, authorization,
	)
	if err != nil {
		return LexicalIndexSource{}, err
	}
	var bytes uint64
	for _, row := range rows {
		if uint64(len(row.text)) > math.MaxUint64-bytes {
			return LexicalIndexSource{}, errors.New("lexical source byte count overflow")
		}
		bytes += uint64(len(row.text))
	}
	return LexicalIndexSource{
		SourceChecksum: checksum, AuthorizationChecksum: authorization,
		SourceWatermark: state.SourceWatermark, AuthorizationWatermark: state.AuthorizationWatermark,
		Documents: uint64(len(buildIDs)), Bytes: bytes,
		rows: rows, buildIDs: buildIDs,
	}, nil
}

// StageLexicalIndexGeneration builds an immutable generation without moving
// the serving head.
func (s *Store) StageLexicalIndexGeneration(
	ctx context.Context, source LexicalIndexSource,
) (LexicalIndexCandidate, error) {
	if source.SourceChecksum == "" || source.SourceChecksum != lexicalReplacementGenerationID(source.rows, source.buildIDs) {
		return LexicalIndexCandidate{}, errors.New("lexical index source manifest is invalid")
	}
	nonce, err := newUUIDv4()
	if err != nil {
		return LexicalIndexCandidate{}, err
	}
	digest := sha256.Sum256([]byte("docbank/lexical-index-repair-generation/v1\x00" + source.SourceChecksum + "\x00" + nonce))
	generationID := hex.EncodeToString(digest[:])
	var generation LexicalGeneration
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		generation, err = stageLexicalGenerationRowsTx(
			ctx, tx, generationID, source.rows, source.buildIDs,
		)
		return err
	})
	if err != nil {
		return LexicalIndexCandidate{}, err
	}
	return LexicalIndexCandidate{Source: source, Generation: generation}, nil
}

// ValidateLexicalIndexGeneration checks the complete stored candidate.
func (s *Store) ValidateLexicalIndexGeneration(
	ctx context.Context, candidate LexicalIndexCandidate,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, err := loadAndValidateLexicalGenerationTx(ctx, tx, candidate.Generation.ID)
		if err != nil {
			return err
		}
		if stored != candidate.Generation {
			return errors.New("lexical index candidate manifest does not match its source")
		}
		return nil
	})
}

// PublishLexicalIndexGeneration atomically fences source and authorization,
// retains the replaced generation for rollback, flips the serving head, and
// advances the durable index watermark.
func (s *Store) PublishLexicalIndexGeneration(
	ctx context.Context, candidate LexicalIndexCandidate, publishedAt time.Time, rollbackRetention time.Duration,
) error {
	if rollbackRetention <= 0 {
		return errors.New("lexical index rollback retention must be positive")
	}
	if publishedAt.IsZero() {
		return errors.New("lexical index publication time is required")
	}
	publishedAt = publishedAt.UTC()
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		current, err := captureLexicalIndexSourceTx(ctx, tx)
		if err != nil {
			return err
		}
		if current.SourceChecksum != candidate.Source.SourceChecksum ||
			current.SourceWatermark != candidate.Source.SourceWatermark ||
			current.AuthorizationChecksum != candidate.Source.AuthorizationChecksum ||
			current.AuthorizationWatermark != candidate.Source.AuthorizationWatermark {
			return ErrLexicalGenerationStale
		}
		stored, err := loadAndValidateLexicalGenerationTx(ctx, tx, candidate.Generation.ID)
		if err != nil {
			return err
		}
		if stored != candidate.Generation {
			return errors.New("lexical index candidate manifest does not match current source")
		}
		var previous string
		err = tx.QueryRowContext(ctx,
			`SELECT generation_id FROM rendition_lexical_heads WHERE singleton=1`,
		).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading previous lexical generation: %w", err)
		}
		if previous != "" && previous != stored.ID {
			rootID := "index_rollback_lexical_" + previous
			var fencingToken int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(fencing_token),0)+1
				FROM current_rendition_roots WHERE root_id=?`, rootID).Scan(&fencingToken); err != nil {
				return fmt.Errorf("allocating lexical rollback fence: %w", err)
			}
			if err := putCurrentRenditionRootTx(ctx, tx, CurrentRenditionRoot{
				ID: rootID, Kind: RenditionRootWorkerLease,
				TargetKind: RenditionRootLexicalGeneration, TargetID: previous,
				FencingToken: fencingToken,
				RecordedAt:   publishedAt.Format(time.RFC3339Nano),
				ExpiresAt:    publishedAt.Add(rollbackRetention).Format(time.RFC3339Nano),
			}); err != nil {
				return fmt.Errorf("retaining lexical rollback generation: %w", err)
			}
		}
		if err := s.publishLexicalHeadTx(ctx, tx, stored.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE index_projection_state
			SET index_watermark=source_watermark,generation_id=?,expected_count=?,indexed_count=?,
			    unavailable_count=0,failure_reason='',updated_at=?
			WHERE projection_kind=? AND projection_key=?`,
			stored.ID, current.Documents, current.Documents, publishedAt.Format(time.RFC3339Nano),
			IndexProjectionLexical, "",
		); err != nil {
			return fmt.Errorf("publishing lexical projection state: %w", err)
		}
		return nil
	})
}

// DiscardLexicalIndexGeneration removes only an unreachable candidate. Build
// rows remain reusable because they are immutable catalog projections.
func (s *Store) DiscardLexicalIndexGeneration(ctx context.Context, generationID string) error {
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rendition_lexical_generation_manifests
			WHERE generation_id=?
			  AND NOT EXISTS (SELECT 1 FROM rendition_lexical_heads WHERE generation_id=?)
			  AND NOT EXISTS (SELECT 1 FROM current_rendition_roots
			                  WHERE target_kind='lexical_generation' AND target_id=? AND active=1)`,
			generationID, generationID, generationID,
		); err != nil {
			return fmt.Errorf("discarding lexical index manifest: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rendition_lexical_generations
			WHERE generation_id=?
			  AND NOT EXISTS (SELECT 1 FROM rendition_lexical_heads WHERE generation_id=?)
			  AND NOT EXISTS (SELECT 1 FROM current_rendition_roots
			                  WHERE target_kind='lexical_generation' AND target_id=? AND active=1)`,
			generationID, generationID, generationID,
		); err != nil {
			return fmt.Errorf("discarding lexical index generation: %w", err)
		}
		return nil
	})
}
