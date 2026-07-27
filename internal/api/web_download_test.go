package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestWebDownloadVerifiesBeforeOneUseBrowserHandoff(t *testing.T) {
	ts, s := newTestServer(t, nil)
	const content = "synthetic quarterly report\n"
	document := createFileWithContent(t, ts, s, "/quarterly-report.txt", content)

	sessionRequest, err := http.NewRequest(
		http.MethodPost, ts.URL+"/api/daemon/web-session", nil,
	)
	require.NoError(t, err)
	sessionResponse, err := ts.Client().Do(sessionRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, sessionResponse.StatusCode)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.NewDecoder(sessionResponse.Body).Decode(&session))
	require.NoError(t, sessionResponse.Body.Close())

	requestBody, err := json.Marshal(map[string]any{
		"node_id": document.ID, "revision": document.Revision,
		"version_id": document.CurrentVersionID, "blob_hash": document.BlobHash,
		"size": document.Size,
	})
	require.NoError(t, err)
	prepareRequest, err := http.NewRequest(
		http.MethodPost, ts.URL+"/api/daemon/web-download", bytes.NewReader(requestBody),
	)
	require.NoError(t, err)
	prepareRequest.Header["X-Api-Key"] = []string{""}
	prepareRequest.Header.Set(api.WebSessionHeader, session.Token)
	prepareRequest.Header.Set("Content-Type", "application/json")
	prepareResponse, err := ts.Client().Do(prepareRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, prepareResponse.StatusCode)
	assert.Equal(t, "application/x-ndjson", prepareResponse.Header.Get("Content-Type"))

	var events []struct {
		Phase     string `json:"phase"`
		Received  int64  `json:"received"`
		Total     int64  `json:"total"`
		URL       string `json:"url"`
		Name      string `json:"name"`
		VersionID string `json:"version_id"`
		BlobHash  string `json:"blob_hash"`
	}
	decoder := json.NewDecoder(prepareResponse.Body)
	for {
		var event struct {
			Phase     string `json:"phase"`
			Received  int64  `json:"received"`
			Total     int64  `json:"total"`
			URL       string `json:"url"`
			Name      string `json:"name"`
			VersionID string `json:"version_id"`
			BlobHash  string `json:"blob_hash"`
		}
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		events = append(events, event)
	}
	require.NoError(t, prepareResponse.Body.Close())
	require.GreaterOrEqual(t, len(events), 2)
	assert.Equal(t, "progress", events[0].Phase)
	ready := events[len(events)-1]
	assert.Equal(t, "ready", ready.Phase)
	assert.Equal(t, document.Size, ready.Received)
	assert.Equal(t, document.Size, ready.Total)
	assert.Equal(t, document.Name, ready.Name)
	assert.Equal(t, document.CurrentVersionID, ready.VersionID)
	assert.Equal(t, document.BlobHash, ready.BlobHash)
	require.Contains(t, ready.URL, "?ticket=")

	downloadRequest, err := http.NewRequest(http.MethodGet, ts.URL+ready.URL, nil)
	require.NoError(t, err)
	downloadRequest.Header["X-Api-Key"] = []string{""}
	downloadResponse, err := ts.Client().Do(downloadRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, downloadResponse.StatusCode)
	downloaded, err := io.ReadAll(downloadResponse.Body)
	require.NoError(t, err)
	require.NoError(t, downloadResponse.Body.Close())
	assert.Equal(t, content, string(downloaded))
	assert.Equal(t, strconv.FormatInt(document.Size, 10),
		downloadResponse.Header.Get("Content-Length"))
	assert.Equal(t, document.CurrentVersionID,
		downloadResponse.Header.Get(api.ContentVersionHeader))
	assert.Equal(t, document.BlobHash, downloadResponse.Header.Get(api.BlobHashHeader))
	sum := sha256.Sum256([]byte(content))
	assert.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":",
		downloadResponse.Header.Get("Content-Digest"))
	disposition, params, err := mime.ParseMediaType(
		downloadResponse.Header.Get("Content-Disposition"),
	)
	require.NoError(t, err)
	assert.Equal(t, "attachment", disposition)
	assert.Equal(t, document.Name, params["filename"])

	reusedRequest, err := http.NewRequest(http.MethodGet, ts.URL+ready.URL, nil)
	require.NoError(t, err)
	reusedRequest.Header["X-Api-Key"] = []string{""}
	reusedResponse, err := ts.Client().Do(reusedRequest)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, reusedResponse.StatusCode)
	require.NoError(t, reusedResponse.Body.Close())
}

func TestWebDownloadPreparesOneRetainedVersion(t *testing.T) {
	ts, s := newTestServer(t, nil)
	const historicalContent = "synthetic first edition\n"
	document := createFileWithContent(t, ts, s, "/report.txt", historicalContent)
	historical, err := s.ContentVersionByID(t.Context(), document.CurrentVersionID)
	require.NoError(t, err)
	replacementHash, replacementSize, err := s.Blobs.Write(strings.NewReader("second edition\n"))
	require.NoError(t, err)
	document, _, err = s.ReplaceContent(
		t.Context(), document.ID, document.Revision,
		replacementHash, replacementSize, "text/plain",
	)
	require.NoError(t, err)

	requestBody, err := json.Marshal(map[string]any{
		"node_id": document.ID, "revision": document.Revision,
		"version_id": historical.ID, "blob_hash": historical.BlobHash,
		"size": historical.Size,
	})
	require.NoError(t, err)
	request, err := http.NewRequest(
		http.MethodPost, ts.URL+"/api/daemon/web-download", bytes.NewReader(requestBody),
	)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)

	var ready struct {
		Phase     string `json:"phase"`
		URL       string `json:"url"`
		VersionID string `json:"version_id"`
		BlobHash  string `json:"blob_hash"`
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var event struct {
			Phase     string `json:"phase"`
			URL       string `json:"url"`
			VersionID string `json:"version_id"`
			BlobHash  string `json:"blob_hash"`
		}
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if event.Phase == "ready" {
			ready = event
		}
	}
	require.NoError(t, response.Body.Close())
	require.NotEmpty(t, ready.URL)
	assert.Equal(t, historical.ID, ready.VersionID)
	assert.Equal(t, historical.BlobHash, ready.BlobHash)

	download, err := ts.Client().Get(ts.URL + ready.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(download.Body)
	require.NoError(t, err)
	require.NoError(t, download.Body.Close())
	require.Equal(t, http.StatusOK, download.StatusCode)
	assert.Equal(t, historicalContent, string(body))
	assert.Equal(t, historical.ID, download.Header.Get(api.ContentVersionHeader))
	assert.Equal(t, historical.BlobHash, download.Header.Get(api.BlobHashHeader))
}

func TestWebDownloadRejectsAStaleSelectionBeforeStaging(t *testing.T) {
	ts, s := newTestServer(t, nil)
	document := createFileWithContent(t, ts, s, "/report.txt", "report")

	requestBody, err := json.Marshal(map[string]any{
		"node_id": document.ID, "revision": document.Revision + 1,
		"version_id": document.CurrentVersionID, "blob_hash": document.BlobHash,
		"size": document.Size,
	})
	require.NoError(t, err)
	request, err := http.NewRequest(
		http.MethodPost, ts.URL+"/api/daemon/web-download", bytes.NewReader(requestBody),
	)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, response.StatusCode)
	var problem struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&problem))
	require.NoError(t, response.Body.Close())
	assert.Equal(t, "download_selection_stale", problem.Code)
}
