package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreparePhotoPreviewInterrupted(t *testing.T) {
	t.Parallel()
	catalog := &interruptedPhotoCatalog{}
	_, s := newTestServer(t, func(d *api.Deps) {
		catalog.Catalog = store.NewPackCatalog(d.Store)
		blobs, err := blob.New(catalog, filepath.Join(d.VaultRoot, "blobs"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, blobs.Close()) })
		d.Blobs = blobs
	})
	hash, size, err := s.Blobs.Write(bytes.NewReader(mediatest.JPEG(24, 16, color.White)))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	for _, wrapped := range []bool{false, true} {
		for _, deadline := range []bool{true, false} {
			name, message := "canceled", "Preview preparation was canceled. Retry."
			if deadline {
				name, message = "timed out", "Preview preparation timed out. Retry."
			}
			if wrapped {
				name = "source unavailable " + name
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				if deadline {
					cancel()
					ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
				} else {
					cancel()
				}
				defer cancel()
				catalog.cause = nil
				if wrapped {
					catalog.cause = ctx.Err()
					ctx = t.Context()
				}
				request := httptest.NewRequestWithContext(ctx, http.MethodPost,
					"http://localhost/api/v1/photos/assets/"+asset.ID+"/preview",
					strings.NewReader(`{"content_version_id":"`+node.CurrentVersionID+`","size":"fit"}`))
				request.Header.Set("X-Api-Key", testAPIKey)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				s.Server.Handler().ServeHTTP(response, request)
				require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
				problem := decodeProblem(t, response.Body.String())
				require.Equal(t, "photo_preview_unavailable", problem.Code)
				require.Equal(t, message, problem.Detail)
			})
		}
	}
}

type interruptedPhotoCatalog struct {
	packstore.Catalog

	cause error
}

func (c *interruptedPhotoCatalog) Resolve(_ context.Context, _ packstore.Hash) (packstore.Location, error) {
	return packstore.Location{}, c.cause
}

