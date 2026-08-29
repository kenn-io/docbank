package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveProcessingSourceFenceExplicitIDsAreSortedAndCurrentLive(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateFile(t.Context(), s.RootID(), "first.txt", fakeHash("first"), 5, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(t.Context(), s.RootID(), "second.txt", fakeHash("second"), 6, "text/plain")
	require.NoError(t, err)

	resolved, err := s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{
		ContentVersionIDs: []string{second.CurrentVersionID, first.CurrentVersionID},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, resolved.ObservedScopeCount)
	want := []string{first.CurrentVersionID, second.CurrentVersionID}
	slices.Sort(want)
	assert.Equal(t, want, resolved.ContentVersionIDs)

	replaced, replacement, err := s.ReplaceContent(t.Context(), first.ID, first.Revision,
		fakeHash("replacement"), 11, "text/plain")
	require.NoError(t, err)
	_, err = s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{
		ContentVersionIDs: []string{first.CurrentVersionID},
	})
	require.ErrorIs(t, err, ErrProcessingSourceFenceStaleVersion)

	_, _, err = s.Trash(t.Context(), replaced.ID, replaced.Revision)
	require.NoError(t, err)
	_, err = s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{
		ContentVersionIDs: []string{replacement.ID},
	})
	require.ErrorIs(t, err, ErrProcessingSourceFenceStaleVersion)

	_, err = s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{
		ContentVersionIDs: []string{"33333333-3333-4333-8333-333333333333"},
	})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestResolveProcessingSourceFenceMetadataFiltersReuseSearchNormalization(t *testing.T) {
	s := newTestStore(t)
	scope, err := s.Mkdir(t.Context(), s.RootID(), "scope")
	require.NoError(t, err)
	inside, err := s.CreateFile(t.Context(), scope.ID, "inside.txt", fakeHash("inside"), 6,
		"Text/Plain; Charset=UTF-8")
	require.NoError(t, err)
	outside, err := s.CreateFile(t.Context(), s.RootID(), "outside.pdf", fakeHash("outside"), 7,
		"application/pdf")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "selected")
	require.NoError(t, err)
	inside, err = func() (Node, error) {
		change, assignErr := s.AssignTag(t.Context(), tag.ID, inside.ID, inside.Revision)
		return change.Node, assignErr
	}()
	require.NoError(t, err)
	require.NoError(t, setProcessingFenceModifiedAt(s, inside.ID, "2026-08-28T10:00:00.000000000Z"))
	require.NoError(t, setProcessingFenceModifiedAt(s, outside.ID, "2026-08-28T11:00:00.000000000Z"))

	filters := SearchOptions{TagID: tag.ID, MIMEType: "TEXT/PLAIN", UnderNodeID: scope.ID,
		ModifiedSince: "2026-08-28T12:00:00+02:00", ModifiedBefore: "2026-08-28T10:00:01Z"}
	resolved, err := s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{Filters: &filters})
	require.NoError(t, err)
	assert.Equal(t, []string{inside.CurrentVersionID}, resolved.ContentVersionIDs)
	assert.Equal(t, 1, resolved.ObservedScopeCount)

	all := SearchOptions{}
	resolved, err = s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{Filters: &all})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{inside.CurrentVersionID, outside.CurrentVersionID}, resolved.ContentVersionIDs)
	assert.Equal(t, 2, resolved.ObservedScopeCount)

	bad := SearchOptions{ModifiedSince: "2026-08-28T10:00:00Z", ModifiedBefore: "2026-08-28T10:00:00Z"}
	_, err = s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{Filters: &bad})
	require.ErrorContains(t, err, "modified_since must be earlier")
}

