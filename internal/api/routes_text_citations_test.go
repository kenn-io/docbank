package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

const citationPath = "/api/v1/text-citations/resolve"

type citationTestGate interface {
	MutateContext(context.Context, func() error) error
	MaintainContext(context.Context, func() error) error
	CaptureContext(context.Context, func() error) error
}

type citationHTTPFixture struct {
	server   *httptest.Server
	store    *testStore
	service  *processing.Service
	citation document.TextCitation
}

func newCitationHTTPFixture(t *testing.T, gate citationTestGate) citationHTTPFixture {
	t.Helper()
	var cfg config.Config
	var service *processing.Service
	ts, s := newTestServer(t, func(d *api.Deps) {
		renditionTextConfig(d)
		cfg = d.Cfg
		var err error
		service, err = processing.NewService(processing.ServiceConfig{
			Catalog: d.Store, Blobs: d.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(d.VaultRoot, "blobs", "tmp"),
		})
		require.NoError(t, err)
		d.Processing = service
	})
	node := createFileWithContent(t, ts, s, "/citation.pdf", "%PDF-1.4 synthetic")
	f := publishRenditionTextFixture(t, s, cfg, node)
	prefix, _, found := strings.Cut(string(f.markdown), "Verified alpha")
	require.True(t, found)
	start := utf8.RuneCountInString(prefix)
	vault, err := uuid.Parse(s.VaultID())
	require.NoError(t, err)
	version, err := uuid.Parse(node.CurrentVersionID)
	require.NoError(t, err)
	return citationHTTPFixture{ts, s, service, document.TextCitation{
		Version: 1, VaultUID: vault, NodeID: node.ID, ContentVersionID: version,
		ContentSHA256: node.BlobHash, RenditionAttachmentID: f.attachment, BuildID: f.build,
		RenditionSHA256: testHash(string(f.markdown)), Start: start, End: start + 14,
	}}
}

func TestTextCitationHTTP(t *testing.T) {
	t.Parallel()
	f := newCitationHTTPFixture(t, api.NewOperationGate())
	resp, body := do(t, f.server, http.MethodPost, citationPath, nil, f.citation)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var got document.ResolvedTextCitation
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Equal(t, f.citation, got.Citation)
	require.Equal(t, "Verified alpha", got.Text)
	require.Equal(t, 14, got.TextBytes)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("Verified alpha"))), got.TextSHA256)

	for _, test := range []struct {
		headers map[string]string
		status  int
		code    string
	}{
		{map[string]string{"X-Api-Key": ""}, 401, "unauthorized"},
		{map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, f.server)},
			403, "web_session_read_only"},
	} {
		resp, body = do(t, f.server, http.MethodPost, citationPath, test.headers, f.citation)
		require.Equal(t, test.status, resp.StatusCode, body)
		require.Contains(t, body, `"code":"`+test.code+`"`)
		require.NotContains(t, body, `"text":`)
	}
	encoded, err := json.Marshal(f.citation)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	for field := range fields {
		t.Run(field+" required", func(t *testing.T) {
			original := fields[field]
			defer func() { fields[field] = original }()
			delete(fields, field)
			resp, body := do(t, f.server, http.MethodPost, citationPath, nil, fields)
			require.Equal(t, 422, resp.StatusCode, body)
			fields[field] = nil
			resp, body = do(t, f.server, http.MethodPost, citationPath, nil, fields)
			require.Equal(t, 422, resp.StatusCode, body)
		})
	}
	fields["unknown"] = true
	resp, body = do(t, f.server, http.MethodPost, citationPath, nil, fields)
	require.Contains(t, []int{400, 422}, resp.StatusCode, body)
	bad := f.citation
	bad.Start, bad.End = 1, 1
	resp, body = do(t, f.server, http.MethodPost, citationPath, nil, bad)
	require.Equal(t, 422, resp.StatusCode, body)
	require.Contains(t, body, `"code":"invalid_text_citation"`)
	bad = f.citation
	bad.End = 20_000
	resp, body = do(t, f.server, http.MethodPost, citationPath, nil, bad)
	require.Equal(t, 422, resp.StatusCode, body)
	bad = f.citation
	bad.Start, bad.End = 1000, 1001
	resp, body = do(t, f.server, http.MethodPost, citationPath, nil, bad)
	require.Equal(t, 416, resp.StatusCode, body)
	bad = f.citation
	bad.BuildID = strings.Repeat("a", 64)
	resp, body = do(t, f.server, http.MethodPost, citationPath, nil, bad)
	require.Equal(t, 404, resp.StatusCode, body)
}

func TestTextCitationHTTPUUIDSyntax(t *testing.T) {
	t.Parallel()
	f := newCitationHTTPFixture(t, api.NewOperationGate())
	encoded, err := json.Marshal(f.citation)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	for _, field := range []string{"vault_uid", "content_version_id"} {
		original := fields[field].(string)
		for _, invalid := range []string{
			strings.ToUpper(original), "urn:uuid:" + original, "{" + original + "}",
			strings.ReplaceAll(original, "-", ""), "11111111-1111-1111-8111-111111111111",
			"11111111-1111-4111-1111-111111111111",
		} {
			fields[field] = invalid
			resp, body := do(t, f.server, http.MethodPost, citationPath, nil, fields)
			require.Equal(t, 422, resp.StatusCode, body)
			require.Contains(t, body, `"code":"validation"`)
			require.NotContains(t, body, `"text":`)
		}
		fields[field] = original
	}
}

func TestTextCitationClientHTTP(t *testing.T) {
	t.Parallel()
	f := newCitationHTTPFixture(t, api.NewOperationGate())
	connection := daemonconn.New(f.server.URL, testAPIKey)
	defer connection.Close()
	got, err := connection.ResolveTextCitation(t.Context(), f.citation)
	require.NoError(t, err)
	require.Equal(t, f.citation, got.Citation)
	require.Equal(t, "Verified alpha", got.Text)
	bad := f.citation
	bad.Start, bad.End = 1000, 1001
	_, err = connection.ResolveTextCitation(t.Context(), bad)
	require.Error(t, err)
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "invalid_citation_range", code)
}

func TestTextCitationHTTPBodyLimit(t *testing.T) {
	t.Parallel()
	f := newCitationHTTPFixture(t, api.NewOperationGate())
	encoded, err := json.Marshal(f.citation)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+citationPath,
		strings.NewReader(string(encoded)+strings.Repeat(" ", 16<<10)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 413, resp.StatusCode)
}
