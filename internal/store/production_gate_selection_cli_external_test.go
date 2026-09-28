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

// This opt-in proof uses a real daemon and synthetic authenticated-human grant.
func TestProductionCLIRealDaemonSelectsRequiredApprovalBeforeFinalization(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_GATE_CLI_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon production gate selection proof")
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
	cli := func(args ...string) (string, error) {
		t.Helper()
		command := exec.CommandContext(t.Context(), binary, args...)
		command.Env = append(os.Environ(), "DOCBANK_HOME="+root)
		data, runErr := command.CombinedOutput()
		return string(data), runErr
	}
	finalize := []string{"production", "drafts", "finalize", setID, strconv.FormatInt(revision, 10),
		"--etag", strconv.FormatInt(etag, 10), "--namespace-id", namespaceID,
		"--snapshot-id", "76000000-0000-4000-8000-000000000031",
		"--operation-id", "76000000-0000-4000-8000-000000000032", "--json"}
	failed, err := cli(finalize...)
	require.Error(t, err)
	require.Contains(t, failed, "invalid_production")
	selectArgs := []string{"production", "drafts", "select-gates", setID,
		strconv.FormatInt(revision, 10), "--approval-id", approvalID, "--json"}
	selected, err := cli(selectArgs...)
	require.NoError(t, err, selected)
	var selection struct {
		SetID      string `json:"set_id"`
		Revision   int64  `json:"revision"`
		ApprovalID string `json:"approval_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(selected), &selection))
	require.Equal(t, setID, selection.SetID)
	require.Equal(t, revision, selection.Revision)
	require.Equal(t, approvalID, selection.ApprovalID)
	replayed, err := cli(selectArgs...)
	require.NoError(t, err, replayed)
	require.JSONEq(t, selected, replayed)
	public, err := cli("production", "approval", "show", approvalID, "--json")
	require.NoError(t, err, public)
	require.NotContains(t, public, "Synthetic approval evidence.")
	finalizedJSON, err := cli(finalize...)
	require.NoError(t, err, finalizedJSON)
	var finalized api.ProductionFinalizationResult
	require.NoError(t, json.Unmarshal([]byte(finalizedJSON), &finalized))
	require.Equal(t, "finalized", finalized.Draft.State)
}
