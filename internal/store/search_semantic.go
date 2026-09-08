package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// SemanticSearchCandidate is one vector neighbor reduced to a current,
// scope-eligible document. Excerpt is intentionally empty: only an independent
// lexical lane may supply text to an explained retrieval report.
type SemanticSearchCandidate struct {
	VaultID           string
	NodeID            int64
	NodeRevision      int64
	ContentVersionID  string
	Path              string
	VectorSpaceID     string
	EmbeddingSetID    string
	InputGenerationID string
	InputID           string
	InputKind         document.EmbeddingInputKind
	Score             float64
	Distance          float64
	Excerpt           string
}

// SemanticSearchResolution binds ranked candidates and coverage to the same
// current-head snapshot after query embedding and vector search complete.
type SemanticSearchResolution struct {
	SourceManifestChecksum string
	Candidates             []SemanticSearchCandidate
	Truncated              bool
	ScopedDocuments        int
	CompleteDocuments      int
}

// SemanticSearchAuthority pins the exact persisted vector-space descriptor and
// one active local index generation for a query. Required and Complete count
// current documents after applying the operator scope.
type SemanticSearchAuthority struct {
	VectorSpace       EmbeddingVectorSpaceRecord
	Lease             VectorIndexReaderLease
	InputKind         document.EmbeddingInputKind
	BindingRequired   bool
	ScopedDocuments   int
	CompleteDocuments int
}

// AcquireSemanticSearchAuthority resolves the query contract from durable E1
// authority, never from a runtime default, and leases one exact active index.
func (s *Store) AcquireSemanticSearchAuthority(ctx context.Context, profileFingerprint,
	bindingID, owner string, at time.Time, duration time.Duration, opts SearchOptions,
) (SemanticSearchAuthority, error) {
	normalized, err := s.normalizeSearchOptions(ctx, opts)
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	binding, fingerprints, err := embeddingProfileBindingAuthority(ctx, s.db, profileFingerprint, bindingID)
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	vectorSpaceID := fingerprints.VectorSpace[bindingID]
	var space EmbeddingVectorSpaceRecord
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var loadErr error
		space, loadErr = loadVectorSpaceTx(ctx, tx, vectorSpaceID)
		return loadErr
	})
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	if space.ID != vectorSpaceID || space.DescriptorFingerprint != binding.Descriptor.Fingerprint ||
		space.CompatibilityID != binding.CompatibilityID || space.Dimensions != binding.Dimensions ||
		space.Metric != binding.Metric || space.Normalization != binding.Normalization ||
		space.ScalarEncoding != binding.ScalarEncoding || space.DocumentFormatter != binding.DocumentFormatter ||
		space.QueryFormatter != binding.QueryFormatter || space.ModelInputFingerprint != binding.ModelInput.Fingerprint {
		return SemanticSearchAuthority{}, errors.New("semantic search vector space does not match profile binding authority")
	}
	lease, err := s.AcquireVectorIndexGeneration(ctx, vectorSpaceID, owner, at, duration)
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	source, required, complete, err := s.semanticSearchAuthorityFence(ctx, profileFingerprint,
		bindingID, binding.InputKind, vectorSpaceID, normalized)
	if err != nil {
		releaseErr := s.ReleaseVectorIndexGeneration(context.WithoutCancel(ctx),
			lease.ID, lease.FencingToken, at)
		return SemanticSearchAuthority{}, errors.Join(err, releaseErr)
	}
	if source.ManifestChecksum != lease.Generation.SourceManifestChecksum {
		releaseErr := s.ReleaseVectorIndexGeneration(context.WithoutCancel(ctx),
			lease.ID, lease.FencingToken, at)
		return SemanticSearchAuthority{}, errors.Join(ErrVectorIndexSourceStale, releaseErr)
	}
	return SemanticSearchAuthority{VectorSpace: space, Lease: lease, InputKind: binding.InputKind,
		BindingRequired: binding.Activation == document.EmbeddingRequired,
		ScopedDocuments: required, CompleteDocuments: complete}, nil
}

func (s *Store) semanticSearchAuthorityFence(ctx context.Context, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID string, opts SearchOptions,
) (source VectorIndexSource, required, complete int, retErr error) {
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		source, err = captureVectorIndexSourceTx(ctx, tx, vectorSpaceID)
		if err != nil {
			return err
		}
		required, complete, err = semanticSearchCoverageTx(ctx, tx, profileFingerprint,
			bindingID, inputKind, vectorSpaceID, opts)
		return err
	})
	return source, required, complete, err
}

