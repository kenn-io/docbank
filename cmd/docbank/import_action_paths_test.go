//go:build linux || windows

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/telemetry"
)

type importActionDaemon struct {
	server   *httptest.Server
	catalog  *store.Store
	requests chan api.TelemetryEventRequest
	jobs     *jobs.Supervisor
}

func newImportActionDaemon(t *testing.T) *importActionDaemon {
	t.Helper()
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "0")
	t.Setenv("TELEMETRY_ENABLED", "0")
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	t.Setenv("DOCBANK_API_KEY", "synthetic-key")
	f := &importActionDaemon{requests: make(chan api.TelemetryEventRequest, 16)}
	var err error
	f.catalog, err = store.Open(filepath.Join(root, "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.catalog.Close()) })
	blobsRoot := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsRoot, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(f.catalog), blobsRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	capture := telemetry.CaptureHandler(telemetry.New(telemetry.Options{Dir: root}), root)
	f.jobs = jobs.New(t.Context(), nil)
	t.Cleanup(func() { require.NoError(t, f.jobs.Shutdown(context.Background())) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	server := api.NewServer(api.Deps{Store: f.catalog, Blobs: blobs, VaultRoot: root, Cfg: cfg, Jobs: f.jobs,
		ShutdownToken: "synthetic-token", Imports: telemetry.NewImports(capture), TelemetryCapture: capture})
	t.Cleanup(server.Close)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/daemon/telemetry/events" {
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			assert.NoError(t, r.Body.Close())
			var event api.TelemetryEventRequest
			assert.NoError(t, json.Unmarshal(body, &event))
			r.Body = io.NopCloser(bytes.NewReader(body))
			f.requests <- event
		}
		server.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	record := daemonconn.NewRecord(strings.TrimPrefix(f.server.URL, "http://"), "synthetic-key", "synthetic-token", "")
	_, err = daemonconn.RuntimeStore(root).Write(record)
	require.NoError(t, err)
	return f
}

func TestPushCommandReportsOnlyNewDocumentsOnce(t *testing.T) {
	f := newImportActionDaemon(t)
	dir := t.TempDir()
	for _, name := range []string{"first.txt", "second.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600))
	}
	for _, step := range []struct {
		name, summary string
		want          int
	}{
		{"new documents", "added 2, updated 0", 1},
		{"skip only", "added 0, updated 0, unchanged 2", 0},
		{"update only", "added 0, updated 1, unchanged 1", 0},
	} {
		t.Run(step.name, func(t *testing.T) {
			if step.name == "update only" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "first.txt"), []byte("updated synthetic content"), 0o600))
			}
			cmd := newPushCmd()
			cmd.SetIn(actionTerminal(t))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{dir, "--to", f.server.URL, "--name", "synthetic-source", "--dest", "/archive"})
			require.NoError(t, cmd.ExecuteContext(t.Context()), out.String())
			assert.Contains(t, out.String(), step.summary)
			assert.Len(t, f.requests, step.want)
			for len(f.requests) > 0 {
				assert.Equal(t, api.TelemetryEventRequest{Event: "document_added", Properties: &api.TelemetryEventProperties{Surface: "cli"}}, <-f.requests)
			}
		})
	}
}
