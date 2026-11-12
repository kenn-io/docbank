package main

import (
	"encoding/json/v2"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedlocal"
)

func TestServeFaceSuggestionsFollowSetupAndRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DO_NOT_TRACK", "1")
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	server := httptest.NewServer(localTextDiscoveryHandler(embedlocal.FaceModel))
	t.Cleanup(server.Close)
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	endpoint, err := kitdaemon.ParseEndpoint(server.URL, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
	require.NoError(t, err)
	_, err = (kitdaemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(kitdaemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
	require.NoError(t, err)

	stop := startServe(t)
	record := waitForDaemon(t, root)
	client := daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	list, err := client.API().ListJobs(t.Context())
	require.NoError(t, err)
	for _, job := range list.Items {
		require.NotEqual(t, "process:face-suggestions", job.Name)
	}
	plan, err := client.API().PlanFaces(t.Context())
	require.NoError(t, err)
	// Repeated setup must succeed when the worker is already registered.
	for range 2 {
		_, err = client.API().ApplyFaces(t.Context(), &apiclient.ApplyFacesRequestOptions{Body: &api.FaceSetupApplyRequest{PlanFingerprint: plan.Fingerprint, Consent: true}})
		require.NoError(t, err)
	}
	list, err = client.API().ListJobs(t.Context())
	require.NoError(t, err)
	index := slices.IndexFunc(list.Items, func(job api.Job) bool { return job.Name == "process:face-suggestions" })
	require.NotEqual(t, -1, index)
	require.Equal(t, "running", list.Items[index].Status)

	stop()
	startServe(t)
	record = waitForDaemon(t, root)
	client = daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	list, err = client.API().ListJobs(t.Context())
	require.NoError(t, err)
	index = slices.IndexFunc(list.Items, func(job api.Job) bool { return job.Name == "process:face-suggestions" })
	require.NotEqual(t, -1, index)
	require.Equal(t, "running", list.Items[index].Status)
}
func TestServeFaceSuggestionsRegisteredAfterRestoreWithVectorHeads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DO_NOT_TRACK", "1")
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[backup]\nrepo = "+strconv.Quote(filepath.ToSlash(repo))+"\n"), 0o600))
	discovery := localTextDiscoveryHandler(embedlocal.FaceModel)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/faces" {
			discovery.ServeHTTP(w, r)
			return
		}
		var request embedclient.FaceRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			return
		}
		response := embedclient.FaceResponse{Model: embedclient.FaceModel, Revision: embedclient.FaceRevision, Recipe: embedclient.FaceRecipe, Data: make([]embedclient.FaceResult, len(request.Boxes))}
		for i := range response.Data {
			response.Data[i].Index = i
			response.Data[i].Embedding = make([]float32, 512)
			response.Data[i].Embedding[0] = 1
		}
		assert.NoError(t, json.MarshalWrite(w, response))
	}))
	t.Cleanup(server.Close)
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	endpoint, err := kitdaemon.ParseEndpoint(server.URL, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
	require.NoError(t, err)
	_, err = (kitdaemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(kitdaemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
	require.NoError(t, err)
	stop := startServe(t)
	record := waitForDaemon(t, root)
	client := daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	plan, err := client.API().PlanFaces(t.Context())
	require.NoError(t, err)
	require.Equal(t, "local-faces", plan.Profile)
	_, err = client.API().ApplyFaces(t.Context(), &apiclient.ApplyFacesRequestOptions{Body: &api.FaceSetupApplyRequest{PlanFingerprint: plan.Fingerprint, Consent: true}})
	require.NoError(t, err)
	require.NoError(t, client.Close())
	stop()
	stop = startServe(t)
	record = waitForDaemon(t, root)
	client = daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	photo := filepath.Join(t.TempDir(), "faces.jpg")
	require.NoError(t, os.WriteFile(photo, mediatest.JPEG(1024, 768, color.White), 0o600))
	_, err = runCLI(t, "add", photo, "--dest", "/photos")
	require.NoError(t, err)
	node, err := client.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/photos/faces.jpg"}})
	require.NoError(t, err)
	for _, x := range []float64{.1, .6} {
		_, err = client.CreatePhotoFace(t.Context(), node.CurrentVersionID, api.CreatePhotoFaceRequest{FaceID: uuid.New().String(), Box: &document.FaceBox{X: x, Y: .1, Width: .2, Height: .3}})
		require.NoError(t, err)
	}
	for range 3 {
		wakeDaemon(t)
		settleDaemon(t)
	}
	require.NoError(t, client.Close())
	stop()
	stop = startServe(t)
	record = waitForDaemon(t, root)
	client = daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	for range 3 {
		wakeDaemon(t)
		settleDaemon(t)
	}
	require.NoError(t, client.Close())
	wakeDaemon(t)
	settleDaemon(t)
	_, err = runCLI(t, "backup", "init")
	require.NoError(t, err)
	_, err = runCLI(t, "backup", "create", "--jobs", "1")
	require.NoError(t, err)
	restoredRoot := filepath.Join(t.TempDir(), "restored")
	_, err = runCLI(t, "backup", "restore", "--target", restoredRoot, "--jobs", "1")
	require.NoError(t, err)

	stop()
	t.Setenv("DOCBANK_HOME", restoredRoot)
	stop = startServe(t)
	record = waitForDaemon(t, restoredRoot)
	client = daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	jobs, err := client.API().ListJobs(t.Context())
	require.NoError(t, err)
	require.Equal(t, "running", requireJob(t, *jobs, "process:face-suggestions").Status)
	require.NoError(t, client.Close())
	stop()
}
