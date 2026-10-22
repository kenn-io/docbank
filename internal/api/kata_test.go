package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/docbank/internal/kataevidence"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

func realKataForDocbank(t *testing.T) (string, func() *generated.ShowIssueResponse) {
	t.Helper()
	backend := katatest.New(t, katatest.WithToken("synthetic-kata-token"))
	sdk := backend.Client(t)
	_, err := sdk.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	client := kataclient.New(config.KataConnection{Endpoint: backend.Endpoint, APIKey: "synthetic-kata-token", Project: "docbank"})
	_, err = client.Create(t.Context(), "seed", "Synthetic issue", "", nil)
	require.NoError(t, err)
	return backend.Endpoint, func() *generated.ShowIssueResponse {
		list, err := sdk.ListAllIssues(t.Context(), &generated.ListAllIssuesRequestOptions{Query: &generated.ListAllIssuesQuery{Sort: new(generated.ListAllIssuesQuerySort("created")), Limit: new(int64(1))}})
		require.NoError(t, err)
		require.Len(t, list.Issues, 1)
		issue, err := sdk.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: list.Issues[0].ProjectID, Ref: list.Issues[0].UID}})
		require.NoError(t, err)
		return issue
	}
}

func TestKataHTTPDisabledAndAuth(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	request := kataevidence.CiteRequest{Title: "Follow up", IdempotencyKey: "test", Selector: kataevidence.Selector{NodeID: 2, EndRune: 5}}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
	require.Equal(t, 503, resp.StatusCode, body)
	require.Contains(t, body, "restart the daemon after changing [integrations.kata]")
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/cite", map[string]string{"X-Api-Key": ""}, request)
	require.Equal(t, 401, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/cite", map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}, request)
	require.Equal(t, 403, resp.StatusCode, body)
}

func TestKataHTTPCreateAppendAndStaleness(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	node := createFileWithContent(t, ts, s, "/notes.txt", "hello 🙂 world")
	request := kataevidence.CiteRequest{Title: "Follow up", IdempotencyKey: "test", Selector: kataevidence.Selector{NodeID: node.ID, StartRune: 6, EndRune: 7, Quote: "🙂", ExpectedVersion: node.CurrentVersionID}}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Contains(t, issue().Issue.Body, "> 🙂")
	metadata, err := json.Marshal(issue().Issue.Metadata[kataevidence.MetadataKey])
	require.NoError(t, err)
	require.Contains(t, string(metadata), `"kind":"docbank"`)
	contextRequest := kataevidence.ContextRequest{Issue: "docbank#" + issue().Issue.ShortID}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/context", nil, contextRequest)
	require.Equal(t, 200, resp.StatusCode, body)
	request.Title, request.IdempotencyKey, request.Issue = "", "", "docbank#"+issue().Issue.ShortID
	request.Selector.StartRune, request.Selector.EndRune, request.Selector.Quote = 0, 5, "hello"
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Len(t, issue().Comments, 1)
	data := []byte("changed document")
	replacement, response := sendContentReplacement(t, ts.URL, ts.Client(), node.ID, node.Revision, data, uploadIdentity(data), int64(len(data)), "text/plain")
	require.Equal(t, 200, replacement.StatusCode, string(response))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/context", nil, contextRequest)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, 2, strings.Count(body, `"state":"changed"`))
	require.Contains(t, body, `"saved_quote":"🙂"`)
	request.Selector.EndRune = 4
	request.Selector.Quote = "hell"
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
	require.Equal(t, 409, resp.StatusCode, body)
	trashed, body := do(t, ts, http.MethodPost, "/api/v1/nodes/"+strconv.FormatInt(node.ID, 10)+"/trash", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, 200, trashed.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/context", nil, contextRequest)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Contains(t, body, `"state":"unavailable"`)
}

func TestKataHTTPRenditionAndGeneratedClient(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	f := newCitationHTTPFixture(t, api.NewOperationGate())
	cfg := config.Default()
	cfg.Server.APIKey = testAPIKey
	cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata, APIKeyEnv: "DKE_API_KATA_KEY"}
	server := api.NewServer(api.Deps{Store: f.store.Store, Blobs: f.store.Blobs, VaultRoot: filepath.Dir(f.store.DBPath), Cfg: cfg, Processing: f.service})
	t.Cleanup(server.Close)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	c := daemonconn.New(ts.URL, testAPIKey)
	defer func() { _ = c.Close() }()
	r := kataevidence.CiteRequest{Title: "Transcript follow-up", IdempotencyKey: "rendition-1", Selector: kataevidence.Selector{NodeID: f.citation.NodeID, StartRune: f.citation.Start, EndRune: f.citation.End, Citation: &f.citation}}
	got, err := c.CiteKataEvidence(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, "docbank#"+issue().Issue.ShortID, got.QualifiedRef)
	require.Contains(t, issue().Issue.Body, "> Verified alpha")
	page, err := c.KataContext(t.Context(), kataevidence.ContextRequest{Issue: "docbank#" + issue().Issue.ShortID})
	require.NoError(t, err)
	require.Len(t, page.Passages, 1)
	require.Equal(t, "available", page.Passages[0].State)
	require.Equal(t, f.citation.BuildID, page.Passages[0].Evidence.Reference.Rendition.BuildID)
	data := []byte("changed source")
	client := ts.Client()
	client.Transport = &apiKeyTransport{key: testAPIKey, next: client.Transport}
	node, err := f.store.NodeByID(t.Context(), f.citation.NodeID)
	require.NoError(t, err)
	response, replaced := sendContentReplacement(t, ts.URL, client, node.ID, node.Revision, data, uploadIdentity(data), int64(len(data)), "text/plain")
	require.Equal(t, 200, response.StatusCode, string(replaced))
	page, err = c.KataContext(t.Context(), kataevidence.ContextRequest{Issue: "docbank#" + issue().Issue.ShortID})
	require.NoError(t, err)
	require.Equal(t, "changed", page.Passages[0].State)
}

func TestKataHTTPTextExtractionMediaCites(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	for _, media := range []string{"application/yaml", "application/x-ndjson", "application/json; charset=utf-8"} {
		t.Run(media, func(t *testing.T) {
			hash, size, err := s.Blobs.Write(strings.NewReader("hello"))
			require.NoError(t, err)
			node, err := s.CreateFile(t.Context(), s.RootID(), strings.TrimPrefix(media, "application/")+".txt", hash, size, media)
			require.NoError(t, err)
			response, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, kataevidence.CiteRequest{Title: "Review " + media, IdempotencyKey: media, Selector: kataevidence.Selector{NodeID: node.ID, EndRune: 5}})
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			require.Contains(t, issue().Issue.Body, "> hello")
		})
	}
}

