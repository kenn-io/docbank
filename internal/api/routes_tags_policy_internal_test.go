package api

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestScopedTagAssignmentRejectsInvalidReceiptFenceBeforeMutation(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "synthetic.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "synthetic.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	tag, err := catalog.CreateTag(t.Context(), "synthetic-tag")
	require.NoError(t, err)
	now := time.Now()
	principal := Principal{
		SubjectID: "synthetic:invalid-extra", CredentialKind: "machine", Audience: "docbank:test",
		Operations: []Operation{OperationRead, OperationMetadataMutation},
		SourceIDs:  []string{node.CurrentVersionID, "invalid-source"}, GrantRevision: 1,
		ExpiresAt: now.Add(time.Hour),
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("synthetic", "1"))
	registerTagAssignmentRoute(api, Deps{
		Store: catalog,
		OperationPolicy: NewOperationPolicy(OperationPolicyOptions{
			Authority: &testGrantAuthority{grant: principal}, Now: func() time.Time { return now },
		}),
	}, NewOperationGate(), http.MethodPut, true)
	request := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/api/v1/nodes/%d/tags/%s", node.ID, tag.ID), nil)
	request.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(node.Revision, 10)))
	request = request.WithContext(ContextWithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	require.NotEqual(t, http.StatusOK, response.Code, response.Body.String())
	assigned, total, err := catalog.NodeTags(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, assigned)
}

// Exercise the route adapter without authMiddleware's scoped-route allowlist.
// A path selector has no grant-fenced content version and must stay owner-only.
func TestTagPathAssignmentHandlerRequiresLocalOwner(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "synthetic.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	_, err = catalog.Mkdir(t.Context(), catalog.RootID(), "synthetic-directory")
	require.NoError(t, err)
	tag, err := catalog.CreateTag(t.Context(), "synthetic-tag")
	require.NoError(t, err)

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("synthetic", "1"))
	deps := Deps{Store: catalog, OperationPolicy: NewOperationPolicy(OperationPolicyOptions{})}
	gate := NewOperationGate()
	registerTagPathAssignmentRoute(api, deps, gate, http.MethodPut, true)
	registerTagPathAssignmentRoute(api, deps, gate, http.MethodDelete, false)
	path := "/api/v1/path/tags/" + tag.ID
	request := func(method string, principal Principal) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(`{"path":"/synthetic-directory"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(ContextWithPrincipal(req.Context(), principal))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		return response
	}
	for _, subject := range []string{"synthetic:alpha", "synthetic:beta"} {
		principal := Principal{SubjectID: subject, CredentialKind: "machine", Audience: "synthetic",
			Operations: []Operation{OperationMetadataMutation}, GrantRevision: 1,
			ExpiresAt: time.Now().Add(time.Hour),
			SourceIDs: []string{"11111111-1111-4111-8111-111111111111"}}
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			response := request(method, principal)
			require.Equal(t, http.StatusForbidden, response.Code, "%s %s: %s", subject, method, response.Body.String())
		}
	}
	current, err := catalog.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	require.Zero(t, current.AssignmentCount)

	response := request(http.MethodPut, LocalAdminPrincipal())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	current, err = catalog.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	require.Equal(t, 1, current.AssignmentCount)
	response = request(http.MethodDelete, LocalAdminPrincipal())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	current, err = catalog.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	require.Zero(t, current.AssignmentCount)
}

func TestUnscopedTagReadHandlersDenyDirectScopedInvocation(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "synthetic.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	node, err := catalog.Mkdir(t.Context(), catalog.RootID(), "synthetic-directory")
	require.NoError(t, err)
	tag, err := catalog.CreateTag(t.Context(), "synthetic-tag")
	require.NoError(t, err)
	_, err = catalog.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
	require.NoError(t, err)
	tag, err = catalog.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)

	mux := http.NewServeMux()
	registerTagRoutes(humago.New(mux, huma.DefaultConfig("synthetic", "1")),
		Deps{Store: catalog, OperationPolicy: NewOperationPolicy(OperationPolicyOptions{})}, NewOperationGate())
	routes := []string{
		"/api/v1/tags/by-name?name=synthetic-tag",
		"/api/v1/tags/" + tag.ID,
		"/api/v1/tags/" + tag.ID + "/nodes?limit=100&offset=0",
		"/api/v1/nodes/" + strconv.FormatInt(node.ID, 10) + "/tags?limit=100&offset=0",
	}
	for _, path := range routes {
		for _, subject := range []string{"synthetic:alpha", "synthetic:beta"} {
			principal := Principal{SubjectID: subject, CredentialKind: "machine", Audience: "synthetic",
				Operations: []Operation{OperationRead}, GrantRevision: 1,
				ExpiresAt: time.Now().Add(time.Hour),
				SourceIDs: []string{"11111111-1111-4111-8111-111111111111"}}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request = request.WithContext(ContextWithPrincipal(request.Context(), principal))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			require.Equal(t, http.StatusForbidden, response.Code, "%s %s: %s", subject, path, response.Body.String())
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request = request.WithContext(ContextWithPrincipal(request.Context(), LocalAdminPrincipal()))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, "%s: %s", path, response.Body.String())
		if path == "/api/v1/tags/"+tag.ID {
			var body Tag
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			require.Equal(t, tag.Revision, body.Revision)
			require.Equal(t, tag.AssignmentCount, body.AssignmentCount)
			require.Equal(t, tagETag(tag), response.Header().Get("ETag"))
		}
	}
}
