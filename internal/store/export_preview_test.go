package store

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

func TestExportPreviewUsesFrozenReceiptsAndOwner(t *testing.T) {
	s := newTestStore(t)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("ab"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}}, nil)
	require.NoError(t, err)
	p, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "text", AllowUnavailable: true}, {Role: "pages", AllowUnavailable: true}}})
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(t.Context(), n.ID, n.Revision, fakeHash("ac"), 99, "text/plain")
	require.NoError(t, err)
	preview, err := s.ExportPlanPreview(t.Context(), "owner", p.ID)
	require.NoError(t, err)
	require.Equal(t, p.Fingerprint, preview.Fingerprint)
	require.Equal(t, source.MemberHash, preview.MemberHash)
	require.Equal(t, 1, preview.Total)
	require.Equal(t, []bundle.RoleSummary{
		{Role: "original", AvailableMembers: 1, Files: 1, Bytes: 12},
		{Role: "text", UnavailableMembers: 1, UnavailableReason: "No eligible retained text in the frozen plan."},
		{Role: "pages", UnavailableMembers: 1, UnavailableReason: "No complete retained page recipe in the frozen plan."},
	}, preview.Roles)
	_, err = s.ExportPlanPreview(t.Context(), "other", p.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.ExportPlanPreview(t.Context(), "owner", "invalid")
	require.ErrorIs(t, err, bundle.ErrConflict)
	p.ExpiresAt = "2000-01-01T00:00:00Z"
	raw, err := canonical.Marshal(p)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE export_plans SET canonical_json=? WHERE id=?`, raw, p.ID)
	require.NoError(t, err)
	_, err = s.ExportPlanPreview(t.Context(), "owner", p.ID)
	require.ErrorIs(t, err, bundle.ErrExpired)
}
