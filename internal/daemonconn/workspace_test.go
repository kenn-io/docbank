package daemonconn_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestWorkspaceClientSavedRunAfterPersonMerge(t *testing.T) {
	c, s := newClient(t, serverKey)
	ctx := t.Context()
	survivor, err := s.CreatePerson(ctx, "Survivor", "operator")
	require.NoError(t, err)
	retired, err := s.CreatePerson(ctx, "Retired", "operator")
	require.NoError(t, err)
	payload := fmt.Appendf(nil, `{"filters":{"people":[{"person_id":%q,"roles":["sender"]}]}}`, retired.PersonID)
	saved, err := c.CreateSavedQuery(ctx, api.SavedQueryCreateRequest{Name: "Person filter", Kind: store.SavedQueryKindQuery, Payload: payload})
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, retired.PersonID, "11111111-1111-4111-8111-111111111111", survivor.Revision, retired.Revision)
	require.NoError(t, err)
	run, err := c.RunSavedQuery(ctx, saved.ID, saved.Revision, api.SavedQueryRunRequest{PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, saved.Fingerprint, run.Run.QueryFingerprint)
	require.Equal(t, run.Run.QueryFingerprint, run.Snapshot.QueryFingerprint)
	require.Equal(t, saved.ID, run.Run.SavedQueryID)
	require.Equal(t, saved.Revision, run.Run.SavedQueryRevision)
	for _, tc := range []struct {
		name   string
		mutate func(*api.SavedQueryRunResult)
	}{
		{"definition fingerprint", func(result *api.SavedQueryRunResult) { result.Run.QueryFingerprint = "invalid" }},
		{"snapshot fingerprint", func(result *api.SavedQueryRunResult) {
			result.Snapshot.QueryFingerprint = "sha256:" + strings.Repeat("0", 64)
		}},
		{"saved revision", func(result *api.SavedQueryRunResult) { result.Run.SavedQueryRevision++ }},
		{"snapshot identity", func(result *api.SavedQueryRunResult) { result.Run.SnapshotID = strings.Repeat("0", 32) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := run
			tc.mutate(&value)
			raw, err := json.Marshal(value)
			require.NoError(t, err)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			}))
			t.Cleanup(ts.Close)
			_, err = daemonconn.New(ts.URL, "key").RunSavedQuery(ctx, saved.ID, saved.Revision, api.SavedQueryRunRequest{PageSize: 50})
			require.Error(t, err)
		})
	}
}

func TestWorkspaceClientRoundTripsExactPagesAndSavedRuns(t *testing.T) {
	c, s := newClient(t, serverKey)
	for i, name := range []string{"alpha.txt", "beta.txt"} {
		_, err := s.CreateFile(t.Context(), s.RootID(), name, strings.Repeat(string(rune('a'+i)), 64), int64(i+1), "text/plain")
		require.NoError(t, err)
	}

	facets := []string{"size", "tags"}
	first, err := c.CreateWorkspaceQuery(t.Context(), api.WorkspaceQueryCreateRequest{
		Query: api.QueryPayload(`{}`), PageSize: 50, Facets: facets,
	})
	require.NoError(t, err)
	require.True(t, first.Snapshot)
	require.Len(t, first.Rows, 2)
	assert.Empty(t, first.NextCursor)
	assert.Len(t, first.MemberHash, 64)
	require.Len(t, first.Facets, len(facets))
	for i, dimension := range facets {
		assert.Equal(t, dimension, first.Facets[i].Dimension)
	}

	saved, err := c.CreateSavedQuery(t.Context(), api.SavedQueryCreateRequest{
		Name: "All files", Kind: store.SavedQueryKindQuery, Payload: api.SavedQueryPayload(`{}`),
	})
	require.NoError(t, err)
	run, err := c.RunSavedQuery(t.Context(), saved.ID, saved.Revision, api.SavedQueryRunRequest{PageSize: 50, Facets: facets})
	require.NoError(t, err)
	assert.Equal(t, saved.ID, run.Run.SavedQueryID)
	assert.Equal(t, run.Run.SnapshotID, run.Snapshot.SnapshotID)
	assert.Equal(t, first.MemberHash, run.Snapshot.MemberHash)
	require.Len(t, run.Snapshot.Facets, len(facets))
	for i, dimension := range facets {
		assert.Equal(t, dimension, run.Snapshot.Facets[i].Dimension)
	}

	_, err = c.ReadWorkspaceQueryPage(t.Context(), strings.Repeat("0", 32), "opaque")
	require.ErrorIs(t, err, store.ErrSnapshotGone)
}

func TestWorkspaceClientRejectsInvalidRequestsBeforeTransport(t *testing.T) {
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(ts.Close)
	c := daemonconn.New(ts.URL, "key")

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
	c := daemonconn.New(ts.URL, "key")

	_, err := c.CreateWorkspaceQuery(t.Context(), api.WorkspaceQueryCreateRequest{Query: api.QueryPayload(`{}`)})
	require.Error(t, err)
	require.NotErrorIs(t, err, store.ErrSnapshotGone)
	assert.Contains(t, err.Error(), "snapshot authority")
}
