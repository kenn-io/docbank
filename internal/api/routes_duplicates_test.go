package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestDuplicatesHTTPCurrentIdentityAndHistoricalBoundary(t *testing.T) {
	ts, s := newTestServer(t, nil)
	first := createFileWithContent(t, ts, s, "/first.txt", "same synthetic content")
	second := createFileWithContent(t, ts, s, "/second.txt", "same synthetic content")
	history := createFileWithContent(t, ts, s, "/history.txt", "same synthetic content")
	replacement := createFileWithContent(t, ts, s, "/replacement.txt", "different")
	_, _, err := s.ReplaceContent(t.Context(), history.ID, history.Revision,
		replacement.BlobHash, replacement.Size, replacement.MimeType)
	require.NoError(t, err)
	trash := createFileWithContent(t, ts, s, "/trash.txt", "same synthetic content")
	_, _, err = s.Trash(t.Context(), trash.ID, trash.Revision)
	require.NoError(t, err)

	resp, body := get(t, ts, "/api/v1/duplicates?limit=1&offset=0", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page struct {
		Items []struct {
			SHA256               string `json:"sha256"`
			ReferenceCount       int    `json:"reference_count"`
			RepresentativeNodeID int64  `json:"representative_node_id"`
			References           []struct {
				Reference api.ContentReference `json:"reference"`
			} `json:"references"`
			ReferencesTruncated bool `json:"references_truncated"`
		} `json:"items"`
		Total           int `json:"total"`
		TotalReferences int `json:"total_references"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, 2, page.Total)
	require.Equal(t, 4, page.TotalReferences)
	require.Len(t, page.Items, 1)
	// Stable hash ordering, not creation time, determines the selected group.
	if page.Items[0].SHA256 != first.BlobHash {
		resp, body = get(t, ts, "/api/v1/duplicates?limit=1&offset=1", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		require.NoError(t, json.Unmarshal([]byte(body), &page))
	}
	require.Len(t, page.Items, 1)
	group := page.Items[0]
	require.Equal(t, first.BlobHash, group.SHA256)
	require.Equal(t, 2, group.ReferenceCount)
	require.Equal(t, first.ID, group.RepresentativeNodeID)
	require.False(t, group.ReferencesTruncated)
	require.Len(t, group.References, 2)
	for i, node := range []struct {
		id            int64
		version, path string
	}{
		{first.ID, first.CurrentVersionID, "/first.txt"}, {second.ID, second.CurrentVersionID, "/second.txt"},
	} {
		ref := group.References[i].Reference
		require.Equal(t, node.id, ref.Node.ID)
		require.Equal(t, node.version, ref.Version.ID)
		require.Equal(t, first.BlobHash, ref.Version.BlobHash)
		require.Equal(t, node.path, ref.Path)
		require.True(t, ref.IsCurrent)
	}
	resp, body = get(t, ts, "/api/v1/content-references?sha256="+first.BlobHash, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var retained api.ContentReferencePage
	require.NoError(t, json.Unmarshal([]byte(body), &retained))
	require.Equal(t, 4, retained.Total, "historical and trash lookup remains separate and complete")
}

func TestDuplicateByHashHTTPReturnsOnlyTheExactCurrentGroup(t *testing.T) {
	ts, s := newTestServer(t, nil)
	first := createFileWithContent(t, ts, s, "/exact-a.txt", "same exact bytes")
	second := createFileWithContent(t, ts, s, "/exact-b.txt", "same exact bytes")
	createFileWithContent(t, ts, s, "/other-a.txt", "other duplicate")
	createFileWithContent(t, ts, s, "/other-b.txt", "other duplicate")

	resp, body := get(t, ts, fmt.Sprintf("/api/v1/duplicates/by-hash?sha256=%s&size=%d", first.BlobHash, first.Size), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var group api.DuplicateContextGroup
	require.NoError(t, json.Unmarshal([]byte(body), &group))
	require.Equal(t, first.BlobHash, group.SHA256)
	require.Equal(t, 2, group.ReferenceCount)
	require.Equal(t, []int64{first.ID, second.ID}, []int64{
		group.References[0].NodeID, group.References[1].NodeID,
	})

	resp, body = get(t, ts, "/api/v1/duplicates/by-hash?sha256="+testHash("missing")+"&size=1", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
}

func TestDuplicatesHTTPPreviewBoundsAndExhaustion(t *testing.T) {
	ts, s := newTestServer(t, nil)
	for i := range 17 {
		createFileWithContent(t, ts, s, fmt.Sprintf("/copy-%02d.txt", i), "shared bytes")
	}
	resp, body := get(t, ts, "/api/v1/duplicates", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page struct {
		Items []struct {
			ReferenceCount      int                    `json:"reference_count"`
			References          []api.ContentReference `json:"references"`
			ReferencesTruncated bool                   `json:"references_truncated"`
		} `json:"items"`
		Total           int `json:"total"`
		TotalReferences int `json:"total_references"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, 1, page.Total)
	require.Equal(t, 17, page.TotalReferences)
	require.Len(t, page.Items, 1)
	require.Equal(t, 17, page.Items[0].ReferenceCount)
	require.Len(t, page.Items[0].References, 16)
	require.True(t, page.Items[0].ReferencesTruncated)
	resp, body = get(t, ts, "/api/v1/duplicates?offset=10", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.NotNil(t, page.Items)
	require.Empty(t, page.Items)
	require.Equal(t, 1, page.Total)
	require.Equal(t, 17, page.TotalReferences)
}

func TestDuplicatesHTTPAuthenticationAndBounds(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/duplicates", map[string]string{"X-Api-Key": ""})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	for _, query := range []string{"limit=0", "limit=101", "offset=-1", "limit=invalid"} {
		resp, body = get(t, ts, "/api/v1/duplicates?"+query, nil)
		require.GreaterOrEqual(t, resp.StatusCode, 400, body)
		require.Less(t, resp.StatusCode, 500, body)
	}
	resp, body = do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	resp, body = get(t, ts, "/api/v1/duplicates?limit=1&offset=0", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	for _, path := range []string{"/api/v1/duplicates/extra", "/api/v1/duplicates?include_trash=true", "/api/v1/duplicates?limit=1&limit=2", "/api/v1/duplicates?limit=%ZZ"} {
		resp, body = get(t, ts, path, headers)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	}
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/duplicates", headers, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
}

func TestDuplicatesHTTPCollectionLabelsKeepImportIdentity(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	label := "Synthetic review"
	first := importCollection(t, ts.URL, ts.Client(), "alpha.txt", "shared import content", &label)
	second := importCollection(t, ts.URL, ts.Client(), "beta.txt", "shared import content", nil)
	resp, body := get(t, ts, "/api/v1/duplicates", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.DuplicatePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	require.Len(t, page.Items[0].References, 2)
	for i, id := range []string{first.IngestID, second.IngestID} {
		ref := page.Items[0].References[i]
		require.Equal(t, 1, ref.CollectionCount)
		require.False(t, ref.CollectionsTruncated)
		require.Len(t, ref.Collections, 1)
		require.Equal(t, id, ref.Collections[0].ID)
	}
	require.NotNil(t, page.Items[0].References[0].Collections[0].Label)
	require.Equal(t, label, *page.Items[0].References[0].Collections[0].Label)
	require.Nil(t, page.Items[0].References[1].Collections[0].Label)
}

func TestDuplicatesHTTPEmptyPopulationAndBackendFailure(t *testing.T) {
	ts, s := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/duplicates", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.DuplicatePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.NotNil(t, page.Items)
	require.Empty(t, page.Items)
	require.Zero(t, page.Total)
	require.Zero(t, page.TotalReferences)
	require.Equal(t, 50, page.Limit)
	require.NoError(t, s.Close())
	resp, body = get(t, ts, "/api/v1/duplicates", nil)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
}
