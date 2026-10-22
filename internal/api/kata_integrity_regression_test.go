package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/kataevidence"
)

func TestKataHTTPIntegrityFailureIsLocalAndPermanent(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	node := createFileWithContent(t, ts, s, "/notes.txt", "hello")
	require.NoError(t, os.WriteFile(filepath.Join(s.BlobsDir, node.BlobHash[:2], node.BlobHash), []byte("bad"), 0o600))
	response, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, kataevidence.CiteRequest{
		Title: "Review", IdempotencyKey: "integrity", Selector: kataevidence.Selector{NodeID: node.ID, EndRune: 5},
	})
	require.NotContains(t, strings.ToLower(body), "retry")
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
	require.Contains(t, body, `"code":"kata_evidence_integrity"`)
	require.Empty(t, issue().Issue.Body)
}
