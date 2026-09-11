package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func TestAuthenticatedSnapshotOwnerIsExplicitAndBrowserRevocationCancelsRequest(t *testing.T) {
	revoked := make(chan string, 1)
	sessions := newWebSessionRegistry(func(owner string) { revoked <- owner })
	token, _, err := sessions.issue()
	require.NoError(t, err)
	entered := make(chan string, 1)
	canceled := make(chan struct{})
	handler := authMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		owner, ok := workspaceSnapshotOwner(r.Context())
		if !ok {
			entered <- "missing"
			return
		}
		entered <- owner
		<-r.Context().Done()
		close(canceled)
	}), "master-key", sessions, "master-owner")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/queries", nil)
	req.Header.Set(WebSessionHeader, token)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	select {
	case owner := <-entered:
		assert.Len(t, owner, 64)
		assert.NotEqual(t, token, owner)
	case <-time.After(time.Second):
		t.Fatal("authenticated handler was not entered")
	}
	sessions.revoke(token)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel admitted browser request")
	}
	select {
	case revokedOwner := <-revoked:
		assert.Len(t, revokedOwner, 64)
		assert.NotEqual(t, token, revokedOwner)
	case <-time.After(time.Second):
		t.Fatal("revocation did not invalidate browser snapshot owner")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled handler did not return")
	}
}

func TestServerShutdownStartsSnapshotCancellationWhenSessionDrainTimesOut(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	_, err = catalog.CreateFile(t.Context(), catalog.RootID(), "bounded.txt",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1, "text/plain")
	require.NoError(t, err)
	value, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	snapshots := store.NewQuerySnapshotService(catalog)
	t.Cleanup(func() { require.NoError(t, snapshots.Close()) })
	page, err := snapshots.Create(t.Context(), "owner", store.SnapshotRequest{Query: value})
	require.NoError(t, err)
	sessions := newWebSessionRegistry()
	sessions.uploadGroup.Add(1)
	defer sessions.uploadGroup.Done()
	server := &Server{snapshots: snapshots, webSessions: sessions}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	err = server.Shutdown(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = snapshots.CopyMembers(t.Context(), "owner", page.SnapshotID, page.MemberHash)
	require.ErrorIs(t, err, store.ErrSnapshotGone)
}

func TestAuthenticatedSnapshotOwnerHasNoMissingContextMasterFallback(t *testing.T) {
	owner, ok := workspaceSnapshotOwner(t.Context())
	assert.Empty(t, owner)
	assert.False(t, ok)
}

func TestWorkspaceFacetResponseRetainsSelectedValueBeyondTopFifty(t *testing.T) {
	value, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	values := make([]store.SnapshotFacetValue, 51)
	for i := range values {
		values[i] = store.SnapshotFacetValue{Key: string(rune('A' + i)), Count: 1}
	}
	values[50].Selected = true
	zero := int64(0)
	wire, err := fromStoreWorkspacePage(store.SnapshotPage{
		Query: value, Facets: []store.SnapshotFacet{{
			Dimension: "tags", Available: true, Total: &zero,
			Missing: &zero, Other: &zero, Values: values,
		}},
	})
	require.NoError(t, err)
	encoded, err := json.Marshal(wire)
	require.NoError(t, err)
	var decoded WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Len(t, decoded.Facets[0].Values, 51)
	assert.True(t, decoded.Facets[0].Values[50].Selected)
}
