package store

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestEvaluationPassageFocusUsesNativeTokensAndRollsBack(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	s.db.SetMaxOpenConns(1)
	for _, tc := range []struct {
		query, text string
		span        [2]int
	}{
		{"lantern?", "clantern 界 lantern archive", [2]int{11, 18}},
		{"lant? missing?", "clantern 界 lantern archive", [2]int{11, 18}},
		{"lantern? archive?", "clantern 界 archive lantern", [2]int{11, 18}},
		{"cafe?", "界 café", [2]int{2, 6}},
		{"foo-bar?", "foo other foo-barrel", [2]int{10, 20}},
		{"lantern?", "\x01\x01界 lantern", [2]int{4, 11}},
		{"lantern?", "\x01\x02\x01\x01\x02界 lantern", [2]int{7, 14}},
		{"lantern?", "\x01lantern trailing", [2]int{1, 8}},
		{"界", "\x01" + strings.Repeat("界", 512) + " trailing", [2]int{1, 513}},
		{"missing?", "lantern", [2]int{}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			focus, err := s.EvaluationPassageFocus(t.Context(), tc.query, []string{tc.text, "unrelated"})
			require.NoError(t, err)
			assert.Equal(t, [][2]int{tc.span, {}}, focus)
			start, end := embeddingeval.BoundedPassageWindow(tc.text, focus[0][0], focus[0][1], 1536)
			require.Contains(t, string([]rune(tc.text)[start:end]), string([]rune(tc.text)[tc.span[0]:tc.span[1]]))
		})
	}
	_, err := s.EvaluationPassageFocus(t.Context(), "lantern", []string{string([]byte{0xff})})
	require.ErrorContains(t, err, "UTF-8")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.EvaluationPassageFocus(ctx, "lantern", []string{"lantern"})
	require.ErrorIs(t, err, context.Canceled)
	var temporary int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_temp_schema WHERE name LIKE 'evaluation_passage_focus%'`).Scan(&temporary))
	require.Zero(t, temporary, "the leased connection must retain no temporary projection")
}

func TestEvaluationSemanticPassagesRetainEligibleNeighbors(t *testing.T) {
	t.Parallel()
	eligible := map[semanticEligibilityKey]SemanticSearchCandidate{
		{"set", "best", "sum"}:  {NodeID: 2, InputKind: document.EmbeddingInputRenditionChunk},
		{"set", "next", "sum"}:  {NodeID: 2, InputKind: document.EmbeddingInputRenditionChunk},
		{"set", "other", "sum"}: {NodeID: 3, InputKind: document.EmbeddingInputRenditionChunk},
	}
	neighbors := []vectorindex.Neighbor{{SetID: "set", InputKey: "stale", InputChecksum: "sum", Score: 1}, {SetID: "set", InputKey: "best", InputChecksum: "sum", Score: .9}, {SetID: "set", InputKey: "other", InputChecksum: "sum", Score: .8}, {SetID: "set", InputKey: "next", InputChecksum: "sum", Score: .7}}
	got := evaluationSemanticReducer(map[int64]struct{}{2: {}}, 3)
	passages, _, _ := got("vault", "space", neighbors, 10, eligible, nil)
	require.Len(t, passages, 2)
	require.Equal(t, "best", passages[0].InputID)
	require.Equal(t, "next", passages[1].InputID)
	one, _, _ := evaluationSemanticReducer(map[int64]struct{}{2: {}}, 1)("vault", "space", neighbors, 10, eligible, nil)
	baseline, _, _ := reduceSemanticCandidates("vault", "space", neighbors, 10, eligible, map[int64]struct{}{3: {}})
	require.Equal(t, baseline, one)
}

func TestEvaluationLexicalPassagesUsesCurrentRenditionAndDisclosesCap(t *testing.T) {
	t.Parallel()
	s, _, nodes, profile := collectionCoverageFixture(t, 2)
	for _, node := range nodes {
		collectionCoveragePublish(t, s, node, profile, "complete")
	}
	ids := []string{nodes[0].CurrentVersionID, nodes[1].CurrentVersionID}
	fences := []EvaluationLexicalFence{{ContentVersionID: ids[0], BuildID: testSHA256([]byte(ids[0] + "complete"))}, {ContentVersionID: ids[1], BuildID: testSHA256([]byte(ids[1] + "complete"))}}
	passages, truncated, err := s.SearchEvaluationLexicalPassages(t.Context(), "synthetic report", fences, 1)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, passages, 1)
	require.Contains(t, ids, passages[0].ContentVersionID)
	require.Equal(t, "Synthetic report", passages[0].Text)
	require.NotEmpty(t, passages[0].BuildID)
	require.NotEmpty(t, passages[0].SegmentID)
	passages, truncated, err = s.SearchEvaluationLexicalPassages(t.Context(), "synthetic report", fences[:1], 10)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Len(t, passages, 1)
	require.Equal(t, ids[0], passages[0].ContentVersionID)
	fence := []EvaluationLexicalFence{{ContentVersionID: nodes[0].CurrentVersionID, BuildID: testSHA256([]byte("other build"))}}
	passages, _, err = s.SearchEvaluationLexicalPassages(t.Context(), "synthetic report", fence, 10)
	require.NoError(t, err)
	require.Empty(t, passages)
	collectionCoveragePublish(t, s, nodes[0], catalogProcessingProfile(t, true), "second")
	fences = []EvaluationLexicalFence{fences[0], {ContentVersionID: ids[0], BuildID: testSHA256([]byte(ids[0] + "second"))}}
	passages, truncated, err = s.SearchEvaluationLexicalPassages(t.Context(), "synthetic report", fences, 10)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Len(t, passages, 2)
	require.ElementsMatch(t, []string{fences[0].BuildID, fences[1].BuildID}, []string{passages[0].BuildID, passages[1].BuildID})
}

func TestEvaluationLexicalFenceRevalidationDetectsStaleHead(t *testing.T) {
	t.Parallel()
	s, _, nodes, profile := collectionCoverageFixture(t, 1)
	collectionCoveragePublish(t, s, nodes[0], profile, "complete")
	fence := EvaluationLexicalFence{ContentVersionID: nodes[0].CurrentVersionID, BuildID: testSHA256([]byte(nodes[0].CurrentVersionID + "complete"))}
	require.NoError(t, s.RevalidateEvaluationLexicalFences(t.Context(), []EvaluationLexicalFence{fence}))
	collectionCoveragePublish(t, s, nodes[0], catalogProcessingProfile(t, true), "second")
	fences := []EvaluationLexicalFence{fence, {ContentVersionID: nodes[0].CurrentVersionID, BuildID: testSHA256([]byte(nodes[0].CurrentVersionID + "second"))}}
	require.NoError(t, s.RevalidateEvaluationLexicalFences(t.Context(), fences))
	fences[1].BuildID = testSHA256([]byte("unpublished build"))
	require.ErrorContains(t, s.RevalidateEvaluationLexicalFences(t.Context(), fences), "stale")
	_, _, err := s.ReplaceContent(t.Context(), nodes[0].ID, nodes[0].Revision, testSHA256([]byte("replacement source")), 20, "application/pdf")
	require.NoError(t, err)
	require.Error(t, s.RevalidateEvaluationLexicalFences(t.Context(), []EvaluationLexicalFence{fence}))
}
