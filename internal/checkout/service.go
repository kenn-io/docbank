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
	syncParent    func(*filepublish.RootStage) error
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
	proof, err := s.checkDestination(ctx, root, canonical, "")
	if err != nil {
		_ = root.Close()
		return checkout, operation, nil, err
	}
	err = s.mutate(ctx, func() error {
		if err := s.recheckDestination(ctx, root, canonical, "", proof); err != nil {
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

func (s Service) protectedRoots(ctx context.Context, excludedID string) ([]string, error) {
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
		return nil, err
	}
	for _, c := range checkouts {
		if c.ID != excludedID && c.Destination != "" && !c.RetentionReleased {
			roots = append(roots, c.Destination)
		}
	}
	return roots, nil
}

func (s Service) checkDestination(ctx context.Context, root *os.Root, destination, excludedID string) (*ingest.PathProtection, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	paths, err := s.protectedRoots(ctx, excludedID)
	if err != nil {
		return nil, err
	}
	paths = append(paths, destination)
	proof, err := ingest.CapturePathProtection(paths)
	if err != nil {
		return nil, err
	}
	if err := s.validateDestination(ctx, root, destination, excludedID); err != nil {
		return nil, err
	}
	return proof, nil
}

func (s Service) recheckDestination(ctx context.Context, root *os.Root, destination, excludedID string, proof *ingest.PathProtection) error {
	paths, err := s.protectedRoots(ctx, excludedID)
	if err != nil {
		return err
	}
	if err := proof.Check(append(paths, destination)); err != nil {
		return err
	}
	return checkEmptyDestination(root, destination)
}

func checkEmptyDestination(root *os.Root, destination string) error {
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

func (s Service) validateDestination(ctx context.Context, root *os.Root, destination, excludedID string) error {
	if err := checkEmptyDestination(root, destination); err != nil {
		return err
	}
	roots, err := s.protectedRoots(ctx, excludedID)
	if err != nil {
		return err
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
			_, overlap, err = ingest.WatchBindingOverlapContext(ctx, []config.WatchConfig{{Name: "checkout protection", Source: other}}, config.StoreBindingConfig{Kind: "filesystem", Path: destination})
		} else {
			overlap, err = ingest.PathsOverlapContext(ctx, destination, other)
		}
		if overlap {
			return fmt.Errorf("%w: destination overlaps a vault, storage root, watched folder, or checkout", store.ErrInvalidPhotoCheckout)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return checkEmptyDestination(root, destination)
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
		return jobs.RetryStorageOperation(ctx, time.Second, func() error {
			if interrupted {
				if _, err := jobs.AdmitStorageOperation(ctx, s.Store, c.OperationID); err != nil {
					return jobs.DeferStorageAdmission(ctx, s.Store, c.OperationID, err)
				}
				if err := s.Store.ClaimPhotoCheckout(ctx, c.ID, c.OperationID); err != nil {
					return err
				}
				if err := s.reconcile(ctx, c); err != nil {
					return jobs.DeferStorageOperation(ctx, s.Store, c.OperationID, err)
				}
				return nil
			}
			err := s.Run(ctx, c, root)
			if errors.Is(err, store.ErrPhotoCheckoutPublicationUnrecorded) {
				interrupted = true
				return jobs.DeferStorageOperation(ctx, s.Store, c.OperationID, err)
			}
			return err
		})
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
	operation, err := s.Store.StorageOperation(ctx, c.OperationID)
	if err != nil {
		return err
	}
	if operation.CancelRequested {
		if _, err := s.Store.FinalizeLocalOperation(ctx, c.OperationID); err != nil {
			return err
		}
		return s.finish(ctx, c, store.StorageOperationCancelled, "checkout recovery cancelled; uncertain copies retain their source holds until retirement")
	}
	failure := "checkout creation was interrupted; partial copies are retained; unpublished source holds and destination reservation are released"
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
	} else if fsErr != nil {
		return jobs.DeferStorageAdmission(ctx, s.Store, c.OperationID, fmt.Errorf("inspecting checkout destination: %w", fsErr))
	}
	if root != nil {
		defer func() { _ = root.Close() }()
		entries, err := s.Store.PhotoCheckoutEntries(ctx, c.ID)
		if err != nil {
			return err
		}
		operation, err := s.Store.StorageOperation(ctx, c.OperationID)
		if err != nil {
			return err
		}
		if err := filepublish.CleanupRootStages(root); err != nil {
			return fmt.Errorf("cleaning checkout stages: %w", err)
		}
		var observed []string
		seen := map[string]bool{}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Published || e.FileID == operation.Cursor {
				// A committed copy or install intent may already contain user edits.
				_, err := root.Lstat(filepath.FromSlash(e.Path))
				if !errors.Is(err, os.ErrNotExist) {
					if err != nil {
						return jobs.DeferStorageAdmission(ctx, s.Store, c.OperationID, fmt.Errorf("file %s at %q: %w", e.FileID, e.Path, err))
					}
					observed = append(observed, e.FileID)
				}
				continue
			}
			if seen[e.Path] {
				continue
			}
			seen[e.Path] = true
			matches, err := observeCheckoutFile(ctx, root, e)
			if err != nil {
				// An unreadable unrecorded copy may still need its frozen source.
				return jobs.DeferStorageAdmission(ctx, s.Store, c.OperationID, fmt.Errorf("file %s at %q: %w", e.FileID, e.Path, err))
			}
			if matches {
				observed = append(observed, e.FileID)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.Store.ObservePhotoCheckout(ctx, c.ID, c.OperationID, observed); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if fsErr != nil && !errors.Is(fsErr, os.ErrNotExist) {
		failure += "; reconciliation failed: " + fsErr.Error()
	}
	cancelled, err := s.Store.FinalizeLocalOperation(ctx, c.OperationID)
	if err != nil {
		return err
	}
	state := store.StorageOperationFailed
	if cancelled {
		state = store.StorageOperationCancelled
	}
	return s.finish(ctx, c, state, failure)
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
			proof, err := s.checkDestination(ctx, root, c.Destination, c.ID)
			if err == nil {
				err = s.mutate(ctx, func() error { return s.recheckDestination(ctx, root, c.Destination, c.ID, proof) })
			}
			if err != nil {
				return errors.Join(err, s.finish(ctx, c, store.StorageOperationFailed, err.Error()))
			}
		}
	}
	entries, copyErr := s.Store.PhotoCheckoutEntries(ctx, c.ID)
	if copyErr == nil {
		var paths []string
		for _, entry := range entries {
			if !entry.Published {
				paths = append(paths, filepath.FromSlash(entry.Path))
			}
		}
		copyErr = filepublish.CheckRootPaths(ctx, root, paths)
		if pathErr, ok := errors.AsType[*os.PathError](copyErr); ok {
			for _, entry := range entries {
				if filepath.FromSlash(entry.Path) == pathErr.Path {
					copyErr = fmt.Errorf("asset %s file %s at %q: %w", entry.AssetID, entry.FileID, entry.Path, copyErr)
					break
				}
			}
		}
	}
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
			installed, err := s.copy(ctx, root, c.Destination, entry)
			if installed {
				copied++
				bytes += entry.Size
				err = errors.Join(err, s.Store.AdvanceStorageOperation(context.WithoutCancel(ctx), c.OperationID, entry.FileID, copied, copied, bytes, "{}"))
			}
			if err != nil {
				if errors.Is(err, store.ErrPhotoCheckoutPublicationUnrecorded) {
					// Recovery must adopt the installed copy before releasing its source hold.
					return err
				}
				if copyErr == nil {
					copyErr = err
				}
				if errors.Is(err, store.ErrPhotoCheckoutSourceUnavailable) {
					continue
				}
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

func (s Service) copy(ctx context.Context, root *os.Root, destination string, e store.PhotoCheckoutEntry) (installed bool, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("file %s at %q: %w", e.FileID, e.Path, retErr)
		}
	}()
	stage, err := filepublish.CreateRootStage(root)
	if err != nil {
		return false, err
	}
	defer func() { retErr = errors.Join(retErr, stage.Cleanup()) }()
	if err := stage.MakeParentDirectories(filepath.FromSlash(e.Path)); err != nil {
		return false, err
	}
	stream, size, err := s.Blobs.OpenStreamContext(ctx, e.Hash)
	if err != nil {
		return false, err
	}
	if size != e.Size {
		_ = stream.Close()
		return false, errors.New("checkout source size differs from frozen base")
	}
	_, copyErr := io.Copy(stage.File, stream)
	if err := errors.Join(copyErr, stream.Close()); err != nil {
		return false, err
	}
	if err := stage.File.Sync(); err != nil {
		return false, err
	}
	// Read the staged bytes back before publication to catch destination I/O damage.
	if _, err := stage.File.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, blob.ReaderWithContext(ctx, stage.File))
	if err != nil {
		return false, err
	}
	if n != e.Size || hex.EncodeToString(digest.Sum(nil)) != e.Hash {
		return false, errors.New("checkout staged bytes failed verification")
	}
	if err := stage.File.Close(); err != nil {
		return false, err
	}
	stage.File = nil
	retErr = s.mutate(ctx, func() error {
		return s.Store.PublishPhotoCheckoutEntry(ctx, e, func() (bool, error) {
			var err error
			installed, err = stage.Install(filepath.FromSlash(e.Path))
			return installed, errors.Join(err, checkDestinationIdentity(root, destination))
		})
	})
	if installed {
		var syncErr error
		if s.syncParent != nil {
			syncErr = s.syncParent(stage)
		} else {
			syncErr = stage.SyncParent()
		}
		retErr = errors.Join(retErr, syncErr)
	}
	return installed, retErr
}
