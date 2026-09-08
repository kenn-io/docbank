package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchSourceFenceRestrictsOrdinaryAndExplainedNameMatches(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateFile(t.Context(), s.RootID(), "mercury mercury first.txt",
		fakeHash("fence-first"), 1, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(t.Context(), s.RootID(), "mercury-second.txt",
		fakeHash("fence-second"), 1, "text/plain")
	require.NoError(t, err)

	unfenced, truncated, err := s.SearchPageWithOptions(t.Context(), "mercury", 2, SearchOptions{})
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []int64{first.ID, second.ID}, searchHitNodeIDs(unfenced))
	unfencedExplained, truncated, err := s.SearchExplainedLexicalCandidates(
		t.Context(), "mercury", 2, SearchOptions{})
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []int64{first.ID, second.ID}, explainedCandidateNodeIDs(unfencedExplained))

	opts := SearchOptions{ContentVersionIDs: []string{second.CurrentVersionID}}
	hits, truncated, err := s.SearchPageWithOptions(t.Context(), "mercury", 1, opts)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []int64{second.ID}, searchHitNodeIDs(hits))
	explained, truncated, err := s.SearchExplainedLexicalCandidates(t.Context(), "mercury", 1, opts)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []int64{second.ID}, explainedCandidateNodeIDs(explained))

	all := SearchOptions{ContentVersionIDs: []string{second.CurrentVersionID, first.CurrentVersionID}}
	hits, truncated, err = s.SearchPageWithOptions(t.Context(), "mercury", 1, all)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Equal(t, []int64{first.ID}, searchHitNodeIDs(hits))
	explained, truncated, err = s.SearchExplainedLexicalCandidates(t.Context(), "mercury", 1, all)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Equal(t, []int64{first.ID}, explainedCandidateNodeIDs(explained))
}

func TestSearchSourceFenceNormalizationOwnsCanonicalBoundedIDs(t *testing.T) {
	s := newTestStore(t)
	first := "00000000-0000-4000-8000-000000000001"
	second := "00000000-0000-4000-8000-000000000002"
	input := []string{second, first}

	normalized, err := s.normalizeSearchOptions(t.Context(), SearchOptions{ContentVersionIDs: input})
	require.NoError(t, err)
	require.Equal(t, []string{first, second}, normalized.ContentVersionIDs)
	require.Equal(t, []string{second, first}, input)
	input[0] = "00000000-0000-4000-8000-000000000003"
	require.Equal(t, []string{first, second}, normalized.ContentVersionIDs)

	nilOptions, err := s.normalizeSearchOptions(t.Context(), SearchOptions{})
	require.NoError(t, err)
	require.Nil(t, nilOptions.ContentVersionIDs)
	empty := make([]string, 0)
	emptyOptions, err := s.normalizeSearchOptions(
		t.Context(), SearchOptions{ContentVersionIDs: empty})
	require.NoError(t, err)
	require.Empty(t, emptyOptions.ContentVersionIDs)
	require.NotNil(t, emptyOptions.ContentVersionIDs)

	ids := make([]string, MaxSearchSourceFenceIDs)
	for index := range ids {
		ids[index] = fmt.Sprintf("00000000-0000-4000-8000-%012x", index)
	}
	bounded, err := s.normalizeSearchOptions(t.Context(), SearchOptions{ContentVersionIDs: ids})
	require.NoError(t, err)
	require.Len(t, bounded.ContentVersionIDs, MaxSearchSourceFenceIDs)
	hits, truncated, err := s.SearchPageWithOptions(t.Context(), "mercury", 10,
		SearchOptions{ContentVersionIDs: ids})
	require.NoError(t, err)
	require.False(t, truncated)
	require.Empty(t, hits)
	tooMany := append(append([]string(nil), ids...),
		fmt.Sprintf("00000000-0000-4000-8000-%012x", MaxSearchSourceFenceIDs))
	_, err = s.normalizeSearchOptions(t.Context(), SearchOptions{ContentVersionIDs: tooMany})
	require.EqualError(t, err, "search source fence exceeds 4096 content versions")
	_, _, err = s.SearchPageWithOptions(t.Context(), "mercury", 10,
		SearchOptions{ContentVersionIDs: tooMany})
	require.EqualError(t, err, "search source fence exceeds 4096 content versions")
	_, _, err = s.SearchExplainedLexicalCandidates(t.Context(), "mercury", 10,
		SearchOptions{ContentVersionIDs: tooMany})
	require.EqualError(t, err, "search source fence exceeds 4096 content versions")
}

