package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionPreviewTicketRejectsSameSizeTamperBeforeDelivery(t *testing.T) {
	const original = "[REDACTED]"
	const changed = "[EXPOSED!]"
	require.Len(t, changed, len(original))
	root := t.TempDir()
	path := filepath.Join(root, "preview.txt")
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	sum := sha256.Sum256([]byte(original))
	downloads := newWebDownloadRegistry(root)
	token, err := downloads.issue(webDownloadTicket{
		path: path, name: "preview.txt", mediaType: "text/plain; charset=utf-8",
		blobHash: hex.EncodeToString(sum[:]), size: int64(len(original)), owner: "master",
		verifyDigestOnDelivery: true,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	registerWebDownload(mux, false, Deps{}, downloads, nil)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	require.NoError(t, os.WriteFile(path, []byte(changed), 0o600))
	response, err := server.Client().Get(server.URL + webDownloadFilePath + "?ticket=" + token)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusGone, response.StatusCode)
	require.NotContains(t, string(body), changed)
	require.Empty(t, response.Header.Get("Content-Digest"))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	response, err = server.Client().Get(server.URL + webDownloadFilePath + "?ticket=" + token)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestProductionPreviewTicketDeliversMatchingVerifiedBytes(t *testing.T) {
	const original = "[REDACTED]"
	root := t.TempDir()
	path := filepath.Join(root, "preview.txt")
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	sum := sha256.Sum256([]byte(original))
	downloads := newWebDownloadRegistry(root)
	token, err := downloads.issue(webDownloadTicket{
		path: path, name: "preview.txt", mediaType: "text/plain; charset=utf-8",
		blobHash: hex.EncodeToString(sum[:]), size: int64(len(original)), owner: "master",
		verifyDigestOnDelivery: true,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	registerWebDownload(mux, false, Deps{}, downloads, nil)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	response, err := server.Client().Get(server.URL + webDownloadFilePath + "?ticket=" + token)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, original, string(body))
	require.Equal(t, hex.EncodeToString(sum[:]), response.Header.Get(BlobHashHeader))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
