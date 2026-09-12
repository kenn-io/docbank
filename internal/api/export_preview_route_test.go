package api_test

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
)

func TestExportPreviewBrowserRouteOwnerAndExactAllowlist(t *testing.T) {
	ts, s := newTestServer(t, nil)
	token := issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}
	owner := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	n := createFileWithContent(t, ts, s, "/synthetic-preview.txt", "original")
	source, err := s.CreateExportSource(t.Context(), owner, bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}}, nil)
	require.NoError(t, err)
	p, err := s.CreateExportPlan(t.Context(), owner, bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "pages", AllowUnavailable: true}}})
	require.NoError(t, err)
	path := "/api/v1/exports/plans/" + p.ID + "/preview"
	resp, body := do(t, ts, http.MethodGet, path, headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, `"unavailable_members":1`)
	for _, suffix := range []string{"?other=1", "/", "/other"} {
		resp, body = do(t, ts, http.MethodGet, path+suffix, headers, nil)
		require.NotEqual(t, http.StatusOK, resp.StatusCode, body)
	}
	resp, body = do(t, ts, http.MethodPost, path, headers, struct{}{})
	require.NotEqual(t, http.StatusOK, resp.StatusCode, body)
	other := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	resp, body = do(t, ts, http.MethodGet, path, other, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
}
