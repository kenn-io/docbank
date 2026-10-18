package checkout

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

// RunCommit settles working files, publishes verified bytes and conditionally advances their bases.
func (s Service) RunCommit(ctx context.Context, c store.PhotoCheckout) error {
	if s.ActivityBegin != nil {
		s.ActivityBegin()
		if s.ActivityEnd != nil {
			defer s.ActivityEnd()
		}
	}
	cancelled, err := s.admitOperation(ctx, c.LastOperationID)
	if err != nil {
		return jobs.DeferStorageAdmission(ctx, s.Store, c.LastOperationID, err)
	}
	if _, err := s.Store.ClaimStorageOperation(ctx, c.LastOperationID); err != nil {
		return err
	}
	var result ScanResult
	progress, err := s.verificationProgress(ctx, c)
	if err != nil {
		return err
	}
	finish := func(failure error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state, message := store.StorageOperationCompleted, ""
		if cancelled {
			state = store.StorageOperationCancelled
		} else if failure != nil {
			state, message = store.StorageOperationFailed, failure.Error()
		}
		if state == store.StorageOperationCompleted || progress.result.HashedBytes > progress.result.TotalBytes {
			progress.result.TotalBytes = progress.result.HashedBytes
		}
		result.HashedBytes, result.TotalBytes, result.Files = progress.result.HashedBytes, progress.result.TotalBytes, progress.result.Files
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return errors.Join(failure, s.Store.FinishPhotoCheckoutOperation(ctx, c, state, string(encoded), message))
	}
	if cancelled {
		return finish(nil)
	}
	completed := make(map[string]bool)
	var path string
	before := func(ctx context.Context, ref string) error {
		path = ref
		return progress.checkpoint(ctx, s, c.LastOperationID, true)
	}
	var pending, conflicts, edited []string
	var only map[string]bool
	// hashed marks retried files whose previous pass recorded their bytes. A
	// file that changed mid-read has no baseline yet, so it gets one more pass.
	hashed := make(map[string]bool)
	for pass := range 3 {
		// Recheck admission before a retry, even when the settlement wait is short.
		if pass > 0 {
			progress.filesSinceCheck = checkoutCheckpointFiles
		}
		var scanErr error
		result, scanErr = s.scanWorking(ctx, c, func(ctx context.Context, file ingest.FolderFile, e store.PhotoCheckoutEntry) error {
			return s.commitEntry(ctx, c, file, e)
		}, nil, scanHooks{before: before, only: only, progress: func(ctx context.Context, n int64, complete bool) error {
			if complete {
				if completed[path] {
					complete = false
				} else {
					completed[path] = true
				}
			}
			return progress.report(ctx, s, c.LastOperationID, n, complete)
		}})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(scanErr, jobs.ErrStorageOperationDeferred) {
			return scanErr
		}
		if errors.Is(scanErr, errCommitCancelled) {
			cancelled = true
		}
		if cancelled {
			return finish(nil)
		}
		if scanErr != nil {
			return finish(scanErr)
		}
		entries, err := s.Store.PhotoCheckoutEntries(ctx, c.ID)
		if err != nil {
			return finish(err)
		}
		pending, conflicts, edited = nil, nil, nil
		retry := make(map[string]bool)
		for _, entry := range entries {
			switch entry.WorkingState {
			case "conflict":
				conflicts = append(conflicts, entry.Path)
			case "pending":
				pending = append(pending, entry.Path)
				if !hashed[entry.FileID] {
					retry[entry.FileID] = true
				}
			case "edited":
				edited = append(edited, entry.Path)
			}
		}
		if len(retry) == 0 || pass == 2 {
			break
		}
		only = retry
		hashed = make(map[string]bool)
		for _, entry := range entries {
			if retry[entry.FileID] && entry.ObservedHash != "" {
				hashed[entry.FileID] = true
			}
		}
		if err := jobs.Wait(ctx, s.settlement()); err != nil {
			return fmt.Errorf("settling checkout files: %w", err)
		}
	}

	cancelled, err = s.Store.FinalizeLocalOperation(ctx, c.LastOperationID)
	if err != nil {
		return err
	}
	if cancelled {
		return finish(nil)
	}
	var failure error
	if len(conflicts) > 0 {
		failure = fmt.Errorf("%w: checkout conflicts: %s", store.ErrStaleRevision, strings.Join(conflicts, ", "))
	}
	if len(pending) > 0 {
		failure = errors.Join(failure, fmt.Errorf("%w: checkout pending: %s", ingest.ErrSourceChanged, strings.Join(pending, ", ")))
	}
	if len(edited) > 0 {
		failure = errors.Join(failure, fmt.Errorf("%w: checkout edited: %s", ingest.ErrSourceChanged, strings.Join(edited, ", ")))
	}
	return finish(failure)
}

var errCommitCancelled = errors.New("checkout commit cancelled")

func (s Service) commitEntry(ctx context.Context, c store.PhotoCheckout, file ingest.FolderFile, e store.PhotoCheckoutEntry) error {
	return s.mutate(ctx, func() error {
		return s.Blobs.WithMutation(ctx, func() error {
			if committed, err := s.Store.CheckPhotoCheckoutCommitEntry(ctx, c, e); err != nil || committed {
				return err
			}
			if _, err := file.File.Seek(0, io.SeekStart); err != nil {
				return err
			}
			head, err := io.ReadAll(io.LimitReader(file.File, 64*1024))
			if err != nil {
				return err
			}
			mediaType := ingest.DetectMIME(e.Path, head)
			if _, err := file.File.Seek(0, io.SeekStart); err != nil {
				return err
			}
			ing := ingest.Ingester{Store: s.Store, Blobs: s.Blobs}
			_, err = ing.CommitVerifiedContent(ctx, file.File, e.ObservedHash, e.ObservedSize, func(physical store.BlobPhysical) error {
				if err := file.Unchanged(); err != nil {
					return err
				}
				return s.Store.CommitPhotoCheckoutEntry(ctx, c, e, e.ObservedHash, e.ObservedSize, mediaType, physical)
			})
			if errors.Is(err, ingest.ErrUploadSizeMismatch) || errors.Is(err, ingest.ErrUploadDigestMismatch) {
				return ingest.ErrSourceChanged
			}
			return err
		})
	})
}
