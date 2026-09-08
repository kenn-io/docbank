package qmdbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/qmdexport"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestSearchMapsOnlySelectedCurrentManifestThroughLiveAuthority(t *testing.T) {
	root, source, receipt := exportFixture(t)
	normalized := store.SearchOptions{MIMEType: "application/pdf"}
	authority := &authorityStub{normalized: normalized, live: []store.QMDExportLiveCandidate{{NodeID: source.NodeID,
		NodeRevision: 4, ContentVersionID: source.ContentVersionID, Path: "/live/document.pdf"}}}
	authorizer := &authorizerStub{}
	var raw []byte
	server := qmdServer(t, func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization"))
		assert.Equal(t, "/query", request.URL.Path)
		var err error
		raw, err = io.ReadAll(request.Body)
		if !assert.NoError(t, err) {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = writer.Write([]byte(`{"results":[{"docid":"#abc123","file":"` + receipt.Manifest.Entries[0].URI + `","title":"remote title","score":0.75,"context":null,"snippet":"bounded match"}]}`))
	})
	client := newTestClient(t, server, root, authorizer, authority, 1<<20)
	rerank := false
	results, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchLexical, Query: "private terms"}},
		Intent: "find relevant", Limit: 5, MinScore: 0.25, Rerank: &rerank, Scope: store.SearchOptions{MIMEType: "application/pdf; charset=utf-8"}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	wantPayload := `{"searches":[{"type":"lex","query":"private terms"}],"limit":5,"minScore":0.25,"collections":["docbank"],"intent":"find relevant","rerank":false}`
	if string(raw) != wantPayload {
		t.Errorf("unexpected QMD wire payload: %s", raw)
	}
	assert.NotContains(t, string(raw), source.ContentVersionID)
	assert.NotContains(t, string(raw), "Scope")
	assert.Equal(t, source.NodeID, results[0].Document.NodeID)
	assert.Equal(t, source.ContentVersionID, results[0].Document.ContentVersionID)
	assert.Equal(t, source.VaultUID, results[0].Document.VaultID)
	assert.Equal(t, int64(4), results[0].NodeRevision)
	assert.Equal(t, "/live/document.pdf", results[0].Path)
	assert.Equal(t, "bounded match", results[0].Excerpt)
	assert.NotEqual(t, "remote title", results[0].Path)
	assert.Equal(t, receipt.GenerationID, results[0].GenerationID)
	assert.Equal(t, receipt.Manifest.Checksum, results[0].ManifestChecksum)
	assert.Equal(t, source, authority.gotCandidates())
	operation := authorizer.gotOperation()
	assert.Equal(t, normalized, operation.Scope)
	assert.Equal(t, 1, operation.QueryCount)
	assert.Equal(t, 5, operation.CandidateLimit)
	assert.Equal(t, len("private terms")+len("find relevant"), operation.DisclosedBytes)
}

func TestSearchRejectsUnknownAndDuplicateURI(t *testing.T) {
	root, _, receipt := exportFixture(t)
	item := func(uri string) string {
		return `{"docid":"#abc","file":"` + uri + `","title":"x","score":0.5,"context":null,"snippet":"x"}`
	}
	for name, response := range map[string]string{
		"unknown URI":   `{"results":[` + item("qmd://docbank/documents/ff/unknown.md") + `]}`,
		"duplicate URI": `{"results":[` + item(receipt.Manifest.Entries[0].URI) + `,` + item(receipt.Manifest.Entries[0].URI) + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := staticQMDServer(t, response, "application/json", http.StatusOK)
			client := newTestClient(t, server, root, &authorizerStub{}, &authorityStub{}, 1<<20)
			_, err := client.Search(t.Context(), validRequest())
			require.ErrorIs(t, err, ErrInvalidResponse)
		})
	}
}

