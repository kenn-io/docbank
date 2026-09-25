package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestMigrationCLIUsesDaemon(t *testing.T) {
	setupVaultHome(t)
	out, err := runCLI(t, "photos", "migrate", "runs", "list", "--limit", "1")
	require.NoError(t, err)
	var page api.MigrationRunPage
	require.NoError(t, json.Unmarshal([]byte(out), &page))
	require.Equal(t, 0, page.Total)
	require.Empty(t, page.Items)
}
