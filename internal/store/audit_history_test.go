package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditHistoryPagesCanonicalNodeEvents(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	taxes, err := s.Mkdir(t.Context(), s.RootID(), "Taxes")
	require.NoError(t, err)
	outside, err := s.Mkdir(t.Context(), s.RootID(), "Outside")
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), taxes.ID, "cli", nil)
	require.NoError(t, err)
	status, err := s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	require.Len(t, status.Scopes, 1)
	enrollment, err := s.AuditHistory(t.Context(), taxes.ID, 10, "")
	require.NoError(t, err)
	require.Len(t, enrollment.Items, 1)
	assert.Equal(t, "audit_enroll", enrollment.Items[0].Kind)

	destination, err := s.Mkdir(t.Context(), taxes.ID, "2027")
	require.NoError(t, err)
	file, err := s.CreateFile(
		t.Context(), taxes.ID, "return.txt", fakeHash("a710"), 7, "text/plain",
	)
	require.NoError(t, err)
	moved, err := s.Move(t.Context(), file.ID, destination.ID, file.Name, file.Revision)
	require.NoError(t, err)

	first, err := s.AuditHistory(t.Context(), file.ID, 2, "")
	require.NoError(t, err)
	assert.Equal(t, file.ID, first.Node.ID)
	assert.Equal(t, "/Taxes/2027/return.txt", first.Path)
	assert.Equal(t, 4, first.Total)
	require.Len(t, first.Items, 2)
	assert.Equal(t, "node_path", first.Items[0].Kind)
	require.NotNil(t, first.Items[0].OldPath)
	require.NotNil(t, first.Items[0].NewPath)
	assert.Equal(t, "/Taxes/return.txt", first.Items[0].OldPath.Path)
	assert.Equal(t, "live", first.Items[0].OldPath.State)
	assert.Equal(t, "/Taxes/2027/return.txt", first.Items[0].NewPath.Path)
	assert.Equal(t, "live", first.Items[0].NewPath.State)
	assert.Equal(t, status.Scopes[0].ID, first.Items[0].ScopeID)
	assert.Equal(t, moved.Revision, first.Items[0].ResultingNodeRevision)
	assert.Equal(t, "node_create", first.Items[1].Kind)
	require.NotEmpty(t, first.NextCursor)
	require.NoError(t, ValidateAuditHistoryCursor(first.NextCursor, file.ID))

	second, err := s.AuditHistory(t.Context(), file.ID, 2, first.NextCursor)
	require.NoError(t, err)
	assert.Equal(t, first.Total, second.Total)
	require.Len(t, second.Items, 2)
	assert.Equal(t, "content_create", second.Items[0].Kind)
	assert.Equal(t, "audit_inherit", second.Items[1].Kind)
	assert.Empty(t, second.NextCursor)

	byPath, err := s.AuditHistoryPath(
		t.Context(), "/Taxes/2027/return.txt", 10, "",
	)
	require.NoError(t, err)
	assert.Equal(t, first.Total, byPath.Total)
	assert.Equal(t, file.ID, byPath.Node.ID)

	_, err = s.AuditHistory(t.Context(), outside.ID, 10, "")
	require.ErrorIs(t, err, ErrAuditNotEnrolled)
	_, err = s.AuditHistory(t.Context(), file.ID, 10,
		encodeAuditHistoryCursor(auditHistoryCursor{
			nodeID: outside.ID, sequence: 1, ordinal: 0,
			eventID: fakeHash("a711"),
		}))
	require.ErrorIs(t, err, ErrInvalidAuditCursor)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditHistoryCursorRemainsStableWhenNewEventsArrive(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	taxes, err := s.Mkdir(t.Context(), s.RootID(), "Taxes")
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), taxes.ID, "cli", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	file, err := s.CreateFile(
		t.Context(), taxes.ID, "return.txt", fakeHash("a712"), 6, "text/plain",
	)
	require.NoError(t, err)

	first, err := s.AuditHistory(t.Context(), file.ID, 1, "")
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	require.NotEmpty(t, first.NextCursor)
	firstID := first.Items[0].ID
	_, err = s.Move(t.Context(), file.ID, taxes.ID, "renamed.txt", file.Revision)
	require.NoError(t, err)

	second, err := s.AuditHistory(t.Context(), file.ID, 10, first.NextCursor)
	require.NoError(t, err)
	require.NotEmpty(t, second.Items)
	for _, event := range second.Items {
		assert.NotEqual(t, firstID, event.ID)
		assert.LessOrEqual(t, event.OperationSequence, first.Items[0].OperationSequence)
	}
}

