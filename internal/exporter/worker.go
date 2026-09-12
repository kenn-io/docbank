// Package exporter executes stored export plans under daemon ownership.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/safefileio"
)

type Gate interface {
	MutateContext(ctx context.Context, fn func() error) error
}
type Worker struct {
	catalog      *store.Store
	blobs        *blob.Store
	gate         Gate
	dir          string
	run          sync.Mutex
	mu           sync.Mutex
	leases       map[string]int
	activeMu     sync.Mutex
	activeOwner  string
	activeCancel context.CancelCauseFunc
}

var errOwnerRevoked = errors.New("export owner revoked")

// CancelOwner interrupts the single active export before durable revocation
// waits for maintenance. The catalog mutation still owns the persistent fence.
func (w *Worker) CancelOwner(owner string) {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	if w.activeOwner == owner && w.activeCancel != nil {
		w.activeCancel(errOwnerRevoked)
	}
}

func New(catalog *store.Store, blobs *blob.Store, root string, gate Gate) (*Worker, error) {
	if catalog == nil || blobs == nil || root == "" || gate == nil {
		return nil, bundle.ErrConflict
	}
	dir := filepath.Join(root, "export-archives")
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("secure export archive directory: %w", err)
	}
	return &Worker{catalog: catalog, blobs: blobs, gate: gate, dir: dir, leases: map[string]int{}}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	if err := w.gate.MutateContext(ctx, func() error { return w.catalog.RevokeAbandonedExportSessions(ctx) }); err != nil {
		return err
	}
	if err := w.gate.MutateContext(ctx, func() error { return w.catalog.RequeueExportJobs(ctx) }); err != nil {
		return err
	}
	files, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		remove := strings.HasPrefix(file.Name(), ".export-")
		if id, ok := strings.CutSuffix(file.Name(), ".zip"); ok {
			if parsed, e := uuid.Parse(id); e == nil && parsed.String() == id {
				retained, e := w.catalog.ExportArchiveRetained(ctx, id)
				if e != nil {
					return e
				}
				remove = !retained
			}
		}
		if remove {
			if err = os.Remove(filepath.Join(w.dir, file.Name())); err != nil {
				return err
			}
		}
	}
	for {
		if err = w.Cleanup(ctx); err != nil {
			return err
		}
		processed, err := w.RunOne(ctx)
		if err != nil {
			return err
		}
		if !processed {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	w.run.Lock()
	defer w.run.Unlock()
	var claim store.ExportClaim
	err := w.gate.MutateContext(ctx, func() error { var err error; claim, err = w.catalog.ClaimExportJob(ctx); return err })
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, claim.Job.Deadline)
	if err != nil {
		return true, fmt.Errorf("export execution deadline: %w", err)
	}
	timed, stopDeadline := context.WithDeadline(ctx, deadline)
	defer stopDeadline()
	work, stopWork := context.WithCancelCause(timed)
	cancel := func() { stopWork(context.Canceled) }
	defer cancel()
	w.activeMu.Lock()
	w.activeOwner, w.activeCancel = claim.Owner, stopWork
	w.activeMu.Unlock()
	defer func() {
		w.activeMu.Lock()
		w.activeOwner, w.activeCancel = "", nil
		w.activeMu.Unlock()
	}()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				if w.catalog.CheckExportClaim(work, claim) != nil {
					cancel()
					return
				}
			}
		}
	}()
	plan, err := w.catalog.ExportPlanForClaim(work, claim)
	var file *os.File
	var receipt bundle.Receipt
	if err == nil {
		file, err = os.CreateTemp(w.dir, ".export-claim-*")
	}
	if file != nil {
		defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	}
	if err == nil {
		lastRoles, lastBytes := claim.Job.CompletedRoles, claim.Job.CompletedBytes
		lastAt := time.Now()
		receipt, err = bundle.Write(work, file, plan, func(visit func(bundle.Document) error) error {
			return w.catalog.WalkExportDocuments(work, plan.ID, visit)
		}, func(role bundle.Role) (io.ReadCloser, error) { return w.openRole(work, claim, role) }, func(roles int, bytes int64) error {
			if roles <= lastRoles || bytes < lastBytes {
				return nil
			}
			if roles != plan.RoleEntries && roles-lastRoles < 100 && time.Since(lastAt) < time.Second {
				return nil
			}
			e := w.gate.MutateContext(work, func() error { return w.catalog.AdvanceExportJob(work, claim, roles, bytes) })
			if e == nil {
				lastRoles, lastBytes, lastAt = roles, bytes, time.Now()
			}
			return e
		})
	}
	if err == nil {
		err = file.Close()
	}
	if err == nil {
		err = w.gate.MutateContext(work, func() error {
			if err := w.catalog.CheckExportClaim(work, claim); err != nil {
				return err
			}
			destination := filepath.Join(w.dir, claim.Job.ID+".zip")
			if err := os.Rename(file.Name(), destination); err != nil {
				return err
			}
			if err := w.catalog.FinishExportJob(work, claim, &receipt, claim.Job.ID+".zip", ""); err != nil {
				_ = os.Remove(destination)
				return err
			}
			return nil
		})
	}
	cancel()
	<-stopped
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if err != nil {
		if errors.Is(context.Cause(work), errOwnerRevoked) {
			return true, w.gate.MutateContext(ctx, func() error { return w.catalog.CancelExportJob(ctx, claim.Owner, claim.Job.ID) })
		}
		finish := w.gate.MutateContext(ctx, func() error { return w.catalog.FinishExportJob(ctx, claim, nil, "", "archive_failed") })
		if errors.Is(finish, bundle.ErrFenced) {
			return true, nil
		}
		return true, finish
	}
	return true, nil
}

