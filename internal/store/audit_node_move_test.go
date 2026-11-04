package store

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestAuditedInScopeMoveRecordsDescendantPathsAndRoundTrips(t *testing.T) {
	t.Parallel()
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	archive, err := s.NodeByPath(t.Context(), "/Projects/Archive")
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)

	moved, movedPath, err := s.Move(
		t.Context(), work.ID, archive.ID, "Renamed", work.Revision,
	)
	require.NoError(t, err)
	assert.Equal(t, "/Projects/Archive/Renamed", movedPath)
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
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&sequence, &records))
	assert.Equal(t, int64(2), sequence)
	assert.Equal(t, int64(15), records)
	var raw []byte
	require.NoError(t, s.db.QueryRowContext(t.Context(),
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
	restored, err := Open(t.Context(), filepath.Join(t.TempDir(), "restored.db"))
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
	t.Parallel()
	s := newAuditedMoveStore(t)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	parent, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)

	renamed, _, err := s.Move(t.Context(), file.ID, parent.ID, "renamed.txt", file.Revision)
	require.NoError(t, err)
	assert.Equal(t, "renamed.txt", renamed.Name)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	var effects int64
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT json_array_length(
		json_extract(record_json, '$.fields.effects'))
		FROM audit_records WHERE kind='path_effect_list'`).Scan(&effects))
	assert.Equal(t, int64(1), effects)
}

func TestAuditedBatchMoveRecordsOneAtomicFinalTopology(t *testing.T) {
	t.Parallel()
	s := newAuditedMoveStore(t)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)

	results, err := s.BatchMove(t.Context(), []BatchMoveRequest{
		{NodeID: report.ID, IfRevision: report.Revision, DestinationPath: "/Projects/Work/child.txt"},
		{NodeID: child.ID, IfRevision: child.Revision, DestinationPath: "/Projects/report.txt"},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "/Projects/Work/child.txt", results[0].Path)
	assert.Equal(t, "/Projects/report.txt", results[1].Path)
	require.NoError(t, s.ValidateMetadata(t.Context()))

	var sequence, moveMutations int64
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records WHERE kind='canonical_mutation')`,
	).Scan(&sequence, &moveMutations))
	assert.Equal(t, int64(2), sequence)
	assert.Equal(t, int64(2), moveMutations, "the complete batch must add one canonical mutation")

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := Open(t.Context(), filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	resolvedReport, err := restored.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	assert.Equal(t, report.ID, resolvedReport.ID)
	resolvedChild, err := restored.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	assert.Equal(t, child.ID, resolvedChild.ID)
}

func TestAuditedRootScopeRenameHandlesRootTimestampTouch(t *testing.T) {
	t.Parallel()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())

	renamed, _, err := s.Move(
		t.Context(), empty.ID, s.RootID(), "RenamedEmpty", empty.Revision,
	)
	require.NoError(t, err)
	assert.Equal(t, "RenamedEmpty", renamed.Name)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	_, err = s.NodeByPath(t.Context(), "/RenamedEmpty")
	require.NoError(t, err)
}

func TestAuditedMoveRejectsRetainedTrashOriginPathChange(t *testing.T) {
	t.Parallel()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	projects, err := s.Mkdir(t.Context(), s.RootID(), "Projects")
	require.NoError(t, err)
	file, err := s.CreateFile(
		t.Context(), projects.ID, "old.txt", fakeHash("a723"), 9, "text/plain",
	)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), file.ID, file.Revision)
	require.NoError(t, err)
	projects, err = s.NodeByID(t.Context(), projects.ID)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())

	_, _, err = s.Move(
		t.Context(), projects.ID, s.RootID(), "RenamedProjects", projects.Revision,
	)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	require.ErrorContains(t, err, "trash-origin paths")
	_, err = s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	var sequence int64
	require.NoError(t, s.db.QueryRowContext(t.Context(),
		`SELECT operation_sequence_high_water FROM audit_authority`,
	).Scan(&sequence))
	assert.Equal(t, int64(1), sequence)
}

