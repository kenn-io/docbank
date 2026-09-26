package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestExportArchiveAuthorityChecksOwnerAndLiveSourceWithoutBytes(t *testing.T) {
	f := completedArchiveReadFixture(t)
	path := "/api/v1/exports/jobs/" + f.job.ID + "/archive/authority"
	response, body := do(t, f.server, http.MethodGet, path, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, "application/json", response.Header.Get("Content-Type"))
	require.Contains(t, body, `"sha256":"`+f.job.Receipt.SHA256+`"`)
	require.Contains(t, body, `"plan_fingerprint":"`+f.job.Receipt.PlanFingerprint+`"`)
	require.Less(t, len(body), 4096)

	response, body = do(t, f.server, http.MethodGet, path, map[string]string{"X-Api-Key": ""}, nil)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
	webToken := issueWebSession(t, f.server)
	response, body = do(t, f.server, http.MethodGet, path,
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: webToken}, nil)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)

	_, _, err := f.catalog.Trash(t.Context(), f.node.ID, f.node.Revision)
	require.NoError(t, err)
	response, body = do(t, f.server, http.MethodGet, path, nil, nil)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, `"code":"visibility_changed"`)
}

func TestExportArchiveAuthorityIsDiscoverable(t *testing.T) {
	operation := api.NewOfflineServer().API().OpenAPI().Paths["/api/v1/exports/jobs/{id}/archive/authority"]
	require.NotNil(t, operation)
	require.NotNil(t, operation.Get)
	require.Equal(t, "getExportArchiveAuthority", operation.Get.OperationID)
}
