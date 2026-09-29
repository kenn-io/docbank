package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

type photoImportRunOutput struct {
	ETag string `header:"ETag"`
	Body PhotoImportRun
}

func photoImportError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrPhotoImportAmbiguous) {
		return NewError(http.StatusConflict, "photo_import_ambiguous", err.Error())
	}
	if errors.Is(err, store.ErrStorageOperationTerminal) {
		return NewError(http.StatusConflict, "photo_import_terminal", err.Error())
	}
	return FromStoreError(err)
}

func registerPhotoImportRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "startPhotoImport", Method: http.MethodPost,
		Path: "/api/v1/photos/imports", Summary: "Import grouped camera files from a daemon-host folder",
		DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *struct{ Body PhotoImportStartRequest }) (*photoImportRunOutput, error) {
		if !filepath.IsAbs(in.Body.SourceRoot) {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "source_root must be an absolute daemon-host path")
		}
		if in.Body.Destination == "" || in.Body.Destination[0] != '/' {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "destination must be an absolute vault path")
		}
		if choice := in.Body.Choice; choice != nil {
			if err := store.ValidatePhotoImportChoice(fromStorePhotoImportChoice(choice)); err != nil {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
			}
		}
		var run store.PhotoImportRun
		var err error
		mutate := func() error {
			var startErr error
			run, startErr = d.Store.StartPhotoImportRun(ctx, in.Body.SourceRoot, in.Body.Destination, 0)
			return startErr
		}
		if g != nil {
			err = g.mutate(mutate)
		} else {
			err = mutate()
		}
		if err != nil {
			return nil, photoImportError(err)
		}
		worker := func(runCtx context.Context) error {
			ing := &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}
			opts := ingest.PhotoImportOptions{
				RunID: run.ID, Choice: fromStorePhotoImportChoice(in.Body.Choice),
				SettleInterval: ingest.DefaultPhotoImportSettleInterval,
			}
			if g != nil {
				opts.Mutate = g.MutateContext
			}
			if d.Tracker != nil {
				opts.ActivityBegin = d.Tracker.Begin
				opts.ActivityEnd = d.Tracker.End
			}
			_, importErr := ing.ImportPhotoDirectory(runCtx, in.Body.SourceRoot, in.Body.Destination, opts)
			return importErr
		}
		if d.Jobs != nil {
			if err := d.Jobs.Start("photo-import:"+run.ID, worker); err != nil {
				_, _ = d.Store.FinishPhotoImportRun(context.Background(), run.ID, store.PhotoImportStateFailed, err.Error())
				return nil, fmt.Errorf("starting photo import worker: %w", err)
			}
		} else {
			go func() { _ = worker(context.WithoutCancel(ctx)) }()
		}
		return &photoImportRunOutput{ETag: revisionETag(run.Revision), Body: fromStorePhotoImportRun(run, false)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listPhotoImports", Method: http.MethodGet,
		Path: "/api/v1/photos/imports", Summary: "List durable grouped photo import runs",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body PhotoImportRunList }, error) {
		runs, err := d.Store.ListPhotoImportRuns(ctx, 100)
		if err != nil {
			return nil, photoImportError(err)
		}
		browser := browserSessionRequest(ctx)
		items := make([]PhotoImportRun, 0, len(runs))
		for _, run := range runs {
			items = append(items, fromStorePhotoImportRun(run, browser))
		}
		return &struct{ Body PhotoImportRunList }{Body: PhotoImportRunList{Items: items}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getPhotoImport", Method: http.MethodGet,
		Path: "/api/v1/photos/imports/{run_id}", Summary: "Inspect one durable grouped photo import run",
	}, func(ctx context.Context, in *struct {
		RunID string `path:"run_id"`
	}) (*photoImportRunOutput, error) {
		run, err := d.Store.PhotoImportRun(ctx, in.RunID)
		if err != nil {
			return nil, photoImportError(err)
		}
		browser := browserSessionRequest(ctx)
		return &photoImportRunOutput{ETag: revisionETag(run.Revision), Body: fromStorePhotoImportRun(run, browser)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "cancelPhotoImport", Method: http.MethodPost,
		Path: "/api/v1/photos/imports/{run_id}/cancel", Summary: "Request cancellation at the next photo group",
	}, func(ctx context.Context, in *struct {
		RunID   string `path:"run_id"`
		IfMatch string `header:"If-Match"`
	}) (*photoImportRunOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var run store.PhotoImportRun
		mutate := func() error {
			var callErr error
			run, callErr = d.Store.RequestPhotoImportCancel(ctx, in.RunID, revision)
			if errors.Is(callErr, store.ErrStaleRevision) {
				current, getErr := d.Store.PhotoImportRun(ctx, in.RunID)
				if getErr != nil {
					return getErr
				}
				run, callErr = d.Store.RequestPhotoImportCancel(ctx, in.RunID, current.Revision)
			}
			return callErr
		}
		if g != nil {
			err = g.mutate(mutate)
		} else {
			err = mutate()
		}
		if err != nil {
			return nil, photoImportError(err)
		}
		return &photoImportRunOutput{ETag: revisionETag(run.Revision), Body: fromStorePhotoImportRun(run, browserSessionRequest(ctx))}, nil
	})
}
