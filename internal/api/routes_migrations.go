package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"go.kenn.io/docbank/internal/photomigration/fotobank"
	"go.kenn.io/docbank/internal/store"
)

func registerMigrationRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "createFotobankInventory", Method: http.MethodPost,
		Path:          "/api/v1/migrations/fotobank/inventories",
		Summary:       "Inventory a stopped Fotobank install or recovery archive",
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct{ Body FotobankInventoryRequest }) (*migrationRunOutput, error) {
		if browserSessionRequest(ctx) {
			return nil, NewError(http.StatusForbidden, "browser_forbidden", "browser sessions cannot inventory migration sources")
		}
		if d.Store == nil {
			return nil, errors.New("migration inventory requires a vault")
		}
		request := in.Body
		install := request.CatalogPath != "" || request.VaultRoot != ""
		archive := request.ArchiveRoot != ""
		if install == archive || install && (request.CatalogPath == "" || request.VaultRoot == "") || archive && (request.CatalogPath != "" || request.VaultRoot != "") {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_inventory_source", "choose an install or an archive")
		}
		if request.OwnerMapPath == "" || !filepath.IsAbs(request.OwnerMapPath) {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_owner_map_path", "owner_map_path must be absolute")
		}
		var out *migrationRunOutput
		err := g.mutate(func() error {
			report, template, err := fotobank.Inventory(ctx, d.Store.SQLiteDriver(), fotobank.Request{
				CatalogPath: request.CatalogPath, VaultRoot: request.VaultRoot,
				ArchiveRoot: request.ArchiveRoot, SnapshotID: request.SnapshotID,
				OwnerMapPath: request.OwnerMapPath, DestinationRoot: d.VaultRoot,
			})
			if err != nil {
				return inventoryError(err)
			}
			run := store.PhotoMigrationRun{ID: uuid.NewString(), Source: report.Source, CreatedAt: report.CreatedAt, Report: report, OwnerMap: template}
			if err := d.Store.SavePhotoMigrationRun(ctx, run); err != nil {
				return errors.Join(FromStoreError(err), os.Remove(request.OwnerMapPath))
			}
			body := fromPhotoMigrationRun(run)
			body.OwnerMapPath = request.OwnerMapPath
			out = &migrationRunOutput{Body: body}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "listMigrationRuns", Method: http.MethodGet,
		Path: "/api/v1/migrations/runs", Summary: "List completed photo migration inventories",
	}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" minimum:"1" maximum:"50"`
		Offset int `query:"offset" minimum:"0"`
	}) (*migrationRunPageOutput, error) {
		if browserSessionRequest(ctx) {
			return nil, NewError(http.StatusForbidden, "browser_forbidden", "browser sessions cannot read migration runs")
		}
		limit := in.Limit
		if limit == 0 {
			limit = 50
		}
		page, err := d.Store.ListPhotoMigrationRuns(ctx, in.Offset, limit)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := MigrationRunPage{Total: page.Total, Items: make([]MigrationRun, 0, len(page.Items))}
		for _, run := range page.Items {
			out.Items = append(out.Items, fromPhotoMigrationRun(run))
		}
		return &migrationRunPageOutput{Body: out}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getMigrationRun", Method: http.MethodGet,
		Path: "/api/v1/migrations/runs/{run_id}", Summary: "Read one completed photo migration inventory",
	}, func(ctx context.Context, in *struct {
		RunID string `path:"run_id"`
	}) (*migrationRunOutput, error) {
		if browserSessionRequest(ctx) {
			return nil, NewError(http.StatusForbidden, "browser_forbidden", "browser sessions cannot read migration runs")
		}
		run, err := d.Store.PhotoMigrationRun(ctx, in.RunID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &migrationRunOutput{Body: fromPhotoMigrationRun(run)}, nil
	})
}

func inventoryError(err error) error {
	var code, status = "inventory_failed", http.StatusUnprocessableEntity
	switch {
	case errors.Is(err, fotobank.ErrSourceRunning):
		code = "fotobank_running"
	case errors.Is(err, fotobank.ErrSourceWAL):
		code = "fotobank_wal_pending"
	case errors.Is(err, fotobank.ErrSchemaMismatch):
		code = "fotobank_schema_mismatch"
	case errors.Is(err, fotobank.ErrEmbeddedSchemaMismatch):
		code = "embedded_docbank_schema_mismatch"
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	}
	return NewError(status, code, err.Error())
}
