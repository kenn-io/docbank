package store_test

import (
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func productionGateProofBinary(t *testing.T) string {
	t.Helper()
	name := "docbank"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", binary,
		"go.kenn.io/docbank/cmd/docbank")
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	return binary
}

func stopProductionGateProofDaemon(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := daemonconn.Stop(ctx, root)
		require.NoError(t, err)
	})
}

// This opt-in proof drives the real CLI and daemon over synthetic frozen log authority.
func TestProductionCLIRealDaemonSelectsFrozenPrivilegeBeforeFinalization(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_PRIVILEGE_CLI_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon CLI frozen privilege proof")
	}
	vault, root, setID, revision, etag, namespaceID, logID := store.ProductionRequiredPrivilegeHTTPFixture(t)
	require.NoError(t, vault.Close())
	binary := productionGateProofBinary(t)
	stopProductionGateProofDaemon(t, root)
	cli := func(args ...string) (string, error) {
		t.Helper()
		command := exec.CommandContext(t.Context(), binary, args...)
		command.Env = append(os.Environ(), "DOCBANK_HOME="+root)
		data, err := command.CombinedOutput()
		return string(data), err
	}
	finalize := []string{"production", "drafts", "finalize", setID, strconv.FormatInt(revision, 10),
		"--etag", strconv.FormatInt(etag, 10), "--namespace-id", namespaceID,
		"--snapshot-id", "77000000-0000-4000-8000-000000000071",
		"--operation-id", "77000000-0000-4000-8000-000000000072", "--json"}
	failed, err := cli(finalize...)
	require.Error(t, err)
	require.Contains(t, failed, "invalid_production")
	selection := []string{"production", "drafts", "select-gates", setID, strconv.FormatInt(revision, 10),
		"--privilege-log-id", logID, "--privilege-log-revision", "1", "--json"}
	selected, err := cli(selection...)
	require.NoError(t, err, selected)
	var result struct {
		LogID    string `json:"privilege_log_id"`
		Revision int64  `json:"privilege_log_revision"`
	}
	require.NoError(t, json.Unmarshal([]byte(selected), &result))
	require.Equal(t, logID, result.LogID)
	require.EqualValues(t, 1, result.Revision)
	replayed, err := cli(selection...)
	require.NoError(t, err, replayed)
	require.JSONEq(t, selected, replayed)
	public, err := cli("production", "privilege-log", "show", logID, "1", "--json")
	require.NoError(t, err, public)
	require.Contains(t, public, "Synthetic public description.")
	require.NotContains(t, public, "Synthetic private rationale.")
	require.NotContains(t, public, "person_ids")
	finalizedJSON, err := cli(finalize...)
	require.NoError(t, err, finalizedJSON)
	var finalized api.ProductionFinalizationResult
	require.NoError(t, json.Unmarshal([]byte(finalizedJSON), &finalized))
	require.Equal(t, "finalized", finalized.Draft.State)
}

// This opt-in proof drives MCP stdio and a real daemon independently of CLI production operations.
func TestProductionMCPRealDaemonSelectsFrozenPrivilegeBeforeFinalization(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_PRIVILEGE_MCP_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon MCP frozen privilege proof")
	}
	vault, root, setID, revision, etag, namespaceID, logID := store.ProductionRequiredPrivilegeHTTPFixture(t)
	require.NoError(t, vault.Close())
	binary := productionGateProofBinary(t)
	stopProductionGateProofDaemon(t, root)
	client := newProductionMCPTestClient(t, binary, root)
	finalize := map[string]any{"set_id": setID, "revision": revision, "etag": etag,
		"namespace_id": namespaceID,
		"snapshot_id":  "77000000-0000-4000-8000-000000000073",
		"operation_id": "77000000-0000-4000-8000-000000000074"}
	require.Equal(t, "invalid_production", client.callErrorCode(t, "finalize_production_draft", finalize))
	selection := map[string]any{"set_id": setID, "revision": revision,
		"privilege_log_id": logID, "privilege_log_revision": 1}
	selected := client.call(t, "select_production_gate_authority", selection)
	require.Equal(t, logID, selected["privilege_log_id"])
	require.Equal(t, selected, client.call(t, "select_production_gate_authority", selection))
	public := client.call(t, "get_production_privilege_log", map[string]any{
		"log_id": logID, "revision": 1, "limit": 1})
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "Synthetic public description.")
	require.NotContains(t, string(encoded), "Synthetic private rationale.")
	require.NotContains(t, string(encoded), "person_ids")
	finalized := client.call(t, "finalize_production_draft", finalize)
	require.Equal(t, "finalized", objectValue(t, finalized, "draft")["state"])
}
