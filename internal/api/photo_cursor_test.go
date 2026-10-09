package api

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	ranked := position
	ranked.Key = ""
	ranked.Score = new(0.123)
	rankedCursor, err := service.encodePhotoCursor(ranked)
	require.NoError(t, err)
	decoded, err = service.decodePhotoCursor(rankedCursor)
	require.NoError(t, err)
	require.Equal(t, ranked, decoded)
	now = now.Add(documentCursorTTL)
	_, err = service.decodePhotoCursor(cursor)
	require.ErrorIs(t, err, store.ErrDocumentCursorExpired)
}
