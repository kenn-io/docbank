package api_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestDocumentCatalogRouteReturnsNormalizedBoundedPages(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	key := []byte("0123456789abcdef0123456789abcdef")
	ts, s := newTestServer(t, func(deps *api.Deps) {
		deps.DocumentCursorKey = key
		deps.DocumentCursorNow = func() time.Time { return now }
	})
	docs, err := s.Mkdir(t.Context(), s.RootID(), "docs")
	require.NoError(t, err)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		_, err = s.CreateFile(t.Context(), docs.ID, name, testHash(name), int64(len(name)), "text/plain")
		require.NoError(t, err)
	}

	resp, body := get(t, ts, "/api/v1/documents?path_prefix=%2F%2Fdocs%2F%2F&page_size=2", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var first api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	assert.Equal(t, "/docs", first.PathPrefix)
	assert.Equal(t, "path", first.Sort)
	assert.Equal(t, "asc", first.Direction)
	assert.Equal(t, 2, first.PageSize)
	require.Len(t, first.Items, 2)
	assert.Equal(t, "/docs/a.txt", first.Items[0].Path)
	assert.NotEmpty(t, first.NextCursor)
	assert.Empty(t, first.PreviousCursor)

	resp, body = get(t, ts, "/api/v1/documents?path_prefix=%2Fdocs&page_size=2&cursor="+
		url.QueryEscape(first.NextCursor), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var second api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Len(t, second.Items, 1)
	assert.Equal(t, "/docs/c.txt", second.Items[0].Path)
	assert.Empty(t, second.NextCursor)
	assert.NotEmpty(t, second.PreviousCursor)

	resp, body = get(t, ts, "/api/v1/documents?path_prefix=%2Fdocs&page_size=2&cursor="+
		url.QueryEscape(second.PreviousCursor), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var back api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &back))
	assert.Equal(t, []api.DocumentSummary{first.Items[0], first.Items[1]}, back.Items)
}

func TestDocumentCatalogRoutePaginatesMaximumLegalPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	ts, s := newTestServer(t, func(deps *api.Deps) {
		deps.DocumentCursorKey = []byte("0123456789abcdef0123456789abcdef")
		deps.DocumentCursorNow = func() time.Time { return now }
	})
	_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", testHash("max-path-a"), 1, "text/plain")
	require.NoError(t, err)
	parentID := s.RootID()
	segments := make([]string, 0, 256)
	for depth := range 255 {
		name := strings.Repeat("x", 63)
		if depth == 0 {
			name = "m" + name[1:]
		}
		dir, mkdirErr := s.Mkdir(t.Context(), parentID, name)
		require.NoError(t, mkdirErr)
		parentID = dir.ID
		segments = append(segments, name)
	}
	fileName := strings.Repeat("y", 63)
	_, err = s.CreateFile(t.Context(), parentID, fileName, testHash("max-path"), 1, "text/plain")
	require.NoError(t, err)
	segments = append(segments, fileName)
	maxPath := "/" + strings.Join(segments, "/")
	require.Len(t, maxPath, 16<<10)
	_, err = s.CreateFile(t.Context(), s.RootID(), "z.txt", testHash("max-path-z"), 1, "text/plain")
	require.NoError(t, err)

	_, body := get(t, ts, "/api/v1/documents?page_size=1", nil)
	var first api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Equal(t, "/a.txt", first.Items[0].Path)
	require.LessOrEqual(t, len(first.NextCursor), api.MaxDocumentCursorBytes)

	resp, body := get(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(first.NextCursor), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var middle api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &middle))
	require.Equal(t, maxPath, middle.Items[0].Path)
	require.NotEmpty(t, middle.NextCursor)
	require.NotEmpty(t, middle.PreviousCursor)
	require.LessOrEqual(t, len(middle.NextCursor), api.MaxDocumentCursorBytes)

	resp, body = get(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(middle.NextCursor), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var last api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &last))
	require.Equal(t, "/z.txt", last.Items[0].Path)

	resp, body = get(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(middle.PreviousCursor), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var back api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &back))
	require.Equal(t, "/a.txt", back.Items[0].Path)
}

