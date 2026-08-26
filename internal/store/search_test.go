package store

import (
	"encoding/json/jsontext"
	"errors"
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
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID,
			InputKind: record.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
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
	assert.Equal(t, document.EmbeddingInputOriginalFile, resolution.Candidates[0].InputKind)
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

func TestResolveSemanticCandidatesRejectsStaleSourceManifest(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID,
			InputKind: record.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	_, err := s.ResolveSemanticCandidates(t.Context(), profile.Fingerprint, record.BindingID,
		record.InputKind, record.VectorSpace.ID, strings.Repeat("f", 64), nil, 10, SearchOptions{})

	require.ErrorIs(t, err, ErrVectorIndexSourceStale)
}

func TestReduceSemanticCandidatesExhaustsNeighborsWithoutDatabaseWork(t *testing.T) {
	const missed = 10_000
	neighbors := make([]vectorindex.Neighbor, missed+1)
	for index := range missed {
		neighbors[index] = vectorindex.Neighbor{
			SetID: "filtered", InputKey: fmt.Sprintf("filtered-%d", index), InputChecksum: fakeHash("filtered"),
		}
	}
	neighbors[missed] = vectorindex.Neighbor{
		SetID: "eligible", InputKey: "eligible-input", InputChecksum: fakeHash("eligible"), Score: 0.75,
	}
	key := semanticEligibilityKey{
		VectorSetID: "eligible", InputID: "eligible-input", InputChecksum: fakeHash("eligible"),
	}
	eligible := map[semanticEligibilityKey]semanticEligibility{
		key: {Node: Node{ID: 42, CurrentVersionID: "version-42"}, Path: "/later.pdf",
			Candidate: SemanticSearchCandidate{EmbeddingSetID: "embedding-set", InputGenerationID: "generation",
				InputKind: document.EmbeddingInputOriginalFile}},
	}

	candidates, truncated := reduceSemanticCandidates("vault", fakeHash("space"), neighbors, 10, eligible)

	assert.False(t, truncated)
	require.Len(t, candidates, 1)
	assert.Equal(t, int64(42), candidates[0].NodeID)
	assert.InDelta(t, 0.75, candidates[0].Score, 1e-12)
}

func TestReduceSemanticCandidatesKeepsBestChunkPerDocument(t *testing.T) {
	spaceID := fakeHash("space")
	firstKey := semanticEligibilityKey{VectorSetID: "set", InputID: "chunk-1", InputChecksum: fakeHash("chunk-1")}
	secondKey := semanticEligibilityKey{VectorSetID: "set", InputID: "chunk-2", InputChecksum: fakeHash("chunk-2")}
	eligible := map[semanticEligibilityKey]semanticEligibility{
		firstKey: {Node: Node{ID: 42, CurrentVersionID: "version-42"}, Path: "/chunked.pdf",
			Candidate: SemanticSearchCandidate{EmbeddingSetID: "embedding-set", InputGenerationID: "generation",
				InputKind: document.EmbeddingInputRenditionChunk}},
		secondKey: {Node: Node{ID: 42, CurrentVersionID: "version-42"}, Path: "/chunked.pdf",
			Candidate: SemanticSearchCandidate{EmbeddingSetID: "embedding-set", InputGenerationID: "generation",
				InputKind: document.EmbeddingInputRenditionChunk}},
	}

	candidates, truncated := reduceSemanticCandidates("vault", spaceID, []vectorindex.Neighbor{
		{SetID: firstKey.VectorSetID, InputKey: firstKey.InputID, InputChecksum: firstKey.InputChecksum, Score: 0.9},
		{SetID: secondKey.VectorSetID, InputKey: secondKey.InputID, InputChecksum: secondKey.InputChecksum, Score: 0.8},
	}, 10, eligible)

	assert.False(t, truncated)
	require.Len(t, candidates, 1)
	assert.Equal(t, "chunk-1", candidates[0].InputID)
	assert.InDelta(t, 0.9, candidates[0].Score, 1e-12)
}

func TestAcquireSemanticSearchAuthorityUsesStoredDescriptorAndCoverage(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID,
			InputKind: record.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	set, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
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
	stored := VectorIndexGenerationRecord{ID: hashVectorIndexTest("semantic-search-generation"),
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

func TestSearchExplainedLexicalCandidatesCitesActiveRenditionSegment(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID,
		strings.Repeat("x", 2048)+" mercury bounded evidence excerpt")
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	generation, err := s.StageLexicalGeneration(t.Context(), hashVectorIndexTest("e9-lexical"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: embeddingCatalogTime}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), attachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: embeddingCatalogTime}, generation.ID))

	candidates, truncated, err := s.SearchExplainedLexicalCandidates(t.Context(), "mercury", 10, SearchOptions{})
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, candidates, 1)
	assert.Equal(t, build.ID, candidates[0].BuildID)
	assert.Equal(t, build.LexicalSegments[0].ID, candidates[0].SegmentID)
	assert.Contains(t, candidates[0].Excerpt, "mercury")
	assert.LessOrEqual(t, len([]rune(candidates[0].Excerpt)), maxExplainedSearchExcerptRunes)
	assert.Equal(t, versions[0], candidates[0].Node.CurrentVersionID)
}

