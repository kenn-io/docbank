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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/telemetry"
	"go.kenn.io/kit/telemetry/posthog"
)

type importActionDaemon struct {
	server   *httptest.Server
	catalog  *store.Store
	requests chan api.TelemetryEventRequest
	jobs     *jobs.Supervisor
	reporter *posthog.Reporter
	mu       sync.Mutex
	surfaces []string
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
	reporter, err := posthog.NewReporter(posthog.Options{APIKey: "synthetic-key", Endpoint: "http://127.0.0.1:1",
		Application: "docbank", EnvPrefix: "DOCBANK", DistinctID: "synthetic-install"},
		posthog.WithAllowedEvent(telemetry.EventDocumentAdded, posthog.RequireProperty("surface", posthog.AllowStringValues("cli"))),
		posthog.WithAllowedEvent(telemetry.EventDocumentOpened, posthog.RequireProperty("surface", posthog.AllowStringValues("cli"))))
	require.NoError(t, err)
	f.reporter = reporter
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	capture := telemetry.CaptureHandler(reporter, root)
	f.jobs = jobs.New(t.Context(), nil)
	t.Cleanup(func() { require.NoError(t, f.jobs.Shutdown(context.Background())) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	server := api.NewServer(api.Deps{Store: f.catalog, Blobs: blobs, VaultRoot: root, Cfg: cfg, Jobs: f.jobs,
		ShutdownToken: "synthetic-token", TelemetryCapture: capture})
	t.Cleanup(server.Close)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/push/uploads" {
			f.mu.Lock()
			f.surfaces = append(f.surfaces, r.Header.Get(api.ActionSurfaceHeader))
			f.mu.Unlock()
		}
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

func TestPushAttributesInteractiveRunsToCLISurface(t *testing.T) {
	for _, name := range []string{"new documents", "watch"} {
		t.Run(name, func(t *testing.T) {
			f := newImportActionDaemon(t)
			require.False(t, f.reporter.Enabled())
			dir := t.TempDir()
			for _, name := range []string{"first.txt", "second.txt"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600))
			}
			args := []string{dir, "--to", f.server.URL, "--name", "synthetic-source", "--dest", "/archive"}
			cmd := newPushCmd()
			cmd.SetIn(actionTerminal(t))
			var out bytes.Buffer
			cmd.SetOut(actionTerminal(t))
			cmd.SetErr(&out)
			if name == "watch" {
				ctx, cancel := context.WithCancel(t.Context())
				cmd.SetArgs(append(args, "--watch", "--settle-time", "1ms", "--scan-interval", "10ms"))
				done := make(chan error, 1)
				go func() { done <- cmd.ExecuteContext(ctx) }()
				require.Eventually(t, func() bool {
					_, err := f.catalog.NodeByPath(t.Context(), "/archive/second.txt")
					return err == nil
				}, 10*time.Second, 10*time.Millisecond)
				cancel()
				<-done
			} else {
				cmd.SetArgs(args)
				require.NoError(t, cmd.ExecuteContext(t.Context()), out.String())
			}
			assert.Empty(t, f.requests, "the CLI never submits additions itself")
			f.mu.Lock()
			defer f.mu.Unlock()
			require.NotEmpty(t, f.surfaces)
			want := ""
			if name != "watch" {
				want = "cli"
			}
			assert.Equal(t, want, f.surfaces[len(f.surfaces)-1])
		})
	}
}
