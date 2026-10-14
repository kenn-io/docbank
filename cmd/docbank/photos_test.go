package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotosCLIWorkflow(t *testing.T) {
	const assetID = "00000000-0000-4000-8000-000000000001"
	const fileID = "00000000-0000-4000-8000-000000000002"
	const assetPath = "/api/v1/photos/assets/" + assetID
	const nodeAssetPath = "/api/v1/photos/nodes/42/asset"
	const virtualPath = "/inbox/workflow-image.jpeg"
	asset := api.PhotoAsset{Agreement: map[string]bool{}, ID: assetID, Kind: "photo", Revision: 7, Files: []api.PhotoFile{{ID: "00000000-0000-4000-8000-000000000002", AssetID: assetID, NodeID: 42, Role: "image", Revision: 3}}}
	excluded := asset
	excluded.Revision = 8
	excluded.ExcludedAt = new("2026-01-01T00:00:00Z")
	detached := asset
	detached.Files = []api.PhotoFile{}
	settings := api.PhotoSettings{Preference: new("image"), Revision: 8}
	resetSettings := api.PhotoSettings{Revision: 9}
	updatedSettings := api.PhotoSettings{Preference: new("raw"), Revision: 9}
	type request struct {
		method, path, queryPath, revision string
		body                              map[string]any
	}
	requests := make(chan request, 32)
	var trashed atomic.Bool
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		input := request{method: r.Method, path: r.URL.Path, queryPath: r.URL.Query().Get("path"), revision: r.Header.Get("If-Match")}
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			assert.NoError(t, json.UnmarshalRead(r.Body, &input.body))
		}
		requests <- input
		w.Header().Set("ETag", `"7"`)
		switch r.URL.Path {
		case "/api/v1/path", "/api/v1/nodes/42":
			if trashed.Load() && r.URL.Path == "/api/v1/path" {
				w.WriteHeader(http.StatusNotFound)
				assert.NoError(t, json.MarshalWrite(w, api.NewError(http.StatusNotFound, "not_found", "path is absent")))
				return
			}
			node := api.Node{ID: 42, Name: "workflow-image.jpeg", Kind: "file", Revision: 3}
			if trashed.Load() {
				node.TrashedAt = "2026-01-01T00:00:00Z"
			}
			assert.NoError(t, json.MarshalWrite(w, node))
		case assetPath, nodeAssetPath, assetPath + "/display", assetPath + "/files", "/api/v1/photos/nodes/42/promote":
			assert.NoError(t, json.MarshalWrite(w, asset))
		case "/api/v1/photos/assets":
			w.WriteHeader(http.StatusCreated)
			assert.NoError(t, json.MarshalWrite(w, asset))
		case assetPath + "/files/" + fileID:
			assert.NoError(t, json.MarshalWrite(w, detached))
		case assetPath + "/exclude":
			if input.body["excluded"] == false {
				w.WriteHeader(http.StatusPreconditionFailed)
				assert.NoError(t, json.MarshalWrite(w, api.NewError(http.StatusPreconditionFailed, "stale_revision", "asset moved on")))
			} else {
				w.Header().Set("ETag", `"8"`)
				assert.NoError(t, json.MarshalWrite(w, excluded))
			}
		case "/api/v1/photos/settings":
			response := settings
			if r.Method == http.MethodPut {
				response = resetSettings
				if input.body["preference"] == "raw" {
					response = updatedSettings
				}
			}
			w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(response.Revision, 10)))
			assert.NoError(t, json.MarshalWrite(w, response))
		default:
			http.NotFound(w, r)
		}
	})
	checkRequests := func(expected ...request) {
		t.Helper()
		require.Len(t, requests, len(expected))
		for _, want := range expected {
			assert.Equal(t, want, <-requests)
		}
	}
	for _, test := range []struct {
		selector string
		requests []request
	}{
		{assetID, []request{{method: "GET", path: assetPath}}},
		{"id:42", []request{{method: "GET", path: "/api/v1/nodes/42"}, {method: "GET", path: nodeAssetPath}}},
		{virtualPath, []request{{method: "GET", path: "/api/v1/path", queryPath: virtualPath}, {method: "GET", path: nodeAssetPath}}},
	} {
		out, err := runCLI(t, "photos", "assets", "inspect", test.selector)
		require.NoError(t, err, out)
		var inspected api.PhotoAsset
		require.NoError(t, json.Unmarshal([]byte(out), &inspected))
		assert.Equal(t, asset, inspected)
		checkRequests(test.requests...)
	}
	for _, test := range []struct {
		args     []string
		response api.PhotoAsset
		requests []request
	}{
		{[]string{"create", "id:42", "--role", "image", "--kind", "photo"}, asset, []request{{method: "GET", path: "/api/v1/nodes/42"}, {method: "POST", path: "/api/v1/photos/assets", body: map[string]any{"node_id": float64(42), "role": "image", "kind": "photo"}}}},
		{[]string{"attach", assetID, "id:42", "--role", "image", "--revision", "7"}, asset, []request{{method: "GET", path: "/api/v1/nodes/42"}, {method: "POST", path: assetPath + "/files", revision: `"7"`, body: map[string]any{"node_id": float64(42), "role": "image"}}}},
		{[]string{"detach", assetID, fileID, "--revision", "7"}, detached, []request{{method: "DELETE", path: assetPath + "/files/" + fileID, revision: `"7"`}}},
		{[]string{"display", assetID}, asset, []request{{method: "GET", path: assetPath}, {method: "PUT", path: assetPath + "/display", revision: `"7"`, body: map[string]any{}}}},
		{[]string{"exclude", assetID, "--revision", "7"}, excluded, []request{{method: "POST", path: assetPath + "/exclude", revision: `"7"`, body: map[string]any{"excluded": true}}}},
		{[]string{"promote", virtualPath}, asset, []request{{method: "GET", path: "/api/v1/path", queryPath: virtualPath}, {method: "GET", path: nodeAssetPath}, {method: "POST", path: "/api/v1/photos/nodes/42/promote", revision: `"7"`, body: map[string]any{}}}},
	} {
		out, err := runCLI(t, append([]string{"photos", "assets"}, test.args...)...)
		require.NoError(t, err, out)
		var received api.PhotoAsset
		require.NoError(t, json.Unmarshal([]byte(out), &received))
		assert.Equal(t, test.response, received)
		checkRequests(test.requests...)
	}
	for _, test := range []struct {
		args     []string
		response api.PhotoSettings
		requests []request
	}{
		{[]string{"show"}, settings, []request{{method: "GET", path: "/api/v1/photos/settings"}}},
		{[]string{"reset"}, resetSettings, []request{{method: "GET", path: "/api/v1/photos/settings"}, {method: "PUT", path: "/api/v1/photos/settings", revision: `"8"`, body: map[string]any{}}}},
		{[]string{"set", "raw", "--revision", "8"}, updatedSettings, []request{{method: "PUT", path: "/api/v1/photos/settings", revision: `"8"`, body: map[string]any{"preference": "raw"}}}},
	} {
		out, err := runCLI(t, append([]string{"photos", "settings"}, test.args...)...)
		require.NoError(t, err, out)
		var received api.PhotoSettings
		require.NoError(t, json.Unmarshal([]byte(out), &received))
		assert.Equal(t, test.response, received)
		checkRequests(test.requests...)
	}
	_, err := runCLI(t, "photos", "assets", "exclude", assetID, "--excluded=false", "--revision", "7")
	require.ErrorIs(t, err, store.ErrStaleRevision)
	assert.Equal(t, exitStale, commandExitCode(err, true))
	checkRequests(request{method: "POST", path: assetPath + "/exclude", revision: `"7"`, body: map[string]any{"excluded": false}})
	trashed.Store(true)
	out, err := runCLI(t, "photos", "assets", "inspect", "id:42")
	require.NoError(t, err, out)
	var received api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(out), &received))
	assert.Equal(t, asset, received)
	checkRequests(request{method: "GET", path: "/api/v1/nodes/42"}, request{method: "GET", path: nodeAssetPath})
	_, err = runCLI(t, "photos", "assets", "inspect", virtualPath)
	require.ErrorIs(t, err, store.ErrNotFound)
	checkRequests(request{method: "GET", path: "/api/v1/path", queryPath: virtualPath})
}

