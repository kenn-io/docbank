package exporter_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/exporter"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

type contentionDriver struct {
	docsqlite.Driver

	dbs []*sql.DB
}

func (d *contentionDriver) Open(path string, options docsqlite.OpenOptions) (*sql.DB, error) {
	options.BusyTimeout = time.Millisecond
	db, err := d.Driver.Open(path, options)
	if err == nil {
		d.dbs = append(d.dbs, db)
	}
	return db, err
}

type mutationFunc func(context.Context, func() error) error

func (f mutationFunc) MutateContext(ctx context.Context, fn func() error) error {
	return f(ctx, fn)
}

func workerFixture(t *testing.T, gate exporter.Gate) (*exporter.Worker, *store.Store, bundle.Job, *contentionDriver, string) {
	t.Helper()
	root := t.TempDir()
	driver := &contentionDriver{Driver: store.DefaultSQLiteDriver()}
	path := filepath.Join(root, "vault.db")
	catalog, err := store.Open(path, driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	hash, size, err := blobs.Write(bytes.NewBufferString("synthetic original\n"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "synthetic.txt", hash, size, "text/plain")
	require.NoError(t, err)
	source, err := catalog.CreateExportSource(t.Context(), "master", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: hash, Size: size}}}, nil)
	require.NoError(t, err)
	plan, err := catalog.CreateExportPlan(t.Context(), "master", bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.NoError(t, err)
	job, err := catalog.QueueExportJob(t.Context(), "master", bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.NoError(t, err)
	worker, err := exporter.New(catalog, blobs, root, gate)
	require.NoError(t, err)
	return worker, catalog, job, driver, path
}

func TestPhotoOwnerExportAccessLoss(t *testing.T) {
	baseGate := api.NewOperationGate()
	var catalog *store.Store
	var ownerBContext context.Context
	var asset store.PhotoAsset
	var sidecarNodeID int64
	calls := 0
	attached := false
	worker, catalog, fixtureJob, _, _ := workerFixture(t, mutationFunc(func(ctx context.Context, fn func() error) error {
		calls++
		if calls == 4 {
			var err error
			asset, err = catalog.AttachPhotoFile(ownerBContext, asset.ID, asset.Revision, sidecarNodeID, store.PhotoRoleSidecar, &asset.Files[0].ID)
			if err != nil {
				return err
			}
			attached = true
		}
		return baseGate.MutateContext(ctx, fn)
	}))
	ctx := t.Context()
	require.NoError(t, catalog.CancelExportJob(ctx, "master", fixtureJob.ID))
	ownerA, err := catalog.CreatePhotoOwner(ctx, "Export owner A")
	require.NoError(t, err)
	ownerB, err := catalog.CreatePhotoOwner(ctx, "Export owner B")
	require.NoError(t, err)
	ownerAContext := store.WithPhotoOwner(ctx, ownerA.ID)
	ownerBContext = store.WithPhotoOwner(ctx, ownerB.ID)
	ordinary, err := catalog.NodeByPath(ctx, "/synthetic.txt")
	require.NoError(t, err)
	sidecarNodeID = ordinary.ID
	raw, err := catalog.CreateFile(ownerBContext, catalog.RootID(), "capture.cr2", ordinary.BlobHash, ordinary.Size, "application/octet-stream")
	require.NoError(t, err)
	asset, err = catalog.PromotePhotoNode(ownerBContext, raw.ID, nil, store.PhotoRoleRAW, "")
	require.NoError(t, err)
	valid, err := catalog.CreateFile(ownerAContext, catalog.RootID(), "valid.txt", ordinary.BlobHash, ordinary.Size, "text/plain")
	require.NoError(t, err)
	makeExport := func(node store.Node) bundle.Job {
		source, err := catalog.CreateExportSource(ownerAContext, "worker-owner", bundle.SourceRequest{
			OperationID: uuid.New().String(), Kind: "explicit",
			Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}},
		}, nil)
		require.NoError(t, err)
		plan, err := catalog.CreateExportPlan(ownerAContext, "worker-owner", bundle.PlanRequest{
			OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash,
			Roles: []bundle.RolePolicy{{Role: "original"}},
		})
		require.NoError(t, err)
		job, err := catalog.QueueExportJob(ownerAContext, "worker-owner", bundle.JobRequest{
			OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint,
		})
		require.NoError(t, err)
		return job
	}
	firstJob := makeExport(ordinary)
	secondJob := makeExport(valid)
	processed, err := worker.RunOne(ctx)
	require.NoError(t, err)
	require.True(t, processed)
	require.True(t, attached)
	failed, err := catalog.ExportJob(ctx, "worker-owner", firstJob.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", failed.State)
	require.Equal(t, "archive_failed", failed.Failure)
	processed, err = worker.RunOne(ctx)
	require.NoError(t, err)
	require.True(t, processed)
	completed, err := catalog.ExportJob(ctx, "worker-owner", secondJob.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", completed.State)
}

func TestWorkerRetriesCatalogContention(t *testing.T) {
	for _, phase := range []string{"startup", "cleanup", "claim", "progress", "finish"} {
		t.Run(phase, func(t *testing.T) {
			gate := api.NewOperationGate()
			var locker *sql.DB
			var catalog *store.Store
			type lockResult struct {
				tx  *sql.Tx
				err error
			}
			locked := make(chan lockResult, 1)
			calls := 0
			worker, catalog, job, driver, path := workerFixture(t, mutationFunc(func(ctx context.Context, fn func() error) error {
				calls++
				// RunOne claims, opens the source, advances progress, then publishes.
				target := map[string]int{"startup": 1, "cleanup": 1, "claim": 1, "progress": 3, "finish": 4}[phase]
				if calls == target {
					tx, err := locker.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					locked <- lockResult{tx, catalog.RequeueExportJobs(ctx)}
				}
				return gate.MutateContext(ctx, fn)
			}))
			var err error
			locker, err = driver.Driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			defer func() { require.NoError(t, locker.Close()) }()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch phase {
				case "startup":
					done <- worker.Run(ctx)
				case "cleanup":
					done <- worker.Cleanup(ctx)
				default:
					_, err := worker.RunOne(ctx)
					done <- err
				}
			}()
			var tx *sql.Tx
			select {
			case result := <-locked:
				tx = result.tx
				err = result.err
			case err := <-done:
				t.Fatalf("worker stopped before contention: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			defer func() { _ = tx.Rollback() }()
			// The real competing transaction exceeds the configured busy timeout.
			require.True(t, catalog.RenditionJobErrorRetryable(err), "expected SQLite contention, got %v", err)
			select {
			case err := <-done:
				t.Fatalf("worker stopped during temporary contention: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			require.NoError(t, tx.Rollback())
			if phase == "startup" {
				require.Eventually(t, func() bool {
					stored, err := catalog.ExportJob(ctx, "master", job.ID)
					return err == nil && stored.State == "completed"
				}, 3*time.Second, 10*time.Millisecond)
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
			} else {
				require.NoError(t, <-done)
			}
			if phase == "cleanup" {
				return
			}
			file, receipt, release, err := worker.Lease(t.Context(), "master", job.ID)
			require.NoError(t, err)
			defer release()
			verified, err := bundle.Verify(t.Context(), file, receipt.Size, job.Fingerprint)
			require.NoError(t, err)
			require.Equal(t, receipt, verified)
		})
	}
}

func TestWorkerDoesNotFenceClaimOnTemporaryReadContention(t *testing.T) {
	opening, resume := make(chan struct{}), make(chan struct{})
	gate := api.NewOperationGate()
	calls := 0
	worker, catalog, job, driver, path := workerFixture(t, mutationFunc(func(ctx context.Context, fn func() error) error {
		calls++
		if calls == 2 {
			close(opening)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return gate.MutateContext(ctx, fn)
	}))
	// Only the external locker should cause contention in this fixture.
	// Let the other SQLite connection take an exclusive lock while no query runs.
	for _, db := range driver.dbs {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(0)
	}
	locker, err := driver.Driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	defer func() { require.NoError(t, locker.Close()) }()
	locker.SetMaxOpenConns(1)
	_, err = locker.ExecContext(t.Context(), "PRAGMA locking_mode=EXCLUSIVE")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := worker.RunOne(ctx); done <- err }()
	select {
	case <-opening:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	tx, err := locker.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	err = catalog.CheckExportClaim(ctx, store.ExportClaim{})
	require.True(t, catalog.RenditionJobErrorRetryable(err), "expected read contention, got %v", err)
	// Allow at least one claim poll to observe the real read lock.
	time.Sleep(350 * time.Millisecond) //nolint:kennlint // claim polls must see the real SQLite read lock, which runs on the wall clock
	// Keep resumed reads from racing the last connection's WAL teardown.
	for _, db := range driver.dbs {
		db.SetMaxIdleConns(2)
	}
	require.NoError(t, tx.Rollback())
	require.NoError(t, locker.Close())
	close(resume)
	require.NoError(t, <-done)
	stored, err := catalog.ExportJob(ctx, "master", job.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", stored.State)
}

func TestWorkerStopsOnPermanentCatalogError(t *testing.T) {
	want := errors.New("permanent catalog failure")
	calls := 0
	worker, catalog, job, _, _ := workerFixture(t, mutationFunc(func(context.Context, func() error) error {
		calls++
		return want
	}))
	_, err := worker.RunOne(t.Context())
	require.ErrorIs(t, err, want)
	require.Equal(t, 1, calls)
	stored, err := catalog.ExportJob(t.Context(), "master", job.ID)
	require.NoError(t, err)
	require.Equal(t, "queued", stored.State)
}
