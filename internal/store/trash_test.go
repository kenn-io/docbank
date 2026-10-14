package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrashAndRestoreRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	f, err := s.CreateFile(ctx, docs.ID, "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	_, origPath, err := s.Trash(ctx, docs.ID, -1)
	require.NoError(t, err)
	assert.Equal(t, "/docs", origPath)

	// Subtree is gone from live views.
	_, err = s.NodeByPath(ctx, "/docs")
	require.ErrorIs(t, err, ErrNotFound)
	trashedChild, err := s.NodeByID(ctx, f.ID)
	require.NoError(t, err)
	assert.NotNil(t, trashedChild.TrashedAt)

	// Name is reusable while trashed.
	_, err = s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)

	// Restore re-suffixes because /docs is taken again.
	restored, restoredPath, err := s.Restore(ctx, docs.ID, -1)
	require.NoError(t, err)
	assert.Equal(t, "/docs (2)", restoredPath)
	assert.Equal(t, "docs (2)", restored.Name)
	assert.Nil(t, restored.TrashedAt)

	back, err := s.NodeByPath(ctx, "/docs (2)/a.txt")
	require.NoError(t, err)
	assert.Equal(t, f.ID, back.ID)
}

func TestRestoreChoosesFirstAvailableExactName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, original, want string
		occupied             []string
	}{
		{"original free", "report.pdf", "report.pdf", []string{"report (2).pdf"}},
		{"noncanonical suffixes", "report.pdf", "report (2).pdf",
			[]string{"report.pdf", "report (02).pdf", "report (+2).pdf", "Report (2).pdf", "report (3).pdf"}},
		{"gap", "report.pdf", "report (3).pdf",
			[]string{"report.pdf", "report (2).pdf", "report (4).pdf"}},
		{"later gap", "report.pdf", "report (4).pdf",
			[]string{"report.pdf", "report (2).pdf", "report (3).pdf", "report (5).pdf"}},
		{"dotfile", ".hidden", ".hidden (3)", []string{".hidden", ".hidden (2)"}},
		{"literal characters", "résumé_%[x].tar.gz", "résumé_%[x].tar (3).gz",
			[]string{"résumé_%[x].tar.gz", "résumé_%[x].tar (2).gz"}},
		{"already suffixed", "report (2).pdf", "report (2) (3).pdf",
			[]string{"report (2).pdf", "report (2) (2).pdf"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			node, err := s.CreateFile(ctx, s.RootID(), tc.original, fakeHash("a1"), 1, "text/plain")
			require.NoError(t, err)
			_, _, err = s.Trash(ctx, node.ID, UnconditionalRev)
			require.NoError(t, err)
			for i, name := range tc.occupied {
				if i%2 == 0 {
					_, err = s.Mkdir(ctx, s.RootID(), name)
				} else {
					_, err = s.CreateFile(ctx, s.RootID(), name, fakeHash("a2"), 1, "text/plain")
				}
				require.NoError(t, err)
			}
			// A trashed sibling and a node in another directory do not reserve
			// the chosen name in the restore destination.
			old, err := s.Mkdir(ctx, s.RootID(), tc.want)
			require.NoError(t, err)
			_, _, err = s.Trash(ctx, old.ID, UnconditionalRev)
			require.NoError(t, err)
			other, err := s.Mkdir(ctx, s.RootID(), "other")
			require.NoError(t, err)
			_, err = s.Mkdir(ctx, other.ID, tc.want)
			require.NoError(t, err)

			restored, path, err := s.Restore(ctx, node.ID, UnconditionalRev)
			require.NoError(t, err)
			assert.Equal(t, tc.want, restored.Name)
			assert.Equal(t, "/"+tc.want, path)
			found, err := s.NodeByPath(ctx, path)
			require.NoError(t, err)
			assert.Equal(t, node.ID, found.ID)
		})
	}
}

