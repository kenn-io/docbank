package ingest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestAddRecoversMissingAuthorityWhenLaterInputPublishesFirst(t *testing.T) {
	for _, tc := range []struct {
		sourceChanged, replace, existing bool
	}{
		{false, false, false}, {true, false, false},
		{false, true, false}, {true, true, false},
		{false, true, true}, {true, true, true},
	} {
		t.Run(fmt.Sprintf("publisher_changed=%t/replace=%t/existing=%t",
			tc.sourceChanged, tc.replace, tc.existing), func(t *testing.T) {
			ing := newTestIngester(t)
			const content = "shared recovery content"
			source := writeTree(t, map[string]string{"first.txt": content, "later.txt": content})
			first, later := filepath.Join(source, "first.txt"), filepath.Join(source, "later.txt")
			seedDestination := "/seed"
			if tc.existing {
				seedDestination = "/recovered"
			}
			report, err := ing.AddPaths(t.Context(), []string{first}, seedDestination)
			require.NoError(t, err)
			require.Equal(t, 1, report.Added)
			packed, err := ing.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
			require.NoError(t, err)
			require.Equal(t, 1, packed.BlobsPacked)
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
			require.NoError(t, store.NewPackCatalog(ing.Store).DeleteIndexEntry(t.Context(), packstore.Hash(hash)))
			_, err = ing.Store.PhysicalContent(t.Context(), hash)
			require.ErrorIs(t, err, store.ErrPhysicalAuthorityMissing)
			loose, err := ing.Blobs.List()
			require.NoError(t, err)
			require.Empty(t, loose)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			published := make(chan struct{})
			var publishOnce sync.Once
			originalOpen, originalSync := openBatchSource, pack.SyncDir
			t.Cleanup(func() {
				openBatchSource, pack.SyncDir = originalOpen, originalSync
			})
			openBatchSource = func(path string) (*os.File, error) {
				if path == first {
					select {
					case <-published:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return originalOpen(path)
			}
			pack.SyncDir = func(dir string) error {
				if err := originalSync(dir); err != nil {
					return err
				}
				var changeErr error
				if filepath.Base(dir) == hash[:2] {
					publishOnce.Do(func() {
						// Release the first input only after the later input's
						// canonical loose file has been published and synced.
						if tc.sourceChanged {
							changeErr = os.WriteFile(later, []byte(content+" changed"), 0o600)
						}
						close(published)
					})
				}
				return changeErr
			}

			report, err = ing.AddPathsWithOptions(ctx, []string{first, later}, "/recovered", Options{
				Replace: tc.replace,
			})
			require.NoError(t, err)
			if tc.sourceChanged {
				require.Len(t, report.Failed, 2)
				require.ErrorIs(t, report.Failed[0].Err, store.ErrPhysicalAuthorityMissing)
				require.ErrorIs(t, report.Failed[1].Err, ErrSourceChanged)
				assert.Zero(t, report.Added)
				_, err = ing.Store.PhysicalContent(t.Context(), hash)
				require.ErrorIs(t, err, store.ErrPhysicalAuthorityMissing)
				return
			}
			require.Empty(t, report.Failed)
			if tc.existing {
				assert.Equal(t, 1, report.Added)
				assert.Equal(t, 1, report.Skipped)
			} else {
				assert.Equal(t, 2, report.Added)
			}
			for _, name := range []string{"first.txt", "later.txt"} {
				node, err := ing.Store.NodeByPath(t.Context(), "/recovered/"+name)
				require.NoError(t, err)
				assert.Equal(t, hash, node.BlobHash)
			}
			reader, err := ing.Blobs.OpenContext(t.Context(), hash)
			require.NoError(t, err)
			data, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			require.NoError(t, readErr)
			require.NoError(t, closeErr)
			assert.Equal(t, content, string(data))
		})
	}
}

func TestAddOverlapsDurableWritesBeforeMetadata(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		replace bool
	}{
		{"success", false}, {"sync failure", false}, {"cancel", false},
		{"success", true}, {"sync failure", true}, {"cancel", true},
	} {
		t.Run(fmt.Sprintf("%s/replace=%t", tc.outcome, tc.replace), func(t *testing.T) {
			ing := newTestIngester(t)
			// These contents use distinct publication stripes, so the test can
			// hold all 32 writers at the directory sync together.
			contents := map[string]string{}
			for i := range 33 {
				contents[fmt.Sprintf("%02d.txt", i)] = fmt.Sprintf("parallel import content %d", i)
			}
			source := writeTree(t, contents)
			if tc.replace {
				for name := range contents {
					require.NoError(t, os.WriteFile(filepath.Join(source, name), []byte("original "+name), 0o600))
				}
				seed, err := ing.AddPaths(t.Context(), []string{source}, "/inbox")
				require.NoError(t, err)
				require.Empty(t, seed.Failed)
				require.Equal(t, 33, seed.Added)
				for name, content := range contents {
					require.NoError(t, os.WriteFile(filepath.Join(source, name), []byte(content), 0o600))
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan struct{}, 32)
			release := make(chan struct{})
			var releaseOnce sync.Once
			var calls, active atomic.Int32
			syncErr := errors.New("injected publication sync failure")
			failedHash := fmt.Sprintf("%x", sha256.Sum256([]byte(contents["03.txt"])))
			originalSync := pack.SyncDir
			pack.SyncDir = func(dir string) error {
				if filepath.Base(dir) == "blobs" && calls.Add(1) <= 32 {
					active.Add(1)
					entered <- struct{}{}
					<-release
					active.Add(-1)
				}
				if tc.outcome == "sync failure" && filepath.Base(dir) == failedHash[:2] {
					return syncErr
				}
				return originalSync(dir)
			}
			t.Cleanup(func() { pack.SyncDir = originalSync })
			var report Report
			var importErr error
			var events []ProgressEvent
			done := make(chan struct{})
			go func() {
				defer close(done)
				report, importErr = ing.AddPathsWithOptions(ctx, []string{source}, "/inbox", Options{
					Replace:  tc.replace,
					Progress: func(event ProgressEvent) { events = append(events, event) },
				})
			}()
			t.Cleanup(func() {
				cancel()
				releaseOnce.Do(func() { close(release) })
				<-done
			})
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for i := range 32 {
				select {
				case <-entered:
				case <-deadline.C:
					t.Fatalf("only %d writes reached durable publication concurrently", i)
				}
			}
			parentPath := "/inbox/" + filepath.Base(source)
			parent, err := ing.Store.NodeByPath(t.Context(), parentPath)
			require.NoError(t, err)
			children, err := ing.Store.Children(t.Context(), parent.ID)
			require.NoError(t, err)
			if tc.replace {
				require.Len(t, children, 33)
				for _, node := range children {
					assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("original "+node.Name))), node.BlobHash)
				}
			} else {
				assert.Empty(t, children, "blocked publication must not grant metadata authority")
			}
			assert.EqualValues(t, 32, active.Load())
			if tc.outcome == "cancel" {
				cancel()
			}
			releaseOnce.Do(func() { close(release) })
			<-done
			assert.Zero(t, active.Load(), "the importer must join its writes before returning")
			for i := 1; i < len(events); i++ {
				assert.GreaterOrEqual(t, events[i].BytesRead, events[i-1].BytesRead)
				assert.GreaterOrEqual(t, events[i].FilesDone, events[i-1].FilesDone)
			}
			assert.Equal(t, tc.outcome != "cancel", events[len(events)-1].Final)
			children, err = ing.Store.Children(t.Context(), parent.ID)
			require.NoError(t, err)
			switch tc.outcome {
			case "cancel":
				require.ErrorIs(t, importErr, context.Canceled)
				if !tc.replace {
					assert.Empty(t, children)
				} else {
					require.Len(t, children, 33)
				}
			case "sync failure":
				require.NoError(t, importErr)
				require.Len(t, report.Failed, 1)
				assert.Equal(t, filepath.Join(source, "03.txt"), report.Failed[0].Path)
				require.ErrorIs(t, report.Failed[0].Err, syncErr)
				assert.Equal(t, 32, report.Added)
				if tc.replace {
					require.Len(t, children, 33)
				} else {
					require.Len(t, children, 32)
				}
				_, err = ing.Store.PhysicalContent(t.Context(), failedHash)
				require.ErrorIs(t, err, store.ErrNotFound)
			case "success":
				require.NoError(t, importErr)
				assert.Empty(t, report.Failed)
				assert.Equal(t, 33, report.Added)
				require.Len(t, children, 33)
			}
			for _, node := range children {
				reader, err := ing.Blobs.OpenContext(t.Context(), node.BlobHash)
				require.NoError(t, err)
				data, readErr := io.ReadAll(reader)
				closeErr := reader.Close()
				require.NoError(t, readErr)
				require.NoError(t, closeErr)
				expected, versionCount := contents[node.Name], 1
				if tc.replace {
					versionCount = 2
					if tc.outcome == "cancel" || tc.outcome == "sync failure" && node.Name == "03.txt" {
						expected, versionCount = "original "+node.Name, 1
					}
				}
				assert.Equal(t, expected, string(data))
				_, total, err := ing.Store.ContentVersions(t.Context(), node.ID, 10, 0)
				require.NoError(t, err)
				assert.Equal(t, versionCount, total)
			}
		})
	}
}

