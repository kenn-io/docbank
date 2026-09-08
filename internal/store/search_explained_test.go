package store

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	candidates, truncated, err := s.SearchExplainedLexicalCandidates(
		t.Context(), "mercury", 10, SearchOptions{})

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
	directory, err := s.Mkdir(t.Context(), s.RootID(), "alpha-folder")
	require.NoError(t, err)
	file, err := s.CreateFile(t.Context(), docs.ID, "alpha.pdf", fakeHash("alpha"), 1, "application/pdf")
	require.NoError(t, err)

	ordinary, _, err := s.SearchPageWithOptions(t.Context(), "alpha", 10, SearchOptions{})
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{directory.ID, file.ID}, searchHitNodeIDs(ordinary))

	candidates, truncated, err := s.SearchExplainedLexicalCandidates(
		t.Context(), "alpha", 10, SearchOptions{})

	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, candidates, 1)
	assert.Equal(t, file.ID, candidates[0].Node.ID)
	assert.Equal(t, "/docs/alpha.pdf", candidates[0].Path)
	assert.Equal(t, SearchMatchName, candidates[0].Match)
	assert.Equal(t, "node_name", candidates[0].EvidenceKind)
	assert.Equal(t, "alpha.pdf", candidates[0].Excerpt)
}

func TestSearchExplainedLexicalCandidatesMatchesOrdinaryFileOrderAndTruncation(t *testing.T) {
	s, generation := newExplainedSearchParityFixture(t)
	opts := SearchOptions{MIMEType: "application/pdf"}

	for _, limit := range []int{1, 2, 3} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			ordinary, ordinaryTruncated, err := s.SearchPageWithOptions(
				t.Context(), "mercury", limit, opts)
			require.NoError(t, err)
			explained, explainedTruncated, err := s.SearchExplainedLexicalCandidates(
				t.Context(), "mercury", limit, opts)
			require.NoError(t, err)
			require.Equal(t, ordinaryTruncated, explainedTruncated)
			require.Len(t, explained, len(ordinary))
			for index := range ordinary {
				require.Equal(t, ordinary[index].Node.ID, explained[index].Node.ID)
			}
			for _, candidate := range explained {
				if candidate.EvidenceKind != "rendition_segment" {
					continue
				}
				var belongs bool
				require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT EXISTS(
					SELECT 1 FROM rendition_lexical_generation_builds gb
					JOIN rendition_lexical_index i ON i.build_id=gb.build_id
					WHERE gb.generation_id=? AND i.build_id=? AND i.segment_id=?)`,
					generation.ID, candidate.BuildID, candidate.SegmentID).Scan(&belongs))
				assert.True(t, belongs, "explained evidence must belong to the captured generation")
			}
		})
	}
}

func TestSearchExplainedLexicalCandidatesBoundsUnicodeExcerptAndRemovesMarkers(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	text := strings.Repeat("界", 700) + " mercury " + strings.Repeat("語", 700)
	build := lexicalSearchBuild(s, profile, catalogBuildID, text)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	generation, err := s.StageLexicalGeneration(t.Context(), hashVectorIndexTest("e9-unicode"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: embeddingCatalogTime}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), attachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: embeddingCatalogTime}, generation.ID))

	candidates, _, err := s.SearchExplainedLexicalCandidates(
		t.Context(), "mercury", 10, SearchOptions{})

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Contains(t, candidates[0].Excerpt, "mercury")
	assert.LessOrEqual(t, len([]rune(candidates[0].Excerpt)), 512)
	assert.NotContains(t, candidates[0].Excerpt, string(rune(1)))
	assert.NotContains(t, candidates[0].Excerpt, string(rune(2)))
}

func TestSearchExplainedLexicalCandidatesRequiresTextualQueryAfterOptionValidation(t *testing.T) {
	s := newTestStore(t)

	_, _, err := s.SearchExplainedLexicalCandidates(
		t.Context(), "", 10, SearchOptions{MIMEType: "not a media type"})
	require.ErrorContains(t, err, "search MIME type")

	_, _, err = s.SearchExplainedLexicalCandidates(
		t.Context(), " ", 10, SearchOptions{ModifiedSince: "2026-09-01T00:00:00Z"})
	require.ErrorIs(t, err, ErrSearchQueryRequired)
}

func TestSearchExplainedLexicalEvidenceCannotMixWithConcurrentPurge(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "mercury snapshot evidence")
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	generation, err := s.StageLexicalGeneration(ctx, hashVectorIndexTest("e9-purge"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: embeddingCatalogTime}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, attachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: embeddingCatalogTime}, generation.ID))

	var candidates []ExplainedLexicalCandidate
	err = s.withLexicalGenerationRead(ctx, func(queryer metadataQuerier, captured LexicalGeneration) error {
		require.Equal(t, generation.ID, captured.ID)
		purged := make(chan error, 1)
		go func() {
			_, purgeErr := s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{versions[0]}})
			purged <- purgeErr
		}()
		require.NoError(t, <-purged)

		var queryErr error
		candidates, queryErr = s.queryExplainedContentCandidates(ctx, queryer, captured.ID,
			ftsQuery("mercury"), "mercury", 10, "", nil, nil)
		return queryErr
	})

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, versions[0], candidates[0].Node.CurrentVersionID)
	assert.Equal(t, build.ID, candidates[0].BuildID)
	assert.Equal(t, build.LexicalSegments[0].ID, candidates[0].SegmentID)
	assert.Empty(t, s.LeasedLexicalGenerationRoots())
}

func searchHitNodeIDs(hits []SearchHit) []int64 {
	result := make([]int64, len(hits))
	for index := range hits {
		result[index] = hits[index].Node.ID
	}
	return result
}

func newExplainedSearchParityFixture(t *testing.T) (*Store, LexicalGeneration) {
	t.Helper()
	s := newTestStore(t)
	profile := catalogProcessingProfile(t, false)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		if err := s.EnsureBlobTx(tx, catalogEvidenceBlobHash,
			int64(len(catalogBlobContents[catalogEvidenceBlobHash]))); err != nil {
			return err
		}
		return s.EnsureBlobTx(tx, catalogMarkdownBlobHash,
			int64(len(catalogBlobContents[catalogMarkdownBlobHash])))
	}))
	type fixtureDocument struct {
		name  string
		seed  string
		texts []string
	}
	documents := []fixtureDocument{
		{name: "mercury-alpha.pdf", seed: "e9a", texts: []string{"mercury alpha content"}},
		{name: "mercury-beta.pdf", seed: "e9b", texts: []string{"mercury beta content"}},
		{name: "gamma.pdf", seed: "e9c", texts: []string{"mercury gamma first", "mercury gamma second"}},
	}
	type publication struct {
		node       Node
		build      RenditionBuildRecord
		attachment RenditionAttachmentRecord
	}
	publications := make([]publication, 0, len(documents))
	for index, document := range documents {
		node, err := s.CreateFile(t.Context(), s.RootID(), document.name,
			fakeHash(document.seed), int64(index+1), "application/pdf")
		require.NoError(t, err)
		build := explainedSearchBuild(s, profile, fakeHash(document.seed+"b"),
			node.BlobHash, document.seed, document.texts)
		require.NoError(t, s.StageRenditionBuild(t.Context(), build))
		publications = append(publications, publication{node: node, build: build,
			attachment: RenditionAttachmentRecord{ID: fakeHash(document.seed + "a"), VaultID: s.VaultID(),
				ContentVersionID: node.CurrentVersionID, BuildID: build.ID, Profile: profile,
				AttachedAt: embeddingCatalogTime}})
	}
	generation, err := s.StageLexicalGeneration(t.Context(), hashVectorIndexTest("e9-parity"))
	require.NoError(t, err)
	for _, publication := range publications {
		require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), publication.attachment,
			RenditionHeadRecord{ContentVersionID: publication.node.CurrentVersionID,
				ProcessingProfileFingerprint: profile.Fingerprint,
				AttachmentID:                 publication.attachment.ID, PublishedAt: embeddingCatalogTime}, generation.ID))
	}
	return s, generation
}

func explainedSearchBuild(s *Store, profile ProcessingProfileRecord, buildID, sourceHash, seed string,
	texts []string,
) RenditionBuildRecord {
	build := catalogRenditionBuild(s, profile)
	build.ID = buildID
	build.SourceSHA256 = sourceHash
	build.ProviderOperationID = "synthetic-operation-" + seed
	build.LexicalSegments = make([]RenditionLexicalSegmentRecord, len(texts))
	for index, text := range texts {
		build.LexicalSegments[index] = RenditionLexicalSegmentRecord{
			ID:     "lexical_segment_" + fakeHash(seed+string(rune('a'+index))),
			UnitID: build.Units[0].ID, Order: index, CharEnd: len([]rune(text)),
			Checksum: testSHA256([]byte(text)), Text: text,
		}
	}
	return build
}