func TestSearchPreservesDistinctSameNodeEntriesInRemoteOrder(t *testing.T) {
	root, source, _ := exportFixture(t)
	other := source
	other.ProcessingProfileFingerprint = strings.Repeat("c", 64)
	other.AttachmentID = "attachment-second-profile"
	receipt, err := qmdexport.Publish(t.Context(), root, "docbank", []store.QMDExportSource{source, other},
		blobReader{source.BlobSHA256: []byte("# Searchable\n")}, qmdexport.Options{})
	require.NoError(t, err)
	require.Len(t, receipt.Manifest.Entries, 2)
	first, second := receipt.Manifest.Entries[1], receipt.Manifest.Entries[0]
	response := `{"results":[` + wireResultJSON(first.URI, 0.9, "first remote") + `,` +
		wireResultJSON(second.URI, 0.4, "second remote") + `]}`
	authority := &authorityStub{live: []store.QMDExportLiveCandidate{
		{NodeID: source.NodeID, NodeRevision: 5, ContentVersionID: source.ContentVersionID, Path: "/current.pdf"},
		{NodeID: source.NodeID, NodeRevision: 5, ContentVersionID: source.ContentVersionID, Path: "/current.pdf"},
	}}
	client := newTestClient(t, staticQMDServer(t, response, "application/json", http.StatusOK), root, &authorizerStub{}, authority, 1<<20)
	results, err := client.Search(t.Context(), validRequest())
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, first.URI, results[0].QMDURI)
	assert.Equal(t, first.AttachmentID, results[0].AttachmentID)
	assert.InDelta(t, 0.9, results[0].Score, 0)
	assert.Equal(t, "first remote", results[0].Excerpt)
	assert.Equal(t, second.URI, results[1].QMDURI)
	assert.Equal(t, second.AttachmentID, results[1].AttachmentID)
	assert.InDelta(t, 0.4, results[1].Score, 0)
	assert.Equal(t, "second remote", results[1].Excerpt)
}

func wireResultJSON(uri string, score float64, snippet string) string {
	return `{"docid":"#synthetic","file":"` + uri + `","title":"remote","score":` + strconv.FormatFloat(score, 'f', -1, 64) + `,"context":null,"snippet":"` + snippet + `"}`
}

func TestSearchRejectsResponseBoundaryStatusAndContentType(t *testing.T) {
	root, _, _ := exportFixture(t)
	tests := []struct {
		name, body, contentType string
		status                  int
		max                     int64
		target                  error
	}{
		{"bytes plus one", `{"results":[],"padding":"` + strings.Repeat("x", 512) + `"}`, "application/json", http.StatusOK, 64, ErrResponseBound},
		{"status", `{"results":[]}`, "application/json", http.StatusUnauthorized, 1 << 20, ErrInvalidResponse},
		{"missing content type", `{"results":[]}`, "", http.StatusOK, 1 << 20, ErrInvalidResponse},
		{"content type parameter", `{"results":[]}`, "application/json; version=1", http.StatusOK, 1 << 20, ErrInvalidResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := staticQMDServer(t, test.body, test.contentType, test.status)
			client := newTestClient(t, server, root, &authorizerStub{}, &authorityStub{}, test.max)
			_, err := client.Search(t.Context(), validRequest())
			require.ErrorIs(t, err, test.target)
		})
	}
}

func TestSearchAcceptsExactResponseByteLimitAndRejectsPlusOne(t *testing.T) {
	root, _, _ := exportFixture(t)
	body := `{"results":[]}`
	for name, maximum := range map[string]struct {
		maximum int64
		target  error
	}{
		"exact":    {maximum: int64(len(body))},
		"plus one": {maximum: int64(len(body) - 1), target: ErrResponseBound},
	} {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(t, staticQMDServer(t, body, "application/json", http.StatusOK), root, &authorizerStub{}, &authorityStub{}, maximum.maximum)
			results, err := client.Search(t.Context(), validRequest())
			if maximum.target != nil {
				require.ErrorIs(t, err, maximum.target)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, results)
		})
	}
}

func TestSearchRejectsGenerationChangesAfterResponseAndLiveFence(t *testing.T) {
	for _, duringFence := range []bool{false, true} {
		t.Run(map[bool]string{false: "after response", true: "during live fence"}[duringFence], func(t *testing.T) {
			root, source, receipt := exportFixture(t)
			publishOther := func() {
				other, body := qmdSource(8, "# Different\n")
				_, err := qmdexport.Publish(t.Context(), root, "docbank", []store.QMDExportSource{other}, blobReader{other.BlobSHA256: body}, qmdexport.Options{})
				require.NoError(t, err)
			}
			authority := &authorityStub{live: []store.QMDExportLiveCandidate{{NodeID: source.NodeID, NodeRevision: 4, ContentVersionID: source.ContentVersionID, Path: "/live.pdf"}}}
			if duringFence {
				authority.revalidateHook = func([]store.QMDExportSource) { publishOther() }
			}
			server := qmdServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if !duringFence {
					publishOther()
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(resultBody(receipt.Manifest.Entries[0].URI)))
			})
			client := newTestClient(t, server, root, &authorizerStub{}, authority, 1<<20)
			_, err := client.Search(t.Context(), validRequest())
			require.ErrorIs(t, err, ErrStaleGeneration)
		})
	}
}

func TestSearchExplicitEmptyStillRunsCurrentLiveFence(t *testing.T) {
	root, _, _ := exportFixture(t)
	authority := &authorityStub{}
	client := newTestClient(t, staticQMDServer(t, `{"results":[]}`, "application/json", http.StatusOK), root, &authorizerStub{}, authority, 1<<20)
	results, err := client.Search(t.Context(), validRequest())
	require.NoError(t, err)
	assert.NotNil(t, results)
	assert.Empty(t, results)
	assert.Equal(t, 1, authority.revalidateCalls())
}

