package store_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionMapChunksReassembleExactRetainedArtifact(t *testing.T) {
	vault, _, set, draft, member := store.ProductionReviewHTTPFixture(t)
	var artifact []byte
	cursor := ""
	for {
		page, err := vault.ProductionMapChunk(t.Context(), set.ID, draft.Revision, member.ID, cursor, 17)
		require.NoError(t, err)
		require.Equal(t, member.MapSHA256, page.MapSHA256)
		require.Equal(t, int64(len(artifact)), page.Offset)
		chunk, err := base64.StdEncoding.DecodeString(page.Data)
		require.NoError(t, err)
		require.NotEmpty(t, chunk)
		require.LessOrEqual(t, len(chunk), 17)
		digest := sha256.Sum256(chunk)
		require.Equal(t, hex.EncodeToString(digest[:]), page.ChunkSHA256)
		artifact = append(artifact, chunk...)
		if page.NextCursor == "" {
			require.Equal(t, int64(len(artifact)), page.TotalBytes)
			break
		}
		cursor = page.NextCursor
	}
	_, mapDigest, err := redaction.DecodeTextMap(artifact)
	require.NoError(t, err)
	require.Equal(t, member.MapSHA256, mapDigest)
	_, err = vault.ProductionMapChunk(t.Context(), set.ID, draft.Revision, member.ID, "bad", 17)
	require.ErrorIs(t, err, store.ErrInvalidProduction)
	_, err = vault.ProductionMapChunk(t.Context(), set.ID, draft.Revision, member.ID, "", 65537)
	require.ErrorIs(t, err, store.ErrInvalidProduction)
	_, err = vault.ProductionMapChunk(t.Context(), set.ID, draft.Revision, "89000000-0000-4000-8000-000000000099", "", 17)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestProductionMapChunkHTTPAndGeneratedClient(t *testing.T) {
	vault, root, set, draft, member := store.ProductionReviewHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/sets/" + set.ID + "/revisions/1/maps/" + member.ID
	get := func(path string, authorized bool) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpServer.URL+path, nil)
		require.NoError(t, err)
		if authorized {
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, body
	}
	status, _ := get(path+"?limit=17", false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, body := get(path+"?limit=17", true)
	require.Equal(t, http.StatusOK, status, string(body))
	var first api.ProductionMapChunk
	require.NoError(t, json.Unmarshal(body, &first))
	require.Equal(t, member.MapSHA256, first.MapSHA256)
	require.NotEmpty(t, first.NextCursor)
	status, body = get(path+"?limit=17&cursor="+url.QueryEscape(first.NextCursor), true)
	require.Equal(t, http.StatusOK, status, string(body))
	var second api.ProductionMapChunk
	require.NoError(t, json.Unmarshal(body, &second))
	require.EqualValues(t, 17, second.Offset)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientPage, err := client.ProductionMapChunk(t.Context(), set.ID, draft.Revision, member.ID, "", 17)
	require.NoError(t, err)
	require.Equal(t, first, clientPage)
	status, _ = get(path+"?cursor=bad", true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = get(path+"?limit=65537", true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = get("/api/v1/productions/sets/"+set.ID+"/revisions/1/maps/89000000-0000-4000-8000-000000000099", true)
	require.Equal(t, http.StatusNotFound, status)
}
