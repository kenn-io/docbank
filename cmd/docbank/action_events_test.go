//go:build linux || windows

package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestInteractiveActionGateAndEndpoint(t *testing.T) {
	terminal := actionTerminal(t)
	redirected, err := os.CreateTemp(t.TempDir(), "redirected")
	require.NoError(t, err)
	defer func() { require.NoError(t, redirected.Close()) }()
	var mu sync.Mutex
	var events []api.TelemetryEventRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/daemon/telemetry/events", r.URL.Path)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		var body api.TelemetryEventRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &body))
		if body.Event == "search_run" {
			<-r.Context().Done()
			return
		}
		mu.Lock()
		events = append(events, body)
		mu.Unlock()
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
		{"terminal input and output", terminal, terminal, false, 2},
		{"terminal input with redirected output", terminal, redirected, false, 2},
		{"unattended input with terminal output", redirected, terminal, false, 0},
		{"agent", terminal, terminal, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			events = nil
			mu.Unlock()
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
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, events, tc.want)
			if tc.want > 0 {
				assert.Equal(t, []api.TelemetryEventRequest{
					{Event: "document_added", Properties: &api.TelemetryEventProperties{Surface: "cli"}},
					{Event: "document_opened", Properties: &api.TelemetryEventProperties{Surface: "cli"}},
				}, events)
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
