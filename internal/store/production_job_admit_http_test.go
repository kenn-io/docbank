package store_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionJobAdmissionHTTPUsesFinalizedAuthority(t *testing.T) {
	vault, root, setID, revision, etag, _ := store.ProductionFinalizedJobHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/sets/" + setID + "/revisions/" + strconv.FormatInt(revision, 10) + "/jobs"
	const jobID = "77000000-0000-4000-8000-000000000041"
	const operationID = "77000000-0000-4000-8000-000000000042"
	post := func(path string, match int64, authorized bool, body []byte) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+path, bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if match != 0 {
			request.Header.Set("If-Match", strconv.FormatInt(match, 10))
		}
		if authorized {
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	requestBody := []byte(`{"job_id":"` + jobID + `","operation_id":"` + operationID + `"}`)
	status, _ := post(path, etag, false, requestBody)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = post(path, 0, true, requestBody)
	require.Equal(t, http.StatusPreconditionRequired, status)
	status, body := post(path, etag, true, requestBody)
	require.Equal(t, http.StatusCreated, status, string(body))
	var first api.ProductionJobStatus
	require.NoError(t, json.Unmarshal(body, &first))
	require.Equal(t, jobID, first.JobID)
	require.Equal(t, setID, first.SetID)
	require.Equal(t, "queued", first.State)
	status, body = post(path, etag, true, requestBody)
	require.Equal(t, http.StatusCreated, status, string(body))
	var replay api.ProductionJobStatus
	require.NoError(t, json.Unmarshal(body, &replay))
	require.Equal(t, first, replay)
	status, _ = post(path, etag+1, true, requestBody)
	require.Equal(t, http.StatusConflict, status)
	status, _ = post(path, etag, true, []byte(`{"job_id":"77000000-0000-4000-8000-000000000043","operation_id":"`+operationID+`"}`))
	require.Equal(t, http.StatusConflict, status)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientResult, err := client.AdmitProductionJob(t.Context(), setID, revision, etag,
		api.ProductionJobAdmissionRequest{JobID: jobID, OperationID: operationID})
	require.NoError(t, err)
	require.Equal(t, first, clientResult)
}
