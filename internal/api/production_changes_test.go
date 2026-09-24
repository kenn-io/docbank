package api

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestProductionChangeWireCoversDomainFields(t *testing.T) {
	member := redaction.Member{ID: "synthetic-member"}
	decision := redaction.Decision{ID: "synthetic-decision"}
	wire := ProductionChange{Kind: "member", MemberID: "synthetic-member",
		Member: (*ProductionMember)(&member), Decision: (*ProductionDecision)(&decision),
		DecisionID: "synthetic-decision", Mode: "redact", RecipeID: "recipe",
		ProfileID: "profile", DisclosureProfileID: "disclosure", NumberingRecipeID: "numbering",
		PolicyID: "policy", PolicyVersion: 2}
	want := redaction.Change{Kind: wire.Kind, MemberID: wire.MemberID, Member: &member,
		Decision: &decision, DecisionID: wire.DecisionID, Mode: wire.Mode,
		RecipeID: wire.RecipeID, ProfileID: wire.ProfileID, DisclosureProfileID: wire.DisclosureProfileID,
		NumberingRecipeID: wire.NumberingRecipeID, PolicyID: wire.PolicyID, PolicyVersion: wire.PolicyVersion}
	require.Equal(t, want, wire.domain())
	request := ProductionChangesRequest{OperationID: "synthetic-operation", Changes: []ProductionChange{wire}}
	require.Equal(t, redaction.ApplyRequest{OperationID: request.OperationID, ETag: 7,
		Changes: []redaction.Change{want}}, request.Domain(7))
}
