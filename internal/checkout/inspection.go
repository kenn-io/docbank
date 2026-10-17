package checkout

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func (s Service) PrepareOperation(ctx context.Context, id, kind, destination string) (store.PhotoCheckout, store.StorageOperation, error) {
	var c store.PhotoCheckout
	var op store.StorageOperation
	if err := creationMu.Acquire(ctx, 1); err != nil {
		return c, op, fmt.Errorf("waiting for checkout creation: %w", err)
	}
	defer creationMu.Release(1)
	request := store.PhotoCheckoutInspection{CheckoutID: id}
	current, err := s.Store.PhotoCheckout(ctx, id)
	if err != nil {
		return c, op, err
	}
	var root *os.Root
	if kind == store.StorageOperationKindPhotoCheckoutRebind {
		if err := store.ValidatePhotoCheckoutRebind(current, ""); err != nil {
			return c, op, err
		}
		if !filepath.IsAbs(destination) {
			return c, op, store.ErrInvalidPhotoCheckout
		}
		canonical, canonicalErr := filepath.EvalSymlinks(destination)
		if canonicalErr != nil {
			return c, op, fmt.Errorf("%w: destination must be an existing folder: %w", store.ErrInvalidPhotoCheckout, canonicalErr)
		}
		root, err = os.OpenRoot(canonical)
		destination = canonical
	} else {
		if err := store.ValidatePhotoCheckoutScanOrCommit(current, kind); err != nil {
			return c, op, err
		}
		root, err = openCheckoutRoot(current)
		destination = current.Destination
	}
	if err != nil {
		return c, op, fmt.Errorf("%w: opening destination: %w", store.ErrInvalidPhotoCheckout, err)
	}
	defer func() { _ = root.Close() }()
	var proof *ingest.PathProtection
	if kind == store.StorageOperationKindPhotoCheckoutRebind {
		proof, err = s.checkRootProtection(ctx, root, destination, id)
		if err != nil {
			return c, op, err
		}
		request.Destination = destination
		request.Identity, err = rootIdentity(root)
		if err != nil {
			return c, op, err
		}
		if err := store.ValidatePhotoCheckoutRebindDestination(current, request.Identity); err != nil {
			return c, op, err
		}
	}
	err = s.mutate(ctx, func() error {
		latest, err := s.Store.PhotoCheckout(ctx, id)
		if err != nil {
			return err
		}
		if latest.Destination != current.Destination || latest.DestinationIdentity != current.DestinationIdentity {
			return store.ErrPhotoCheckoutConflict
		}
		if kind == store.StorageOperationKindPhotoCheckoutRebind {
			if err := s.recheckRootProtection(ctx, root, destination, id, proof); err != nil {
				return err
			}
		}
		c, op, err = s.Store.PreparePhotoCheckoutOperation(ctx, request, kind)
		return err
	})
	return c, op, err
}

func (s Service) StartOperation(supervisor *jobs.Supervisor, c store.PhotoCheckout) error {
	if supervisor == nil {
		return errors.New("photo checkout requires a job supervisor")
	}
	err := supervisor.Start("storage:"+c.LastOperationID, func(ctx context.Context) error {
		return jobs.RetryStorageOperation(ctx, time.Second, func() error {
			if c.OperationKind == store.StorageOperationKindPhotoCheckoutCommit {
				return s.RunCommit(ctx, c)
			}
			return s.RunInspection(ctx, c)
		})
	})
	if err != nil && !errors.Is(err, jobs.ErrDuplicate) {
		return errors.Join(err, s.Store.FinishPhotoCheckoutOperation(context.Background(), c, store.StorageOperationFailed, "{}", err.Error()))
	}
	return err
}

const checkoutCheckpointFiles = 64

const checkoutCheckpointInterval = 250 * time.Millisecond

type checkoutProgress struct {
	result          ScanResult
	reported        int64
	reportedFiles   int64
	reportedAt      time.Time
	checkedAt       time.Time
	checkedBytes    int64
	filesSinceCheck int
}

