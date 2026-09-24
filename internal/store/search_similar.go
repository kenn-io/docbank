package store

import (
	"bytes"
	"cmp"
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
	// InputIDs, when non-nil, limits a passage seed to its exact overlapping
	// rendition inputs. An empty selection is unavailable, never a document seed.
	InputIDs []string
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
		if source.InputIDs != nil && !slices.Contains(source.InputIDs, key.InputID) {
			continue
		}
		rows = append(rows, vectorindex.RowIdentity{SetID: key.VectorSetID, InputKey: key.InputID, InputChecksum: key.InputChecksum})
	}
	if len(rows) == 0 {
		return ErrSimilarSourceUnavailable
	}
	slices.SortFunc(rows, func(a, b vectorindex.RowIdentity) int {
		if a.SetID != b.SetID {
			return cmp.Compare(a.SetID, b.SetID)
		}
		if a.InputKey != b.InputKey {
			return cmp.Compare(a.InputKey, b.InputKey)
		}
		return cmp.Compare(a.InputChecksum, b.InputChecksum)
	})
	source.rows = rows
	return nil
}

// ConnectionGeneration identifies the active chunk generation backing one
// exact current document. Callers verify its blob before reading input spans.
type ConnectionGeneration struct {
	BlobHash, VectorSpaceID, EmbeddingSetID, InputGenerationID string
	AttachmentID, BuildID, SourceSHA256                        string
	NodeRevision                                               int64
}

func (s *Store) ActiveConnectionGeneration(ctx context.Context, nodeID int64, versionID, profile, binding string) (ConnectionGeneration, error) {
	var result ConnectionGeneration
	err := s.db.QueryRowContext(ctx, `SELECT eig.generation_blob_hash,es.vector_space_id,
		es.embedding_set_id,es.input_generation_id,eig.attachment_id,ra.build_id,cv.blob_hash,n.revision
		FROM `+nodeFrom+`
		JOIN embedding_heads eh ON eh.content_version_id=cv.version_id
		JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id
		JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
		JOIN rendition_attachments ra ON ra.attachment_id=eig.attachment_id
		JOIN rendition_heads rh ON rh.content_version_id=cv.version_id
			AND rh.profile_fingerprint=? AND rh.attachment_id=ra.attachment_id
		WHERE n.id=? AND n.kind='file' AND n.trashed_at IS NULL
			AND cv.version_id=? AND eh.profile_fingerprint=? AND eh.binding_id=?
			AND eh.input_kind='rendition_chunk' AND es.input_kind='rendition_chunk'
			AND es.content_version_id=cv.version_id AND es.profile_fingerprint=eh.profile_fingerprint
			AND es.binding_id=eh.binding_id AND es.vector_space_id=eh.vector_space_id`,
		profile, nodeID, versionID, profile, binding).Scan(&result.BlobHash, &result.VectorSpaceID,
		&result.EmbeddingSetID, &result.InputGenerationID, &result.AttachmentID, &result.BuildID,
		&result.SourceSHA256, &result.NodeRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionGeneration{}, ErrSimilarSourceUnavailable
	}
	return result, err
}

