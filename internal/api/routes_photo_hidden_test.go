package api_test

import (
	"bytes"
	"encoding/json/v2"
	"image"
	"image/jpeg"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoHiddenHTTPAndClient(t *testing.T) {
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
	for _, path := range []string{"/api/v1/photos/assets/" + asset.ID, "/api/v1/photos/nodes/" + strconv.FormatInt(node.ID, 10) + "/asset"} {
		response, body = get(t, ts, path, nil)
		require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	}
	response, body = get(t, ts, "/api/v1/nodes/"+strconv.FormatInt(node.ID, 10), nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/photos/assets/" + asset.ID + "/exclude", map[string]bool{"excluded": false}},
		{http.MethodPost, "/api/v1/photos/assets/" + asset.ID + "/exclude", map[string]bool{"excluded": true}},
		{http.MethodPut, "/api/v1/photos/assets/" + asset.ID + "/display", map[string]any{"file_id": nil}},
		{http.MethodPost, "/api/v1/photos/nodes/" + strconv.FormatInt(node.ID, 10) + "/promote", map[string]string{}},
	} {
		response, body = do(t, ts, request.method, request.path, map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(asset.Revision, 10))}, request.body)
		require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	}
	state, cookie, err := connection.PhotoHidden(ctx, "unlock", "correct", "")
	require.NoError(t, err)
	require.NotNil(t, state.ExpiresAt)
	require.NotEmpty(t, cookie)
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
	_, _, err = connection.PhotoHidden(ctx, "lock", "", "")
	require.NoError(t, err)
	_, err = connection.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, false, cookie)
	require.Error(t, err)
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
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic source JPEG"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 4, 3)), nil))
	receipt, err := s.Blobs.WriteDetailedContext(ctx, bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	require.NoError(t, err)
	canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{ContractVersion: document.VisualPreviewContractV1, SourceSHA256: hash, Recipe: recipe, State: document.VisualPreviewReady, Output: &document.VisualPreviewOutputV1{BlobSHA256: receipt.Hash, Size: receipt.Size, MediaType: "image/jpeg", Width: 4, Height: 3}})
	require.NoError(t, err)
	generation, err := s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, canonical, &store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, Created: receipt.Created, PackEligible: receipt.PackEligible})
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	path := "/api/v1/photos/assets/" + asset.ID + "/previews/" + generation.GenerationID
	headers := map[string]string{"If-None-Match": strconv.Quote(generation.GenerationID)}
	response, body := get(t, ts, path, headers)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	headers["Cookie"] = "docbank-hidden=" + token
	response, body = get(t, ts, path, headers)
	require.Equal(t, http.StatusNotModified, response.StatusCode, body)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	delete(headers, "If-None-Match")
	response, body = get(t, ts, path, headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, encoded.String(), body)
	require.NoError(t, s.LockPhotoHidden(ctx))
	headers["If-None-Match"] = strconv.Quote(generation.GenerationID)
	response, body = get(t, ts, path, headers)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
}

func TestPhotoHiddenBrowserAllowlistAndCookie(t *testing.T) {
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
