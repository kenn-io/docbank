package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestAuditPreviewEnableAndStatusLifecycle(t *testing.T) {
	ts, s := newTestServer(t, nil)
	taxes, err := s.Mkdir(t.Context(), s.RootID(), "Taxes")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), taxes.ID, "return.txt", testHash("return"), 6, "text/plain")
	require.NoError(t, err)
	c := client.New(ts.URL, testAPIKey)

	status, err := c.AuditStatus(t.Context(), "/Taxes/return.txt", 0)
	require.NoError(t, err)
	assert.False(t, status.Enabled)
	require.NotNil(t, status.Membership)
	assert.False(t, status.Membership.Protected)

	preview, err := c.PreviewAudit(t.Context(), client.AuditPreviewOptions{Path: "/Taxes"})
	require.NoError(t, err)
	assert.Equal(t, taxes.ID, preview.TargetNodeID)
	assert.Equal(t, 2, preview.MemberCount)
	assert.Equal(t, 1, preview.VersionCount)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/audit/enable", nil, map[string]any{
		"preview_token":                   preview.PreviewToken,
		"acknowledge_permanent_retention": false,
	})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	assert.Equal(t, "audit_acknowledgment_required", problem.Code)

	status, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
	require.NoError(t, err)
	assert.True(t, status.Enabled)
	require.Len(t, status.Scopes, 1)
	assert.Equal(t, preview.ScopeID, status.Scopes[0].ID)
	assert.Equal(t, preview.BaselineDigest, status.Scopes[0].BaselineDigest)
	status, err = c.AuditStatus(t.Context(), "", taxes.ID)
	require.NoError(t, err)
	require.NotNil(t, status.Membership)
	assert.Equal(t, []string{preview.BaselineDigest}, status.Membership.BaselineDigests)

	status, err = c.AuditStatus(t.Context(), "/Taxes/return.txt", 0)
	require.NoError(t, err)
	require.NotNil(t, status.Membership)
	assert.True(t, status.Membership.Protected)
	assert.Equal(t, []string{preview.ScopeID}, status.Membership.ScopeIDs)

	inherited, err := s.Mkdir(t.Context(), taxes.ID, "2027")
	require.NoError(t, err)
	status, err = c.AuditStatus(t.Context(), "", inherited.ID)
	require.NoError(t, err)
	require.NotNil(t, status.Membership)
	assert.True(t, status.Membership.Protected)
	assert.Equal(t, []string{preview.ScopeID}, status.Membership.ScopeIDs)
	require.Len(t, status.Membership.BaselineDigests, 1)
	assert.NotEqual(t, preview.BaselineDigest, status.Membership.BaselineDigests[0],
		"an inherited node binds to its creation baseline, not the scope enrollment baseline")

	_, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
	require.ErrorIs(t, err, store.ErrAuditPreviewStale)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditEnableRejectsPreviewAfterVaultMutation(t *testing.T) {
	ts, s := newTestServer(t, nil)
	taxes, err := s.Mkdir(t.Context(), s.RootID(), "Taxes")
	require.NoError(t, err)
	c := client.New(ts.URL, testAPIKey)
	preview, err := c.PreviewAudit(t.Context(), client.AuditPreviewOptions{NodeID: taxes.ID})
	require.NoError(t, err)

	_, err = s.Mkdir(t.Context(), s.RootID(), "Changed after review")
	require.NoError(t, err)
	_, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
	require.ErrorIs(t, err, store.ErrAuditPreviewStale)

	_, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
	require.ErrorIs(t, err, store.ErrAuditPreviewStale, "failed execution consumes the token")
	status, err := c.AuditStatus(t.Context(), "", 0)
	require.NoError(t, err)
	assert.False(t, status.Enabled)
}

