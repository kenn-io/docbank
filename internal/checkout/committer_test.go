package checkout

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
	"go.kenn.io/kit/packstore"
)

func completedCheckout(t *testing.T, s Service, selection store.PhotoCheckoutSelection) (store.PhotoCheckout, []store.PhotoCheckoutEntry) {
	t.Helper()
	c, _, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 1<<30)
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	require.NoError(t, s.Run(t.Context(), c, root))
	c, err = s.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	return c, entries
}

func TestCheckoutCommitRejectsScannerRootSwap(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("requires renaming an open directory and creating a symlink")
	}
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	asset := checkoutPhoto(t, s, "photo.jpg", "original")
	parent := filepath.Join(t.TempDir(), "working")
	destination := filepath.Join(parent, "checkout")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	c, _, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, 100)
	require.NoError(t, err)
	require.NoError(t, s.Run(t.Context(), c, root))
	require.NoError(t, root.Close())
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	foreignParent := t.TempDir()
	foreignFile := filepath.Join(foreignParent, "checkout", filepath.FromSlash(entries[0].Path))
	require.NoError(t, os.MkdirAll(filepath.Dir(foreignFile), 0o700))
	require.NoError(t, os.WriteFile(foreignFile, []byte("foreign bytes"), 0o600))
	c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	_, err = s.Store.ClaimStorageOperation(t.Context(), c.LastOperationID)
	require.NoError(t, err)
	root, err = openCheckoutRoot(c)
	require.NoError(t, err)
	require.NoError(t, os.Rename(parent, parent+"-original"))
	require.NoError(t, os.Symlink(foreignParent, parent))
	restored := false
	restore := func() {
		if !restored {
			require.NoError(t, os.Remove(parent))
			require.NoError(t, os.Rename(parent+"-original", parent))
			restored = true
		}
	}
	_, scanErr := s.scanWorking(t.Context(), c, func(ctx context.Context, file ingest.FolderFile, entry store.PhotoCheckoutEntry) error {
		return s.commitEntry(ctx, c, file, entry)
	}, nil, scanHooks{root: root, before: func(context.Context, string) error {
		// Restore the recorded path after the scanner opens the other root.
		restore()
		return nil
	}})
	restore()
	require.NoError(t, root.Close())
	_, total, err := s.Store.ContentVersions(t.Context(), asset.Files[0].NodeID, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total, "bytes outside the pinned checkout must never become a version")
	require.ErrorIs(t, scanErr, store.ErrInvalidPhotoCheckout)
	require.ErrorContains(t, scanErr, "destination identity changed")
}

func TestCheckoutCommitRejectsAmbiguousStoredPaths(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	first := checkoutPhoto(t, s, "first.jpg", "first")
	second := checkoutPhoto(t, s, "second.jpg", "second")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{first.ID, second.ID}})
	// Creation rejects duplicate names. Inject colliding stored paths to
	// exercise verification of an already recorded checkout.
	db, err := s.Store.SQLiteDriver().Open(filepath.Join(s.VaultRoot, "docbank.db"), sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `UPDATE photo_checkout_entries SET relative_path=? WHERE checkout_id=? AND file_id=?`, entries[0].Path, c.ID, entries[1].FileID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path)), []byte("edited"), 0o600))
	c, op, err := s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	verificationErr := s.RunCommit(t.Context(), c)
	for _, file := range []store.PhotoFile{first.Files[0], second.Files[0]} {
		_, total, err := s.Store.ContentVersions(t.Context(), file.NodeID, 100, 0)
		require.NoError(t, err)
		assert.Equal(t, 1, total, "ambiguous working paths must leave photo node %d unchanged", file.NodeID)
	}
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationFailed, finished.State)
	require.ErrorIs(t, verificationErr, store.ErrInvalidPhotoCheckout)
}

func TestCheckoutFailedCreationRebindsReleasedDestination(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Nanosecond
	first := checkoutPhoto(t, s, "a.jpg", "first")
	second := checkoutPhoto(t, s, "b.jpg", "second")
	c, _, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{first.ID, second.ID}}, t.TempDir(), 10000)
	require.NoError(t, err)
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	// A failed directory sync leaves the first published copy preserved.
	s.syncParent = func(*filepublish.RootStage) error { return os.ErrPermission }
	require.ErrorIs(t, s.Run(t.Context(), c, root), os.ErrPermission)
	require.NoError(t, root.Close())
	status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID, "", 100)
	require.NoError(t, err)
	require.Equal(t, "failed", status.Checkout.State)
	require.Equal(t, int64(1), status.PublishedFiles)
	destination := c.Destination + "-moved"
	require.NoError(t, os.Rename(c.Destination, destination))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(destination)) })
	require.NoError(t, os.Mkdir(c.Destination, 0o700))
	require.NoError(t, s.Store.ReleaseCheckoutDestination(t.Context(), c.ID))
	current, err := s.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.Empty(t, current.Destination)
	require.Equal(t, "failed", current.State)
	working := filepath.Join(destination, filepath.FromSlash(entries[0].Path))
	require.NoError(t, os.WriteFile(working, []byte("recovered edit"), 0o600))
	stamp := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(working, stamp, stamp))
	_, err = s.rebindCheckout(t.Context(), c.ID, destination)
	require.NoError(t, err)
}

func TestCheckoutPartialRebind(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	first := checkoutPhoto(t, s, "first.jpg", "first")
	second := checkoutPhoto(t, s, "second.jpg", "second")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{first.ID, second.ID}})
	var snapshot bytes.Buffer
	require.NoError(t, s.Store.ExportMetadata(t.Context(), &snapshot))
	restored := checkoutService(t)
	require.NoError(t, restored.Store.ImportMetadata(t.Context(), bytes.NewReader(snapshot.Bytes())))
	_, err := restored.rebindCheckout(t.Context(), c.ID, t.TempDir())
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	require.NoError(t, os.Remove(filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))))
	scan, err := restored.rebindCheckout(t.Context(), c.ID, c.Destination)
	require.NoError(t, err)
	missing := 0
	for _, entry := range scan.Entries {
		if entry.WorkingState == "missing" {
			missing++
		}
	}
	require.Equal(t, 1, missing)
	status, err := restored.Store.PhotoCheckoutStatus(t.Context(), c.ID, "", 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), status.PublishedFiles)
	rebound, err := restored.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", rebound.State)
	require.False(t, rebound.RetentionReleased)
}

