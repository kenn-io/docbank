package api_test

import (
	"bufio"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func readPhotoChangeData(t *testing.T, reader *bufio.Scanner, body io.Closer) string {
	t.Helper()
	timer := time.AfterFunc(5*time.Second, func() { _ = body.Close() })
	defer timer.Stop()
	eventLine := ""
	for reader.Scan() {
		line := reader.Text()
		if strings.HasPrefix(line, "event: ") {
			eventLine = line
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			require.Equal(t, "event: change", eventLine)
			require.True(t, reader.Scan())
			require.Empty(t, reader.Text())
			return data
		}
	}
	t.Fatalf("missing SSE event: %v", reader.Err())
	return ""
}

func readPhotoChange(t *testing.T, reader *bufio.Scanner, body io.Closer) store.PhotoEvent {
	t.Helper()
	var event store.PhotoEvent
	require.NoError(t, json.Unmarshal([]byte(readPhotoChangeData(t, reader, body)), &event))
	return event
}

func openPhotoEventStream(t *testing.T, ts *httptest.Server, token string, cookies []*http.Cookie) (*http.Response, *bufio.Scanner) {
	t.Helper()
	authTransport, ok := ts.Client().Transport.(*apiKeyTransport)
	require.True(t, ok)
	transport, ok := authTransport.next.(*http.Transport)
	require.True(t, ok)
	transport.ResponseHeaderTimeout = 5 * time.Second
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/photos/events", nil)
	require.NoError(t, err)
	if token != "" {
		request.Header.Set("X-Api-Key", "")
		request.Header.Set(api.WebSessionHeader, token)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	t.Cleanup(func() { _ = response.Body.Close() })
	return response, bufio.NewScanner(response.Body)
}

func requirePhotoEventStreamEnds(t *testing.T, response *http.Response, scanner *bufio.Scanner, terminate func()) {
	t.Helper()
	closeTimer := time.AfterFunc(5*time.Second, func() { _ = response.Body.Close() })
	defer closeTimer.Stop()
	terminate()
	for scanner.Scan() {
	}
	require.NoError(t, scanner.Err())
}

func TestPhotoEventsSSEWireShape(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	response, scanner := openPhotoEventStream(t, ts, "", nil)
	require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
	readFields := func() map[string]any {
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(readPhotoChangeData(t, scanner, response.Body)), &fields))
		return fields
	}
	require.Equal(t, map[string]any{"refetch": true}, readFields())
	node, err := s.CreateFile(t.Context(), s.RootID(), "arrival.jpg", testHash("arrival"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	fields := readFields()
	require.Len(t, fields, 2)
	require.Equal(t, asset.ID, fields["asset_id"])
	require.IsType(t, float64(0), fields["revision"])
}

func TestPhotoEventsLockedStreamHidesIDsAndRelaysRefetch(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", testHash("hidden"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	publicNode, err := s.CreateFile(ctx, s.RootID(), "public.jpg", testHash("public"), 4, "image/jpeg")
	require.NoError(t, err)
	publicAsset, err := s.PhotoAssetForNode(ctx, publicNode.ID)
	require.NoError(t, err)
	lockedResponse, lockedScanner := openPhotoEventStream(t, ts, "", nil)
	require.True(t, readPhotoChange(t, lockedScanner, lockedResponse.Body).Refetch)
	unlocked, body := do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", nil, map[string]string{"passcode": "synthetic-passcode"})
	require.Equal(t, http.StatusOK, unlocked.StatusCode, body)
	response, scanner := openPhotoEventStream(t, ts, "", unlocked.Cookies())
	require.True(t, readPhotoChange(t, scanner, response.Body).Refetch)
	_, _, err = s.Move(ctx, node.ID, s.RootID(), "hidden-renamed.jpg", node.Revision)
	require.NoError(t, err)
	// The locked holder can relay a generic change pulse to the unlocked tab.
	_, _, err = s.Move(ctx, publicNode.ID, s.RootID(), "public-renamed.jpg", publicNode.Revision)
	require.NoError(t, err)
	require.Equal(t, asset.ID, readPhotoChange(t, scanner, response.Body).AssetID)
	require.Equal(t, publicAsset.ID, readPhotoChange(t, scanner, response.Body).AssetID)
	require.True(t, readPhotoChange(t, lockedScanner, lockedResponse.Body).Refetch)
	require.Equal(t, publicAsset.ID, readPhotoChange(t, lockedScanner, lockedResponse.Body).AssetID)
}

func TestPhotoEventsLockedHolderRelaysRefetchForUnlockedTab(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", testHash("hidden"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	lockedResponse, lockedScanner := openPhotoEventStream(t, ts, "", nil)
	require.True(t, readPhotoChange(t, lockedScanner, lockedResponse.Body).Refetch)

	unlocked, body := do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", nil, map[string]string{"passcode": "synthetic-passcode"})
	require.Equal(t, http.StatusOK, unlocked.StatusCode, body)
	require.Len(t, unlocked.Cookies(), 1)
	_, err = s.SetPhotoAssetHidden(store.WithPhotoHiddenToken(ctx, unlocked.Cookies()[0].Value), asset.ID, asset.Revision, false)
	require.NoError(t, err)

	data := readPhotoChangeData(t, lockedScanner, lockedResponse.Body)
	require.Equal(t, `{"refetch":true}`, data)
	require.NotContains(t, data, asset.ID)
}

func TestPhotoEventsStreamEndsOnShutdownOrSessionRevocation(t *testing.T) {
	t.Parallel()
	for _, termination := range []string{"server close", "session delete"} {
		t.Run(termination, func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			var token string
			terminate := s.Server.Close
			if termination == "session delete" {
				issuedResponse, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
				require.Equal(t, http.StatusCreated, issuedResponse.StatusCode, body)
				var issued struct {
					Token string `json:"token"`
				}
				require.NoError(t, json.Unmarshal([]byte(body), &issued))
				token = issued.Token
				terminate = func() {
					revoked, body := do(t, ts, http.MethodDelete, "/api/daemon/web-session", map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}, nil)
					require.Equal(t, http.StatusNoContent, revoked.StatusCode, body)
				}
			}
			response, scanner := openPhotoEventStream(t, ts, token, nil)
			_ = readPhotoChange(t, scanner, response.Body)
			requirePhotoEventStreamEnds(t, response, scanner, terminate)
		})
	}
}

func TestPhotoEventsStreamLimit(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	authTransport, ok := ts.Client().Transport.(*apiKeyTransport)
	require.True(t, ok)
	transport, ok := authTransport.next.(*http.Transport)
	require.True(t, ok)
	transport.ResponseHeaderTimeout = 5 * time.Second
	var streams []*http.Response
	defer func() {
		for _, response := range streams {
			_ = response.Body.Close()
		}
	}()
	for range 128 {
		response, err := ts.Client().Get(ts.URL + "/api/v1/photos/events")
		require.NoError(t, err)
		streams = append(streams, response)
		require.Equal(t, http.StatusOK, response.StatusCode)
	}
	response, err := ts.Client().Get(ts.URL + "/api/v1/photos/events")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, new(api.Error).ContentType("application/json"), response.Header.Get("Content-Type"))
	var problem api.Error
	require.NoError(t, json.Unmarshal(body, &problem))
	require.Equal(t, http.StatusTooManyRequests, problem.Status)
	require.Equal(t, "photo_event_stream_limit", problem.Code)
	require.NoError(t, streams[0].Body.Close())
	require.Eventually(t, func() bool {
		response, err := ts.Client().Get(ts.URL + "/api/v1/photos/events")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	}, 3*time.Second, 10*time.Millisecond)
}
