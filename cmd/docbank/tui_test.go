package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

func TestTUIHelpDefinesReadOnlyInteractiveBoundary(t *testing.T) {
	out, err := runCLI(t, "tui", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "Open a read-only terminal interface")
	assert.Contains(t, out, "authenticated daemon API")
	assert.Contains(t, out, "initial TUI is deliberately read-only")
	assert.Contains(t, out, "/                    Search names and extracted text")
	assert.Contains(t, out, "a                    Browse permanent audited history")
}

func TestTUIBackendReacquiresAfterPinnedDaemonConnectionCloses(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(api.Node{
			ID: 1, Kind: "dir", Path: "/", Revision: 1,
		}))
	}))
	t.Cleanup(live.Close)

	var reacquires atomic.Int32
	backend := &tuiDaemonBackend{
		initial: client.New("http://127.0.0.1:1", ""),
		ensure: func(context.Context) (*client.Client, error) {
			reacquires.Add(1)
			return client.New(live.URL, ""), nil
		},
	}
	node, err := backend.Stat(t.Context(), "/")
	require.NoError(t, err)
	assert.Equal(t, int64(1), node.ID)
	assert.Equal(t, int32(1), reacquires.Load())
}

func TestTUIBackendDoesNotRetryDaemonProblemResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		assert.NoError(t, json.NewEncoder(w).Encode(api.NewError(
			http.StatusNotFound, "not_found", "synthetic node is absent",
		)))
	}))
	t.Cleanup(server.Close)

	var acquires atomic.Int32
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*client.Client, error) {
		acquires.Add(1)
		return client.New(server.URL, ""), nil
	}}
	_, err := backend.Node(t.Context(), 42)
	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, int32(1), acquires.Load())
}