func TestCheckoutCommitResumeAfterPublication(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	asset := checkoutPhoto(t, s, "photo.jpg", "original")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path)), []byte("edited"), 0o600))
	c, _, err := s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	// Cancel after the atomic version/receipt write, before the worker finalizes.
	originalMutate := s.Mutate
	s.Mutate = func(ctx context.Context, fn func() error) error {
		err := fn()
		current, readErr := s.Store.PhotoCheckoutEntries(context.WithoutCancel(ctx), c.ID)
		require.NoError(t, readErr)
		if current[0].VersionID != entries[0].VersionID {
			cancel()
		}
		return err
	}
	require.ErrorIs(t, s.RunCommit(ctx, c), context.Canceled)
	s.Mutate = originalMutate
	require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path)), []byte("second edit"), 0o600))
	require.ErrorIs(t, s.RunCommit(t.Context(), c), ingest.ErrSourceChanged)
	_, total, err := s.Store.ContentVersions(t.Context(), asset.Files[0].NodeID, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	op, err := s.Store.StorageOperation(t.Context(), c.LastOperationID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationFailed, op.State)
	require.Equal(t, int64(1), op.CopiedObjects)
	after, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "edited", after[0].WorkingState)
	working, err := os.ReadFile(filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path)))
	require.NoError(t, err)
	require.Equal(t, "second edit", string(working))
	hash, err := packstore.ParseHash("8e42daab3486377c2401dc3e6231edf263e51a59f5328cf025f8ace0eae3ffeb")
	require.NoError(t, err)
	_, err = s.Store.PhysicalContent(t.Context(), hash.String())
	require.ErrorIs(t, err, store.ErrNotFound)
	layout, err := packstore.NewLayout(filepath.Join(s.VaultRoot, "blobs"), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(t, err)
	_, err = os.Stat(layout.LoosePath(hash))
	require.ErrorIs(t, err, os.ErrNotExist, "resuming must not publish the second edit without version authority")
}

func TestCheckoutSidecarCommitKeepsNodeAndPairing(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	asset := checkoutPhoto(t, s, "photo.jpg", "original")
	hash, size, err := s.Blobs.Write(strings.NewReader("sidecar"))
	require.NoError(t, err)
	sidecar, err := s.Store.CreateFile(t.Context(), s.Store.RootID(), "photo.jpg.xmp", hash, size, "application/rdf+xml")
	require.NoError(t, err)
	target := asset.Files[0].ID
	asset, err = s.Store.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, store.PhotoRoleSidecar, &target)
	require.NoError(t, err)
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
	var paired store.PhotoCheckoutEntry
	for _, entry := range entries {
		if entry.SidecarOfID != nil {
			paired = entry
		}
	}
	require.NotEmpty(t, paired.FileID)
	require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(paired.Path)), []byte("edited sidecar"), 0o600))
	c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	require.NoError(t, s.RunCommit(t.Context(), c))
	updated, err := s.Store.PhotoAssetForNode(t.Context(), sidecar.ID)
	require.NoError(t, err)
	var matches []store.PhotoFile
	for _, file := range updated.Files {
		if file.ID == paired.FileID {
			matches = append(matches, file)
		}
	}
	require.Len(t, matches, 1)
	require.Equal(t, sidecar.ID, matches[0].NodeID)
	require.Equal(t, &target, matches[0].SidecarOfID)
	versions, total, err := s.Store.ContentVersions(t.Context(), sidecar.ID, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.NotEqual(t, paired.VersionID, versions[0].ID)
}

func TestCheckoutAliasSpellingStaysUntracked(t *testing.T) {
	t.Parallel()
	for _, hardlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("distinct Unicode spelling hardlink=%t", hardlink), func(t *testing.T) {
			if runtime.GOOS != "linux" {
				t.Skip("requires distinct NFC and NFD directory entries on Linux")
			}
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "café.jpg", "original")
			c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}})
			tracked := filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
			extraRef := strings.ReplaceAll(entries[0].Path, "é", "e\u0301")
			require.NotEqual(t, entries[0].Path, extraRef)
			extra := filepath.Join(c.Destination, filepath.FromSlash(extraRef))
			if hardlink {
				require.NoError(t, os.Link(tracked, extra))
			} else {
				require.NoError(t, os.WriteFile(extra, []byte("different bytes"), 0o600))
			}
			result, err := s.scanWorking(t.Context(), c, nil, nil, scanHooks{})
			require.NoError(t, err)
			require.Equal(t, []string{extraRef}, result.Untracked)
			_, err = s.rebindCheckout(t.Context(), c.ID, c.Destination)
			require.NoError(t, err)
			c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
			require.NoError(t, err)
			require.NoError(t, s.RunCommit(t.Context(), c))
			_, total, err := s.Store.ContentVersions(t.Context(), a.Files[0].NodeID, 100, 0)
			require.NoError(t, err)
			require.Equal(t, 1, total, "the extra spelling must never publish to the tracked node")
		})
	}
}

func TestCheckoutCaseOnlyRenameStaysMissingAndUntracked(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	asset := checkoutPhoto(t, s, "photo.jpg", "original")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
	entry := entries[0]
	original := filepath.Join(c.Destination, filepath.FromSlash(entry.Path))
	renamedRef := strings.TrimSuffix(entry.Path, "photo.jpg") + "PHOTO.jpg"
	renamed := filepath.Join(c.Destination, filepath.FromSlash(renamedRef))
	require.NoError(t, os.Rename(original, renamed))
	info, err := os.Lstat(original)
	if os.IsNotExist(err) {
		t.Skip("requires a case-insensitive filesystem")
	}
	require.NoError(t, err)
	renamedInfo, err := os.Lstat(renamed)
	require.NoError(t, err)
	require.True(t, os.SameFile(info, renamedInfo))

	result, err := s.scanWorking(t.Context(), c, nil, nil, scanHooks{})
	require.NoError(t, err)
	require.Equal(t, []string{renamedRef}, result.Untracked)
	require.EqualValues(t, 1, result.Results["missing"])
	c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	require.NoError(t, s.RunCommit(t.Context(), c))
	contents, err := os.ReadFile(renamed)
	require.NoError(t, err)
	require.Equal(t, "original", string(contents))
	_, total, err := s.Store.ContentVersions(t.Context(), asset.Files[0].NodeID, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
}

func TestLookupTrackedEntry(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	f, err := root.Create("cafe\u0301.jpg")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	listing, err := os.ReadDir(root.Name())
	require.NoError(t, err)
	entry := store.PhotoCheckoutEntry{Path: "café.jpg", FileID: "tracked"}
	for _, tc := range []struct {
		name      string
		ambiguous bool
		want      bool
	}{
		{"rewritten spelling", false, true},
		{"ambiguous key", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := entry
			if tc.ambiguous {
				candidate = store.PhotoCheckoutEntry{}
			}
			got, ok := lookupTrackedEntry(map[string]store.PhotoCheckoutEntry{entry.Path: entry}, map[string]store.PhotoCheckoutEntry{entry.Path: candidate}, "cafe\u0301.jpg", func(name string) (os.FileInfo, error) {
				require.Contains(t, []string{"café.jpg", "cafe\u0301.jpg"}, name)
				return root.Lstat("cafe\u0301.jpg")
			}, func(parent string) ([]os.DirEntry, error) {
				require.Equal(t, ".", parent)
				return listing, nil
			})
			require.Equal(t, tc.want, ok)
			if tc.want {
				require.Equal(t, entry, got)
			}
		})
	}
}

