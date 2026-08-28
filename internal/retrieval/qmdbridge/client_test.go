package qmdbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/qmdexport"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestSearchMapsOnlyCurrentManifestResultsThroughLiveAuthority(t *testing.T) {
	root, source, receipt := exportFixture(t)
	normalizedScope := store.SearchOptions{MIMEType: "application/pdf"}
	authority := &authorityStub{normalized: &normalizedScope, live: []store.QMDExportLiveCandidate{{NodeID: 7, NodeRevision: 4,
		ContentVersionID: source.ContentVersionID, Path: "/live/document.pdf"}}}
	authorizer := &authorizerStub{}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization"))
		assert.Equal(t, "/query", request.URL.Path)
		var payload wireRequest
		if !assert.NoError(t, json.UnmarshalRead(request.Body, &payload, json.RejectUnknownMembers(true))) {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, []string{"docbank"}, payload.Collections)
		assert.NotContains(t, string(mustJSON(t, payload)), "Searchable")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"results":[{"docid":"#abc123","file":"` + receipt.Manifest.Entries[0].URI + `","title":"Result","score":0.75,"context":null,"snippet":"1: bounded match"}]}`))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server, root, authorizer, authority, 1<<20)

	results, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchLexical, Query: "private terms"}},
		Intent: "find the relevant document", Limit: 5})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, 1, requests)
	assert.Equal(t, int64(7), results[0].Document.NodeID)
	assert.Equal(t, source.ContentVersionID, results[0].Document.ContentVersionID)
	assert.Equal(t, "/live/document.pdf", results[0].Path)
	assert.Equal(t, receipt.Manifest.Entries[0].URI, results[0].QMDURI)
	assert.Equal(t, receipt.GenerationID, results[0].GenerationID)
	assert.Equal(t, receipt.Manifest.Checksum, results[0].ManifestChecksum)
	assert.Equal(t, "1: bounded match", results[0].Excerpt)
	require.Len(t, authority.got, 1)
	assert.Equal(t, source, authority.got[0])
	assert.Equal(t, receipt.GenerationID, authorizer.operation.GenerationID)
	assert.Equal(t, normalizedScope, authorizer.operation.Scope)
	assert.Equal(t, normalizedScope, authority.scope)
	assert.Equal(t, len("private terms")+len("find the relevant document"), authorizer.operation.DisclosedBytes)
}

func TestSearchAuthorizesImmediatelyBeforeEgress(t *testing.T) {
	root, _, _ := exportFixture(t)
	authorizer := &authorizerStub{err: errors.New("denied")}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	client := newTestClient(t, server, root, authorizer, &authorityStub{}, 1<<20)

	_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchLexical, Query: "private"}}, Limit: 5})
	require.ErrorContains(t, err, "authorize")
	assert.Zero(t, requests)
}

func TestSearchRejectsInvalidScopeBeforeEgress(t *testing.T) {
	root, _, _ := exportFixture(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	client := newTestClient(t, server, root, &authorizerStub{}, &authorityStub{normalizeErr: errors.New("invalid scope")}, 1<<20)

	_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchLexical, Query: "private"}},
		Limit: 5, Scope: store.SearchOptions{UnderNodeID: -1}})
	require.ErrorContains(t, err, "scope")
	assert.Zero(t, requests)
}

func TestSearchPreservesResponseBodyDeadline(t *testing.T) {
	root, _, _ := exportFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server, root, &authorizerStub{}, &authorityStub{}, 1<<20)
	client.profile.RequestTimeout = 25 * time.Millisecond

	_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchLexical, Query: "private"}}, Limit: 5})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestSearchRejectsUnknownDuplicateOversizedAndStaleResults(t *testing.T) {
	t.Run("unknown URI", func(t *testing.T) {
		root, _, _ := exportFixture(t)
		server := qmdServer(t, `{"results":[{"docid":"#abc","file":"qmd://docbank/documents/ff/unknown.md","title":"x","score":0.5,"context":null,"snippet":"x"}]}`, nil)
		client := newTestClient(t, server, root, &authorizerStub{}, &authorityStub{}, 1<<20)
		_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchVector, Query: "private"}}, Limit: 5})
		require.ErrorIs(t, err, ErrInvalidResponse)
	})

	t.Run("duplicate URI", func(t *testing.T) {
		root, _, receipt := exportFixture(t)
		item := `{"docid":"#abc","file":"` + receipt.Manifest.Entries[0].URI + `","title":"x","score":0.5,"context":null,"snippet":"x"}`
		server := qmdServer(t, `{"results":[`+item+`,`+item+`]}`, nil)
		client := newTestClient(t, server, root, &authorizerStub{}, &authorityStub{}, 1<<20)
		_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchVector, Query: "private"}}, Limit: 5})
		require.ErrorIs(t, err, ErrInvalidResponse)
	})

	t.Run("bounded response", func(t *testing.T) {
		root, _, _ := exportFixture(t)
		server := qmdServer(t, `{"results":[],"padding":"`+strings.Repeat("x", 2048)+`"}`, nil)
		client := newTestClient(t, server, root, &authorizerStub{}, &authorityStub{}, 512)
		_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchVector, Query: "private"}}, Limit: 5})
		require.ErrorIs(t, err, ErrResponseBound)
	})

	t.Run("generation changes during query", func(t *testing.T) {
		root, _, receipt := exportFixture(t)
		server := qmdServer(t, `{"results":[{"docid":"#abc","file":"`+receipt.Manifest.Entries[0].URI+`","title":"x","score":0.5,"context":null,"snippet":"x"}]}`, func() {
			other, body := qmdSource(8, "# Different\n")
			_, err := qmdexport.Publish(t.Context(), root, "docbank", []store.QMDExportSource{other}, blobReader{other.BlobSHA256: body}, qmdexport.Options{})
			require.NoError(t, err)
		})
		client := newTestClient(t, server, root, &authorizerStub{}, &authorityStub{}, 1<<20)
		_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchVector, Query: "private"}}, Limit: 5})
		require.ErrorIs(t, err, ErrStaleGeneration)
	})

	t.Run("generation changes during live fence", func(t *testing.T) {
		root, source, receipt := exportFixture(t)
		server := qmdServer(t, `{"results":[{"docid":"#abc","file":"`+receipt.Manifest.Entries[0].URI+`","title":"x","score":0.5,"context":null,"snippet":"x"}]}`, nil)
		authority := &authorityStub{live: []store.QMDExportLiveCandidate{{NodeID: 7, NodeRevision: 4,
			ContentVersionID: source.ContentVersionID, Path: "/live/document.pdf"}}, before: func() {
			other, body := qmdSource(8, "# Different\n")
			_, err := qmdexport.Publish(t.Context(), root, "docbank", []store.QMDExportSource{other}, blobReader{other.BlobSHA256: body}, qmdexport.Options{})
			require.NoError(t, err)
		}}
		client := newTestClient(t, server, root, &authorizerStub{}, authority, 1<<20)
		_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchVector, Query: "private"}}, Limit: 5})
		require.ErrorIs(t, err, ErrStaleGeneration)
	})
}

func newTestClient(t *testing.T, server *httptest.Server, root string, authorizer Authorizer, authority Authority, responseBytes int64) *Client {
	t.Helper()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client, err := New(Profile{ID: "qmd-test", CompatibilityEpoch: "qmd-current", SecretBinding: "secret:qmd",
		EndpointPath: "/query", RequestTimeout: time.Second, MaxResponseBytes: responseBytes,
		EgressPolicy: providerhttp.EgressPolicy{Scheme: "http", Host: "qmd.test", Port: uint16(port),
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, ProxyMode: providerhttp.ProxyDisabled}},
		secretStub{}, authorizer, authority, root, resolverStub{}, &http.Client{})
	require.NoError(t, err)
	return client
}

func qmdServer(t *testing.T, response string, beforeResponse func()) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if beforeResponse != nil {
			beforeResponse()
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server
}

type secretStub struct{}

func (secretStub) ResolveSecret(context.Context, string) (string, error) {
	return "synthetic-secret", nil
}

type authorizerStub struct {
	operation Operation
	err       error
}

func (stub *authorizerStub) AuthorizeQMDQuery(_ context.Context, operation Operation) error {
	stub.operation = operation
	return stub.err
}

type authorityStub struct {
	got          []store.QMDExportSource
	live         []store.QMDExportLiveCandidate
	err          error
	before       func()
	normalized   *store.SearchOptions
	normalizeErr error
	scope        store.SearchOptions
}

func (stub *authorityStub) NormalizeQMDSearchScope(_ context.Context, scope store.SearchOptions) (store.SearchOptions, error) {
	if stub.normalized != nil {
		return *stub.normalized, stub.normalizeErr
	}
	return scope, stub.normalizeErr
}

func (stub *authorityStub) RevalidateQMDExportCandidates(_ context.Context, candidates []store.QMDExportSource, scope store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
	stub.got = append([]store.QMDExportSource(nil), candidates...)
	stub.scope = scope
	if stub.before != nil {
		stub.before()
	}
	return append([]store.QMDExportLiveCandidate(nil), stub.live...), stub.err
}

type resolverStub struct{}

func (resolverStub) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

type blobReader map[string][]byte

func (reader blobReader) OpenStreamContext(_ context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	content, ok := reader[hash]
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
	root := filepath.Join(t.TempDir(), "qmd")
	source, body := qmdSource(7, "# Searchable\n")
	receipt, err := qmdexport.Publish(t.Context(), root, "docbank", []store.QMDExportSource{source}, blobReader{source.BlobSHA256: body}, qmdexport.Options{})
	require.NoError(t, err)
	return root, source, receipt
}

func qmdSource(nodeID int64, markdown string) (store.QMDExportSource, []byte) {
	digest := sha256.Sum256([]byte(markdown))
	checksum := hex.EncodeToString(digest[:])
	version := "00000000-0000-4000-8000-" + fmtNode(nodeID)
	return store.QMDExportSource{VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: nodeID,
		ContentVersionID: version, ProcessingProfileFingerprint: strings.Repeat("a", 64),
		AttachmentID: "attachment-" + version, BuildID: strings.Repeat("b", 64), ArtifactID: "markdown",
		BlobSHA256: checksum, BlobSize: int64(len(markdown)), ArtifactChecksum: checksum, MarkdownChecksum: checksum}, []byte(markdown)
}

func fmtNode(nodeID int64) string {
	return strings.Repeat("0", 12-len(strconv.FormatInt(nodeID, 10))) + strconv.FormatInt(nodeID, 10)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value, json.Deterministic(true))
	require.NoError(t, err)
	return encoded
}

var _ providerhttp.Resolver = resolverStub{}

func TestFilesystemRootIsVolumeAware(t *testing.T) {
	volumeRoot := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	assert.True(t, filesystemRoot(volumeRoot))
}
