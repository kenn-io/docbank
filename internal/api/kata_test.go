package api_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/docbank/internal/kataevidence"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

func realKataForDocbank(t *testing.T) (*httptest.Server, func() kataclient.Issue) {
	t.Helper()
	return realKataForDocbankWithDelay(t, nil)
}

func realKataForDocbankWithDelay(t *testing.T, delayRequest func(*http.Request) bool) (*httptest.Server, func() kataclient.Issue) {
	t.Helper()
	backend := katatest.New(t, katatest.WithToken("synthetic-kata-token"))
	sdk := backend.Client(t)
	_, err := sdk.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	client := kataclient.New(config.KataConnection{Endpoint: backend.Endpoint, APIKey: "synthetic-kata-token", Project: "docbank"})
	_, err = client.Create(t.Context(), "seed", "Synthetic issue", "", nil)
	require.NoError(t, err)
	target, err := url.Parse(backend.Endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delayRequest != nil && delayRequest(r) {
			timer := time.NewTimer(6 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, func() kataclient.Issue {
		list, err := sdk.ListAllIssues(t.Context(), &generated.ListAllIssuesRequestOptions{Query: &generated.ListAllIssuesQuery{Sort: new(generated.ListAllIssuesQuerySort("created")), Limit: new(int64(1))}})
		require.NoError(t, err)
		require.Len(t, list.Issues, 1)
		issue, err := client.Get(t.Context(), list.Issues[0].ProjectName+"#"+list.Issues[0].UID)
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
	require.Contains(t, body, "kata integration is disabled")
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/cite", map[string]string{"X-Api-Key": ""}, request)
	require.Equal(t, 401, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/cite", map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}, request)
	require.Equal(t, 403, resp.StatusCode, body)
}

func TestKataHTTPCreateAppendAndStaleness(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	node := createFileWithContent(t, ts, s, "/notes.txt", "hello 🙂 world")
	request := kataevidence.CiteRequest{Title: "Follow up", IdempotencyKey: "test", Selector: kataevidence.Selector{NodeID: node.ID, StartRune: 6, EndRune: 7, Quote: "🙂", ExpectedVersion: node.CurrentVersionID}}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Contains(t, issue().Body, "> 🙂")
	require.Contains(t, string(issue().Metadata[kataevidence.MetadataKey]), `"kind":"docbank"`)
	contextRequest := kataevidence.ContextRequest{Issue: issue().QualifiedRef}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/kata/context", nil, contextRequest)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Contains(t, body, `"state":"available"`)
	request.Title, request.IdempotencyKey, request.Issue = "", "", issue().QualifiedRef
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

func TestKataHTTPRoutesOutlastFiveSecondBodyReadTimeout(t *testing.T) {
	tests := []struct {
		name         string
		delayRequest func(*http.Request) bool
		request      func(*testing.T, *httptest.Server, *testStore, string) (*http.Response, string)
	}{
		{
			name: "cite",
			delayRequest: func(r *http.Request) bool {
				return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues")
			},
			request: func(t *testing.T, ts *httptest.Server, s *testStore, _ string) (*http.Response, string) {
				t.Helper()
				node := createFileWithContent(t, ts, s, "/notes.txt", "hello")
				request := kataevidence.CiteRequest{
					Title: "Follow up", IdempotencyKey: "slow-create",
					Selector: kataevidence.Selector{NodeID: node.ID, EndRune: 5, Quote: "hello", ExpectedVersion: node.CurrentVersionID},
				}
				return do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
			},
		},
		{
			name: "context",
			delayRequest: func(r *http.Request) bool {
				return r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/")
			},
			request: func(t *testing.T, ts *httptest.Server, _ *testStore, ref string) (*http.Response, string) {
				t.Helper()
				return do(t, ts, http.MethodPost, "/api/v1/kata/context", nil, kataevidence.ContextRequest{Issue: ref})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kata, issue := realKataForDocbankWithDelay(t, tt.delayRequest)
			t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
			ts, s := newTestServer(t, func(d *api.Deps) {
				d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
			})

			ref := issue().QualifiedRef
			started := time.Now()
			response, body := tt.request(t, ts, s, ref)
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			require.GreaterOrEqual(t, time.Since(started), 6*time.Second)
		})
	}
}

func TestKataHTTPOriginalPreparationFailuresAreNotRetryable(t *testing.T) {
	kata, _ := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	tests := []struct {
		name       string
		mimeType   string
		content    []byte
		contentLen int
		status     int
		code       string
		detail     string
	}{
		{name: "too_large", mimeType: "text/plain", contentLen: (16 << 20) + 1, status: http.StatusConflict, code: "kata_evidence_unsupported", detail: "document exceeds"},
		{name: "unsupported_media", mimeType: "application/pdf", content: []byte("synthetic PDF"), status: http.StatusConflict, code: "kata_evidence_unsupported", detail: "retained text rendition"},
		{name: "invalid_utf8", mimeType: "text/plain", content: []byte{0xff}, status: http.StatusUnprocessableEntity, code: "invalid_kata_evidence", detail: "not UTF-8 text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := tt.content
			if tt.contentLen > 0 {
				content = bytes.Repeat([]byte("x"), tt.contentLen)
			}
			hash, size, err := s.Blobs.Write(bytes.NewReader(content))
			require.NoError(t, err)
			node, err := s.CreateFile(t.Context(), s.RootID(), tt.name+".txt", hash, size, tt.mimeType)
			require.NoError(t, err)
			request := kataevidence.CiteRequest{
				Title: "Prepare citation", IdempotencyKey: "prepare-" + tt.name,
				Selector: kataevidence.Selector{NodeID: node.ID, EndRune: 1},
			}
			response, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
			require.Equal(t, tt.status, response.StatusCode, body)
			require.Contains(t, body, `"code":"`+tt.code+`"`)
			require.Contains(t, body, tt.detail)
			require.NotContains(t, strings.ToLower(body), "retry")
		})
	}
}

func TestKataHTTPRejectsMismatchedQuoteBeforeKataWrite(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	node := createFileWithContent(t, ts, s, "/notes.txt", "hello")
	request := kataevidence.CiteRequest{Title: "Follow up", IdempotencyKey: "test", Selector: kataevidence.Selector{NodeID: node.ID, EndRune: 5, Quote: "other"}}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
	require.Equal(t, 409, resp.StatusCode, body)
	require.Empty(t, issue().Body)
}

func TestKataHTTPRenditionAndGeneratedClient(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	f := newCitationHTTPFixture(t, api.NewOperationGate())
	cfg := config.Default()
	cfg.Server.APIKey = testAPIKey
	cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
	server := api.NewServer(api.Deps{Store: f.store.Store, Blobs: f.store.Blobs, VaultRoot: filepath.Dir(f.store.DBPath), Cfg: cfg, Processing: f.service})
	t.Cleanup(server.Close)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	c := daemonconn.New(ts.URL, testAPIKey)
	defer func() { _ = c.Close() }()
	r := kataevidence.CiteRequest{Title: "Transcript follow-up", IdempotencyKey: "rendition-1", Selector: kataevidence.Selector{NodeID: f.citation.NodeID, StartRune: f.citation.Start, EndRune: f.citation.End, Citation: &f.citation}}
	got, err := c.CiteKataEvidence(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, issue().QualifiedRef, got.QualifiedRef)
	require.Contains(t, issue().Body, "> Verified alpha")
	page, err := c.KataContext(t.Context(), kataevidence.ContextRequest{Issue: issue().QualifiedRef})
	require.NoError(t, err)
	require.Len(t, page.Passages, 1)
	require.Equal(t, "available", page.Passages[0].State)
	require.Equal(t, f.citation.BuildID, page.Passages[0].Evidence.Reference.Docbank.BuildID)
	data := []byte("changed source")
	client := ts.Client()
	client.Transport = &apiKeyTransport{key: testAPIKey, next: client.Transport}
	node, err := f.store.NodeByID(t.Context(), f.citation.NodeID)
	require.NoError(t, err)
	response, body := sendContentReplacement(t, ts.URL, client, node.ID, node.Revision, data, uploadIdentity(data), int64(len(data)), "text/plain")
	require.Equal(t, 200, response.StatusCode, string(body))
	page, err = c.KataContext(t.Context(), kataevidence.ContextRequest{Issue: issue().QualifiedRef})
	require.NoError(t, err)
	require.Equal(t, "changed", page.Passages[0].State)
}

func TestKataHTTPPathCreateReplaysAfterSourceDisappears(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	node := createFileWithContent(t, ts, s, "/notes.txt", "hello")
	request := map[string]any{"title": "Follow up", "idempotency_key": "path-replay", "selector": map[string]any{"path": "/notes.txt", "start_rune": 0, "end_rune": 5, "quote": "hello"}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
	require.Equal(t, 200, response.StatusCode, body)
	original := issue().Body
	response, body = do(t, ts, http.MethodPost, "/api/v1/nodes/"+strconv.FormatInt(node.ID, 10)+"/trash", map[string]string{"If-Match": `"1"`}, nil)
	require.Equal(t, 200, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
	require.Equal(t, 200, response.StatusCode, body)
	require.Equal(t, original, issue().Body)
}

func TestKataHTTPJSONOriginalWithCharset(t *testing.T) {
	kata, issue := realKataForDocbank(t)
	t.Setenv("DKE_API_KATA_KEY", "synthetic-kata-token")
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.Kata = config.KataConfig{Enabled: true, Endpoint: kata.URL, APIKeyEnv: "DKE_API_KATA_KEY"}
	})
	node := createFileWithContent(t, ts, s, "/notes.json", "initial")
	data := []byte(`{"note":"hello"}`)
	replacement, response := sendContentReplacement(t, ts.URL, ts.Client(), node.ID, node.Revision, data, uploadIdentity(data), int64(len(data)), "application/json; charset=utf-8")
	require.Equal(t, http.StatusOK, replacement.StatusCode, string(response))
	request := kataevidence.CiteRequest{Title: "Review JSON", IdempotencyKey: "json-1", Selector: kataevidence.Selector{NodeID: node.ID, EndRune: len(data)}}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/kata/cite", nil, request)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "Evidence: notes.json\n\n> {\"note\":\"hello\"}\n", issue().Body)
}