func TestCheckoutScanDetectsByteAndIdentityChanges(t *testing.T) {
	t.Parallel()
	for _, replaced := range []bool{false, true} {
		t.Run(strconv.FormatBool(replaced), func(t *testing.T) {
			s := checkoutService(t)
			s.settleTime = time.Nanosecond
			if replaced {
				s.settleTime = time.Hour
			}
			asset := checkoutPhoto(t, s, "photo.jpg", "original")
			c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
			name := filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
			if replaced {
				require.NoError(t, os.WriteFile(name, []byte("edited"), 0o600))
				scan, err := s.scanCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, "pending", scan.Entries[0].WorkingState)
				oldIdentity := scan.Entries[0].ObservedIdentity
				require.NotEmpty(t, oldIdentity)
				oldSince, err := time.Parse(time.RFC3339Nano, scan.Entries[0].ObservedSince)
				require.NoError(t, err)
				// Let the first observation settle before replacing the file.
				s.settleTime = time.Nanosecond
				require.GreaterOrEqual(t, time.Since(oldSince), s.settlement())
				before, err := os.Stat(name)
				require.NoError(t, err)
				replacement := filepath.Join(c.Destination, "replacement")
				require.NoError(t, os.WriteFile(replacement, []byte("edited"), 0o600))
				require.NoError(t, os.Chtimes(replacement, before.ModTime(), before.ModTime()))
				require.NoError(t, os.Remove(name))
				require.NoError(t, os.Rename(replacement, name))
				scan, err = s.scanCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, "pending", scan.Entries[0].WorkingState)
				require.NotEqual(t, oldIdentity, scan.Entries[0].ObservedIdentity)
				since, err := time.Parse(time.RFC3339Nano, scan.Entries[0].ObservedSince)
				require.NoError(t, err)
				require.True(t, since.After(oldSince), "replacement must restart the settlement window")
			} else {
				before, err := os.Stat(name)
				require.NoError(t, err)
				scan, err := s.scanCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, "clean", scan.Entries[0].WorkingState)
				identity := scan.Entries[0].ObservedIdentity

				require.NoError(t, os.WriteFile(name, []byte("edited!!"), 0o600))
				require.NoError(t, os.Chtimes(name, before.ModTime(), before.ModTime()))
				after, err := os.Stat(name)
				require.NoError(t, err)
				require.Equal(t, before.Size(), after.Size())
				require.Equal(t, before.ModTime(), after.ModTime())
				_, err = s.scanCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				scan, err = s.scanCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, identity, scan.Entries[0].ObservedIdentity)
				require.Equal(t, "edited", scan.Entries[0].WorkingState)
				require.Equal(t, "d28440923548a6cb86eb3206ec83150cfebd15b1bc4df0e5c74080ece15945b1", scan.Entries[0].ObservedHash)
			}
		})
	}
}

func TestCheckoutConflictsAllowIndependentCommits(t *testing.T) {
	t.Parallel()
	for _, edited := range []bool{false, true} {
		for _, change := range []string{"head", "detached", "trashed"} {
			t.Run(fmt.Sprintf("%s/edited=%t", change, edited), func(t *testing.T) {
				t.Parallel()
				s := checkoutService(t)
				s.settleTime = time.Millisecond
				first := checkoutPhoto(t, s, "a.jpg", "first")
				second := checkoutPhoto(t, s, "z.jpg", "second")
				if first.ID > second.ID {
					first, second = second, first
				}
				c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{first.ID, second.ID}})
				hash, size, err := s.Blobs.Write(strings.NewReader("external"))
				require.NoError(t, err)
				switch change {
				case "head":
					_, _, err = s.Store.ReplaceContent(t.Context(), first.Files[0].NodeID, store.UnconditionalRev, hash, size, "image/jpeg")
				case "trashed":
					_, _, err = s.Store.Trash(t.Context(), first.Files[0].NodeID, store.UnconditionalRev)
				case "detached":
					_, err = s.Store.DetachPhotoFile(t.Context(), first.ID, first.Revision, first.Files[0].ID, store.PhotoDetachOptions{})
				}
				require.NoError(t, err)
				for _, e := range entries {
					if edited || e.AssetID != first.ID {
						content := "edited"
						if e.AssetID == first.ID {
							content = "working edit"
						}
						require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(e.Path)), []byte(content), 0o600))
					}
				}
				for range 2 {
					c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
					require.NoError(t, err)
					err = s.RunCommit(t.Context(), c)
					op, readErr := s.Store.StorageOperation(t.Context(), c.LastOperationID)
					require.NoError(t, readErr)
					require.Equal(t, int64(2), op.CompletedObjects)
					var receipt ScanResult
					require.NoError(t, json.Unmarshal([]byte(op.ReceiptJSON), &receipt))
					require.Equal(t, int64(2), receipt.Files)
					state := "clean"
					if edited {
						state = "conflict"
						require.ErrorIs(t, err, store.ErrStaleRevision)
						require.ErrorContains(t, err, entries[0].Path)
						status, statusErr := s.Store.PhotoCheckoutStatus(t.Context(), c.ID, "", 100)
						require.NoError(t, statusErr)
						require.Equal(t, "completed", status.Checkout.State)
						require.Equal(t, err.Error(), status.Checkout.Error)
						require.Equal(t, "failed", status.Checkout.OperationState)
					} else {
						require.NoError(t, err)
					}
					scan, err := s.scanCheckout(t.Context(), c.ID)
					require.NoError(t, err)
					var conflicted store.PhotoCheckoutEntry
					for _, entry := range scan.Entries {
						if entry.AssetID == first.ID {
							conflicted = entry
						}
					}
					require.Equal(t, state, conflicted.WorkingState)
					if change == "head" && edited {
						require.Equal(t, entries[0].VersionID, conflicted.VersionID)
						versions, total, err := s.Store.ContentVersions(t.Context(), first.Files[0].NodeID, 100, 0)
						require.NoError(t, err)
						require.Equal(t, 2, total)
						require.Equal(t, hash, versions[0].BlobHash)
						inventory, err := s.Blobs.ListDetailed()
						require.NoError(t, err)
						require.NotContains(t, inventory, "578fc60ccde2943065b5e1c589a611e716e082589afe62ffc78a99201afe1152")
					}
					_, total, err := s.Store.ContentVersions(t.Context(), second.Files[0].NodeID, 100, 0)
					require.NoError(t, err)
					require.Equal(t, 2, total)
				}
			})
		}
	}
}

