package docbank

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
	"uuid"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedSearchDoesNotRecordMissLog(t *testing.T) {
	t.Parallel()
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	stored, err := vault.Put(t.Context(), "/synthetic.txt", strings.NewReader("ordinary fixture content"), PutOptions{})
	require.NoError(t, err)

	report, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{
		Query: "unfindablequestionzz", Mode: DocumentSearchLexical, Profile: "supplied-transcript",
		Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{stored.Version.ID}},
	})
	require.NoError(t, err)
	require.Empty(t, report.Results)

	vaultID, err := uuid.Parse(vault.ID())
	require.NoError(t, err)
	arguments, err := json.Marshal(map[string]any{
		"query": "synthetic embedded miss", "profile": "supplied-transcript", "mode": "lexical", "filters": map[string]any{},
	})
	require.NoError(t, err)
	result, err := vault.CallReadTool(t.Context(), vaultID, &sdkmcp.CallToolRequest{
		Params: &sdkmcp.CallToolParamsRaw{Name: "search_documents", Arguments: arguments},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, result)
	require.NotContains(t, result.StructuredContent, "miss_log_id")

	rows, err := vault.metadata.ListMissLogRecords(t.Context(), time.Time{}, 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}
