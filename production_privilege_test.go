package docbank

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/productiontest"
)

func TestEmbeddedPrivilegeValidationUsesStoredAuthority(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	draft := productiontest.SeedPrivilegeLogDraft(t, vault.metadata)
	request := api.ProductionPrivilegeValidationRequest{
		OperationID:        "edededed-eded-4ded-8ded-edededededed",
		ExpectedGeneration: draft.Generation, ValidatedAt: "2026-09-22T14:00:00Z",
	}
	validated, err := vault.ValidateProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision, request)
	require.NoError(t, err)
	require.Equal(t, draft.Generation, validated.DraftGeneration)
	require.Equal(t, draft.LogID, validated.Validation.Inputs.LogID)
	replay, err := vault.ValidateProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision, request)
	require.NoError(t, err)
	require.Equal(t, validated, replay)
}

func TestEmbeddedPrivilegeFreezeRechecksStoredAuthority(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	draft := productiontest.SeedPrivilegeLogDraft(t, vault.metadata)
	validation, err := vault.ValidateProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision,
		api.ProductionPrivilegeValidationRequest{
			OperationID:        "edededed-eded-4ded-8ded-edededededed",
			ExpectedGeneration: draft.Generation, ValidatedAt: "2026-09-22T14:00:00Z",
		})
	require.NoError(t, err)
	request := api.ProductionPrivilegeFreezeRequest{
		OperationID:        "fefefefe-fefe-4fef-8fef-fefefefefefe",
		ExpectedGeneration: draft.Generation, ExpectedInputsSHA256: validation.Validation.InputsSHA256,
		FrozenAt: "2026-09-22T14:01:00Z",
	}
	invalidTime := request
	invalidTime.FrozenAt = "2026-09-22T14:01:00+01:00"
	_, err = vault.FreezeProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision, invalidTime)
	var problem *documentproduction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, documentproduction.ProblemInvalidContract, problem.Code)
	stale := request
	stale.OperationID = "01010101-0101-4101-8101-010101010101"
	stale.ExpectedInputsSHA256 = strings.Repeat("0", 64)
	_, err = vault.FreezeProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision, stale)
	require.ErrorAs(t, err, &problem)
	require.Equal(t, documentproduction.ProblemPrivilegeLogStale, problem.Code)
	receipt, err := vault.FreezeProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision, request)
	require.NoError(t, err)
	require.Equal(t, validation.Validation.InputsSHA256, receipt.InputsSHA256)
	replay, err := vault.FreezeProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision, request)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
	public, err := vault.ProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision, "", 25)
	require.NoError(t, err)
	require.Equal(t, receipt.SHA256, public.Receipt.SHA256)
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Synthetic private rationale.")
	request.FrozenAt = "2026-09-22T14:02:00Z"
	_, err = vault.FreezeProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision, request)
	require.Error(t, err, "a changed replay must conflict")
}
