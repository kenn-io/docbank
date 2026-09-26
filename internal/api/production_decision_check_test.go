package api

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestProductionDecisionCheckResultBoundsExpansionBeforeSerialization(t *testing.T) {
	decision := redaction.Decision{ID: "89000000-0000-4000-8000-000000000061",
		MemberID: "89000000-0000-4000-8000-000000000041"}
	boxes := make([]redaction.Box, 65)
	for index := range boxes {
		boxes[index] = redaction.Box{Page: 1, X0: int64(index), X1: int64(index + 1)}
	}
	problem := &redaction.Problem{Code: "selection_expansion_required", Expanded: &redaction.Selector{Kind: "rectangle", Boxes: boxes}, ExpandedBoxes: boxes}
	result := productionDecisionCheckResult("89000000-0000-4000-8000-000000000060", 1, 7, decision, problem)
	require.Equal(t, "selection_expansion_required", result.Outcome)
	require.Equal(t, 65, result.ExpandedBoxCount)
	require.Nil(t, result.Expanded, "oversized expanded geometry must not enter the response")

	problem.Expanded.Boxes = boxes[:1]
	problem.ExpandedBoxes = boxes[:1]
	result = productionDecisionCheckResult("89000000-0000-4000-8000-000000000060", 1, 7, decision, problem)
	require.Equal(t, 1, result.ExpandedBoxCount)
	require.Equal(t, boxes[:1], result.Expanded.Boxes)
}