func TestAuditEnableReportsStaleWhenTargetIsTrashedOrDeleted(t *testing.T) {
	for _, hardDelete := range []bool{false, true} {
		name := "trashed"
		if hardDelete {
			name = "deleted"
		}
		t.Run(name, func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			taxes, err := s.Mkdir(t.Context(), s.RootID(), "Taxes")
			require.NoError(t, err)
			c := client.New(ts.URL, testAPIKey)
			preview, err := c.PreviewAudit(t.Context(), client.AuditPreviewOptions{
				NodeID: taxes.ID,
			})
			require.NoError(t, err)

			_, _, err = s.Trash(t.Context(), taxes.ID, taxes.Revision)
			require.NoError(t, err)
			if hardDelete {
				_, err = s.TrashEmpty(t.Context(), 0, true)
				require.NoError(t, err)
			}
			_, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
			require.ErrorIs(t, err, store.ErrAuditPreviewStale)
		})
	}
}

func TestAuditPreviewRequiresOneTarget(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, body := range []map[string]any{
		{},
		{"path": "/", "node_id": 1},
	} {
		resp, raw := do(t, ts, http.MethodPost, "/api/v1/audit/preview", nil, body)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
		var problem api.Error
		require.NoError(t, json.Unmarshal([]byte(raw), &problem))
		assert.Equal(t, "validation", problem.Code)
	}
}

func TestAuditRoutesRejectRelativePaths(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	tests := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{
			name: "preview", method: http.MethodPost, path: "/api/v1/audit/preview",
			body: map[string]any{"path": "Taxes"},
		},
		{
			name: "status", method: http.MethodGet, path: "/api/v1/audit/status?path=Taxes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp, raw := do(t, ts, test.method, test.path, nil, test.body)
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
			var problem api.Error
			require.NoError(t, json.Unmarshal([]byte(raw), &problem))
			assert.Equal(t, "validation", problem.Code)
			assert.Contains(t, problem.Detail, "must be absolute")
		})
	}
}

func TestAuditPreviewRejectsMissingAndNonDirectoryTargets(t *testing.T) {
	ts, s := newTestServer(t, nil)
	file, err := s.CreateFile(t.Context(), s.RootID(), "return.txt", testHash("return"), 6, "text/plain")
	require.NoError(t, err)
	trashed, err := s.Mkdir(t.Context(), s.RootID(), "Old Taxes")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)

	tests := []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{name: "missing ID", body: map[string]any{"node_id": int64(999_999)}, status: http.StatusNotFound, code: "not_found"},
		{name: "trashed ID", body: map[string]any{"node_id": trashed.ID}, status: http.StatusNotFound, code: "not_found"},
		{name: "file ID", body: map[string]any{"node_id": file.ID}, status: http.StatusUnprocessableEntity, code: "not_dir"},
		{name: "file path", body: map[string]any{"path": "/return.txt"}, status: http.StatusUnprocessableEntity, code: "not_dir"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp, raw := do(t, ts, http.MethodPost, "/api/v1/audit/preview", nil, test.body)
			assert.Equal(t, test.status, resp.StatusCode)
			var problem api.Error
			require.NoError(t, json.Unmarshal([]byte(raw), &problem))
			assert.Equal(t, test.code, problem.Code)
		})
	}
}

func TestAuditPreviewTokenIsDaemonLocal(t *testing.T) {
	ts, s := newTestServer(t, nil)
	taxes, err := s.Mkdir(t.Context(), s.RootID(), "Taxes")
	require.NoError(t, err)
	c := client.New(ts.URL, testAPIKey)
	preview, err := c.PreviewAudit(t.Context(), client.AuditPreviewOptions{NodeID: taxes.ID})
	require.NoError(t, err)

	// A fresh server over the same store models daemon restart: no preview
	// registry is restored from disk or metadata backup.
	cfg := config.Default()
	cfg.Server.APIKey = testAPIKey
	restarted := api.NewServer(api.Deps{
		Store: s.Store, Blobs: s.Blobs, VaultRoot: filepath.Dir(s.DBPath),
		Cfg: cfg,
	})
	restartServer := httptest.NewServer(restarted.Handler())
	t.Cleanup(restartServer.Close)
	restartClient := client.New(restartServer.URL, testAPIKey)
	_, err = restartClient.EnableAudit(t.Context(), preview.PreviewToken, true)
	require.ErrorIs(t, err, store.ErrAuditPreviewStale)
}
