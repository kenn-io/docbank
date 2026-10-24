package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
	"go.kenn.io/kit/packstore"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestPhotoBrowsePageSizeLimits(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	for i := range 251 {
		name := fmt.Sprintf("synthetic-%03d.jpg", i)
		_, err := s.CreateFile(t.Context(), s.RootID(), name, testHash(name), 10, "image/jpeg")
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		projection string
		size       int
		status     int
		errorText  string
	}{
		{"", 250, http.StatusOK, ""},
		{"", 251, http.StatusUnprocessableEntity, "page_size must be at most 250 without projection map"},
		{"", 1000, http.StatusUnprocessableEntity, "page_size must be at most 250 without projection map"},
		{"map", 1000, http.StatusOK, ""},
		{"map", 1001, http.StatusUnprocessableEntity, "1000"},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.projection, tc.size), func(t *testing.T) {
			response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`), Projection: tc.projection, PageSize: tc.size})
			require.Equal(t, tc.status, response.StatusCode, body)
			if tc.errorText != "" {
				require.Contains(t, body, tc.errorText)
				return
			}
			var page api.PhotoBrowsePage
			require.NoError(t, json.Unmarshal([]byte(body), &page))
			if tc.projection == "map" {
				require.Len(t, page.MapItems, 251)
				require.Empty(t, page.NextCursor)
			} else {
				require.Len(t, page.Items, 250)
				require.NotEmpty(t, page.NextCursor)
			}
		})
	}
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
			fields := syntheticGPSFields()
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
	asset, err := s.PhotoAssetForNode(t.Context(), qualityNode.ID)
	require.NoError(t, err)
	prepared, err := s.PreparePhotoAuthored(t.Context(), store.PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	_, err = s.EditPhotoAuthoredBatch(t.Context(), store.PhotoAuthoredBatch{Targets: apiEditTargets(prepared.Targets), Patch: store.PhotoAuthoredPatch{Rotation: new(90), Rating: new(4), Flag: new("pick")}})
	require.NoError(t, err)
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
	mapResponse, mapBody := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"kinds":["photo"]}}`), PageSize: 1, Projection: "map"})
	require.Equal(t, http.StatusOK, mapResponse.StatusCode, mapBody)
	var compact api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(mapBody), &compact))
	require.Empty(t, compact.Items)
	require.Len(t, compact.MapItems, 1)
	require.Equal(t, page.Total, compact.Total)
	require.Equal(t, page.Items[0].AssetID, compact.MapItems[0].AssetID)
	require.Equal(t, page.Items[0].Revision, compact.MapItems[0].Revision)
	require.Equal(t, 90, compact.MapItems[0].Rotation)
	require.Equal(t, new(0.0), compact.MapItems[0].Latitude)
	require.Equal(t, new(12.0), compact.MapItems[0].Longitude)
	require.Equal(t, page.Items[0].Previews.Grid.State, compact.MapItems[0].Previews.Grid.State)
	for _, omitted := range []string{"camera_make", "quality", "fit", "large", "content_version_id"} {
		require.NotContains(t, mapBody, `"`+omitted+`"`)
	}
	mapResponse, mapBody = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"kinds":["photo"]}}`), PageSize: 1, Projection: "map", Cursor: compact.NextCursor})
	require.Equal(t, http.StatusOK, mapResponse.StatusCode, mapBody)
	compact = api.PhotoBrowsePage{}
	require.NoError(t, json.Unmarshal([]byte(mapBody), &compact))
	require.Len(t, compact.MapItems, 1)
	require.NotEqual(t, page.Items[0].AssetID, compact.MapItems[0].AssetID)
	require.Empty(t, compact.NextCursor)
	mapResponse, mapBody = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"text":"absent-synthetic-photo"}`), Projection: "map"})
	require.Equal(t, http.StatusOK, mapResponse.StatusCode, mapBody)
	require.NotContains(t, mapBody, `"map_items"`)
	row := page.Items[0]
	require.Equal(t, 4, row.Rating)
	require.Equal(t, "pick", row.Flag)
	require.NotContains(t, body, `"latitude"`)
	require.NotContains(t, body, `"longitude"`)
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
	rankedRequest := api.PhotoBrowseRequest{Query: api.QueryPayload(`{"text":"jpg","sort":{"field":"relevance","direction":"desc"}}`), PageSize: 1, Facets: []string{"camera", "year"}}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, rankedRequest)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var ranked api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &ranked))
	require.Len(t, ranked.Facets, 2)
	require.True(t, ranked.RelevanceAvailable)
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
	generation, data, previewHash := publishTestPhotoPreview(t, s, node)
	queryResponse, queryBody := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`)})
	require.Equal(t, http.StatusOK, queryResponse.StatusCode, queryBody)
	var library api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(queryBody), &library))
	queryResponse, queryBody = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`), Projection: "map"})
	require.Equal(t, http.StatusOK, queryResponse.StatusCode, queryBody)
	var mapped api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(queryBody), &mapped))
	require.Equal(t, library.Items[0].Previews.Grid, mapped.MapItems[0].Previews.Grid)
	require.Equal(t, "ready", mapped.MapItems[0].Previews.Grid.State)
	path := "/api/v1/photos/assets/" + asset.ID + "/previews/" + generation.GenerationID
	response, body := get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, data, []byte(body))
	require.Equal(t, "image/jpeg", response.Header.Get("Content-Type"))
	require.Equal(t, "private, no-cache", response.Header.Get("Cache-Control"))
	require.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
	etag := `"` + generation.GenerationID + `"`
	require.Equal(t, etag, response.Header.Get("ETag"))
	require.Equal(t, "X-Api-Key, Authorization, Cookie, "+api.WebSessionHeader, response.Header.Get("Vary"))
	digest := sha256.Sum256(data)
	require.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":", response.Header.Get("Content-Digest"))
	for _, condition := range []string{etag, `"another-generation", W/` + etag, "*"} {
		response, body = get(t, ts, path, map[string]string{"If-None-Match": condition})
		require.Equal(t, http.StatusNotModified, response.StatusCode, body)
		require.Empty(t, body)
		require.Equal(t, etag, response.Header.Get("ETag"))
		require.Equal(t, "private, no-cache", response.Header.Get("Cache-Control"))
		require.Equal(t, "X-Api-Key, Authorization, Cookie, "+api.WebSessionHeader, response.Header.Get("Vary"))
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
	require.Equal(t, previewHash, page.Items[0].Previews.Grid.SHA256)
	blobPath := filepath.Join(s.BlobsDir, previewHash[:2], previewHash)
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

func publishTestPhotoPreview(t *testing.T, s *testStore, node store.Node) (store.VisualPreviewGeneration, []byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	require.NoError(t, jpeg.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 4, 3)), nil))
	data := buffer.Bytes()
	receipt, err := s.Blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	require.NoError(t, err)
	canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{ContractVersion: document.VisualPreviewContractV1, SourceSHA256: node.BlobHash, Recipe: recipe, State: document.VisualPreviewReady, Output: &document.VisualPreviewOutputV1{BlobSHA256: receipt.Hash, Size: receipt.Size, MediaType: "image/jpeg", Width: 4, Height: 3}})
	require.NoError(t, err)
	generation, err := s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, canonical, &store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, Created: receipt.Created, PackEligible: receipt.PackEligible})
	require.NoError(t, err)
	return generation, data, receipt.Hash
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
	hash, size, err = s.Blobs.Write(bytes.NewReader(source))
	require.NoError(t, err)
	node, err = s.CreateFile(t.Context(), s.RootID(), "synthetic-hidden-fit.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err = s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	path = "/api/v1/photos/assets/" + asset.ID + "/preview"
	request = api.PreparePhotoPreviewRequest{ContentVersionID: node.CurrentVersionID, Size: "fit"}
	require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	request.Hidden = true
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	_, cookie, err := daemonconn.New(ts.URL, testAPIKey).PhotoHidden(t.Context(), "unlock", "synthetic-passcode", "")
	require.NoError(t, err)
	headers := map[string]string{"Cookie": cookie}
	request.Hidden = false
	response, body = do(t, ts, http.MethodPost, path, headers, request)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	request.Hidden = true
	response, body = do(t, ts, http.MethodPost, path, headers, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	first = api.PhotoPreviewSlot{}
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Equal(t, "ready", first.State)
}

func syntheticGPSFields() []document.SourceMetadataFieldV1 {
	return []document.SourceMetadataFieldV1{
		{Key: "image.exif.gps_latitude", Namespace: "image.exif", SourceField: "GPSLatitude", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new("0")}},
		{Key: "image.exif.gps_longitude", Namespace: "image.exif", SourceField: "GPSLongitude", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new("12")}},
	}
}