func TestReplayedAuditTopologyRejectsImpossibleIntermediateState(t *testing.T) {
	t.Parallel()
	s := newAuditedMoveStore(t)
	topology, err := currentAuditTopology(t.Context(), s.db)
	require.NoError(t, err)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)

	tests := map[string]struct {
		name    string
		message string
	}{
		"invalid canonical name": {name: "..", message: "invalid canonical name"},
		"live sibling collision": {name: "Archive", message: "same parent and name"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := append([]audit.Record(nil), topology...)
			for index, record := range candidate {
				if mustAuditUnsignedField(t, record, metadataNodeIDField) != uint64(report.ID) {
					continue
				}
				candidate[index] = mustReplaceAuditRecordField(
					t, record, "name", audit.Bytes([]byte(test.name)),
				)
			}
			err := validateReplayedAuditTopology(candidate)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func TestAuditedMoveRefusesMembershipAndWitnessBoundaryCrossings(t *testing.T) {
	t.Parallel()
	s := newAuditedMoveStore(t)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)

	_, _, err = s.Move(t.Context(), report.ID, empty.ID, report.Name, report.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	unchanged, err := s.NodeByID(t.Context(), report.ID)
	require.NoError(t, err)
	assert.Equal(t, report, unchanged)

	outside, err := s.NodeByPath(t.Context(), "/Empty/outside.txt")
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	movedOutside, _, err := s.Move(
		t.Context(), outside.ID, projects.ID, outside.Name, outside.Revision,
	)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	assert.Zero(t, movedOutside)
}

func TestAuditedMoveRollsBackTreeAndHistory(t *testing.T) {
	t.Parallel()
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	archive, err := s.NodeByPath(t.Context(), "/Projects/Archive")
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `CREATE TRIGGER reject_audit_move_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audit move failure'); END`)
	require.NoError(t, err)

	moved, _, err := s.Move(t.Context(), work.ID, archive.ID, "failed", work.Revision)
	require.ErrorContains(t, err, "forced audit move failure")
	assert.Zero(t, moved)
	unchanged, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
	require.NoError(t, err)
	assert.Equal(t, work.ID, *unchanged.ParentID)
	var sequence, records int64
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&sequence, &records))
	assert.Equal(t, int64(1), sequence)
	assert.Equal(t, int64(8), records)
}

func newAuditedMoveStore(t *testing.T) *Store {
	t.Helper()
	return newAuditedMoveStoreAt(t, "/Projects")
}

func newAuditedMoveStoreAt(t *testing.T, scopePath string) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "vault.db"))
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
	scope, err := s.NodeByPath(t.Context(), scopePath)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	return s
}

func mustAuditUnsignedField(t *testing.T, record audit.Record, name string) uint64 {
	t.Helper()
	value, err := auditUnsignedField(record, name)
	require.NoError(t, err)
	return value
}

func TestAuditedBatchMoveRecordsDescendantPaths(t *testing.T) {
	t.Parallel()
	for _, moveChild := range []bool{false, true} {
		t.Run(fmt.Sprintf("moveChild=%t", moveChild), func(t *testing.T) {
			s := newAuditedMoveStore(t)
			work, err := s.NodeByPath(t.Context(), "/Projects/Work")
			require.NoError(t, err)
			child, err := s.NodeByPath(t.Context(), "/Projects/Work/child.txt")
			require.NoError(t, err)
			requests := []BatchMoveRequest{{SourcePath: "/Projects/Work", DestinationPath: "/Projects/Archive/Renamed"}}
			childPath := "/Projects/Archive/Renamed/child.txt"
			if moveChild {
				childPath = "/Projects/Archive/child.txt"
				requests = append(requests, BatchMoveRequest{SourcePath: "/Projects/Work/child.txt", DestinationPath: childPath})
			}
			_, err = s.BatchMove(t.Context(), requests)
			require.NoError(t, err)
			resolved, err := s.NodeByPath(t.Context(), childPath)
			require.NoError(t, err)
			assert.Equal(t, child.ID, resolved.ID)
			var raw []byte
			require.NoError(t, s.db.QueryRowContext(t.Context(),
				`SELECT record_json FROM audit_records WHERE kind='path_effect_list'`).Scan(&raw))
			effectList, err := audit.UnmarshalJSONRecord(raw)
			require.NoError(t, err)
			effects, err := auditRecordListField(effectList, "effects")
			require.NoError(t, err)
			require.Len(t, effects, 2)
			assert.Equal(t, uint64(work.ID), mustAuditUnsignedField(t, effects[0], "member_node_id"))
			assert.Equal(t, uint64(child.ID), mustAuditUnsignedField(t, effects[1], "member_node_id"))
			oldPath, err := auditNestedField(effects[1], "old")
			require.NoError(t, err)
			newPath, err := auditNestedField(effects[1], "new")
			require.NoError(t, err)
			oldBytes, ok := auditFieldBytes(oldPath, auditPathField)
			require.True(t, ok)
			assert.Equal(t, []byte("/Projects/Work/child.txt"), oldBytes)
			newBytes, ok := auditFieldBytes(newPath, auditPathField)
			require.True(t, ok)
			assert.Equal(t, []byte(childPath), newBytes)
			require.NoError(t, s.ValidateMetadata(t.Context()))
		})
	}
}

