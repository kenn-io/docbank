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
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestPhotoInspectReportsInteractiveSuccess(t *testing.T) {
	f := newImportActionDaemon(t)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "synthetic.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := f.catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	rootCmd.SetIn(actionTerminal(t))
	photoInspectCmd.SetOut(actionTerminal(t))
	defer photoInspectCmd.SetOut(nil)
	defer rootCmd.SetIn(nil)
	out, err := runCLI(t, "photos", "assets", "inspect", asset.ID)
	require.NoError(t, err, out)
	require.Len(t, f.requests, 1)
	assert.Equal(t, api.TelemetryEventRequest{Event: "document_opened", Properties: &api.TelemetryEventProperties{Surface: "cli"}}, <-f.requests)
}

func TestInteractiveActionGateAndEndpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		terminal := actionTerminal(t)
		redirected, err := os.CreateTemp(t.TempDir(), "redirected")
		require.NoError(t, err)
		defer func() { require.NoError(t, redirected.Close()) }()
		events := make(chan api.TelemetryEventRequest, 2)
		timedOut := make(chan bool, 1)
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body api.TelemetryEventRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &body))
			if body.Event == "search_run" {
				select {
				case <-r.Context().Done():
					timedOut <- true
				case <-time.After(250 * time.Millisecond):
					timedOut <- false
				}
				return
			}
			events <- body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"queued"}`))
		}))
		previousTransport := http.DefaultTransport
		http.DefaultTransport = server.Client().Transport
		defer func() { http.DefaultTransport = previousTransport }()
		connection := daemonconn.New(server.URL, "synthetic-key")
		for _, tc := range []struct {
			name          string
			stdin, stdout *os.File
			agent         bool
			want          int
		}{
			{"terminal input and output", terminal, terminal, false, 2},
			{"terminal input with redirected output", terminal, redirected, false, 0},
			{"unattended input with terminal output", redirected, terminal, false, 0},
			{"agent", terminal, terminal, true, 0},
		} {
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
			require.Len(t, events, tc.want, tc.name)
			if tc.want > 0 {
				assert.Equal(t, []api.TelemetryEventRequest{
					{Event: "document_added", Properties: &api.TelemetryEventProperties{Surface: "cli"}},
					{Event: "document_opened", Properties: &api.TelemetryEventProperties{Surface: "cli"}},
				}, []api.TelemetryEventRequest{<-events, <-events})
			}
		}
		cmd := &cobra.Command{}
		cmd.SetIn(terminal)
		cmd.SetOut(terminal)
		cmd.SetContext(t.Context())
		reportAction(cmd, connection, "search_run", "hybrid")
		assert.True(t, <-timedOut, "action request must cancel before the server stall ends")
	})
}

func TestDocumentSearchReportsSubmissionBeforeFailure(t *testing.T) {
	for _, mode := range []string{"lexical", "semantic", "hybrid", "auto", "similar"} {
		t.Run(mode, func(t *testing.T) {
			var events []api.TelemetryEventRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/daemon/telemetry/events":
					var event api.TelemetryEventRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &event))
					events = append(events, event)
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"status":"queued"}`))
				case "/api/v1/processing/profiles":
					assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "synthetic", Fingerprint: strings.Repeat("a", 64), EmbeddingBindings: []string{"synthetic"}, QueryEmbeddingBindings: []string{"synthetic"}}}))
				case "/api/v1/info":
					assert.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}))
				case "/api/v1/nodes/42":
					assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 42, Kind: "file", CurrentVersionID: processingTestVersionID}))
				case "/api/v1/search", "/api/v1/search/similar":
					assert.Len(t, events, 1, "submission precedes the search request")
					http.Error(w, `{"detail":"synthetic search failure"}`, http.StatusServiceUnavailable)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			cmd.SetIn(actionTerminal(t))
			cmd.SetOut(actionTerminal(t))
			connection := daemonconn.New(server.URL, "synthetic")
			options := documentSearchCLIOptions{Mode: mode, Profile: "synthetic", ContentVersionIDs: []string{processingTestVersionID}, Limit: 10, Intent: true}
			var err error
			if mode == "similar" {
				selector, parseErr := parseNodeSelector("id:42")
				require.NoError(t, parseErr)
				options.Mode = "semantic"
				err = runSimilarSearch(cmd, connection, selector, "", options)
			} else {
				err = runDocumentSearch(cmd, connection, "synthetic query", options)
			}
			require.Error(t, err)
			wantMode := mode
			if mode == "auto" {
				wantMode = "hybrid"
			}
			if mode == "similar" {
				wantMode = "semantic"
			}
			if mode == "lexical" {
				wantMode = "text"
			}
			require.Equal(t, []api.TelemetryEventRequest{{Event: "search_run", Properties: &api.TelemetryEventProperties{Surface: "cli", SearchType: wantMode}}}, events)
		})
	}
}

func TestAutomaticSearchDiscoveryFailureStaysSilent(t *testing.T) {
	var events []api.TelemetryEventRequest
	var record kitdaemon.RuntimeRecord
	var profiles, scope, searches int
	handler := automaticSearchDaemon(t, automaticSearchCase{profileError: "unauthorized"}, &record, &profiles, &scope, &searches)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/telemetry/events" {
			var event api.TelemetryEventRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &event))
			events = append(events, event)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"queued"}`))
			return
		}
		handler(w, r)
	}))
	defer server.Close()
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	record = daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "key", "synthetic-token", "")
	_, err := daemonconn.RuntimeStore(root).Write(record)
	require.NoError(t, err)
	rootCmd.SetIn(actionTerminal(t))
	searchCmd.SetOut(actionTerminal(t))
	defer rootCmd.SetIn(nil)
	defer searchCmd.SetOut(nil)
	_, err = runCLI(t, "search", "synthetic query")
	require.Error(t, err)
	assert.Equal(t, 1, profiles)
	assert.Zero(t, searches)
	assert.Empty(t, events)
}
