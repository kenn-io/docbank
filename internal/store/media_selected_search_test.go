package store

import (
	"database/sql"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
	"testing"
)

func TestSelectedMediaEmbeddingBuildFence(t *testing.T) {
	t.Parallel()
	s, version, profile, attachment := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: record.BindingID, InputKind: record.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime, FencingToken: 1}))
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	var build string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, attachment).Scan(&build))
	neighbors := []vectorindex.Neighbor{{SetID: record.VectorSet.ID, InputKey: record.InputGeneration.Inputs[0].ID, InputChecksum: record.InputGeneration.Inputs[0].RenderedChecksum, Score: 1}}
	requested := []SearchCandidateIdentity{{NodeID: sRootVersionNode(t, s, version), ContentVersionID: version, BuildID: build, Evidence: []SearchEvidenceIdentity{{NodeID: sRootVersionNode(t, s, version), ContentVersionID: version, Kind: "embedding", BuildID: build, VectorSpaceID: record.VectorSpace.ID, EmbeddingSetID: record.ID, InputGenerationID: record.InputGeneration.ID, InputID: record.InputGeneration.Inputs[0].ID, InputKind: record.InputKind, SourceManifestChecksum: source.ManifestChecksum}}}}
	filter, args := semanticSearchFilterSQL(SearchOptions{SelectedBuilds: []SearchSelectedBuild{{ContentVersionID: version, BuildID: build}}})
	plan, err := s.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT es.content_version_id FROM embedding_sets es JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id JOIN content_versions cv ON cv.version_id=es.content_version_id WHERE 1=1 `+filter, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, plan.Close()) }()
	for plan.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, plan.Scan(&id, &parent, &unused, &detail))
		require.NotContains(t, detail, "CORRELATED", "selected pairs must not be scanned once per embedding")
	}
	require.NoError(t, plan.Err())
	for _, wrong := range []bool{false, true} {
		selected := build
		if wrong {
			selected = fakeHash("different-selected-build")
		}
		opts := SearchOptions{SelectedBuilds: []SearchSelectedBuild{{ContentVersionID: version, BuildID: selected}}}
		require.NoError(t, s.withReadTx(t.Context(), func(tx *sql.Tx) error {
			rows, whole, err := semanticANNRowsForSourceTx(t.Context(), tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, opts)
			require.NoError(t, err)
			require.False(t, whole)
			if wrong {
				require.Empty(t, rows)
			} else {
				require.NotEmpty(t, rows)
			}
			return nil
		}))
		result, err := s.ResolveSemanticCandidates(t.Context(), profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, source.ManifestChecksum, neighbors, 1, opts, nil)
		require.NoError(t, err)
		current, err := s.RevalidateSearchCandidates(t.Context(), requested, opts, profile.Fingerprint, record.BindingID)
		require.NoError(t, err)
		require.NotNil(t, current.Coverage)
		require.Equal(t, 1, current.Coverage.ScopedDocuments)
		if wrong {
			require.Empty(t, result.Candidates)
			require.Empty(t, current.Candidates)
			require.Zero(t, current.Coverage.CompleteDocuments)
		} else {
			require.Len(t, result.Candidates, 1)
			require.Len(t, current.Candidates, 1)
			require.Equal(t, 1, current.Coverage.CompleteDocuments)
		}
	}
	// Two selected builds for the same content are not covered by one head.
	opts := SearchOptions{SelectedBuilds: []SearchSelectedBuild{{ContentVersionID: version, BuildID: build}, {ContentVersionID: version, BuildID: fakeHash("another-selected-build")}}}
	current, err := s.RevalidateSearchCandidates(t.Context(), nil, opts, profile.Fingerprint, record.BindingID)
	require.NoError(t, err)
	require.Equal(t, 1, current.Coverage.ScopedDocuments)
	require.Zero(t, current.Coverage.CompleteDocuments)
}

func TestSelectedSemanticRowsAvailable(t *testing.T) {
	t.Parallel()
	s, version, profile, attachment := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: record.BindingID, InputKind: record.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime, FencingToken: 1}))
	var buildID string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, attachment).Scan(&buildID))
	opts := SearchOptions{SelectedBuilds: []SearchSelectedBuild{{ContentVersionID: version, BuildID: buildID}}}
	available, err := s.SelectedSemanticRowsAvailable(t.Context(), profile.Fingerprint, "chunk", opts)
	require.NoError(t, err)
	require.True(t, available)
	opts.ContentVersionIDs = []string{"123e4567-e89b-42d3-a456-426614174001"}
	available, err = s.SelectedSemanticRowsAvailable(t.Context(), profile.Fingerprint, "chunk", opts)
	require.NoError(t, err)
	require.False(t, available, "selected builds must still respect a separate version fence")
	opts.ContentVersionIDs = nil
	available, err = s.SelectedSemanticRowsAvailable(t.Context(), testSHA256([]byte("unpersisted-profile")), "chunk", opts)
	require.NoError(t, err)
	require.False(t, available)
	_, err = s.SelectedSemanticRowsAvailable(t.Context(), profile.Fingerprint, "missing-binding", opts)
	require.ErrorIs(t, err, ErrNotFound, "a missing binding in a stored profile remains an authority error")
	_, err = s.SelectedSemanticRowsAvailable(t.Context(), "malformed-profile", "chunk", opts)
	require.ErrorContains(t, err, "fingerprint")
}

func sRootVersionNode(t *testing.T, s *Store, version string) int64 {
	t.Helper()
	value, err := s.ContentVersionByID(t.Context(), version)
	require.NoError(t, err)
	return value.NodeID
}
