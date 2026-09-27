package main

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/productiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPrivilegeCLIReadsFrozenPublicPagesFromRealDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	catalog, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	productiontest.SeedFrozenPrivilegeLog(t, catalog)
	require.NoError(t, catalog.Close())
	startTestDaemon(t, dir)

	const logID = "13131313-1313-4313-8313-131313131313"
	out, err := runCLI(t, "production", "privilege-log", "show", logID, "1", "--limit", "1", "--json")
	require.NoError(t, err)
	var first api.ProductionPrivilegePublicPage
	require.NoError(t, json.Unmarshal([]byte(out), &first))
	require.Equal(t, logID, first.Receipt.LogID)
	require.Len(t, first.Rows, 1)
	require.Equal(t, "1", first.NextCursor)
	require.NotContains(t, out, "Synthetic private rationale.")
	require.NotContains(t, out, "person_ids")
	out, err = runCLI(t, "production", "privilege-log", "show", logID, "1",
		"--limit", "1", "--cursor", first.NextCursor, "--json")
	require.NoError(t, err)
	var second api.ProductionPrivilegePublicPage
	require.NoError(t, json.Unmarshal([]byte(out), &second))
	require.Len(t, second.Rows, 1)
	require.Equal(t, "Second synthetic public description.", second.Rows[0].PublicDescription)
	require.Empty(t, second.NextCursor)
	_, err = runCLI(t, "production", "privilege-log", "show", logID, "1", "--limit", "101")
	require.Error(t, err)
}
