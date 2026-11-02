package api_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// newBubbleTestServer builds the daemon newTestServer builds, but serves it
// over the synctest bubble's in-memory network. Call it inside synctest.Test:
// the server's timers then run on the bubble's fake clock, and synctest.Wait
// returns once its handlers are idle. The in-memory network has no loopback
// address, so requests are presented to the daemon as a loopback peer, as
// newTestServer's clients are.
func newBubbleTestServer(t *testing.T, mutate func(*api.Deps)) (*httptest.Server, *testStore) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dbPath := filepath.Join(dir, "docbank.db")
	s, err := store.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	blobsDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	d := api.Deps{
		Store: s, Blobs: blobs, VaultRoot: dir, Cfg: config.Default(),
		WebURL: testWebURL, EnsureEmail: processing.EnsureEmailTarget, PublishEmailDocuments: processing.PublishEmailDocuments,
	}
	d.Cfg.Server.APIKey = testAPIKey
	if mutate != nil {
		mutate(&d)
	}
	apiServer := api.NewServer(d)
	t.Cleanup(apiServer.Close)
	handler := apiServer.Handler()
	ts := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "127.0.0.1:1234"
		handler.ServeHTTP(w, r)
	}))
	ts.Client().Transport = &apiKeyTransport{key: d.Cfg.Server.APIKey, next: ts.Client().Transport}
	ts.URL = "http://127.0.0.1"
	return ts, &testStore{
		Store: s, Blobs: blobs, BlobsDir: blobsDir, DBPath: dbPath, Server: apiServer,
	}
}
