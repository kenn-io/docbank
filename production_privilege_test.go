package docbank

import (
	"testing"

	"github.com/stretchr/testify/require"
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
