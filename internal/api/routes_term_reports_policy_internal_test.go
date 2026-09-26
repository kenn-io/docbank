package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
)

// Calling the route adapter directly bypasses authMiddleware's route allowlist.
// The handlers must still refuse a scoped principal before touching a shared
// report owner, history store, or cached export.
func TestTermReportHandlersDenyScopedPrincipalBeforeSharedState(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("synthetic", "1"))
	registerTermReportRoutes(api, Deps{}, NewOperationGate(), nil, nil, nil)

	requestBody := `{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only","terms":[{"number":1,"expression":"alpha","syntax":"simple","dates":{"start":"2026-01-01","end":"2026-12-31"}}]}`
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/search-exports", requestBody},
		{http.MethodGet, "/api/v1/search-exports", ""},
		{http.MethodGet, "/api/v1/search-exports/synthetic-handle", ""},
		{http.MethodPost, "/api/v1/search-exports/synthetic-handle/dates", `{}`},
		{http.MethodPost, "/api/v1/search-exports/synthetic-handle/revisions", `{"choices":[]}`},
		{http.MethodPost, "/api/v1/search-exports/synthetic-handle/download", `{"format":"csv"}`},
		{http.MethodGet, "/api/v1/search-exports/synthetic-handle/csv", ""},
		{http.MethodGet, "/api/v1/search-exports/synthetic-handle/bundle", ""},
	}
	tested := make(map[string]bool, len(routes))
	for _, route := range routes {
		tested[route.method+" "+strings.Replace(route.path, "synthetic-handle", "{id}", 1)] = true
		for _, subject := range []string{"synthetic:alpha", "synthetic:beta"} {
			t.Run(subject+route.method+route.path, func(t *testing.T) {
				request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
				request.Header.Set("Content-Type", "application/json")
				ctx := context.WithValue(request.Context(), workspaceSnapshotOwnerContextKey{}, "shared-owner")
				ctx = ContextWithPrincipal(ctx, Principal{
					SubjectID: subject, CredentialKind: "synthetic", Operations: []Operation{OperationRead, OperationExportShare},
					SourceIDs: []string{subject + ":source"}, GrantRevision: 1,
				})
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, request.WithContext(ctx))
				require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			})
		}
	}
	// If a new export operation is registered, this inventory forces a direct
	// handler-path denial case instead of relying on the outer route allowlist.
	registered := 0
	for path, item := range api.OpenAPI().Paths {
		if !strings.HasPrefix(path, "/api/v1/search-exports") {
			continue
		}
		for _, operation := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete,
			item.Options, item.Head, item.Patch, item.Trace,
		} {
			if operation != nil {
				registered++
				require.True(t, tested[operation.Method+" "+path], "untested report route: %s %s", operation.Method, path)
			}
		}
	}
	require.Equal(t, len(tested), registered)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/search-exports", nil)
	request = request.WithContext(context.WithValue(request.Context(), workspaceSnapshotOwnerContextKey{}, "shared-owner"))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())

	request = request.WithContext(ContextWithPrincipal(request.Context(), LocalAdminPrincipal()))
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
}
