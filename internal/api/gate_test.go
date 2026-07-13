package api

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

func TestGateFreezerBlocksMutationOnlyUntilEnd(t *testing.T) {
	g := &gate{}
	freezer := &gateFreezer{gate: g}
	require.NoError(t, freezer.Begin(t.Context()))

	mutated := make(chan struct{})
	go func() {
		_ = g.mutate(func() error {
			close(mutated)
			return nil
		})
	}()
	select {
	case <-mutated:
		t.Fatal("mutation passed while backup freeze was held")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, freezer.End(context.Background()))
	select {
	case <-mutated:
	case <-time.After(time.Second):
		t.Fatal("mutation remained blocked after backup freeze ended")
	}
	require.Error(t, freezer.End(context.Background()))
}

func TestBackupCaptureBlocksGCButAllowsLiveDeletion(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = metadata.Close() })
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })

	source := filepath.Join(root, "source.txt")
	require.NoError(t, os.WriteFile(source, []byte("capture before collection"), 0o600))
	ing := &ingest.Ingester{Store: metadata, Blobs: blobs}
	require.NoError(t, blobs.WithMutation(ctx, func() error {
		report, err := ing.AddPaths(ctx, []string{source}, "/inbox")
		if err == nil {
			assert.Equal(t, 1, report.Added)
		}
		return err
	}))
	node, err := metadata.NodeByPath(ctx, "/inbox/source.txt")
	require.NoError(t, err)

	repo, err := backup.Init(filepath.Join(root, "backup"))
	require.NoError(t, err)
	g := &gate{}
	d := Deps{Store: metadata, Blobs: blobs}
	metadataCaptured := make(chan struct{})
	resumeBackup := make(chan struct{})
	var resumeOnce sync.Once
	t.Cleanup(func() { resumeOnce.Do(func() { close(resumeBackup) }) })
	type captureResult struct {
		snapshot BackupSnapshot
		err      error
	}
	captured := make(chan captureResult, 1)
	go func() {
		var pauseOnce sync.Once
		snapshot, err := createBackupSnapshot(ctx, repo, d, g,
			backupCreateRequest{Jobs: 1}, func(event backup.ProgressEvent) {
				if event.Stage == backup.ProgressStageMetadata && event.Final {
					pauseOnce.Do(func() {
						close(metadataCaptured)
						<-resumeBackup
					})
				}
			})
		captured <- captureResult{snapshot: snapshot, err: err}
	}()
	select {
	case <-metadataCaptured:
	case <-time.After(5 * time.Second):
		t.Fatal("backup did not reach the post-metadata capture boundary")
	}

	// The short exclusive freeze is over: an ordinary mutation can remove
	// the live node while the pinned snapshot still requires its blob.
	require.NoError(t, g.mutate(func() error {
		if _, _, err := metadata.Trash(ctx, node.ID, node.Revision); err != nil {
			return err
		}
		_, err := metadata.TrashEmpty(ctx, 0, true)
		return err
	}))

	maintenanceAttempted := make(chan struct{})
	maintenanceEntered := make(chan struct{})
	type gcResult struct {
		report GCReport
		err    error
	}
	collected := make(chan gcResult, 1)
	go func() {
		close(maintenanceAttempted)
		var report GCReport
		err := g.maintain(func() error {
			close(maintenanceEntered)
			return blobs.WithMutation(ctx, func() error {
				var err error
				report, err = runGC(ctx, d, true)
				return err
			})
		})
		collected <- gcResult{report: report, err: err}
	}()
	<-maintenanceAttempted
	select {
	case <-maintenanceEntered:
		t.Fatal("GC entered while the backup still required pinned content")
	case <-time.After(50 * time.Millisecond):
	}

	resumeOnce.Do(func() { close(resumeBackup) })
	var result captureResult
	select {
	case result = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("backup did not finish after content capture resumed")
	}
	require.NoError(t, result.err)
	assert.Equal(t, int64(1), result.snapshot.Files)

	var gc gcResult
	select {
	case gc = <-collected:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not resume after backup completion")
	}
	require.NoError(t, gc.err)
	assert.Equal(t, 1, gc.report.Removed)

	verified, err := backup.Verify(ctx, repo, backupapp.New("test"), backup.VerifyOptions{})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)
}