func TestPhotoBrowseBrowserCoordinatesRequireTiles(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ts, s := newTestServer(t, func(d *api.Deps) {
				if enabled {
					d.Cfg.Web.MapTiles = config.MapTilesConfig{TileURL: "https://tiles.example.org/{z}/{x}/{y}", Attribution: "Example", MaxZoom: 19}
				}
			})
			node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.jpg", testHash("gps"), 10, "image/jpeg")
			require.NoError(t, err)
			canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: syntheticGPSFields()})
			require.NoError(t, err)
			_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, testHash("synthetic gps metadata"), canonical)
			require.NoError(t, err)
			headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
			resp, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", headers, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`), Projection: "map"})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var compact api.PhotoBrowsePage
			require.NoError(t, json.Unmarshal([]byte(body), &compact))
			require.Len(t, compact.MapItems, 1)
			if enabled {
				require.Equal(t, new(0.0), compact.MapItems[0].Latitude)
				require.Equal(t, new(12.0), compact.MapItems[0].Longitude)
			} else {
				require.Nil(t, compact.MapItems[0].Latitude)
				require.Nil(t, compact.MapItems[0].Longitude)
			}
		})
	}
}

func TestMapTileRouteIsInstalledBehindAuthentication(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Web.MapTiles = config.MapTilesConfig{TileURL: "https://127.0.0.1:1/{z}/{x}/{y}.png", Attribution: "Example", MaxZoom: 19}
	})
	const path = "/api/daemon/web-map-tiles/1/0/0"
	resp, _ := do(t, ts, http.MethodGet, path, map[string]string{"X-Api-Key": ""}, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp, body := do(t, ts, http.MethodGet, path, map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}, nil)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Contains(t, body, "map_tile_unavailable")
}

