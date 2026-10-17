//go:build linux || windows

package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPhotoInspectReportsInteractiveSuccess(t *testing.T) {
	f := newImportActionDaemon(t)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "synthetic.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := f.catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	rootCmd.SetIn(actionTerminal(t))
	defer rootCmd.SetIn(nil)
	out, err := runCLI(t, "photos", "assets", "inspect", asset.ID)
	require.NoError(t, err, out)
	require.Len(t, f.requests, 1)
	assert.Equal(t, api.TelemetryEventRequest{Event: "document_opened", Properties: &api.TelemetryEventProperties{Surface: "cli"}}, <-f.requests)
}

func TestInteractiveActionGateAndEndpoint(t *testing.T) {
	terminal := actionTerminal(t)
	redirected, err := os.CreateTemp(t.TempDir(), "redirected")
	require.NoError(t, err)
	defer func() { require.NoError(t, redirected.Close()) }()
	events := make(chan api.TelemetryEventRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body api.TelemetryEventRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &body))
		if body.Event == "search_run" {
			<-r.Context().Done()
			return
		}
		events <- body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	defer server.Close()
	connection := daemonconn.New(server.URL, "synthetic-key")
	for _, tc := range []struct {
		name          string
		stdin, stdout *os.File
		agent         bool
		want          int
	}{
		{"terminal input with redirected output", terminal, redirected, false, 2},
		{"unattended input with terminal output", redirected, terminal, false, 0},
		{"agent", terminal, terminal, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetIn(tc.stdin)
			cmd.SetOut(tc.stdout)
			cmd.Flags().Bool("agent", tc.agent, "")
			// A canceled document command still reports its acknowledged result.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			cmd.SetContext(ctx)
			reportAction(cmd, connection, "document_added", "add")
			reportOpened(cmd, connection)
			require.Len(t, events, tc.want)
			if tc.want > 0 {
				assert.Equal(t, []api.TelemetryEventRequest{
					{Event: "document_added", Properties: &api.TelemetryEventProperties{Surface: "cli"}},
					{Event: "document_opened", Properties: &api.TelemetryEventProperties{Surface: "cli"}},
				}, []api.TelemetryEventRequest{<-events, <-events})
			}
		})
	}
	cmd := &cobra.Command{}
	cmd.SetIn(terminal)
	cmd.SetContext(t.Context())
	start := time.Now()
	reportAction(cmd, connection, "search_run", "hybrid")
	assert.Less(t, time.Since(start), time.Second)
}
