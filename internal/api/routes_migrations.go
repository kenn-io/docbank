package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/photomigration/fotobank"
	"go.kenn.io/docbank/internal/store"
)

func registerMigrationRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "createFotobankInventory", Method: http.MethodPost,
		Path:          "/api/v1/migrations/fotobank/inventories",
		Summary:       "Inventory a stopped Fotobank install or recovery archive",
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct{ Body FotobankInventoryRequest }) (*fotobankInventoryOutput, error) {
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
		if request.OutputDir == "" || !filepath.IsAbs(request.OutputDir) {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_output_dir", "output_dir must be absolute")
		}
		result, err := fotobank.Inventory(ctx, d.Store.SQLiteDriver(), fotobank.Request{
			CatalogPath: request.CatalogPath, VaultRoot: request.VaultRoot, ArchiveRoot: request.ArchiveRoot,
			OutputDir: request.OutputDir, DestinationRoot: d.VaultRoot,
		})
		if err != nil {
			return nil, inventoryError(err)
		}
		return &fotobankInventoryOutput{Body: FotobankInventory{
			Report: fromPhotoMigrationReport(result.Report), ReportPath: result.ReportPath, OwnerMapPath: result.OwnerMapPath,
		}}, nil
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
