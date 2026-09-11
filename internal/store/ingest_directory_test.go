package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlannedIngestRejectsConcurrentLabelWithoutDirectoryAuthority(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	label := "Synthetic review"
	plan, err := s.PrepareIngestDirectory(ctx, "/new/inbox/sub")
	require.NoError(t, err)

	winner, err := s.BeginIngestWithLabel(ctx, "cli", "winner", &label)
	require.NoError(t, err)
	_, _, err = s.IngestFileWithMembership(
		ctx, winner, s.RootID(), "winner.txt", fakeHash("a1"), 1,
		"text/plain", "/synthetic/winner.txt", "",
	)
	require.NoError(t, err)
	beforeRoot, err := s.NodeByID(ctx, s.RootID())
	require.NoError(t, err)
	before := exportStoreMetadata(t, s)

	loser, err := s.BeginIngestWithLabel(ctx, "cli", "loser", &label)
	require.NoError(t, err)
	var resolution IngestDirectoryResolution
	_, _, resolution, err = s.IngestFileWithMembershipPlanned(
		ctx, loser, plan, "note.txt", fakeHash("b2"), 1,
		"text/plain", "/synthetic/sub/note.txt", "",
	)
	require.ErrorIs(t, err, ErrExists)
	assert.Zero(t, resolution)
	assert.True(t, IsInitialIngestAdmissionError(err))
	assert.Equal(t, before, exportStoreMetadata(t, s))
	afterRoot, err := s.NodeByID(ctx, s.RootID())
	require.NoError(t, err)
	assert.Equal(t, beforeRoot, afterRoot)
	_, err = s.NodeByPath(ctx, "/new")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPlannedIngestRejectsAuditActivatedAfterPreparation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	label := "Synthetic audited review"
	plan, err := s.PrepareIngestDirectory(ctx, "/new/inbox/sub")
	require.NoError(t, err)
	baseline, err := s.PreviewInitialAudit(ctx, s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, baseline)
	require.NoError(t, err)
	beforeRoot, err := s.NodeByID(ctx, s.RootID())
	require.NoError(t, err)
	before := exportStoreMetadata(t, s)

	run, err := s.BeginIngestWithLabel(ctx, "cli", "audited", &label)
	require.NoError(t, err)
	var resolution IngestDirectoryResolution
	_, _, resolution, err = s.IngestFileWithMembershipPlanned(
		ctx, run, plan, "note.txt", fakeHash("a1"), 1,
		"text/plain", "/synthetic/sub/note.txt", "",
	)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	assert.Zero(t, resolution)
	assert.True(t, IsInitialIngestAdmissionError(err))
	assert.Equal(t, before, exportStoreMetadata(t, s))
	afterRoot, err := s.NodeByID(ctx, s.RootID())
	require.NoError(t, err)
	assert.Equal(t, beforeRoot, afterRoot)
	_, err = s.NodeByPath(ctx, "/new")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPlannedIngestKeepsResolvedDirectoryIdentity(t *testing.T) {
	t.Run("rename", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		dest, err := s.Mkdir(ctx, s.RootID(), "inbox")
		require.NoError(t, err)
		child, err := s.Mkdir(ctx, dest.ID, "observed-child")
		require.NoError(t, err)
		plan, err := s.PrepareIngestDirectory(ctx, "/inbox/observed-child")
		require.NoError(t, err)
		moved, _, err := s.Move(ctx, child.ID, s.RootID(), "renamed", child.Revision)
		require.NoError(t, err)
		label := "Renamed destination"
		run, err := s.BeginIngestWithLabel(ctx, "cli", "rename", &label)
		require.NoError(t, err)
		node, _, _, err := s.IngestFileWithMembershipPlanned(
			ctx, run, plan, "note.txt", fakeHash("a1"), 1,
			"text/plain", "/synthetic/note.txt", "",
		)
		require.NoError(t, err)
		require.NotNil(t, node.ParentID)
		assert.Equal(t, moved.ID, *node.ParentID)
		_, err = s.NodeByPath(ctx, "/inbox/observed-child")
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.NodeByPath(ctx, "/renamed/note.txt")
		require.NoError(t, err)
	})

	t.Run("trash", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		dest, err := s.Mkdir(ctx, s.RootID(), "inbox")
		require.NoError(t, err)
		plan, err := s.PrepareIngestDirectory(ctx, "/inbox/sub")
		require.NoError(t, err)
		_, _, err = s.Trash(ctx, dest.ID, dest.Revision)
		require.NoError(t, err)
		before := exportStoreMetadata(t, s)
		label := "Trashed destination"
		run, err := s.BeginIngestWithLabel(ctx, "cli", "trash", &label)
		require.NoError(t, err)
		_, _, _, err = s.IngestFileWithMembershipPlanned(
			ctx, run, plan, "note.txt", fakeHash("a1"), 1,
			"text/plain", "/synthetic/note.txt", "",
		)
		require.ErrorIs(t, err, ErrNotFound)
		assert.Equal(t, before, exportStoreMetadata(t, s))
		_, err = s.NodeByPath(ctx, "/inbox")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestFinalizePlannedIngestDirectoriesLeavesNoZeroDocumentReceipt(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	label := "Empty synthetic tree"
	run, err := s.BeginIngestWithLabel(ctx, "cli", "empty", &label)
	require.NoError(t, err)
	plan, err := s.PrepareIngestDirectory(ctx, "/new/inbox/empty")
	require.NoError(t, err)

	_, err = s.FinalizeIngestDirectories(ctx, run, []IngestDirectoryPlan{plan})
	require.NoError(t, err)
	_, err = s.NodeByPath(ctx, "/new/inbox/empty")
	require.NoError(t, err)
	_, err = s.CollectionByID(ctx, run.ID())
	require.ErrorIs(t, err, ErrNotFound)
	var ingests, labels, provenance int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM ingests),
		(SELECT COUNT(*) FROM collection_labels),
		(SELECT COUNT(*) FROM provenance)`).Scan(&ingests, &labels, &provenance))
	assert.Zero(t, ingests)
	assert.Zero(t, labels)
	assert.Zero(t, provenance)
}

func TestFinalizePlannedIngestDirectoriesRejectsEmptyOnlyAdmission(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *Store, *string)
		want  error
	}{
		{
			name: "label collision",
			setup: func(t *testing.T, s *Store, label *string) {
				t.Helper()
				run, err := s.BeginIngestWithLabel(t.Context(), "cli", "winner", label)
				require.NoError(t, err)
				_, _, err = s.IngestFileWithMembership(
					t.Context(), run, s.RootID(), "winner.txt", fakeHash("a1"), 1,
					"text/plain", "/synthetic/winner.txt", "",
				)
				require.NoError(t, err)
			},
			want: ErrExists,
		},
		{
			name: "active audit",
			setup: func(t *testing.T, s *Store, _ *string) {
				t.Helper()
				plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
				require.NoError(t, err)
				_, err = s.EnableInitialAudit(t.Context(), plan)
				require.NoError(t, err)
			},
			want: ErrAuditMutationUnsupported,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			label := "Empty admission"
			plan, err := s.PrepareIngestDirectory(t.Context(), "/new/inbox/empty")
			require.NoError(t, err)
			test.setup(t, s, &label)
			before := exportStoreMetadata(t, s)
			run, err := s.BeginIngestWithLabel(t.Context(), "cli", "empty loser", &label)
			require.NoError(t, err)

			_, err = s.FinalizeIngestDirectories(
				t.Context(), run, []IngestDirectoryPlan{plan},
			)
			require.ErrorIs(t, err, test.want)
			assert.True(t, IsInitialIngestAdmissionError(err))
			assert.Equal(t, before, exportStoreMetadata(t, s))
			_, err = s.NodeByPath(t.Context(), "/new")
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

func TestPlannedExactConflictIsNotLabelAdmissionRejection(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	plan, err := s.PrepareIngestDirectory(ctx, "/new/inbox")
	require.NoError(t, err)
	parent, err := s.MkdirAll(ctx, "/new/inbox")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, parent.ID, "note.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	label := "Exact create"
	run, err := s.BeginIngestWithLabel(ctx, "cli", "exact", &label)
	require.NoError(t, err)

	_, _, err = s.IngestFileExactPlanned(
		ctx, run, plan, "note.txt", fakeHash("b2"), 1,
		"text/plain", "/synthetic/note.txt", "",
	)
	require.ErrorIs(t, err, ErrExists)
	assert.False(t, IsInitialIngestAdmissionError(err))
	_, err = s.NodeByPath(ctx, "/new/inbox/note (2).txt")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.CollectionByID(ctx, run.ID())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestIngestDirectoryResolutionKeepsSharedParentIdentity(t *testing.T) {
	for _, mutation := range []string{"move", "trash"} {
		t.Run(mutation, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			parentPlan, err := s.PrepareIngestDirectory(ctx, "/new/inbox/source")
			require.NoError(t, err)
			emptyPlan, err := s.ExtendIngestDirectory(ctx, parentPlan, "a-empty")
			require.NoError(t, err)
			siblingPlan, err := s.ExtendIngestDirectory(ctx, parentPlan, "sibling")
			require.NoError(t, err)
			label := "Common prefix " + mutation
			run, err := s.BeginIngestWithLabel(ctx, "cli", mutation, &label)
			require.NoError(t, err)
			_, _, resolution, err := s.IngestFileWithMembershipPlanned(
				ctx, run, parentPlan, "note.txt", fakeHash("a1"), 1,
				"text/plain", "/synthetic/note.txt", "",
			)
			require.NoError(t, err)
			resolvedParent := resolution.Rebase(parentPlan)
			parentID, ok := resolvedParent.ResolvedID()
			require.True(t, ok)
			emptyPlan = resolution.Rebase(emptyPlan)
			siblingPlan = resolution.Rebase(siblingPlan)
			parent, err := s.NodeByID(ctx, parentID)
			require.NoError(t, err)
			if mutation == "move" {
				_, _, err = s.Move(ctx, parent.ID, s.RootID(), "moved-source", parent.Revision)
			} else {
				_, _, err = s.Trash(ctx, parent.ID, parent.Revision)
			}
			require.NoError(t, err)
			beforeFinalize := exportStoreMetadata(t, s)

			_, err = s.FinalizeIngestDirectories(
				ctx, run, []IngestDirectoryPlan{emptyPlan, siblingPlan},
			)
			if mutation == "move" {
				require.NoError(t, err)
				_, err = s.NodeByPath(ctx, "/moved-source/a-empty")
				require.NoError(t, err)
				_, err = s.NodeByPath(ctx, "/moved-source/sibling")
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrNotFound)
				assert.Equal(t, beforeFinalize, exportStoreMetadata(t, s))
			}
			_, err = s.NodeByPath(ctx, "/new/inbox/source")
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

func exportStoreMetadata(t *testing.T, s *Store) string {
	t.Helper()
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	return exported.String()
}