func TestCheckoutPartialPrunedHistorySurvivesCommitAndRebind(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	first := checkoutPhoto(t, s, "present.jpg", "first")
	second := checkoutPhoto(t, s, "pruned.jpg", "second")
	third := checkoutPhoto(t, s, "missing.jpg", "third")
	c, op, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{first.ID, second.ID, third.ID}}, t.TempDir(), 10000)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	require.NoError(t, s.Store.ClaimPhotoCheckout(t.Context(), c.ID, op.ID))
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	var present, pruned, recovered store.PhotoCheckoutEntry
	for _, e := range entries {
		switch e.AssetID {
		case first.ID:
			present = e
		case second.ID:
			pruned = e
		case third.ID:
			recovered = e
		}
	}
	working := filepath.Join(c.Destination, filepath.FromSlash(present.Path))
	require.NoError(t, os.MkdirAll(filepath.Dir(working), 0o700))
	require.NoError(t, s.Store.PublishPhotoCheckoutEntry(t.Context(), present, func() (bool, error) { return true, os.WriteFile(working, []byte("first"), 0o600) }))
	require.NoError(t, s.Store.FinishPhotoCheckout(t.Context(), c.ID, op.ID, store.StorageOperationFailed, "{}", "partial", time.Now().Add(time.Hour)))
	hash, size, err := s.Blobs.Write(strings.NewReader("replacement"))
	require.NoError(t, err)
	_, _, err = s.Store.ReplaceContent(t.Context(), second.Files[0].NodeID, store.UnconditionalRev, hash, size, "image/jpeg")
	require.NoError(t, err)
	result, err := s.Store.PruneContentVersions(t.Context(), second.Files[0].NodeID, store.UnconditionalRev, store.VersionPruneSelector{VersionIDs: []string{pruned.VersionID}}, true)
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedVersions)
	_, err = s.rebindCheckout(t.Context(), c.ID, c.Destination)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(working, []byte("edited"), 0o600))
	c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	require.True(t, c.DestinationReserved)
	require.True(t, c.RetentionReleased)
	require.NoError(t, s.RunCommit(t.Context(), c))
	var backup bytes.Buffer
	require.NoError(t, s.Store.ExportMetadata(t.Context(), &backup))
	restored := checkoutService(t)
	require.NoError(t, restored.Store.ImportMetadata(t.Context(), bytes.NewReader(backup.Bytes())))
	// A pruned base remains a conflict while independent files stay editable.
	oldPath := filepath.Join(c.Destination, filepath.FromSlash(pruned.Path))
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o700))
	require.NoError(t, os.WriteFile(oldPath, []byte("pruned edit"), 0o600))
	recoveredPath := filepath.Join(c.Destination, filepath.FromSlash(recovered.Path))
	require.NoError(t, os.MkdirAll(filepath.Dir(recoveredPath), 0o700))
	require.NoError(t, os.WriteFile(recoveredPath, []byte("third"), 0o600))
	scan, err := restored.rebindCheckout(t.Context(), c.ID, c.Destination)
	require.NoError(t, err)
	for _, entry := range scan.Entries {
		if entry.FileID == pruned.FileID {
			require.Equal(t, "conflict", entry.WorkingState)
		}
	}
	hash, size, err = restored.Blobs.Write(strings.NewReader("third replacement"))
	require.NoError(t, err)
	_, _, err = restored.Store.ReplaceContent(t.Context(), third.Files[0].NodeID, store.UnconditionalRev, hash, size, "image/jpeg")
	require.NoError(t, err)
	result, err = restored.Store.PruneContentVersions(t.Context(), third.Files[0].NodeID, store.UnconditionalRev, store.VersionPruneSelector{VersionIDs: []string{recovered.VersionID}}, true)
	require.NoError(t, err)
	require.Zero(t, result.DeletedVersions)
	require.Len(t, result.DependencyRetained, 1)
	require.Equal(t, recovered.VersionID, result.DependencyRetained[0].ID)
	require.NoError(t, os.Remove(recoveredPath))
	scan, err = restored.rebindCheckout(t.Context(), c.ID, c.Destination)
	require.NoError(t, err)
	for _, entry := range scan.Entries {
		if entry.FileID == recovered.FileID {
			require.Equal(t, "missing", entry.WorkingState)
		}
	}
	result, err = restored.Store.PruneContentVersions(t.Context(), third.Files[0].NodeID, store.UnconditionalRev, store.VersionPruneSelector{VersionIDs: []string{recovered.VersionID}}, true)
	require.NoError(t, err)
	require.Zero(t, result.DeletedVersions)
	require.Len(t, result.DependencyRetained, 1)
	require.Equal(t, recovered.VersionID, result.DependencyRetained[0].ID)
	require.NoError(t, os.WriteFile(working, []byte("another edit"), 0o600))
	c, _, err = restored.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	err = restored.RunCommit(t.Context(), c)
	require.ErrorIs(t, err, store.ErrStaleRevision)
	require.ErrorContains(t, err, pruned.Path)
	_, total, err := restored.Store.ContentVersions(t.Context(), first.Files[0].NodeID, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 3, total)
}

func TestCheckoutInspectionRefusesIneligibleCheckoutBeforeOpeningRoot(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	asset := checkoutPhoto(t, s, "photo.jpg", "original")
	c, _, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), 10000)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	require.NoError(t, os.Remove(c.Destination))
	_, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	require.ErrorContains(t, err, "before commit")
}

func TestCheckoutCommitAdmissionStopsBeforeRetryFiles(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	a := checkoutPhoto(t, s, "a.jpg", "first")
	b := checkoutPhoto(t, s, "b.jpg", "second")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{a.ID, b.ID}})
	edit := "edited"
	for _, entry := range entries {
		require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(entry.Path)), []byte(edit), 0o600))
	}
	c, _, err := s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	calls := 0
	s.admit = func(ctx context.Context, metadata *store.Store, id string) (bool, error) {
		calls++
		if calls == 2 {
			if _, err := metadata.SetLaneControl(ctx, store.LaneControl{Lane: "photo_checkout", Paused: true, Concurrency: 1}, 1); err != nil {
				return false, err
			}
		}
		observed, err := metadata.PhotoCheckoutEntries(ctx, c.ID)
		if err != nil {
			return false, err
		}
		if len(observed) == 2 && observed[0].ObservedHash != "" && observed[1].ObservedHash != "" {
			if err := metadata.RequestStorageOperationCancel(ctx, id); err != nil {
				return false, err
			}
		}
		return jobs.AdmitStorageOperation(ctx, metadata, id)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	finished := make(chan struct{})
	var runErr error
	go func() { defer close(finished); runErr = s.RunCommit(ctx, c) }()
	t.Cleanup(func() { cancel(); <-finished })
	// Hold the pause past a checkpoint interval to exercise admission during
	// the first pass as well as admission before the retry.
	require.Eventually(t, func() bool {
		op, err := s.Store.StorageOperation(ctx, c.LastOperationID)
		control, controlErr := s.Store.LaneControl(ctx, "photo_checkout")
		return err == nil && controlErr == nil && control.Paused && op.State == store.StorageOperationQueued
	}, 30*time.Second, 2*checkoutCheckpointInterval)
	paused, err := s.Store.PhotoCheckoutEntries(ctx, c.ID)
	require.NoError(t, err)
	require.Empty(t, paused[0].ObservedHash, "pause must precede the first digest")
	_, err = s.Store.SetLaneControl(ctx, store.LaneControl{Lane: "photo_checkout", Concurrency: 1}, 2)
	require.NoError(t, err)
	<-finished
	require.NoError(t, runErr)
	op, err := s.Store.StorageOperation(t.Context(), c.LastOperationID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationCancelled, op.State)
	after, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.NotEmpty(t, after[0].ObservedHash)
	require.NotEmpty(t, after[1].ObservedHash, "the first scan records both files before the retry checkpoint")
	require.NotEqual(t, "missing", after[1].WorkingState)
	for _, asset := range []store.PhotoAsset{a, b} {
		_, total, err := s.Store.ContentVersions(t.Context(), asset.Files[0].NodeID, 100, 0)
		require.NoError(t, err)
		require.Equal(t, 1, total, "cancelled verification must not create a version")
	}
}

