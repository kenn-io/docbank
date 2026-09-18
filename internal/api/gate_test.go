package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	internalconfig "go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/ingest"
	internalmaintenance "go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
)

func TestGateFreezerBlocksMutationOnlyUntilEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := NewOperationGate()
		freezer := &gateFreezer{gate: g}
		require.NoError(t, freezer.Begin(t.Context()))
		t.Cleanup(func() { _ = freezer.End(context.Background()) })

		mutated := make(chan struct{})
		go func() {
			_ = g.mutate(func() error {
				close(mutated)
				return nil
			})
		}()
		synctest.Wait()
		select {
		case <-mutated:
			t.Fatal("mutation passed while backup freeze was held")
		default:
		}

		require.NoError(t, freezer.End(context.Background()))
		synctest.Wait()
		select {
		case <-mutated:
		default:
			t.Fatal("mutation remained blocked after backup freeze ended")
		}
		require.Error(t, freezer.End(context.Background()))
	})
}

func TestBackupCaptureBlocksPlacementAuthorityCommit(t *testing.T) {
	g := NewOperationGate()
	captureStarted := make(chan struct{})
	releaseCapture := make(chan struct{})
	captureDone := make(chan error, 1)
	go func() {
		captureDone <- g.capture(func() error {
			close(captureStarted)
			<-releaseCapture
			return nil
		})
	}()
	<-captureStarted

	commitStarted := make(chan struct{})
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- g.PhysicalMutate(func() error {
			close(commitStarted)
			return nil
		})
	}()
	select {
	case <-commitStarted:
		t.Fatal("placement authority commit started during backup capture")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseCapture)
	require.NoError(t, <-captureDone)
	select {
	case <-commitStarted:
	case <-time.After(time.Second):
		t.Fatal("placement authority commit did not resume after backup capture")
	}
	require.NoError(t, <-commitDone)
}

func TestQueuedMaintenanceRejectsRouteMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := NewOperationGate()
		captureEntered := make(chan struct{})
		releaseCapture := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(releaseCapture) }) })
		captureDone := make(chan error, 1)
		go func() {
			captureDone <- g.capture(func() error {
				close(captureEntered)
				<-releaseCapture
				return nil
			})
		}()
		<-captureEntered

		maintenanceDone := make(chan error, 1)
		go func() {
			maintenanceDone <- g.MaintainContext(t.Context(), func() error { return nil })
		}()
		synctest.Wait()
		g.admission.RLock()
		maintenanceQueued := g.maintenance == 1
		g.admission.RUnlock()
		assert.True(t, maintenanceQueued)

		err := g.mutate(func() error {
			t.Fatal("route mutation ran while maintenance was queued")
			return nil
		})
		var apiErr *Error
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, "maintenance_busy", apiErr.Code)

		releaseOnce.Do(func() { close(releaseCapture) })
		synctest.Wait()
		require.NoError(t, <-captureDone)
		require.NoError(t, <-maintenanceDone)
	})
}

func TestDaemonLogicalMutationDoesNotFailCloseRouteMutations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := NewOperationGate()
		entered := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- g.MutateContext(t.Context(), func() error { close(entered); <-release; return nil }) }()
		<-entered
		reached := false
		require.NoError(t, g.mutate(func() error { reached = true; return nil }))
		assert.True(t, reached)
		close(release)
		synctest.Wait()
		require.NoError(t, <-done)
	})
}

func TestCanceledQueuedMaintenanceStopsRejectingRouteMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := NewOperationGate()
		captureEntered := make(chan struct{})
		releaseCapture := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(releaseCapture) }) })
		go func() {
			_ = g.capture(func() error {
				close(captureEntered)
				<-releaseCapture
				return nil
			})
		}()
		<-captureEntered

		ctx, cancel := context.WithCancel(t.Context())
		maintenanceDone := make(chan error, 1)
		go func() {
			maintenanceDone <- g.maintainContext(ctx, func() error {
				t.Error("canceled maintenance entered")
				return nil
			})
		}()
		synctest.Wait()
		g.admission.RLock()
		maintenanceQueued := g.maintenance == 1
		g.admission.RUnlock()
		assert.True(t, maintenanceQueued)

		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-maintenanceDone, context.Canceled)
		require.NoError(t, g.mutate(func() error { return nil }),
			"canceled maintenance must stop rejecting mutations before backup completes")
		releaseOnce.Do(func() { close(releaseCapture) })
		synctest.Wait()
	})
}

