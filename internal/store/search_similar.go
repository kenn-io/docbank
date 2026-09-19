package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

var ErrSimilarSourceUnavailable = errors.New("similar-document source has no current embedding")

type SimilarSource struct {
	NodeID           int64
	ContentVersionID string
}

type SimilarSearchAuthority struct {
	SemanticSearchAuthority

	SourceRows []vectorindex.RowIdentity
}

type similarSourceCapture struct {
	SimilarSource

	rows []vectorindex.RowIdentity
}

func validateSimilarSource(ctx context.Context, tx metadataQuerier, source SimilarSource, opts SearchOptions) error {
	if !slices.Contains(opts.ContentVersionIDs, source.ContentVersionID) {
		return ErrInvalidProcessingSourceFence
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT n.id FROM `+nodeFrom+`
		WHERE n.id=? AND n.kind='file' AND n.trashed_at IS NULL AND cv.version_id=?`,
		source.NodeID, source.ContentVersionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (source *similarSourceCapture) load(ctx context.Context, tx metadataQuerier, profile, binding string,
	kind document.EmbeddingInputKind, space string, opts SearchOptions,
) error {
	if err := validateSimilarSource(ctx, tx, source.SimilarSource, opts); err != nil {
		return err
	}
	eligible, err := loadSemanticEligibility(ctx, tx, profile, binding, kind, space,
		" AND n.id=? AND cv.version_id=?", []any{source.NodeID, source.ContentVersionID})
	if err != nil {
		return err
	}
	if len(eligible) == 0 {
		return ErrSimilarSourceUnavailable
	}
	rows := make([]vectorindex.RowIdentity, 0, len(eligible))
	for key := range eligible {
		rows = append(rows, vectorindex.RowIdentity{SetID: key.VectorSetID, InputKey: key.InputID, InputChecksum: key.InputChecksum})
	}
	source.rows = rows
	return nil
}

func (s *Store) AcquireSimilarSearchAuthority(ctx context.Context, profile, binding, owner string,
	at time.Time, duration time.Duration, opts SearchOptions, source SimilarSource,
) (SimilarSearchAuthority, error) {
	opts, err := s.NormalizeSearchOptions(ctx, opts)
	if err != nil {
		return SimilarSearchAuthority{}, err
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := validateSimilarSource(ctx, tx, source, opts); err != nil {
			return err
		}
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_heads WHERE content_version_id=? AND profile_fingerprint=? AND binding_id=?)`,
			source.ContentVersionID, profile, binding).Scan(&present); err != nil {
			return err
		}
		if !present {
			return ErrSimilarSourceUnavailable
		}
		return nil
	})
	if err != nil {
		return SimilarSearchAuthority{}, err
	}
	capture := similarSourceCapture{SimilarSource: source}
	authority, err := s.acquireSemanticSearchAuthority(ctx, profile, binding, owner, at, duration, opts, &capture)
	if err != nil {
		return SimilarSearchAuthority{}, err
	}
	return SimilarSearchAuthority{SemanticSearchAuthority: authority, SourceRows: capture.rows}, nil
}

type SimilarSearchCandidate struct {
	SemanticSearchCandidate

	DuplicateCount int
}

type SimilarSearchResolution struct {
	SourceManifestChecksum             string
	Candidates                         []SimilarSearchCandidate
	Truncated                          bool
	ScopedDocuments, CompleteDocuments int
}