func validRequest() Request {
	return Request{Searches: []Search{{Type: SearchVector, Query: "synthetic query"}}, Limit: 5}
}

func resultBody(uri string) string {
	return `{"results":[{"docid":"#abc","file":"` + uri + `","title":"x","score":0.5,"context":null,"snippet":"x"}]}`
}

func newTestClient(t *testing.T, server *httptest.Server, root string, authorizer Authorizer, authority Authority, responseBytes int64) *Client {
	t.Helper()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	p := validProfile()
	p.EgressPolicy.Port = uint16(port)
	p.RequestTimeout = time.Second
	p.MaxResponseBytes = responseBytes
	client, err := New(p, secretStub{}, authorizer, authority, root, resolverStub{}, &http.Client{})
	require.NoError(t, err)
	return client
}

func qmdServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func staticQMDServer(t *testing.T, body, contentType string, status int) *httptest.Server {
	t.Helper()
	return qmdServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

type secretStub struct {
	hook  func()
	value string
	err   error
}

func (s secretStub) ResolveSecret(context.Context, string) (string, error) {
	if s.hook != nil {
		s.hook()
	}
	if s.value == "" {
		s.value = "synthetic-secret"
	}
	return s.value, s.err
}

type authorizerStub struct {
	mu        sync.Mutex
	operation Operation
	hook      func()
	err       error
}

func (s *authorizerStub) AuthorizeQMDQuery(_ context.Context, op Operation) error {
	if s.hook != nil {
		s.hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.operation = op
	return s.err
}
func (s *authorizerStub) gotOperation() Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.operation
}

type authorityStub struct {
	mu             sync.Mutex
	normalized     store.SearchOptions
	normalizeHook  func()
	normalizeErr   error
	live           []store.QMDExportLiveCandidate
	revalidateHook func([]store.QMDExportSource)
	err            error
	got            []store.QMDExportSource
	calls          int
}

func (s *authorityStub) NormalizeQMDSearchScope(_ context.Context, scope store.SearchOptions) (store.SearchOptions, error) {
	if s.normalizeHook != nil {
		s.normalizeHook()
	}
	if !reflect.DeepEqual(s.normalized, store.SearchOptions{}) {
		return s.normalized, s.normalizeErr
	}
	return scope, s.normalizeErr
}
func (s *authorityStub) RevalidateQMDExportCandidates(_ context.Context, values []store.QMDExportSource, _ store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
	if s.revalidateHook != nil {
		s.revalidateHook(values)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.got = append([]store.QMDExportSource(nil), values...)
	return append([]store.QMDExportLiveCandidate(nil), s.live...), s.err
}
func (s *authorityStub) gotCandidates() store.QMDExportSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[0]
}
func (s *authorityStub) revalidateCalls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

type resolverStub struct{}

func (resolverStub) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

type blobReader map[string][]byte

func (r blobReader) OpenStreamContext(_ context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	content, ok := r[hash]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return &verifiedReader{Reader: bytes.NewReader(content)}, int64(len(content)), nil
}

type verifiedReader struct{ *bytes.Reader }

func (*verifiedReader) Close() error   { return nil }
func (*verifiedReader) Verify() error  { return nil }
func (*verifiedReader) Verified() bool { return true }

func exportFixture(t *testing.T) (string, store.QMDExportSource, qmdexport.Receipt) {
	t.Helper()
	root := canonicalTempRoot(t)
	source, body := qmdSource(7, "# Searchable\n")
	receipt, err := qmdexport.Publish(t.Context(), root, "docbank", []store.QMDExportSource{source}, blobReader{source.BlobSHA256: body}, qmdexport.Options{})
	require.NoError(t, err)
	return root, source, receipt
}
func qmdSource(nodeID int64, markdown string) (store.QMDExportSource, []byte) {
	digest := sha256.Sum256([]byte(markdown))
	checksum := hex.EncodeToString(digest[:])
	version := "00000000-0000-4000-8000-" + fmtNode(nodeID)
	return store.QMDExportSource{VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: nodeID, ContentVersionID: version,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), AttachmentID: "attachment-" + version, BuildID: strings.Repeat("b", 64), ArtifactID: "markdown-" + strconv.FormatInt(nodeID, 10), BlobSHA256: checksum, BlobSize: int64(len(markdown)), ArtifactChecksum: checksum, MarkdownChecksum: checksum}, []byte(markdown)
}
func fmtNode(id int64) string {
	value := strconv.FormatInt(id, 10)
	return strings.Repeat("0", 12-len(value)) + value
}

var _ providerhttp.Resolver = resolverStub{}
var _ = json.Deterministic
