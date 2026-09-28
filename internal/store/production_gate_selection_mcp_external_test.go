package store_test

import (
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

// This opt-in proof uses MCP stdio, a real daemon, and synthetic human authority.
func TestProductionMCPRealDaemonSelectsRequiredApprovalBeforeFinalization(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_GATE_MCP_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon MCP production gate selection proof")
	}
	vault, root, setID, revision, etag, namespaceID, approvalID := store.ProductionRequiredApprovalHTTPFixture(t)
	require.NoError(t, vault.Close())
	name := "docbank"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", binary,
		"go.kenn.io/docbank/cmd/docbank")
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, stopErr := daemonconn.Stop(ctx, root)
		require.NoError(t, stopErr)
	})
	client := newProductionMCPTestClient(t, binary, root)
	finalize := map[string]any{"set_id": setID, "revision": revision, "etag": etag,
		"namespace_id": namespaceID,
		"snapshot_id":  "76000000-0000-4000-8000-000000000041",
		"operation_id": "76000000-0000-4000-8000-000000000042"}
	require.Equal(t, "invalid_production", client.callErrorCode(t, "finalize_production_draft", finalize))
	selection := map[string]any{"set_id": setID, "revision": revision, "approval_id": approvalID}
	first := client.call(t, "select_production_gate_authority", selection)
	require.Equal(t, setID, first["set_id"])
	require.Equal(t, approvalID, first["approval_id"])
	require.Equal(t, first, client.call(t, "select_production_gate_authority", selection))
	public := client.call(t, "get_production_approval", map[string]any{"approval_id": approvalID})
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Synthetic approval evidence.")
	finalized := client.call(t, "finalize_production_draft", finalize)
	require.Equal(t, "finalized", objectValue(t, finalized, "draft")["state"])
}

func (client *productionMCPTestClient) callErrorCode(t *testing.T, name string, arguments map[string]any) string {
	t.Helper()
	client.nextID++
	request := map[string]any{"jsonrpc": "2.0", "id": client.nextID, "method": "tools/call",
		"params": map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}, "name": name, "arguments": arguments}}
	frame, err := json.Marshal(request)
	require.NoError(t, err)
	_, err = client.stdin.Write(append(frame, '\n'))
	require.NoError(t, err, client.stderr.String())
	reply, err := client.stdout.ReadBytes('\n')
	require.NoError(t, err, client.stderr.String())
	var envelope struct {
		ID     int            `json:"id"`
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(reply, &envelope))
	require.Equal(t, client.nextID, envelope.ID)
	require.Empty(t, envelope.Error, "%s: %s", name, reply)
	require.Equal(t, true, envelope.Result["isError"], "%s: %s", name, reply)
	code, ok := objectValue(t, envelope.Result, "structuredContent")["code"].(string)
	require.True(t, ok)
	return code
}