func TestWithPhotoRevisionRetriesOnceOnlyWhenInferred(t *testing.T) {
	stale := fmt.Errorf("asset moved on: %w", store.ErrStaleRevision)
	run := func(args []string, failures int) (reads int, writes []int64, err error) {
		cmd := &cobra.Command{RunE: func(cmd *cobra.Command, _ []string) error {
			current := func() (*int64, error) {
				reads++
				revision := int64(10 + reads)
				return &revision, nil
			}
			_, err := withPhotoRevision(cmd, current, func(revision *int64) (struct{}, error) {
				writes = append(writes, *revision)
				if len(writes) <= failures {
					return struct{}{}, stale
				}
				return struct{}{}, nil
			})
			return err
		}}
		cmd.Flags().Int64Var(&photoRevision, "revision", 0, "")
		cmd.SetArgs(args)
		t.Cleanup(func() { photoRevision = 0 })
		return reads, writes, cmd.Execute()
	}

	reads, writes, err := run(nil, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, reads)
	assert.Equal(t, []int64{11, 12}, writes)

	reads, writes, err = run(nil, 2)
	require.ErrorIs(t, err, store.ErrStaleRevision)
	assert.Equal(t, 2, reads)
	assert.Equal(t, []int64{11, 12}, writes)

	reads, writes, err = run([]string{"--revision", "7"}, 1)
	require.ErrorIs(t, err, store.ErrStaleRevision)
	assert.Zero(t, reads)
	assert.Equal(t, []int64{7}, writes)
}

func TestPhotoImportRejectsEmptySourceAndPointsToJobs(t *testing.T) {
	queued := api.StorageOperation{ID: "00000000-0000-4000-8000-000000000021", Kind: "photo_import", State: "queued"}
	requests := make(chan api.PhotoImportStartRequest, 2)
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/photos/imports":
			assert.Equal(t, http.MethodPost, r.Method)
			var request api.PhotoImportStartRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			requests <- request
			w.WriteHeader(http.StatusAccepted)
			assert.NoError(t, json.MarshalWrite(w, queued))
		default:
			http.NotFound(w, r)
		}
	})
	_, err := runCLI(t, "photos", "import", "")
	require.Error(t, err)
	assert.Equal(t, exitUsage, commandExitCode(err, true))
	source := t.TempDir()
	absoluteSource, err := filepath.Abs(source)
	require.NoError(t, err)
	for _, asJSON := range []bool{false, true} {
		args := []string{"photos", "import", source, "/photos"}
		if asJSON {
			args = append(args, "--json")
		}
		output, err := runCLI(t, args...)
		require.NoError(t, err, output)
		require.Len(t, requests, 1)
		assert.Equal(t, api.PhotoImportStartRequest{SourceRoot: absoluteSource, Destination: "/photos"}, <-requests)
		if asJSON {
			var accepted api.StorageOperation
			require.NoError(t, json.Unmarshal([]byte(output), &accepted))
			assert.Equal(t, queued, accepted)
		} else {
			assert.Contains(t, output, "docbank jobs show "+queued.ID+" --json")
			assert.Contains(t, output, "docbank jobs cancel "+queued.ID)
		}
	}
}
