package api_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/docbank/internal/kataevidence"
)

func TestKataHTTPExplainsQuoteCapacity(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	node := createFileWithContent(t, ts, s, "/notes.txt", "hello")
	ref := kataevidence.Reference{Version: 1, Kind: "docbank", Docbank: &kataevidence.DocumentReference{VaultUID: s.VaultID(), NodeID: node.ID, ContentVersionID: node.CurrentVersionID, ContentSHA256: node.BlobHash, EndRune: 1}}
	entries := make([]kataevidence.Entry, 64)
	for i := range entries {
		ref.Docbank = new(*ref.Docbank)
		ref.Docbank.NodeID = int64(i + 100)
		entries[i] = kataevidence.Entry{ID: kataevidence.ID(ref), Passage: kataevidence.PassageID(ref, "h"), Reference: ref, Quote: "h", Label: "notes.txt"}
	}
	data, err := json.Marshal(map[string]any{"version": 1, "entries": entries})
	require.NoError(t, err)
	client := kataclient.New(config.KataConnection{Endpoint: kata.URL, APIKey: "synthetic-kata-token", Project: "docbank"})
	qualified := "docbank#" + issue().Issue.ShortID
	_, err = client.Patch(t.Context(), qualified, kataevidence.MetadataKey, nil, jsontext.Value(data))
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, kataevidence.CiteRequest{Issue: qualified, Selector: kataevidence.Selector{NodeID: node.ID, EndRune: 5}})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, `"code":"kata_evidence_limit"`)
	require.Contains(t, body, "issue already holds the maximum of 64 Docbank quotes")
}