func TestRestoreNameCollisionHonorsCanceledSearch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.Mkdir(t.Context(), s.RootID(), "report.pdf")
	require.NoError(t, err)
	_, err = s.Mkdir(t.Context(), s.RootID(), "report (2).pdf")
	require.NoError(t, err)
	tx, err := s.writeDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Keep the transaction alive: both point probes succeed, but the longer
	// suffix search must use the caller's canceled context.
	_, err = nextFreeNameTx(ctx, tx, s.RootID(), "report.pdf")
	require.ErrorIs(t, err, context.Canceled)
}

func TestNestedTrashRestoreKeepsEarlierTrash(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	inner, err := s.Mkdir(ctx, docs.ID, "inner")
	require.NoError(t, err)

	// Trash inner first (separate operation), then the whole of docs.
	_, _, err = s.Trash(ctx, inner.ID, -1)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, docs.ID, -1)
	require.NoError(t, err)

	// Restoring docs must NOT resurrect inner.
	_, _, err = s.Restore(ctx, docs.ID, -1)
	require.NoError(t, err)
	_, err = s.NodeByPath(ctx, "/docs/inner")
	require.ErrorIs(t, err, ErrNotFound)

	// inner is still restorable on its own.
	_, _, err = s.Restore(ctx, inner.ID, -1)
	require.NoError(t, err)
	_, err = s.NodeByPath(ctx, "/docs/inner")
	require.NoError(t, err)
}

// deleteTrashRoot hard-deletes a node row directly — a test-only shortcut
// for "the original parent no longer exists".
func deleteTrashRoot(t *testing.T, s *Store, id int64) {
	t.Helper()
	_, err := s.db.Exec(`DELETE FROM nodes WHERE id = ?`, id)
	require.NoError(t, err)
}

func TestRestoreFallsBackToRootWhenParentGone(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	f, err := s.CreateFile(ctx, docs.ID, "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	// f becomes its own trash root, then its original home disappears.
	_, _, err = s.Trash(ctx, f.ID, -1)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, docs.ID, -1)
	require.NoError(t, err)
	deleteTrashRoot(t, s, docs.ID)

	restored, _, err := s.Restore(ctx, f.ID, -1)
	require.NoError(t, err)
	p, err := s.Path(ctx, restored.ID)
	require.NoError(t, err)
	assert.Equal(t, "/a.txt", p)
}

func TestTrashGuards(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	_, _, err := s.Trash(ctx, s.RootID(), -1)
	require.ErrorIs(t, err, ErrIsRoot)

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, docs.ID, -1)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, docs.ID, -1)
	require.ErrorIs(t, err, ErrNotFound)

	// Restore of a non-trash-root child is refused.
	inner, err := s.Mkdir(ctx, s.RootID(), "x")
	require.NoError(t, err)
	leaf, err := s.CreateFile(ctx, inner.ID, "y.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, inner.ID, -1)
	require.NoError(t, err)
	_, _, err = s.Restore(ctx, leaf.ID, -1)
	assert.ErrorIs(t, err, ErrNotTrashed)
}

func TestTrashPath(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, docs.ID, "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	n, origPath, err := s.TrashPath(ctx, "/docs")
	require.NoError(t, err)
	assert.Equal(t, docs.ID, n.ID)
	// Pre-trash path from inside the trash transaction: afterwards the
	// trash root is re-parented and the path is uncomputable.
	assert.Equal(t, "/docs", origPath)

	// Gone from the live tree; restorable as a trash root.
	_, err = s.NodeByPath(ctx, "/docs")
	require.ErrorIs(t, err, ErrNotFound)
	roots, err := s.TrashedRoots(ctx)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, docs.ID, roots[0].ID)

	// The root and no-longer-resolving paths are refused.
	_, _, err = s.TrashPath(ctx, "/")
	require.ErrorIs(t, err, ErrIsRoot)
	_, _, err = s.TrashPath(ctx, "/docs")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestTrashedRootsPageIsBounded(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	for _, name := range []string{"first", "second", "third"} {
		node, err := s.Mkdir(ctx, s.RootID(), name)
		require.NoError(t, err)
		_, _, err = s.Trash(ctx, node.ID, node.Revision)
		require.NoError(t, err)
	}

	first, total, err := s.TrashedRootsPage(ctx, 2, 0)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, 3, total)

	last, total, err := s.TrashedRootsPage(ctx, 2, 2)
	require.NoError(t, err)
	require.Len(t, last, 1)
	assert.Equal(t, 3, total)
	assert.NotEqual(t, first[0].ID, last[0].ID)
	assert.NotEqual(t, first[1].ID, last[0].ID)

	_, _, err = s.TrashedRootsPage(ctx, 0, 0)
	require.ErrorContains(t, err, "trash limit")
	_, _, err = s.TrashedRootsPage(ctx, 1, -1)
	require.ErrorContains(t, err, "trash offset")
}

