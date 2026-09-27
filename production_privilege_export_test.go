package docbank

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/productiontest"
)

func TestEmbeddedPrivilegeExportReturnsOnlyFrozenPublicRows(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	productiontest.SeedFrozenPrivilegeLog(t, vault.metadata)
	const logID = "13131313-1313-4313-8313-131313131313"
	exported, err := vault.ExportProductionPrivilegeLog(t.Context(), logID, 1, "csv")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(exported.Content), "id,"))
	require.Contains(t, string(exported.Content), "Synthetic public description.")
	require.NotContains(t, string(exported.Content), "Synthetic private rationale.")
	_, err = vault.ExportProductionPrivilegeLog(t.Context(), logID, 2, "csv")
	require.Error(t, err)
}