func TestDocumentCatalogRouteRejectsInvalidAndExpiredCursors(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	key := []byte("0123456789abcdef0123456789abcdef")
	ts, s := newTestServer(t, func(deps *api.Deps) {
		deps.DocumentCursorKey = key
		deps.DocumentCursorNow = func() time.Time { return now }
	})
	for _, name := range []string{"a.txt", "b.txt"} {
		_, err := s.CreateFile(t.Context(), s.RootID(), name, testHash(name), 1, "text/plain")
		require.NoError(t, err)
	}
	_, body := get(t, ts, "/api/v1/documents?page_size=1", nil)
	var page api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.NotEmpty(t, page.NextCursor)
	assert.LessOrEqual(t, len(page.NextCursor), api.MaxDocumentCursorBytes)

	tamperedSuffix := byte('A')
	if page.NextCursor[len(page.NextCursor)-1] == tamperedSuffix {
		tamperedSuffix = 'B'
	}
	tampered := page.NextCursor[:len(page.NextCursor)-1] + string(tamperedSuffix)
	assertDocumentCursorProblem(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(tampered), "invalid_document_cursor")
	for _, changedQuery := range []string{
		"path_prefix=%2Fother&page_size=1", "sort=name&page_size=1",
		"direction=desc&page_size=1", "page_size=2",
	} {
		assertDocumentCursorProblem(t, ts, "/api/v1/documents?"+changedQuery+"&cursor="+
			url.QueryEscape(page.NextCursor), "invalid_document_cursor")
	}
	for _, malformed := range []string{"not-a-cursor", "!.!", ".", "a."} {
		assertDocumentCursorProblem(t, ts, "/api/v1/documents?page_size=1&cursor="+
			url.QueryEscape(malformed), "invalid_document_cursor")
	}
	nonCanonical := nonCanonicalDocumentCursor(t, page.NextCursor)
	assertDocumentCursorProblem(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(nonCanonical), "invalid_document_cursor")
	assertDocumentCursorProblem(t, ts, "/api/v1/documents?cursor="+
		strings.Repeat("a", api.MaxDocumentCursorBytes+1), "invalid_document_cursor")

	unknownVersion := rewriteDocumentCursorVersion(t, page.NextCursor, key, 99)
	assertDocumentCursorProblem(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(unknownVersion), "invalid_document_cursor")

	wrongKey, _ := newTestServer(t, func(deps *api.Deps) {
		deps.DocumentCursorKey = []byte("abcdef0123456789abcdef0123456789")
		deps.DocumentCursorNow = func() time.Time { return now }
	})
	assertDocumentCursorProblem(t, wrongKey, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(page.NextCursor), "invalid_document_cursor")

	now = now.Add(15 * time.Minute)
	assertDocumentCursorProblem(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(page.NextCursor), "cursor_expired")
}

func TestDocumentCatalogRouteAppliesDefaultAndMaximumPageSizes(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/documents", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 50, page.PageSize)

	resp, body = get(t, ts, "/api/v1/documents?page_size=251", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
}

func TestDocumentSummaryResolveReportsStaleIdentity(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	node, err := s.CreateFile(t.Context(), s.RootID(), "current.txt", testHash("summary"), 1, "text/plain")
	require.NoError(t, err)
	body, err := json.Marshal(api.DocumentSummaryResolveRequest{Identities: []api.DocumentIdentity{{
		NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Path: "/old.txt",
	}}})
	require.NoError(t, err)
	resp, result := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/documents/resolve",
		map[string]string{"X-Api-Key": testAPIKey}, string(body))
	require.Equal(t, http.StatusConflict, resp.StatusCode, result)
	assert.Equal(t, "stale_version", decodeProblem(t, result).Code)
}

func assertDocumentCursorProblem(t *testing.T, ts *httptest.Server, path, code string) {
	t.Helper()
	resp, body := get(t, ts, path, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	assert.Equal(t, code, problem.Code)
}

func rewriteDocumentCursorVersion(t *testing.T, cursor string, key []byte, version int) string {
	t.Helper()
	parts := strings.Split(cursor, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(payload, &fields))
	fields["v"] = version
	payload, err = json.Marshal(fields)
	require.NoError(t, err)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func nonCanonicalDocumentCursor(t *testing.T, cursor string) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	parts := strings.Split(cursor, ".")
	require.Len(t, parts, 2)
	last := strings.IndexByte(alphabet, parts[1][len(parts[1])-1])
	require.NotEqual(t, -1, last)
	alias := (last &^ 3) | ((last + 1) & 3)
	require.NotEqual(t, last, alias)
	parts[1] = parts[1][:len(parts[1])-1] + string(alphabet[alias])
	return strings.Join(parts, ".")
}

func TestDocumentSummaryResolveRejectsDuplicateIdentity(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	node, err := s.CreateFile(t.Context(), s.RootID(), "current.txt", testHash("duplicate-summary"), 1, "text/plain")
	require.NoError(t, err)
	identity := api.DocumentIdentity{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Path: "/current.txt"}
	body, err := json.Marshal(api.DocumentSummaryResolveRequest{Identities: []api.DocumentIdentity{identity, identity}})
	require.NoError(t, err)
	resp, result := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/documents/resolve",
		map[string]string{"X-Api-Key": testAPIKey}, string(body))
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, result)
	assert.Equal(t, "invalid_document_query", decodeProblem(t, result).Code)
}
