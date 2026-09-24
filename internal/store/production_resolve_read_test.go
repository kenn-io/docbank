package store

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestProductionResolvedMaskPagePinsReviewAndCursorScope(t *testing.T) {
	s, _, set, draft, member := ProductionReviewHTTPFixture(t)
	binding := ProductionReviewBindingHTTPFixture(t, s, set.ID, member.ID)
	first, err := s.ProductionResolvedMaskPage(t.Context(), set.ID, 1, member.ID, draft.ETag, 1, "", 1)
	require.NoError(t, err)
	require.Equal(t, binding, first.ReviewBinding)
	require.Equal(t, member.MapSHA256, first.MapSHA256)
	require.Equal(t, draft.RecipeSHA256, first.RecipeSHA256)
	require.Equal(t, 1, first.Page.Number)
	require.Greater(t, first.TotalBoxes, 1)
	require.Len(t, first.Items, 1)
	require.NotEmpty(t, first.NextCursor)
	seen := map[redaction.Box]struct{}{first.Items[0]: {}}
	cursor := first.NextCursor
	for cursor != "" {
		page, err := s.ProductionResolvedMaskPage(t.Context(), set.ID, 1, member.ID, draft.ETag, 1, cursor, 1)
		require.NoError(t, err)
		require.Equal(t, binding, page.ReviewBinding)
		require.Equal(t, first.ResolvedSHA256, page.ResolvedSHA256)
		require.Len(t, page.Items, 1)
		_, duplicate := seen[page.Items[0]]
		require.False(t, duplicate)
		seen[page.Items[0]] = struct{}{}
		require.NotEqual(t, cursor, page.NextCursor)
		cursor = page.NextCursor
	}
	require.Len(t, seen, first.TotalBoxes)
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "synthetic relevance")
	_, err = s.ProductionResolvedMaskPage(t.Context(), set.ID, 1, member.ID, draft.ETag+1, 1, "", 1)
	require.ErrorIs(t, err, ErrProductionRevisionConflict)
	_, err = s.ProductionResolvedMaskPage(t.Context(), set.ID, 1, member.ID, draft.ETag, 2, first.NextCursor, 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.ProductionResolvedMaskPage(t.Context(), set.ID, 1, member.ID, draft.ETag, 1, "bad cursor", 1)
	require.ErrorIs(t, err, ErrInvalidProduction)
	_, err = s.ProductionResolvedMaskPage(t.Context(), set.ID, 1, member.ID, draft.ETag, 1, "", 201)
	require.ErrorIs(t, err, ErrInvalidProduction)
	_, err = s.ProductionResolvedMaskPage(t.Context(), set.ID, 1,
		"89000000-0000-4000-8000-000000000099", draft.ETag, 1, "", 1)
	require.ErrorIs(t, err, ErrNotFound)
}
