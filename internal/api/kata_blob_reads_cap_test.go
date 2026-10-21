package api_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/kataevidence"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

type kataReadCountingCatalog struct {
	*store.PackCatalog

	reads int
}

func (c *kataReadCountingCatalog) ResolveLocations(ctx context.Context, hash packstore.Hash) (packstore.Resolution, error) {
	c.reads++
	return c.PackCatalog.ResolveLocations(ctx, hash)
}

func TestKataContextUnchangedOriginalAvoidsBlobReads(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	var catalog *kataReadCountingCatalog
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
		catalog = &kataReadCountingCatalog{PackCatalog: store.NewPackCatalog(d.Store)}
		var err error
		d.Blobs, err = blob.New(catalog, filepath.Join(d.VaultRoot, "blobs"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, d.Blobs.Close()) })
	})
	node := createFileWithContent(t, ts, s, "/notes.txt", "hello")
	response, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, kataevidence.CiteRequest{Title: "Review", IdempotencyKey: "count-reads", Selector: kataevidence.Selector{NodeID: node.ID, EndRune: 5}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Positive(t, catalog.reads, "the real blob reader must use the counting catalog")
	catalog.reads = 0
	response, body = do(t, ts, http.MethodPost, "/api/v1/kata/context", nil, kataevidence.ContextRequest{Issue: "docbank#" + issue().Issue.ShortID})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"state":"available"`)
	require.Contains(t, body, `"saved_quote":"hello"`)
	require.Zero(t, catalog.reads, "matching version and hash should use the saved quote")
}
