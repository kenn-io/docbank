package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPhotoBrowseRouteContract(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	for _, name := range []string{"a.jpg", "b.jpg", "c.mp4"} {
		mime := "image/jpeg"
		if strings.HasSuffix(name, "mp4") {
			mime = "video/mp4"
		}
		_, err := s.CreateFile(t.Context(), s.RootID(), name, testHash(name), 10, mime)
		require.NoError(t, err)
	}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"kinds":["photo"]}}`), PageSize: 1})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, int64(2), page.Total)
	require.Len(t, page.Items, 1)
	require.NotEmpty(t, page.NextCursor)
	require.Equal(t, "missing", page.Items[0].Previews.Grid.State)
	require.Empty(t, page.Items[0].Previews.Grid.URL)
	require.Nil(t, page.Items[0].CaptureTime)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"kinds":["photo"]}}`), PageSize: 1, Cursor: page.NextCursor})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var second api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Equal(t, int64(2), second.Total)
	require.NotEqual(t, page.Items[0].AssetID, second.Items[0].AssetID)
	require.Empty(t, second.NextCursor)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"kinds":["video"]}}`), PageSize: 1, Cursor: page.NextCursor})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Equal(t, "invalid_photo_cursor", decodeProblem(t, body).Code)
	for _, raw := range []string{`{"filters":{"iso_min":1.0}}`, `{"filters":{"iso_min":1e1}}`, `{"filters":{"gps_bounds":{"south":"0","west":"0","north":"91","east":"0"}}}`} {
		response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(raw)})
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"syntax":"advanced","text":"camera:A*"}`)})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.NotNil(t, decodeProblem(t, body).Position)
}

func TestPhotoBrowseCursorAfterLongName(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	for _, name := range []string{strings.Repeat("a", 17000) + ".jpg", "b.jpg"} {
		_, err := s.CreateFile(t.Context(), s.RootID(), name, testHash(name), 10, "image/jpeg")
		require.NoError(t, err)
	}
	request := api.PhotoBrowseRequest{Query: api.QueryPayload(`{"sort":{"field":"name","direction":"asc"}}`), PageSize: 1}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var first api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.NotEmpty(t, first.NextCursor)
	request.Cursor = first.NextCursor
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var second api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Len(t, second.Items, 1)
	require.NotEqual(t, first.Items[0].AssetID, second.Items[0].AssetID)
	require.Empty(t, second.NextCursor)
}

func TestReadPhotoPreviewVerifiedBytes(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	sourceHash, sourceSize, err := s.Blobs.Write(strings.NewReader("synthetic source JPEG"))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.jpg", sourceHash, sourceSize, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	var buffer bytes.Buffer
	require.NoError(t, jpeg.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 4, 3)), nil))
	data := buffer.Bytes()
	receipt, err := s.Blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	require.NoError(t, err)
	canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{ContractVersion: document.VisualPreviewContractV1, SourceSHA256: sourceHash, Recipe: recipe, State: document.VisualPreviewReady, Output: &document.VisualPreviewOutputV1{BlobSHA256: receipt.Hash, Size: receipt.Size, MediaType: "image/jpeg", Width: 4, Height: 3}})
	require.NoError(t, err)
	generation, err := s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, canonical, &store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, Created: receipt.Created, PackEligible: receipt.PackEligible})
	require.NoError(t, err)
	path := "/api/v1/photos/assets/" + asset.ID + "/previews/" + generation.GenerationID
	response, body := get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, data, []byte(body))
	require.Equal(t, "image/jpeg", response.Header.Get("Content-Type"))
	require.Equal(t, "private, no-store", response.Header.Get("Cache-Control"))
	require.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
	require.Equal(t, `"`+generation.GenerationID+`"`, response.Header.Get("ETag"))
	digest := sha256.Sum256(data)
	require.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":", response.Header.Get("Content-Digest"))
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`)})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, path, page.Items[0].Previews.Grid.URL)
	require.Equal(t, receipt.Hash, page.Items[0].Previews.Grid.SHA256)
	blobPath := filepath.Join(s.BlobsDir, receipt.Hash[:2], receipt.Hash)
	for _, corrupt := range [][]byte{data[:len(data)-1], bytes.Repeat([]byte("x"), len(data))} {
		require.NoError(t, os.WriteFile(blobPath, corrupt, 0o600))
		response, body = get(t, ts, path, nil)
		require.GreaterOrEqual(t, response.StatusCode, 500, body)
		require.Empty(t, response.Header.Get("ETag"))
		require.NotEqual(t, "image/jpeg", response.Header.Get("Content-Type"))
	}
	require.NoError(t, os.Remove(blobPath))
	response, body = get(t, ts, path, nil)
	require.GreaterOrEqual(t, response.StatusCode, 500, body)
	require.NoError(t, os.WriteFile(blobPath, data, 0o600))
	packed, err := s.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Positive(t, packed.BlobsPacked)
	response, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, data, []byte(body))
	packs, err := filepath.Glob(filepath.Join(s.BlobsDir, "packs", "*", "*"+packstore.PackExt))
	require.NoError(t, err)
	require.NotEmpty(t, packs)
	for _, pack := range packs {
		require.NoError(t, os.Truncate(pack, 16))
	}
	response, body = get(t, ts, path, nil)
	require.GreaterOrEqual(t, response.StatusCode, 500, body)
	_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	response, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestPhotoBrowserSessionRevocation(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	require.NotEmpty(t, issued.Token)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	request := api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`)}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", headers, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query?unexpected=1", headers, request)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	response, body = do(t, ts, http.MethodDelete, "/api/daemon/web-session", headers, nil)
	require.Equal(t, http.StatusNoContent, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", headers, request)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
}