func (w *Worker) openRole(ctx context.Context, claim store.ExportClaim, role bundle.Role) (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	go func() {
		err := w.gate.MutateContext(ctx, func() error {
			if err := w.catalog.CheckExportClaim(ctx, claim); err != nil {
				return err
			}
			stream, size, err := w.blobs.OpenStreamContext(ctx, role.SHA256)
			if err != nil {
				return err
			}
			if size != role.Size {
				_ = stream.Close()
				return bundle.ErrConflict
			}
			n, copyErr := io.CopyBuffer(writer, stream, make([]byte, bundle.BufferSize))
			closeErr := stream.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != role.Size || !stream.Verified() {
				return bundle.ErrInvalidArchive
			}
			return nil
		})
		_ = writer.CloseWithError(err)
	}()
	return reader, nil
}

// Lease pins an open, reverified immutable archive until release. Tickets own
// this lease; consuming a ticket never deletes the retained archive.
func (w *Worker) Lease(ctx context.Context, owner, id string) (*os.File, bundle.Receipt, func(), error) {
	w.mu.Lock()
	total := 0
	for _, n := range w.leases {
		total += n
	}
	if total >= 32 {
		w.mu.Unlock()
		return nil, bundle.Receipt{}, nil, bundle.ErrLimit
	}
	w.leases[id]++
	w.mu.Unlock()
	var once sync.Once
	var file *os.File
	release := func() {
		once.Do(func() {
			if file != nil {
				_ = file.Close()
			}
			w.mu.Lock()
			w.leases[id]--
			if w.leases[id] == 0 {
				delete(w.leases, id)
			}
			w.mu.Unlock()
		})
	}
	job, err := w.catalog.ExportJob(ctx, owner, id)
	if err != nil {
		release()
		return nil, bundle.Receipt{}, nil, err
	}
	if job.State != "completed" || job.Receipt == nil {
		release()
		return nil, bundle.Receipt{}, nil, bundle.ErrConflict
	}
	file, err = os.Open(filepath.Join(w.dir, id+".zip"))
	if err != nil {
		release()
		return nil, bundle.Receipt{}, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != job.Receipt.Size {
		release()
		return nil, bundle.Receipt{}, nil, bundle.ErrInvalidArchive
	}
	receipt, err := bundle.Verify(ctx, file, info.Size(), job.Fingerprint)
	if err != nil || receipt != *job.Receipt {
		release()
		return nil, bundle.Receipt{}, nil, bundle.ErrInvalidArchive
	}
	return file, receipt, release, nil
}

func (w *Worker) Cleanup(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gate.MutateContext(ctx, func() error {
		ids, err := w.catalog.ExpiredExportJobs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if w.leases[id] > 0 {
				continue
			}
			if err = w.catalog.DeleteExpiredExportJob(ctx, id); err != nil {
				return err
			}
			if err = os.Remove(filepath.Join(w.dir, id+".zip")); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return w.catalog.CleanupExportAuthority(ctx)
	})
}
