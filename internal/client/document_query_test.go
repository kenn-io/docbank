package client_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestListDocumentsUsesTypedDaemonCatalogRoute(t *testing.T) {
	requestSeen := make(chan *http.Request, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestSeen <- request.Clone(request.Context())
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, api.DocumentPage{
			PathPrefix: "/docs", Sort: "modified_at", Direction: "desc", PageSize: 2,
			Items: []api.DocumentSummary{{
				NodeID: 7, ContentVersionID: "11111111-1111-4111-8111-111111111111",
				Path: "/docs/report.pdf", Name: "report.pdf", MediaType: "application/pdf",
				Size: 42, ModifiedAt: "2026-08-28T10:00:00.000000000Z",
				LatestProcessingState: "completed",
				ActiveRenditions: []api.DocumentRenditionIdentity{{
					ProfileFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					AttachmentID:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					BuildID:            "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				}},
			}},
			NextCursor: "next-opaque", PreviousCursor: "previous-opaque",
		})
	}))
	t.Cleanup(ts.Close)

	c := client.New(ts.URL, "daemon-key")
	page, err := c.ListDocuments(t.Context(), api.DocumentQuery{
		PathPrefix: "/docs", Sort: "modified_at", Direction: "desc", PageSize: 2, Cursor: "cursor-opaque",
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, int64(7), page.Items[0].NodeID)
	assert.Equal(t, "completed", page.Items[0].LatestProcessingState)
	require.Len(t, page.Items[0].ActiveRenditions, 1)
	assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		page.Items[0].ActiveRenditions[0].AttachmentID)

	request := <-requestSeen
	assert.Equal(t, http.MethodGet, request.Method)
	assert.Equal(t, "/api/v1/documents", request.URL.Path)
	assert.Equal(t, "/docs", request.URL.Query().Get("path_prefix"))
	assert.Equal(t, "modified_at", request.URL.Query().Get("sort"))
	assert.Equal(t, "desc", request.URL.Query().Get("direction"))
	assert.Equal(t, "2", request.URL.Query().Get("page_size"))
	assert.Equal(t, "cursor-opaque", request.URL.Query().Get("cursor"))
	assert.Equal(t, "daemon-key", request.Header.Get("X-Api-Key"))
}

func TestListDocumentsUsesStoreUnicodePathNormalization(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/Cafe\u0301", request.URL.Query().Get("path_prefix"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, api.DocumentPage{
			PathPrefix: "/Café", Sort: "path", Direction: "asc", PageSize: 50,
			Items: []api.DocumentSummary{},
		})
	}))
	t.Cleanup(ts.Close)

	page, err := client.New(ts.URL, "daemon-key").ListDocuments(t.Context(), api.DocumentQuery{
		PathPrefix: "/Cafe\u0301",
	})
	require.NoError(t, err)
	assert.Equal(t, "/Café", page.PathPrefix)
}
