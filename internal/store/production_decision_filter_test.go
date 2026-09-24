package store

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestProductionDecisionPagesFilterUncertaintyAndScopeCursors(t *testing.T) {
	s, _, set, draft, member := ProductionReviewHTTPFixture(t)
	changes := make([]redaction.Change, 0, 2)
	for index := 10; index < 12; index++ {
		decision := productionAuthorityDecision(member, fmt.Sprintf("89000000-0000-4000-8000-%012d", index))
		decision.Uncertain = true
		changes = append(changes, redaction.Change{Kind: "decision", Decision: &decision})
	}
	_, err := s.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, draft.Revision,
		redaction.ApplyRequest{OperationID: "89000000-0000-4000-8000-000000000020",
			ETag: draft.ETag, Changes: changes})
	require.NoError(t, err)
	uncertain := true
	first, next, err := s.ProductionDecisionsFiltered(t.Context(), set.ID, draft.Revision, "", 1, &uncertain)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.True(t, first[0].Uncertain)
	require.NotEmpty(t, next)
	second, end, err := s.ProductionDecisionsFiltered(t.Context(), set.ID, draft.Revision, next, 1, &uncertain)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.True(t, second[0].Uncertain)
	require.Less(t, first[0].ID, second[0].ID)
	require.Empty(t, end)
	definite := false
	ordinary, _, err := s.ProductionDecisionsFiltered(t.Context(), set.ID, draft.Revision, "", 500, &definite)
	require.NoError(t, err)
	require.Len(t, ordinary, 1)
	require.False(t, ordinary[0].Uncertain)
	_, _, err = s.ProductionDecisionsFiltered(t.Context(), set.ID, draft.Revision, next, 1, &definite)
	require.ErrorIs(t, err, ErrInvalidProduction)
	_, _, err = s.ProductionDecisionsFiltered(t.Context(), set.ID, draft.Revision, next, 1, nil)
	require.ErrorIs(t, err, ErrInvalidProduction)
	_, _, err = s.ProductionDecisionsFiltered(t.Context(), set.ID, draft.Revision, "", 501, nil)
	require.ErrorIs(t, err, ErrInvalidProduction)
}

func TestProductionDecisionPagesStayWithinJSONBudget(t *testing.T) {
	s, _, set, draft, member := ProductionReviewHTTPFixture(t)
	for batch := range 2 {
		changes := make([]redaction.Change, 0, 175)
		for index := range 175 {
			decision := productionAuthorityDecision(member,
				fmt.Sprintf("89000000-0000-4000-8000-%012d", 1000+batch*175+index))
			decision.Reason = strings.Repeat("synthetic ", 340)
			changes = append(changes, redaction.Change{Kind: "decision", Decision: &decision})
		}
		_, err := s.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, draft.Revision,
			redaction.ApplyRequest{OperationID: fmt.Sprintf("89000000-0000-4000-8000-%012d", 30+batch),
				ETag: draft.ETag, Changes: changes})
		require.NoError(t, err)
		draft.ETag++
	}
	all := make(map[string]struct{})
	cursor := ""
	for {
		page, next, err := s.ProductionDecisions(t.Context(), set.ID, draft.Revision, cursor, 500)
		require.NoError(t, err)
		raw, err := json.Marshal(struct {
			Items      []redaction.Decision `json:"items"`
			NextCursor string               `json:"next_cursor"`
		}{Items: page, NextCursor: next})
		require.NoError(t, err)
		require.LessOrEqual(t, len(raw), 1<<20)
		for _, decision := range page {
			_, exists := all[decision.ID]
			require.False(t, exists)
			all[decision.ID] = struct{}{}
		}
		if next == "" {
			break
		}
		require.NotEqual(t, cursor, next)
		cursor = next
	}
	require.Len(t, all, 351)
}
