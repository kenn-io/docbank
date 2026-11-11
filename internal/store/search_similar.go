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
	var rows []vectorindex.RowIdentity
	err := scanSemanticRows(ctx, tx, profile, binding, kind, space,
		" AND n.id=? AND cv.version_id=?", []any{source.NodeID, source.ContentVersionID}, nil, nil, false, false,
		func(key semanticEligibilityKey, _ int64, _ string) {
			rows = append(rows, vectorindex.RowIdentity{SetID: key.VectorSetID, InputKey: key.InputID, InputChecksum: key.InputChecksum})
		})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrSimilarSourceUnavailable
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
	err = s.withReadTx(ctx, func(tx *sql.Tx) error {
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

// ResolveSimilarCandidates groups current eligible copies from neighbors in
// descending score order, as returned by SearchSimilarRows.
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
	err = s.withReadTx(ctx, func(tx *sql.Tx) error {
		capture := similarSourceCapture{SimilarSource: source}
		if err := capture.load(ctx, tx, profile, binding, kind, space, opts); err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrSimilarSourceUnavailable) {
				return ErrProcessingSourceFenceStaleVersion
			}
			return err
		}
		current, err := s.vectorIndexSourceChecksumTx(ctx, tx, space)
		if errors.Is(err, ErrNotFound) {
			return ErrVectorIndexSourceStale
		}
		if err != nil {
			return err
		}
		if current != manifest {
			return ErrVectorIndexSourceStale
		}
		result.SourceManifestChecksum = manifest
		result.ScopedDocuments, result.CompleteDocuments, _, err = semanticSearchCoverageTx(ctx, tx, profile, binding, kind, space, opts)
		if err != nil {
			return err
		}
		return s.similarCandidatesTx(ctx, tx, profile, binding, kind, space, neighbors, limit, opts, source, &result)
	})
	if err != nil {
		return SimilarSearchResolution{}, err
	}
	return result, nil
}

func (s *Store) similarCandidatesTx(ctx context.Context, tx metadataQuerier, profile, binding string,
	kind document.EmbeddingInputKind, space string, neighbors []vectorindex.Neighbor, limit int,
	opts SearchOptions, source SimilarSource, result *SimilarSearchResolution,
) error {
	filter, args := searchFilterSQL(opts)
	byNode := make(map[int64]struct{})
	groups := make(map[string]*SimilarSearchCandidate)
	prefix, walked := min(limit+1, len(neighbors)), 0
	for {
		members, err := loadSemanticMemberships(ctx, tx, profile, binding, kind, space, filter, args,
			semanticNeighborKeys(neighbors[walked:prefix]))
		if err != nil {
			return err
		}
		for _, neighbor := range neighbors[walked:prefix] {
			for _, member := range members[semanticEligibilityKey{VectorSetID: neighbor.SetID, InputID: neighbor.InputKey, InputChecksum: neighbor.InputChecksum}] {
				if member.NodeID == source.NodeID {
					continue
				}
				if _, seen := byNode[member.NodeID]; seen {
					continue
				}
				member.VaultID, member.VectorSpaceID, member.InputID, member.Score = s.vaultID, space, neighbor.InputKey, neighbor.Score
				byNode[member.NodeID] = struct{}{}
				group := groups[member.BlobHash]
				if group == nil || member.Score > group.Score || (member.Score == group.Score &&
					compareDuplicateRepresentatives(member.modifiedAt, member.NodeID, group.modifiedAt, group.NodeID) < 0) {
					groups[member.BlobHash] = &SimilarSearchCandidate{SemanticSearchCandidate: member}
				}
			}
		}
		if prefix == len(neighbors) {
			break
		}
		strong := 0
		for _, group := range groups {
			if group.Score > neighbors[prefix].Score {
				strong++
			}
		}
		if strong >= limit && len(groups) > limit {
			break
		}
		walked, prefix = prefix, min(prefix*2, len(neighbors))
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
	hashes := make([]string, len(result.Candidates))
	for i, candidate := range result.Candidates {
		hashes[i] = candidate.BlobHash
	}
	copies := make(map[int64]string)
	err := scanSemanticRows(ctx, tx, profile, binding, kind, space, filter, args, hashes, nil, false, false,
		func(_ semanticEligibilityKey, nodeID int64, hash string) { copies[nodeID] = hash })
	if err != nil {
		return err
	}
	delete(copies, source.NodeID)
	counts := make(map[string]int)
	for _, hash := range copies {
		counts[hash]++
	}
	for i := range result.Candidates {
		result.Candidates[i].DuplicateCount = counts[result.Candidates[i].BlobHash] - 1
		result.Candidates[i].Path, err = pathOf(ctx, tx, result.Candidates[i].NodeID)
		if err != nil {
			return err
		}
	}
	return nil
}