func TestResolveProcessingSourceFenceAccepts4096AndNeverTruncates4097(t *testing.T) {
	s := newTestStore(t)
	ids := insertProcessingFenceFiles(t, s, 4097)

	resolved, err := s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{
		ContentVersionIDs: ids[:4096],
	})
	require.NoError(t, err)
	assert.Len(t, resolved.ContentVersionIDs, 4096)
	assert.Equal(t, 4096, resolved.ObservedScopeCount)

	_, err = s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{
		ContentVersionIDs: ids,
	})
	var tooLarge *ProcessingSourceFenceScopeError
	require.ErrorAs(t, err, &tooLarge)
	require.ErrorIs(t, err, ErrProcessingSourceFenceScopeTooLarge)
	assert.Equal(t, 4097, tooLarge.ObservedScopeCount)
	assert.Contains(t, err.Error(), "narrow the source scope")

	filters := SearchOptions{}
	resolved, err = s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{Filters: &filters})
	require.ErrorAs(t, err, &tooLarge)
	assert.Empty(t, resolved.ContentVersionIDs, "an oversized scope must not return a partial fence")
	assert.Equal(t, 4097, tooLarge.ObservedScopeCount, "the full observed population must be reported")
}

func TestResolveProcessingSourceFenceRejectsInvalidModesAndIDs(t *testing.T) {
	s := newTestStore(t)
	empty := SearchOptions{}
	for _, request := range []ProcessingSourceFenceRequest{
		{},
		{ContentVersionIDs: []string{"11111111-1111-4111-8111-111111111111"}, Filters: &empty},
		{ContentVersionIDs: []string{"not-a-uuid"}},
		{ContentVersionIDs: []string{
			"11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-111111111111",
		}},
	} {
		_, err := s.ResolveProcessingSourceFence(t.Context(), request)
		require.ErrorIs(t, err, ErrInvalidProcessingSourceFence)
	}
}

func TestResolveProcessingSourceFenceUsesOneReadSnapshot(t *testing.T) {
	s := newTestStore(t)
	created, err := s.CreateFile(t.Context(), s.RootID(), "snapshot.txt", fakeHash("old"), 3, "text/plain")
	require.NoError(t, err)

	snapshot, err := s.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = snapshot.Close() })

	replaced, replacement, err := s.ReplaceContent(t.Context(), created.ID, created.Revision,
		fakeHash("new"), 3, "text/plain")
	require.NoError(t, err)
	assert.Equal(t, replacement.ID, replaced.CurrentVersionID)

	resolved, err := s.resolveFilteredProcessingSourceFenceSnapshot(t.Context(), snapshot.tx, SearchOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{created.CurrentVersionID}, resolved.ContentVersionIDs,
		"one resolution must not mix authority committed after its snapshot was pinned")
	require.NoError(t, snapshot.Close())

	resolved, err = s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{
		Filters: &SearchOptions{},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{replacement.ID}, resolved.ContentVersionIDs)
}

func insertProcessingFenceFiles(t *testing.T, s *Store, count int) []string {
	t.Helper()
	ids := make([]string, count)
	err := s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		for i := range count {
			ids[i] = uuid.NewString()
			hash := fakeHash(fmt.Sprintf("processing-fence-%d", i))
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO blobs(hash,size,created_at) VALUES(?,1,'2026-08-28T00:00:00.000000000Z')`, hash); err != nil {
				return err
			}
			result, err := tx.ExecContext(t.Context(), `INSERT INTO nodes(
				parent_id,name,kind,current_version_id,created_at,modified_at
			) VALUES(?,?,'file',?,'2026-08-28T00:00:00.000000000Z','2026-08-28T00:00:00.000000000Z')`,
				s.RootID(), fmt.Sprintf("f-%04d", i), ids[i])
			if err != nil {
				return err
			}
			nodeID, err := result.LastInsertId()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO content_versions(
				version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
				introduced_operation_id,transition_kind
			) VALUES(?,?,?,1,'text/plain','2026-08-28T00:00:00.000000000Z',1,?,'content_create')`,
				ids[i], nodeID, hash, uuid.NewString()); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	return ids
}

func setProcessingFenceModifiedAt(s *Store, nodeID int64, value string) error {
	result, err := s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id=?`, value, nodeID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("synthetic node was not updated")
	}
	return nil
}
