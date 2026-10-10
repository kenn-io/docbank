package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoHiddenCookiesIsolateLoopbackDaemons(t *testing.T) {
	t.Parallel()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	var origins []*url.URL
	for range 2 {
		ts, s := newTestServer(t, nil)
		require.NoError(t, s.SetupPhotoHidden(t.Context(), "correct"))
		response, body := do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", nil, map[string]string{"passcode": "correct"})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		origin, err := url.Parse(ts.URL)
		require.NoError(t, err)
		origins = append(origins, origin)
		jar.SetCookies(origin, response.Cookies())
	}
	client := &http.Client{Jar: jar}
	for _, origin := range origins {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin.String()+"/api/v1/photos/hidden", nil)
		require.NoError(t, err)
		request.Header.Set("X-Api-Key", testAPIKey)
		response, err := client.Do(request)
		require.NoError(t, err)
		var state store.PhotoHiddenState
		err = json.UnmarshalRead(response.Body, &state)
		require.NoError(t, response.Body.Close())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.NotNil(t, state.ExpiresAt)
	}
}

func TestPhotoHiddenHTTPAndClient(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic JPEG"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "private.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	storedAsset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	connection := daemonconn.New(ts.URL, testAPIKey)
	_, _, err = connection.PhotoHidden(ctx, "setup", "correct", "")
	require.NoError(t, err)
	asset, err := connection.SetPhotoAssetHidden(ctx, storedAsset.ID, storedAsset.Revision, true, "")
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`)})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Empty(t, page.Items)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`), Hidden: true})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/photos/assets/"+asset.ID, nil)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	state, cookie, err := connection.PhotoHidden(ctx, "unlock", "correct", "")
	require.NoError(t, err)
	require.NotNil(t, state.ExpiresAt)
	require.NotEmpty(t, cookie)
	_, err = connection.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true, "")
	require.ErrorIs(t, err, store.ErrHiddenLocked)
	_, err = connection.PhotoAsset(ctx, asset.ID, cookie)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", map[string]string{"Cookie": cookie}, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`), Hidden: true})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	asset, err = connection.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, false, cookie)
	require.NoError(t, err)
	require.Nil(t, asset.HiddenAt)
	for range 4 {
		response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", nil, map[string]string{"passcode": "wrong"})
		require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", nil, map[string]string{"passcode": "wrong"})
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode, body)
	require.NotEmpty(t, response.Header.Get("Retry-After"))
	response, body = get(t, ts, "/api/v1/photos/hidden", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, "locked_until")
}

func TestPhotoHiddenPreviewChecksBeforeETag(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic source JPEG"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	generation, data, _ := publishTestPhotoPreview(t, s, node)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	path := "/api/v1/photos/assets/" + asset.ID + "/previews/" + generation.GenerationID
	headers := map[string]string{"If-None-Match": strconv.Quote(generation.GenerationID)}
	response, body := get(t, ts, path, headers)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	_, cookie, err := daemonconn.New(ts.URL, testAPIKey).PhotoHidden(ctx, "unlock", "correct", "")
	require.NoError(t, err)
	headers["Cookie"] = cookie
	response, body = get(t, ts, path, headers)
	require.Equal(t, http.StatusNotModified, response.StatusCode, body)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	delete(headers, "If-None-Match")
	response, body = get(t, ts, path, headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, string(data), body)
	require.NoError(t, s.LockPhotoHidden(ctx))
	headers["If-None-Match"] = strconv.Quote(generation.GenerationID)
	response, body = get(t, ts, path, headers)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
}

func TestPhotoHiddenBrowserAllowlistAndCookie(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	response, body := get(t, ts, "/photos/hidden", map[string]string{"X-Api-Key": ""})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, response.Header.Get("Content-Type"), "text/html")
	response, body = do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/setup", headers, map[string]string{"passcode": "correct"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", headers, map[string]string{"passcode": "correct"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	cookies := response.Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].HttpOnly)
	require.Equal(t, http.SameSiteStrictMode, cookies[0].SameSite)
	require.False(t, cookies[0].Secure)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/reset", headers, map[string]string{})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
}

func TestPhotoHiddenLockDuringMaintenance(t *testing.T) {
	t.Parallel()
	gate := api.NewOperationGate()
	ts, s := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	_, cookie, err := daemonconn.New(ts.URL, testAPIKey).PhotoHidden(ctx, "unlock", "correct", "")
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- gate.MaintainContext(ctx, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	t.Cleanup(func() { close(release); require.NoError(t, <-done) })
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/hidden/lock", map[string]string{"Cookie": cookie}, map[string]string{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	state, err := s.PhotoHiddenState(store.WithPhotoHiddenToken(ctx, strings.SplitN(cookie, "=", 2)[1]))
	require.NoError(t, err)
	require.Nil(t, state.ExpiresAt)
}
