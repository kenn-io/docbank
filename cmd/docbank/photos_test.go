package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestPhotosCLIWorkflow(t *testing.T) {
	paths := [][]string{
		{"photos", "assets", "create"},
		{"photos", "assets", "inspect"},
		{"photos", "assets", "attach"},
		{"photos", "assets", "detach"},
		{"photos", "assets", "exclude"},
		{"photos", "assets", "promote"},
		{"photos", "assets", "display"},
		{"photos", "settings", "show"},
		{"photos", "settings", "set"},
		{"photos", "settings", "reset"},
	}
	for _, path := range paths {
		command, args, err := rootCmd.Find(path)
		require.NoError(t, err, path)
		require.NotNil(t, command, path)
		assert.Equal(t, rootCmd.CommandPath()+" "+strings.Join(path, " "), command.CommandPath())
		assert.Empty(t, args)
	}
	for _, path := range [][]string{
		{"photos", "assets", "attach"}, {"photos", "assets", "detach"},
		{"photos", "assets", "exclude"}, {"photos", "assets", "display"},
		{"photos", "settings", "set"}, {"photos", "settings", "reset"},
	} {
		command, _, err := rootCmd.Find(path)
		require.NoError(t, err)
		assert.NotNil(t, command.Flags().Lookup("revision"), path)
	}

	vault := t.TempDir()
	t.Setenv("DOCBANK_HOME", vault)
	const token = "synthetic-proof-token"
	const assetID = "00000000-0000-4000-8000-000000000001"
	const assetPath = "/api/v1/photos/assets/" + assetID
	const nodeAssetPath = "/api/v1/photos/nodes/42/asset"
	const virtualPath = "/inbox/workflow-image.jpeg"
	asset := api.PhotoAsset{Agreement: map[string]bool{}, ID: assetID, Kind: "photo", Revision: 7, Files: []api.PhotoFile{{ID: "00000000-0000-4000-8000-000000000002", AssetID: assetID, NodeID: 42, Role: "image", Revision: 3}}}
	excluded := asset
	excluded.Revision = 8
	excluded.ExcludedAt = new("2026-01-01T00:00:00Z")
	settings := api.PhotoSettings{Preference: new("image"), Revision: 8}
	resetSettings := api.PhotoSettings{Revision: 9}
	type request struct {
		method, path, queryPath, revision string
		body                              map[string]any
	}
	requests := make(chan request, 32)
	var trashed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case kitdaemon.DefaultPingPath:
			assert.NoError(t, json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: daemonconn.Service, Version: version.Version, PID: os.Getpid()}))
			return
		case daemonauth.ChallengePath:
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof(token, nonce)}))
			return
		}
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
		case assetPath, nodeAssetPath, assetPath + "/display", "/api/v1/photos/nodes/42/promote":
			assert.NoError(t, json.MarshalWrite(w, asset))
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
			}
			w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(response.Revision, 10)))
			assert.NoError(t, json.MarshalWrite(w, response))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, err := daemonconn.RuntimeStore(vault).Write(daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "synthetic-key", token, ""))
	require.NoError(t, err)
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
	for _, action := range []string{"show", "reset"} {
		out, err := runCLI(t, "photos", "settings", action)
		require.NoError(t, err, out)
		var received api.PhotoSettings
		require.NoError(t, json.Unmarshal([]byte(out), &received))
		if action == "show" {
			assert.Equal(t, settings, received)
			checkRequests(request{method: "GET", path: "/api/v1/photos/settings"})
		} else {
			assert.Equal(t, resetSettings, received)
			checkRequests(request{method: "GET", path: "/api/v1/photos/settings"}, request{method: "PUT", path: "/api/v1/photos/settings", revision: `"8"`, body: map[string]any{}})
		}
	}
	_, err = runCLI(t, "photos", "assets", "exclude", assetID, "--excluded=false", "--revision", "7")
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

func TestPhotoImportCommands(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"photos", "import"})
	require.NoError(t, err)
	assert.NotNil(t, command.Flags().Lookup("json"))
	command, _, err = rootCmd.Find([]string{"photos", "imports"})
	require.NoError(t, err)
	assert.NotEqual(t, "imports", command.Name())
}

func TestPhotoImportRejectsEmptySource(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	_, err := runCLI(t, "photos", "import", "")
	require.Error(t, err)
	assert.Equal(t, exitUsage, commandExitCode(err, true))
}

func TestPhotoImportOutputPointsToJobs(t *testing.T) {
	previousJSON := photoImportJSON
	t.Cleanup(func() { photoImportJSON = previousJSON })
	operation := api.StorageOperation{ID: "00000000-0000-4000-8000-000000000021", Kind: "photo_import", State: "queued"}
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	photoImportJSON = false
	require.NoError(t, writePhotoImportOutput(command, operation))
	assert.Contains(t, output.String(), "docbank jobs show "+operation.ID+" --json")
	assert.Contains(t, output.String(), "docbank jobs cancel "+operation.ID)
	photoImportJSON = true
	output.Reset()
	require.NoError(t, writePhotoImportOutput(command, operation))
	var accepted api.StorageOperation
	require.NoError(t, json.Unmarshal(output.Bytes(), &accepted))
	assert.Equal(t, operation, accepted)
}

func TestPhotoImportAmbiguitiesReachJobsShow(t *testing.T) {
	vault := t.TempDir()
	t.Setenv("DOCBANK_HOME", vault)
	const token = "synthetic-proof-token"
	operation := api.StorageOperation{ID: "00000000-0000-4000-8000-000000000021", Kind: "photo_import", State: "completed", Receipt: store.PhotoImportReceipt{
		Ambiguous: 1, Ambiguities: []store.PhotoImportAmbiguity{{Reason: store.PhotoImportMultipleRAW, Files: []store.PhotoImportAmbiguousFile{
			{SourcePath: "IMG.ARW", NodeID: 42, Role: store.PhotoRoleRAW},
			{SourcePath: "IMG.DNG", NodeID: 43, Role: store.PhotoRoleRAW},
			{SourcePath: "IMG.JPG", NodeID: 44, Role: store.PhotoRoleImage},
		}}},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case kitdaemon.DefaultPingPath:
			assert.NoError(t, json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: daemonconn.Service, Version: version.Version, PID: os.Getpid()}))
		case daemonauth.ChallengePath:
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof(token, nonce)}))
		case "/api/v1/jobs/" + operation.ID:
			assert.Equal(t, http.MethodGet, r.Method)
			assert.NoError(t, json.MarshalWrite(w, operation))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, err := daemonconn.RuntimeStore(vault).Write(daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "synthetic-key", token, ""))
	require.NoError(t, err)
	output, err := runCLI(t, "jobs", "show", operation.ID, "--json")
	require.NoError(t, err, output)
	expected, err := json.Marshal(operation)
	require.NoError(t, err)
	assert.JSONEq(t, string(expected), output)
}
