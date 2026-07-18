package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestAuditedTrashRecordsSubtreeAndRoundTrips(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)

	trashed, priorPath, err := s.Trash(t.Context(), work.ID, work.Revision)
	require.NoError(t, err)
	assert.Equal(t, "/Projects/Work", priorPath)
	assert.NotNil(t, trashed.TrashedAt)
	assert.Equal(t, work.Revision+1, trashed.Revision)
	trashedChild, err := s.NodeByID(t.Context(), child.ID)
	require.NoError(t, err)
	assert.NotNil(t, trashedChild.TrashedAt)
	assert.Equal(t, child.Revision+1, trashedChild.Revision)
	updatedProjects, err := s.NodeByID(t.Context(), projects.ID)
	require.NoError(t, err)
	assert.Equal(t, projects.Revision+1, updatedProjects.Revision)
	require.NoError(t, s.ValidateMetadata(t.Context()))

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
	assertAuditPathState(t, effects[0], "old", "/Projects/Work", "live")
	assertAuditPathState(t, effects[0], "new", "@trash/known/Projects/Work", "trash")
	assertAuditPathState(t, effects[1], "old", "/Projects/Work/child.txt", "live")
	assertAuditPathState(
		t, effects[1], "new", "@trash/known/Projects/Work/child.txt", "trash",
	)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
	restoredTrash, err := restored.TrashedRoots(t.Context())
	require.NoError(t, err)
	var found bool
	for _, node := range restoredTrash {
		found = found || node.ID == work.ID
	}
	assert.True(t, found, "restored metadata should retain the audited trash root")
}

func TestAuditedTrashPathUsesSameTransaction(t *testing.T) {
	s := newAuditedMoveStore(t)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)

	trashed, priorPath, err := s.TrashPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	assert.Equal(t, report.ID, trashed.ID)
	assert.Equal(t, "/Projects/report.txt", priorPath)
	assert.NotNil(t, trashed.TrashedAt)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedRootScopeTrashRecordsRootParentTouch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	root, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())

	trashed, priorPath, err := s.Trash(t.Context(), empty.ID, empty.Revision)
	require.NoError(t, err)
	assert.Equal(t, "/Empty", priorPath)
	assert.NotNil(t, trashed.TrashedAt)
	updatedRoot, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)
	assert.Equal(t, root.Revision+1, updatedRoot.Revision)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedTrashRefusesScopeBoundaryWitnessChange(t *testing.T) {
	s := newAuditedMoveStore(t)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)

	trashed, _, err := s.Trash(t.Context(), projects.ID, projects.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	assert.Zero(t, trashed)
	unchanged, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	assert.Equal(t, projects, unchanged)
}

func TestAuditedTrashRollsBackTreeAndHistory(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_audit_trash_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audit trash failure'); END`)
	require.NoError(t, err)

	trashed, _, err := s.Trash(t.Context(), work.ID, work.Revision)
	require.ErrorContains(t, err, "forced audit trash failure")
	assert.Zero(t, trashed)
	unchanged, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	assert.Equal(t, work.ID, *unchanged.ParentID)
	var sequence, records int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&sequence, &records))
	assert.Equal(t, int64(1), sequence)
	assert.Equal(t, int64(8), records)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedTrashReplayRejectsOmittedDescendant(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), work.ID, work.Revision)
	require.NoError(t, err)

	authority, scope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, scope, records)
	require.NoError(t, err)
	replay, err := newAuditedHistoryReplay(authority, scope, initial)
	require.NoError(t, err)
	mutations, err := auditRecordsBySequence(records["canonical_mutation"], authority.sequence)
	require.NoError(t, err)
	mutation := mutations[2]
	digest, err := auditDigestField(mutation.record, auditTopologyDeltaField)
	require.NoError(t, err)
	topologyRecords := auditRecordsByDigest(records[auditTopologyDeltaField])
	delta := topologyRecords[digest]
	changes, err := auditRecordListField(delta.record, "changes")
	require.NoError(t, err)
	filtered := changes[:0]
	for _, change := range changes {
		if mustAuditUnsignedField(t, change, metadataNodeIDField) != uint64(child.ID) {
			filtered = append(filtered, change)
		}
	}
	delta.record = mustReplaceAuditRecordField(
		t, delta.record, "changes", audit.List(auditNestedValues(filtered)...),
	)
	topologyRecords[digest] = delta
	_, _, err = replay.validateNodeTrashTopology(
		mutation.record, mustAuditOperationID(t, mutation.record),
		topologyRecords, map[string]bool{},
	)
	require.ErrorContains(t, err, "complete live subtree")
}

func assertAuditPathState(
	t *testing.T, effect audit.Record, field, path, state string,
) {
	t.Helper()
	value, err := auditNestedField(effect, field)
	require.NoError(t, err)
	pathValue, err := auditField(value, "path")
	require.NoError(t, err)
	pathBytes, ok := pathValue.BytesValue()
	require.True(t, ok)
	assert.Equal(t, path, string(pathBytes))
	require.NoError(t, requireAuditText(value, "state", state))
}

func mustAuditOperationID(t *testing.T, record audit.Record) string {
	t.Helper()
	value, err := auditUUIDField(record, auditOperationIDField)
	require.NoError(t, err)
	return value
}
