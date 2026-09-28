package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// CreateFotobankInventory asks the daemon to read one stopped Fotobank source
// and persist the resulting report and owner map.
func (c *Connection) CreateFotobankInventory(ctx context.Context, request api.FotobankInventoryRequest) (api.MigrationRun, error) {
	var responseHTTP *http.Response
	response, err := c.apiWithResponse(&responseHTTP).CreateFotobankInventory(ctx, &apiclient.CreateFotobankInventoryRequestOptions{
		Body: &request,
	})
	if err != nil {
		return api.MigrationRun{}, err
	}
	if response == nil {
		return api.MigrationRun{}, &responseDecodeError{err: errors.New("daemon returned an empty migration run")}
	}
	if err := validateMigrationRunIdentity(*response); err != nil {
		return api.MigrationRun{}, &responseDecodeError{err: err}
	}
	if response.OwnerMapPath != request.OwnerMapPath {
		return api.MigrationRun{}, &responseDecodeError{err: fmt.Errorf("migration response owner-map path %q does not match request %q", response.OwnerMapPath, request.OwnerMapPath)}
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
	for index, run := range response.Items {
		if err := validateMigrationRunIdentity(run); err != nil {
			return api.MigrationRunPage{}, fmt.Errorf("migration run page item %d: %w", index, err)
		}
	}
	return *response, nil
}

// MigrationRun reads one completed inventory run by its daemon identity.
func (c *Connection) MigrationRun(ctx context.Context, id string) (api.MigrationRun, error) {
	if !validUUIDv4(id) {
		return api.MigrationRun{}, errors.New("migration run ID must be a canonical UUIDv4")
	}
	response, err := c.API().GetMigrationRun(ctx, &apiclient.GetMigrationRunRequestOptions{
		PathParams: &apiclient.GetMigrationRunPath{RunID: id},
	})
	if err != nil {
		return api.MigrationRun{}, err
	}
	if response == nil {
		return api.MigrationRun{}, errors.New("daemon returned an empty migration run")
	}
	if err := validateMigrationRunIdentity(*response); err != nil {
		return api.MigrationRun{}, err
	}
	if response.ID != id {
		return api.MigrationRun{}, errors.New("migration run response ID does not match requested ID")
	}
	return *response, nil
}

func validateMigrationRunIdentity(run api.MigrationRun) error {
	if !validUUIDv4(run.ID) {
		return errors.New("migration run ID must be a canonical UUIDv4")
	}
	return nil
}
