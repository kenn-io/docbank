package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPrepareOriginalDownload(t *testing.T) {
	const version = "11111111-1111-4111-8111-111111111111"
	const path = "/api/daemon/web-download/file?ticket=secret"
	selection := api.OriginalDownloadSelection{VaultID: version, NodeID: 1, BlobHash: strings.Repeat("a", 64), Size: 42}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/versions/"+version+"/download-ticket", r.URL.Path)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		var actual api.OriginalDownloadSelection
		assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
		assert.Equal(t, selection, actual)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.OriginalDownloadTicket{URL: path, Name: "original.bin", ExpiresInSeconds: 37}))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "synthetic-key")
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	ticket, err := c.PrepareOriginalDownload(t.Context(), version, selection)
	require.NoError(t, err)
	require.Equal(t, server.URL+path, ticket.URL)
	require.Equal(t, 37, ticket.ExpiresInSeconds)
}