func (s *Store) ResolveSimilarCandidates(ctx context.Context, profile, binding string,
	kind document.EmbeddingInputKind, space, manifest string, neighbors []vectorindex.Neighbor,
	limit int, opts SearchOptions, source SimilarSource,
) (SimilarSearchResolution, error) {
	if err := validateCatalogSHA256(space, "similar search vector space"); err != nil {
		return SimilarSearchResolution{}, err
	}
	if err := validateCatalogSHA256(manifest, "similar search manifest"); err != nil {
		return SimilarSearchResolution{}, err
	}
	if limit < 1 || limit > document.MaxRetrievalCandidateLimit {
		return SimilarSearchResolution{}, errors.New("invalid similar search limit")
	}
	opts, err := s.NormalizeSearchOptions(ctx, opts)
	if err != nil {
		return SimilarSearchResolution{}, err
	}
	var result SimilarSearchResolution
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		capture := similarSourceCapture{SimilarSource: source}
		if err := capture.load(ctx, tx, profile, binding, kind, space, opts); err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrSimilarSourceUnavailable) {
				return ErrProcessingSourceFenceStaleVersion
			}
			return err
		}
		current, err := captureVectorIndexSourceTx(ctx, tx, space)
		if errors.Is(err, ErrNotFound) {
			return ErrVectorIndexSourceStale
		}
		if err != nil {
			return err
		}
		if current.ManifestChecksum != manifest {
			return ErrVectorIndexSourceStale
		}
		result.SourceManifestChecksum = manifest
		result.ScopedDocuments, result.CompleteDocuments, err = semanticSearchCoverageTx(ctx, tx, profile, binding, kind, space, opts)
		if err != nil {
			return err
		}
		filter, args := searchFilterSQL(opts)
		eligible, err := loadSemanticMemberships(ctx, tx, profile, binding, kind, space, filter, args)
		if err != nil {
			return err
		}
		byNode := make(map[int64]SemanticSearchCandidate)
		for _, neighbor := range neighbors {
			for _, member := range eligible[semanticEligibilityKey{VectorSetID: neighbor.SetID, InputID: neighbor.InputKey, InputChecksum: neighbor.InputChecksum}] {
				if member.NodeID == source.NodeID {
					continue
				}
				if _, seen := byNode[member.NodeID]; seen {
					continue
				}
				member.VaultID, member.VectorSpaceID, member.InputID, member.Score = s.vaultID, space, neighbor.InputKey, neighbor.Score
				byNode[member.NodeID] = member
			}
		}
		rows, err := tx.QueryContext(ctx, `WITH `+CurrentContentMembershipCTE+`
			SELECT node_id,blob_hash FROM (SELECT m.* FROM `+nodeFrom+` JOIN current_content_members m ON n.id=m.node_id
			WHERE n.id<>? `+filter+`) ORDER BY blob_hash, `+DuplicateRepresentativeOrder,
			append([]any{source.NodeID}, args...)...)
		if err != nil {
			return err
		}
		groups := make(map[string]*SimilarSearchCandidate)
		err = func() (retErr error) {
			defer func() { retErr = errors.Join(retErr, rows.Close()) }()
			for rows.Next() {
				var nodeID int64
				var hash string
				if err := rows.Scan(&nodeID, &hash); err != nil {
					return err
				}
				member, ok := byNode[nodeID]
				if !ok {
					continue
				}
				if group := groups[hash]; group != nil {
					group.DuplicateCount++
					if member.Score > group.Score || member.Score == group.Score && member.NodeID < group.NodeID {
						duplicateCount := group.DuplicateCount
						*group = SimilarSearchCandidate{SemanticSearchCandidate: member, DuplicateCount: duplicateCount}
					}
				} else {
					groups[hash] = &SimilarSearchCandidate{SemanticSearchCandidate: member}
				}
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		for _, group := range groups {
			result.Candidates = append(result.Candidates, *group)
		}
		slices.SortFunc(result.Candidates, func(a, b SimilarSearchCandidate) int {
			if a.Score > b.Score {
				return -1
			}
			if a.Score < b.Score {
				return 1
			}
			if a.NodeID < b.NodeID {
				return -1
			}
			if a.NodeID > b.NodeID {
				return 1
			}
			return 0
		})
		result.Truncated = len(result.Candidates) > limit
		result.Candidates = result.Candidates[:min(limit, len(result.Candidates))]
		for i := range result.Candidates {
			result.Candidates[i].Path, err = pathOf(ctx, tx, result.Candidates[i].NodeID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return SimilarSearchResolution{}, err
	}
	return result, nil
}