func TestAuditHistoryPreservesTrashAndRestoreCoordinates(t *testing.T) {
	s := newAuditedMoveStore(t)
	work, err := s.NodeByPath(t.Context(), "/Projects/Work")
	require.NoError(t, err)
	trashed, _, err := s.Trash(t.Context(), work.ID, work.Revision)
	require.NoError(t, err)

	page, err := s.AuditHistory(t.Context(), work.ID, 10, "")
	require.NoError(t, err)
	trashedPath := findAuditHistoryEvent(t, page.Items, "node_path")
	require.NotNil(t, trashedPath.OldPath)
	require.NotNil(t, trashedPath.NewPath)
	assert.Equal(t, AuditPathState{Path: "/Projects/Work", State: "live"}, *trashedPath.OldPath)
	assert.Equal(t, AuditPathState{
		Path: "@trash/known/Projects/Work", State: "trash",
	}, *trashedPath.NewPath)

	_, err = s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	page, err = s.AuditHistory(t.Context(), work.ID, 10, "")
	require.NoError(t, err)
	require.NotNil(t, page.Items[0].OldPath)
	require.NotNil(t, page.Items[0].NewPath)
	assert.Equal(t, "@trash/known/Projects/Work", page.Items[0].OldPath.Path)
	assert.Equal(t, "trash", page.Items[0].OldPath.State)
	assert.Equal(t, "/Projects/Work", page.Items[0].NewPath.Path)
	assert.Equal(t, "live", page.Items[0].NewPath.State)
}

func TestAuditHistoryProjectsTagAndProvenanceChanges(t *testing.T) {
	t.Run("tag assignment and rename", func(t *testing.T) {
		s, tag, report := newAuditedTagStore(t)
		assigned, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
		require.NoError(t, err)
		_, err = s.RenameTag(t.Context(), tag.ID, assigned.Tag.Revision, "needs review")
		require.NoError(t, err)

		page, err := s.AuditHistory(t.Context(), report.ID, 10, "")
		require.NoError(t, err)
		rename := findAuditHistoryEvent(t, page.Items, "tag_rename")
		require.NotNil(t, rename.Attachment)
		assert.Equal(t, "tag_definition", rename.Attachment.Kind)
		assert.Equal(t, tag.ID, rename.Attachment.Identity.TagID)
		require.NotNil(t, rename.Attachment.Before)
		require.NotNil(t, rename.Attachment.After)
		assert.Equal(t, tag.Name, rename.Attachment.Before.TagName)
		assert.Equal(t, "needs review", rename.Attachment.After.TagName)

		assignment := findAuditHistoryEvent(t, page.Items, "tag_assign")
		require.NotNil(t, assignment.Attachment)
		assert.Equal(t, tag.ID, assignment.Attachment.Identity.TagID)
		assert.Equal(t, report.ID, assignment.Attachment.Identity.NodeID)
		assert.Nil(t, assignment.Attachment.Before)
		require.NotNil(t, assignment.Attachment.After)
		assert.Equal(t, report.ID, assignment.Attachment.After.NodeID)
	})

	t.Run("filesystem provenance", func(t *testing.T) {
		s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		seedMetadataRoundTrip(t, s)
		scope, err := s.NodeByPath(t.Context(), "/Projects")
		require.NoError(t, err)
		seedInitialAuditAuthority(t, s, scope.ID)
		run, err := s.BeginIngest(t.Context(), "cli", "/source/reports")
		require.NoError(t, err)
		file, added, err := s.IngestFile(
			t.Context(), run, scope.ID, "history.txt", fakeHash("a713"), 7,
			"text/plain", "/source/reports/history.txt", "2026-07-18T12:00:00Z",
		)
		require.NoError(t, err)
		require.True(t, added)

		page, err := s.AuditHistory(t.Context(), file.ID, 10, "")
		require.NoError(t, err)
		provenance := findAuditHistoryEvent(t, page.Items, "provenance_add")
		require.NotNil(t, provenance.Attachment)
		assert.Equal(t, "provenance", provenance.Attachment.Kind)
		require.NotEmpty(t, provenance.Attachment.Identity.ProvenanceID)
		assert.Nil(t, provenance.Attachment.Before)
		require.NotNil(t, provenance.Attachment.After)
		assert.Equal(t, provenance.Attachment.Identity.ProvenanceID,
			provenance.Attachment.After.ProvenanceID)
		assert.Equal(t, file.ID, provenance.Attachment.After.NodeID)
		assert.Equal(t, run.ID(), provenance.Attachment.After.IngestID)
		require.NotNil(t, provenance.Attachment.After.OriginalPath)
		assert.Equal(t, "/source/reports/history.txt",
			*provenance.Attachment.After.OriginalPath)
	})
}

func findAuditHistoryEvent(t *testing.T, events []AuditEvent, kind string) AuditEvent {
	t.Helper()
	for _, event := range events {
		if event.Kind == kind {
			return event
		}
	}
	require.FailNow(t, "audit event not found", "kind=%s", kind)
	return AuditEvent{}
}