func TestAddCancellationCleansUncommittedPackedDuplicates(t *testing.T) {
	ing := newTestIngesterWithOptions(t, blob.ManagedOptions())
	contents := map[string]string{}
	for i := range 8 {
		contents[fmt.Sprintf("%02d.txt", i)] = fmt.Sprintf("packed content %d", i)
	}
	source := writeTree(t, contents)
	label := new("Reserved label")
	first, err := ing.AddPathsWithOptions(t.Context(), []string{source}, "/inbox", Options{
		CollectionLabel: label,
	})
	require.NoError(t, err)
	require.Equal(t, 8, first.Added)
	packed, err := ing.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 8, packed.BlobsPacked)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	report, err := ing.AddPathsWithOptions(ctx, []string{source}, "/new/inbox", Options{
		CollectionLabel: label,
		Progress: func(event ProgressEvent) {
			if event.Failed > 0 {
				cancel()
			}
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, report.Failed, 1)
	require.ErrorIs(t, report.Failed[0].Err, store.ErrExists)
	assert.Empty(t, report.IngestID)
	loose, err := ing.Blobs.List()
	require.NoError(t, err)
	assert.Empty(t, loose, "cancellation must clean the seven remaining prepared duplicates")
	_, err = ing.Store.NodeByPath(t.Context(), "/new")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAddKeepsFileAndDirectoryOrder(t *testing.T) {
	for _, labeled := range []bool{false, true} {
		t.Run(fmt.Sprintf("labeled=%t", labeled), func(t *testing.T) {
			ing := newTestIngester(t)
			first := writeTree(t, map[string]string{"same.txt": "first", "album": "file before directory"})
			second := writeTree(t, map[string]string{"same.txt": "second", "album/photo.txt": "later directory"})
			paths := []string{
				filepath.Join(first, "same.txt"), filepath.Join(second, "same.txt"),
				filepath.Join(first, "same.txt"), filepath.Join(first, "album"),
				filepath.Join(second, "album"),
			}
			opts := Options{}
			if labeled {
				opts.CollectionLabel = new("Ordered import")
			}
			report, err := ing.AddPathsWithOptions(t.Context(), paths, "/inbox", opts)
			require.NoError(t, err)
			assert.Equal(t, 3, report.Added)
			assert.Equal(t, 1, report.Skipped)
			require.Len(t, report.Failed, 1)
			assert.Equal(t, filepath.Join(second, "album"), report.Failed[0].Path)
			for name, content := range map[string]string{
				"same.txt": "first", "same (2).txt": "second", "album": "file before directory",
			} {
				node, err := ing.Store.NodeByPath(t.Context(), "/inbox/"+name)
				require.NoError(t, err)
				assert.False(t, node.IsDir())
				assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(content))), node.BlobHash)
			}
			_, err = ing.Store.NodeByPath(t.Context(), "/inbox/album/photo.txt")
			assert.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestAddReplaceKeepsNormalizedTargetOrder(t *testing.T) {
	for _, labeled := range []bool{false, true} {
		for _, seeded := range []bool{false, true} {
			t.Run(fmt.Sprintf("labeled=%t/seeded=%t", labeled, seeded), func(t *testing.T) {
				ing := newTestIngester(t)
				var paths []string
				for i, name := range []string{"caf\u00e9.txt", "cafe\u0301.txt", "caf\u00e9.txt"} {
					path := filepath.Join(t.TempDir(), name)
					require.NoError(t, os.WriteFile(path, []byte([]string{"first", "second", "third"}[i]), 0o600))
					paths = append(paths, path)
				}
				if seeded {
					seed := filepath.Join(t.TempDir(), "caf\u00e9.txt")
					require.NoError(t, os.WriteFile(seed, []byte("seed"), 0o600))
					report, err := ing.AddPaths(t.Context(), []string{seed}, "/inbox")
					require.NoError(t, err)
					require.Empty(t, report.Failed)
					require.Equal(t, 1, report.Added)
				}
				opts := Options{Replace: true}
				if labeled {
					opts.CollectionLabel = new("Replacement import")
				}
				paths = append(paths, paths[2])
				report, err := ing.AddPathsWithOptions(t.Context(), paths, "/inbox", opts)
				require.NoError(t, err)
				require.Empty(t, report.Failed)
				require.Equal(t, 3, report.Added)
				require.Equal(t, 1, report.Skipped)
				node, err := ing.Store.NodeByPath(t.Context(), "/inbox/caf\u00e9.txt")
				require.NoError(t, err)
				versions, total, err := ing.Store.ContentVersions(t.Context(), node.ID, 10, 0)
				require.NoError(t, err)
				expected := []string{"third", "second", "first"}
				if seeded {
					expected = append(expected, "seed")
				}
				require.Equal(t, len(expected), total)
				for i, version := range versions {
					reader, err := ing.Blobs.OpenContext(t.Context(), version.BlobHash)
					require.NoError(t, err)
					data, readErr := io.ReadAll(reader)
					closeErr := reader.Close()
					require.NoError(t, readErr)
					require.NoError(t, closeErr)
					require.Equal(t, expected[i], string(data))
				}
			})
		}
	}
}
