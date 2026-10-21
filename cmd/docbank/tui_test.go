package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	doctui "go.kenn.io/docbank/internal/tui"
)

func TestTUIQuitReturnsBeforeSessionReportCompletes(t *testing.T) {
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/daemon/telemetry/events" {
			var event api.TelemetryEventRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &event))
			if event.Event == "session_ended" {
				started <- r.Context()
				<-release
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"queued"}`))
			return
		}
		if r.URL.Path == "/api/v1/path" {
			_, _ = w.Write([]byte(`{"id":1,"kind":"dir","path":"/","revision":1}`))
			return
		}
		if strings.Contains(r.URL.Path, "/children") {
			_, _ = w.Write([]byte(`{"directory":{"id":1,"kind":"dir","path":"/","revision":1},"items":[],"total":0}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	t.Cleanup(func() {
		close(release)
		// Wait for the detached exit report to release its lock before TempDir cleanup.
		lock := flock.New(filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state.json.lock"))
		defer func() { require.NoError(t, lock.Close()) }()
		require.Eventually(t, func() bool {
			locked, err := lock.TryLock()
			require.NoError(t, err)
			return locked
		}, 2*time.Second, time.Millisecond)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	resetFlags(rootCmd)
	previousOptions := tuiProgramOptions
	tuiProgramOptions = []tea.ProgramOption{tea.WithInput(strings.NewReader("q"))}
	tuiCmd.SetContext(ctx)
	t.Cleanup(func() {
		tuiProgramOptions = previousOptions
		tuiCmd.SetContext(context.Background())
	})
	var stderr bytes.Buffer
	returned := make(chan int, 1)
	go func() { returned <- runProcess([]string{"tui"}, io.Discard, &stderr) }()
	var reportContext context.Context
	select {
	case reportContext = <-started:
	case <-ctx.Done():
		t.Fatal("TUI quit did not report session_ended")
	}
	// Hold the handler even if the client times out. A synchronous report can
	// return on cancellation without ever receiving the handler's response.
	select {
	case code := <-returned:
		require.Equal(t, exitSuccess, code, stderr.String())
		require.NoError(t, reportContext.Err(), "TUI quit waited for the session report's client cancellation")
	case <-reportContext.Done():
		t.Fatal("session report's client canceled before TUI quit returned")
	case <-ctx.Done():
		t.Fatal("TUI quit waited for the stalled session report")
	}
}

func TestTUIProgramRan(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"normal exit", nil, true},
		{"interrupted", tea.ErrInterrupted, true},
		{"killed", tea.ErrProgramKilled, true},
		{"other error", errors.New("synthetic startup failure"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tuiProgramRan(tc.err))
		})
	}
}

func TestTUIBackendForwardsPackagePageCursors(t *testing.T) {
	const packageID = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		query := r.URL.Query()
		switch r.URL.Path {
		case "/api/v1/packages":
			assert.Equal(t, "received", query.Get("direction"))
			assert.Equal(t, "next-package", query.Get("after"))
			assert.Equal(t, "250", query.Get("limit"))
			assert.NoError(t, json.MarshalWrite(w, api.PackagePage{NextAfter: "later-package"}))
		case "/api/v1/packages/by-id/" + packageID + "/members":
			assert.Equal(t, "250", query.Get("after_ordinal"))
			assert.Equal(t, "250", query.Get("limit"))
			assert.NoError(t, json.MarshalWrite(w, api.PackageMemberPage{NextAfterOrdinal: 500}))
		case "/api/v1/packages/label-candidates":
			assert.Equal(t, "EXT000001", query.Get("label"))
			assert.Equal(t, packageID, query.Get("package_id"))
			assert.Equal(t, "next-label", query.Get("cursor"))
			assert.Equal(t, "100", query.Get("limit"))
			assert.NoError(t, json.MarshalWrite(w, api.PackageLabelCandidatePage{NextCursor: "later-label"}))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, ""), nil
	}}
	packages, err := backend.Packages(t.Context(), "received", "next-package", 250)
	require.NoError(t, err)
	assert.Equal(t, "later-package", packages.NextAfter)
	members, err := backend.PackageMembers(t.Context(), packageID, 250, 250)
	require.NoError(t, err)
	assert.Equal(t, 500, members.NextAfterOrdinal)
	labels, err := backend.LookupLabel(t.Context(), "EXT000001", packageID, "next-label", 100)
	require.NoError(t, err)
	assert.Equal(t, "later-label", labels.NextCursor)
}

