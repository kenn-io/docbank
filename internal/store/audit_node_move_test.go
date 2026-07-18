package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestAuditedInScopeMoveRecordsDescendantPathsAndRoundTrips(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	archive, err := s.NodeByPath(t.Context(), "/Projects/Archive")
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)

	moved, err := s.Move(
		t.Context(), work.ID, archive.ID, "Renamed", work.Revision,
	)
	require.NoError(t, err)
	assert.Equal(t, archive.ID, *moved.ParentID)
	assert.Equal(t, "Renamed", moved.Name)
	assert.Equal(t, work.Revision+1, moved.Revision)
	_, err = s.NodeByPath(t.Context(), "/Projects/Work")
	require.ErrorIs(t, err, ErrNotFound)
	resolvedChild, err := s.NodeByPath(t.Context(), "/Projects/Archive/Renamed/child.txt")
	require.NoError(t, err)
	assert.Equal(t, child.ID, resolvedChild.ID)
	assert.Equal(t, child.Revision, resolvedChild.Revision)
	updatedProjects, err := s.NodeByID(t.Context(), projects.ID)
	require.NoError(t, err)
	updatedArchive, err := s.NodeByID(t.Context(), archive.ID)
	require.NoError(t, err)
	assert.Equal(t, projects.Revision+1, updatedProjects.Revision)
	assert.Equal(t, archive.Revision+1, updatedArchive.Revision)

	var sequence, records int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&sequence, &records))
	assert.Equal(t, int64(2), sequence)
	assert.Equal(t, int64(15), records)
	var raw []byte
	require.NoError(t, s.db.QueryRow(
		`SELECT record_json FROM audit_records WHERE kind='path_effect_list'`,
	).Scan(&raw))
	effectList, err := audit.UnmarshalJSONRecord(raw)
	require.NoError(t, err)
	effects, err := auditRecordListField(effectList, "effects")
	require.NoError(t, err)
	require.Len(t, effects, 2)
	assert.Equal(t, uint64(work.ID), mustAuditUnsignedField(t, effects[0], "member_node_id"))
	assert.Equal(t, uint64(child.ID), mustAuditUnsignedField(t, effects[1], "member_node_id"))

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
	_, err = restored.NodeByPath(t.Context(), "/Projects/Archive/Renamed/child.txt")
	require.NoError(t, err)
}

func TestAuditedInScopeRenameRecordsOnePathEffect(t *testing.T) {
	s := newAuditedMoveStore(t)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	parent, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)

	renamed, err := s.Move(t.Context(), file.ID, parent.ID, "renamed.txt", file.Revision)
	require.NoError(t, err)
	assert.Equal(t, "renamed.txt", renamed.Name)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	var effects int64
	require.NoError(t, s.db.QueryRow(`SELECT json_array_length(
		json_extract(record_json, '$.fields.effects'))
		FROM audit_records WHERE kind='path_effect_list'`).Scan(&effects))
	assert.Equal(t, int64(1), effects)
}

func TestAuditedRootScopeRenameHandlesRootTimestampTouch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())

	renamed, err := s.Move(
		t.Context(), projects.ID, s.RootID(), "RenamedProjects", projects.Revision,
	)
	require.NoError(t, err)
	assert.Equal(t, "RenamedProjects", renamed.Name)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	_, err = s.NodeByPath(t.Context(), "/RenamedProjects/report.txt")
	require.NoError(t, err)
}

func TestAuditedMoveRefusesMembershipAndWitnessBoundaryCrossings(t *testing.T) {
	s := newAuditedMoveStore(t)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)

	_, err = s.Move(t.Context(), report.ID, empty.ID, report.Name, report.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	unchanged, err := s.NodeByID(t.Context(), report.ID)
	require.NoError(t, err)
	assert.Equal(t, report, unchanged)

	outside, err := s.NodeByPath(t.Context(), "/Empty/outside.txt")
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	movedOutside, err := s.Move(
		t.Context(), outside.ID, projects.ID, outside.Name, outside.Revision,
	)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	assert.Zero(t, movedOutside)
}

func TestAuditedMoveRollsBackTreeAndHistory(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	archive, err := s.NodeByPath(t.Context(), "/Projects/Archive")
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_audit_move_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audit move failure'); END`)
	require.NoError(t, err)

	moved, err := s.Move(t.Context(), work.ID, archive.ID, "failed", work.Revision)
	require.ErrorContains(t, err, "forced audit move failure")
	assert.Zero(t, moved)
	unchanged, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	assert.Equal(t, work.ID, *unchanged.ParentID)
	var sequence, records int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&sequence, &records))
	assert.Equal(t, int64(1), sequence)
	assert.Equal(t, int64(8), records)
}

func newAuditedMoveStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	archive, err := s.Mkdir(t.Context(), projects.ID, "Archive")
	require.NoError(t, err)
	assert.NotZero(t, archive.ID)
	work, err := s.Mkdir(t.Context(), projects.ID, "Work")
	require.NoError(t, err)
	_, err = s.CreateFile(
		t.Context(), work.ID, "child.txt", fakeHash("a722"), 8, "text/plain",
	)
	require.NoError(t, err)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	_, err = s.CreateFile(
		t.Context(), empty.ID, "outside.txt", fakeHash("a721"), 7, "text/plain",
	)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, projects.ID)
	return s
}

func mustAuditUnsignedField(t *testing.T, record audit.Record, name string) uint64 {
	t.Helper()
	value, err := auditUnsignedField(record, name)
	require.NoError(t, err)
	return value
}