func TestSearchExplainedLexicalCandidatesIncludesNamePath(t *testing.T) {
	s := newTestStore(t)
	docs, err := s.Mkdir(t.Context(), s.RootID(), "docs")
	require.NoError(t, err)
	_, err = s.Mkdir(t.Context(), s.RootID(), "alpha-folder")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), docs.ID, "alpha.pdf", fakeHash("alpha"), 1, "application/pdf")
	require.NoError(t, err)

	candidates, truncated, err := s.SearchExplainedLexicalCandidates(t.Context(), "alpha", 10, SearchOptions{})

	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, candidates, 1)
	assert.Equal(t, "/docs/alpha.pdf", candidates[0].Path)
}

func TestSearchFindsLiveNodesOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, docs.ID, "tax-return-2024.pdf", fakeHash("a1"), 1, "application/pdf")
	require.NoError(t, err)
	trashed, err := s.CreateFile(ctx, docs.ID, "tax-return-2019.pdf", fakeHash("b2"), 1, "application/pdf")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, trashed.ID, -1)
	require.NoError(t, err)

	hits, _, err := s.SearchPage(ctx, "tax", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "tax-return-2024.pdf", hits[0].Node.Name)
	assert.Equal(t, "/docs/tax-return-2024.pdf", hits[0].Path)
	assert.Equal(t, SearchMatchName, hits[0].Match)
}

func TestSearchPrefixAndRename(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	f, err := s.CreateFile(ctx, s.RootID(), "insurance-policy.pdf", fakeHash("a1"), 1, "application/pdf")
	require.NoError(t, err)

	hits, _, err := s.SearchPage(ctx, "insur", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1)

	// Rename must update the index (FTS triggers).
	_, _, err = s.Move(ctx, f.ID, s.RootID(), "car-policy.pdf", -1)
	require.NoError(t, err)
	hits, _, err = s.SearchPage(ctx, "insur", 0)
	require.NoError(t, err)
	assert.Empty(t, hits)
	hits, _, err = s.SearchPage(ctx, "car", 0)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

func TestSearchSurvivesOperatorInput(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.CreateFile(ctx, s.RootID(), "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	// FTS operator syntax in user input must not error.
	for _, q := range []string{`"unbalanced`, `AND OR NOT`, `a*b(c)`} {
		_, _, err := s.SearchPage(ctx, q, 0)
		assert.NoError(t, err, q)
	}
}

func TestSearchRanksMoreRelevantFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	// Create two files: one with term frequency 3, one with frequency 1.
	// BM25 ranking should place the higher-frequency match first. The
	// less-relevant name is inserted FIRST so unordered rowid/scan order
	// disagrees with rank order — dropping the ORDER BY fails this test.
	_, err := s.CreateFile(ctx, s.RootID(), "tax report.pdf", fakeHash("b2"), 1, "application/pdf")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "tax tax tax.pdf", fakeHash("a1"), 1, "application/pdf")
	require.NoError(t, err)

	hits, _, err := s.SearchPage(ctx, "tax", 0)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, "tax tax tax.pdf", hits[0].Node.Name)
	assert.Equal(t, "tax report.pdf", hits[1].Node.Name)
}

func TestSearchTieBreaksByName(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	// Same token count and term frequency → equal BM25 rank. Insert in
	// reverse name order so unordered scan order disagrees with the name
	// tie-break — dropping the secondary ORDER BY fails this test.
	_, err := s.CreateFile(ctx, s.RootID(), "tax c.pdf", fakeHash("c3"), 1, "application/pdf")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "tax b.pdf", fakeHash("b2"), 1, "application/pdf")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "tax a.pdf", fakeHash("a1"), 1, "application/pdf")
	require.NoError(t, err)

	hits, _, err := s.SearchPage(ctx, "tax", 0)
	require.NoError(t, err)
	require.Len(t, hits, 3)
	assert.Equal(t, "tax a.pdf", hits[0].Node.Name)
	assert.Equal(t, "tax b.pdf", hits[1].Node.Name)
	assert.Equal(t, "tax c.pdf", hits[2].Node.Name)
}

func TestSearchPageReportsTruncation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for i, name := range []string{"report-a.pdf", "report-b.pdf", "report-c.pdf"} {
		_, err := s.CreateFile(ctx, s.RootID(), name, fakeHash(string(rune('a'+i))), 1, "application/pdf")
		require.NoError(t, err)
	}

	hits, truncated, err := s.SearchPage(ctx, "report", 2)
	require.NoError(t, err)
	assert.Len(t, hits, 2)
	assert.True(t, truncated)

	hits, truncated, err = s.SearchPage(ctx, "report", 3)
	require.NoError(t, err)
	assert.Len(t, hits, 3)
	assert.False(t, truncated)
}

