package redaction

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateCreateRequestRejectsClientAuthorityAndBoundsInstructions(t *testing.T) {
	valid := CreateRequest{
		OperationID:  "11111111-1111-4111-8111-111111111111",
		Name:         "Synthetic review",
		Instructions: "Keep budget discussion.",
	}
	require.NoError(t, ValidateCreateRequest(valid))

	invalidOperation := valid
	invalidOperation.OperationID = "11111111-1111-4111-A111-111111111111"
	require.Error(t, ValidateCreateRequest(invalidOperation))

	oversized := valid
	oversized.Instructions = strings.Repeat("x", MaxInstructionsBytes+1)
	require.Error(t, ValidateCreateRequest(oversized))

	invalidProfile := valid
	invalidProfile.ProfileID = "../../caller-controlled"
	require.Error(t, ValidateCreateRequest(invalidProfile))
}

func TestProductionCommandJSONUsesStableSnakeCaseAndOmitsInactiveUnionFields(t *testing.T) {
	raw, err := json.Marshal(Change{Kind: "recipe", RecipeID: RecipeID600DPI})
	require.NoError(t, err)
	require.JSONEq(t, `{"kind":"recipe","recipe_id":"raster-redaction/v1-600dpi"}`, string(raw))

	raw, err = json.Marshal(MembershipSealRequest{
		OperationID: "55555555-5555-4555-8555-555555555555", ETag: 7, Total: 2,
		MemberHash: strings.Repeat("a", 64),
	})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"operation_id"`)
	require.Contains(t, string(raw), `"member_hash"`)
}

func TestValidateApplyRequestBoundsAtomicBatch(t *testing.T) {
	valid := ApplyRequest{OperationID: "22222222-2222-4222-8222-222222222222", ETag: 1,
		Changes: []Change{{Kind: "recipe", RecipeID: RecipeID300DPI}}}
	require.NoError(t, ValidateApplyRequest(valid))

	tooMany := valid
	tooMany.Changes = make([]Change, MaxChangesPerBatch+1)
	for i := range tooMany.Changes {
		tooMany.Changes[i] = Change{Kind: "recipe", RecipeID: RecipeID300DPI}
	}
	require.Error(t, ValidateApplyRequest(tooMany))
}

func TestValidateInstructionsEditRequestBoundsText(t *testing.T) {
	require.NoError(t, ValidateInstructionsEditRequest(InstructionsEditRequest{
		OperationID: "55555555-5555-4555-8555-555555555555", ETag: 1,
		Instructions: "Retain synthetic relevant text.",
	}))
	require.Error(t, ValidateInstructionsEditRequest(InstructionsEditRequest{
		OperationID: "55555555-5555-4555-8555-555555555555", ETag: 1,
		Instructions: strings.Repeat("x", MaxInstructionsBytes+1),
	}))
}
