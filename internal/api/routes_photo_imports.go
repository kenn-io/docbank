package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

type photoImportRunOutput struct {
	Body PhotoImportRun
}

// NewPhotoImportRunner builds the runner the daemon uses for new and resumed
// photo import operations.
func NewPhotoImportRunner(d Deps) ingest.PhotoImportRunner {
	runner := ingest.PhotoImportRunner{
		Ingester: &ingest.Ingester{Store: d.Store, Blobs: d.Blobs},
		Options:  ingest.PhotoImportOptions{SettleInterval: ingest.DefaultPhotoImportSettleInterval},
	}
	if d.Gate != nil {
		runner.Options.Mutate = d.Gate.MutateContext
	}
	if d.Tracker != nil {
		runner.Options.ActivityBegin = d.Tracker.Begin
		runner.Options.ActivityEnd = d.Tracker.End
	}
	return runner
}

func photoImportOperation(ctx context.Context, d Deps, id string) (store.StorageOperation, error) {
	operation, err := d.Store.StorageOperation(ctx, id)
	if err != nil {
		return store.StorageOperation{}, FromStoreError(err)
	}
	if operation.Kind != store.StorageOperationKindPhotoImport {
		return store.StorageOperation{}, FromStoreError(store.ErrNotFound)
	}
	return operation, nil
}

func registerPhotoImportRoutes(api huma.API, d Deps, g *gate) {
	runnerDeps := d
	runnerDeps.Gate = g
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
		if d.Jobs == nil {
			return nil, NewError(http.StatusServiceUnavailable, "photo_import_unavailable", "background job supervisor is unavailable")
		}
		request, err := json.Marshal(store.PhotoImportRequest{SourceRoot: in.Body.SourceRoot, Destination: in.Body.Destination})
		if err != nil {
			return nil, err
		}
		operation, err := d.Store.CreateLocalOperation(ctx, store.StorageOperationKindPhotoImport, string(request))
		if err != nil {
			return nil, FromStoreError(err)
		}
		runner := NewPhotoImportRunner(runnerDeps)
		if err := runner.Start(d.Jobs, operation.ID); err != nil {
			d.Logger.Error("start queued photo import", "operation_id", operation.ID, "error", err)
		}
		return &photoImportRunOutput{Body: fromStorePhotoImport(operation, false)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listPhotoImports", Method: http.MethodGet,
		Path: "/api/v1/photos/imports", Summary: "List photo imports, newest first",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body PhotoImportRunList }, error) {
		operations, err := d.Store.StorageOperations(ctx, 1000)
		if err != nil {
			return nil, FromStoreError(err)
		}
		browser := browserSessionRequest(ctx)
		items := make([]PhotoImportRun, 0)
		for _, operation := range operations {
			if operation.Kind == store.StorageOperationKindPhotoImport {
				items = append(items, fromStorePhotoImport(operation, browser))
			}
		}
		return &struct{ Body PhotoImportRunList }{Body: PhotoImportRunList{Items: items}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getPhotoImport", Method: http.MethodGet,
		Path: "/api/v1/photos/imports/{run_id}", Summary: "Inspect one photo import and its ambiguous groups",
	}, func(ctx context.Context, in *struct {
		RunID string `path:"run_id"`
	}) (*photoImportRunOutput, error) {
		operation, err := photoImportOperation(ctx, d, in.RunID)
		if err != nil {
			return nil, err
		}
		return &photoImportRunOutput{Body: fromStorePhotoImport(operation, browserSessionRequest(ctx))}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "cancelPhotoImport", Method: http.MethodPost,
		Path: "/api/v1/photos/imports/{run_id}/cancel", Summary: "Request cancellation before the next photo group",
	}, func(ctx context.Context, in *struct {
		RunID string `path:"run_id"`
	}) (*photoImportRunOutput, error) {
		if _, err := photoImportOperation(ctx, d, in.RunID); err != nil {
			return nil, err
		}
		if err := d.Store.RequestStorageOperationCancel(ctx, in.RunID); err != nil {
			if errors.Is(err, store.ErrStorageOperationTerminal) {
				return nil, NewError(http.StatusConflict, "photo_import_terminal", err.Error())
			}
			return nil, FromStoreError(err)
		}
		operation, err := photoImportOperation(ctx, d, in.RunID)
		if err != nil {
			return nil, err
		}
		return &photoImportRunOutput{Body: fromStorePhotoImport(operation, browserSessionRequest(ctx))}, nil
	})
}
