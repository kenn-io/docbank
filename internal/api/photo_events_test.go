package api_test

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func readPhotoChangeData(t *testing.T, reader *bufio.Scanner, body io.Closer) string {
	t.Helper()
	timer := time.AfterFunc(5*time.Second, func() { _ = body.Close() })
	defer timer.Stop()
	for reader.Scan() {
		line := reader.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return data
		}
	}
	t.Fatalf("missing SSE event: %v", reader.Err())
	return ""
}

func TestPhotoEventsReachLockedStream(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", testHash("hidden-event"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	// This stream carries no Hidden unlock.
	response, err := ts.Client().Get(ts.URL + "/api/v1/photos/events")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	scanner := bufio.NewScanner(response.Body)
	_, err = s.SetPhotoAssetExcluded(store.WithPhotoHiddenToken(ctx, token), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, readPhotoChangeData(t, scanner, response.Body))
}

func TestPhotoEventsStreamLimitAndShutdown(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	var bodies []io.ReadCloser
	defer func() {
		for _, body := range bodies {
			_ = body.Close()
		}
	}()
	for range 128 {
		response, err := ts.Client().Get(ts.URL + "/api/v1/photos/events")
		require.NoError(t, err)
		bodies = append(bodies, response.Body)
		require.Equal(t, http.StatusOK, response.StatusCode)
	}
	response, body := get(t, ts, "/api/v1/photos/events", nil)
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode, body)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "photo_event_stream_limit", problem.Code)
	// A disconnected client frees its slot.
	require.NoError(t, bodies[0].Close())
	bodies = bodies[1:]
	require.Eventually(t, func() bool {
		response, err := ts.Client().Get(ts.URL + "/api/v1/photos/events")
		if err != nil {
			return false
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return false
		}
		bodies = append(bodies, response.Body)
		return true
	}, 5*time.Second, 50*time.Millisecond)
	s.Server.Close()
	for _, body := range bodies {
		timer := time.AfterFunc(5*time.Second, func() { _ = body.Close() })
		remaining, err := io.ReadAll(body)
		timer.Stop()
		require.NoError(t, err)
		require.Empty(t, remaining)
	}
}

func TestPhotoEventsStreamKeepsDaemonActive(t *testing.T) {
	t.Parallel()
	tracker := api.NewActivityTracker()
	_, s := newTestServer(t, func(d *api.Deps) { d.Tracker = tracker })
	synctest.Test(t, func(t *testing.T) {
		defer s.Server.Close()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, testWebURL+"api/v1/photos/events", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		request.Header.Set("X-Api-Key", testAPIKey)
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.Server.Handler().ServeHTTP(response, request)
		}()
		synctest.Wait()
		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, "text/event-stream", response.Header().Get("Content-Type"))
		time.Sleep(6 * time.Minute)
		require.Zero(t, tracker.IdleFor(), "an open Photos stream must keep the daemon active")
		cancel()
		<-done
	})
}
