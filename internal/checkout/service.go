package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

var creationMu sync.Mutex

type Service struct {
	Store         *store.Store
	Blobs         *blob.Store
	VaultRoot     string
	Config        config.Config
	Mutate        func(context.Context, func() error) error
	ActivityBegin func()
	ActivityEnd   func()
	admit         func(context.Context, *store.Store, string) (bool, error)
}

func (s Service) admitOperation(ctx context.Context, id string) (bool, error) {
	if s.ActivityBegin != nil && s.ActivityEnd != nil {
		s.ActivityEnd()
		defer s.ActivityBegin()
	}

	if s.admit != nil {
		return s.admit(ctx, s.Store, id)
	}
	return jobs.AdmitStorageOperation(ctx, s.Store, id)
}

func (s Service) mutate(ctx context.Context, fn func() error) error {
	if s.Mutate != nil {
		return s.Mutate(ctx, fn)
	}
	return fn()
}

// Prepare holds the empty destination until the supervised job closes it.
func (s Service) Prepare(ctx context.Context, selection store.PhotoCheckoutSelection, destination string, byteLimit int64) (store.PhotoCheckout, store.StorageOperation, *os.Root, error) {
	creationMu.Lock()
	defer creationMu.Unlock()
	var checkout store.PhotoCheckout
	var operation store.StorageOperation
	if !filepath.IsAbs(destination) {
		return checkout, operation, nil, fmt.Errorf("%w: destination must be an absolute daemon-host path", store.ErrInvalidPhotoCheckout)
	}
	canonical, err := filepath.EvalSymlinks(destination)
	if err != nil {
		return checkout, operation, nil, fmt.Errorf("%w: destination must be an existing folder: %w", store.ErrInvalidPhotoCheckout, err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return checkout, operation, nil, fmt.Errorf("%w: opening destination: %w", store.ErrInvalidPhotoCheckout, err)
	}
	err = s.mutate(ctx, func() error {
		if err := s.validateDestination(ctx, root, canonical, ""); err != nil {
			return err
		}
		var err error
		checkout, operation, err = s.Store.CreatePhotoCheckout(ctx, selection, canonical, byteLimit)
		return err
	})
	if err != nil {
		_ = root.Close()
		return checkout, operation, nil, err
	}
	return checkout, operation, root, nil
}

func (s Service) validateDestination(ctx context.Context, root *os.Root, destination, excludedID string) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	_, readErr := dir.Readdirnames(1)
	closeErr := dir.Close()
	if readErr != io.EOF {
		return fmt.Errorf("%w: destination must be empty", store.ErrInvalidPhotoCheckout)
	}
	if closeErr != nil {
		return closeErr
	}
	roots := []string{s.VaultRoot}
	for _, b := range s.Config.StoreBindings {
		if b.Path != "" {
			roots = append(roots, b.Path)
		}
	}
	for _, w := range s.Config.Watches {
		roots = append(roots, w.Source)
	}
	checkouts, err := s.Store.ListPhotoCheckouts(ctx)
	if err != nil {
		return err
	}
	for _, c := range checkouts {
		if c.ID != excludedID && c.Destination != "" {
			roots = append(roots, c.Destination)
		}
	}
	for _, other := range roots {
		if other == "" {
			continue
		}
		info, statErr := os.Stat(other)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		var overlap bool
		if statErr == nil && info.IsDir() {
			_, overlap, err = ingest.WatchBindingOverlap([]config.WatchConfig{{Name: "checkout protection", Source: other}}, config.StoreBindingConfig{Kind: "filesystem", Path: destination})
		} else {
			overlap, err = ingest.PathsOverlap(destination, other)
		}
		if overlap {
			return fmt.Errorf("%w: destination overlaps a vault, storage root, watched folder, or checkout", store.ErrInvalidPhotoCheckout)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Reject a replacement between opening the handle and checking its pathname.
	held, err := root.Stat(".")
	if err != nil {
		return err
	}
	named, err := os.Stat(destination)
	if err != nil {
		return err
	}
	if !os.SameFile(held, named) {
		return fmt.Errorf("%w: destination changed during validation", store.ErrInvalidPhotoCheckout)
	}
	return nil
}

func (s Service) Start(supervisor *jobs.Supervisor, c store.PhotoCheckout, root *os.Root) error {
	err := s.start(supervisor, c, root, false)
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		if !errors.Is(err, jobs.ErrDuplicate) {
			err = errors.Join(err, s.finish(context.Background(), c, store.StorageOperationFailed, err.Error()))
		}
	}
	return err
}

func (s Service) start(supervisor *jobs.Supervisor, c store.PhotoCheckout, root *os.Root, interrupted bool) error {
	if supervisor == nil {
		return errors.New("photo checkout requires a job supervisor")
	}
	return supervisor.Start("storage:"+c.OperationID, func(ctx context.Context) error {
		if root != nil {
			defer func() { _ = root.Close() }()
		}
		if interrupted {
			return s.reconcile(ctx, c)
		}
		return jobs.RetryStorageOperation(ctx, time.Second, func() error { return s.Run(ctx, c, root) })
	})
}

// Resume registers work without accessing external checkout destinations during startup.
func (s Service) Resume(ctx context.Context, supervisor *jobs.Supervisor) error {
	operations, err := s.Store.ResumableStorageOperations(ctx)
	if err != nil {
		return err
	}
	for _, op := range operations {
		if op.Kind != store.StorageOperationKindPhotoCheckout {
			continue
		}
		var request struct {
			CheckoutID string `json:"checkout_id"`
		}
		if err := json.Unmarshal([]byte(op.RequestJSON), &request); err != nil {
			return err
		}
		c, err := s.Store.PhotoCheckout(ctx, request.CheckoutID)
		if err != nil {
			return err
		}
		started, err := s.Store.PhotoCheckoutStarted(ctx, c.ID)
		if err != nil {
			return err
		}
		if err := s.start(supervisor, c, nil, started); err != nil && !errors.Is(err, jobs.ErrDuplicate) {
			return err
		}
	}
	return nil
}

func (s Service) reconcile(ctx context.Context, c store.PhotoCheckout) error {
	if s.ActivityBegin != nil {
		s.ActivityBegin()
		if s.ActivityEnd != nil {
			defer s.ActivityEnd()
		}
	}
	failure := "checkout creation was interrupted; partial copies and frozen bases are retained"
	canonical, fsErr := filepath.EvalSymlinks(c.Destination)
	invalidDestination := fsErr == nil && canonical != c.Destination
	if invalidDestination {
		fsErr = fmt.Errorf("destination now resolves to %q", canonical)
	}
	var root *os.Root
	if fsErr == nil {
		var info os.FileInfo
		info, fsErr = os.Stat(c.Destination)
		if fsErr == nil {
			if info.IsDir() {
				root, fsErr = os.OpenRoot(c.Destination)
			} else {
				invalidDestination = true
				fsErr = errors.New("destination is no longer a directory")
			}
		}
	}
	if invalidDestination || errors.Is(fsErr, os.ErrNotExist) {
		if err := s.Store.ObservePhotoCheckout(ctx, c.ID, c.OperationID, nil); err != nil {
			return err
		}
	}
	if root != nil {
		defer func() { _ = root.Close() }()
		entries, err := s.Store.PhotoCheckoutEntries(ctx, c.ID)
		if err != nil {
			return err
		}
		fsErr = filepublish.CleanupRootStages(root)
		var verified []string
		seen := map[string]bool{}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if seen[e.Path] {
				continue
			}
			seen[e.Path] = true
			matches, err := observeCheckoutFile(ctx, root, e)
			if err != nil {
				fsErr = errors.Join(fsErr, fmt.Errorf("file %s at %q: %w", e.FileID, e.Path, err))
			}
			if matches {
				verified = append(verified, e.FileID)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.Store.ObservePhotoCheckout(ctx, c.ID, c.OperationID, verified); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if fsErr != nil && !errors.Is(fsErr, os.ErrNotExist) {
		failure += "; reconciliation failed: " + fsErr.Error()
	}
	return s.finish(ctx, c, store.StorageOperationFailed, failure)
}

func observeCheckoutFile(ctx context.Context, root *os.Root, entry store.PhotoCheckoutEntry) (bool, error) {
	name := filepath.FromSlash(entry.Path)
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != entry.Size {
		return false, nil
	}
	file, err := root.Open(name)
	if err != nil {
		return false, err
	}
	digest := sha256.New()
	size, readErr := io.Copy(digest, blob.ReaderWithContext(ctx, file))
	err = errors.Join(readErr, file.Close())
	return err == nil && size == entry.Size && hex.EncodeToString(digest.Sum(nil)) == entry.Hash, err
}
func (s Service) finish(ctx context.Context, c store.PhotoCheckout, state store.StorageOperationState, failure string) error {
	return s.Store.FinishPhotoCheckout(ctx, c.ID, c.OperationID, state, "{}", failure, time.Now().Add(30*24*time.Hour))
}

func (s Service) Run(ctx context.Context, c store.PhotoCheckout, root *os.Root) error {
	if s.ActivityBegin != nil {
		s.ActivityBegin()
		if s.ActivityEnd != nil {
			defer s.ActivityEnd()
		}
	}
	cancelled, err := s.admitOperation(ctx, c.OperationID)
	if err != nil {
		return jobs.DeferStorageAdmission(ctx, s.Store, c.OperationID, err)
	}
	started := false
	if root == nil {
		started, err = s.Store.PhotoCheckoutStarted(ctx, c.ID)
		if err != nil {
			return err
		}
	}
	if err := s.Store.ClaimPhotoCheckout(ctx, c.ID, c.OperationID); err != nil {
		return err
	}
	if cancelled {
		return s.finish(ctx, c, store.StorageOperationCancelled, "")
	}
	if root == nil {
		canonical, err := filepath.EvalSymlinks(c.Destination)
		if err == nil && canonical != c.Destination {
			err = errors.New("checkout destination changed")
		}
		if err == nil {
			root, err = os.OpenRoot(c.Destination)
		}
		if err != nil {
			return s.finish(ctx, c, store.StorageOperationFailed, err.Error())
		}
		defer func() { _ = root.Close() }()
		if !started {
			if err := s.mutate(ctx, func() error { return s.validateDestination(ctx, root, c.Destination, c.ID) }); err != nil {
				return errors.Join(err, s.finish(ctx, c, store.StorageOperationFailed, err.Error()))
			}
		}
	}
	entries, copyErr := s.Store.PhotoCheckoutEntries(ctx, c.ID)
	var copied, bytes int64
	if copyErr == nil && !cancelled {
		for _, entry := range entries {
			if entry.Published {
				copied++
				bytes += entry.Size
				continue
			}
			var admissionErr error
			cancelled, admissionErr = s.admitOperation(ctx, c.OperationID)
			if admissionErr != nil {
				return jobs.DeferStorageAdmission(ctx, s.Store, c.OperationID, admissionErr)
			}
			if cancelled {
				break
			}
			if err := s.copy(ctx, root, c.Destination, entry); err != nil {
				if copyErr == nil {
					copyErr = err
				}
				if errors.Is(err, store.ErrPhotoCheckoutSourceUnavailable) {
					continue
				}
				break
			}
			copied++
			bytes += entry.Size
			if err := s.Store.AdvanceStorageOperation(ctx, c.OperationID, entry.FileID, copied, copied, bytes, "{}"); err != nil {
				copyErr = errors.Join(copyErr, err)
				break
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !cancelled {
		cancelled, err = s.Store.FinalizeLocalOperation(ctx, c.OperationID)
		if err != nil {
			return errors.Join(copyErr, err)
		}
	}
	state, failure := store.StorageOperationCompleted, ""
	if copyErr == nil && !cancelled {
		copyErr = checkDestinationIdentity(root, c.Destination)
	}
	if cancelled {
		state = store.StorageOperationCancelled
	} else if copyErr != nil {
		state, failure = store.StorageOperationFailed, copyErr.Error()
	}
	return errors.Join(copyErr, s.finish(context.WithoutCancel(ctx), c, state, failure))
}

func checkDestinationIdentity(root *os.Root, destination string) error {
	held, heldErr := root.Stat(".")
	named, namedErr := os.Stat(destination)
	if err := errors.Join(heldErr, namedErr); err != nil {
		return fmt.Errorf("%w: destination changed during checkout: %w", store.ErrInvalidPhotoCheckout, err)
	}
	if !os.SameFile(held, named) {
		return fmt.Errorf("%w: destination changed during checkout", store.ErrInvalidPhotoCheckout)
	}
	return nil
}

func (s Service) copy(ctx context.Context, root *os.Root, destination string, e store.PhotoCheckoutEntry) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("file %s at %q: %w", e.FileID, e.Path, retErr)
		}
	}()
	stage, err := filepublish.CreateRootStage(root)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, stage.Cleanup()) }()
	if err := stage.MakeParentDirectories(filepath.FromSlash(e.Path)); err != nil {
		return err
	}
	stream, size, err := s.Blobs.OpenStreamContext(ctx, e.Hash)
	if err != nil {
		return err
	}
	if size != e.Size {
		_ = stream.Close()
		return errors.New("checkout source size differs from frozen base")
	}
	n, copyErr := io.Copy(stage.File, stream)
	if err := errors.Join(copyErr, stream.Verify(), stream.Close()); err != nil {
		return err
	}
	if n != e.Size {
		return errors.New("checkout bytes differ from frozen base")
	}
	if err := stage.File.Sync(); err != nil {
		return err
	}
	// Read the staged bytes back before publication to catch destination I/O damage.
	if _, err := stage.File.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	n, err = io.Copy(digest, blob.ReaderWithContext(ctx, stage.File))
	if err != nil {
		return err
	}
	if n != e.Size || hex.EncodeToString(digest.Sum(nil)) != e.Hash {
		return errors.New("checkout staged bytes failed verification")
	}
	if err := stage.File.Close(); err != nil {
		return err
	}
	stage.File = nil
	return s.mutate(ctx, func() error {
		return s.Store.PublishPhotoCheckoutEntry(ctx, e, func() (bool, error) {
			installed, err := stage.Publish(filepath.FromSlash(e.Path))
			return installed, errors.Join(err, checkDestinationIdentity(root, destination))
		})
	})
}
