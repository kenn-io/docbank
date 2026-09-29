package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func validatePhotoImportRun(run api.PhotoImportRun) error {
	if !validUUIDv4(run.ID) || run.Destination == "" ||
		run.TotalGroups < 0 || run.CompletedGroups < 0 || run.AddedGroups < 0 ||
		run.SkippedGroups < 0 || run.FailedGroups < 0 || run.AmbiguousGroups < 0 {
		return errors.New("photo import response has invalid identity or counters")
	}
	active := run.State == "queued" || run.State == "running"
	if active == (run.FinishedAt != "") {
		return errors.New("photo import response state does not match its finish time")
	}
	return nil
}

func validatePhotoImportID(id string) error {
	if !validUUIDv4(id) {
		return errors.New("photo import ID must be a canonical UUIDv4")
	}
	return nil
}

func (c *Connection) StartPhotoImport(ctx context.Context, sourceRoot, destination string) (api.PhotoImportRun, error) {
	if !filepath.IsAbs(sourceRoot) || destination == "" || destination[0] != '/' {
		return api.PhotoImportRun{}, errors.New("photo import source root must be absolute and destination must be a vault path")
	}
	run, err := c.API().StartPhotoImport(ctx, &apiclient.StartPhotoImportRequestOptions{
		Body: &apiclient.StartPhotoImportBody{SourceRoot: sourceRoot, Destination: destination},
	})
	if err != nil {
		return api.PhotoImportRun{}, err
	}
	if err := validatePhotoImportRun(*run); err != nil {
		return api.PhotoImportRun{}, &responseDecodeError{err: err}
	}
	return *run, nil
}

func (c *Connection) PhotoImports(ctx context.Context) ([]api.PhotoImportRun, error) {
	runs, err := c.API().ListPhotoImports(ctx)
	if err != nil {
		return nil, err
	}
	for _, run := range runs.Items {
		if err := validatePhotoImportRun(run); err != nil {
			return nil, &responseDecodeError{err: fmt.Errorf("photo import list: %w", err)}
		}
	}
	return runs.Items, nil
}

func (c *Connection) PhotoImport(ctx context.Context, id string) (api.PhotoImportRun, error) {
	if err := validatePhotoImportID(id); err != nil {
		return api.PhotoImportRun{}, err
	}
	run, err := c.API().GetPhotoImport(ctx, &apiclient.GetPhotoImportRequestOptions{
		PathParams: &apiclient.GetPhotoImportPath{RunID: id},
	})
	if err != nil {
		return api.PhotoImportRun{}, err
	}
	if err := validatePhotoImportRun(*run); err != nil {
		return api.PhotoImportRun{}, &responseDecodeError{err: err}
	}
	return *run, nil
}

func (c *Connection) CancelPhotoImport(ctx context.Context, id string) (api.PhotoImportRun, error) {
	if err := validatePhotoImportID(id); err != nil {
		return api.PhotoImportRun{}, err
	}
	run, err := c.API().CancelPhotoImport(ctx, &apiclient.CancelPhotoImportRequestOptions{
		PathParams: &apiclient.CancelPhotoImportPath{RunID: id},
	})
	if err != nil {
		return api.PhotoImportRun{}, err
	}
	if err := validatePhotoImportRun(*run); err != nil {
		return api.PhotoImportRun{}, &responseDecodeError{err: err}
	}
	return *run, nil
}