func TestTrashEmptyRetainsAuthorityPinsAndDeletesUnrelatedRoots(t *testing.T) {
	t.Parallel()
	for _, authority := range []string{"source version", "input artifact", "checkout entry", "sealed snapshot"} {
		t.Run(authority, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			name, mediaType := "protected.bin", "application/octet-stream"
			if authority == "checkout entry" {
				name, mediaType = "protected.jpg", "image/jpeg"
			}
			protected, err := s.CreateFile(
				ctx, s.RootID(), name, fakeHash("a1"), 10, mediaType)
			require.NoError(t, err)
			unrelated, err := s.CreateFile(
				ctx, s.RootID(), "unrelated.bin", fakeHash("b2"), 20, "application/octet-stream")
			require.NoError(t, err)
			stamp := nowRFC3339()
			if authority == "source version" || authority == "input artifact" {
				_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
					fakeHash("a1"), stamp)
				require.NoError(t, err)
			}
			switch authority {
			case "sealed snapshot":
				snapshotID, idErr := newUUIDv4()
				require.NoError(t, idErr)
				occurrence := strings.Repeat("a", 32)
				_, err = s.SealCollectionSnapshot(ctx, SnapshotSealRequest{SnapshotID: snapshotID,
					Members: []CollectionSnapshotMember{{Ordinal: 1, OccurrenceID: occurrence,
						NodeID: protected.ID, ContentVersionID: protected.CurrentVersionID,
						BlobSHA256: protected.BlobHash, Size: protected.Size, FamilyID: occurrence, FamilyOrder: 1,
						DisplayName: protected.Name, FrozenFieldsJSON: "{}", DocumentKind: "other"}}})
			case "source version":
				_, err = s.db.Exec(`INSERT INTO media_source_versions VALUES(
					'source-version','source',1,?,?,?)`, protected.CurrentVersionID, `{}`, stamp)
			case "checkout entry":
				asset, lookupErr := s.PhotoAssetForNode(ctx, protected.ID)
				require.NoError(t, lookupErr)
				checkout, operation, createErr := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "checkout", 0)
				require.NoError(t, createErr)
				err = s.FinishPhotoCheckout(ctx, checkout.ID, operation.ID, StorageOperationFailed, `{}`, "checkout failed", time.Time{})
			case "input artifact":
				_, err = s.db.Exec(`INSERT INTO media_occurrences VALUES(
					'occurrence','source',NULL,'operator','ref','1','','','','{}',1,?,NULL)`, stamp)
				require.NoError(t, err)
				_, err = s.db.Exec(`INSERT INTO media_input_artifacts VALUES(
					'input','occurrence','source',NULL,?,'recording','supplied','','',?,?)`,
					protected.CurrentVersionID, protected.BlobHash, stamp)
			}
			require.NoError(t, err)
			_, _, err = s.Trash(ctx, protected.ID, protected.Revision)
			require.NoError(t, err)
			_, _, err = s.Trash(ctx, unrelated.ID, unrelated.Revision)
			require.NoError(t, err)

			preview, err := s.TrashEmptyBounded(ctx, 0, 1, false)
			require.NoError(t, err)
			require.Equal(t, int64(1), preview.Candidates)
			require.False(t, preview.More)
			expectedRetained := map[string]int64{"checkout entry": 1}[authority]
			require.Equal(t, expectedRetained, preview.Retained)
			run, err := s.TrashEmptyBounded(ctx, 0, 1, true)
			require.NoError(t, err)
			require.Equal(t, preview.Candidates, run.Candidates)
			require.Equal(t, preview.Retained, run.Retained)
			require.Equal(t, int64(1), run.Deleted)
			_, err = s.NodeByID(ctx, protected.ID)
			require.NoError(t, err)
			_, err = s.NodeByID(ctx, unrelated.ID)
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

func TestTrashEmptyRemovesProcessedDocumentRenditionAttachment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		remaining int
	}{{"only attachment", 0}, {"second live attachment", 1}} {
		t.Run(test.name, func(t *testing.T) {
			s, versions := newRenditionCatalogFixture(t)
			ctx := t.Context()
			profile := catalogProcessingProfile(t, false)
			build := catalogRenditionBuild(s, profile)
			require.NoError(t, s.StageRenditionBuild(ctx, build))
			attachment := RenditionAttachmentRecord{
				ID: catalogAttachmentFirst, VaultID: s.VaultID(),
				ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
				AttachedAt: "2026-08-24T15:10:00.000000000Z",
			}
			require.NoError(t, publishAttachmentForTest(t, s, attachment))
			if test.remaining == 1 {
				attachment.ID = catalogAttachmentSecond
				attachment.ContentVersionID = versions[1]
				attachment.Profile = catalogProcessingProfile(t, true)
				require.NoError(t, publishAttachmentForTest(t, s, attachment))
			}

			node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
			require.NoError(t, err)
			_, _, err = s.Trash(ctx, node.ID, node.Revision)
			require.NoError(t, err)
			report, err := s.TrashEmpty(ctx, 0, true)
			require.NoError(t, err)
			assert.Equal(t, int64(1), report.Deleted)

			_, err = s.ActiveRendition(ctx, versions[0], profile.Fingerprint)
			require.ErrorIs(t, err, ErrNotFound)

			var attachments, heads, builds int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_attachments`).Scan(&attachments))
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&heads))
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&builds))
			assert.Equal(t, []int{test.remaining, test.remaining, 1}, []int{attachments, heads, builds},
				"permanent document deletion revokes version authority while immutable builds remain for GC")
		})
	}
}

func TestVaultEmptyTrashBoundedStore(t *testing.T) {
	t.Parallel()
	const total, limit = 205, 101
	s := newTestStore(t)
	ctx := t.Context()
	for i := range total {
		node, err := s.Mkdir(ctx, s.RootID(), fmt.Sprintf("folder-%d", i))
		require.NoError(t, err)
		_, err = s.CreateFile(ctx, node.ID, "notes.txt", fakeHash("b2"), 1, "text/plain")
		require.NoError(t, err)
		_, _, err = s.Trash(ctx, node.ID, UnconditionalRev)
		require.NoError(t, err)
	}
	recent, err := s.TrashEmpty(ctx, time.Hour, false)
	require.NoError(t, err)
	assert.Equal(t, TrashEmptyResult{}, recent)
	for remaining := total; remaining > 0; remaining -= limit {
		count, more := int64(min(remaining, limit)), remaining > limit
		dryRun, err := s.TrashEmptyBounded(ctx, 0, limit, false)
		require.NoError(t, err)
		assert.Equal(t, TrashEmptyResult{Candidates: count, More: more}, dryRun)
		roots, err := s.TrashedRoots(ctx)
		require.NoError(t, err)
		require.Len(t, roots, remaining)
		deleted, err := s.TrashEmptyBounded(ctx, 0, limit, true)
		require.NoError(t, err)
		assert.Equal(t, TrashEmptyResult{Candidates: count, Deleted: count, More: more, Run: true}, deleted)
		roots, err = s.TrashedRoots(ctx)
		require.NoError(t, err)
		require.Len(t, roots, remaining-int(count))
	}
	var nodeCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodeCount))
	assert.Equal(t, 1, nodeCount)
	var blobCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobCount))
	assert.Equal(t, 1, blobCount)
	_, err = s.TrashEmptyBounded(ctx, 0, 0, true)
	require.ErrorContains(t, err, "maximum trash roots must be positive")
}

func TestTrashEmptyAdvancesAffectedTagRevisionsOnce(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	dir, err := s.Mkdir(ctx, s.RootID(), "tagged-trash")
	require.NoError(t, err)
	child, err := s.CreateFile(ctx, dir.ID, "child.txt", fakeHash("tagged"), 6, "text/plain")
	require.NoError(t, err)
	dir, err = s.NodeByID(ctx, dir.ID)
	require.NoError(t, err)
	affected, err := s.CreateTag(ctx, "affected")
	require.NoError(t, err)
	change, err := s.AssignTag(ctx, affected.ID, dir.ID, dir.Revision)
	require.NoError(t, err)
	dir = change.Node
	change, err = s.AssignTag(ctx, affected.ID, child.ID, child.Revision)
	require.NoError(t, err)
	affected = change.Tag

	live, err := s.Mkdir(ctx, s.RootID(), "live")
	require.NoError(t, err)
	unaffected, err := s.CreateTag(ctx, "unaffected")
	require.NoError(t, err)
	change, err = s.AssignTag(ctx, unaffected.ID, live.ID, live.Revision)
	require.NoError(t, err)
	unaffected = change.Tag

	_, _, err = s.Trash(ctx, dir.ID, dir.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, false)
	require.NoError(t, err)
	afterDryRun, err := s.TagByID(ctx, affected.ID)
	require.NoError(t, err)
	assert.Equal(t, affected.Revision, afterDryRun.Revision)

	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	afterEmpty, err := s.TagByID(ctx, affected.ID)
	require.NoError(t, err)
	assert.Equal(t, affected.Revision+1, afterEmpty.Revision)
	assert.Zero(t, afterEmpty.AssignmentCount)
	stillUnaffected, err := s.TagByID(ctx, unaffected.ID)
	require.NoError(t, err)
	assert.Equal(t, unaffected, stillUnaffected)
}

func TestTrashEmptyWholeSecondTimestamp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	// trashed_at is compared to the cutoff as a string, so a whole-second
	// timestamp must sort before a cutoff with fractional digits in the same
	// second. Under the old variable-width RFC3339Nano format it rendered
	// without fractional digits ("...:00Z" > "...:00.5Z") and survived.
	d, err := s.Mkdir(ctx, s.RootID(), "old")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, d.ID, -1)
	require.NoError(t, err)
	stamp := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(timestampLayout)
	_, err = s.db.Exec(`UPDATE nodes SET trashed_at = ? WHERE id = ?`, stamp, d.ID)
	require.NoError(t, err)

	rep, err := s.TrashEmpty(ctx, time.Hour, true)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rep.Deleted)
}

func TestRestoreAfterParentHardDeleteNeverRetargets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	// f gets a lower id than its parent D, so hard-deleting D frees the
	// maximum rowid — exactly the id a fresh directory would reuse if node
	// ids were reusable.
	f, err := s.CreateFile(ctx, s.RootID(), "f.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	d, err := s.Mkdir(ctx, s.RootID(), "D")
	require.NoError(t, err)
	_, _, err = s.Move(ctx, f.ID, d.ID, "f.txt", -1)
	require.NoError(t, err)

	_, _, err = s.Trash(ctx, f.ID, -1) // records trash_parent = D
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, d.ID, -1)
	require.NoError(t, err)
	deleteTrashRoot(t, s, d.ID)

	e, err := s.Mkdir(ctx, s.RootID(), "E")
	require.NoError(t, err)
	require.NotEqual(t, d.ID, e.ID, "node ids must never be reused")

	// The original parent is gone: restore must fall back to the root, not
	// land inside an unrelated directory occupying a reused id.
	restored, _, err := s.Restore(ctx, f.ID, -1)
	require.NoError(t, err)
	p, err := s.Path(ctx, restored.ID)
	require.NoError(t, err)
	assert.Equal(t, "/f.txt", p)
}
