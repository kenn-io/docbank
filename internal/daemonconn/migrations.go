package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// CreateFotobankInventory asks the daemon to read one stopped Fotobank source
// and persist the resulting report and owner map.
func (c *Connection) CreateFotobankInventory(ctx context.Context, request api.FotobankInventoryRequest) (api.MigrationRun, error) {
	response, err := c.API().CreateFotobankInventory(ctx, &apiclient.CreateFotobankInventoryRequestOptions{
		Body: &request,
	})
	if err != nil {
		return api.MigrationRun{}, err
	}
	if response == nil || response.ID == "" {
		return api.MigrationRun{}, errors.New("daemon returned an empty migration run")
	}
	return *response, nil
}

// ListMigrationRuns returns a bounded page of completed inventory runs.
func (c *Connection) ListMigrationRuns(ctx context.Context, offset, limit int) (api.MigrationRunPage, error) {
	if offset < 0 || limit < 1 || limit > 50 {
		return api.MigrationRunPage{}, errors.New("invalid migration run page")
	}
	offsetValue, limitValue := int64(offset), int64(limit)
	response, err := c.API().ListMigrationRuns(ctx, &apiclient.ListMigrationRunsRequestOptions{
		Query: &apiclient.ListMigrationRunsQuery{Offset: &offsetValue, Limit: &limitValue},
	})
	if err != nil {
		return api.MigrationRunPage{}, err
	}
	if response == nil {
		return api.MigrationRunPage{}, errors.New("daemon returned an empty migration run page")
	}
	return *response, nil
}

// MigrationRun reads one completed inventory run by its daemon identity.
func (c *Connection) MigrationRun(ctx context.Context, id string) (api.MigrationRun, error) {
	if id == "" {
		return api.MigrationRun{}, errors.New("migration run ID is required")
	}
	response, err := c.API().GetMigrationRun(ctx, &apiclient.GetMigrationRunRequestOptions{
		PathParams: &apiclient.GetMigrationRunPath{RunID: id},
	})
	if err != nil {
		return api.MigrationRun{}, err
	}
	if response == nil || response.ID == "" {
		return api.MigrationRun{}, errors.New("daemon returned an empty migration run")
	}
	return *response, nil
}
