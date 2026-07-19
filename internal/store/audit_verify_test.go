package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyAuditReturnsReplayedEvidenceAndProtectedBlobs(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)

	dormant, err := s.VerifyAudit(t.Context())
	require.NoError(t, err)
	assert.False(t, dormant.Evidence.Enabled)
	assert.Empty(t, dormant.ProtectedBlobs)
	assert.Zero(t, dormant.ProtectedBytes)

	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), projects.ID, "api", nil)
	require.NoError(t, err)
	enabled, err := s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)

	verified, err := s.VerifyAudit(t.Context())
	require.NoError(t, err)
	assert.True(t, verified.Evidence.Enabled)
	assert.Equal(t, enabled.VaultID, verified.Evidence.VaultID)
	assert.Equal(t, enabled.LineageID, verified.Evidence.LineageID)
	assert.Equal(t, enabled.OperationSequenceHighWater,
		verified.Evidence.OperationSequenceHighWater)
	assert.Equal(t, enabled.AllocationEntryCount, verified.Evidence.AllocationEntryCount)
	assert.Equal(t, enabled.AllocationHead, verified.Evidence.AllocationHead)
	require.Len(t, verified.Evidence.Scopes, 1)
	assert.Equal(t, enabled.Scopes[0].ID, verified.Evidence.Scopes[0].ID)
	assert.Equal(t, enabled.Scopes[0].EntryCount, verified.Evidence.Scopes[0].EntryCount)
	assert.Equal(t, enabled.Scopes[0].ChainHead, verified.Evidence.Scopes[0].ChainHead)
	assert.Equal(t, []BlobInfo{
		{Hash: metadataHashCurrent, Size: 12},
		{Hash: metadataHashTrashed, Size: 5},
		{Hash: metadataHashVersion, Size: 9},
	}, verified.ProtectedBlobs)
	assert.Equal(t, int64(26), verified.ProtectedBytes)
}

func TestVerifyAuditRejectsAuthorityThatDoesNotMatchReplay(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, projects.ID)

	_, err = s.db.ExecContext(t.Context(), `UPDATE audit_scopes SET chain_head=(
		SELECT digest FROM audit_records WHERE kind='enrollment_baseline' LIMIT 1
	)`)
	require.NoError(t, err)
	_, err = s.VerifyAudit(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "audit scope authority does not match replayed history")
}