func TestSearchPageFiltersNameAndContentMatchesByTag(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	tag, err := s.CreateTag(ctx, "taxes")
	require.NoError(t, err)

	nameMatch, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-return.pdf", fakeHash("a1"), 4, "application/pdf",
	)
	require.NoError(t, err)
	contentMatch, err := s.CreateFile(
		ctx, s.RootID(), "notes.md", fakeHash("b2"), 4, "text/markdown",
	)
	require.NoError(t, err)
	untagged, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-draft.pdf", fakeHash("c3"), 4, "application/pdf",
	)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, nameMatch.ID, nameMatch.Revision)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, contentMatch.ID, contentMatch.Revision)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: contentMatch.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "quarterly tax notes",
	}))

	hits, truncated, err := s.SearchPageWithOptions(
		ctx, "quarterly", 10, SearchOptions{TagID: tag.ID},
	)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.False(t, truncated)
	assert.Equal(t, nameMatch.ID, hits[0].Node.ID)
	assert.Equal(t, SearchMatchName, hits[0].Match)
	assert.Equal(t, contentMatch.ID, hits[1].Node.ID)
	assert.Equal(t, SearchMatchContent, hits[1].Match)
	assert.NotEqual(t, untagged.ID, hits[0].Node.ID)
	assert.NotEqual(t, untagged.ID, hits[1].Node.ID)

	hits, truncated, err = s.SearchPageWithOptions(
		ctx, "quarterly", 1, SearchOptions{TagID: tag.ID},
	)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
	assert.True(t, truncated)

	_, _, err = s.SearchPageWithOptions(ctx, "quarterly", 10, SearchOptions{
		TagID: "11111111-1111-4111-8111-111111111111",
	})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSearchPageFiltersCurrentMediaTypeWithParameters(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	tag, err := s.CreateTag(ctx, "reviewed")
	require.NoError(t, err)
	nameMatch, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-return.txt", fakeHash("d4"), 4, "text/plain",
	)
	require.NoError(t, err)
	contentMatch, err := s.CreateFile(
		ctx, s.RootID(), "notes.bin", fakeHash("e5"), 4, "text/plain; charset=utf-8",
	)
	require.NoError(t, err)
	untaggedText, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-draft.txt", fakeHash("f6"), 4, "text/plain; charset=us-ascii",
	)
	require.NoError(t, err)
	_, err = s.CreateFile(
		ctx, s.RootID(), "quarterly-scan.pdf", fakeHash("a7"), 4, "application/pdf",
	)
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "quarterly-folder")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, nameMatch.ID, nameMatch.Revision)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, contentMatch.ID, contentMatch.Revision)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: contentMatch.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "quarterly notes",
	}))

	hits, truncated, err := s.SearchPageWithOptions(
		ctx, "quarterly", 10, SearchOptions{MIMEType: "TEXT/PLAIN"},
	)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, hits, 3)
	assert.Equal(t, untaggedText.ID, hits[0].Node.ID)
	assert.Equal(t, nameMatch.ID, hits[1].Node.ID)
	assert.Equal(t, contentMatch.ID, hits[2].Node.ID)
	assert.Equal(t, SearchMatchContent, hits[2].Match)

	hits, _, err = s.SearchPageWithOptions(ctx, "quarterly", 10, SearchOptions{
		TagID: tag.ID, MIMEType: "text/plain",
	})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, nameMatch.ID, hits[0].Node.ID)
	assert.Equal(t, contentMatch.ID, hits[1].Node.ID)

	_, _, err = s.SearchPageWithOptions(
		ctx, "quarterly", 10, SearchOptions{MIMEType: "text/plain; charset=utf-8"},
	)
	require.ErrorContains(t, err, "must not include parameters")
	_, _, err = s.SearchPageWithOptions(
		ctx, "quarterly", 10, SearchOptions{MIMEType: "not a media type"},
	)
	require.ErrorContains(t, err, "is invalid")
	_, _, err = s.SearchPageWithOptions(
		ctx, "quarterly", 10, SearchOptions{MIMEType: "text/*"},
	)
	require.ErrorContains(t, err, "must not contain wildcards")
}

func TestSearchPageFiltersDescendantsByStableDirectory(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	scope, err := s.Mkdir(ctx, s.RootID(), "quarterly")
	require.NoError(t, err)
	nested, err := s.Mkdir(ctx, scope.ID, "2026")
	require.NoError(t, err)
	insidePDF, err := s.CreateFile(
		ctx, scope.ID, "quarterly-a.pdf", fakeHash("b8"), 4, "application/pdf",
	)
	require.NoError(t, err)
	insideText, err := s.CreateFile(
		ctx, nested.ID, "quarterly-b.txt", fakeHash("c9"), 4, "text/plain",
	)
	require.NoError(t, err)
	outside, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-c.pdf", fakeHash("da"), 4, "application/pdf",
	)
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "reviewed")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, insidePDF.ID, insidePDF.Revision)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, outside.ID, outside.Revision)
	require.NoError(t, err)

	hits, truncated, err := s.SearchPageWithOptions(
		ctx, "quarterly", 10, SearchOptions{UnderNodeID: scope.ID},
	)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, hits, 2)
	assert.ElementsMatch(t, []int64{insidePDF.ID, insideText.ID},
		[]int64{hits[0].Node.ID, hits[1].Node.ID})
	for _, hit := range hits {
		assert.NotEqual(t, scope.ID, hit.Node.ID, "the selected directory is not its own descendant")
	}

	hits, _, err = s.SearchPageWithOptions(ctx, "quarterly", 10, SearchOptions{
		TagID: tag.ID, MIMEType: "application/pdf", UnderNodeID: scope.ID,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, insidePDF.ID, hits[0].Node.ID)

	_, _, err = s.SearchPageWithOptions(
		ctx, "quarterly", 10, SearchOptions{UnderNodeID: insidePDF.ID},
	)
	require.ErrorIs(t, err, ErrNotDir)
	trashed, err := s.Mkdir(ctx, s.RootID(), "old-quarterly")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, trashed.ID, trashed.Revision)
	require.NoError(t, err)
	_, _, err = s.SearchPageWithOptions(
		ctx, "quarterly", 10, SearchOptions{UnderNodeID: trashed.ID},
	)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSearchPageFiltersByModificationTime(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	old, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-old.txt", fakeHash("eb"), 4, "text/plain",
	)
	require.NoError(t, err)
	nameMatch, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-current.txt", fakeHash("fc"), 4, "text/plain",
	)
	require.NoError(t, err)
	contentMatch, err := s.CreateFile(
		ctx, s.RootID(), "notes.txt", fakeHash("0d"), 4, "text/plain",
	)
	require.NoError(t, err)
	boundary, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-boundary.txt", fakeHash("1e"), 4, "text/plain",
	)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: contentMatch.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "quarterly notes",
	}))
	for id, stamp := range map[int64]string{
		old.ID:          "2026-01-01T00:00:00.000000000Z",
		nameMatch.ID:    "2026-01-02T00:00:00.000000000Z",
		contentMatch.ID: "2026-01-02T12:00:00.000000000Z",
		boundary.ID:     "2026-01-03T00:00:00.000000000Z",
	} {
		_, err = s.db.ExecContext(ctx, `UPDATE nodes SET modified_at=? WHERE id=?`, stamp, id)
		require.NoError(t, err)
	}

	hits, truncated, err := s.SearchPageWithOptions(ctx, "quarterly", 10, SearchOptions{
		ModifiedSince:  "2026-01-01T19:00:00-05:00",
		ModifiedBefore: "2026-01-03T00:00:00Z",
	})
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, hits, 2)
	assert.Equal(t, nameMatch.ID, hits[0].Node.ID)
	assert.Equal(t, SearchMatchName, hits[0].Match)
	assert.Equal(t, contentMatch.ID, hits[1].Node.ID)
	assert.Equal(t, SearchMatchContent, hits[1].Match)

	since, before, err := NormalizeSearchTimeBounds(
		"2026-01-01T19:00:00-05:00", "2026-01-03T01:00:00+01:00",
	)
	require.NoError(t, err)
	assert.Equal(t, "2026-01-02T00:00:00.000000000Z", since)
	assert.Equal(t, "2026-01-03T00:00:00.000000000Z", before)
	_, _, err = NormalizeSearchTimeBounds("yesterday", "")
	require.ErrorContains(t, err, "absolute RFC3339 timestamp")
	_, _, err = NormalizeSearchTimeBounds(
		"2026-01-03T00:00:00Z", "2026-01-03T00:00:00Z",
	)
	require.ErrorContains(t, err, "must be earlier")
}

