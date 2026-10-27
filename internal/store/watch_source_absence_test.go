package store

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func requireAbsence(t *testing.T, s *Store, ref string, want string) {
	t.Helper()
	var got string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COALESCE((SELECT absent_since FROM watch_source_absences WHERE watch_name='notes' AND source_ref=?),'')`, ref).Scan(&got))
	require.Equal(t, want, got)
}

func TestSourcePresentSearchAndCatalog(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	for i := range 5 {
		ref := fmt.Sprintf("draft-%d.md", i)
		seedWatchRebind(t, s, ref, ref)
	}
	require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", WatchPresenceScan{ListedDirs: []string{""}, RegularFiles: []string{"draft-0.md", "draft-2.md", "draft-4.md"}}, time.Now()))
	hits, _, err := s.SearchPageWithOptions(ctx, "draft", 10, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, hits, 5)
	hits, _, err = s.SearchPageWithOptions(ctx, "draft", 10, SearchOptions{SourcePresent: true})
	require.NoError(t, err)
	require.Len(t, hits, 3)
	fuzzy, _, _, err := discoverUnbudgeted(t, s, "darft", 10, SearchOptions{SourcePresent: true})
	require.NoError(t, err)
	require.Len(t, fuzzy, 3)
	q := DocumentCatalogQuery{SourcePresent: true, PageSize: 2}
	first, err := s.ListDocuments(ctx, q, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Equal(t, []string{"/draft-0.md", "/draft-2.md"}, documentCatalogPaths(first.Items))
	require.False(t, first.HasPrevious)
	require.True(t, first.HasNext)
	next, err := s.ListDocuments(ctx, q, &first.LastPosition, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Equal(t, []string{"/draft-4.md"}, documentCatalogPaths(next.Items))
	require.True(t, next.HasPrevious)
	require.False(t, next.HasNext)
	back, err := s.ListDocuments(ctx, q, &next.FirstPosition, DocumentCatalogTraversalPrevious)
	require.NoError(t, err)
	require.Equal(t, documentCatalogPaths(first.Items), documentCatalogPaths(back.Items))
	_, _, err = s.AcceptPush(ctx, PushSource{Name: "notes", Ref: "draft-1.md"}, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "draft-1.md", Hash: fakeHash("b2"), Size: 6, MIMEType: "text/plain"})
	require.NoError(t, err)
	hits, _, err = s.SearchPageWithOptions(ctx, "draft", 10, SearchOptions{SourcePresent: true})
	require.NoError(t, err)
	require.Len(t, hits, 4, "a push-owned node is exempt even with a marked watch cursor")
}

func TestSourcePresentFenceCountsBeforeCap(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	insertProcessingFenceFiles(t, s, 4096)
	seedWatchRebind(t, s, "gone.md", "gone.md")
	require.NoError(t, s.ReconcileWatchSourcePresence(t.Context(), "notes", WatchPresenceScan{ListedDirs: []string{""}}, time.Now()))
	_, err := s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{Filters: &SearchOptions{}})
	require.ErrorIs(t, err, ErrProcessingSourceFenceScopeTooLarge)
	filtered, err := s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{Filters: &SearchOptions{SourcePresent: true}})
	require.NoError(t, err)
	require.Equal(t, 4096, filtered.ObservedScopeCount)
	require.Len(t, filtered.ContentVersionIDs, 4096)
}

func TestWatchSourceAbsenceEvidenceAndStorageBoundary(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := seedWatchRebind(t, s, "sub/draft.md", "draft.md")
	ctx := t.Context()
	var before bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &before))
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, scan := range []WatchPresenceScan{
		{}, {ListedDirs: []string{""}, Unsettled: []string{"sub"}}, {ListedDirs: []string{""}, ListedEntries: []string{"sub"}},
		{ListedDirs: []string{""}, Excluded: func(ref string) bool { return ref == "sub" }},
		{ListedDirs: []string{"sub"}, Unsettled: []string{"sub/draft.md"}},
	} {
		changed, err := s.WatchSourcePresenceChanged(ctx, "notes", scan)
		require.NoError(t, err)
		require.False(t, changed)
		require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", scan, now))
		requireAbsence(t, s, "sub/draft.md", "")
	}
	absent := WatchPresenceScan{ListedDirs: []string{"sub"}}
	require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", absent, now))
	require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", absent, now.Add(time.Hour)))
	requireAbsence(t, s, "sub/draft.md", "2026-01-02T03:04:05Z")
	require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", WatchPresenceScan{RegularFiles: []string{"sub/draft.md"}, Unsettled: []string{"sub/draft.md"}}, now))
	requireAbsence(t, s, "sub/draft.md", "2026-01-02T03:04:05Z")
	require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", WatchPresenceScan{}, now))
	requireAbsence(t, s, "sub/draft.md", "2026-01-02T03:04:05Z")
	var after bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &after))
	require.Equal(t, before.String(), after.String())
	require.NoError(t, s.ValidateMetadata(ctx))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(after.Bytes())))
	requireAbsence(t, restored, "sub/draft.md", "")
	require.Equal(t, node, requireNodeByID(t, s, node.ID))
	require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", WatchPresenceScan{RegularFiles: []string{"sub/draft.md"}}, now))
	requireAbsence(t, s, "sub/draft.md", "")
	trashed, _, err := s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", absent, now))
	requireAbsence(t, s, "sub/draft.md", "2026-01-02T03:04:05Z")
	require.Equal(t, trashed, requireNodeByID(t, s, node.ID))
}

func TestWatchSourceAbsenceFromDeletedSubtree(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, listed := range [][]string{{""}, {"", "a"}} {
		s := newTestStore(t)
		seedWatchRebind(t, s, "a/b/draft.md", "draft.md")
		scan := WatchPresenceScan{ListedDirs: listed}
		changed, err := s.WatchSourcePresenceChanged(ctx, "notes", scan)
		require.NoError(t, err)
		require.True(t, changed, "listed %q", listed)
		require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", scan, now))
		requireAbsence(t, s, "a/b/draft.md", "2026-01-02T03:04:05Z")
		changed, err = s.WatchSourcePresenceChanged(ctx, "notes", scan)
		require.NoError(t, err)
		require.False(t, changed, "an existing mark needs no write")
	}
}

func TestWatchSourceAbsenceCascadeAndRebind(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := seedWatchRebind(t, s, "draft.md", "draft.md")
	ctx := t.Context()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mark := func(ref string) {
		t.Helper()
		require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes", WatchPresenceScan{ListedDirs: []string{""}}, now))
		requireAbsence(t, s, ref, "2026-01-02T03:04:05Z")
	}
	mark("draft.md")
	_, err := s.db.ExecContext(ctx, `UPDATE watch_sources SET source_ref='moved.md' WHERE watch_name='notes'`)
	require.NoError(t, err)
	requireAbsence(t, s, "moved.md", "2026-01-02T03:04:05Z")
	_, err = s.RebindWatchedSource(ctx, "notes", "moved.md", "final.md", node.ID)
	require.NoError(t, err)
	requireAbsence(t, s, "final.md", "")
	mark("final.md")
	_, err = s.RebindWatchedSource(ctx, "notes", "moved.md", "final.md", node.ID)
	require.NoError(t, err)
	requireAbsence(t, s, "final.md", "")
	mark("final.md")
	require.NoError(t, s.DeleteWatchSourceAbsencesExcept(ctx, []string{"notes"}))
	requireAbsence(t, s, "final.md", "2026-01-02T03:04:05Z")
	require.NoError(t, s.DeleteWatchSourceAbsencesExcept(ctx, []string{"other"}))
	requireAbsence(t, s, "final.md", "")
	mark("final.md")
	_, err = s.db.ExecContext(ctx, `DELETE FROM watch_sources WHERE watch_name='notes'`)
	require.NoError(t, err)
	requireAbsence(t, s, "final.md", "")
}

// Missing parent evidence must never change a cursor's mark; exact regular-file
// evidence clears it. The model deliberately uses a fixed ref and literal time.
func FuzzWatchSourcePresence(f *testing.F) {
	f.Add([]byte{1, 0, 3, 2, 1, 4})
	f.Add([]byte{1, 5, 2})
	f.Fuzz(func(t *testing.T, steps []byte) {
		if len(steps) > 32 {
			steps = steps[:32]
		}
		s := newTestStore(t)
		seedWatchRebind(t, s, "draft.md", "draft.md")
		want := ""
		for _, step := range steps {
			scan := WatchPresenceScan{}
			switch step % 6 {
			case 1:
				scan.ListedDirs = []string{""}
				if want == "" {
					want = "2026-01-02T03:04:05Z"
				}
			case 2:
				scan.RegularFiles = []string{"draft.md"}
				want = ""
			case 3:
				scan.ListedDirs = []string{""}
				scan.Unsettled = []string{"draft.md"}
			case 4:
				scan.RegularFiles = []string{"DRAFT.md"}
			case 5:
				scan.RegularFiles = []string{"draft.md"}
				scan.Unsettled = []string{"draft.md"}
			}
			require.NoError(t, s.ReconcileWatchSourcePresence(t.Context(), "notes", scan, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
			requireAbsence(t, s, "draft.md", want)
		}
	})
}
