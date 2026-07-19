package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
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

	firstHistory, err := c.AuditHistory(t.Context(), "", inherited.ID, 1, "")
	require.NoError(t, err)
	assert.Equal(t, "/Taxes/2027", firstHistory.Path)
	assert.Equal(t, 2, firstHistory.Total)
	require.Len(t, firstHistory.Items, 1)
	assert.Equal(t, "node_create", firstHistory.Items[0].Kind)
	require.NotEmpty(t, firstHistory.NextCursor)
	secondHistory, err := c.AuditHistory(
		t.Context(), "/Taxes/2027", 0, 1, firstHistory.NextCursor,
	)
	require.NoError(t, err)
	require.Len(t, secondHistory.Items, 1)
	assert.Equal(t, "audit_inherit", secondHistory.Items[0].Kind)
	assert.Empty(t, secondHistory.NextCursor)

	trashed, _, err := s.Trash(t.Context(), inherited.ID, inherited.Revision)
	require.NoError(t, err)
	trashHistory, err := c.AuditHistory(t.Context(), "", inherited.ID, 10, "")
	require.NoError(t, err)
	require.NotNil(t, trashHistory.Items[0].NewPath)
	assert.Equal(t, "trash", trashHistory.Items[0].NewPath.State)
	assert.Equal(t, "@trash/known/Taxes/2027", trashHistory.Items[0].NewPath.Path)
	restored, _, err := s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	restoreHistory, err := c.AuditHistory(t.Context(), "/Taxes/2027", 0, 10, "")
	require.NoError(t, err)
	require.NotNil(t, restoreHistory.Items[0].OldPath)
	assert.Equal(t, "trash", restoreHistory.Items[0].OldPath.State)
	assert.Equal(t, "@trash/known/Taxes/2027", restoreHistory.Items[0].OldPath.Path)

	tag, err := s.CreateTag(t.Context(), "reviewed")
	require.NoError(t, err)
	_, err = s.AssignTag(t.Context(), tag.ID, inherited.ID, restored.Revision)
	require.NoError(t, err)
	tagHistory, err := c.AuditHistory(t.Context(), "", inherited.ID, 10, "")
	require.NoError(t, err)
	require.NotNil(t, tagHistory.Items[0].Attachment)
	assert.Equal(t, "tag_assignment", tagHistory.Items[0].Attachment.Kind)
	assert.Equal(t, tag.ID, tagHistory.Items[0].Attachment.Identity.TagID)
	require.NotNil(t, tagHistory.Items[0].Attachment.After)

	run, err := s.BeginIngest(t.Context(), "cli", "/source/taxes")
	require.NoError(t, err)
	ingested, added, err := s.IngestFile(
		t.Context(), run, taxes.ID, "receipt.txt", testHash("audit provenance"), 7,
		"text/plain", "/source/taxes/receipt.txt", "2026-07-18T12:00:00Z",
	)
	require.NoError(t, err)
	require.True(t, added)
	provenanceHistory, err := c.AuditHistory(t.Context(), "", ingested.ID, 10, "")
	require.NoError(t, err)
	require.NotEmpty(t, provenanceHistory.Items)
	provenanceEvent := provenanceHistory.Items[0]
	assert.Equal(t, "provenance_add", provenanceEvent.Kind)
	require.NotNil(t, provenanceEvent.Attachment)
	assert.Equal(t, "provenance", provenanceEvent.Attachment.Kind)
	require.NotNil(t, provenanceEvent.Attachment.After)
	assert.Equal(t, "/source/taxes/receipt.txt",
		*provenanceEvent.Attachment.After.OriginalPath)

	_, err = c.AuditHistory(t.Context(), "", s.RootID(), 10, "")
	require.ErrorIs(t, err, store.ErrAuditNotEnrolled)

	resp, raw := do(t, ts, http.MethodGet, fmt.Sprintf(
		"/api/v1/audit/history?node_id=%d&limit=10&cursor=bad", inherited.ID,
	), nil, nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	var cursorProblem api.Error
	require.NoError(t, json.Unmarshal([]byte(raw), &cursorProblem))
	assert.Equal(t, "invalid_audit_cursor", cursorProblem.Code)

	_, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
	require.ErrorIs(t, err, store.ErrAuditPreviewStale)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditVerifyReturnsStableEvidenceAndChecksProtectedBytes(t *testing.T) {
	ts, s := newTestServer(t, nil)
	c := client.New(ts.URL, testAPIKey)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		ts.URL+"/api/v1/audit/verify", nil)
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", testAPIKey)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	dormant, err := c.VerifyAudit(t.Context(), nil)
	require.NoError(t, err)
	assert.False(t, dormant.Enabled)
	assert.Nil(t, dormant.Evidence)

	file := createFileWithContent(t, ts, s, "/record.txt", "protected content")
	preview, err := c.PreviewAudit(t.Context(), client.AuditPreviewOptions{NodeID: s.RootID()})
	require.NoError(t, err)
	status, err := c.EnableAudit(t.Context(), preview.PreviewToken, true)
	require.NoError(t, err)

	report, err := c.VerifyAudit(t.Context(), nil)
	require.NoError(t, err)
	assert.True(t, report.Enabled)
	require.NotNil(t, report.Evidence)
	assert.Equal(t, status.VaultID, report.Evidence.VaultID)
	assert.Equal(t, status.LineageID, report.Evidence.LineageID)
	assert.Equal(t, status.AllocationHead, report.Evidence.AllocationHead)
	require.Len(t, report.Evidence.Scopes, 1)
	assert.Equal(t, status.Scopes[0].ChainHead, report.Evidence.Scopes[0].ChainHead)
	assert.Equal(t, 1, report.ProtectedBlobs)
	assert.Equal(t, int64(len("protected content")), report.ProtectedBytes)
	assert.Equal(t, 1, report.VerifiedBlobs)
	assert.Empty(t, report.Problems)
	assert.Empty(t, report.MetadataProblems)
	recorded := *report.Evidence
	invalid := recorded
	invalid.AllocationEntryCount++
	raw, err := json.Marshal(api.AuditVerifyRequest{Expected: &invalid})
	require.NoError(t, err)
	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost,
		ts.URL+"/api/v1/audit/verify", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", testAPIKey)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	_, err = s.Mkdir(t.Context(), s.RootID(), "after-evidence")
	require.NoError(t, err)
	report, err = c.VerifyAudit(t.Context(), &recorded)
	require.NoError(t, err)
	require.NotNil(t, report.EvidenceCheck)
	assert.True(t, report.EvidenceCheck.Extends)
	assert.Empty(t, report.EvidenceCheck.Problems)

	divergent := recorded
	divergent.AllocationHead = testHash("divergent allocation")
	report, err = c.VerifyAudit(t.Context(), &divergent)
	require.NoError(t, err)
	require.NotNil(t, report.EvidenceCheck)
	assert.False(t, report.EvidenceCheck.Extends)
	require.Len(t, report.EvidenceCheck.Problems, 1)
	assert.Equal(t, "allocation_diverged", report.EvidenceCheck.Problems[0].Code)

	require.NoError(t, s.Blobs.Remove(file.BlobHash))
	report, err = c.VerifyAudit(t.Context(), &divergent)
	require.NoError(t, err)
	require.NotNil(t, report.EvidenceCheck)
	require.Len(t, report.EvidenceCheck.Problems, 1)
	assert.Equal(t, "allocation_diverged", report.EvidenceCheck.Problems[0].Code)
	assert.Equal(t, 0, report.VerifiedBlobs)
	require.Len(t, report.Problems, 1)
	assert.Equal(t, file.BlobHash, report.Problems[0].Hash)
	assert.Equal(t, "missing", report.Problems[0].Problem)
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
		{
			name: "history", method: http.MethodGet,
			path: "/api/v1/audit/history?path=Taxes&limit=10",
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