func TestPhotoBrowseRowsCarryFileModifiedTime(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	run, err := s.BeginIngest(t.Context(), "cli", "wallpapers")
	require.NoError(t, err)
	_, err = s.IngestFileExact(t.Context(), run, s.RootID(), "wallpaper.jpg", testHash("wallpaper"), 10, "image/jpeg", "/shots/wallpaper.jpg", "2024-03-05T10:00:00Z")
	require.NoError(t, err)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`)})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Nil(t, page.Items[0].CaptureTime)
	require.Equal(t, new("2024-03-05T10:00:00Z"), page.Items[0].FileModifiedTime)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`), Projection: "map"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var compact api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &compact))
	require.Len(t, compact.MapItems, 1)
	require.Equal(t, new("2024-03-05T10:00:00Z"), compact.MapItems[0].FileModifiedTime)
}

func TestLookupPhotoFlags(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "live.jpg", testHash("live"), 10, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	liveID := asset.ID
	for _, size := range []int{20000, 20001, 0, -2} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ids := []string{}
			if size > 0 {
				for i := range size {
					ids = append(ids, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))
				}
				ids[0] = liveID
			} else if size == -2 {
				ids = []string{"ABCDEF01-2345-4678-89AB-CDEF01234567"}
			}
			response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/flags", nil, api.LookupPhotoFlagsRequest{AssetIDs: ids})
			if size <= 0 || size > 20000 {
				require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
				if size < 0 {
					require.Contains(t, body, ids[0])
				}
				return
			}
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			var result api.PhotoFlagsResponse
			require.NoError(t, json.Unmarshal([]byte(body), &result))
			require.Equal(t, []store.PhotoFlagRow{{AssetID: liveID, Flag: ""}}, result.Items)
		})
	}
}

func TestPhotoMeaningHiddenFallsBackWithinUnlockedView(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	node, err := s.CreateFile(t.Context(), s.RootID(), "hidden-sunset.jpg", testHash("hidden-sunset"), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	request := api.PhotoBrowseRequest{Hidden: true, Query: api.QueryPayload(`{"mode":"semantic","text":"sunset","sort":{"field":"name","direction":"asc"}}`), PageSize: 1}
	_, cookie, err := daemonconn.New(ts.URL, testAPIKey).PhotoHidden(t.Context(), "unlock", "synthetic-passcode", "")
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", map[string]string{"Cookie": cookie}, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, "hidden_photos", page.FallbackReason)
	require.Len(t, page.Items, 1)
	require.Equal(t, asset.ID, page.Items[0].AssetID)
}

func photoImageHTTPFixture(t *testing.T, provider processingTestEmbeddingProvider, names []string, unavailable ...*atomic.Bool) (*httptest.Server, *testStore, []string) {
	t.Helper()
	provider.descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputImage}
	var err error
	provider.descriptor, err = document.NewEmbeddingDescriptor(provider.descriptor)
	require.NoError(t, err)
	var embedding document.EmbeddingProvider = provider
	if len(unavailable) > 0 {
		embedding = togglePhotoImageProvider{EmbeddingProvider: provider, unavailable: unavailable[0]}
	}
	return photoImageHTTPProfilesFixture(t, map[string]document.EmbeddingProvider{"private": embedding}, names, false)
}

