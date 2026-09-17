package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"hash/crc32"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
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
	require.NoError(t, json.UnmarshalRead(sessionResponse.Body, &session))
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
	decoder := jsontext.NewDecoder(prepareResponse.Body)
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
		err := json.UnmarshalDecode(decoder, &event)
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
	decoder := jsontext.NewDecoder(response.Body)
	for {
		var event struct {
			Phase     string `json:"phase"`
			URL       string `json:"url"`
			VersionID string `json:"version_id"`
			BlobHash  string `json:"blob_hash"`
		}
		err := json.UnmarshalDecode(decoder, &event)
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
	require.NoError(t, json.UnmarshalRead(response.Body, &problem))
	require.NoError(t, response.Body.Close())
	assert.Equal(t, "download_selection_stale", problem.Code)
}

func TestWebPreviewRejectsIneligibleAndOversizedSourcesBeforeStaging(t *testing.T) {
	t.Run("active document MIME", func(t *testing.T) {
		ts, s := newTestServer(t, nil)
		hash, size, err := s.Blobs.Write(strings.NewReader("<script>top.location='https://example.invalid'</script>"))
		require.NoError(t, err)
		document, err := s.CreateFile(t.Context(), s.RootID(), "hostile.html", hash, size, "text/html")
		require.NoError(t, err)

		response := prepareWebDownload(t, ts, "", map[string]any{
			"node_id": document.ID, "revision": document.Revision,
			"version_id": document.CurrentVersionID, "blob_hash": document.BlobHash,
			"size": document.Size, "purpose": "preview",
		})
		assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode)
		var problem struct {
			Code string `json:"code"`
		}
		require.NoError(t, json.UnmarshalRead(response.Body, &problem))
		require.NoError(t, response.Body.Close())
		assert.Equal(t, "preview_unsupported", problem.Code)
	})

	t.Run("text larger than sixteen MiB", func(t *testing.T) {
		ts, s := newTestServer(t, nil)
		document := createFileWithContent(t, ts, s, "/oversized.txt", strings.Repeat("x", (16<<20)+1))

		response := prepareWebDownload(t, ts, "", map[string]any{
			"node_id": document.ID, "revision": document.Revision,
			"version_id": document.CurrentVersionID, "blob_hash": document.BlobHash,
			"size": document.Size, "purpose": "preview",
		})
		assert.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
		var problem struct {
			Code string `json:"code"`
		}
		require.NoError(t, json.UnmarshalRead(response.Body, &problem))
		require.NoError(t, response.Body.Close())
		assert.Equal(t, "preview_too_large", problem.Code)
	})
}