func TestAuditedBatchMoveRejectsLaterItemAboveTrashOrigin(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.Mkdir(ctx, s.RootID(), "first")
	require.NoError(t, err)
	second, err := s.Mkdir(ctx, s.RootID(), "second")
	require.NoError(t, err)
	file, err := s.CreateFile(ctx, second.ID, "old.txt", fakeHash("a724"), 3, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, file.ID, file.Revision)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())

	_, err = s.BatchMove(ctx, []BatchMoveRequest{
		{SourcePath: "/first", DestinationPath: "/renamed-first"},
		{SourcePath: "/second", DestinationPath: "/renamed-second"},
	})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	require.ErrorContains(t, err, fmt.Sprintf("moving node %d would change retained trash-origin paths", second.ID))
	var sequence int64
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT operation_sequence_high_water FROM audit_authority`).Scan(&sequence))
	assert.Equal(t, int64(1), sequence)
	unchanged, err := s.NodeByPath(ctx, "/first")
	require.NoError(t, err)
	assert.Equal(t, first.ID, unchanged.ID)
	unchanged, err = s.NodeByPath(ctx, "/second")
	require.NoError(t, err)
	assert.Equal(t, second.ID, unchanged.ID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestAuditTrashOriginTopologyMatchesFullReplay(t *testing.T) {
	t.Parallel()
	for _, rootScope := range []bool{false, true} {
		for _, state := range []string{"no trash", "known origin", "nested trash", "inherited bridge", "inherited trash", "unknown ancestor", "unknown origin", "outside scope"} {
			t.Run(fmt.Sprintf("root=%t/%s", rootScope, state), func(t *testing.T) {
				scopePath := "/Projects"
				if rootScope {
					scopePath = "/"
				}
				s := newAuditedMoveStoreAt(t, scopePath)
				ctx := t.Context()
				projects, err := s.NodeByPath(ctx, "/Projects")
				require.NoError(t, err)
				var oldFileID int64
				require.NoError(t, s.db.QueryRowContext(ctx,
					`SELECT id FROM nodes WHERE trash_parent=$1 AND trash_name='old.bin'`, projects.ID,
				).Scan(&oldFileID))
				_, _, err = s.Restore(ctx, oldFileID, UnconditionalRev)
				require.NoError(t, err)
				work, err := s.NodeByPath(ctx, "/Projects/Work")
				require.NoError(t, err)
				child, err := s.NodeByPath(ctx, "/Projects/Work/child.txt")
				require.NoError(t, err)
				empty, err := s.NodeByPath(ctx, "/Empty")
				require.NoError(t, err)
				var innerID int64
				if state == "inherited bridge" {
					inner, err := s.Mkdir(ctx, work.ID, "Inner")
					require.NoError(t, err)
					innerID = inner.ID
					_, _, err = s.Move(ctx, child.ID, inner.ID, child.Name, UnconditionalRev)
					require.NoError(t, err)
				}
				if state != "no trash" && state != "outside scope" && state != "inherited trash" {
					_, _, err = s.Trash(ctx, child.ID, UnconditionalRev)
					require.NoError(t, err)
				}
				if state == "nested trash" || state == "inherited bridge" || state == "inherited trash" || state == "unknown ancestor" {
					_, _, err = s.Trash(ctx, work.ID, UnconditionalRev)
					require.NoError(t, err)
				}

				tx, err := s.db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer func() { require.NoError(t, tx.Rollback()) }()
				switch state {
				case "outside scope":
					outside, lookupErr := s.nodeByPath(ctx, tx, "/Empty/outside.txt")
					require.NoError(t, lookupErr)
					err = s.trashNodeTx(ctx, tx, outside, nowRFC3339())
				case "unknown origin":
					_, err = tx.ExecContext(ctx, `UPDATE nodes SET trash_parent=NULL WHERE id=$1`, child.ID)
				case "unknown ancestor":
					_, err = tx.ExecContext(ctx, `UPDATE nodes SET trash_parent=NULL WHERE id=$1`, work.ID)
				}
				require.NoError(t, err)
				full, err := currentAuditTopology(ctx, tx)
				require.NoError(t, err)
				members, err := auditScopeMemberSetTx(ctx, tx, testAuditScopeID)
				require.NoError(t, err)
				for _, record := range full {
					id := mustAuditUnsignedField(t, record, metadataNodeIDField)
					oldAffected, oldErr := auditMoveAffectsTrashOrigin(full, members, map[uint64]bool{id: true})
					answer, err := auditMoveAffectsTrashOriginTx(ctx, tx, testAuditScopeID, int64(id))
					assert.Equal(t, oldAffected != 0, answer, "node %d", id)
					require.NoError(t, oldErr, "node %d", id)
					require.NoError(t, err)
					want := false
					switch state {
					case "known origin", "nested trash", "inherited bridge":
						want = id == uint64(work.ID) || id == uint64(projects.ID) || id == uint64(s.RootID()) || id == uint64(innerID)
					case "inherited trash":
						want = id == uint64(projects.ID) || id == uint64(s.RootID())
					case "unknown ancestor":
						want = id == uint64(work.ID)
					case "outside scope":
						want = rootScope && (id == uint64(empty.ID) || id == uint64(s.RootID()))
					}
					assert.Equal(t, want, answer, "node %d", id)
				}
			})
		}
	}
}

func checkAuditTrashOriginMove(t *testing.T, openStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := openStore(t)
	ctx := t.Context()
	projects, err := s.Mkdir(ctx, s.RootID(), "Projects")
	require.NoError(t, err)
	work, err := s.Mkdir(ctx, projects.ID, "Work")
	require.NoError(t, err)
	file, err := s.CreateFile(ctx, work.ID, "child.txt", fakeHash("a1"), 8, "text/plain")
	require.NoError(t, err)
	sibling, err := s.CreateFile(ctx, s.RootID(), "sibling.txt", fakeHash("b1"), 8, "text/plain")
	require.NoError(t, err)
	unrelated, err := s.Mkdir(ctx, s.RootID(), "Unrelated")
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "cli", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, file.ID, UnconditionalRev)
	require.NoError(t, err)
	_, _, err = s.Move(ctx, projects.ID, s.RootID(), "Renamed", UnconditionalRev)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	require.ErrorContains(t, err, "trash-origin paths")
	unchanged, err := s.NodeByPath(ctx, "/Projects/Work")
	require.NoError(t, err)
	assert.Equal(t, work.ID, unchanged.ID)
	projects, err = s.NodeByPath(ctx, "/Projects")
	require.NoError(t, err)
	var before, after bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &before))
	_, err = s.BatchMove(ctx, []BatchMoveRequest{
		{NodeID: projects.ID, IfRevision: projects.Revision, DestinationPath: "/Renamed"},
	})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	require.ErrorContains(t, err, "trash-origin paths")
	require.NoError(t, s.ExportMetadata(ctx, &after))
	assert.Equal(t, before.String(), after.String(), "rejected batch move preserves tree, trash, and audit history")
	results, err := s.BatchMove(ctx, []BatchMoveRequest{
		{NodeID: unrelated.ID, IfRevision: unrelated.Revision, DestinationPath: "/Moved"},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "/Moved", results[0].Path)
	movedFolder, err := s.NodeByPath(ctx, "/Moved")
	require.NoError(t, err)
	assert.Equal(t, unrelated.ID, movedFolder.ID)
	moved, path, err := s.Move(ctx, sibling.ID, s.RootID(), "renamed.txt", UnconditionalRev)
	require.NoError(t, err)
	assert.Equal(t, sibling.ID, moved.ID)
	assert.Equal(t, "/renamed.txt", path)
}
