package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
		return checkout, operation, nil, fmt.Errorf("%w: destination must be an existing folder: %v", store.ErrInvalidPhotoCheckout, err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return checkout, operation, nil, fmt.Errorf("%w: opening destination: %v", store.ErrInvalidPhotoCheckout, err)
	}
	err = s.mutate(ctx, func() error {
		if err := s.validateDestination(ctx, root, canonical); err != nil {
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

func (s Service) validateDestination(ctx context.Context, root *os.Root, destination string) error {
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
		if c.Destination != "" {
			roots = append(roots, c.Destination)
		}
	}
	for _, other := range roots {
		if other == "" {
			continue
		}
		overlap, err := ingest.PathsOverlap(destination, other)
		if overlap {
			return fmt.Errorf("%w: destination overlaps a vault, storage root, watched folder, or checkout", store.ErrInvalidPhotoCheckout)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, watch := range s.Config.Watches {
		_, overlap, err := ingest.WatchBindingOverlap([]config.WatchConfig{watch}, config.StoreBindingConfig{Kind: "filesystem", Path: destination})
		if overlap {
			return fmt.Errorf("%w: destination overlaps a watched folder", store.ErrInvalidPhotoCheckout)
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

func (s Service) Start(supervisor *jobs.Supervisor, checkout store.PhotoCheckout, root *os.Root) error {
	var err error
	if supervisor == nil {
		err = errors.New("photo checkout requires a job supervisor")
	} else {
		err = supervisor.Start("storage:"+checkout.OperationID, func(ctx context.Context) error {
			defer func() { _ = root.Close() }()
			return s.Run(ctx, checkout, root)
		})
	}
	if err != nil {
		_ = root.Close()
		return errors.Join(err, s.finish(context.Background(), checkout, store.StorageOperationFailed, "{}", err.Error()))
	}
	return nil
}

// Recover preserves published copies and removes only recognized private stages.
func (s Service) Recover(ctx context.Context) error {
	operations, err := s.Store.ResumableStorageOperations(ctx)
	if err != nil {
		return err
	}
	checkouts, err := s.Store.ListPhotoCheckouts(ctx)
	if err != nil {
		return err
	}
	byOperation := map[string]store.PhotoCheckout{}
	for _, c := range checkouts {
		byOperation[c.OperationID] = c
	}
	for _, op := range operations {
		if op.Kind != store.StorageOperationKindPhotoCheckout {
			continue
		}
		c, exists := byOperation[op.ID]
		if !exists {
			return fmt.Errorf("checkout operation %s has no definition", op.ID)
		}
		failure := "checkout creation was interrupted; partial copies and frozen bases are retained"
		if c.Destination != "" {
			canonical, err := filepath.EvalSymlinks(c.Destination)
			var root *os.Root
			if err == nil && canonical == c.Destination {
				root, err = os.OpenRoot(c.Destination)
			}
			if root != nil {
				entries, entriesErr := s.Store.PhotoCheckoutEntries(ctx, c.ID)
				if entriesErr != nil {
					_ = root.Close()
					return entriesErr
				}
				var verified []string
				cleaned := map[string]bool{}
				for _, e := range entries {
					parent := filepath.Dir(filepath.FromSlash(e.Path))
					if !cleaned[parent] {
						entriesErr = errors.Join(entriesErr, filepublish.CleanupRootStages(root, parent))
						cleaned[parent] = true
					}
					matches, verifyErr := observeCheckoutFile(root, e)
					entriesErr = errors.Join(entriesErr, verifyErr)
					if matches {
						verified = append(verified, e.FileID)
					}
				}
				if err := s.Store.ObservePhotoCheckout(ctx, c.ID, c.OperationID, verified); err != nil {
					_ = root.Close()
					return err
				}
				err = errors.Join(entriesErr, root.Close())
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				failure += "; staging cleanup failed: " + err.Error()
			}
		}
		if err := s.finish(ctx, c, store.StorageOperationFailed, op.ReceiptJSON, failure); err != nil {
			return err
		}
	}
	return nil
}

func observeCheckoutFile(root *os.Root, entry store.PhotoCheckoutEntry) (bool, error) {
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
	size, readErr := io.Copy(digest, file)
	err = errors.Join(readErr, file.Close())
	return err == nil && size == entry.Size && hex.EncodeToString(digest.Sum(nil)) == entry.Hash, err
}

func (s Service) finish(ctx context.Context, c store.PhotoCheckout, state store.StorageOperationState, receipt, failure string) error {
	return s.Store.FinishPhotoCheckout(ctx, c.ID, c.OperationID, state, receipt, failure, time.Now().Add(30*24*time.Hour))
}

func (s Service) Run(ctx context.Context, c store.PhotoCheckout, root *os.Root) error {
	if s.ActivityBegin != nil {
		s.ActivityBegin()
	}
	if s.ActivityEnd != nil {
		defer s.ActivityEnd()
	}
	cancelled, err := jobs.AdmitStorageOperation(ctx, s.Store, c.OperationID)
	if err != nil {
		return s.finish(context.WithoutCancel(ctx), c, store.StorageOperationFailed, "{}", err.Error())
	}
	if _, err := s.Store.ClaimStorageOperation(ctx, c.OperationID); err != nil {
		return err
	}
	if err := s.Store.SetPhotoCheckoutState(ctx, c.ID, "running", ""); err != nil {
		return err
	}
	entries, copyErr := s.Store.PhotoCheckoutEntries(ctx, c.ID)
	var copied, bytes int64
	if copyErr == nil && !cancelled {
		for _, entry := range entries {
			cancelled, copyErr = jobs.AdmitStorageOperation(ctx, s.Store, c.OperationID)
			if copyErr != nil || cancelled {
				break
			}
			copyErr = s.copy(ctx, root, c.Destination, entry)
			if copyErr != nil {
				break
			}
			copied++
			bytes += entry.Size
			copyErr = s.Store.AdvanceStorageOperation(ctx, c.OperationID, entry.FileID, copied, copied, bytes, "{}")
			if copyErr != nil {
				break
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if copyErr == nil && !cancelled {
		copyErr = checkDestinationIdentity(root, c.Destination)
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
	return errors.Join(copyErr, s.finish(context.WithoutCancel(ctx), c, state, "{}", failure))
}

func checkDestinationIdentity(root *os.Root, destination string) error {
	held, heldErr := root.Stat(".")
	named, namedErr := os.Stat(destination)
	if heldErr != nil || namedErr != nil || !os.SameFile(held, named) {
		return fmt.Errorf("%w: destination changed during checkout: %v", store.ErrInvalidPhotoCheckout, errors.Join(heldErr, namedErr))
	}
	return nil
}

func (s Service) copy(ctx context.Context, root *os.Root, destination string, e store.PhotoCheckoutEntry) (retErr error) {
	parent := filepath.Dir(filepath.FromSlash(e.Path))
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := filepublish.CreateRootStage(root, parent)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, stage.Cleanup()) }()
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
	n, err = io.Copy(digest, stage.File)
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
			if err := checkDestinationIdentity(root, destination); err != nil {
				return false, err
			}
			installed, err := stage.Publish(filepath.FromSlash(e.Path))
			return installed, errors.Join(err, checkDestinationIdentity(root, destination))
		})
	})
}