// photoImageHTTPProfilesFixture ingests names as photos; distinctCaptures gives each its own pixels and capture time so they group as one burst.
func photoImageHTTPProfilesFixture(t *testing.T, providers map[string]document.EmbeddingProvider, names []string, distinctCaptures bool) (*httptest.Server, *testStore, []string) {
	t.Helper()
	var service *processing.Service
	ts, catalog := newTestServer(t, func(deps *api.Deps) {
		profiles := map[string]processing.ProfileConfig{}
		for name, provider := range providers {
			descriptor := provider.Descriptor()
			profile := processingTestProfile(document.RenditionDescriptor{})
			profile.Rendition = nil
			profile.RetentionDisclosure.RetainSanitizedMarkdown = false
			profile.RetentionDisclosure.TrustBoundary = string(descriptor.TrustBoundary)
			profile.Embeddings = []document.EmbeddingBindingV1{{Activation: document.EmbeddingOptional, AuthorizationFingerprint: processingTestHash("embedding-authorization"), CompatibilityID: descriptor.CompatibilityID, CredentialBinding: "credential:test", Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}, Dimensions: descriptor.Dimension, DisclosureFingerprint: processingTestHash("embedding-disclosure"), DocumentFormatter: descriptor.DocumentFormatter, InputKind: document.EmbeddingInputImage, MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20, Metric: descriptor.Metric, Model: descriptor.Model, ModelInput: descriptor.ModelInput, Name: "semantic", Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter, ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary)}}
			profiles[name] = processing.ProfileConfig{Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{"semantic": provider}}
		}
		gate := api.NewOperationGate()
		deps.Gate = gate
		var err error
		service, err = processing.NewService(processing.ServiceConfig{Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate, SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"), Profiles: profiles})
		require.NoError(t, err)
		deps.Processing = service
	})
	hash, size, err := catalog.Blobs.Write(bytes.NewReader(mediatest.JPEG(24, 24, color.White)))
	require.NoError(t, err)
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	require.NoError(t, err)
	run, err := catalog.BeginIngest(t.Context(), "cli", "synthetic similarity")
	require.NoError(t, err)
	for i, name := range names {
		var node store.Node
		if distinctCaptures {
			hash, size, err := catalog.Blobs.Write(bytes.NewReader(mediatest.JPEG(24, 24, color.RGBA{R: uint8(i * 80), A: 255})))
			require.NoError(t, err)
			node, err = catalog.IngestFileExact(t.Context(), run, catalog.RootID(), name, hash, size, "image/jpeg", "/synthetic/"+name, "2024-02-01T12:00:00Z")
			require.NoError(t, err)
			stamp := fmt.Sprintf("2024-02-01T12:00:%02dZ", i*10)
			canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{Key: "created", Namespace: "image.exif", SourceField: "DateTimeOriginal", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp, Timestamp: &document.SourceMetadataTimestampV1{Raw: stamp, Normalized: stamp, Precision: document.SourceMetadataPrecisionSecond, Timezone: document.SourceMetadataTimezoneUTC}}}}})
			require.NoError(t, err)
			_, err = catalog.PublishSourceMetadata(t.Context(), hash, processing.SourceMetadataExtractorFingerprint, canonical)
			require.NoError(t, err)
		} else {
			node, err = catalog.CreateFile(t.Context(), catalog.RootID(), name, hash, size, "image/jpeg")
			require.NoError(t, err)
		}

		_, err = processing.EnsureVisualPreview(t.Context(), catalog.Store, catalog.Blobs, node.CurrentVersionID, recipe)
		require.NoError(t, err)
		for name := range providers {
			selector := processing.Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: name}
			plan, err := service.Plan(t.Context(), selector)
			require.NoError(t, err)
			_, err = service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: i == 0})
			require.NoError(t, err)
		}
	}
	spaces, err := catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	indexer, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: catalog.Store, Owner: "syntax-images", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond, Mutate: api.NewOperationGate().MutateContext, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
		reader, err := catalog.Blobs.OpenContext(ctx, member.PayloadBlobHash)
		if err != nil {
			return nil, err
		}
		defer func() { require.NoError(t, reader.Close()) }()
		return io.ReadAll(reader)
	}})
	require.NoError(t, err)
	for _, space := range spaces {
		_, err = indexer.Rebuild(t.Context(), space)
		require.NoError(t, err)
	}
	return ts, catalog, spaces
}

