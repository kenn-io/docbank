package main

import (
	"net/http/httptest"
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
	for range 2 {
		_, err = client.API().ApplyFaces(t.Context(), &apiclient.ApplyFacesRequestOptions{Body: &api.FaceSetupApplyRequest{PlanFingerprint: plan.Fingerprint, Consent: true}})
		require.NoError(t, err)
	}
	for round := range 2 {
		settleDaemon(t)
		list, err = client.API().ListJobs(t.Context())
		require.NoError(t, err)
		count := 0
		for _, job := range list.Items {
			if job.Name == "process:face-suggestions" {
				count++
			}
		}
		require.Equal(t, 1, count)
		if round == 1 {
			break
		}
		stop()
		stop = startServe(t)
		record = waitForDaemon(t, root)
		client = daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	}
}
