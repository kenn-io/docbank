package store

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditedNodeCreationInheritsMembershipAndRoundTrips(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)

	directory, err := s.Mkdir(t.Context(), scope.ID, "2026")
	require.NoError(t, err)
	file, err := s.CreateFile(
		t.Context(), directory.ID, "return.txt", fakeHash("ac1"), 31, "text/plain",
	)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(t.Context()))

	var sequence, allocationCount, scopeCount, baselineCount int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT allocation_entry_count FROM audit_authority),
		(SELECT entry_count FROM audit_scopes),
		(SELECT COUNT(*) FROM audit_baselines)`).Scan(
		&sequence, &allocationCount, &scopeCount, &baselineCount,
	))
	assert.Equal(t, int64(3), sequence)
	assert.Equal(t, sequence, allocationCount)
	assert.Equal(t, sequence, scopeCount)
	assert.Equal(t, int64(3), baselineCount)

	rows, err := s.db.Query(`SELECT node_id FROM audit_memberships
		WHERE node_id IN (?,?) ORDER BY node_id`, directory.ID, file.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var inherited []int64
	for rows.Next() {
		var nodeID int64
		require.NoError(t, rows.Scan(&nodeID))
		inherited = append(inherited, nodeID)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []int64{directory.ID, file.ID}, inherited)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
	restoredFile, err := restored.NodeByPath(t.Context(), "/Projects/2026/return.txt")
	require.NoError(t, err)
	assert.Equal(t, file.ID, restoredFile.ID)
	assert.Equal(t, file.CurrentVersionID, restoredFile.CurrentVersionID)
}

func TestAuditedNodeCreationImportRejectsMembershipRetarget(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	created, err := s.Mkdir(t.Context(), scope.ID, "retargeted")
	require.NoError(t, err)
	var initialBaseline string
	require.NoError(t, s.db.QueryRow(
		`SELECT digest FROM audit_baselines WHERE operation_id=?`, testAuditOperationID,
	).Scan(&initialBaseline))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := retargetAuditMembership(
		t, exported.Bytes(), uint64(created.ID), initialBaseline,
	)

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(malformed))
	require.ErrorContains(t, err, "membership")
	var auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&auditRows))
	assert.Zero(t, auditRows)
}

func TestAuditedNodeCreationRollsBackWholeOperation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	_, err = s.db.Exec(`CREATE TRIGGER reject_creation_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audited creation failure'); END`)
	require.NoError(t, err)

	_, err = s.CreateFile(
		t.Context(), scope.ID, "rollback.txt", fakeHash("ac2"), 19, "text/plain",
	)
	require.ErrorContains(t, err, "forced audited creation failure")
	_, err = s.NodeByPath(t.Context(), "/Projects/rollback.txt")
	require.ErrorIs(t, err, ErrNotFound)
	var blobRows, baselineRows, recordRows int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM blobs WHERE hash=?),
		(SELECT COUNT(*) FROM audit_baselines),
		(SELECT COUNT(*) FROM audit_records)`, fakeHash("ac2")).Scan(
		&blobRows, &baselineRows, &recordRows,
	))
	assert.Zero(t, blobRows)
	assert.Equal(t, int64(1), baselineRows)
	assert.Equal(t, int64(8), recordRows)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func retargetAuditMembership(
	t *testing.T, input []byte, nodeID uint64, baselineDigest string,
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var membership metadataAuditMembership
		if err := json.Unmarshal(line, &membership); err != nil ||
			membership.Type != metadataAuditMembershipType || membership.NodeID != nodeID {
			continue
		}
		membership.BaselineDigest = baselineDigest
		var err error
		lines[index], err = json.Marshal(membership)
		require.NoError(t, err)
		return append(bytes.Join(lines, []byte{'\n'}), '\n')
	}
	require.FailNow(t, "audit membership not found", nodeID)
	return nil
}
