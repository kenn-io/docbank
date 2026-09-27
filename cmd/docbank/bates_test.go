package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
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

func TestBatesPlanCLIRequiresNamespace(t *testing.T) {
	_, err := runCLI(t, "bates", "plan", "snapshot")
	require.ErrorContains(t, err, "--namespace is required")
}

func TestBatesReserveCLIRequiresRecipe(t *testing.T) {
	_, err := runCLI(t, "bates", "reserve", "22222222-2222-4222-8222-222222222222")
	require.ErrorContains(t, err, "--recipe is required")
}

func TestBatesExportRunRequiresRecipe(t *testing.T) {
	_, err := runCLI(t, "bates", "export", "run", "11111111-1111-4111-8111-111111111111")
	require.ErrorContains(t, err, "--recipe is required")
}

func TestBatesPlanRecipeRoundTripsAndNeverOverwrites(t *testing.T) {
	plan := api.BatesPlan{StartSequence: 41, Namespace: api.BatesNamespace{
		NamespaceID: "11111111-1111-4111-8111-111111111111", Prefix: "OUR", Padding: 6}}
	path := filepath.Join(t.TempDir(), "recipe.json")
	recipe := batesRecipeForPlan(plan, "bottom-right", 24)
	require.NoError(t, writeBatesRecipe(path, recipe))
	read, err := readBatesRecipe(path)
	require.NoError(t, err)
	require.Equal(t, recipe, read)
	require.Equal(t, 41, read.StartAt, "the recipe must pin the previewed first number")
	require.ErrorIs(t, writeBatesRecipe(path, recipe), os.ErrExist)
}
