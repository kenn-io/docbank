package processing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

// PageWorker is one supervisor-owned local worker. SQL holds durable requests;
// this object only owns cancellation and the current process-lifetime claim.
type PageWorker struct {
	catalog *store.Store
	blobs   *blob.Store
	runtime *pagerender.Runtime
	gate    RenditionMutationGate
	active  chan struct{}
}

func NewPageWorker(catalog *store.Store, blobs *blob.Store, runtime *pagerender.Runtime, gate RenditionMutationGate) (*PageWorker, error) {
	if catalog == nil || blobs == nil || runtime == nil || gate == nil {
		return nil, pagerender.ErrUnavailable
	}
	return &PageWorker{catalog: catalog, blobs: blobs, runtime: runtime, gate: gate, active: make(chan struct{}, 1)}, nil
}

func (w *PageWorker) Run(ctx context.Context) error {
	if err := w.gate.MutateContext(ctx, func() error { return w.catalog.RequeuePageJobs(ctx) }); err != nil {
		return err
	}
	for {
		processed, err := w.RunOne(ctx)
		if err != nil {
			return err
		}
		if !processed {
			if err := waitRenditionWorker(ctx, time.Second); err != nil {
				return err
			}
		}
	}
}

func (w *PageWorker) RunOne(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	select {
	case w.active <- struct{}{}:
		defer func() { <-w.active }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	var claim store.PageJobClaim
	err := w.gate.MutateContext(ctx, func() error { var err error; claim, err = w.catalog.ClaimPageJob(ctx); return err })
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	work, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				if w.catalog.CheckPageClaim(work, claim) != nil {
					cancel()
					return
				}
			}
		}
	}()
	err = w.process(work, claim)
	cancel()
	<-stopped
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	state, failure := "completed", ""
	if err != nil {
		state = "failed"
		switch {
		case errors.Is(err, pagerender.ErrUnavailable):
			failure = "unavailable"
		case errors.Is(err, pagerender.ErrUnsupported):
			failure = "unsupported"
		case errors.Is(err, pagerender.ErrInvalidOutput):
			failure = "invalid_output"
		case errors.Is(err, store.ErrPageFenced), errors.Is(err, store.ErrNotFound):
			failure = "stale_source"
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			failure = "interrupted"
		default:
			failure = "storage"
		}
	}
	finishErr := w.gate.MutateContext(ctx, func() error { return w.catalog.FinishPageJob(ctx, claim, state, failure) })
	if errors.Is(finishErr, store.ErrPageFenced) {
		return true, nil
	}
	return true, finishErr
}

func (w *PageWorker) process(ctx context.Context, claim store.PageJobClaim) error {
	request := claim.Job.Request
	if request.RuntimeFingerprint != w.runtime.Fingerprint() {
		return pagerender.ErrUnavailable
	}
	inventory, err := w.catalog.PageInventory(ctx, request.Binding())
	if err != nil {
		return err
	}
	version, err := w.catalog.ContentVersionViewByID(ctx, request.NodeID, request.Source.VersionID)
	if err != nil {
		return err
	}
	var source []byte
	err = w.gate.MutateContext(ctx, func() error {
		if err := w.catalog.CheckPageClaim(ctx, claim); err != nil {
			return err
		}
		stream, size, err := w.blobs.OpenStreamContext(ctx, request.Source.SHA256)
		if err != nil {
			return err
		}
		defer func() { _ = stream.Close() }()
		if size != request.Source.Size || size > document.MaxPageSourceBytes {
			return pagerender.ErrInvalidOutput
		}
		source, err = io.ReadAll(io.LimitReader(stream, size+1))
		if err != nil {
			return err
		}
		if int64(len(source)) != size || !stream.Verified() {
			return pagerender.ErrInvalidOutput
		}
		return nil
	})
	if err != nil {
		return err
	}
	defer clear(source)
	if inventory.PageCount == 0 {
		frames, err := w.runtime.Inspect(ctx, source, request.Source, version.Version.MimeType)
		if err != nil {
			return err
		}
		if err := w.gate.MutateContext(ctx, func() error { return w.catalog.PublishPageFrames(ctx, claim, frames) }); err != nil {
			return err
		}
		inventory, err = w.catalog.PageInventory(ctx, request.Binding())
		if err != nil {
			return err
		}
	}
	var total int64
	for _, page := range request.Pages {
		if page > inventory.PageCount {
			return pagerender.ErrUnsupported
		}
		frame := inventory.Frames[page-1].Frame
		recipe, err := w.runtime.Recipe(frame, request.DPI)
		if err != nil {
			return err
		}
		_, recipeHash, err := document.MarshalPageRecipeV1(recipe)
		if err != nil {
			return err
		}
		existing, err := w.catalog.PageImage(ctx, request.Binding(), recipeHash, page)
		if err == nil {
			total += existing.Image.Size
			if total > document.MaxPageJobBytes {
				return store.ErrPageLimit
			}
			if err := w.gate.MutateContext(ctx, func() error { return w.catalog.PublishPageImage(ctx, claim, existing.Image, recipe, nil) }); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		receipt, output, err := w.runtime.Render(ctx, source, frame, recipe)
		if err != nil {
			return err
		}
		total += receipt.Size
		if total > document.MaxPageJobBytes {
			clear(output)
			return store.ErrPageLimit
		}
		err = w.gate.MutateContext(ctx, func() error {
			return w.blobs.WithMutation(ctx, func() error {
				if err := w.catalog.CheckPageClaim(ctx, claim); err != nil {
					return err
				}
				physical, err := w.blobs.WriteDetailedContext(ctx, bytes.NewReader(output))
				if err != nil {
					return err
				}
				encoding, err := physical.EncodingName()
				if err != nil {
					return err
				}
				authority := store.BlobPhysical{Encoding: encoding, StoredBytes: physical.StoredSize, PackEligible: physical.PackEligible, Created: physical.Created}
				if physical.Hash != receipt.SHA256 || physical.Size != receipt.Size {
					return pagerender.ErrInvalidOutput
				}
				if err := w.catalog.PublishPageImage(ctx, claim, receipt, recipe, &authority); err != nil {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					defer cancel()
					return errors.Join(err, w.catalog.RecordAbandonedPageBlob(cleanup, physical.Hash, physical.Size, authority))
				}
				return nil
			})
		})
		clear(output)
		if err != nil {
			return err
		}
	}
	return nil
}