func semanticSearchCoverageTx(ctx context.Context, tx metadataQuerier, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID string, opts SearchOptions,
) (required, complete int, retErr error) {
	filterSQL, filterArgs := searchFilterSQL(opts)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+nodeFrom+`
		WHERE n.kind='file' AND n.trashed_at IS NULL AND cv.version_id IS NOT NULL `+filterSQL,
		filterArgs...).Scan(&required); err != nil {
		return 0, 0, err
	}
	args := []any{profileFingerprint, bindingID, inputKind, vectorSpaceID}
	args = append(args, filterArgs...)
	err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT n.id) FROM `+nodeFrom+`
		JOIN embedding_heads eh ON eh.content_version_id=cv.version_id
		JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id
		 AND es.content_version_id=eh.content_version_id AND es.binding_id=eh.binding_id
		 AND es.input_kind=eh.input_kind AND es.vector_space_id=eh.vector_space_id
		 AND es.profile_fingerprint=eh.profile_fingerprint
		JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
		WHERE n.kind='file' AND n.trashed_at IS NULL
		  AND eh.profile_fingerprint=? AND eh.binding_id=? AND eh.input_kind=?
		  AND eh.vector_space_id=?
		  AND (eh.input_kind='original_file' OR EXISTS(
		    SELECT 1 FROM rendition_heads rh
		    WHERE rh.content_version_id=eh.content_version_id
		      AND rh.profile_fingerprint=eh.profile_fingerprint
		      AND rh.attachment_id=eig.attachment_id
		  )) `+filterSQL, args...).Scan(&complete)
	return required, complete, err
}

// ResolveSemanticCandidates applies current-version, live-head, and operator
// scope fencing before reducing ordered vector rows to bounded documents.
func (s *Store) ResolveSemanticCandidates(ctx context.Context, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID, expectedSourceManifest string,
	neighbors []vectorindex.Neighbor, limit int, opts SearchOptions,
) (_ SemanticSearchResolution, retErr error) {
	if err := validateCatalogSHA256(vectorSpaceID, "semantic search vector-space ID"); err != nil {
		return SemanticSearchResolution{}, err
	}
	if err := validateCatalogSHA256(expectedSourceManifest, "semantic search source manifest"); err != nil {
		return SemanticSearchResolution{}, err
	}
	if limit < 1 || limit > document.MaxRetrievalCandidateLimit {
		return SemanticSearchResolution{}, fmt.Errorf("semantic search limit must be between 1 and %d", document.MaxRetrievalCandidateLimit)
	}
	var result SemanticSearchResolution
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		normalized, normalizeErr := s.normalizeSearchOptionsWithQuerier(ctx, tx, opts)
		if normalizeErr != nil {
			return normalizeErr
		}
		current, captureErr := captureVectorIndexSourceTx(ctx, tx, vectorSpaceID)
		if captureErr != nil {
			if errors.Is(captureErr, ErrNotFound) {
				return ErrVectorIndexSourceStale
			}
			return captureErr
		}
		if current.ManifestChecksum != expectedSourceManifest {
			return ErrVectorIndexSourceStale
		}
		result.SourceManifestChecksum = current.ManifestChecksum
		var err error
		result.ScopedDocuments, result.CompleteDocuments, err = semanticSearchCoverageTx(ctx, tx,
			profileFingerprint, bindingID, inputKind, vectorSpaceID, normalized)
		if err != nil {
			return err
		}
		filterSQL, filterArgs := searchFilterSQL(normalized)
		eligible, loadErr := loadSemanticEligibility(ctx, tx, profileFingerprint, bindingID,
			inputKind, vectorSpaceID, filterSQL, filterArgs)
		if loadErr != nil {
			return loadErr
		}
		result.Candidates, result.Truncated = reduceSemanticCandidates(s.vaultID, vectorSpaceID,
			neighbors, limit, eligible)
		return nil
	})
	if err != nil {
		return SemanticSearchResolution{}, err
	}
	return result, nil
}

type semanticEligibilityKey struct {
	VectorSetID   string
	InputID       string
	InputChecksum string
}

type semanticEligibility struct {
	Node      Node
	Path      string
	Candidate SemanticSearchCandidate
}

func loadSemanticEligibility(ctx context.Context, tx metadataQuerier, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID, filterSQL string, filterArgs []any,
) (map[semanticEligibilityKey][]semanticEligibility, error) {
	bounds, err := vectorindex.EffectiveOptions(vectorindex.Options{})
	if err != nil {
		return nil, err
	}
	return loadSemanticEligibilityWithBounds(ctx, tx, profileFingerprint, bindingID, inputKind,
		vectorSpaceID, filterSQL, filterArgs, bounds)
}

func loadSemanticEligibilityWithBounds(ctx context.Context, tx metadataQuerier,
	profileFingerprint, bindingID string, inputKind document.EmbeddingInputKind,
	vectorSpaceID, filterSQL string, filterArgs []any, bounds vectorindex.Options,
) (_ map[semanticEligibilityKey][]semanticEligibility, retErr error) {
	bounds, err := vectorindex.EffectiveOptions(bounds)
	if err != nil {
		return nil, err
	}
	args := []any{vectorSpaceID, profileFingerprint, bindingID, inputKind}
	args = append(args, filterArgs...)
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE node_paths(id,path) AS (
		SELECT id,'' FROM nodes WHERE parent_id IS NULL
		UNION ALL
		SELECT child.id,parent.path || '/' || child.name
		FROM nodes child JOIN node_paths parent ON child.parent_id=parent.id
	)
		SELECT `+nodeCols+`,es.embedding_set_id,es.input_generation_id,es.input_kind,
			evr.vector_set_id,evr.input_id,evr.checksum,COALESCE(NULLIF(node_paths.path,''),'/')
		FROM `+nodeFrom+`
		JOIN embedding_sets es ON es.content_version_id=cv.version_id
		JOIN embedding_heads eh ON eh.content_version_id=es.content_version_id
		 AND eh.binding_id=es.binding_id AND eh.input_kind=es.input_kind
		 AND eh.embedding_set_id=es.embedding_set_id AND eh.vector_space_id=es.vector_space_id
		 AND eh.profile_fingerprint=es.profile_fingerprint
		JOIN embedding_vector_rows evr ON evr.vector_set_id=es.vector_set_id
		JOIN embedding_generation_inputs egi ON egi.generation_id=es.input_generation_id
		 AND egi.input_id=evr.input_id AND egi.rendered_checksum=evr.checksum
		JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
		JOIN node_paths ON node_paths.id=n.id
		WHERE es.vector_space_id=? AND es.profile_fingerprint=? AND es.binding_id=? AND es.input_kind=?
		  AND n.current_version_id=es.content_version_id AND n.trashed_at IS NULL
		  AND (es.input_kind='original_file' OR EXISTS(
		    SELECT 1 FROM rendition_heads rh
		    WHERE rh.content_version_id=es.content_version_id
		      AND rh.profile_fingerprint=es.profile_fingerprint
		      AND rh.attachment_id=eig.attachment_id
		  ))
		  `+filterSQL+`
		ORDER BY n.id,es.content_version_id,es.embedding_set_id,evr.row_order`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	eligible := make(map[semanticEligibilityKey][]semanticEligibility)
	memberships := 0
	for rows.Next() {
		var (
			entry semanticEligibility
			key   semanticEligibilityKey
		)
		node, scanErr := scanSemanticCandidate(rows, &entry.Candidate,
			&key.VectorSetID, &key.InputID, &key.InputChecksum, &entry.Path)
		if scanErr != nil {
			return nil, scanErr
		}
		memberships++
		if memberships > bounds.MaxRows {
			return nil, errors.New("semantic search eligibility membership exceeds vector index bounds")
		}
		entry.Node = node
		eligible[key] = append(eligible[key], entry)
	}
	return eligible, rows.Err()
}

