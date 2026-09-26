package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestAssessProductionDecisionDoesNotMutateAndDistinguishesExpansionFromConflict(t *testing.T) {
	s, _, seededSet, seededDraft, member, _ := ProductionPreviewStageFixture(t)
	set, draft, err := s.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
		OperationID: "89000000-0000-4000-8000-000000000053", Name: "Synthetic decision check"})
	require.NoError(t, err)
	_, err = s.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, draft.Revision,
		redaction.ApplyRequest{OperationID: "89000000-0000-4000-8000-000000000054", ETag: draft.ETag,
			Changes: []redaction.Change{{Kind: "member", Member: &member}}})
	require.NoError(t, err)
	draft, err = s.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	frame := loadProductionInputsForTest(t, s, set.ID, draft.Revision).Members[0].Resolved.Pages[0]
	makeDecision := func(id, action string, selector redaction.Selector) redaction.Decision {
		return redaction.Decision{ID: id, MemberID: member.ID, Action: action,
			Reason: "synthetic reason", Selector: selector}
	}
	page := makeDecision("89000000-0000-4000-8000-000000000045", "redact", redaction.Selector{
		Kind: "page", MapSHA256: member.MapSHA256, Pages: []int{1}})
	problem, err := s.AssessProductionDecision(t.Context(), set.ID, draft.Revision, draft.ETag, page)
	require.NoError(t, err)
	require.Nil(t, problem)

	rectangle := makeDecision("89000000-0000-4000-8000-000000000046", "redact", redaction.Selector{
		Kind: "rectangle", MapSHA256: member.MapSHA256,
		Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame.FrameSHA256, X0: 1000, Y0: 2000, X1: 5000, Y1: 6000}},
	})
	problem, err = s.AssessProductionDecision(t.Context(), set.ID, draft.Revision, draft.ETag, rectangle)
	require.NoError(t, err)
	require.NotNil(t, problem)
	require.Equal(t, "selection_expansion_required", problem.Code)
	require.Equal(t, member.MapSHA256, problem.ExpandedMapSHA256)
	require.NotNil(t, problem.Expanded)
	require.Equal(t, "rectangle", problem.Expanded.Kind)
	require.NotEmpty(t, problem.Expanded.Boxes)
	require.LessOrEqual(t, problem.Expanded.Boxes[0].X0, rectangle.Selector.Boxes[0].X0)
	require.GreaterOrEqual(t, problem.Expanded.Boxes[0].X1, rectangle.Selector.Boxes[0].X1)
	rectangle.Selector = *problem.Expanded
	problem, err = s.AssessProductionDecision(t.Context(), set.ID, draft.Revision, draft.ETag, rectangle)
	require.NoError(t, err)
	require.Nil(t, problem, "the accepted expansion must resolve without changing the draft")

	keep := makeDecision("89000000-0000-4000-8000-000000000047", "keep", redaction.Selector{
		Kind: "page", MapSHA256: member.MapSHA256, Pages: []int{1}})
	problem, err = s.AssessProductionDecision(t.Context(), seededSet.ID, seededDraft.Revision, seededDraft.ETag, keep)
	require.NoError(t, err)
	require.NotNil(t, problem)
	require.Equal(t, "decision_conflict", problem.Code)
	require.Contains(t, problem.DecisionIDs, keep.ID)

	fresh, err := s.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	require.Equal(t, draft.ETag, fresh.ETag, "assessment must leave the draft unchanged")
}

func TestAssessProductionDecisionRejectsStaleDraftAndMap(t *testing.T) {
	s, _, set, draft, member, _ := ProductionPreviewStageFixture(t)
	decision := redaction.Decision{ID: "89000000-0000-4000-8000-000000000048", MemberID: member.ID,
		Action: "redact", Reason: "synthetic reason", Selector: redaction.Selector{
			Kind: "page", MapSHA256: member.MapSHA256, Pages: []int{1}}}
	_, err := s.AssessProductionDecision(t.Context(), set.ID, draft.Revision, draft.ETag+1, decision)
	require.ErrorIs(t, err, ErrProductionRevisionConflict)
	decision.Selector.MapSHA256 = fakeHash("f4")
	_, err = s.AssessProductionDecision(t.Context(), set.ID, draft.Revision, draft.ETag, decision)
	require.ErrorIs(t, err, ErrInvalidProduction)
}

func TestAssessProductionDecisionReplacesExistingIdentity(t *testing.T) {
	s, _, set, draft, member, _ := ProductionPreviewStageFixture(t)
	replacement := redaction.Decision{ID: "89000000-0000-4000-8000-000000000043", MemberID: member.ID,
		Action: "keep", Selector: redaction.Selector{
			Kind: "text", MapSHA256: member.MapSHA256, Span: &redaction.Span{Start: 0, End: 1}}}
	problem, err := s.AssessProductionDecision(t.Context(), set.ID, draft.Revision, draft.ETag, replacement)
	require.NoError(t, err)
	require.Nil(t, problem, "same decision ID must be assessed as a replacement")
	fresh, err := s.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	require.Equal(t, draft.ETag, fresh.ETag)
}
