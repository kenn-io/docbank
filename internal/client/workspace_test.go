package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

func TestWorkspaceClientRoundTripsExactPagesAndSavedRuns(t *testing.T) {
	c, s := newClient(t, serverKey)
	for i, name := range []string{"alpha.txt", "beta.txt"} {
		_, err := s.CreateFile(t.Context(), s.RootID(), name, strings.Repeat(string(rune('a'+i)), 64), int64(i+1), "text/plain")
		require.NoError(t, err)
	}

	first, err := c.CreateWorkspaceQuery(t.Context(), api.WorkspaceQueryCreateRequest{
		Query: api.QueryPayload(`{}`), PageSize: 50, Facets: []string{"size"},
	})
	require.NoError(t, err)
	require.True(t, first.Snapshot)
	require.Len(t, first.Rows, 2)
	assert.Empty(t, first.NextCursor)
	assert.Len(t, first.MemberHash, 64)

	saved, err := c.CreateSavedQuery(t.Context(), api.SavedQueryCreateRequest{
		Name: "All files", Kind: store.SavedQueryKindQuery, Payload: api.SavedQueryPayload(`{}`),
	})
	require.NoError(t, err)
	run, err := c.RunSavedQuery(t.Context(), saved.ID, saved.Revision, api.SavedQueryRunRequest{PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, saved.ID, run.Run.SavedQueryID)
	assert.Equal(t, run.Run.SnapshotID, run.Snapshot.SnapshotID)
	assert.Equal(t, first.MemberHash, run.Snapshot.MemberHash)

	_, err = c.ReadWorkspaceQueryPage(t.Context(), strings.Repeat("0", 32), "opaque")
	require.ErrorIs(t, err, store.ErrSnapshotGone)
}

func TestWorkspaceClientRejectsInvalidRequestsBeforeTransport(t *testing.T) {
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(ts.Close)
	c := client.New(ts.URL, "key")

	_, err := c.CreateWorkspaceQuery(t.Context(), api.WorkspaceQueryCreateRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query")
	_, err = c.CreateWorkspaceQuery(t.Context(), api.WorkspaceQueryCreateRequest{
		Query: api.QueryPayload(`{}`), PageSize: 51,
	})
	require.Error(t, err)
	_, err = c.ReadWorkspaceQueryPage(t.Context(), "not-a-snapshot", "opaque")
	require.Error(t, err)
	_, err = c.ReadWorkspaceQueryPage(t.Context(), strings.Repeat("0", 32), "")
	require.Error(t, err)
	_, err = c.RunSavedQuery(t.Context(), "not-a-uuid", 1, api.SavedQueryRunRequest{})
	require.Error(t, err)
	assert.Zero(t, requests)
}

func TestWorkspaceClientRejectsSuccessfulResponseWithoutSnapshotAuthority(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"snapshot":false,"rows":[],"facets":[],"dependencies":[]}`))
	}))
	t.Cleanup(ts.Close)
	c := client.New(ts.URL, "key")

	_, err := c.CreateWorkspaceQuery(t.Context(), api.WorkspaceQueryCreateRequest{Query: api.QueryPayload(`{}`)})
	require.Error(t, err)
	require.NotErrorIs(t, err, store.ErrSnapshotGone)
	assert.Contains(t, err.Error(), "snapshot authority")
}