func TestSearchContentFollowsStableNameMatches(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	nameMatch, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-forecast.md", fakeHash("a1"), 5, "text/markdown",
	)
	require.NoError(t, err)
	bodyMatch, err := s.CreateFile(
		ctx, s.RootID(), "notes.md", fakeHash("b2"), 5, "text/markdown; charset=utf-8",
	)
	require.NoError(t, err)
	unsupported, err := s.CreateFile(
		ctx, s.RootID(), "scan.pdf", bodyMatch.BlobHash, 5, "application/pdf",
	)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: nameMatch.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "unrelated body",
	}))
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: bodyMatch.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "quarterly forecast assumptions",
	}))

	hits, truncated, err := s.SearchPage(ctx, "quarterly", 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.False(t, truncated)
	assert.Equal(t, nameMatch.ID, hits[0].Node.ID)
	assert.Equal(t, SearchMatchName, hits[0].Match)
	assert.Equal(t, bodyMatch.ID, hits[1].Node.ID)
	assert.Equal(t, SearchMatchContent, hits[1].Match)
	assert.NotEqual(t, unsupported.ID, hits[1].Node.ID,
		"a shared blob does not make an unsupported current MIME searchable")

	// The same limit still returns the filename match first and truthfully
	// reports that a content match remains.
	hits, truncated, err = s.SearchPage(ctx, "quarterly", 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, nameMatch.ID, hits[0].Node.ID)
	assert.True(t, truncated)

	// Relabeling the current bytes with an unsupported MIME must revoke the
	// content match even though the immutable blob's derived text remains.
	_, _, err = s.ReplaceContent(
		ctx, bodyMatch.ID, bodyMatch.Revision, bodyMatch.BlobHash, bodyMatch.Size,
		"application/octet-stream",
	)
	require.NoError(t, err)
	hits, truncated, err = s.SearchPage(ctx, "quarterly", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.False(t, truncated)
	assert.Equal(t, nameMatch.ID, hits[0].Node.ID)
}

func TestSearchAttachmentEligibilityKeepsSharedBuildVersionScoped(t *testing.T) {
	// Mutation caught: joining lexical rows to content by source hash or build
	// alone would let one attachment confer search visibility on another version.
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "eligible mercury phrase")
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	generation, err := s.StageLexicalGeneration(ctx, fakeHash("91"))
	require.NoError(t, err)

	first := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, first, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: first.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z",
	}, generation.ID))

	hits, truncated, err := s.SearchPage(ctx, "mercury", 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, hits, 1)
	assert.Equal(t, versions[0], hits[0].Node.CurrentVersionID)
	assert.Equal(t, SearchMatchContent, hits[0].Match)

	secondProfile := catalogProcessingProfile(t, true)
	second := RenditionAttachmentRecord{
		ID: catalogAttachmentSecond, VaultID: s.VaultID(),
		ContentVersionID: versions[1], BuildID: build.ID, Profile: secondProfile,
		AttachedAt: "2026-08-22T10:02:00.000000000Z",
	}
	require.NoError(t, s.AttachRenditionBuild(ctx, second))
	hits, _, err = s.SearchPage(ctx, "mercury", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1, "a staged attachment without a head is not eligible")

	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, second, RenditionHeadRecord{
		ContentVersionID: versions[1], ProcessingProfileFingerprint: secondProfile.Fingerprint,
		AttachmentID: second.ID, PublishedAt: "2026-08-22T10:03:00.000000000Z",
	}, generation.ID))
	hits, _, err = s.SearchPageWithOptions(ctx, "mercury", 10, SearchOptions{MIMEType: "application/pdf"})
	require.NoError(t, err)
	require.Len(t, hits, 2, "independently headed attachments may share one vault-local build")
	assert.Equal(t, "synthetic-source-a.pdf", hits[0].Node.Name)
	assert.Equal(t, "synthetic-source-b.pdf", hits[1].Node.Name)

	secondNode, err := s.NodeByPath(ctx, "/synthetic-source-b.pdf")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, secondNode.ID, secondNode.Revision)
	require.NoError(t, err)
	hits, _, err = s.SearchPage(ctx, "mercury", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1, "a trashed attachment must not remain searchable")

	firstNode, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(
		ctx, firstNode.ID, firstNode.Revision, fakeHash("d9"), 7, "application/pdf",
	)
	require.NoError(t, err)
	hits, _, err = s.SearchPage(ctx, "mercury", 10)
	require.NoError(t, err)
	assert.Empty(t, hits, "an attachment to a historical source version must not serve")
}

