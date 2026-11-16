package store

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

func checkSemanticResultsUseCurrentDerivedEvidence(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	for _, kind := range []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk, document.EmbeddingInputImage} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			s := open(t)
			var record EmbeddingSetRecord
			if kind == document.EmbeddingInputImage {
				fixture := seedImageEmbeddingCatalog(t, s)
				record, _ = fixture.publish(t)
			} else {
				_, version, profile, attachment := seedEmbeddingCatalog(t, s)
				record = embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, kind, "chunk", attachment)
				require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
				require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
					FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: record.BindingID, InputKind: kind},
					SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
				}))
			}
			source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
			require.NoError(t, err)
			input := record.InputGeneration.Inputs[0]
			neighbors := []vectorindex.Neighbor{{SetID: record.VectorSet.ID, InputKey: input.ID, InputChecksum: input.RenderedChecksum, Score: 0.9}}
			opts := SearchOptions{ContentVersionIDs: []string{record.ContentVersionID}}
			resolved, err := s.ResolveSemanticCandidates(t.Context(), record.ProcessingProfileFingerprint, record.BindingID, kind, record.VectorSpace.ID, source.ManifestChecksum, neighbors, 1, opts, nil)
			require.NoError(t, err)
			require.Len(t, resolved.Candidates, 1)
			candidate := resolved.Candidates[0]
			require.Equal(t, record.ID, candidate.EmbeddingSetID)
			require.Equal(t, input.ID, candidate.InputID)
			require.Empty(t, candidate.Excerpt)
			evidence := SearchCandidateIdentity{NodeID: candidate.NodeID, ContentVersionID: candidate.ContentVersionID,
				Evidence: []SearchEvidenceIdentity{{Kind: "embedding", NodeID: candidate.NodeID, ContentVersionID: candidate.ContentVersionID,
					EmbeddingSetID: record.ID, InputGenerationID: record.InputGeneration.ID, InputID: input.ID, InputKind: kind,
					VectorSpaceID: record.VectorSpace.ID, SourceManifestChecksum: source.ManifestChecksum}}}
			checked, err := s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{evidence}, opts, record.ProcessingProfileFingerprint, record.BindingID)
			require.NoError(t, err)
			require.Len(t, checked.Candidates, 1)
			require.Equal(t, 1, checked.Coverage.CompleteDocuments)
			_, _, err = s.Trash(t.Context(), candidate.NodeID, UnconditionalRev)
			require.NoError(t, err)
			_, err = s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{evidence}, opts, record.ProcessingProfileFingerprint, record.BindingID)
			require.ErrorIs(t, err, ErrVectorIndexSourceStale)
		})
	}
}

