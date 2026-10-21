package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/docbank/internal/kataevidence"
)

func TestKataContextRejectsForgedOriginalQuote(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	node := createFileWithContent(t, ts, s, "/notes.txt", "hello")
	response, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, kataevidence.CiteRequest{Title: "Review", IdempotencyKey: "verify-quote", Selector: kataevidence.Selector{NodeID: node.ID, EndRune: 5}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	client := kataclient.New(config.KataConnection{Endpoint: kata.URL, APIKey: "synthetic-kata-token", Project: "docbank"})
	saved, err := client.Get(t.Context(), "docbank#"+issue().Issue.ShortID)
	require.NoError(t, err)
	var envelope struct {
		Version int                  `json:"version"`
		Entries []kataevidence.Entry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(saved.Metadata[kataevidence.MetadataKey], &envelope))
	require.Len(t, envelope.Entries, 1)
	response, body = do(t, ts, http.MethodPost, "/api/v1/kata/context", nil, kataevidence.ContextRequest{Issue: saved.QualifiedRef})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var original kataevidence.Context
	require.NoError(t, json.Unmarshal([]byte(body), &original))
	require.Len(t, original.Passages, 1)
	require.Equal(t, "available", original.Passages[0].State)
	require.Equal(t, "hello", original.Passages[0].Evidence.Excerpt)
	envelope.Entries[0].Quote = "phony"
	forged, err := json.Marshal(envelope)
	require.NoError(t, err)
	_, err = client.Patch(t.Context(), saved.QualifiedRef, kataevidence.MetadataKey, saved.Metadata[kataevidence.MetadataKey], forged)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/kata/context", nil, kataevidence.ContextRequest{Issue: saved.QualifiedRef})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page kataevidence.Context
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Passages, 1)
	require.Equal(t, "phony", page.Passages[0].SavedQuote)
	require.Equal(t, "failed", page.Passages[0].State)
	require.Empty(t, page.Passages[0].Evidence.Excerpt)
}