// SearchConnectionCandidates scores only the selected source chunk rows and
// reuses similar-search scope, stale-generation checks, and duplicate grouping.
func (s *Store) SearchConnectionCandidates(ctx context.Context, profile, binding string, source SimilarSource,
	limit int, opts SearchOptions,
) (_ SimilarSearchResolution, metric string, generationID string, retErr error) {
	if len(source.InputIDs) == 0 {
		return SimilarSearchResolution{}, "", "", ErrSimilarSourceUnavailable
	}
	authority, err := s.AcquireSimilarSearchAuthority(ctx, profile, binding, "connection-candidates", time.Now().UTC(), 5*time.Minute, opts, source)
	if err != nil {
		return SimilarSearchResolution{}, "", "", err
	}
	defer func() {
		releaseErr := s.ReleaseVectorIndexGeneration(context.WithoutCancel(ctx), authority.Lease.ID,
			authority.Lease.FencingToken, time.Now().UTC())
		retErr = errors.Join(retErr, releaseErr)
	}()
	if authority.InputKind != document.EmbeddingInputRenditionChunk || len(authority.SourceRows) == 0 {
		return SimilarSearchResolution{}, "", "", ErrSimilarSourceUnavailable
	}
	stored := authority.Lease.Generation
	index, err := vectorindex.OpenGeneration(bytes.NewReader(stored.Bytes), int64(len(stored.Bytes)))
	if err != nil {
		return SimilarSearchResolution{}, "", "", err
	}
	metadata, descriptor := index.Metadata(), authority.VectorSpace.Descriptor
	if metadata.VectorSpaceID != authority.VectorSpace.ID || metadata.Dimension != descriptor.Dimension ||
		metadata.Metric != descriptor.Metric || metadata.Normalization != descriptor.Normalization ||
		metadata.Manifest.Checksum != stored.IndexManifestChecksum || metadata.RowCount != stored.RowCount {
		return SimilarSearchResolution{}, "", "", ErrVectorIndexSourceStale
	}
	neighbors, err := index.SearchSimilarRows(ctx, authority.SourceRows, authority.ANNRows)
	if err != nil {
		return SimilarSearchResolution{}, "", "", err
	}
	if metadata.Metric == document.VectorMetricL2 {
		for i := range neighbors {
			neighbors[i].Score = -neighbors[i].Distance
		}
	}
	result, err := s.ResolveSimilarCandidates(ctx, profile, binding, authority.InputKind,
		authority.VectorSpace.ID, stored.SourceManifestChecksum, neighbors, limit, opts, source)
	return result, metadata.Metric, stored.ID, err
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

	DuplicateCount   int
	DuplicateMembers []SimilarSearchDuplicateMember
	SourceInputID    string
}

// SimilarSearchDuplicateMember preserves each scoped vector match suppressed
// by content grouping. It carries the same exact vector provenance as the
// representative; callers still verify its rendition before citing it.
type SimilarSearchDuplicateMember struct {
	SemanticSearchCandidate

	SourceInputID string
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
	collectMembers := len(opts.ContentVersionIDs) != 0
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
		byNode := make(map[int64]SimilarSearchCandidate)
		for _, neighbor := range neighbors {
			for _, member := range eligible[semanticEligibilityKey{VectorSetID: neighbor.SetID, InputID: neighbor.InputKey, InputChecksum: neighbor.InputChecksum}] {
				if member.NodeID == source.NodeID {
					continue
				}
				if _, seen := byNode[member.NodeID]; seen {
					continue
				}
				member.VaultID, member.VectorSpaceID, member.InputID, member.Score = s.vaultID, space, neighbor.InputKey, neighbor.Score
				byNode[member.NodeID] = SimilarSearchCandidate{SemanticSearchCandidate: member, SourceInputID: neighbor.SourceRow.InputKey}
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
					if member.Score > group.Score {
						if collectMembers {
							group.DuplicateMembers = append(group.DuplicateMembers, SimilarSearchDuplicateMember{
								SemanticSearchCandidate: group.SemanticSearchCandidate, SourceInputID: group.SourceInputID})
						}
						group.SemanticSearchCandidate, group.SourceInputID = member.SemanticSearchCandidate, member.SourceInputID
					} else if collectMembers {
						group.DuplicateMembers = append(group.DuplicateMembers, SimilarSearchDuplicateMember{
							SemanticSearchCandidate: member.SemanticSearchCandidate, SourceInputID: member.SourceInputID})
					}
					group.DuplicateCount++
				} else {
					selected := member
					groups[hash] = &selected
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
			for j := range result.Candidates[i].DuplicateMembers {
				member := &result.Candidates[i].DuplicateMembers[j]
				member.Path, err = pathOf(ctx, tx, member.NodeID)
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return SimilarSearchResolution{}, err
	}
	return result, nil
}