func TestSearchSourceFenceRejectsNoncanonicalAndDuplicateIDs(t *testing.T) {
	s := newTestStore(t)
	tests := []struct {
		name string
		ids  []string
		want string
	}{
		{name: "malformed", ids: []string{"not-a-uuid"}, want: "invalid content version ID"},
		{name: "uppercase", ids: []string{"00000000-0000-4000-8000-00000000000A"}, want: "invalid content version ID"},
		{name: "wrong version", ids: []string{"00000000-0000-5000-8000-000000000001"}, want: "invalid content version ID"},
		{name: "wrong variant", ids: []string{"00000000-0000-4000-7000-000000000001"}, want: "invalid content version ID"},
		{name: "duplicate", ids: []string{
			"00000000-0000-4000-8000-000000000001",
			"00000000-0000-4000-8000-000000000001",
		}, want: "duplicate content version ID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := s.normalizeSearchOptions(
				t.Context(), SearchOptions{ContentVersionIDs: test.ids})
			require.ErrorContains(t, err, test.want)
			_, _, err = s.SearchPageWithOptions(t.Context(), "mercury", 10,
				SearchOptions{ContentVersionIDs: test.ids})
			require.ErrorContains(t, err, test.want)
			_, _, err = s.SearchExplainedLexicalCandidates(t.Context(), "mercury", 10,
				SearchOptions{ContentVersionIDs: test.ids})
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestSearchSourceFenceDoesNotCreateQuerylessModeOrFallbackWhenUnmatched(t *testing.T) {
	s := newTestStore(t)
	file, err := s.CreateFile(t.Context(), s.RootID(), "mercury.txt",
		fakeHash("fence-unmatched"), 1, "text/plain")
	require.NoError(t, err)
	missing := "00000000-0000-4000-8000-000000000001"
	opts := SearchOptions{ContentVersionIDs: []string{missing}}

	_, _, err = s.SearchPageWithOptions(t.Context(), "", 10, opts)
	require.ErrorIs(t, err, ErrSearchQueryRequired)
	_, _, err = s.SearchExplainedLexicalCandidates(t.Context(), "", 10, opts)
	require.ErrorIs(t, err, ErrSearchQueryRequired)
	hits, truncated, err := s.SearchPageWithOptions(t.Context(), "mercury", 10, opts)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Empty(t, hits)
	explained, truncated, err := s.SearchExplainedLexicalCandidates(
		t.Context(), "mercury", 10, opts)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Empty(t, explained)
	require.NotEqual(t, missing, file.CurrentVersionID)
}

func TestSearchSourceFenceComposesWithCurrentFilters(t *testing.T) {
	s := newTestStore(t)
	inside, err := s.Mkdir(t.Context(), s.RootID(), "inside")
	require.NoError(t, err)
	outside, err := s.Mkdir(t.Context(), s.RootID(), "outside")
	require.NoError(t, err)
	selected, err := s.CreateTag(t.Context(), "selected")
	require.NoError(t, err)
	otherTag, err := s.CreateTag(t.Context(), "other")
	require.NoError(t, err)
	outsideWinner, err := s.CreateFile(t.Context(), outside.ID, "mercury mercury mercury.txt",
		fakeHash("fence-outside"), 1, "text/plain")
	require.NoError(t, err)
	target, err := s.CreateFile(t.Context(), inside.ID, "mercury-target.txt",
		fakeHash("fence-target"), 1, "text/plain")
	require.NoError(t, err)
	newerTarget, err := s.CreateFile(t.Context(), inside.ID, "newer-target.txt",
		fakeHash("fence-newer-target"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.AssignTag(t.Context(), selected.ID, target.ID, target.Revision)
	require.NoError(t, err)
	_, err = s.AssignTag(t.Context(), selected.ID, newerTarget.ID, newerTarget.Revision)
	require.NoError(t, err)
	_, err = s.AssignTag(t.Context(), otherTag.ID, outsideWinner.ID, outsideWinner.Revision)
	require.NoError(t, err)
	for _, node := range []Node{outsideWinner, target} {
		_, err = s.db.ExecContext(t.Context(), `UPDATE nodes SET modified_at=? WHERE id=?`,
			"2026-01-02T00:00:00.000000000Z", node.ID)
		require.NoError(t, err)
	}
	_, err = s.db.ExecContext(t.Context(), `UPDATE nodes SET modified_at=? WHERE id=?`,
		"2026-01-03T00:00:00.000000000Z", newerTarget.ID)
	require.NoError(t, err)

	opts := SearchOptions{ContentVersionIDs: []string{target.CurrentVersionID},
		TagID: selected.ID, MIMEType: "text/plain", UnderNodeID: inside.ID,
		ModifiedSince: "2026-01-02T00:00:00Z", ModifiedBefore: "2026-01-03T00:00:00Z"}
	requireSourceFenceSearchIDs(t, s, "mercury", opts, []int64{target.ID})

	tests := []struct {
		name   string
		mutate func(*SearchOptions)
	}{
		{name: "tag", mutate: func(opts *SearchOptions) { opts.TagID = otherTag.ID }},
		{name: "MIME", mutate: func(opts *SearchOptions) { opts.MIMEType = "application/pdf" }},
		{name: "directory", mutate: func(opts *SearchOptions) { opts.UnderNodeID = outside.ID }},
		{name: "half-open time", mutate: func(opts *SearchOptions) {
			opts.ModifiedSince = ""
			opts.ModifiedBefore = "2026-01-02T00:00:00Z"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filtered := opts
			test.mutate(&filtered)
			requireSourceFenceSearchIDs(t, s, "mercury", filtered, nil)
		})
	}

	filterIDs := []string{target.CurrentVersionID, newerTarget.CurrentVersionID}
	filterOnly, truncated, err := s.SearchPageWithOptions(t.Context(), "", 10, SearchOptions{
		ContentVersionIDs: filterIDs, TagID: selected.ID,
	})
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []int64{newerTarget.ID, target.ID}, searchHitNodeIDs(filterOnly))
	require.Equal(t, []string{"/inside/newer-target.txt", "/inside/mercury-target.txt"},
		[]string{filterOnly[0].Path, filterOnly[1].Path})
	require.Equal(t, []string{SearchMatchFilter, SearchMatchFilter},
		[]string{filterOnly[0].Match, filterOnly[1].Match})
	filterOnly, truncated, err = s.SearchPageWithOptions(t.Context(), "", 10, SearchOptions{
		ContentVersionIDs: filterIDs,
		ModifiedSince:     "2026-01-02T00:00:00Z",
	})
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []int64{newerTarget.ID, target.ID}, searchHitNodeIDs(filterOnly))
	require.Equal(t, []string{"/inside/newer-target.txt", "/inside/mercury-target.txt"},
		[]string{filterOnly[0].Path, filterOnly[1].Path})
}

func TestSearchSourceFenceRestrictsActiveRenditionContentAndPreservesOrdering(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "mercury source fence content")
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	generation, err := s.StageLexicalGeneration(t.Context(), hashVectorIndexTest("source-fence-lexical"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), attachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: embeddingCatalogTime}, generation.ID))
	contentNode := nodeForVersion(t, s, versions[0])
	nameNode, err := s.CreateFile(t.Context(), s.RootID(), "mercury-name.pdf",
		fakeHash("fence-name"), 1, "application/pdf")
	require.NoError(t, err)

	opts := SearchOptions{ContentVersionIDs: []string{versions[0], nameNode.CurrentVersionID}}
	hits, truncated, err := s.SearchPageWithOptions(t.Context(), "mercury", 2, opts)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []int64{nameNode.ID, contentNode.ID}, searchHitNodeIDs(hits))
	require.Equal(t, []string{SearchMatchName, SearchMatchContent}, []string{hits[0].Match, hits[1].Match})
	explained, truncated, err := s.SearchExplainedLexicalCandidates(t.Context(), "mercury", 2, opts)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []int64{nameNode.ID, contentNode.ID}, explainedCandidateNodeIDs(explained))
	require.Equal(t, "rendition_segment", explained[1].EvidenceKind)
	require.Equal(t, build.ID, explained[1].BuildID)
	require.Equal(t, build.LexicalSegments[0].ID, explained[1].SegmentID)

	hits, truncated, err = s.SearchPageWithOptions(t.Context(), "mercury", 1, opts)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Equal(t, []int64{nameNode.ID}, searchHitNodeIDs(hits))
	explained, truncated, err = s.SearchExplainedLexicalCandidates(t.Context(), "mercury", 1, opts)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Equal(t, []int64{nameNode.ID}, explainedCandidateNodeIDs(explained))

	requireSourceFenceSearchIDs(t, s, "mercury",
		SearchOptions{ContentVersionIDs: []string{versions[1]}}, nil)
}

func requireSourceFenceSearchIDs(
	t *testing.T, s *Store, query string, opts SearchOptions, expected []int64,
) {
	t.Helper()
	hits, truncated, err := s.SearchPageWithOptions(t.Context(), query, 10, opts)
	require.NoError(t, err)
	require.False(t, truncated)
	if expected == nil {
		require.Empty(t, hits)
	} else {
		require.Equal(t, expected, searchHitNodeIDs(hits))
	}
	explained, truncated, err := s.SearchExplainedLexicalCandidates(t.Context(), query, 10, opts)
	require.NoError(t, err)
	require.False(t, truncated)
	if expected == nil {
		require.Empty(t, explained)
	} else {
		require.Equal(t, expected, explainedCandidateNodeIDs(explained))
	}
}

func explainedCandidateNodeIDs(candidates []ExplainedLexicalCandidate) []int64 {
	result := make([]int64, len(candidates))
	for index := range candidates {
		result[index] = candidates[index].Node.ID
	}
	return result
}