func TestCheckoutBoundRebindRecoversIdentity(t *testing.T) {
	t.Parallel()
	for _, previous := range []string{"nonempty", "empty", "missing", "unbound restore", "refilled", "path", "ancestor"} {
		t.Run(previous, func(t *testing.T) {
			if (previous == "path" || previous == "ancestor") && runtime.GOOS == "windows" {
				t.Skip("requires Unix replaced-path handling")
			}
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "original")
			parent := filepath.Join(t.TempDir(), "working")
			original := filepath.Join(parent, "checkout")
			require.NoError(t, os.MkdirAll(original, 0o700))
			c, _, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, original, 100)
			require.NoError(t, err)
			require.NoError(t, s.Run(t.Context(), c, root))
			require.NoError(t, root.Close())
			entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			destination := t.TempDir()
			path := filepath.Join(destination, filepath.FromSlash(entries[0].Path))
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
			replaced := ""
			if previous == "path" || previous == "ancestor" {
				replaced = c.Destination
				if previous == "ancestor" {
					replaced = filepath.Dir(c.Destination)
				}
				require.NoError(t, os.Rename(replaced, replaced+"-moved"))
				require.NoError(t, os.WriteFile(replaced, []byte("replacement"), 0o600))
			}
			if previous == "empty" || previous == "missing" || previous == "refilled" {
				require.NoError(t, os.RemoveAll(c.Destination))
				if previous == "empty" || previous == "refilled" {
					require.NoError(t, os.Mkdir(c.Destination, 0o700))
				}
			}
			if previous == "unbound restore" {
				var snapshot bytes.Buffer
				require.NoError(t, s.Store.ExportMetadata(t.Context(), &snapshot))
				s = checkoutService(t)
				require.NoError(t, s.Store.ImportMetadata(t.Context(), bytes.NewReader(snapshot.Bytes())))
			}
			mutations := 0
			s.Mutate = func(_ context.Context, fn func() error) error {
				mutations++
				if previous == "refilled" && mutations == 2 {
					if err := os.WriteFile(filepath.Join(c.Destination, "new-copy.jpg"), []byte("copy"), 0o600); err != nil {
						return err
					}
				}
				return fn()
			}
			scan, err := s.rebindCheckout(t.Context(), c.ID, destination)
			if previous == "nonempty" || previous == "refilled" {
				if previous == "nonempty" {
					require.Zero(t, mutations, "the previous-folder check must precede admission's mutation gate")
				} else {
					require.Equal(t, 2, mutations, "the previous-folder check must precede the final bind gate")
				}
				require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
				require.ErrorContains(t, err, "clear or release the previous folder first")
				current, err := s.Store.PhotoCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, c.Destination, current.Destination)
			} else {
				require.NoError(t, err)
				current, err := s.Store.PhotoCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				canonical, err := filepath.EvalSymlinks(destination)
				require.NoError(t, err)
				require.Equal(t, canonical, current.Destination)
				if replaced != "" {
					require.Len(t, scan.Entries, 1)
					require.Equal(t, "clean", scan.Entries[0].WorkingState)
					data, err := os.ReadFile(replaced)
					require.NoError(t, err)
					require.Equal(t, "replacement", string(data))
				}
			}
		})
	}
	t.Run("replaced directory identity", func(t *testing.T) {
		s := checkoutService(t)
		a := checkoutPhoto(t, s, "photo.jpg", "original")
		c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}})
		require.NoError(t, os.Rename(c.Destination, c.Destination+"-old"))
		require.NoError(t, os.Mkdir(c.Destination, 0o700))
		path := filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
		_, err := s.scanCheckout(t.Context(), c.ID)
		require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
		scan, err := s.rebindCheckout(t.Context(), c.ID, c.Destination)
		require.NoError(t, err)
		require.Equal(t, "clean", scan.Entries[0].WorkingState)
		c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
		require.NoError(t, err)
		require.NoError(t, s.RunCommit(t.Context(), c))
	})
}

func TestCheckoutCommitBoundsChangingFilesAndRetires(t *testing.T) {
	t.Parallel()
	for _, keepsChanging := range []bool{false, true} {
		t.Run(fmt.Sprintf("keeps changing=%t", keepsChanging), func(t *testing.T) {
			s := checkoutService(t)
			s.settleTime = 10 * time.Millisecond
			a := checkoutPhoto(t, s, "a.jpg", "original first")
			b := checkoutPhoto(t, s, "b.jpg", "original second")
			c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{a.ID, b.ID}})
			pending, stable := entries[0], entries[1]
			pendingPath := filepath.Join(c.Destination, filepath.FromSlash(pending.Path))
			stablePath := filepath.Join(c.Destination, filepath.FromSlash(stable.Path))
			require.NoError(t, os.WriteFile(pendingPath, []byte("first"), 0o600))
			require.NoError(t, os.WriteFile(stablePath, []byte("second"), 0o600))
			stamp := time.Unix(1_700_000_000, 0)
			require.NoError(t, os.Chtimes(pendingPath, stamp, stamp))
			require.NoError(t, os.Chtimes(stablePath, stamp, stamp))
			_, err := s.scanCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			observed, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			// The stable file has already settled before this commit starts.
			for _, entry := range observed {
				if entry.FileID == stable.FileID {
					require.NoError(t, s.Store.ObservePhotoCheckoutEntries(t.Context(), c.ID, c.DestinationIdentity, []store.PhotoCheckoutObservation{{FileID: entry.FileID, Identity: entry.ObservedIdentity, Hash: entry.ObservedHash, Size: entry.ObservedSize, Mtime: entry.ObservedMtime, Since: stamp.Format(time.RFC3339Nano), State: entry.WorkingState}}))
				}
			}
			require.NoError(t, os.WriteFile(pendingPath, []byte("new first"), 0o600))
			require.NoError(t, os.Chtimes(pendingPath, stamp, stamp))
			c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
			require.NoError(t, err)
			calls, changed := 0, false
			s.admit = func(ctx context.Context, _ *store.Store, id string) (bool, error) {
				calls++
				if calls == 2 {
					_, err := s.Store.RetirePhotoCheckout(ctx, c.ID)
					require.NoError(t, err)
				}
				if keepsChanging {
					require.NoError(t, os.WriteFile(pendingPath, []byte(strconv.Itoa(calls)), 0o600))
					require.NoError(t, os.Chtimes(pendingPath, stamp, stamp))
				} else if calls == 3 {
					changed = true
					require.NoError(t, os.WriteFile(stablePath, []byte("latest"), 0o600))
					require.NoError(t, os.Chtimes(stablePath, stamp, stamp))
				}
				return false, nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			err = s.RunCommit(ctx, c)
			if keepsChanging {
				require.ErrorContains(t, err, "pending")
				require.ErrorContains(t, err, pending.Path)
			} else {
				require.NoError(t, err)
				op, err := s.Store.StorageOperation(t.Context(), c.LastOperationID)
				require.NoError(t, err)
				var receipt ScanResult
				require.NoError(t, json.Unmarshal([]byte(op.ReceiptJSON), &receipt))
				require.Equal(t, int64(24), receipt.HashedBytes, "only the pending nine-byte file needs a second hash")
				require.Equal(t, receipt.HashedBytes, receipt.TotalBytes)
				require.True(t, changed)
			}
			require.NoError(t, ctx.Err())
			status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID, "", 100)
			require.NoError(t, err)
			require.NotNil(t, status.Checkout.RetiredAt)
			for _, e := range status.Entries {
				if e.FileID == stable.FileID {
					require.Equal(t, "clean", e.WorkingState)
				}
			}
			nodeID := a.Files[0].NodeID
			if stable.AssetID == b.ID {
				nodeID = b.Files[0].NodeID
			}
			_, total, err := s.Store.ContentVersions(t.Context(), nodeID, 100, 0)
			require.NoError(t, err)
			require.Equal(t, 2, total)
		})
	}
	t.Run("mtime-only interruption", func(t *testing.T) {
		s := checkoutService(t)
		s.settleTime = time.Millisecond
		asset := checkoutPhoto(t, s, "photo.jpg", "original")
		c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
		path := filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
		c, op, err := s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
		require.NoError(t, err)
		changed := false
		s.Mutate = func(_ context.Context, fn func() error) error {
			if !changed {
				changed = true
				stamp := time.Now().Add(time.Hour)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					return err
				}
			}
			return fn()
		}
		require.NoError(t, s.RunCommit(t.Context(), c))
		require.True(t, changed)
		finished, err := s.Store.StorageOperation(t.Context(), op.ID)
		require.NoError(t, err)
		var receipt ScanResult
		require.NoError(t, json.Unmarshal([]byte(finished.ReceiptJSON), &receipt))
		require.Equal(t, int64(1), receipt.Files)
		require.Equal(t, int64(16), receipt.HashedBytes)
		require.Equal(t, int64(1), finished.CompletedObjects)
	})
}