func TestLexicalGenerationBuildFailureLeavesNoReadablePartialGeneration(t *testing.T) {
	// Mutation caught: committing FTS rows before marking the generation
	// complete would leave a failed generation available to a later head flip.
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstBuild := lexicalSearchBuild(s, profile, catalogBuildID, "prior mercury phrase")
	require.NoError(t, s.StageRenditionBuild(ctx, firstBuild))
	firstGeneration, err := s.StageLexicalGeneration(ctx, fakeHash("92"))
	require.NoError(t, err)
	firstAttachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: firstBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, firstAttachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: firstAttachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z",
	}, firstGeneration.ID))

	secondBuild, _ := lexicalSearchReplacementBuild(
		t, s, profile, fakeHash("b3"), "replacement venus phrase",
	)
	require.NoError(t, s.StageRenditionBuild(ctx, secondBuild))
	secondGenerationID := fakeHash("93")
	_, err = s.db.Exec(`
		CREATE TRIGGER fail_lexical_generation_catalog
		BEFORE INSERT ON rendition_lexical_generations
		WHEN new.generation_id = '` + secondGenerationID + `'
		BEGIN SELECT RAISE(ABORT, 'injected failure after FTS build'); END`)
	require.NoError(t, err)

	_, err = s.StageLexicalGeneration(ctx, secondGenerationID)
	require.ErrorContains(t, err, "injected failure after FTS build")
	var partialRows int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM rendition_lexical_fts WHERE build_id=?`, secondBuild.ID,
	).Scan(&partialRows))
	assert.Zero(t, partialRows)
	active, err := s.ActiveLexicalGeneration(ctx)
	require.NoError(t, err)
	assert.Equal(t, firstGeneration, active)
	hits, _, err := s.SearchPage(ctx, "mercury", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	hits, _, err = s.SearchPage(ctx, "venus", 10)
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func TestLexicalGenerationsShareImmutableBuildRows(t *testing.T) {
	// Mutation caught: storing FTS rows per generation copies every existing
	// build again whenever one new build extends the catalog.
	s, _ := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	first := lexicalSearchBuild(s, profile, fakeHash("c1"), "first shared lexical row")
	require.NoError(t, s.StageRenditionBuild(ctx, first))
	_, err := s.StageLexicalGeneration(ctx, fakeHash("c2"))
	require.NoError(t, err)

	second, _ := lexicalSearchReplacementBuild(
		t, s, profile, fakeHash("c3"), "second shared lexical row",
	)
	require.NoError(t, s.StageRenditionBuild(ctx, second))
	_, err = s.StageLexicalGeneration(ctx, fakeHash("c4"))
	require.NoError(t, err)

	var catalogRows, indexedRows int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM rendition_lexical_segments`,
	).Scan(&catalogRows))
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM rendition_lexical_fts`,
	).Scan(&indexedRows))
	assert.Equal(t, catalogRows, indexedRows,
		"immutable build text is indexed once and shared by generation membership")
}

func TestLexicalProjectionRebuildsGenerationKeyedCandidateCache(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, fakeHash("c5"), "candidate cache migration")
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	firstGenerationID := fakeHash("c6")
	_, err := s.StageLexicalGeneration(ctx, firstGenerationID)
	require.NoError(t, err)

	_, err = s.db.Exec(`DROP TABLE rendition_lexical_fts;
		CREATE VIRTUAL TABLE rendition_lexical_fts USING fts5(
			generation_id UNINDEXED,build_id UNINDEXED,segment_id UNINDEXED,text
		);
		INSERT INTO rendition_lexical_fts(generation_id,build_id,segment_id,text)
		SELECT ?,build_id,segment_id,text FROM rendition_lexical_segments`, firstGenerationID)
	require.NoError(t, err)

	_, err = s.StageLexicalGeneration(ctx, fakeHash("c7"))
	require.NoError(t, err)
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('rendition_lexical_fts') ORDER BY cid`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var columns []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"build_id", "segment_id", "text"}, columns)
	var indexedRows int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_lexical_fts`).Scan(&indexedRows))
	assert.Equal(t, 1, indexedRows)
}

func TestLexicalGenerationHeadFailureRollsBackAttachmentAndBothHeads(t *testing.T) {
	// Mutation caught: publishing the attachment or rendition head outside the
	// lexical-head transaction would replace the prior serving state on failure.
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstBuild := lexicalSearchBuild(s, profile, catalogBuildID, "prior mercury phrase")
	require.NoError(t, s.StageRenditionBuild(ctx, firstBuild))
	firstGeneration, err := s.StageLexicalGeneration(ctx, fakeHash("94"))
	require.NoError(t, err)
	firstAttachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: firstBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, firstAttachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: firstAttachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z",
	}, firstGeneration.ID))

	secondBuild, secondVersion := lexicalSearchReplacementBuild(
		t, s, profile, fakeHash("b4"), "replacement venus phrase",
	)
	require.NoError(t, s.StageRenditionBuild(ctx, secondBuild))
	secondGeneration, err := s.StageLexicalGeneration(ctx, fakeHash("95"))
	require.NoError(t, err)
	secondAttachment := RenditionAttachmentRecord{
		ID: fakeHash("53"), VaultID: s.VaultID(), ContentVersionID: secondVersion,
		BuildID: secondBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:02:00.000000000Z",
	}
	_, err = s.db.Exec(`
		CREATE TRIGGER fail_lexical_head_flip
		BEFORE UPDATE ON rendition_lexical_heads
		BEGIN SELECT RAISE(ABORT, 'injected failure before head flip'); END`)
	require.NoError(t, err)

	err = s.PublishRenditionAndLexicalHeads(ctx, secondAttachment, RenditionHeadRecord{
		ContentVersionID: secondVersion, ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: secondAttachment.ID, PublishedAt: "2026-08-22T10:03:00.000000000Z",
	}, secondGeneration.ID)
	require.ErrorContains(t, err, "injected failure before head flip")

	view, err := s.ActiveRendition(ctx, versions[0], profile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, firstAttachment.ID, view.Attachment.ID)
	active, err := s.ActiveLexicalGeneration(ctx)
	require.NoError(t, err)
	assert.Equal(t, firstGeneration, active)
	var attachments int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM rendition_attachments WHERE attachment_id=?`, secondAttachment.ID,
	).Scan(&attachments))
	assert.Zero(t, attachments)
	hits, _, err := s.SearchPage(ctx, "mercury", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	hits, _, err = s.SearchPage(ctx, "venus", 10)
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func TestLexicalGenerationRejectsOutOfOrderStagedSnapshot(t *testing.T) {
	// Two workers may stage snapshots before either publishes. An older snapshot
	// must not publish after a newer head because it would silently remove that
	// headed build from the active lexical generation.
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstBuild := lexicalSearchBuild(s, profile, fakeHash("a8"), "first mercury authority")
	require.NoError(t, s.StageRenditionBuild(ctx, firstBuild))
	olderGeneration, err := s.StageLexicalGeneration(ctx, fakeHash("a9"))
	require.NoError(t, err)

	secondBuild, secondVersion := lexicalSearchReplacementBuild(
		t, s, profile, fakeHash("aa"), "second venus authority",
	)
	require.NoError(t, s.StageRenditionBuild(ctx, secondBuild))
	newerGeneration, err := s.StageLexicalGeneration(ctx, fakeHash("ab"))
	require.NoError(t, err)
	secondAttachment := RenditionAttachmentRecord{
		ID: fakeHash("ac"), VaultID: s.VaultID(), ContentVersionID: secondVersion,
		BuildID: secondBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, secondAttachment, RenditionHeadRecord{
		ContentVersionID: secondVersion, ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: secondAttachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z",
	}, newerGeneration.ID))

	firstAttachment := RenditionAttachmentRecord{
		ID: fakeHash("ad"), VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: firstBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:02:00.000000000Z",
	}
	err = s.PublishRenditionAndLexicalHeads(ctx, firstAttachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: firstAttachment.ID, PublishedAt: "2026-08-22T10:03:00.000000000Z",
	}, olderGeneration.ID)
	require.ErrorContains(t, err, "current rendition head build")
	active, err := s.ActiveLexicalGeneration(ctx)
	require.NoError(t, err)
	assert.Equal(t, newerGeneration, active)
	_, err = s.ActiveRendition(ctx, versions[0], profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound, "the rejected publication rolls back its rendition head")
	hits, _, err := s.SearchPage(ctx, "venus", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
}

