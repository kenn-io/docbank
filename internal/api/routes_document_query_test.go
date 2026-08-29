package api_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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
	require.LessOrEqual(t, len(first.NextCursor), 2048)

	resp, body := get(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(first.NextCursor), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var middle api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &middle))
	require.Equal(t, maxPath, middle.Items[0].Path)
	require.NotEmpty(t, middle.NextCursor)
	require.NotEmpty(t, middle.PreviousCursor)
	require.Len(t, middle.PreviousCursor, len(middle.NextCursor))
	require.LessOrEqual(t, len(middle.NextCursor), 2048)

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
	assert.Len(t, page.NextCursor, 78)

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
		strings.Repeat("a", 2049), "invalid_document_cursor")

	unknownVersion := rewriteDocumentCursorVersion(t, page.NextCursor, key, 99)
	assertDocumentCursorProblem(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(unknownVersion), "invalid_document_cursor")
	unknownHandle := rewriteDocumentCursor(t, page.NextCursor, key, func(payload []byte) {
		payload[len(payload)-1] ^= 0xff
	})
	assertDocumentCursorProblem(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(unknownHandle), "invalid_document_cursor")
	mismatchedExpiry := rewriteDocumentCursor(t, page.NextCursor, key, func(payload []byte) {
		expiresAt := binary.BigEndian.Uint64(payload[1:9])
		binary.BigEndian.PutUint64(payload[1:9], expiresAt+60)
	})
	assertDocumentCursorProblem(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(mismatchedExpiry), "invalid_document_cursor")

	wrongKey, _ := newTestServer(t, func(deps *api.Deps) {
		deps.DocumentCursorKey = []byte("abcdef0123456789abcdef0123456789")
		deps.DocumentCursorNow = func() time.Time { return now }
	})
	assertDocumentCursorProblem(t, wrongKey, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(page.NextCursor), "invalid_document_cursor")

	now = now.Add(15 * time.Minute)
	assertDocumentCursorProblem(t, ts, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(page.NextCursor), "cursor_expired")

	restarted, _ := newTestServer(t, func(deps *api.Deps) {
		deps.DocumentCursorKey = key
		deps.DocumentCursorNow = func() time.Time { return now.Add(-15 * time.Minute) }
	})
	assertDocumentCursorProblem(t, restarted, "/api/v1/documents?page_size=1&cursor="+
		url.QueryEscape(page.NextCursor), "invalid_document_cursor")
}

func TestDocumentCatalogRouteAppliesDefaultAndMaximumPageSizes(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/documents", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 50, page.PageSize)

	resp, body = get(t, ts, "/api/v1/documents?page_size=251", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
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
	return rewriteDocumentCursor(t, cursor, key, func(payload []byte) {
		payload[0] = byte(version)
	})
}

func rewriteDocumentCursor(
	t *testing.T, cursor string, key []byte, mutate func([]byte),
) string {
	t.Helper()
	parts := strings.Split(cursor, ".")
	require.Len(t, parts, 2)
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	mutate(payload)
	mac := hmac.New(sha256.New, key)
	_, err = mac.Write(payload)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
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
