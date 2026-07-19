package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestAuditedRestoreRecordsSubtreeAndRoundTrips(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	trashed, _, err := s.Trash(t.Context(), work.ID, work.Revision)
	require.NoError(t, err)

	restored, restoredPath, err := s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	assert.Equal(t, "/Projects/Work", restoredPath)
	assert.Nil(t, restored.TrashedAt)
	assert.Equal(t, work.Revision+2, restored.Revision)
	restoredChild, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	assert.Equal(t, child.ID, restoredChild.ID)
	assert.Nil(t, restoredChild.TrashedAt)
	assert.Equal(t, child.Revision+2, restoredChild.Revision)
	updatedProjects, err := s.NodeByID(t.Context(), projects.ID)
	require.NoError(t, err)
	assert.Equal(t, projects.Revision+2, updatedProjects.Revision)
	require.NoError(t, s.ValidateMetadata(t.Context()))

	effects := auditPathEffectsForSequence(t, s, 3)
	require.Len(t, effects, 2)
	assertAuditPathState(t, effects[0], "old", "@trash/known/Projects/Work", "trash")
	assertAuditPathState(t, effects[0], "new", "/Projects/Work", "live")
	assertAuditPathState(
		t, effects[1], "old", "@trash/known/Projects/Work/child.txt", "trash",
	)
	assertAuditPathState(t, effects[1], "new", "/Projects/Work/child.txt", "live")

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	roundTrip, err := Open(filepath.Join(t.TempDir(), "round-trip.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, roundTrip.Close()) })
	require.NoError(t, roundTrip.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var restoredExport bytes.Buffer
	require.NoError(t, roundTrip.ExportMetadata(t.Context(), &restoredExport))
	assert.Equal(t, exported.Bytes(), restoredExport.Bytes())
	_, err = roundTrip.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
}

func TestAuditedRestoreUsesCanonicalConflictSuffix(t *testing.T) {
	s := newAuditedMoveStore(t)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	trashed, _, err := s.Trash(t.Context(), report.ID, report.Revision)
	require.NoError(t, err)
	replacement, err := s.CreateFile(
		t.Context(), projects.ID, "report.txt", fakeHash("cafe"), 18, "text/plain",
	)
	require.NoError(t, err)

	restored, _, err := s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	assert.Equal(t, "report (2).txt", restored.Name)
	stillReplacement, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	assert.Equal(t, replacement.ID, stillReplacement.ID)
	resolved, err := s.NodeByPath(t.Context(), "/Projects/report (2).txt")
	require.NoError(t, err)
	assert.Equal(t, report.ID, resolved.ID)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedRestoreAllowsUnchangedRetainedTrashOriginPath(t *testing.T) {
	s := newAuditedMoveStore(t)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), child.ID, child.Revision)
	require.NoError(t, err)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	trashedWork, _, err := s.Trash(t.Context(), work.ID, work.Revision)
	require.NoError(t, err)

	restored, _, err := s.Restore(t.Context(), trashedWork.ID, trashedWork.Revision)
	require.NoError(t, err)
	assert.Equal(t, "/Projects/Work", mustNodePath(t, s, restored.ID))
	stillTrashed, err := s.NodeByID(t.Context(), child.ID)
	require.NoError(t, err)
	assert.NotNil(t, stillTrashed.TrashedAt)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedRestoreRejectsConflictThatRetargetsTrashOrigin(t *testing.T) {
	s := newAuditedMoveStore(t)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), child.ID, child.Revision)
	require.NoError(t, err)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	trashedWork, _, err := s.Trash(t.Context(), work.ID, work.Revision)
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	_, err = s.Mkdir(t.Context(), projects.ID, "Work")
	require.NoError(t, err)

	restored, _, err := s.Restore(t.Context(), trashedWork.ID, trashedWork.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	require.ErrorContains(t, err, "retained trash-origin paths")
	assert.Zero(t, restored)
	stillTrashed, err := s.NodeByID(t.Context(), work.ID)
	require.NoError(t, err)
	assert.NotNil(t, stillTrashed.TrashedAt)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedRootScopeRestoreRecordsRootParentTouch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	trashed, _, err := s.Trash(t.Context(), empty.ID, empty.Revision)
	require.NoError(t, err)
	rootAfterTrash, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)

	restored, _, err := s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	assert.Equal(t, "/Empty", mustNodePath(t, s, restored.ID))
	rootAfterRestore, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)
	assert.Equal(t, rootAfterTrash.Revision+1, rootAfterRestore.Revision)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedRestoreRefusesFallbackFromTrashedOrigin(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	empty, err := s.Mkdir(t.Context(), s.RootID(), "Empty")
	require.NoError(t, err)
	file, err := s.CreateFile(
		t.Context(), empty.ID, "file.txt", fakeHash("fa11bac"), 8, "text/plain",
	)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	trashedFile, _, err := s.Trash(t.Context(), file.ID, file.Revision)
	require.NoError(t, err)
	empty, err = s.NodeByID(t.Context(), empty.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), empty.ID, empty.Revision)
	require.NoError(t, err)

	restored, _, err := s.Restore(t.Context(), trashedFile.ID, trashedFile.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	assert.Zero(t, restored)
	stillTrashed, err := s.NodeByID(t.Context(), file.ID)
	require.NoError(t, err)
	assert.NotNil(t, stillTrashed.TrashedAt)
}

func TestAuditedRestoreRollsBackTreeAndHistory(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	trashed, _, err := s.Trash(t.Context(), work.ID, work.Revision)
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_audit_restore_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audit restore failure'); END`)
	require.NoError(t, err)

	restored, _, err := s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.ErrorContains(t, err, "forced audit restore failure")
	assert.Zero(t, restored)
	stillTrashed, err := s.NodeByID(t.Context(), work.ID)
	require.NoError(t, err)
	assert.NotNil(t, stillTrashed.TrashedAt)
	_, err = s.NodeByPath(t.Context(), "/Projects/Work")
	require.ErrorIs(t, err, ErrNotFound)
	var sequence int64
	require.NoError(t, s.db.QueryRow(
		`SELECT operation_sequence_high_water FROM audit_authority`,
	).Scan(&sequence))
	assert.Equal(t, int64(2), sequence)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedRestoreReplayRejectsOmittedDescendant(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	trashed, _, err := s.Trash(t.Context(), work.ID, work.Revision)
	require.NoError(t, err)
	_, _, err = s.Restore(t.Context(), trashed.ID, trashed.Revision)
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
	allocations, err := auditRecordsBySequence(records["allocation_entry"], authority.sequence)
	require.NoError(t, err)
	entries, err := auditScopeRecordsByCount(records["scope_chain_entry"], scope)
	require.NoError(t, err)
	topologyRecords := auditRecordsByDigest(records[auditTopologyDeltaField])
	pathEffectRecords := auditRecordsByDigest(records["path_effect_list"])
	eventRecords, err := auditEventRecordsByID(records[auditEventField])
	require.NoError(t, err)
	usedTopology := map[string]bool{}
	usedPathEffects := map[string]bool{}
	usedEvents := map[string]bool{}
	require.NoError(t, replay.applyNodeTrash(
		s.VaultID(), mutations[2], allocations[2], entries[2],
		topologyRecords, pathEffectRecords, eventRecords,
		usedTopology, usedPathEffects, usedEvents,
	))

	mutation := mutations[3]
	digest, err := auditDigestField(mutation.record, auditTopologyDeltaField)
	require.NoError(t, err)
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
	_, _, err = replay.validateNodeRestoreTopology(
		mutation.record, mustAuditOperationID(t, mutation.record),
		topologyRecords, usedTopology,
	)
	require.ErrorContains(t, err, "complete trash subtree")
}

func TestAuditedRestoreReplayRejectsTrashSubtreeCycle(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), work.ID, work.Revision)
	require.NoError(t, err)
	topology, err := currentAuditTopology(t.Context(), s.db)
	require.NoError(t, err)
	index := make(map[uint64]int, len(topology))
	for i, node := range topology {
		nodeID := mustAuditUnsignedField(t, node, metadataNodeIDField)
		index[nodeID] = i
	}
	rootID, childID := uint64(work.ID), uint64(child.ID)
	topology[index[rootID]] = mustReplaceAuditRecordField(
		t, topology[index[rootID]], auditParentIDField, audit.Unsigned(childID),
	)
	replay := auditedHistoryReplay{topology: topology, topologyIndex: index}

	_, err = replay.trashedTopologySubtree(rootID)
	require.ErrorContains(t, err, "contains a cycle")
}

func auditPathEffectsForSequence(
	t *testing.T, s *Store, sequence int64,
) []audit.Record {
	t.Helper()
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	mutations, err := auditRecordsBySequence(records["canonical_mutation"], sequence)
	require.NoError(t, err)
	digest, err := auditDigestField(mutations[sequence].record, "path_effect_digest")
	require.NoError(t, err)
	lists := auditRecordsByDigest(records["path_effect_list"])
	effects, err := auditRecordListField(lists[digest].record, "effects")
	require.NoError(t, err)
	return effects
}

func mustNodePath(t *testing.T, s *Store, nodeID int64) string {
	t.Helper()
	path, err := s.Path(t.Context(), nodeID)
	require.NoError(t, err)
	return path
}