type togglePhotoImageProvider struct {
	document.EmbeddingProvider

	unavailable *atomic.Bool
}

func (p togglePhotoImageProvider) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if p.unavailable.Load() {
		return document.EmbeddingResult{}, errors.New("synthetic provider unavailable")
	}
	return p.EmbeddingProvider.Embed(ctx, inputs, authorization)
}

func TestPhotoFallbackCursorSurvivesProviderRecovery(t *testing.T) {
	t.Parallel()
	unavailable := new(atomic.Bool)
	ts, catalog, _ := photoImageHTTPFixture(t, newProcessingTestEmbeddingProvider(t), []string{"sunset-00.jpg"}, unavailable)
	hash, size, err := catalog.Blobs.Write(bytes.NewReader(mediatest.JPEG(24, 24, color.White)))
	require.NoError(t, err)
	for i := 1; i < 51; i++ {
		_, err := catalog.CreateFile(t.Context(), catalog.RootID(), fmt.Sprintf("sunset-%02d.jpg", i), hash, size, "image/jpeg")
		require.NoError(t, err)
	}
	const mode = "semantic"
	request := api.PhotoBrowseRequest{Query: api.QueryPayload(fmt.Sprintf(`{"mode":%q,"text":"sunset","sort":{"field":"name","direction":"asc"}}`, mode)), PageSize: 50}
	unavailable.Store(true)
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var first api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Equal(t, "provider_unavailable", first.FallbackReason)
	require.NotEmpty(t, first.NextCursor)
	require.Equal(t, mode, first.RequestedMode)
	require.Equal(t, "lexical", first.ActualMode)
	require.Len(t, first.Items, 50)
	unavailable.Store(false)
	request.Cursor = first.NextCursor
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var next api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &next))
	require.Equal(t, "lexical", next.ActualMode)
	require.Equal(t, "provider_unavailable", next.FallbackReason)
	require.Empty(t, next.SnapshotID)
	require.Len(t, next.Items, 1)
	require.Equal(t, "sunset-50.jpg", next.Items[0].Name)
	request.Cursor = ""
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &next))
	require.Equal(t, mode, next.ActualMode)
	require.NotEmpty(t, next.SnapshotID)
}

func TestPhotoStackGenerationStaysLocalAfterConsentRevoked(t *testing.T) {
	t.Parallel()
	provider := newProcessingTestEmbeddingProvider(t)
	provider.descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputImage}
	var err error
	provider.descriptor, err = document.NewEmbeddingDescriptor(provider.descriptor)
	require.NoError(t, err)
	ts, _, _ := photoImageHTTPProfilesFixture(t, map[string]document.EmbeddingProvider{"private": &provider, "same-space": &provider}, []string{"one.jpg", "two.jpg"}, true)
	response, body := do(t, ts, http.MethodGet, "/api/v1/photos/settings", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var before api.PhotoSettings
	require.NoError(t, json.Unmarshal([]byte(body), &before))
	require.NotEmpty(t, before.VectorGenerationID)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"mode":"semantic","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), Stacks: true})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Stacks, 1)
	require.Len(t, page.Stacks[0].AssetIDs, 2)

	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consent/revocations", nil, document.ProcessingConsentRevocationRequest{Principal: "embedded:operator", Scope: "document-processing"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/settings", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var after api.PhotoSettings
	require.NoError(t, json.Unmarshal([]byte(body), &after))
	require.Equal(t, before.VectorGenerationID, after.VectorGenerationID)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/stacks/group", nil, api.GroupPhotoStacksRequest{AssetIDs: page.Stacks[0].AssetIDs})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var grouped api.GroupPhotoStacksResponse
	require.NoError(t, json.Unmarshal([]byte(body), &grouped))
	require.Equal(t, before.VectorGenerationID, grouped.VectorGenerationID)
	require.Equal(t, [][]string{page.Stacks[0].AssetIDs}, grouped.Groups)
}

