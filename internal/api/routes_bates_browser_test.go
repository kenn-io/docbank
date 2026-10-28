package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestBatesBrowserCandidates(t *testing.T) {
	t.Parallel()
	srv, s := newPackageTestServer(t)
	snapshot, pages := seedBatesSnapshot(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	recipe := batesTestRecipe(api.BatesNamespace{
		NamespaceID: ns.NamespaceID, Prefix: ns.Prefix, Padding: ns.Padding,
	}, 41)
	request, err := json.Marshal(api.BatesReserveRequest{OperationID: uuid.New().String(),
		SnapshotID: snapshot.SnapshotID, Recipe: recipe, Pages: pages})
	require.NoError(t, err)
	reserved := srv.call(t, http.MethodPost, "/api/v1/bates/allocations", string(request), nil)
	require.Equal(t, http.StatusCreated, reserved.Code, reserved.Body.String())
	allocation := decodeBatesAllocation(t, reserved.Body.Bytes())
	request, err = json.Marshal(api.BatesExportRequest{AllocationID: allocation.AllocationID, Recipe: recipe})
	require.NoError(t, err)
	published := srv.call(t, http.MethodPost, "/api/v1/bates/exports", string(request), nil)
	require.Equal(t, http.StatusCreated, published.Code, published.Body.String())
	var artifact api.BatesExport
	require.NoError(t, json.Unmarshal(published.Body.Bytes(), &artifact))
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, srv.ts)}
	const path = "/api/v1/bates/exports/candidates"
	found := srv.call(t, http.MethodGet, path+"?bates_label=OUR000041&limit=50", "", browser)
	require.Equal(t, http.StatusOK, found.Code, found.Body.String())
	var result api.BatesCandidatePage
	require.NoError(t, json.Unmarshal(found.Body.Bytes(), &result))
	require.Len(t, result.Items, 1)
	require.Equal(t, artifact.ArtifactID, result.Items[0].ArtifactID)
	require.Equal(t, allocation.AllocationID, result.Items[0].AllocationID)
	require.Len(t, result.Items[0].Evidence, 1)
	require.Equal(t, "OUR000041", result.Items[0].Evidence[0].Label)
	require.Equal(t, 1, result.Items[0].Evidence[0].OutputPage)
	missing := srv.call(t, http.MethodGet, path+"?bates_label=NONE000001", "", browser)
	require.Equal(t, http.StatusOK, missing.Code, missing.Body.String())
	require.NoError(t, json.Unmarshal(missing.Body.Bytes(), &result))
	require.Empty(t, result.Items)
	for _, query := range []string{
		"", "?bates_label=", "?custodian_label=OUR", "?person_id=someone",
		"?bates_label=OUR000041&unknown=1", "?bates_label=OUR000041&custodian_label=OUR",
		"?bates_label=OUR000041&bates_label=OUR000041",
		"?bates_label=OUR000041&cursor=&cursor=", "?bates_label=OUR000041&limit=50&limit=50",
		"?bates_label=OUR000041&invalid=%zz",
	} {
		response := srv.call(t, http.MethodGet, path+query, "", browser)
		require.Equal(t, http.StatusForbidden, response.Code, query+": "+response.Body.String())
		require.Contains(t, response.Body.String(), "web_session_read_only")
	}
	for _, target := range []struct{ method, path string }{
		{http.MethodPost, path},
		{http.MethodGet, "/api/v1/bates/exports/" + allocation.AllocationID + "/content"},
	} {
		response := srv.call(t, target.method, target.path, "", browser)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	}
	keyRead := srv.get(t, path+"?custodian_label=Unknown")
	require.Equal(t, http.StatusOK, keyRead.Code, keyRead.Body.String())
}
