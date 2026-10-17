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
		Ingester:     &ingest.Ingester{Store: d.Store, Blobs: d.Blobs},
		ImportResult: func(id string, added, terminal bool) { d.Imports.Result("photos:"+id, added, terminal) },
		Options:      ingest.PhotoImportOptions{SettleInterval: ingest.DefaultPhotoImportSettleInterval},
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
	}, func(ctx context.Context, in *struct {
		Body    PhotoImportStartRequest
		Surface string `header:"X-Docbank-Surface"`
	}) (*photoImportStartOutput, error) {
		if !filepath.IsAbs(in.Body.SourceRoot) {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "source_root must be an absolute daemon-host path")
		}
		if info, err := os.Stat(in.Body.SourceRoot); err != nil || !info.IsDir() {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("source_root %q is not an existing daemon-host folder", in.Body.SourceRoot))
		}
		if problem := validatePhotoImportDestination(in.Body.Destination); problem != nil {
			return nil, problem
		}
		if d.Jobs == nil {
			return nil, NewError(http.StatusServiceUnavailable, "photo_import_unavailable", "background job supervisor is unavailable")
		}
		request, err := json.Marshal(store.PhotoImportRequest{SourceRoot: in.Body.SourceRoot, Destination: in.Body.Destination})
		if err != nil {
			return nil, err
		}
		var operation store.StorageOperation
		surface := ""
		if in.Surface == "cli" {
			surface = "cli"
		}
		err = d.Imports.Admit(surface, func() (string, bool, error) {
			err := g.MutateContext(ctx, func() error {
				var createErr error
				operation, createErr = d.Store.CreateLocalOperation(ctx, store.StorageOperationKindPhotoImport, string(request))
				return createErr
			})
			return "photos:" + operation.ID, true, err
		})
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

func validatePhotoImportDestination(destination string) *Error {
	if destination == "" || destination[0] != '/' {
		return NewError(http.StatusUnprocessableEntity, "validation",
			"destination must be an absolute vault path")
	}
	return nil
}