func TestPhotoRankedRoutes(t *testing.T) {
	t.Parallel()
	provider := newProcessingTestEmbeddingProvider(t)
	var calls atomic.Int64
	provider.onEmbed = func() { calls.Add(1) }
	provider.descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputImage}
	var err error
	provider.descriptor, err = document.NewEmbeddingDescriptor(provider.descriptor)
	require.NoError(t, err)
	ts, _, _ := photoImageHTTPProfilesFixture(t, map[string]document.EmbeddingProvider{"private": &provider}, []string{"sunset-a.jpg", "sunset-b.jpg"}, true)
	t.Run("continuation accepts a capture-sort request", func(t *testing.T) {
		request := api.PhotoBrowseRequest{Query: api.QueryPayload(`{"mode":"semantic","text":"sunset","sort":{"field":"capture_time","direction":"desc"}}`), PageSize: 1}
		response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var first api.PhotoBrowsePage
		require.NoError(t, json.Unmarshal([]byte(body), &first))
		require.NotEmpty(t, first.NextCursor)
		request.SnapshotID, request.Cursor = first.SnapshotID, first.NextCursor
		response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var next api.PhotoBrowsePage
		require.NoError(t, json.Unmarshal([]byte(body), &next))
		require.Len(t, next.Items, 1)
		require.NotEqual(t, first.Items[0].AssetID, next.Items[0].AssetID)
	})
	t.Run("capture reuses the accepted ranking with the snapshot row contract", func(t *testing.T) {
		request := api.PhotoBrowseRequest{Query: api.QueryPayload(`{"mode":"semantic","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), PageSize: 1}
		response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var accepted api.PhotoBrowsePage
		require.NoError(t, json.Unmarshal([]byte(body), &accepted))
		require.Equal(t, "semantic", accepted.ActualMode)
		beforeCapture := calls.Load()
		response, body = do(t, ts, http.MethodPost, "/api/v1/workspace/queries", nil, api.WorkspaceQueryCreateRequest{Photos: true, Query: request.Query, PageSize: 250, SnapshotID: accepted.SnapshotID})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var capture struct {
			Rows []map[string]any `json:"rows"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &capture))
		require.Equal(t, beforeCapture, calls.Load(), "capture must reuse the accepted ranking")
		require.NotEmpty(t, capture.Rows)
		for _, row := range capture.Rows {
			for _, key := range []string{"node_id", "content_version_id", "blob_hash", "size", "revision", "name", "path", "mime_type", "media_family", "modified_at", "sort_key", "tags", "collection_ids", "asset_id", "flag"} {
				require.Contains(t, row, key)
			}
			require.Equal(t, "image", row["media_family"])
			require.Equal(t, "image/jpeg", row["mime_type"])
			require.NotEmpty(t, row["modified_at"])
			require.Contains(t, []any{"", "pick", "reject"}, row["flag"])
			require.IsType(t, []any{}, row["tags"])
			require.IsType(t, []any{}, row["collection_ids"])
			require.NotContains(t, row, "photo", "Cull consumes the snapshot row contract")
		}
		require.Equal(t, accepted.Items[0].AssetID, capture.Rows[0]["asset_id"])
	})
	t.Run("stacks refresh returns the accepted stacks", func(t *testing.T) {
		response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"mode":"semantic","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), Stacks: true, PageSize: 250})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var accepted api.PhotoBrowsePage
		require.NoError(t, json.Unmarshal([]byte(body), &accepted))
		require.NotEmpty(t, accepted.SnapshotID)
		require.Len(t, accepted.Stacks, 1)
		response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query/stacks", nil, api.PhotoSnapshotStacksRequest{SnapshotID: accepted.SnapshotID})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var refreshed api.PhotoSnapshotStacksResponse
		require.NoError(t, json.Unmarshal([]byte(body), &refreshed))
		require.Len(t, refreshed.Stacks, 1)
		require.ElementsMatch(t, accepted.Stacks[0].AssetIDs, refreshed.Stacks[0].AssetIDs)
		require.NotEmpty(t, refreshed.Stacks[0].RepresentativeID)
	})
}