func TestLexicalGenerationReaderLeasePinsAndEnumeratesExactRoots(t *testing.T) {
	// Mutation caught: returning only the active generation value leaves no
	// acquire/release lifetime or enumerable root for generation garbage collection.
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstBuild := lexicalSearchBuild(s, profile, catalogBuildID, "prior mercury phrase")
	require.NoError(t, s.StageRenditionBuild(ctx, firstBuild))
	firstGeneration, err := s.StageLexicalGeneration(ctx, fakeHash("96"))
	require.NoError(t, err)
	firstAttachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: firstBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, firstAttachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: firstAttachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z",
	}, firstGeneration.ID))

	firstLease, err := s.AcquireLexicalGeneration(ctx)
	require.NoError(t, err)
	assert.Equal(t, firstGeneration, firstLease.Generation)
	assert.Equal(t, []LexicalGenerationRoot{{
		GenerationID: firstGeneration.ID, ReaderCount: 1,
	}}, s.LeasedLexicalGenerationRoots())

	secondBuild, secondVersion := lexicalSearchReplacementBuild(
		t, s, profile, fakeHash("b6"), "replacement venus phrase",
	)
	require.NoError(t, s.StageRenditionBuild(ctx, secondBuild))
	secondGeneration, err := s.StageLexicalGeneration(ctx, fakeHash("97"))
	require.NoError(t, err)
	secondAttachment := RenditionAttachmentRecord{
		ID: fakeHash("57"), VaultID: s.VaultID(), ContentVersionID: secondVersion,
		BuildID: secondBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:02:00.000000000Z",
	}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, secondAttachment, RenditionHeadRecord{
		ContentVersionID: secondVersion, ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: secondAttachment.ID, PublishedAt: "2026-08-22T10:03:00.000000000Z",
	}, secondGeneration.ID))
	assert.Equal(t, []LexicalGenerationRoot{{
		GenerationID: firstGeneration.ID, ReaderCount: 1,
	}}, s.LeasedLexicalGenerationRoots(), "the old generation remains rooted after the head flip")

	secondLease, err := s.AcquireLexicalGeneration(ctx)
	require.NoError(t, err)
	assert.Equal(t, secondGeneration, secondLease.Generation)
	assert.Equal(t, []LexicalGenerationRoot{
		{GenerationID: firstGeneration.ID, ReaderCount: 1},
		{GenerationID: secondGeneration.ID, ReaderCount: 1},
	}, s.LeasedLexicalGenerationRoots())

	require.NoError(t, firstLease.Release())
	assert.Equal(t, []LexicalGenerationRoot{{
		GenerationID: secondGeneration.ID, ReaderCount: 1,
	}}, s.LeasedLexicalGenerationRoots())
	require.NoError(t, firstLease.Release(), "release is idempotent")
	require.NoError(t, secondLease.Release())
	assert.Empty(t, s.LeasedLexicalGenerationRoots())
}

