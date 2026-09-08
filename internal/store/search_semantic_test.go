package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestResolveSemanticCandidatesReturnsOnlyCurrentScopedHeads(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	publishEmbeddingSetForSemanticSearch(t, s, record)
	otherBinding := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "required", "")
	require.Equal(t, record.VectorSet.ID, otherBinding.VectorSet.ID)
	require.Equal(t, record.VectorSpace.ID, otherBinding.VectorSpace.ID)
	publishEmbeddingSetForSemanticSearch(t, s, otherBinding)
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "late-unembedded.pdf",
		fakeHash("late-unembedded"), 1, "application/pdf")
	require.NoError(t, err)

	resolution, err := s.ResolveSemanticCandidates(t.Context(), profile.Fingerprint, record.BindingID,
		record.InputKind, record.VectorSpace.ID, source.ManifestChecksum,
		[]vectorindex.Neighbor{{
			SetID: record.VectorSet.ID, InputKey: versionID,
			InputChecksum: record.InputGeneration.Inputs[0].RenderedChecksum, Score: 0.9}},
		10, SearchOptions{MIMEType: "application/pdf"})
	require.NoError(t, err)
	assert.False(t, resolution.Truncated)
	assert.Equal(t, 3, resolution.ScopedDocuments)
	assert.Equal(t, 1, resolution.CompleteDocuments)
	require.Len(t, resolution.Candidates, 1)
	assert.Equal(t, versionID, resolution.Candidates[0].ContentVersionID)
	assert.Equal(t, record.ID, resolution.Candidates[0].EmbeddingSetID)
	assert.NotEqual(t, otherBinding.ID, resolution.Candidates[0].EmbeddingSetID)
	assert.Equal(t, document.EmbeddingInputOriginalFile, resolution.Candidates[0].InputKind)
	assert.Positive(t, resolution.Candidates[0].NodeRevision)
	assert.Empty(t, resolution.Candidates[0].Excerpt, "direct-file semantic evidence cannot fabricate text")

	filtered, err := s.ResolveSemanticCandidates(t.Context(), profile.Fingerprint, record.BindingID,
		record.InputKind, record.VectorSpace.ID, source.ManifestChecksum,
		[]vectorindex.Neighbor{{
			SetID: record.VectorSet.ID, InputKey: versionID,
			InputChecksum: record.InputGeneration.Inputs[0].RenderedChecksum, Score: 0.9}},
		10, SearchOptions{MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Empty(t, filtered.Candidates, "scope filters apply before the semantic document cutoff")
}

func TestSourceFenceScopesSemanticSearchAuthorityAndCandidates(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	publishEmbeddingSetForSemanticSearch(t, s, record)
	set, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
		MaxRows: 100, MaxDimension: record.VectorSpace.Descriptor.Dimension,
		MaxBytes: len(record.VectorSet.Payload),
	})
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{record.VectorSet.ID})
	require.NoError(t, err)
	generation, err := vectorindex.BuildGeneration(
		manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	stored := VectorIndexGenerationRecord{ID: VectorIndexGenerationID(source.ManifestChecksum, generation.Bytes()),
		VectorSpaceID: record.VectorSpace.ID, SourceManifestChecksum: source.ManifestChecksum,
		IndexManifestChecksum: generation.Metadata().Manifest.Checksum, Bytes: generation.Bytes(),
		RowCount: generation.Metadata().RowCount, BuiltAt: embeddingCatalogTime}
	require.NoError(t, putActiveVectorIndexGenerationForTest(t, s, stored))
	unembedded, err := s.CreateFile(t.Context(), s.RootID(), "source-fence-unembedded.pdf",
		fakeHash("source-fence-unembedded"), 1, "application/pdf")
	require.NoError(t, err)
	invalidFence := SearchOptions{ContentVersionIDs: []string{"not-a-content-version"}}
	_, err = s.AcquireSemanticSearchAuthority(t.Context(), profile.Fingerprint,
		record.BindingID, "source-fence-invalid", time.Now().UTC(), time.Minute, invalidFence)
	require.ErrorContains(t, err, "invalid content version ID")
	var leases int
	require.NoError(t, s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM vector_index_reader_leases`).Scan(&leases))
	require.Zero(t, leases, "invalid scope must fail before acquiring semantic authority")
	_, err = s.ResolveSemanticCandidates(t.Context(), profile.Fingerprint,
		record.BindingID, record.InputKind, record.VectorSpace.ID, source.ManifestChecksum,
		nil, 10, invalidFence)
	require.ErrorContains(t, err, "invalid content version ID")

	now := time.Now().UTC()
	included, err := s.AcquireSemanticSearchAuthority(t.Context(), profile.Fingerprint,
		record.BindingID, "source-fence-included", now, time.Minute,
		SearchOptions{ContentVersionIDs: []string{versionID}})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, s.ReleaseVectorIndexGeneration(context.Background(),
			included.Lease.ID, included.Lease.FencingToken, now))
	})
	require.Equal(t, 1, included.ScopedDocuments)
	require.Equal(t, 1, included.CompleteDocuments)
	require.Equal(t, source.ManifestChecksum, included.Lease.Generation.SourceManifestChecksum)

	excluded, err := s.AcquireSemanticSearchAuthority(t.Context(), profile.Fingerprint,
		record.BindingID, "source-fence-unembedded", now, time.Minute,
		SearchOptions{ContentVersionIDs: []string{unembedded.CurrentVersionID}})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, s.ReleaseVectorIndexGeneration(context.Background(),
			excluded.Lease.ID, excluded.Lease.FencingToken, now))
	})
	require.Equal(t, 1, excluded.ScopedDocuments)
	require.Equal(t, 0, excluded.CompleteDocuments)
	require.Equal(t, source.ManifestChecksum, excluded.Lease.Generation.SourceManifestChecksum)

	neighbors := []vectorindex.Neighbor{{SetID: record.VectorSet.ID, InputKey: versionID,
		InputChecksum: record.InputGeneration.Inputs[0].RenderedChecksum, Score: 0.9}}
	resolution, err := s.ResolveSemanticCandidates(t.Context(), profile.Fingerprint,
		record.BindingID, record.InputKind, record.VectorSpace.ID, source.ManifestChecksum,
		neighbors, 10, SearchOptions{ContentVersionIDs: []string{versionID}})
	require.NoError(t, err)
	require.Equal(t, source.ManifestChecksum, resolution.SourceManifestChecksum)
	require.Equal(t, 1, resolution.ScopedDocuments)
	require.Equal(t, 1, resolution.CompleteDocuments)
	require.False(t, resolution.Truncated)
	require.Len(t, resolution.Candidates, 1)
	require.Equal(t, versionID, resolution.Candidates[0].ContentVersionID)

	resolution, err = s.ResolveSemanticCandidates(t.Context(), profile.Fingerprint,
		record.BindingID, record.InputKind, record.VectorSpace.ID, source.ManifestChecksum,
		neighbors, 10, SearchOptions{ContentVersionIDs: []string{unembedded.CurrentVersionID}})
	require.NoError(t, err)
	require.Equal(t, source.ManifestChecksum, resolution.SourceManifestChecksum)
	require.Equal(t, 1, resolution.ScopedDocuments)
	require.Equal(t, 0, resolution.CompleteDocuments)
	require.False(t, resolution.Truncated)
	require.Empty(t, resolution.Candidates)
}

func TestResolveSemanticCandidatesKeepsSharedPayloadMembershipsAndExcludesOtherBinding(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := embeddingCatalogProfile(t)
	build := catalogRenditionBuild(s, profile)
	build.EvidenceChecksum = embeddingCatalogEvidence(t).Checksum
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	for index, attachmentID := range []string{catalogAttachmentFirst, catalogAttachmentSecond} {
		attachment := RenditionAttachmentRecord{ID: attachmentID, VaultID: s.VaultID(),
			ContentVersionID: versions[index], BuildID: build.ID, Profile: profile,
			AttachedAt: embeddingCatalogTime}
		require.NoError(t, publishRenditionForTest(t, s, attachment, embeddingCatalogTime,
			testSHA256([]byte(fmt.Sprintf("semantic-shared-lexical-%d", index)))))
	}
	first := embeddingSetFixture(s, versions[0], profile.Fingerprint,
		document.EmbeddingInputRenditionChunk, "chunk", catalogAttachmentFirst)
	second := embeddingSetFixture(s, versions[1], profile.Fingerprint,
		document.EmbeddingInputRenditionChunk, "chunk", catalogAttachmentSecond)
	require.Equal(t, first.VectorSet.ID, second.VectorSet.ID,
		"synthetic documents must reference the same canonical vector payload")
	publishEmbeddingSetForSemanticSearch(t, s, first)
	publishEmbeddingSetForSemanticSearch(t, s, second)

	otherBinding := embeddingSetFixture(s, versions[0], profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "required", "")
	publishEmbeddingSetForSemanticSearch(t, s, otherBinding)
	require.Equal(t, first.VectorSpace.ID, otherBinding.VectorSpace.ID,
		"the non-selected binding must share the selected vector space")
	source, err := s.CaptureVectorIndexSource(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	neighbors := make([]vectorindex.Neighbor, len(first.InputGeneration.Inputs))
	for i, input := range first.InputGeneration.Inputs {
		neighbors[i] = vectorindex.Neighbor{
			SetID: first.VectorSet.ID, InputKey: input.ID, InputChecksum: input.RenderedChecksum,
			Score: 1 - float64(i)/10}
	}

	resolution, err := s.ResolveSemanticCandidates(t.Context(), profile.Fingerprint, "chunk",
		document.EmbeddingInputRenditionChunk, first.VectorSpace.ID, source.ManifestChecksum,
		neighbors, 10, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, resolution.Candidates, 2)
	assert.Equal(t, []string{versions[0], versions[1]}, []string{
		resolution.Candidates[0].ContentVersionID, resolution.Candidates[1].ContentVersionID})
	for index, candidate := range resolution.Candidates {
		assert.Equal(t, document.EmbeddingInputRenditionChunk, candidate.InputKind)
		assert.Equal(t, []string{first.ID, second.ID}[index], candidate.EmbeddingSetID)
		assert.NotEqual(t, otherBinding.ID, candidate.EmbeddingSetID)
	}
	require.ErrorContains(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := loadSemanticEligibilityWithBounds(t.Context(), tx, profile.Fingerprint, "chunk",
			document.EmbeddingInputRenditionChunk, first.VectorSpace.ID, "", nil,
			vectorindex.Options{MaxRows: 1})
		return err
	}), "membership exceeds")
}

func TestResolveSemanticCandidatesRejectsStaleSourceManifest(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	publishEmbeddingSetForSemanticSearch(t, s, record)

	_, err := s.ResolveSemanticCandidates(t.Context(), profile.Fingerprint, record.BindingID,
		record.InputKind, record.VectorSpace.ID, strings.Repeat("f", 64), nil, 10, SearchOptions{})
	require.ErrorIs(t, err, ErrVectorIndexSourceStale)
}

func TestReduceSemanticCandidatesExhaustsNeighborsAndKeepsBestDocumentMatch(t *testing.T) {
	const missed = 10_000
	neighbors := make([]vectorindex.Neighbor, missed+2)
	for index := range missed {
		neighbors[index] = vectorindex.Neighbor{
			SetID: "filtered", InputKey: fmt.Sprintf("filtered-%d", index),
			InputChecksum: fakeHash("filtered")}
	}
	firstKey := semanticEligibilityKey{VectorSetID: "eligible", InputID: "chunk-1",
		InputChecksum: fakeHash("chunk-1")}
	secondKey := semanticEligibilityKey{VectorSetID: "eligible", InputID: "chunk-2",
		InputChecksum: fakeHash("chunk-2")}
	neighbors[missed] = vectorindex.Neighbor{
		SetID: firstKey.VectorSetID, InputKey: firstKey.InputID, InputChecksum: firstKey.InputChecksum, Score: 0.75}
	neighbors[missed+1] = vectorindex.Neighbor{
		SetID: secondKey.VectorSetID, InputKey: secondKey.InputID, InputChecksum: secondKey.InputChecksum, Score: 0.5}
	entry := semanticEligibility{Node: Node{ID: 42, Revision: 7, CurrentVersionID: "version-42"},
		Path: "/later.pdf", Candidate: SemanticSearchCandidate{EmbeddingSetID: "embedding-set",
			InputGenerationID: "generation", InputKind: document.EmbeddingInputRenditionChunk}}
	eligible := map[semanticEligibilityKey][]semanticEligibility{firstKey: {entry}, secondKey: {entry}}

	candidates, truncated := reduceSemanticCandidates("vault", fakeHash("space"), neighbors, 10, eligible)

	assert.False(t, truncated)
	require.Len(t, candidates, 1)
	assert.Equal(t, int64(42), candidates[0].NodeID)
	assert.Equal(t, int64(7), candidates[0].NodeRevision)
	assert.Equal(t, "chunk-1", candidates[0].InputID)
	assert.InDelta(t, 0.75, candidates[0].Score, 1e-12)
}

func TestAcquireSemanticSearchAuthorityUsesSourceBoundCanonicalGeneration(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	publishEmbeddingSetForSemanticSearch(t, s, record)
	set, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
		MaxRows: 100, MaxDimension: record.VectorSpace.Descriptor.Dimension,
		MaxBytes: len(record.VectorSet.Payload),
	})
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{record.VectorSet.ID})
	require.NoError(t, err)
	generation, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	stored := VectorIndexGenerationRecord{ID: VectorIndexGenerationID(source.ManifestChecksum, generation.Bytes()),
		VectorSpaceID: record.VectorSpace.ID, SourceManifestChecksum: source.ManifestChecksum,
		IndexManifestChecksum: generation.Metadata().Manifest.Checksum, Bytes: generation.Bytes(),
		RowCount: generation.Metadata().RowCount, BuiltAt: embeddingCatalogTime}
	require.NoError(t, putActiveVectorIndexGenerationForTest(t, s, stored))

	now := time.Now().UTC()
	authority, err := s.AcquireSemanticSearchAuthority(t.Context(), profile.Fingerprint,
		record.BindingID, "retrieval-test", now, time.Minute, SearchOptions{MIMEType: "application/pdf"})
	require.NoError(t, err)
	assert.Equal(t, record.VectorSpace.Descriptor, authority.VectorSpace.Descriptor)
	assert.False(t, authority.BindingRequired)
	assert.Equal(t, 2, authority.ScopedDocuments)
	assert.Equal(t, 1, authority.CompleteDocuments)
	assert.Equal(t, stored.ID, authority.Lease.Generation.ID)
	require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), authority.Lease.ID,
		authority.Lease.FencingToken, now))
}

func TestAcquireSemanticSearchAuthorityPreservesReleaseFailure(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	publishEmbeddingSetForSemanticSearch(t, s, record)
	set, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
		MaxRows: 100, MaxDimension: record.VectorSpace.Descriptor.Dimension,
		MaxBytes: len(record.VectorSet.Payload),
	})
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{record.VectorSet.ID})
	require.NoError(t, err)
	generation, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	staleSource := strings.Repeat("f", 64)
	stored := VectorIndexGenerationRecord{ID: VectorIndexGenerationID(staleSource, generation.Bytes()),
		VectorSpaceID: record.VectorSpace.ID, SourceManifestChecksum: staleSource,
		IndexManifestChecksum: generation.Metadata().Manifest.Checksum, Bytes: generation.Bytes(),
		RowCount: generation.Metadata().RowCount, BuiltAt: embeddingCatalogTime}
	require.NoError(t, putActiveVectorIndexGenerationForTest(t, s, stored))
	_, err = s.db.Exec(`CREATE TRIGGER fail_semantic_lease_release BEFORE DELETE ON vector_index_reader_leases
		BEGIN SELECT RAISE(ABORT, 'synthetic lease release failure'); END`)
	require.NoError(t, err)

	_, err = s.AcquireSemanticSearchAuthority(t.Context(), profile.Fingerprint,
		record.BindingID, "retrieval-test", time.Now().UTC(), time.Minute, SearchOptions{})
	require.ErrorIs(t, err, ErrVectorIndexSourceStale)
	require.ErrorContains(t, err, "synthetic lease release failure")
}

func TestAcquireSemanticSearchAuthorityRejectsCorruptGeneration(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	publishEmbeddingSetForSemanticSearch(t, s, record)
	set, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
		MaxRows: 100, MaxDimension: record.VectorSpace.Descriptor.Dimension,
		MaxBytes: len(record.VectorSet.Payload),
	})
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{record.VectorSet.ID})
	require.NoError(t, err)
	generation, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	stored := VectorIndexGenerationRecord{ID: VectorIndexGenerationID(source.ManifestChecksum, generation.Bytes()),
		VectorSpaceID: record.VectorSpace.ID, SourceManifestChecksum: source.ManifestChecksum,
		IndexManifestChecksum: generation.Metadata().Manifest.Checksum, Bytes: generation.Bytes(),
		RowCount: generation.Metadata().RowCount, BuiltAt: embeddingCatalogTime}
	require.NoError(t, putActiveVectorIndexGenerationForTest(t, s, stored))
	corrupt := append([]byte(nil), generation.Bytes()...)
	corrupt[len(corrupt)-1] ^= 1
	_, err = s.db.Exec(`UPDATE vector_index_generations SET generation_bytes=? WHERE generation_id=?`, corrupt, stored.ID)
	require.NoError(t, err)

	_, err = s.AcquireSemanticSearchAuthority(t.Context(), profile.Fingerprint,
		record.BindingID, "retrieval-test", time.Now().UTC(), time.Minute, SearchOptions{})
	require.ErrorContains(t, err, "generation ID does not match source-bound bytes")
	var leases int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM vector_index_reader_leases`).Scan(&leases))
	assert.Zero(t, leases)
}

func publishEmbeddingSetForSemanticSearch(t *testing.T, s *Store, record EmbeddingSetRecord) {
	t.Helper()
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key: EmbeddingHeadKey{ContentVersionID: record.ContentVersionID, BindingID: record.BindingID,
			InputKind: record.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: record.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
	}))
}
