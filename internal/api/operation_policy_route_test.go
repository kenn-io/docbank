package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestScopedAPICapabilitiesAndProtectedReadsRecheckGrant(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var allowed, hidden store.Node
	authority := &routeGrantAuthority{}
	var authenticated api.Principal
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		allowed = createPolicyFile(t, deps, "allowed.txt", "allowed")
		hidden = createPolicyFile(t, deps, "hidden.txt", "hidden")
		authenticated = routePrincipal(now, allowed.CurrentVersionID)
		authority.set(authenticated)
		deps.OperationPolicy = api.NewOperationPolicy(api.OperationPolicyOptions{
			Authority: authority, Now: func() time.Time { return now },
		})
		deps.AuthenticatePrincipal = policyRouteAuthenticator(&authenticated)
	})
	headers := policyRouteHeaders()

	resp, body := get(t, ts, "/api/v1/capabilities", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var capabilities api.Capabilities
	require.NoError(t, json.Unmarshal([]byte(body), &capabilities))
	assert.Equal(t, []string{string(api.OperationRead)}, capabilities.Operations)

	resp, body = get(t, ts, "/api/v1/versions/"+allowed.CurrentVersionID, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/versions/"+hidden.CurrentVersionID, headers)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/versions/"+hidden.CurrentVersionID+"/content", headers)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/nodes/"+strconv.FormatInt(hidden.ID, 10), headers)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	authenticated.Local = true
	resp, body = get(t, ts, "/api/v1/versions/"+hidden.CurrentVersionID, headers)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	authenticated.Local = false

	// The first allowed read represents a filled caller cache. A newer current
	// grant must prevent disclosure on the very next selector read.
	authority.mutate(func(grant *api.Principal) { grant.GrantRevision++ })
	resp, body = get(t, ts, "/api/v1/versions/"+allowed.CurrentVersionID, headers)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)

	authority.set(authenticated)
	authority.mutate(func(grant *api.Principal) { grant.ExpiresAt = now })
	resp, body = get(t, ts, "/api/v1/versions/"+allowed.CurrentVersionID+"/content", headers)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
}

