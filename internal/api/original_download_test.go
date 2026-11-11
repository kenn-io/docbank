package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestOriginalDownloadTicket(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, func(d *api.Deps) { d.Cfg.Web.Enabled = false; d.WebURL = "" })
	content := "synthetic original bytes"
	document := createFileWithContent(t, ts, s, "/original.bin", content)
	// A retained older version must remain selectable after replacement.
	hash, size, err := s.Blobs.WriteContext(t.Context(), strings.NewReader("replacement"))
	require.NoError(t, err)
	current, _, err := s.ReplaceContent(t.Context(), document.ID, document.Revision, hash, size, "text/plain")
	require.NoError(t, err)
	selection := api.OriginalDownloadSelection{VaultID: s.VaultID(), NodeID: document.ID,
		BlobHash: document.BlobHash, Size: document.Size}
	prepare := func(selection api.OriginalDownloadSelection, status int) string {
		t.Helper()
		body, err := json.Marshal(selection)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			ts.URL+"/api/v1/versions/"+document.CurrentVersionID+"/download-ticket", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		response, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		require.Equal(t, status, response.StatusCode)
		if status != http.StatusOK {
			return ""
		}
		var ticket api.OriginalDownloadTicket
		require.NoError(t, json.UnmarshalRead(response.Body, &ticket))
		require.Equal(t, 120, ticket.ExpiresInSeconds)
		require.Equal(t, current.Name, ticket.Name)
		return ts.URL + ticket.URL
	}
	get := func(address string, status int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
		require.NoError(t, err)
		// Explicitly omit the API key on the capability request.
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		require.Equal(t, status, response.StatusCode)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		if status == http.StatusOK {
			digest := sha256.Sum256(data)
			require.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":", response.Trailer.Get("Content-Digest"))
		}
		return data
	}
	address := prepare(selection, http.StatusOK)
	downloaded := get(address, http.StatusOK)
	require.Equal(t, content, string(downloaded))
	digest := sha256.Sum256(downloaded)
	require.Equal(t, document.BlobHash, hex.EncodeToString(digest[:]))
	get(address, http.StatusNotFound)
	uppercase := selection
	uppercase.VaultID = strings.ToUpper(selection.VaultID)
	address = prepare(uppercase, http.StatusOK)
	require.Equal(t, content, string(get(address, http.StatusOK)))
	address = prepare(selection, http.StatusOK)
	// Renaming does not change the immutable selected original or burn a ticket.
	current, _, err = s.Move(t.Context(), current.ID, s.RootID(), "renamed.bin", current.Revision)
	require.NoError(t, err)
	require.Equal(t, content, string(get(address, http.StatusOK)))
	get(address, http.StatusNotFound)
	bad := selection
	bad.BlobHash = strings.Repeat("a", 64)
	prepare(bad, http.StatusConflict)
	bad = selection
	bad.Size++
	prepare(bad, http.StatusConflict)
	bad = selection
	bad.VaultID = "11111111-1111-4111-8111-111111111111"
	prepare(bad, http.StatusConflict)
	other := createFileWithContent(t, ts, s, "/other.bin", "another original")
	bad = selection
	bad.NodeID = other.ID
	prepare(bad, http.StatusNotFound)
	address = prepare(selection, http.StatusOK)
	_, _, err = s.Trash(t.Context(), document.ID, current.Revision)
	require.NoError(t, err)
	get(address, http.StatusConflict)
	prepare(selection, http.StatusConflict)
	{
		node := createFileWithContent(t, ts, s, "/corrupt.bin", "synthetic original bytes")
		body, err := json.Marshal(api.OriginalDownloadSelection{VaultID: s.VaultID(), NodeID: node.ID, BlobHash: node.BlobHash, Size: node.Size})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(s.BlobsDir, node.BlobHash[:2], node.BlobHash), []byte("synthetic original byte!"), 0o600))
		issue, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/versions/"+node.CurrentVersionID+"/download-ticket", bytes.NewReader(body))
		require.NoError(t, err)
		issue.Header.Set("Content-Type", "application/json")
		response, err := ts.Client().Do(issue)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		var ticket api.OriginalDownloadTicket
		require.NoError(t, json.UnmarshalRead(response.Body, &ticket))
		require.NoError(t, response.Body.Close())
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+ticket.URL, nil)
		require.NoError(t, err)
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		response, err = client.Do(req)
		if err == nil {
			_, err = io.ReadAll(response.Body)
			require.Empty(t, response.Trailer.Get("Content-Digest"))
			_ = response.Body.Close()
		}
		require.Error(t, err)
		replay, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, replay.StatusCode)
		require.NoError(t, replay.Body.Close())
	}
}

func TestOriginalDownloadTicketExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s, key := newTestAPIServer(t, func(d *api.Deps) { d.Cfg.Web.Enabled = false; d.WebURL = "" })
		hash, size, err := s.Blobs.WriteContext(t.Context(), strings.NewReader("synthetic original"))
		require.NoError(t, err)
		node, err := s.CreateFile(t.Context(), s.RootID(), "expiry.bin", hash, size, "text/plain")
		require.NoError(t, err)
		body, err := json.Marshal(api.OriginalDownloadSelection{VaultID: s.VaultID(), NodeID: node.ID,
			BlobHash: hash, Size: size})
		require.NoError(t, err)
		response := serveLoopback(t.Context(), s.Server.Handler(), http.MethodPost,
			"/api/v1/versions/"+node.CurrentVersionID+"/download-ticket",
			map[string]string{"X-Api-Key": key, "Content-Type": "application/json"}, bytes.NewReader(body))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var ticket api.OriginalDownloadTicket
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &ticket))
		time.Sleep(2*time.Minute + time.Nanosecond)
		expired := serveLoopback(t.Context(), s.Server.Handler(), http.MethodGet, ticket.URL, nil, nil)
		require.Equal(t, http.StatusNotFound, expired.Code)
		require.Contains(t, expired.Body.String(), "download_not_found")
	})
}