func (s Service) verificationProgress(ctx context.Context, c store.PhotoCheckout) (*checkoutProgress, error) {
	entries, err := s.Store.PhotoCheckoutEntries(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	p := &checkoutProgress{result: ScanResult{Untracked: []string{}}}
	for _, e := range entries {
		p.result.TotalBytes += e.Size
	}
	return p, nil
}

// checkpoint checks admission before the first file and at bounded file, byte,
// or time intervals. Context cancellation remains immediate between chunks.
func (p *checkoutProgress) checkpoint(ctx context.Context, s Service, id string, beforeFile bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if beforeFile {
		p.filesSinceCheck++
	}
	if !p.checkedAt.IsZero() && p.filesSinceCheck < checkoutCheckpointFiles && p.result.HashedBytes-p.checkedBytes < 4<<20 && time.Since(p.checkedAt) < checkoutCheckpointInterval {
		return nil
	}
	p.checkedAt = time.Now()
	cancelled, err := s.admitOperation(ctx, id)
	if err != nil {
		return jobs.DeferStorageAdmission(ctx, s.Store, id, err)
	}
	if cancelled {
		return errCommitCancelled
	}
	p.checkedBytes, p.filesSinceCheck = p.result.HashedBytes, 0
	return nil
}

func (p *checkoutProgress) report(ctx context.Context, s Service, id string, n int64, complete bool) error {
	p.result.HashedBytes += n
	if complete {
		p.result.Files++
	}
	if err := p.checkpoint(ctx, s, id, false); err != nil {
		return err
	}
	// Publish bounded updates; finishing the operation writes the exact totals.
	if !p.reportedAt.IsZero() && p.result.HashedBytes-p.reported < 4<<20 && p.result.Files-p.reportedFiles < checkoutCheckpointFiles && time.Since(p.reportedAt) < checkoutCheckpointInterval {
		return nil
	}
	if p.result.HashedBytes > p.result.TotalBytes {
		p.result.TotalBytes = p.result.HashedBytes
	}
	encoded, err := json.Marshal(p.result)
	if err != nil {
		return err
	}
	if err := s.mutate(ctx, func() error { return s.Store.ReportPhotoCheckoutProgress(ctx, id, p.result.Files, string(encoded)) }); err != nil {
		return err
	}
	p.reported, p.reportedFiles, p.reportedAt = p.result.HashedBytes, p.result.Files, time.Now()
	return nil
}

func (s Service) RunInspection(ctx context.Context, c store.PhotoCheckout) error {
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
	op, err := s.Store.ClaimStorageOperation(ctx, c.LastOperationID)
	if err != nil {
		return err
	}
	p, err := s.verificationProgress(ctx, c)
	if err != nil {
		return err
	}
	var result ScanResult
	if !cancelled {
		hooks := scanHooks{progress: func(ctx context.Context, n int64, complete bool) error {
			return p.report(ctx, s, c.LastOperationID, n, complete)
		}}
		hooks.before = func(ctx context.Context, _ string) error {
			return p.checkpoint(ctx, s, c.LastOperationID, true)
		}
		hooks.finalize = func(ctx context.Context) error {
			cancelled, err := s.Store.FinalizeLocalOperation(ctx, c.LastOperationID)
			if err != nil {
				return err
			}
			if cancelled {
				return errCommitCancelled
			}
			return nil
		}
		if c.OperationKind == store.StorageOperationKindPhotoCheckoutRebind {
			var request store.PhotoCheckoutInspection
			err = json.Unmarshal([]byte(op.RequestJSON), &request)
			if err == nil {
				root, openErr := os.OpenRoot(request.Destination)
				if openErr != nil {
					err = openErr
				} else {
					err = verifyRootIdentity(root, request.Identity)
					if err == nil {
						result, err = s.rebind(ctx, c.ID, request.Destination, c.LastOperationID, root, hooks)
					}
					err = errors.Join(err, root.Close())
				}
			}
		} else {
			c.State = "completed"
			result, err = s.scanWorking(ctx, c, nil, nil, hooks)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, jobs.ErrStorageOperationDeferred) {
		return err
	}
	cancelled = cancelled || errors.Is(err, errCommitCancelled)
	if !cancelled && err == nil {
		p.result.TotalBytes = p.result.HashedBytes
	}
	result.HashedBytes, result.TotalBytes, result.Files = p.result.HashedBytes, p.result.TotalBytes, p.result.Files
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return marshalErr
	}
	state, failure := store.StorageOperationCompleted, ""
	if cancelled {
		state = store.StorageOperationCancelled
		err = nil
	} else if err != nil {
		state, failure = store.StorageOperationFailed, err.Error()
	}
	return errors.Join(err, s.Store.FinishPhotoCheckoutOperation(ctx, c, state, string(encoded), failure))
}
