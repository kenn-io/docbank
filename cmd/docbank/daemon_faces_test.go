package main

import (
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	kitdaemon "go.kenn.io/kit/daemon"
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
