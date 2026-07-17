package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/audit"
)

func TestInitializeAuditAuthorityCreatesValidatedClosedSet(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	agentLabel := "archive-agent"

	result, err := s.initializeAuditAuthority(t.Context(), target.ID, "api", &agentLabel)
	require.NoError(t, err)
	require.NoError(t, validateUUIDv4(result.scopeID))
	require.NoError(t, validateUUIDv4(result.operationID))
	assert.Len(t, result.baselineDigest, 64)
	assert.Equal(t, 3, result.memberCount,
		"the project scope adopts its live descendants and retained origin trash")

	counts, err := auditAuthorityCounts(t.Context(), s.db)
	require.NoError(t, err)
	assert.Equal(t, [5]int64{1, 1, 1, 3, 8}, counts)

	var raw string
	require.NoError(t, s.db.QueryRow(
		`SELECT record_json FROM audit_records WHERE kind='canonical_mutation'`,
	).Scan(&raw))
	mutation, err := audit.UnmarshalJSONRecord([]byte(raw))
	require.NoError(t, err)
	origin, err := auditTextField(mutation, auditOriginField)
	require.NoError(t, err)
	assert.Equal(t, "api", origin)
	label, err := auditField(mutation, "agent_label")
	require.NoError(t, err)
	labelText, ok := label.TextValue()
	require.True(t, ok)
	assert.Equal(t, agentLabel, labelText)

	_, err = s.initializeAuditAuthority(t.Context(), target.ID, "api", nil)
	require.ErrorContains(t, err, "already initialized")
}

func TestInitializeAuditAuthorityRollsBackPartialWrite(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_audit_membership
		BEFORE INSERT ON audit_memberships BEGIN
		SELECT RAISE(ABORT, 'forced enrollment failure'); END`)
	require.NoError(t, err)

	_, err = s.initializeAuditAuthority(t.Context(), target.ID, "cli", nil)
	require.ErrorContains(t, err, "forced enrollment failure")
	counts, err := auditAuthorityCounts(t.Context(), s.db)
	require.NoError(t, err)
	assert.Equal(t, [5]int64{}, counts)
}

func TestInitializeAuditAuthorityRejectsInvalidTargetsWithoutAuthority(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)

	for _, targetNodeID := range []int64{10, 11, 999} {
		_, err := s.initializeAuditAuthority(t.Context(), targetNodeID, "cli", nil)
		require.ErrorContains(t, err, "must be a live directory")
	}
	counts, err := auditAuthorityCounts(t.Context(), s.db)
	require.NoError(t, err)
	assert.Equal(t, [5]int64{}, counts)
}

func TestInitializeAuditAuthorityRejectsInvalidMetadataAndRollsBack(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE sqlite_sequence SET seq=5 WHERE name='nodes'`)
	require.NoError(t, err)

	_, err = s.initializeAuditAuthority(t.Context(), target.ID, "cli", nil)
	require.ErrorContains(t, err, "node ID high-water mark 5 is below maximum node ID 12")
	counts, err := auditAuthorityCounts(t.Context(), s.db)
	require.NoError(t, err)
	assert.Equal(t, [5]int64{}, counts)
}
