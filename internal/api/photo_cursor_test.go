package api

import (
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestPhotoBrowserPermissions(t *testing.T) {
	t.Parallel()
	base := "/api/v1/photos/assets/00000000-0000-4000-8000-000000000001"
	preview := base + "/previews/" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{http.MethodPost, "/api/v1/photos/assets/query", true}, {http.MethodGet, preview, true},
		{http.MethodPost, base + "/trash", true},
		{http.MethodGet, base + "?run=true", false},
		{http.MethodPost, base + "/trash?run=true", false},
		{http.MethodPost, base + "/trash/extra", false},
		{http.MethodPost, base + "/files", false},
		{http.MethodPatch, base, false},
		{http.MethodDelete, base + "/trash", false},
		{http.MethodGet, "/api/v1/photos/assets/query", false}, {http.MethodPost, "/api/v1/photos/assets/query?extra=1", false}, {http.MethodGet, preview + "?extra=1", false}, {http.MethodPost, preview, false}, {http.MethodGet, preview + "/other", false}, {http.MethodGet, base + "/previews/bad", false}, {http.MethodGet, base, false}, {http.MethodPost, base + "/exclude", false}, {http.MethodGet, "/api/v1/content-versions/00000000-0000-4000-8000-000000000001/content", false},
	} {
		require.Equal(t, tc.allowed, webSessionRequestAllowed(httptest.NewRequest(tc.method, tc.path, nil)), tc.path)
	}
}

func TestPhotoRankedCursorOwnershipAndExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		catalog, err := store.Open(filepath.Join(root, "docbank.db"))
		require.NoError(t, err)
		defer func() { require.NoError(t, catalog.Close()) }()
		for _, name := range []string{"Canon.jpg", "Canon second.jpg"} {
			_, err := catalog.CreateFile(t.Context(), catalog.RootID(), name, strings.Repeat("a", 64), 10, "image/jpeg")
			require.NoError(t, err)
		}
		cfg := config.Default()
		cfg.Server.APIKey = "synthetic-key"
		server := NewServer(Deps{Store: catalog, VaultRoot: root, Cfg: cfg, WebURL: "http://docbank-0123456789abcdef0123456789abcdef.localhost:43210/"})
		defer server.Close()
		token, _, err := server.webSessions.issue()
		require.NoError(t, err)
		read := func(cursor, session string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/photos/assets/query", strings.NewReader(fmt.Sprintf(`{"query":{"text":"Canon","sort":{"field":"relevance","direction":"desc"}},"page_size":1,"cursor":%q}`, cursor)))
			request.Header.Set("Content-Type", "application/json")
			if session == "" {
				request.Header.Set("X-Api-Key", cfg.Server.APIKey)
			} else {
				request.Header.Set(WebSessionHeader, session)
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			return response
		}
		response := read("", "")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var first PhotoBrowsePage
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &first))
		require.NotEmpty(t, first.NextCursor)
		response = read(first.NextCursor, token)
		require.Equal(t, http.StatusUnprocessableEntity, response.Code, response.Body.String())
		response = read(first.NextCursor, "")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		time.Sleep(16 * time.Minute)
		response = read(first.NextCursor, "")
		require.Equal(t, http.StatusUnprocessableEntity, response.Code, response.Body.String())
		var problem Error
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
		require.Equal(t, "cursor_expired", problem.Code)
	})
}

func TestPhotoCursor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	service := newDocumentCursorTestService(&now)
	position := store.PhotoBrowsePosition{Total: 7, Key: "capture.jpg", AssetID: "00000000-0000-4000-8000-000000000001", QueryIdentity: strings.Repeat("a", 64)}
	cursor, err := service.encodePhotoCursor(position)
	require.NoError(t, err)
	decoded, err := service.decodePhotoCursor(cursor)
	require.NoError(t, err)
	require.Equal(t, position, decoded)
	for _, invalid := range []string{cursor + "a", strings.Repeat("a", MaxDocumentCursorBytes+1), "bad", cursor + "."} {
		_, err := service.decodePhotoCursor(invalid)
		require.ErrorIs(t, err, store.ErrInvalidPhotoCursor)
	}
	payload, err := json.Marshal(photoCursorPayload{Type: "photo-v2", IssuedAt: now.Unix(), Position: position})
	require.NoError(t, err)
	_, err = service.decodePhotoCursor(service.signCursorEnvelope(payload))
	require.ErrorIs(t, err, store.ErrInvalidPhotoCursor)
	rankedCursor, err := service.encodePhotoRankedCursor("snapshot", "offset")
	require.NoError(t, err)
	ranked, err := service.decodePhotoRankedCursor(rankedCursor)
	require.NoError(t, err)
	require.Equal(t, "snapshot", ranked.SnapshotID)
	require.Equal(t, "offset", ranked.Cursor)
	_, err = service.decodePhotoCursor(rankedCursor)
	require.ErrorIs(t, err, store.ErrInvalidPhotoCursor)
	_, err = service.decodePhotoRankedCursor(cursor)
	require.ErrorIs(t, err, store.ErrInvalidPhotoCursor)
	now = now.Add(documentCursorTTL)
	_, err = service.decodePhotoCursor(cursor)
	require.ErrorIs(t, err, store.ErrDocumentCursorExpired)
}
