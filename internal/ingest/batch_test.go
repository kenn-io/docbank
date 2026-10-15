package ingest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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

func TestAddOverlapsDurableWritesBeforeMetadata(t *testing.T) {
	for _, outcome := range []string{"success", "sync failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			ing := newTestIngester(t)
			contents := map[string]string{}
			for i := range 9 {
				contents[fmt.Sprintf("%02d.txt", i)] = fmt.Sprintf("synthetic content %d", i)
			}
			source := writeTree(t, contents)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan struct{}, 8)
			release := make(chan struct{})
			var releaseOnce sync.Once
			var calls, active atomic.Int32
			syncErr := errors.New("injected publication sync failure")
			failedHash := fmt.Sprintf("%x", sha256.Sum256([]byte(contents["03.txt"])))
			originalSync := pack.SyncDir
			pack.SyncDir = func(dir string) error {
				if filepath.Base(dir) == "blobs" && calls.Add(1) <= 8 {
					active.Add(1)
					entered <- struct{}{}
					<-release
					active.Add(-1)
				}
				if outcome == "sync failure" && filepath.Base(dir) == failedHash[:2] {
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
			for i := range 8 {
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
			assert.Empty(t, children, "blocked publication must not grant metadata authority")
			assert.EqualValues(t, 8, active.Load())
			if outcome == "cancel" {
				cancel()
			}
			releaseOnce.Do(func() { close(release) })
			<-done
			assert.Zero(t, active.Load(), "the importer must join its writes before returning")
			for i := 1; i < len(events); i++ {
				assert.GreaterOrEqual(t, events[i].BytesRead, events[i-1].BytesRead)
				assert.GreaterOrEqual(t, events[i].FilesDone, events[i-1].FilesDone)
			}
			assert.Equal(t, outcome != "cancel", events[len(events)-1].Final)
			children, err = ing.Store.Children(t.Context(), parent.ID)
			require.NoError(t, err)
			switch outcome {
			case "cancel":
				require.ErrorIs(t, importErr, context.Canceled)
				assert.Empty(t, children)
			case "sync failure":
				require.NoError(t, importErr)
				require.Len(t, report.Failed, 1)
				assert.Equal(t, filepath.Join(source, "03.txt"), report.Failed[0].Path)
				require.ErrorIs(t, report.Failed[0].Err, syncErr)
				assert.Equal(t, 8, report.Added)
				require.Len(t, children, 8)
				_, err = ing.Store.PhysicalContent(t.Context(), failedHash)
				require.ErrorIs(t, err, store.ErrNotFound)
			case "success":
				require.NoError(t, importErr)
				assert.Empty(t, report.Failed)
				assert.Equal(t, 9, report.Added)
				require.Len(t, children, 9)
			}
			for _, node := range children {
				reader, err := ing.Blobs.OpenContext(t.Context(), node.BlobHash)
				require.NoError(t, err)
				data, readErr := io.ReadAll(reader)
				closeErr := reader.Close()
				require.NoError(t, readErr)
				require.NoError(t, closeErr)
				assert.Equal(t, contents[node.Name], string(data))
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