func reduceSemanticCandidates(vaultID, vectorSpaceID string, neighbors []vectorindex.Neighbor,
	limit int, eligible map[semanticEligibilityKey][]semanticEligibility,
) ([]SemanticSearchCandidate, bool) {
	seen := make(map[int64]struct{}, limit+1)
	candidates := make([]SemanticSearchCandidate, 0, min(limit+1, len(eligible)))
	for _, neighbor := range neighbors {
		entries := eligible[semanticEligibilityKey{VectorSetID: neighbor.SetID,
			InputID: neighbor.InputKey, InputChecksum: neighbor.InputChecksum}]
		for _, entry := range entries {
			if _, duplicate := seen[entry.Node.ID]; duplicate {
				continue
			}
			seen[entry.Node.ID] = struct{}{}
			candidate := entry.Candidate
			candidate.VaultID = vaultID
			candidate.NodeID = entry.Node.ID
			candidate.NodeRevision = entry.Node.Revision
			candidate.ContentVersionID = entry.Node.CurrentVersionID
			candidate.VectorSpaceID = vectorSpaceID
			candidate.InputID = neighbor.InputKey
			candidate.Score, candidate.Distance = neighbor.Score, neighbor.Distance
			candidate.Path = entry.Path
			candidates = append(candidates, candidate)
			if len(candidates) == limit+1 {
				return candidates[:limit], true
			}
		}
	}
	return candidates, false
}

func scanSemanticCandidate(row interface{ Scan(dest ...any) error }, candidate *SemanticSearchCandidate,
	dest ...any,
) (Node, error) {
	var node Node
	fields := []any{&node.ID, &node.ParentID, &node.Name, &node.Kind,
		&node.CurrentVersionID, &node.BlobHash, &node.MD5, &node.Size, &node.MimeType,
		&node.Revision, &node.CreatedAt, &node.ModifiedAt, &node.TrashedAt,
		&candidate.EmbeddingSetID, &candidate.InputGenerationID, &candidate.InputKind}
	err := row.Scan(append(fields, dest...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("scanning semantic search candidate: %w", err)
	}
	return node, nil
}
