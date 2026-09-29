package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPhotoClientAcceptsCreateTimestampsAndNoOpMutations(t *testing.T) {
	assetID := "00000000-0000-4000-8000-000000000001"
	fileID := "00000000-0000-4000-8000-000000000010"
	createdAt := "2026-09-22T00:00:00Z"
	asset := api.PhotoAsset{
		ID: assetID, Kind: "photo", Revision: 1,
		DisplayFileID: &fileID, DisplaySource: "default",
		CreatedAt: createdAt, UpdatedAt: createdAt,
		Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: 1, Role: "raw", CreatedAt: createdAt}},
	}
	settings := api.PhotoSettings{Revision: 1, UpdatedAt: createdAt}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON := func(value any) {
			body, err := json.Marshal(value)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(body)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/assets":
			w.Header().Set("ETag", `"1"`)
			w.WriteHeader(http.StatusCreated)
			writeJSON(asset)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/assets/"+assetID+"/exclude":
			excludedAt := createdAt
			asset.ExcludedAt = &excludedAt
			w.Header().Set("ETag", `"1"`)
			writeJSON(asset)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/photos/assets/"+assetID+"/display":
			w.Header().Set("ETag", `"1"`)
			writeJSON(asset)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/photos/settings":
			w.Header().Set("ETag", `"1"`)
			writeJSON(settings)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := New(server.URL, "synthetic-key")
	created, err := client.CreatePhotoAsset(t.Context(), 1, "raw", "photo")
	require.NoError(t, err)
	require.Equal(t, createdAt, created.CreatedAt)
	require.Equal(t, createdAt, created.UpdatedAt)

	excluded, err := client.ExcludePhotoAsset(t.Context(), assetID, 1, true)
	require.NoError(t, err)
	require.NotNil(t, excluded.ExcludedAt)
	_, err = client.SetPhotoDisplay(t.Context(), assetID, 1, nil)
	require.NoError(t, err)
	_, err = client.SetPhotoSettings(t.Context(), 1, nil)
	require.NoError(t, err)
}

func TestPhotoNodeAddressedResponsesMustContainTheNode(t *testing.T) {
	assetID := "00000000-0000-4000-8000-000000000001"
	fileID := "00000000-0000-4000-8000-000000000010"
	createdAt := "2026-09-22T00:00:00Z"
	asset := api.PhotoAsset{
		ID: assetID, Kind: "photo", Revision: 2,
		DisplayFileID: &fileID, DisplaySource: "default",
		CreatedAt: createdAt, UpdatedAt: createdAt,
		Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: 1, Role: "image", CreatedAt: createdAt}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		body, err := json.Marshal(asset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	client := New(server.URL, "synthetic-key")

	_, err := client.PhotoAssetForNode(t.Context(), 2)
	require.ErrorContains(t, err, "does not contain node 2")
	_, err = client.AttachPhotoFile(t.Context(), assetID, 1, 2, "image", nil)
	require.True(t, IsResponseDecodeError(err))
	_, err = client.DetachPhotoFile(t.Context(), assetID, 1, fileID, false)
	require.True(t, IsResponseDecodeError(err))
	_, err = client.AttachPhotoFile(t.Context(), assetID, 1, 1, "image", nil)
	require.NoError(t, err)
}

func TestPhotoImportClients(t *testing.T) {
	runID := "00000000-0000-4000-8000-000000000021"
	run := api.PhotoImportRun{ID: runID, Revision: 1, State: "running", SourceRoot: `C:\camera`, Destination: "/photos",
		TotalGroups: 2, StartedAt: "2026-09-22T00:00:00Z", UpdatedAt: "2026-09-22T00:00:00Z"}
	var selected *api.PhotoImportChoice
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(value any, status int, revision int64) {
			w.Header().Set("ETag", `"`+string(rune('0'+revision))+`"`)
			w.WriteHeader(status)
			body, err := json.Marshal(value)
			if err != nil {
				t.Errorf("marshal response: %v", err)
				return
			}
			_, _ = w.Write(body)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/imports":
			var request api.PhotoImportStartRequest
			if err := json.UnmarshalRead(r.Body, &request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			selected = request.Choice
			write(run, http.StatusAccepted, run.Revision)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/photos/imports":
			write(api.PhotoImportRunList{Items: []api.PhotoImportRun{run}}, http.StatusOK, run.Revision)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/photos/imports/"+runID:
			write(run, http.StatusOK, run.Revision)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/imports/"+runID+"/cancel":
			if r.Header.Get("If-Match") != `"1"` {
				http.Error(w, "missing If-Match", http.StatusPreconditionRequired)
				return
			}
			run.Revision = 2
			run.State = "cancel-requested"
			run.CancelRequested = true
			write(run, http.StatusOK, run.Revision)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := New(server.URL, "synthetic-key")
	choice := &api.PhotoImportChoice{GroupKey: "photo\x00camera\x00capture", RawSourcePath: `C:\camera\capture.ARW`}
	started, err := client.StartPhotoImport(t.Context(), `C:\camera`, "/photos", choice)
	require.NoError(t, err)
	assert.Equal(t, runID, started.ID)
	require.NotNil(t, selected)
	assert.Equal(t, choice.GroupKey, selected.GroupKey)
	runs, err := client.PhotoImports(t.Context())
	require.NoError(t, err)
	require.Len(t, runs, 1)
	fetched, err := client.PhotoImport(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, runID, fetched.ID)
	cancelled, err := client.CancelPhotoImport(t.Context(), runID, fetched.Revision)
	require.NoError(t, err)
	assert.True(t, cancelled.CancelRequested)
	assert.True(t, strings.HasPrefix(cancelled.State, "cancel"))
}