func TestCheckoutCommitCleansPackedDuplicate(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	a := checkoutPhoto(t, s, "original.jpg", "original")
	duplicate := checkoutPhoto(t, s, "duplicate.jpg", strings.Repeat("packed edited bytes", 512))
	packed, err := s.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Positive(t, packed.BlobsPacked)
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}})
	require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path)), []byte(strings.Repeat("packed edited bytes", 512)), 0o600))
	c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	require.NoError(t, s.RunCommit(t.Context(), c))
	node, err := s.Store.NodeByID(t.Context(), duplicate.Files[0].NodeID)
	require.NoError(t, err)
	loose, err := s.Blobs.List()
	require.NoError(t, err)
	require.NotContains(t, loose, node.BlobHash)
}

func (s Service) scanCheckout(ctx context.Context, id string) (store.PhotoCheckoutStatus, error) {
	c, err := s.Store.PhotoCheckout(ctx, id)
	if err != nil {
		return store.PhotoCheckoutStatus{}, err
	}
	return s.scan(ctx, c, nil)
}

func (s Service) rebindCheckout(ctx context.Context, id, destination string) (store.PhotoCheckoutStatus, error) {
	kind := store.StorageOperationKindPhotoCheckoutRebind
	c, _, err := s.PrepareOperation(ctx, id, kind, destination)
	if err != nil {
		return store.PhotoCheckoutStatus{}, err
	}
	if err := s.RunInspection(ctx, c); err != nil {
		return store.PhotoCheckoutStatus{}, err
	}
	return s.Store.PhotoCheckoutStatus(ctx, id, "", 100)
}

func (s Service) scan(ctx context.Context, c store.PhotoCheckout, process func(context.Context, ingest.FolderFile, store.PhotoCheckoutEntry) error) (store.PhotoCheckoutStatus, error) {
	_, err := s.scanWorking(ctx, c, process, nil, scanHooks{})
	if err != nil {
		return store.PhotoCheckoutStatus{}, err
	}
	return s.Store.PhotoCheckoutStatus(ctx, c.ID, "", 100)
}

func TestCheckoutUnreadableFiles(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix file permissions")
	}
	s := checkoutService(t)
	asset := checkoutPhoto(t, s, "photo.jpg", "original")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
	untracked := filepath.Join(c.Destination, "untracked.jpg")
	require.NoError(t, os.WriteFile(untracked, []byte("untracked"), 0o600))
	require.NoError(t, os.Chmod(untracked, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(untracked, 0o600)) })
	_, err := os.ReadFile(untracked)
	if err == nil {
		t.Skip("file permissions do not prevent reads")
	}
	require.ErrorIs(t, err, os.ErrPermission)
	result, err := s.scanWorking(t.Context(), c, nil, nil, scanHooks{})
	require.NoError(t, err)
	require.Equal(t, int64(1), result.Results["untracked"])
	require.Equal(t, []string{"untracked.jpg"}, result.Untracked)
	tracked := filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
	require.NoError(t, os.Chmod(tracked, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(tracked, 0o600)) })
	c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	require.ErrorIs(t, s.RunCommit(t.Context(), c), os.ErrPermission)
}

func TestCheckoutAbortedScanKeepsItsObservation(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"non-regular", "excluded", "interrupted digest"} {
		t.Run(reason, func(t *testing.T) {
			if reason == "excluded" && runtime.GOOS == "windows" {
				t.Skip("requires symlink creation privilege")
			}
			s := checkoutService(t)
			asset := checkoutPhoto(t, s, "photo.jpg", "original")
			c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
			before, err := s.scanCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			name := filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
			message := "not a regular file"
			switch reason {
			case "excluded":
				folder := filepath.Dir(name)
				require.NoError(t, os.Rename(folder, folder+"-original"))
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "absent"), folder))
				message = "excluded by scanner"
			case "non-regular":
				require.NoError(t, os.Remove(name))
				require.NoError(t, os.Mkdir(name, 0o700))
			}
			if reason == "interrupted digest" {
				_, err = s.scanWorking(t.Context(), c, nil, nil, scanHooks{progress: func(_ context.Context, n int64, _ bool) error {
					if n > 0 {
						return errCommitCancelled
					}
					return nil
				}})
				require.ErrorIs(t, err, errCommitCancelled)
			} else {
				_, err = s.scanCheckout(t.Context(), c.ID)
				require.ErrorContains(t, err, message)
			}
			after, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, before.Entries, after, "an aborted scan must preserve settlement and working state")
		})
	}
}

func TestCheckoutCommitGroupsHiddenAndExcludedFilesAcrossRetry(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	first := checkoutPhoto(t, s, "hidden.jpg", "first")
	second := checkoutPhoto(t, s, "excluded.jpg", "second")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{first.ID, second.ID}})
	require.NoError(t, s.Store.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	_, err := s.Store.SetPhotoAssetHidden(t.Context(), first.ID, first.Revision, true)
	require.NoError(t, err)
	_, err = s.Store.SetPhotoAssetExcluded(t.Context(), second.ID, second.Revision, true)
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(e.Path)), []byte("edited"), 0600))
	}
	c, op, err := s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Mutate = func(ctx context.Context, fn func() error) error {
		collections, _, err := s.Store.Collections(ctx, 10, 0)
		if err != nil {
			return err
		}
		if len(collections) == 1 && collections[0].FileCount == 1 {
			cancel()
			return ctx.Err()
		}
		return fn()
	}
	require.ErrorIs(t, s.RunCommit(ctx, c), context.Canceled)
	s.Mutate = nil
	require.NoError(t, s.RunCommit(t.Context(), c))
	collections, total, err := s.Store.Collections(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, collections, 1)
	require.Equal(t, op.ID, collections[0].ID)
	require.Equal(t, int64(2), collections[0].FileCount)
	for _, e := range entries {
		committed, err := s.Store.CheckPhotoCheckoutCommitEntry(t.Context(), c, e)
		require.NoError(t, err)
		require.True(t, committed)
	}
	for _, asset := range []store.PhotoAsset{first, second} {
		_, total, err := s.Store.ContentVersions(t.Context(), asset.Files[0].NodeID, 10, 0)
		require.NoError(t, err)
		require.Equal(t, 2, total)
	}
}

