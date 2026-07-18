package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditEnrollmentPreviewEnablesExactReviewedAuthority(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	var before bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &before))
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)

	plan, err := s.PreviewInitialAudit(t.Context(), target.ID, "api", nil)
	require.NoError(t, err)
	preview := plan.Preview()
	assert.Equal(t, s.VaultID(), preview.VaultID)
	assert.Equal(t, target.ID, preview.TargetNodeID)
	assert.Equal(t, "/Projects", preview.TargetPath)
	assert.Equal(t, 3, preview.MemberCount)
	assert.Equal(t, 1, preview.DirectoryCount)
	assert.Equal(t, 2, preview.FileCount)
	assert.Positive(t, preview.VersionCount)
	assert.Positive(t, preview.LogicalVersionBytes)
	assert.Positive(t, preview.UniqueBlobs)
	assert.Positive(t, preview.UniqueBlobBytes)
	assert.Positive(t, preview.VaultTopologyNodes)
	assert.Positive(t, preview.AuthorityJSONBytes)
	require.NoError(t, validateUUIDv4(preview.ScopeID))
	require.NoError(t, validateUUIDv4(preview.OperationID))
	assert.Len(t, preview.BaselineDigest, 64)
	counts, err := auditAuthorityCounts(t.Context(), s.db)
	require.NoError(t, err)
	assert.Equal(t, [5]int64{}, counts, "preview must not install authority")

	status, err := s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	assert.True(t, status.Enabled)
	assert.Equal(t, s.VaultID(), status.VaultID)
	assert.Equal(t, int64(1), status.OperationSequenceHighWater)
	assert.Equal(t, int64(1), status.AllocationEntryCount)
	require.Len(t, status.Scopes, 1)
	assert.Equal(t, preview.ScopeID, status.Scopes[0].ID)
	assert.Equal(t, preview.OperationID, status.Scopes[0].EnableOperationID)
	assert.Equal(t, preview.BaselineDigest, status.Scopes[0].BaselineDigest)
	assert.Equal(t, preview.MemberCount, status.Scopes[0].MemberCount)
	assert.Equal(t, preview.TargetPath, status.Scopes[0].TargetPath)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	var after bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &after))
	require.Greater(t, after.Len(), before.Len())
	assert.Equal(t, int64(after.Len()-before.Len()), preview.AuthorityJSONBytes,
		"preview must count the exact metadata-v1 audit JSONL growth")
	assertAuditMetadataRoundTrip(t, s)
}

func TestAuditEnrollmentPreviewRejectsChangedVaultState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), target.ID, "api", nil)
	require.NoError(t, err)

	_, err = s.Mkdir(t.Context(), s.RootID(), "Changed after preview")
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.ErrorIs(t, err, ErrAuditPreviewStale)
	counts, err := auditAuthorityCounts(t.Context(), s.db)
	require.NoError(t, err)
	assert.Equal(t, [5]int64{}, counts)
}

func TestAuditEnrollmentPreviewRejectsTrashedOrDeletedTargetAsStale(t *testing.T) {
	for _, hardDelete := range []bool{false, true} {
		name := "trashed"
		if hardDelete {
			name = "deleted"
		}
		t.Run(name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			seedMetadataRoundTrip(t, s)
			target, err := s.NodeByPath(t.Context(), "/Projects")
			require.NoError(t, err)
			plan, err := s.PreviewInitialAudit(t.Context(), target.ID, "api", nil)
			require.NoError(t, err)

			_, _, err = s.Trash(t.Context(), target.ID, target.Revision)
			require.NoError(t, err)
			if hardDelete {
				_, err = s.TrashEmpty(t.Context(), 0, true)
				require.NoError(t, err)
			}
			_, err = s.EnableInitialAudit(t.Context(), plan)
			require.ErrorIs(t, err, ErrAuditPreviewStale)
			counts, err := auditAuthorityCounts(t.Context(), s.db)
			require.NoError(t, err)
			assert.Equal(t, [5]int64{}, counts)
		})
	}
}

func TestAuditEnrollmentPreviewIsVaultBoundAndFirstActivationOnly(t *testing.T) {
	first, err := Open(filepath.Join(t.TempDir(), "first.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	seedMetadataRoundTrip(t, first)
	target, err := first.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	plan, err := first.PreviewInitialAudit(t.Context(), target.ID, "api", nil)
	require.NoError(t, err)

	second, err := Open(filepath.Join(t.TempDir(), "second.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	_, err = second.EnableInitialAudit(t.Context(), plan)
	require.ErrorIs(t, err, ErrAuditPreviewStale)

	_, err = first.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	_, err = first.PreviewInitialAudit(t.Context(), target.ID, "api", nil)
	require.ErrorIs(t, err, ErrAuditAlreadyEnabled)
	_, err = first.EnableInitialAudit(t.Context(), plan)
	require.ErrorIs(t, err, ErrAuditAlreadyEnabled)
}

func TestAuditStatusExplainsDormantAndProtectedNodes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)

	status, err := s.AuditStatus(t.Context(), &report.ID)
	require.NoError(t, err)
	assert.False(t, status.Enabled)
	require.NotNil(t, status.Membership)
	assert.False(t, status.Membership.Protected)
	assert.Equal(t, "/Projects/report.txt", status.Membership.Path)

	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), projects.ID, "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	status, err = s.AuditStatus(t.Context(), &report.ID)
	require.NoError(t, err)
	require.NotNil(t, status.Membership)
	assert.True(t, status.Membership.Protected)
	assert.Equal(t, []string{plan.Preview().ScopeID}, status.Membership.ScopeIDs)
	assert.Equal(t, []string{plan.Preview().BaselineDigest}, status.Membership.BaselineDigests)

	missing := int64(99999)
	_, err = s.AuditStatus(t.Context(), &missing)
	require.ErrorIs(t, err, ErrNotFound)
}