func TestLexicalGenerationReadSnapshotCannotMixPublicationEpochs(t *testing.T) {
	// Mutation caught: selecting a generation before starting the query's read
	// snapshot lets a concurrent head flip combine old FTS rows with new
	// rendition heads, returning the empty hybrid instead of either epoch.
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstBuild := lexicalSearchBuild(s, profile, catalogBuildID, "prior epoch phrase")
	require.NoError(t, s.StageRenditionBuild(ctx, firstBuild))
	firstGeneration, err := s.StageLexicalGeneration(ctx, fakeHash("9a"))
	require.NoError(t, err)
	firstAttachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: firstBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, firstAttachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: firstAttachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z",
	}, firstGeneration.ID))

	secondBuild := lexicalSearchBuild(s, profile, fakeHash("ba"), "replacement epoch phrase")
	secondPolicy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":0,"role":"provider_markdown"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	secondBuild.CapturedArtifactPolicy = secondPolicy
	secondBuild.CapturedArtifactPolicyFingerprint = testSHA256(secondPolicy)
	secondBuild.ProviderOperationID = "synthetic-operation-replacement"
	require.NoError(t, s.StageRenditionBuild(ctx, secondBuild))
	secondGeneration, err := s.StageLexicalGeneration(ctx, fakeHash("9b"))
	require.NoError(t, err)
	secondAttachment := RenditionAttachmentRecord{
		ID: catalogAttachmentSecond, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: secondBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:02:00.000000000Z",
	}

	var visibleBuildIDs []string
	err = s.withLexicalGenerationRead(ctx, func(
		queryer metadataQuerier, generation LexicalGeneration,
	) (retErr error) {
		assert.Equal(t, firstGeneration, generation)
		require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, secondAttachment, RenditionHeadRecord{
			ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
			AttachmentID: secondAttachment.ID, PublishedAt: "2026-08-22T10:03:00.000000000Z",
		}, secondGeneration.ID))
		assert.Equal(t, []LexicalGenerationRoot{{
			GenerationID: firstGeneration.ID, ReaderCount: 1,
		}}, s.LeasedLexicalGenerationRoots(), "the selected old generation remains externally rooted")

		rows, queryErr := queryer.QueryContext(ctx, `
			SELECT f.build_id
			FROM rendition_lexical_fts f
			JOIN rendition_lexical_generation_builds gb ON gb.build_id=f.build_id
			JOIN rendition_attachments a ON a.build_id=f.build_id
			JOIN rendition_heads rh
			  ON rh.content_version_id=a.content_version_id
			 AND rh.profile_fingerprint=a.profile_fingerprint
			 AND rh.attachment_id=a.attachment_id
			WHERE rendition_lexical_fts MATCH 'epoch'
			  AND gb.generation_id=?`, generation.ID)
		if queryErr != nil {
			return queryErr
		}
		defer func() {
			retErr = errors.Join(retErr, rows.Close())
		}()
		for rows.Next() {
			var buildID string
			if queryErr := rows.Scan(&buildID); queryErr != nil {
				return queryErr
			}
			visibleBuildIDs = append(visibleBuildIDs, buildID)
		}
		if queryErr := rows.Err(); queryErr != nil {
			return queryErr
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{firstBuild.ID}, visibleBuildIDs,
		"the read returns the complete old epoch; old FTS plus new heads would be empty")
	assert.Empty(t, s.LeasedLexicalGenerationRoots(), "the lease releases after row consumption")
	active, err := s.ActiveLexicalGeneration(ctx)
	require.NoError(t, err)
	assert.Equal(t, secondGeneration, active)
}

func TestLexicalGenerationReuseRejectsSameCountContentSubstitution(t *testing.T) {
	// Mutation caught: validating only row counts lets a substituted FTS row
	// masquerade as an already-complete immutable generation.
	s, _ := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "canonical mercury phrase")
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	generationID := fakeHash("98")
	_, err := s.StageLexicalGeneration(ctx, generationID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `
		UPDATE rendition_lexical_fts SET text='substituted venus phrase'
		WHERE build_id=?`, build.ID)
	require.NoError(t, err)

	_, err = s.StageLexicalGeneration(ctx, generationID)
	require.ErrorContains(t, err, "immutable manifest")
}