func TestCheckoutInspectionBoundsCheckpointsAndKeepsFinalProgress(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	for i := range 130 {
		checkoutPhoto(t, s, fmt.Sprintf("photo-%03d.jpg", i), "x")
	}
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{All: true})
	require.Len(t, entries, 130)
	c, op, err := s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutScan, "")
	require.NoError(t, err)
	admissions, writes := 0, 0
	s.admit = func(ctx context.Context, catalog *store.Store, id string) (bool, error) {
		admissions++
		return jobs.AdmitStorageOperation(ctx, catalog, id)
	}
	s.Mutate = func(_ context.Context, fn func() error) error { writes++; return fn() }
	require.NoError(t, s.RunInspection(t.Context(), c))
	require.Less(t, admissions, 20, "admission reads should cover batches of small files")
	require.Less(t, writes, 20, "progress writes should cover batches of small files")
	t.Logf("130 files: %d admission checks, %d mutation calls", admissions, writes)
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, int64(130), finished.CompletedObjects)
	var receipt ScanResult
	require.NoError(t, json.Unmarshal([]byte(finished.ReceiptJSON), &receipt))
	require.Equal(t, int64(130), receipt.Files)
	require.Equal(t, int64(130), receipt.HashedBytes)
	require.Equal(t, int64(130), receipt.Results["clean"])
}

func TestCheckoutSourceChangeReplacesHashedObservation(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Nanosecond
	asset := checkoutPhoto(t, s, "photo.jpg", "original")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
	path := filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
	require.NoError(t, os.WriteFile(path, []byte("edited"), 0o600))
	for range 2 {
		_, err := s.scanCheckout(t.Context(), c.ID)
		require.NoError(t, err)
	}
	changed := false
	result, err := s.scanWorking(t.Context(), c, func(_ context.Context, file ingest.FolderFile, _ store.PhotoCheckoutEntry) error {
		changed = true
		if err := os.WriteFile(path, []byte("changed while saving"), 0o600); err != nil {
			return err
		}
		return file.Unchanged()
	}, nil, scanHooks{})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, int64(1), result.Results["pending"])
	current, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Len(t, current, 1)
	require.Empty(t, current[0].ObservedHash, "a changed source replaces the earlier digest observation")
}

func TestPhotoCheckoutCommitAfterAuditActivation(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Nanosecond
	asset := checkoutPhoto(t, s, "photo.jpg", "original")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
	plan, err := s.Store.PreviewInitialAudit(t.Context(), s.Store.RootID(), "cli", nil)
	require.NoError(t, err)
	_, err = s.Store.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path)), []byte("audited edit"), 0o600))
	c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	require.NoError(t, s.RunCommit(t.Context(), c))
	node, err := s.Store.NodeByID(t.Context(), asset.Files[0].NodeID)
	require.NoError(t, err)
	require.NotEqual(t, entries[0].VersionID, node.CurrentVersionID)
	after, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, node.CurrentVersionID, after[0].VersionID)
	require.Equal(t, node.BlobHash, after[0].Hash)
	recorded, err := s.Store.CheckPhotoCheckoutCommitEntry(t.Context(), c, entries[0])
	require.NoError(t, err)
	require.True(t, recorded)
	bindings, err := s.Store.ProvenanceVersionBindings(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	facts, err := s.Store.NodeProvenance(t.Context(), node.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, facts.Items, 1)
	require.Equal(t, facts.Items[0].Identity, bindings[0].ProvenanceIdentity)
	var snapshot bytes.Buffer
	require.NoError(t, s.Store.ExportMetadata(t.Context(), &snapshot))
	restored := checkoutService(t)
	require.NoError(t, restored.Store.ImportMetadata(t.Context(), bytes.NewReader(snapshot.Bytes())))
	restoredNode, err := restored.Store.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	require.Equal(t, node.CurrentVersionID, restoredNode.CurrentVersionID)
	restoredBindings, err := restored.Store.ProvenanceVersionBindings(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, bindings, restoredBindings)
}

func TestCheckoutCommitWithoutCaptureDateKeepsWorkingPath(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	hash, size, err := s.Blobs.Write(strings.NewReader("original"))
	require.NoError(t, err)
	run, err := s.Store.BeginIngest(t.Context(), "cli", "synthetic photo")
	require.NoError(t, err)
	imported, err := s.Store.IngestFileExact(t.Context(), run, s.Store.RootID(), "photo.jpg", hash, size, "image/jpeg", "photo.jpg", "2024-03-05T10:00:00Z")
	require.NoError(t, err)
	asset, err := s.Store.PhotoAssetForNode(t.Context(), imported.ID)
	require.NoError(t, err)
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
	original := entries[0]
	file := filepath.Join(c.Destination, filepath.FromSlash(original.Path))
	require.NoError(t, os.WriteFile(file, []byte("\x89PNG\r\n\x1a\nsynthetic image"), 0o600))
	mtime := time.Date(2025, 8, 9, 10, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(file, mtime, mtime))
	c, op, err := s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationKindPhotoCheckoutCommit, op.Kind)
	require.NoError(t, s.RunCommit(t.Context(), c))
	versions, total, err := s.Store.ContentVersions(t.Context(), asset.Files[0].NodeID, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.NotEqual(t, original.VersionID, versions[0].ID)
	node, err := s.Store.NodeByID(t.Context(), asset.Files[0].NodeID)
	require.NoError(t, err)
	require.Equal(t, "image/png", node.MimeType)
	expression, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	page, err := s.Store.ListPhotoAssets(t.Context(), store.PhotoBrowseRequest{Query: expression, PageSize: 10}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, new("2024-03-05T10:00:00Z"), page.Items[0].FileModifiedTime)
	estimate, err := s.Store.EstimatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{Years: &store.PhotoCheckoutYears{From: 2024, To: 2024}})
	require.NoError(t, err)
	require.Equal(t, int64(1), estimate.Assets)
	entries, err = s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, original.Path, entries[0].Path)
	require.FileExists(t, file)
	require.Equal(t, versions[0].ID, entries[0].VersionID)
	require.Equal(t, int64(23), entries[0].Size)
	require.Equal(t, "clean", entries[0].WorkingState)
	metadata, err := s.Store.MissingSourceMetadataTargetsAfter(t.Context(), "checkout-test-extractor", "", 100)
	require.NoError(t, err)
	require.Contains(t, metadata, store.SourceMetadataTarget{SourceSHA256: entries[0].Hash, Size: 23})
	previews, err := s.Store.MissingPhotoVisualPreviewTargetsAfter(t.Context(), "checkout-test-recipe", "", 100)
	require.NoError(t, err)
	require.Contains(t, previews, store.PhotoVisualPreviewTarget{VersionID: entries[0].VersionID, SourceSHA256: entries[0].Hash, Size: 23, MediaType: "image/png"})
	reader, size, err := s.Blobs.OpenStreamContext(t.Context(), entries[0].Hash)
	require.NoError(t, err)
	content, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, int64(23), size)
	require.Equal(t, "\x89PNG\r\n\x1a\nsynthetic image", string(content))
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, "@finalizing", finished.Cursor)
	require.Equal(t, int64(1), finished.CopiedObjects)
	require.Equal(t, int64(23), finished.CopiedBytes)
}

