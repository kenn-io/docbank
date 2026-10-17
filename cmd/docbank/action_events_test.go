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
		name           string
		stdout, stderr *os.File
		agent          bool
	}{
		{"terminal output", terminal, redirected, false},
		{"redirected output with terminal stderr", redirected, terminal, false},
		{"unattended", redirected, redirected, false},
		{"agent", terminal, terminal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetOut(tc.stdout)
			cmd.SetErr(tc.stderr)
			cmd.Flags().Bool("agent", tc.agent, "")
			// A canceled document command still reports its acknowledged result.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			cmd.SetContext(ctx)
			reportAction(cmd, connection, "document_added", "add", 100*time.Millisecond)
			reportOpened(cmd, connection)
		})
	}
	cmd := &cobra.Command{}
	cmd.SetOut(terminal)
	cmd.SetContext(t.Context())
	start := time.Now()
	reportAction(cmd, connection, "search_run", "hybrid", 100*time.Millisecond)
	assert.Less(t, time.Since(start), time.Second)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 2)
	assert.Equal(t, "document_added", events[0].Event)
	assert.Equal(t, "document_opened", events[1].Event)
	for _, event := range events {
		assert.Equal(t, &api.TelemetryEventProperties{Surface: "cli"}, event.Properties)
	}
}