func TestWebPreviewTicketCancellationIsOwnerScopedAndSessionRevoked(t *testing.T) {
	ts, s := newTestServer(t, nil)
	document := createFileWithContent(t, ts, s, "/report.txt", "selected preview\n")
	firstSession := issueWebSession(t, ts)
	otherSession := issueWebSession(t, ts)
	authority := map[string]any{
		"node_id": document.ID, "revision": document.Revision,
		"version_id": document.CurrentVersionID, "blob_hash": document.BlobHash,
		"size": document.Size, "purpose": "preview",
	}

	firstURL := readyWebDownloadURL(t, prepareWebDownload(t, ts, firstSession, authority))
	response := cancelWebDownload(t, ts, otherSession, firstURL)
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
	response, err := ts.Client().Get(ts.URL + firstURL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())

	cancelledURL := readyWebDownloadURL(t, prepareWebDownload(t, ts, firstSession, authority))
	response = cancelWebDownload(t, ts, firstSession, cancelledURL)
	assert.Equal(t, http.StatusNoContent, response.StatusCode)
	require.NoError(t, response.Body.Close())
	response, err = ts.Client().Get(ts.URL + cancelledURL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())

	revokedURL := readyWebDownloadURL(t, prepareWebDownload(t, ts, firstSession, authority))
	revoke, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	revoke.Header.Set(api.WebSessionHeader, firstSession)
	response, err = ts.Client().Do(revoke)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, response.StatusCode)
	require.NoError(t, response.Body.Close())
	response, err = ts.Client().Get(ts.URL + revokedURL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestWebPreviewRejectsRasterDimensionsBeforePublishingTicket(t *testing.T) {
	ts, s := newTestServer(t, nil)
	content := oversizedPNGHeader(100_000, 100_000)
	hash, size, err := s.Blobs.Write(bytes.NewReader(content))
	require.NoError(t, err)
	document, err := s.CreateFile(t.Context(), s.RootID(), "oversized.png", hash, size, "image/png")
	require.NoError(t, err)

	response := prepareWebDownload(t, ts, "", map[string]any{
		"node_id": document.ID, "revision": document.Revision,
		"version_id": document.CurrentVersionID, "blob_hash": document.BlobHash,
		"size": document.Size, "purpose": "preview",
	})
	require.Equal(t, http.StatusOK, response.StatusCode)
	decoder := jsontext.NewDecoder(response.Body)
	var phases []string
	for {
		var event struct {
			Phase string `json:"phase"`
		}
		err := json.UnmarshalDecode(decoder, &event)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		phases = append(phases, event.Phase)
	}
	require.NoError(t, response.Body.Close())
	assert.Contains(t, phases, "error")
	assert.NotContains(t, phases, "ready")
}

func oversizedPNGHeader(width, height uint32) []byte {
	var out bytes.Buffer
	out.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8], ihdr[9], ihdr[10], ihdr[11], ihdr[12] = 8, 2, 0, 0, 0
	writePNGChunk(&out, "IHDR", ihdr)
	writePNGChunk(&out, "IEND", nil)
	return out.Bytes()
}

func writePNGChunk(out *bytes.Buffer, kind string, payload []byte) {
	_ = binary.Write(out, binary.BigEndian, uint32(len(payload)))
	_, _ = out.WriteString(kind)
	_, _ = out.Write(payload)
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte(kind))
	_, _ = crc.Write(payload)
	_ = binary.Write(out, binary.BigEndian, crc.Sum32())
}

func issueWebSession(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	response, err := ts.Client().Post(ts.URL+"/api/daemon/web-session", "", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.UnmarshalRead(response.Body, &session))
	require.NoError(t, response.Body.Close())
	return session.Token
}

func prepareWebDownload(
	t *testing.T, ts *httptest.Server, session string, authority map[string]any,
) *http.Response {
	t.Helper()
	body, err := json.Marshal(authority)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/web-download", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	if session != "" {
		request.Header["X-Api-Key"] = []string{""}
		request.Header.Set(api.WebSessionHeader, session)
	}
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	return response
}

func readyWebDownloadURL(t *testing.T, response *http.Response) string {
	t.Helper()
	require.Equal(t, http.StatusOK, response.StatusCode)
	decoder := jsontext.NewDecoder(response.Body)
	var readyURL string
	for {
		var event struct {
			Phase string `json:"phase"`
			URL   string `json:"url"`
		}
		err := json.UnmarshalDecode(decoder, &event)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if event.Phase == "ready" {
			readyURL = event.URL
		}
	}
	require.NoError(t, response.Body.Close())
	require.NotEmpty(t, readyURL)
	return readyURL
}

func cancelWebDownload(
	t *testing.T, ts *httptest.Server, session, readyURL string,
) *http.Response {
	t.Helper()
	ticket := strings.TrimPrefix(readyURL, "/api/daemon/web-download/file?ticket=")
	require.NotEqual(t, readyURL, ticket)
	request, err := http.NewRequest(
		http.MethodDelete, ts.URL+"/api/daemon/web-download?ticket="+ticket, nil,
	)
	require.NoError(t, err)
	request.Header["X-Api-Key"] = []string{""}
	request.Header.Set(api.WebSessionHeader, session)
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	return response
}