func TestCheckoutCommitAfterRevertKeepsModifiedTime(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	s.settleTime = time.Millisecond
	hash, size, err := s.Blobs.Write(strings.NewReader("original"))
	require.NoError(t, err)
	run, err := s.Store.BeginIngest(t.Context(), "cli", "synthetic photo")
	require.NoError(t, err)
	imported, err := s.Store.IngestFileExact(t.Context(), run, s.Store.RootID(), "photo.jpg", hash, size, "image/jpeg", "photo.jpg", "2024-03-05T10:00:00Z")
	require.NoError(t, err)
	hash, size, err = s.Blobs.Write(strings.NewReader("replacement"))
	require.NoError(t, err)
	replaced, _, err := s.Store.ReplaceContent(t.Context(), imported.ID, imported.Revision, hash, size, "image/jpeg")
	require.NoError(t, err)
	reverted, _, _, err := s.Store.RevertContent(t.Context(), imported.ID, replaced.Revision, imported.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, imported.ID, reverted.ID)
	asset, err := s.Store.PhotoAssetForNode(t.Context(), imported.ID)
	require.NoError(t, err)
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
	file := filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
	require.NoError(t, os.WriteFile(file, []byte("edited photo"), 0o600))
	c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	require.NoError(t, s.RunCommit(t.Context(), c))
	expression, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	page, err := s.Store.ListPhotoAssets(t.Context(), store.PhotoBrowseRequest{Query: expression, PageSize: 10}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, new("2024-03-05T10:00:00Z"), page.Items[0].FileModifiedTime)
	estimate, err := s.Store.EstimatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{Years: &store.PhotoCheckoutYears{From: 2024, To: 2024}})
	require.NoError(t, err)
	require.Equal(t, int64(1), estimate.Assets)
}

func TestCheckoutEditedRestoreRebindCommit(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	first := checkoutPhoto(t, s, "first.jpg", "first")
	second := checkoutPhoto(t, s, "second.jpg", "second")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{first.ID, second.ID}})
	var snapshot bytes.Buffer
	require.NoError(t, s.Store.ExportMetadata(t.Context(), &snapshot))
	for _, scenario := range []struct {
		name     string
		matching bool
		single   bool
	}{
		{"all edited", false, false},
		{"one matching base", true, false},
		{"single edited file", false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c, entries := c, entries
			snapshotData := snapshot.Bytes()
			if scenario.single {
				single, singleEntries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{first.ID}})
				c, entries = single, singleEntries
				var singleSnapshot bytes.Buffer
				require.NoError(t, s.Store.ExportMetadata(t.Context(), &singleSnapshot))
				snapshotData = singleSnapshot.Bytes()
			}
			restored := checkoutService(t)
			restored.settleTime = time.Nanosecond
			require.NoError(t, restored.Store.ImportMetadata(t.Context(), bytes.NewReader(snapshotData)))
			destination := t.TempDir()
			for i, entry := range entries {
				content := "recovered edit"
				if scenario.matching && i == 1 {
					if entry.AssetID == first.ID {
						content = "first"
					} else {
						content = "second"
					}
				}
				path := filepath.Join(destination, filepath.FromSlash(entry.Path))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			}
			c, op, err := restored.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutRebind, destination)
			require.NoError(t, err)
			require.NoError(t, restored.RunInspection(t.Context(), c))
			operation, err := restored.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			var receipt ScanResult
			require.NoError(t, json.Unmarshal([]byte(operation.ReceiptJSON), &receipt))
			wantCounts := map[string]int64{"pending": 2}
			if scenario.matching {
				wantCounts = map[string]int64{"pending": 1, "clean": 1}
			} else if scenario.single {
				wantCounts = map[string]int64{"pending": 1}
			}
			require.Equal(t, wantCounts, receipt.Results)
			page, err := restored.Store.PhotoCheckoutStatus(t.Context(), c.ID, "", 100)
			require.NoError(t, err)
			require.Equal(t, "completed", page.Checkout.State)
			scan, err := restored.scanCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			for _, entry := range scan.Entries {
				want := "edited"
				if scenario.matching && entry.FileID == entries[1].FileID {
					want = "clean"
				}
				require.Equal(t, want, entry.WorkingState)
			}
			for _, entry := range entries {
				asset, err := restored.Store.PhotoAssetByID(t.Context(), entry.AssetID)
				require.NoError(t, err)
				_, total, err := restored.Store.ContentVersions(t.Context(), asset.Files[0].NodeID, 100, 0)
				require.NoError(t, err)
				require.Equal(t, 1, total, "rebind preserves the edit without creating a version")
			}
			c, _, err = restored.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
			require.NoError(t, err)
			require.NoError(t, restored.RunCommit(t.Context(), c))
			committed, err := restored.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			for _, entry := range committed {
				base := entries[0]
				if !scenario.single && entry.FileID == entries[1].FileID {
					base = entries[1]
				}
				require.Equal(t, "clean", entry.WorkingState)
				if scenario.matching && entry.FileID == entries[1].FileID {
					require.Equal(t, base.VersionID, entry.VersionID)
				} else {
					require.NotEqual(t, base.VersionID, entry.VersionID)
					require.NotEqual(t, base.Hash, entry.Hash)
				}
			}
		})
	}
}

func TestCheckoutReimportAfterEditKeepsAsset(t *testing.T) {
	t.Parallel()
	for _, checkoutEdit := range []bool{true, false} {
		name := "replace-content"
		if checkoutEdit {
			name = "checkout-commit"
		}
		t.Run(name, func(t *testing.T) {
			s := checkoutService(t)
			s.settleTime = time.Millisecond
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("original photo"), 0o600))
			ing := ingest.Ingester{Store: s.Store, Blobs: s.Blobs}
			first, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", ingest.PhotoImportOptions{SettleInterval: time.Millisecond})
			require.NoError(t, err)
			require.Empty(t, first.Errors)
			require.Equal(t, int64(1), first.Receipt.Added)
			node, err := s.Store.NodeByPath(t.Context(), "/photos/photo.jpg")
			require.NoError(t, err)
			asset, err := s.Store.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			if checkoutEdit {
				c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
				require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path)), []byte("edited photo"), 0o600))
				c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
				require.NoError(t, err)
				require.NoError(t, s.RunCommit(t.Context(), c))
			} else {
				hash, size, err := s.Blobs.Write(strings.NewReader("edited photo"))
				require.NoError(t, err)
				_, _, err = s.Store.ReplaceContent(t.Context(), node.ID, node.Revision, hash, size, "image/jpeg")
				require.NoError(t, err)
			}
			before, err := s.Store.NodeByID(t.Context(), node.ID)
			require.NoError(t, err)
			second, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", ingest.PhotoImportOptions{SettleInterval: time.Millisecond})
			require.NoError(t, err)
			require.Empty(t, second.Errors)
			after, err := s.Store.NodeByID(t.Context(), node.ID)
			require.NoError(t, err)
			afterAsset, err := s.Store.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			expression, err := query.Parse([]byte(`{}`))
			require.NoError(t, err)
			page, err := s.Store.ListPhotoAssets(t.Context(), store.PhotoBrowseRequest{Query: expression, PageSize: 10}, nil)
			require.NoError(t, err)
			_, versions, err := s.Store.ContentVersions(t.Context(), node.ID, 100, 0)
			require.NoError(t, err)
			require.Len(t, page.Items, 1, "re-import created a duplicate asset")
			require.Equal(t, asset.ID, afterAsset.ID)
			require.NotEqual(t, before.CurrentVersionID, after.CurrentVersionID)
			require.Equal(t, node.BlobHash, after.BlobHash)
			require.Equal(t, 3, versions)
			require.Equal(t, int64(1), second.Receipt.Added)
			require.Zero(t, second.Receipt.Skipped)
		})
	}
}
