package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/mcp"
	"go.kenn.io/docbank/internal/processing"
)

func mcpRedactionRenderArguments(vault, preparation, masks, review, destination string) map[string]any {
	return map[string]any{
		"vault_id": vault, "preparation_path": preparation, "masks_path": masks,
		"review_path": review, "destination_path": destination,
	}
}

func TestMCPRedactionRenderRejectsSelfConsistentPDF(t *testing.T) {
	if runRedactionVerificationInSubprocess(t) {
		return
	}
	checkMCPRenderZIP(t, "unmasked PDF", "redaction_integrity")
}

func TestMCPRedactionRenderPreservesConcurrentDestination(t *testing.T) {
	if runRedactionVerificationInSubprocess(t) {
		return
	}
	checkMCPRenderZIP(t, "valid", "redaction_render_local_io")
}

func checkMCPRenderZIP(t *testing.T, scenario, code string) {
	t.Helper()
	f := newRedactionVerifyFixture(t)
	t.Setenv("DOCBANK_HOME", t.TempDir())
	archive := f.archive(t, scenario)
	size, digest := int64(len(archive)), redactionPDFHash(archive)
	_, err := daemonconn.VerifyRedactionArchive(t.Context(), bytes.NewReader(archive), size, f.review)
	require.NoError(t, err, "the replacement must pass archive checks before PDF verification")
	const vaultID = "00000000-0000-4000-8000-000000000056"
	dir := t.TempDir()
	destination := filepath.Join(dir, "delivery.zip")
	const concurrentOutput = "saved by another process"
	var renders atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vault":
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.MarshalWrite(w, api.VaultIdentity{VaultID: vaultID}))
		case "/api/v1/redaction/render":
			renders.Add(1)
			assert.Equal(t, http.MethodPost, r.Method)
			var request processing.RedactionRenderRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			assert.Equal(t, f.review.SHA256, request.ReviewSHA256)
			assert.Equal(t, uuid.Nil(), request.AllocationID)
			stages, stageErr := os.ReadDir(dir)
			if assert.NoError(t, stageErr) && assert.Len(t, stages, 1) {
				payload, payloadErr := os.Stat(filepath.Join(dir, stages[0].Name(), "payload.tmp"))
				if assert.NoError(t, payloadErr) {
					assert.Zero(t, payload.Size())
				}
			}
			if scenario == "valid" {
				assert.NoError(t, os.WriteFile(destination, []byte(concurrentOutput), 0o600))
			}
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("X-Content-Sha256", digest)
			w.Header().Set("X-Redaction-Review-Sha256", f.review.SHA256)
			_, _ = w.Write(archive)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer daemon.Close()
	connection := daemonconn.New(daemon.URL, "synthetic-render-key")
	defer func() { _ = connection.Close() }()
	agent := startWorkflowMCPServer(t, mcp.NewServerWithConnection(connection, mcp.ServerOptions{AllowPackageWrites: true}))
	defer agent.stop(t)
	var failure struct {
		Code string `json:"code"`
	}
	raw := agent.call(t, "render_native_pdf", mcpRedactionRenderArguments(vaultID, f.args[2], f.args[3], f.args[4], destination))
	require.NoError(t, json.Unmarshal(raw, &failure))
	require.Equal(t, code, failure.Code)
	require.EqualValues(t, 1, renders.Load())
	if scenario == "valid" {
		content, err := os.ReadFile(destination)
		require.NoError(t, err)
		require.Equal(t, concurrentOutput, string(content))
		require.NoError(t, os.Remove(destination))
	} else {
		require.NoFileExists(t, destination)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