func TestLexicalGenerationPublicationRejectsSameCountContentSubstitution(t *testing.T) {
	// Mutation caught: target-build count equality alone allows substituted FTS
	// content to become serving authority during the atomic head flip.
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "canonical mercury phrase")
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	generationID := fakeHash("99")
	_, err := s.StageLexicalGeneration(ctx, generationID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `
		UPDATE rendition_lexical_fts SET text='substituted venus phrase'
		WHERE build_id=?`, build.ID)
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}

	err = s.PublishRenditionAndLexicalHeads(ctx, attachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z",
	}, generationID)
	require.ErrorContains(t, err, "immutable manifest")
	_, err = s.ActiveLexicalGeneration(ctx)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLexicalGenerationPublicationRejectsMissingZeroSegmentBuildMembership(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	generation, err := s.StageLexicalGeneration(ctx, fakeHash("9a"))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.Units = nil
	build.LexicalSegments = nil
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:04:00.000000000Z",
	}

	err = s.PublishRenditionAndLexicalHeads(ctx, attachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: "2026-08-22T10:05:00.000000000Z",
	}, generation.ID)
	require.ErrorContains(t, err, "does not exactly contain build")
	_, err = s.ActiveLexicalGeneration(ctx)
	require.ErrorIs(t, err, ErrNotFound)
}

func lexicalSearchBuild(
	s *Store, profile ProcessingProfileRecord, buildID, text string,
) RenditionBuildRecord {
	build := catalogRenditionBuild(s, profile)
	build.ID = buildID
	build.LexicalSegments[0].Text = text
	build.LexicalSegments[0].CharEnd = len([]rune(text))
	build.LexicalSegments[0].Checksum = testSHA256([]byte(text))
	return build
}

func lexicalSearchReplacementBuild(
	t *testing.T, s *Store, profile ProcessingProfileRecord, buildID, text string,
) (RenditionBuildRecord, string) {
	t.Helper()
	sourceHash := fakeHash(buildID[:2] + "d8")
	node, err := s.CreateFile(
		t.Context(), s.RootID(), "replacement-"+buildID[:8]+".pdf",
		sourceHash, 7, "application/pdf",
	)
	require.NoError(t, err)
	build := lexicalSearchBuild(s, profile, buildID, text)
	build.SourceSHA256 = sourceHash
	return build, node.CurrentVersionID
}

func TestPendingAndFailedTextExtractions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	textNode, err := s.CreateFile(
		ctx, s.RootID(), "notes.txt", fakeHash("a1"), 10, "text/plain; charset=utf-8",
	)
	require.NoError(t, err)
	jsonNode, err := s.CreateFile(
		ctx, s.RootID(), "session.jsonl", fakeHash("b2"), 20, "application/x-ndjson",
	)
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "scan.pdf", fakeHash("c3"), 30, "application/pdf")
	require.NoError(t, err)
	oldTextHash := textNode.BlobHash
	textNode, _, err = s.ReplaceContent(
		ctx, textNode.ID, textNode.Revision, fakeHash("a0"), 12, "text/plain",
	)
	require.NoError(t, err)

	_, err = s.db.Exec(`DELETE FROM text_extraction_queue`)
	require.NoError(t, err)
	pending, err := s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	require.NoError(t, s.SeedTextExtractionQueue(ctx, "plain-text", 1))

	pending, err = s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.ElementsMatch(t, []string{textNode.BlobHash, jsonNode.BlobHash},
		[]string{pending[0].BlobHash, pending[1].BlobHash})
	assert.NotEqual(t, oldTextHash, textNode.BlobHash,
		"startup discovery should seed selected versions, not retained history")

	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: textNode.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionFailed, Error: "not valid UTF-8",
	}))
	pending, err = s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, jsonNode.BlobHash, pending[0].BlobHash)

	// A future extractor implementation naturally retries the old result.
	require.NoError(t, s.SeedTextExtractionQueue(ctx, "plain-text", 2))
	pending, err = s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, pending, 2)
}

func TestPendingTextExtractionsSkipsSupersededQueuedContent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(
		ctx, s.RootID(), "notes.txt", fakeHash("d1"), 10, "text/plain",
	)
	require.NoError(t, err)
	current, _, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("d2"), 12, "text/plain",
	)
	require.NoError(t, err)

	var queued int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM text_extraction_queue`,
	).Scan(&queued))
	assert.Equal(t, 2, queued, "the stale queue hint should exercise dequeue validation")

	pending, err := s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, current.BlobHash, pending[0].BlobHash)
	assert.NotEqual(t, created.BlobHash, pending[0].BlobHash)
}

func TestTextExtractionQueueDefersFailuresBehindReadyWork(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hashes := make([]string, 65)
	for i := range hashes {
		hashes[i] = fakeHash(fmt.Sprintf("%02x", i+1))
		_, err := s.CreateFile(
			ctx, s.RootID(), fmt.Sprintf("item-%02d.txt", i+1),
			hashes[i], 1, "text/plain",
		)
		require.NoError(t, err)
	}
	notBefore := time.Now().UTC().Add(time.Hour)
	for _, hash := range hashes[:64] {
		require.NoError(t, s.DeferTextExtraction(ctx, hash, notBefore))
	}

	pending, err := s.PendingTextExtractions(ctx, 64)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, hashes[64], pending[0].BlobHash,
		"deferred failures must not starve later ready work")
}
