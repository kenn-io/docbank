package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
	doctui "go.kenn.io/docbank/internal/tui"
)

func TestTUIHelpDefinesRecoverableMutationBoundary(t *testing.T) {
	out, err := runCLI(t, "tui", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "Open a terminal interface")
	assert.Contains(t, out, "authenticated daemon API")
	assert.Contains(t, out, "explicit revision-bound confirmation")
	assert.Contains(t, out, "outside the TUI")
	assert.Contains(t, out, "/                    Search names and extracted text")
	assert.Contains(t, out, "x                    Move the selected node to recoverable trash")
	assert.Contains(t, out, "T                    Browse and restore recoverable trash")
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

func TestTUIBackendDoesNotRetryMalformedDaemonResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte("{"))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	var acquires atomic.Int32
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*client.Client, error) {
		acquires.Add(1)
		return client.New(server.URL, ""), nil
	}}
	_, err := backend.Node(t.Context(), 42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding GET /api/v1/nodes/42 response")
	assert.NotContains(t, err.Error(), "reconnecting")
	assert.Equal(t, int32(1), acquires.Load())
}

func TestTUIBackendRetriesInterruptedDaemonAcquisition(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(api.Node{
			ID: 1, Kind: "dir", Path: "/", Revision: 1,
		}))
	}))
	t.Cleanup(live.Close)

	var acquires atomic.Int32
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*client.Client, error) {
		if acquires.Add(1) == 1 {
			return nil, fmt.Errorf(
				"ownership proof raced daemon exit: %w",
				client.ErrTransientDaemonAcquisition,
			)
		}
		return client.New(live.URL, ""), nil
	}}
	node, err := backend.Stat(t.Context(), "/")
	require.NoError(t, err)
	assert.Equal(t, int64(1), node.ID)
	assert.Equal(t, int32(2), acquires.Load())
}

func TestTUIBackendDoesNotRetryDeterministicAcquisitionFailure(t *testing.T) {
	deterministic := errors.New("config.toml has an unknown key")
	var acquires atomic.Int32
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*client.Client, error) {
		acquires.Add(1)
		return nil, deterministic
	}}
	_, err := backend.Stat(t.Context(), "/")
	require.ErrorIs(t, err, deterministic)
	assert.Equal(t, int32(1), acquires.Load())
}

func TestTUIBackendDoesNotRetryCanceledAcquisition(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var acquires atomic.Int32
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*client.Client, error) {
		acquires.Add(1)
		return nil, fmt.Errorf(
			"%w: %w", client.ErrTransientDaemonAcquisition, context.Canceled,
		)
	}}
	_, err := backend.Stat(ctx, "/")
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), acquires.Load())
}

func TestTUIBackendDoesNotReplayMutationAfterResponseIsLost(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support connection hijacking")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijacking test connection: %v", err)
			return
		}
		if err := connection.Close(); err != nil {
			t.Errorf("closing test connection: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	var acquires atomic.Int32
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*client.Client, error) {
		acquires.Add(1)
		return client.New(server.URL, ""), nil
	}}
	_, err := backend.Trash(t.Context(), 42, 3)
	require.ErrorContains(t, err, "trash outcome is unconfirmed")
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, int32(1), acquires.Load())
}

func TestTUIBackendReportsTruncatedMutationReceiptAsUnconfirmed(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("{"))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	backend := &tuiDaemonBackend{ensure: func(context.Context) (*client.Client, error) {
		return client.New(server.URL, ""), nil
	}}
	_, err := backend.Trash(t.Context(), 42, 3)
	require.ErrorContains(t, err, "trash outcome is unconfirmed")
	require.ErrorIs(t, err, doctui.ErrMutationUnconfirmed)
	assert.Equal(t, int32(1), requests.Load())
}
