package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/photomigration"
)

// CreateFotobankInventory asks the daemon to read one stopped Fotobank source
// and write the report and owner-map template into the requested directory.
func (c *Connection) CreateFotobankInventory(ctx context.Context, request api.FotobankInventoryRequest) (api.FotobankInventory, error) {
	var responseHTTP *http.Response
	response, err := c.apiWithResponse(&responseHTTP).CreateFotobankInventory(ctx, &apiclient.CreateFotobankInventoryRequestOptions{
		Body: &request,
	})
	if err != nil {
		return api.FotobankInventory{}, err
	}
	if response == nil {
		return api.FotobankInventory{}, &responseDecodeError{err: errors.New("daemon returned an empty migration inventory")}
	}
	wantReport := filepath.Join(request.OutputDir, photomigration.ReportFileName)
	wantOwnerMap := filepath.Join(request.OutputDir, photomigration.OwnerMapFileName)
	if response.ReportPath != wantReport || response.OwnerMapPath != wantOwnerMap {
		return api.FotobankInventory{}, &responseDecodeError{err: fmt.Errorf("migration response paths %q and %q do not match output directory %q", response.ReportPath, response.OwnerMapPath, request.OutputDir)}
	}
	return *response, nil
}