func TestPhotoBrowseRouteContract(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	var qualityNode store.Node
	for _, name := range []string{"a.jpg", "b.jpg", "c.mp4"} {
		mime := "image/jpeg"
		if strings.HasSuffix(name, "mp4") {
			mime = "video/mp4"
		}
		node, err := s.CreateFile(t.Context(), s.RootID(), name, testHash(name), 10, mime)
		require.NoError(t, err)
		if name == "a.jpg" {
			qualityNode = node
			fields := []document.SourceMetadataFieldV1{}
			sources := map[string]string{"camera_make": "Make", "camera_model": "Model", "lens_make": "LensMake", "lens_model": "LensModel", "iso": "PhotographicSensitivity", "f_number": "FNumber", "exposure_time_seconds": "ExposureTime", "exposure_bias_ev": "ExposureBiasValue", "focal_length_mm": "FocalLength", "orientation": "Orientation"}
			for key, value := range map[string]string{"camera_make": "Synthetic Camera", "camera_model": "Model 7", "lens_make": "Synthetic Lens", "lens_model": "Lens 85"} {
				fields = append(fields, document.SourceMetadataFieldV1{Key: "image.exif." + key, Namespace: "image.exif", SourceField: sources[key], Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new(value)}})
			}
			for key, value := range map[string]float64{"f_number": 3.2, "exposure_time_seconds": 0.4, "exposure_bias_ev": -1.5, "focal_length_mm": 85} {
				fields = append(fields, document.SourceMetadataFieldV1{Key: "image.exif." + key, Namespace: "image.exif", SourceField: sources[key], Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataNumber, Number: new(value)}})
			}
			for key, value := range map[string]int64{"iso": 640, "orientation": 6} {
				fields = append(fields, document.SourceMetadataFieldV1{Key: "image.exif." + key, Namespace: "image.exif", SourceField: sources[key], Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(value)}})
			}
			canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: fields})
			require.NoError(t, err)
			_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, testHash("synthetic camera metadata"), canonical)
			require.NoError(t, err)
		}
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
	require.Equal(t, "pending", page.Items[0].Quality.State)
	require.Contains(t, body, `"signals":null`)
	row := page.Items[0]
	require.Equal(t, new("Synthetic Camera"), row.CameraMake)
	require.Equal(t, new("Model 7"), row.CameraModel)
	require.Equal(t, new("Synthetic Lens"), row.LensMake)
	require.Equal(t, new("Lens 85"), row.LensModel)
	require.Equal(t, new(int64(640)), row.ISO)
	require.Equal(t, new(3.2), row.FNumber)
	require.Equal(t, new(0.4), row.ExposureTimeSeconds)
	require.Equal(t, new(-1.5), row.ExposureBiasEV)
	require.Equal(t, new(85.0), row.FocalLengthMM)
	require.Equal(t, new(int64(6)), row.Orientation)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"kinds":["photo"]}}`), PageSize: 1, Cursor: page.NextCursor})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var second api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Equal(t, int64(2), second.Total)
	require.NotEqual(t, page.Items[0].AssetID, second.Items[0].AssetID)
	require.Empty(t, second.NextCursor)
	for _, key := range []string{"camera_make", "camera_model", "lens_make", "lens_model", "iso", "f_number", "exposure_time_seconds", "exposure_bias_ev", "focal_length_mm", "orientation"} {
		require.Contains(t, body, `"`+key+`":null`)
	}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"kinds":["video"]}}`), PageSize: 1, Cursor: page.NextCursor})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Equal(t, "invalid_photo_cursor", decodeProblem(t, body).Code)
	request := api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"kinds":["photo"]},"sort":{"field":"name","direction":"asc"}}`)}
	require.NoError(t, s.PublishPhotoQualitySignals(t.Context(), store.PhotoVisualPreviewTarget{VersionID: qualityNode.CurrentVersionID, SourceSHA256: qualityNode.BlobHash, Size: 10, MediaType: "image/jpeg"}, document.PhotoQualitySignals{Blur: 1}))
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, "ready", page.Items[0].Quality.State)
	require.Contains(t, body, `"focus":0`)
	require.Zero(t, page.Items[0].Quality.Signals.Focus)
	for _, state := range []document.VisualPreviewState{document.VisualPreviewUnsupported, document.VisualPreviewFailed} {
		terminal, err := s.CreateFile(t.Context(), s.RootID(), string(state)+".jpg", testHash(string(state)), 10, "image/jpeg")
		require.NoError(t, err)
		recipe, err := document.BuiltInVisualPreviewRecipe("grid")
		require.NoError(t, err)
		canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{
			ContractVersion: document.VisualPreviewContractV1, SourceSHA256: terminal.BlobHash, Recipe: recipe, State: state,
			Failure: &document.VisualPreviewFailureV1{Code: "decode_failed", Detail: "synthetic terminal preview"},
		})
		require.NoError(t, err)
		_, err = s.PublishVisualPreview(t.Context(), terminal.CurrentVersionID, canonical, nil)
		require.NoError(t, err)
		response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		found := false
		for _, row := range page.Items {
			if row.ContentVersionID == terminal.CurrentVersionID {
				require.Equal(t, "unavailable", row.Quality.State)
				require.Nil(t, row.Quality.Signals)
				found = true
			}
		}
		require.True(t, found)
		require.Contains(t, body, `"signals":null`)
	}
	unmeasurable, err := s.CreateFile(t.Context(), s.RootID(), "unmeasurable.jpg", testHash("unmeasurable"), 10, "image/jpeg")
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoQualityUnavailable(t.Context(), store.PhotoVisualPreviewTarget{
		VersionID: unmeasurable.CurrentVersionID, SourceSHA256: unmeasurable.BlobHash, Size: 10, MediaType: "image/jpeg",
	}))
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	states := map[string]string{}
	for _, row := range page.Items {
		states[row.ContentVersionID] = row.Quality.State
	}
	require.Equal(t, "unavailable", states[unmeasurable.CurrentVersionID])
	request.Query = api.QueryPayload(`{"filters":{"kinds":["video"]}}`)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NotContains(t, body, `"quality"`)
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
	require.Equal(t, "private, no-cache", response.Header.Get("Cache-Control"))
	require.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
	etag := `"` + generation.GenerationID + `"`
	require.Equal(t, etag, response.Header.Get("ETag"))
	require.Equal(t, "X-Api-Key, Authorization, "+api.WebSessionHeader, response.Header.Get("Vary"))
	digest := sha256.Sum256(data)
	require.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":", response.Header.Get("Content-Digest"))
	for _, condition := range []string{etag, `"another-generation", W/` + etag, "*"} {
		response, body = get(t, ts, path, map[string]string{"If-None-Match": condition})
		require.Equal(t, http.StatusNotModified, response.StatusCode, body)
		require.Empty(t, body)
		require.Equal(t, etag, response.Header.Get("ETag"))
		require.Equal(t, "private, no-cache", response.Header.Get("Cache-Control"))
		require.Equal(t, "X-Api-Key, Authorization, "+api.WebSessionHeader, response.Header.Get("Vary"))
	}
	response, body = get(t, ts, path, map[string]string{"If-None-Match": `"another-generation"`})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, data, []byte(body))

	response, body = do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	webHeaders := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token, "If-None-Match": etag}
	response, body = get(t, ts, path, webHeaders)
	require.Equal(t, http.StatusNotModified, response.StatusCode, body)
	response, body = do(t, ts, http.MethodDelete, "/api/daemon/web-session", webHeaders, nil)
	require.Equal(t, http.StatusNoContent, response.StatusCode, body)
	response, body = get(t, ts, path, webHeaders)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)

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
	// A valid cached generation revalidates without reopening its blob.
	conditionalHeaders := map[string]string{"If-None-Match": etag}
	response, body = get(t, ts, path, conditionalHeaders)
	require.Equal(t, http.StatusNotModified, response.StatusCode, body)
	require.Empty(t, body)
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
	asset, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	response, body = get(t, ts, path, conditionalHeaders)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	require.Empty(t, response.Header.Get("ETag"))
	_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, false)
	require.NoError(t, err)
	response, body = get(t, ts, path, conditionalHeaders)
	require.Equal(t, http.StatusNotModified, response.StatusCode, body)
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, testHash("replacement-photo-source"), 20, "image/jpeg")
	require.NoError(t, err)
	response, body = get(t, ts, path, conditionalHeaders)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	require.Empty(t, response.Header.Get("ETag"))
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

func TestPreparePhotoPreviewEligibilityAndReuse(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	source := mediatest.JPEG(24, 16, color.White)
	hash, size, err := s.Blobs.Write(bytes.NewReader(source))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic-fit.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	path := "/api/v1/photos/assets/" + asset.ID + "/preview"
	request := api.PreparePhotoPreviewRequest{ContentVersionID: node.CurrentVersionID, Size: "fit"}
	response, body := do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var first api.PhotoPreviewSlot
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Equal(t, "ready", first.State)
	require.NotEmpty(t, first.GenerationID)
	require.NotEmpty(t, first.URL)
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var second api.PhotoPreviewSlot
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Equal(t, first, second)
	response, body = do(t, ts, http.MethodPost, strings.Replace(path, asset.ID, "12345678-1234-1123-8123-123456789abc", 1), nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, `"code":"invalid_query"`)
	request.ContentVersionID = "12345678-1234-4123-8123-123456789abc"
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	request.ContentVersionID = node.CurrentVersionID
	request.Size = "grid"
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	request.Size = "large"
	sourcePath := filepath.Join(s.BlobsDir, hash[:2], hash)
	for _, corrupt := range [][]byte{nil, bytes.Repeat([]byte("x"), int(size))} {
		if corrupt == nil {
			require.NoError(t, os.Remove(sourcePath))
		} else {
			require.NoError(t, os.WriteFile(sourcePath, corrupt, 0o600))
		}
		response, body = do(t, ts, http.MethodPost, path, nil, request)
		require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
		require.Contains(t, body, `"code":"photo_preview_unavailable"`)
	}
	require.NoError(t, os.WriteFile(sourcePath, source, 0o600))
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Equal(t, "ready", second.State)
	asset, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, false)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	hash, size, err = s.Blobs.Write(strings.NewReader("synthetic unsupported raw"))
	require.NoError(t, err)
	node, err = s.CreateFile(t.Context(), s.RootID(), "synthetic.mp4", hash, size, "video/mp4")
	require.NoError(t, err)
	asset, err = s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	path = "/api/v1/photos/assets/" + asset.ID + "/preview"
	request = api.PreparePhotoPreviewRequest{ContentVersionID: node.CurrentVersionID, Size: "fit"}
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	first = api.PhotoPreviewSlot{}
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Equal(t, "unsupported", first.State)
	require.Empty(t, first.URL)
}