func checkSemanticScopeFiltersAndExcerpts(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s, record, _ := publishedVectorIndexFixture(t, open(t))
	version, err := s.ContentVersionByID(t.Context(), record.ContentVersionID)
	require.NoError(t, err)
	directory, err := s.MkdirAll(t.Context(), "/scope/nested")
	require.NoError(t, err)
	moved, _, err := s.Move(t.Context(), version.NodeID, directory.ID, "scoped.pdf", UnconditionalRev)
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "semantic-scope")
	require.NoError(t, err)
	tagged, err := s.AssignTag(t.Context(), tag.ID, moved.ID, moved.Revision)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: version.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "before\x00after"}))
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, vectorIndexGenerationFixture("semantic-filter", source, []byte("index"), time.Now().UTC())))
	neighbors := []vectorindex.Neighbor{{SetID: record.VectorSet.ID, InputKey: record.InputGeneration.Inputs[0].ID, InputChecksum: record.InputGeneration.Inputs[0].RenderedChecksum, Score: 0.9}}
	at, err := time.Parse(time.RFC3339Nano, tagged.Node.ModifiedAt)
	require.NoError(t, err)
	_, err = s.MkdirAll(t.Context(), "/outside-scope")
	require.NoError(t, err)
	opts := SearchOptions{ContentVersionIDs: []string{record.ContentVersionID}, TagID: tag.ID, MIMEType: "application/pdf", UnderPath: "/scope", ModifiedSince: at.Format(time.RFC3339Nano), ModifiedBefore: at.Add(time.Nanosecond).Format(time.RFC3339Nano)}
	t.Run("excerpt stops before later match markers", func(t *testing.T) {
		require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: version.BlobHash,
			Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK,
			Text: strings.Repeat("a", 600) + "\x01" + strings.Repeat("b", 10)}))
		result, err := s.ResolveSemanticCandidates(t.Context(), record.ProcessingProfileFingerprint, record.BindingID,
			record.InputKind, record.VectorSpace.ID, source.ManifestChecksum, neighbors, 1, opts, nil)
		require.NoError(t, err)
		require.Len(t, result.Candidates, 1)
		require.Equal(t, strings.Repeat("a", 512), result.Candidates[0].Excerpt)
		require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: version.BlobHash,
			Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "before\x00after"}))
	})
	for _, excluded := range []string{"matching filters", "tag", "directory", "since", "before"} {
		t.Run(excluded, func(t *testing.T) {
			scope := opts
			switch excluded {
			case "tag":
				other, err := s.CreateTag(t.Context(), "other-tag")
				require.NoError(t, err)
				scope.TagID = other.ID
			case "directory":
				scope.UnderPath = "/outside-scope"
			case "since":
				scope.ModifiedSince = at.Add(time.Nanosecond).Format(time.RFC3339Nano)
				scope.ModifiedBefore = ""
			case "before":
				scope.ModifiedBefore = at.Format(time.RFC3339Nano)
				scope.ModifiedSince = ""
			}
			result, err := s.ResolveSemanticCandidates(t.Context(), record.ProcessingProfileFingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, source.ManifestChecksum, neighbors, 1, scope, nil)
			require.NoError(t, err)
			if excluded != "matching filters" {
				require.Empty(t, result.Candidates)
				return
			}
			require.Len(t, result.Candidates, 1)
			require.Equal(t, "/scope/nested/scoped.pdf", result.Candidates[0].Path)
			require.Equal(t, "before", result.Candidates[0].Excerpt)
			authority, err := s.AcquireSemanticSearchAuthority(t.Context(), record.ProcessingProfileFingerprint, record.BindingID, "scoped-reader", time.Now(), time.Minute, scope)
			require.NoError(t, err)
			require.Len(t, authority.ANNRows, 1)
			require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), authority.Lease.ID, authority.Lease.FencingToken, time.Now()))
		})
	}
}

func checkSemanticResolutionSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s, record, source := publishedVectorIndexFixture(t, newPostgresQueryStore(t))
	connection, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	var config *pgx.ConnConfig
	require.NoError(t, connection.Raw(func(raw any) error {
		pg, ok := raw.(*stdlib.Conn)
		require.True(t, ok)
		config = pg.Conn().Config().Copy()
		return nil
	}))
	require.NoError(t, connection.Close())
	version, err := s.ContentVersionByID(t.Context(), record.ContentVersionID)
	require.NoError(t, err)
	var changed bool
	config.Tracer = &afterFirstSelect{run: func() {
		_, _, err = s.Trash(t.Context(), version.NodeID, UnconditionalRev)
		require.NoError(t, err)
		changed = true
	}}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	reader := &Store{db: db, rootID: s.rootID, vaultID: s.vaultID, postgresSchema: s.postgresSchema}
	neighbors := []vectorindex.Neighbor{{SetID: record.VectorSet.ID, InputKey: record.InputGeneration.Inputs[0].ID, InputChecksum: record.InputGeneration.Inputs[0].RenderedChecksum, Score: 0.9}}
	result, err := reader.ResolveSemanticCandidates(t.Context(), record.ProcessingProfileFingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, source.ManifestChecksum, neighbors, 1, SearchOptions{}, nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, result.Candidates, 1)
	require.Equal(t, version.NodeID, result.Candidates[0].NodeID)
	_, err = reader.ResolveSemanticCandidates(t.Context(), record.ProcessingProfileFingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, source.ManifestChecksum, neighbors, 1, SearchOptions{}, nil)
	require.ErrorIs(t, err, ErrVectorIndexSourceStale)
}
