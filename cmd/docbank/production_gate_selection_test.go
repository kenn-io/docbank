package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionGateSelectionCLIReachesDaemonAndValidatesReferences(t *testing.T) {
	_ = setupVaultHome(t)
	createdJSON, err := runCLI(t, "production", "sets", "create", "--name", "Synthetic gate selection", "--json")
	require.NoError(t, err)
	var created api.ProductionSetCreated
	require.NoError(t, json.Unmarshal([]byte(createdJSON), &created))
	_, err = runCLI(t, "production", "drafts", "select-gates", created.Set.ID, "1", "--json")
	require.ErrorContains(t, err, "invalid_production")
	_, err = runCLI(t, "production", "drafts", "select-gates", created.Set.ID, "1",
		"--privilege-log-id", "7a000000-0000-4000-8000-000000000011")
	require.ErrorContains(t, err, "--privilege-log-id and --privilege-log-revision must be supplied together")
}
