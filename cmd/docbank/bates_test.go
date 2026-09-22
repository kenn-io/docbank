package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestBatesNamespacesCLIListsAndCreatesViaDaemon(t *testing.T) {
	_ = setupVaultHome(t)
	created, err := runCLI(t, "bates", "namespaces", "--create", "--prefix", "OUR", "--padding", "6", "--json")
	require.NoError(t, err)
	var namespace api.BatesNamespace
	require.NoError(t, json.Unmarshal([]byte(created), &namespace))
	require.Equal(t, "OUR", namespace.Prefix)
	require.Equal(t, 6, namespace.Padding)
	listed, err := runCLI(t, "bates", "namespaces", "--json")
	require.NoError(t, err)
	var page api.BatesNamespacePage
	require.NoError(t, json.Unmarshal([]byte(listed), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, namespace.NamespaceID, page.Items[0].NamespaceID)
}

func TestBatesPlanCLIRequiresNamespaceAndRecipe(t *testing.T) {
	_, err := runCLI(t, "bates", "plan", "snapshot")
	require.ErrorContains(t, err, "--namespace is required")
	_, err = runCLI(t, "bates", "plan", "snapshot", "--namespace", "namespace")
	require.ErrorContains(t, err, "--recipe-sha256 is required")
}

func TestBatesExportRunRequiresRecipe(t *testing.T) {
	_, err := runCLI(t, "bates", "export", "run", "11111111-1111-4111-8111-111111111111")
	require.ErrorContains(t, err, "--recipe is required")
}