func TestScopedAPINarrowsFencesCountsAndWrites(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var allowed, hidden store.Node
	var hiddenJobID string
	authority := &routeGrantAuthority{}
	var authenticated api.Principal
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		allowed = createPolicyFile(t, deps, "allowed.txt", "allowed")
		hidden = createPolicyFile(t, deps, "hidden.txt", "hidden")
		visibleTag, err := deps.Store.CreateTag(t.Context(), "visible")
		require.NoError(t, err)
		_, err = deps.Store.AssignTag(t.Context(), visibleTag.ID, allowed.ID, allowed.Revision)
		require.NoError(t, err)
		allowed, err = deps.Store.NodeByID(t.Context(), allowed.ID)
		require.NoError(t, err)
		_, err = deps.Store.AssignTag(t.Context(), visibleTag.ID, hidden.ID, hidden.Revision)
		require.NoError(t, err)
		hiddenOnly, err := deps.Store.CreateTag(t.Context(), "hidden-only")
		require.NoError(t, err)
		hidden, err = deps.Store.NodeByID(t.Context(), hidden.ID)
		require.NoError(t, err)
		_, err = deps.Store.AssignTag(t.Context(), hiddenOnly.ID, hidden.ID, hidden.Revision)
		require.NoError(t, err)

		provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
		require.NoError(t, err)
		gate := api.NewOperationGate()
		deps.Gate = gate
		deps.Processing, err = processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			Profiles: map[string]processing.ProfileConfig{"private": {
				Profile:           processingTestProfile(provider.Descriptor()),
				RenditionProvider: provider,
			}},
		})
		require.NoError(t, err)
		selector := processing.Selector{NodeID: hidden.ID, ContentVersionID: hidden.CurrentVersionID, Profile: "private"}
		plan, err := deps.Processing.Plan(t.Context(), selector)
		require.NoError(t, err)
		job, err := deps.Processing.Start(t.Context(), processing.StartRequest{
			Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true,
		})
		require.NoError(t, err)
		hiddenJobID = job.ID

		authenticated = routePrincipal(now, allowed.CurrentVersionID)
		authenticated.Operations = append(authenticated.Operations, api.OperationProcessing)
		authority.set(authenticated)
		deps.OperationPolicy = api.NewOperationPolicy(api.OperationPolicyOptions{
			Authority: authority, Now: func() time.Time { return now },
		})
		deps.AuthenticatePrincipal = policyRouteAuthenticator(&authenticated)
	})
	headers := policyRouteHeaders()

	resp, body := do(t, ts, http.MethodPost, "/api/v1/processing/source-fences/resolve", headers,
		map[string]any{"content_version_ids": []string{hidden.CurrentVersionID, allowed.CurrentVersionID}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var resolution api.DocumentSourceFenceResolution
	require.NoError(t, json.Unmarshal([]byte(body), &resolution))
	assert.Equal(t, []string{allowed.CurrentVersionID}, resolution.Fence.ContentVersionIDs)

	resp, body = get(t, ts, "/api/v1/tags?limit=100&offset=0", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var tags api.TagPage
	require.NoError(t, json.Unmarshal([]byte(body), &tags))
	require.Len(t, tags.Items, 1)
	assert.Equal(t, "visible", tags.Items[0].Name)
	assert.Equal(t, 1, tags.Items[0].AssignmentCount)
	assert.Equal(t, 1, tags.Total)
	assert.NotContains(t, body, "hidden-only")

	resp, body = do(t, ts, http.MethodPost, "/api/v1/tags", headers, map[string]any{"name": "blocked"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, body)

	resp, body = get(t, ts, "/api/v1/processing/jobs/"+hiddenJobID, headers)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
}

func TestScopedDocumentQueriesEnforceCurrentGrantBeforeCatalogRead(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	authority := &routeGrantAuthority{}
	var authenticated api.Principal
	var allowed, hidden store.Node
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		allowed = createPolicyFile(t, deps, "allowed-query.txt", "allowed")
		hidden = createPolicyFile(t, deps, "hidden-query.txt", "hidden")
		authenticated = routePrincipal(now, allowed.CurrentVersionID)
		authority.set(authenticated)
		deps.OperationPolicy = api.NewOperationPolicy(api.OperationPolicyOptions{
			Authority: authority, Now: func() time.Time { return now },
		})
		deps.AuthenticatePrincipal = policyRouteAuthenticator(&authenticated)
	})
	headers := policyRouteHeaders()

	query := api.ScopedDocumentQuery{PageSize: 10,
		ContentVersionIDs: []string{allowed.CurrentVersionID, hidden.CurrentVersionID}}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/documents/scoped", headers, query)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, allowed.CurrentVersionID, page.Items[0].ContentVersionID)
	assert.NotContains(t, body, "hidden-query")
	query.ContentVersionIDs = []string{hidden.CurrentVersionID}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/documents/scoped", headers, query)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Empty(t, page.Items, "an empty grant intersection must not become an unrestricted query")
	query.ContentVersionIDs = []string{allowed.CurrentVersionID, hidden.CurrentVersionID}
	resp, body = get(t, ts, "/api/v1/documents", headers)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)

	resolve := api.DocumentSummaryResolveRequest{Identities: []api.DocumentIdentity{{
		NodeID: allowed.ID, ContentVersionID: allowed.CurrentVersionID, Path: "/allowed-query.txt",
	}}}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/documents/resolve", headers, resolve)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var summaries api.DocumentSummaryResolveResponse
	require.NoError(t, json.Unmarshal([]byte(body), &summaries))
	require.Len(t, summaries.Items, 1)
	assert.Equal(t, allowed.CurrentVersionID, summaries.Items[0].ContentVersionID)

	resolve.Identities = append(resolve.Identities, api.DocumentIdentity{
		NodeID: hidden.ID, ContentVersionID: hidden.CurrentVersionID, Path: "/hidden-query.txt"})
	resp, body = do(t, ts, http.MethodPost, "/api/v1/documents/resolve", headers, resolve)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)

	authority.mutate(func(grant *api.Principal) { grant.GrantRevision++ })
	resp, body = do(t, ts, http.MethodPost, "/api/v1/documents/scoped", headers, query)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, body)
}

func createPolicyFile(t *testing.T, deps *api.Deps, name, content string) store.Node {
	t.Helper()
	hash, size, err := deps.Blobs.Write(strings.NewReader(content))
	require.NoError(t, err)
	node, err := deps.Store.CreateFile(t.Context(), deps.Store.RootID(), name, hash, size, "text/plain")
	require.NoError(t, err)
	return node
}

func routePrincipal(now time.Time, sourceIDs ...string) api.Principal {
	return api.Principal{
		SubjectID: "subject:route-test", CredentialKind: "machine", Audience: "docbank:test",
		Operations: []api.Operation{api.OperationRead}, SourceIDs: sourceIDs,
		GrantRevision: 1, ExpiresAt: now.Add(time.Hour),
	}
}

func policyRouteAuthenticator(principal *api.Principal) api.PrincipalAuthenticator {
	return func(request *http.Request) (api.Principal, bool) {
		if request.Header.Get("Authorization") != "Bearer synthetic-remote" {
			return api.Principal{}, false
		}
		return *principal, true
	}
}

func policyRouteHeaders() map[string]string {
	return map[string]string{"X-Api-Key": "", "Authorization": "Bearer synthetic-remote"}
}

type routeGrantAuthority struct {
	mu    sync.Mutex
	grant api.Principal
}

func (a *routeGrantAuthority) CurrentGrant(_ context.Context, _ string) (api.Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.grant, nil
}

func (a *routeGrantAuthority) set(grant api.Principal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grant = grant
}

func (a *routeGrantAuthority) mutate(fn func(*api.Principal)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(&a.grant)
}