func TestTUIProcessingRetainsJobAfterInterruptedResponse(t *testing.T) {
	job := api.ProcessingJob{ID: strings.Repeat("a", 64), ContentVersionID: processingTestVersionID, ProfileFingerprint: strings.Repeat("b", 64)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		assert.NoError(t, json.MarshalWrite(w, api.ProcessingJobEvent{Sequence: 1, Type: "job", Job: &job}))
	}))
	t.Cleanup(server.Close)
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, ""), nil
	}}
	stream, err := backend.StartProcessingStream(t.Context(), api.StartProcessingRequest{Selector: api.ProcessingSelector{ContentVersionID: job.ContentVersionID}}, job.ProfileFingerprint)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })
	accepted, err := stream.Next()
	require.NoError(t, err)
	require.NotNil(t, accepted.Job)
	_, err = stream.Next()
	require.Error(t, err)
	assert.Equal(t, job.ID, accepted.Job.ID)
}

func TestTUIHelpDefinesRecoverableMutationBoundary(t *testing.T) {
	out, err := runCLI(t, "tui", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "Open a terminal browser")
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
		assert.NoError(t, json.MarshalWrite(w, api.Node{
			ID: 1, Kind: "dir", Path: "/", Revision: 1,
		}))
	}))
	t.Cleanup(live.Close)

	var reacquires atomic.Int32
	backend := &tuiDaemonBackend{
		initial: daemonconn.New("http://127.0.0.1:1", ""),
		ensure: func(context.Context) (*daemonconn.Connection, error) {
			reacquires.Add(1)
			return daemonconn.New(live.URL, ""), nil
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
		assert.NoError(t, json.MarshalWrite(w, api.NewError(
			http.StatusNotFound, "not_found", "synthetic node is absent",
		)))
	}))
	t.Cleanup(server.Close)

	var acquires atomic.Int32
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		acquires.Add(1)
		return daemonconn.New(server.URL, ""), nil
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
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		acquires.Add(1)
		return daemonconn.New(server.URL, ""), nil
	}}
	_, err := backend.Node(t.Context(), 42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error decoding response")
	assert.NotContains(t, err.Error(), "reconnecting")
	assert.Equal(t, int32(1), acquires.Load())
}

func TestTUIBackendRetriesInterruptedDaemonAcquisition(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.Node{
			ID: 1, Kind: "dir", Path: "/", Revision: 1,
		}))
	}))
	t.Cleanup(live.Close)

	var acquires atomic.Int32
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		if acquires.Add(1) == 1 {
			return nil, fmt.Errorf(
				"ownership proof raced daemon exit: %w",
				daemonconn.ErrTransientDaemonAcquisition,
			)
		}
		return daemonconn.New(live.URL, ""), nil
	}}
	node, err := backend.Stat(t.Context(), "/")
	require.NoError(t, err)
	assert.Equal(t, int64(1), node.ID)
	assert.Equal(t, int32(2), acquires.Load())
}

func TestTUIBackendDoesNotRetryDeterministicAcquisitionFailure(t *testing.T) {
	deterministic := errors.New("config.toml has an unknown key")
	var acquires atomic.Int32
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
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
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		acquires.Add(1)
		return nil, fmt.Errorf(
			"%w: %w", daemonconn.ErrTransientDaemonAcquisition, context.Canceled,
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
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		acquires.Add(1)
		return daemonconn.New(server.URL, ""), nil
	}}
	_, err := backend.Trash(t.Context(), 42, 3)
	require.ErrorContains(t, err, "trash outcome is unconfirmed")
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, int32(1), acquires.Load())
}

func TestTUIBackendDoesNotReplayActionAfterResponseIsLost(t *testing.T) {
	var accepted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		accepted = append(accepted, string(body))
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

	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, ""), nil
	}}
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_THREAD_ID", "")
	err := backend.ReportAction(t.Context(), "search_run", "lexical", "")
	require.Error(t, err)
	assert.True(t, daemonconn.IsTransportError(err))
	require.Len(t, accepted, 1)
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

	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, ""), nil
	}}
	_, err := backend.Trash(t.Context(), 42, 3)
	require.ErrorContains(t, err, "trash outcome is unconfirmed")
	require.ErrorIs(t, err, doctui.ErrMutationUnconfirmed)
	assert.Equal(t, int32(1), requests.Load())
}
