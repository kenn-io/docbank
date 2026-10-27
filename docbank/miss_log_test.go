package docbank

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
	"uuid"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// Receipt assertions use an idle vault; contention is exercised explicitly below.
func stopMissLogBackgroundWrites(vault *Vault) {
	vault.processingCancel()
	vault.processingWG.Wait()
}

func TestEmbeddedMissLogDoesNotWaitForBusyWrite(t *testing.T) {
	t.Parallel()
	for _, maintenance := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary write", true: "maintenance"}[maintenance], func(t *testing.T) {
			vault, err := New(t.Context(), Config{Root: t.TempDir(), MissLog: MissLogOptions{Enabled: true}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			stopMissLogBackgroundWrites(vault)
			id, err := uuid.Parse(vault.ID())
			require.NoError(t, err)
			args, err := json.Marshal(map[string]any{"query": "synthetic busy miss", "profile": "supplied-transcript", "mode": "lexical", "filters": map[string]any{}})
			require.NoError(t, err)
			if maintenance {
				vault.preservation.Lock()
				defer vault.preservation.Unlock()
			}
			vault.mutation.Lock()
			defer vault.mutation.Unlock()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			type outcome struct {
				result *sdkmcp.CallToolResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := vault.CallReadTool(ctx, id, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "search_documents", Arguments: args}})
				done <- outcome{result, err}
			}()
			select {
			case result := <-done:
				require.NoError(t, result.err)
				require.False(t, result.result.IsError)
			case <-ctx.Done():
				t.Fatal("an embedded search must not wait for best-effort logging")
			}
		})
	}
}

func TestEmbeddedMissLogUsesOwnedVaultAndEmptyScope(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		vault, err := New(t.Context(), Config{Root: t.TempDir(), MissLog: MissLogOptions{Enabled: enabled}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, vault.Close()) })
		stopMissLogBackgroundWrites(vault)
		id, err := uuid.Parse(vault.ID())
		require.NoError(t, err)
		args, err := json.Marshal(map[string]any{"query": "synthetic embedded miss", "profile": "supplied-transcript", "mode": "lexical", "filters": map[string]any{}})
		require.NoError(t, err)
		result, err := vault.CallReadTool(t.Context(), id, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "search_documents", Arguments: args}})
		require.NoError(t, err)
		require.False(t, result.IsError, result)
		rows, err := vault.metadata.ListMissLogRecords(t.Context(), 10, 0)
		require.NoError(t, err)
		if enabled {
			require.Len(t, rows, 1)
			require.Contains(t, rows[0].Receipt, "synthetic embedded miss")
		} else {
			require.Empty(t, rows)
		}
		require.NoError(t, vault.Close())
	}
}

func TestEmbeddedMissLogDirectDocumentSearch(t *testing.T) {
	t.Parallel()
	vault, err := New(t.Context(), Config{Root: t.TempDir(), MissLog: MissLogOptions{Enabled: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	stopMissLogBackgroundWrites(vault)
	receipt, err := vault.Put(t.Context(), "/synthetic.txt", strings.NewReader("ordinary fixture content"), PutOptions{})
	require.NoError(t, err)
	report, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{Query: "unfindablequestionzz", Mode: DocumentSearchLexical, Profile: "supplied-transcript", Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}})
	require.NoError(t, err)
	require.Empty(t, report.Results)
	require.NotEmpty(t, report.MissLogID)
	records, err := vault.metadata.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Contains(t, records[0].Receipt, "unfindablequestionzz")
}