func TestScheduledPackDeadlineClearsAdmissionAfterBackupCapture(t *testing.T) {
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = metadata.Close() })
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })

	hash, size, err := blobs.Write(strings.NewReader("scheduled pack capture"))
	require.NoError(t, err)
	_, err = metadata.CreateFile(t.Context(), metadata.RootID(), "capture.txt", hash, size, "text/plain")
	require.NoError(t, err)
	repo, err := backup.Init(filepath.Join(root, "backup"))
	require.NoError(t, err)

	g := NewOperationGate()
	cfg := internalconfig.Default()
	cfg.Server.APIKey = "test-api-key"
	server := NewServer(Deps{
		Store: metadata, Blobs: blobs, VaultRoot: root, Cfg: cfg, Gate: g,
	})
	t.Cleanup(server.Close)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	captureDone := make(chan error, 1)
	metadataCaptured := make(chan struct{})
	releaseCapture := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseCapture) }) })
	go func() {
		var pauseOnce sync.Once
		_, captureErr := createBackupSnapshot(t.Context(), repo,
			Deps{Store: metadata, Blobs: blobs, Cfg: cfg}, g,
			backupCreateRequest{Jobs: 1}, func(event backup.ProgressEvent) {
				if event.Stage == backup.ProgressStageMetadata && event.Final {
					pauseOnce.Do(func() {
						close(metadataCaptured)
						<-releaseCapture
					})
				}
			})
		captureDone <- captureErr
	}()
	select {
	case <-metadataCaptured:
	case captureErr := <-captureDone:
		require.NoError(t, captureErr)
		t.Fatal("backup capture finished before the preservation boundary")
	case <-time.After(5 * time.Second):
		t.Fatal("backup capture did not reach the preservation boundary")
	}

	schedulerCtx, cancelScheduler := context.WithCancel(t.Context())
	defer cancelScheduler()
	packStarted := make(chan struct{})
	packReturned := make(chan struct{})
	var runCount int
	var returnOnce sync.Once
	schedulerDone := make(chan error, 1)
	go func() {
		schedulerDone <- internalmaintenance.RunPackSchedule(
			schedulerCtx, 50*time.Millisecond,
			func(runCtx context.Context) (internalmaintenance.PackReport, error) {
				runCount++
				if runCount > 1 {
					<-runCtx.Done()
					return internalmaintenance.PackReport{}, runCtx.Err()
				}
				close(packStarted)
				var report internalmaintenance.PackReport
				err := g.MaintainContext(runCtx, func() error {
					var err error
					report, err = internalmaintenance.Pack(
						runCtx, metadata, blobs, 1<<20)
					return err
				})
				returnOnce.Do(func() { close(packReturned) })
				return report, err
			},
			slog.New(slog.DiscardHandler),
		)
	}()
	select {
	case <-packStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled pack did not start")
	}
	select {
	case <-packReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled pack admission did not release at its interval")
	}
	select {
	case <-captureDone:
		t.Fatal("backup capture ended before the HTTP mutation")
	default:
	}
	select {
	case <-schedulerDone:
		t.Fatal("scheduled pack scheduler stopped before the HTTP mutation")
	default:
	}

	body := strings.NewReader(fmt.Sprintf(
		`{"parent_id":%d,"name":"during-capture","kind":"dir"}`,
		metadata.RootID(),
	))
	req, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost, ts.URL+"/api/v1/nodes", body,
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", cfg.Server.APIKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	cancelScheduler()
	select {
	case schedulerErr := <-schedulerDone:
		require.ErrorIs(t, schedulerErr, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled pack did not stop")
	}
	releaseOnce.Do(func() { close(releaseCapture) })
	select {
	case captureErr := <-captureDone:
		require.NoError(t, captureErr)
	case <-time.After(5 * time.Second):
		t.Fatal("backup capture did not finish after release")
	}
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
	g := NewOperationGate()
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
	// Real backup I/O is bounded by the Go test timeout, not a speed requirement.
	select {
	case <-metadataCaptured:
	case result := <-captured:
		require.NoError(t, result.err)
		t.Fatal("backup finished before the post-metadata capture boundary")
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
		err := g.MaintainContext(t.Context(), func() error {
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
	result := <-captured
	require.NoError(t, result.err)
	assert.Equal(t, int64(1), result.snapshot.Files)

	gc := <-collected
	require.NoError(t, gc.err)
	assert.Equal(t, 1, gc.report.Removed)

	verified, err := backup.Verify(ctx, repo, backupapp.New("test"), backup.VerifyOptions{})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)
}
