package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

type photoImportStartOutput struct {
	Body StorageOperation
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

func registerPhotoImportRoutes(api huma.API, d Deps, g *gate) {
	runnerDeps := d
	runnerDeps.Gate = g
	huma.Register(api, huma.Operation{
		OperationID: "startPhotoImport", Method: http.MethodPost,
		Path: "/api/v1/photos/imports", Summary: "Import grouped camera files from a daemon-host folder",
		DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *struct{ Body PhotoImportStartRequest }) (*photoImportStartOutput, error) {
		if !filepath.IsAbs(in.Body.SourceRoot) {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "source_root must be an absolute daemon-host path")
		}
		if info, err := os.Stat(in.Body.SourceRoot); err != nil || !info.IsDir() {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("source_root %q is not an existing daemon-host folder", in.Body.SourceRoot))
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
		return &photoImportStartOutput{Body: storageOperationAPI(operation)}, nil
	})
}
