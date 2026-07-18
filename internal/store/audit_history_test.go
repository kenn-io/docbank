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
	assert.Equal(t, "/Taxes/return.txt", *first.Items[0].OldPath)
	assert.Equal(t, "/Taxes/2027/return.txt", *first.Items[0].NewPath)
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
