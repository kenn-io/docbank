package daemonconn

import (
	"context"
	"errors"
	"path/filepath"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// StartPhotoImport queues a folder import. Status and cancellation go through
// the shared Jobs routes under the returned operation ID.
func (c *Connection) StartPhotoImport(ctx context.Context, sourceRoot, destination string) (api.StorageOperation, error) {
	if !filepath.IsAbs(sourceRoot) || destination == "" || destination[0] != '/' {
		return api.StorageOperation{}, errors.New("photo import source root must be absolute and destination must be a vault path")
	}
	operation, err := c.API().StartPhotoImport(ctx, &apiclient.StartPhotoImportRequestOptions{
		Body: &apiclient.StartPhotoImportBody{SourceRoot: sourceRoot, Destination: destination},
	})
	if err != nil {
		return api.StorageOperation{}, err
	}
	if operation == nil || !validUUIDv4(operation.ID) || operation.Kind != "photo_import" ||
		operation.State != "queued" || operation.FinishedAt != "" {
		return api.StorageOperation{}, &responseDecodeError{err: errors.New("photo import start response has invalid operation identity or state")}
	}
	return *operation, nil
}