func TestKataHTTPContextIdenticalRewriteAvailable(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	node := createFileWithContent(t, ts, s, "/notes.txt", "hello")
	response, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, kataevidence.CiteRequest{Title: "Review", IdempotencyKey: "rewrite", Selector: kataevidence.Selector{NodeID: node.ID, EndRune: 5}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var saved struct {
		Entries []kataevidence.Entry `json:"entries"`
	}
	metadata, err := json.Marshal(issue().Issue.Metadata[kataevidence.MetadataKey])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(metadata, &saved))
	require.Len(t, saved.Entries, 1)
	hash, size, err := s.Blobs.Write(strings.NewReader("hello"))
	require.NoError(t, err)
	_, version, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, hash, size, "text/plain")
	require.NoError(t, err)
	require.NotEqual(t, node.CurrentVersionID, version.ID)
	response, body = do(t, ts, http.MethodPost, "/api/v1/kata/context", nil, kataevidence.ContextRequest{Issue: "docbank#" + issue().Issue.ShortID})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page kataevidence.Context
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Passages, 1)
	require.Equal(t, "available", page.Passages[0].State)
	require.Equal(t, "hello", page.Passages[0].Evidence.Excerpt)
	require.Equal(t, saved.Entries[0].Reference, page.Passages[0].Evidence.Reference)
	require.Equal(t, saved.Entries[0].ID, page.Passages[0].Evidence.ID)
	require.Equal(t, node.CurrentVersionID, page.Passages[0].Evidence.Reference.Docbank.ContentVersionID)
	require.Equal(t, page.Passages[0].Evidence.ID, kataevidence.ID(page.Passages[0].Evidence.Reference))
}

func TestKataHTTPKeylessServerRejectsToken(t *testing.T) {
	kata := katatest.New(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.Endpoint, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	response, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, kataevidence.CiteRequest{Title: "Review", IdempotencyKey: "token", Selector: kataevidence.Selector{NodeID: 2, EndRune: 5}})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	require.Contains(t, body, `"code":"kata_token_invalid"`)
	require.Contains(t, body, "Kata token was rejected; configure Kata with that token")
	require.NotContains(t, body, "synthetic-kata-token")
}

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
