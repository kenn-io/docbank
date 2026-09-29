package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

func validatePhotoImportRun(run api.PhotoImportRun, etag string) error {
	if !validUUIDv4(run.ID) || run.Revision < 1 || run.SourceRoot == "" || run.Destination == "" ||
		run.TotalGroups < 0 || run.CompletedGroups < 0 || run.CompletedGroups > run.TotalGroups ||
		run.AddedGroups < 0 || run.SkippedGroups < 0 || run.FailedGroups < 0 || run.AmbiguousGroups < 0 {
		return errors.New("photo import response has invalid identity or counters")
	}
	if run.FinishedAt == "" && run.State != store.PhotoImportStateRunning && run.State != store.PhotoImportStateCancelRequested {
		return errors.New("photo import response has invalid active state")
	}
	if run.FinishedAt != "" && (run.State == store.PhotoImportStateRunning || run.State == store.PhotoImportStateCancelRequested) {
		return errors.New("photo import response has invalid terminal timestamp")
	}
	if etag != "" {
		if err := validatePhotoETag(etag, run.Revision); err != nil {
			return err
		}
	}
	return nil
}

func validatePhotoImportID(id string) error {
	if !validUUIDv4(id) {
		return errors.New("photo import ID must be a canonical UUIDv4")
	}
	return nil
}

func (c *Connection) StartPhotoImport(ctx context.Context, sourceRoot, destination string, choice *api.PhotoImportChoice) (api.PhotoImportRun, error) {
	if !filepath.IsAbs(sourceRoot) || destination == "" || destination[0] != '/' {
		return api.PhotoImportRun{}, errors.New("photo import source root must be absolute and destination must be a vault path")
	}
	if choice != nil && (choice.GroupKey == "" || choice.RawAssetID == "" && choice.RawFileID == "" && choice.RawSourcePath == "" && choice.RawBlobHash == "") {
		return api.PhotoImportRun{}, errors.New("photo import choices need a group key and RAW identity")
	}
	var response *http.Response
	run, err := c.apiWithResponse(&response).StartPhotoImport(ctx, &apiclient.StartPhotoImportRequestOptions{
		Body: &apiclient.StartPhotoImportBody{SourceRoot: sourceRoot, Destination: destination, Choice: choice},
	})
	if err != nil {
		return api.PhotoImportRun{}, photoMutationRequestError(response, err)
	}
	if err := validatePhotoImportRun(*run, response.Header.Get("ETag")); err != nil {
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
		if err := validatePhotoImportRun(run, ""); err != nil {
			return nil, &responseDecodeError{err: fmt.Errorf("photo import list: %w", err)}
		}
	}
	return runs.Items, nil
}

func (c *Connection) PhotoImport(ctx context.Context, id string) (api.PhotoImportRun, error) {
	if err := validatePhotoImportID(id); err != nil {
		return api.PhotoImportRun{}, err
	}
	var response *http.Response
	run, err := c.apiWithResponse(&response).GetPhotoImport(ctx, &apiclient.GetPhotoImportRequestOptions{
		PathParams: &apiclient.GetPhotoImportPath{RunID: id},
	})
	if err != nil {
		return api.PhotoImportRun{}, err
	}
	if err := validatePhotoImportRun(*run, response.Header.Get("ETag")); err != nil {
		return api.PhotoImportRun{}, &responseDecodeError{err: err}
	}
	return *run, nil
}

func (c *Connection) CancelPhotoImport(ctx context.Context, id string, revision int64) (api.PhotoImportRun, error) {
	if err := validatePhotoImportID(id); err != nil {
		return api.PhotoImportRun{}, err
	}
	if revision < 1 {
		return api.PhotoImportRun{}, errors.New("photo import revision must be positive")
	}
	var response *http.Response
	run, err := c.apiWithResponse(&response).CancelPhotoImport(ctx, &apiclient.CancelPhotoImportRequestOptions{
		PathParams: &apiclient.CancelPhotoImportPath{RunID: id},
		Header:     &apiclient.CancelPhotoImportHeaders{IfMatch: photoIfMatch(revision)},
	})
	if err != nil {
		return api.PhotoImportRun{}, err
	}
	if err := validatePhotoImportRun(*run, response.Header.Get("ETag")); err != nil {
		return api.PhotoImportRun{}, &responseDecodeError{err: err}
	}
	return *run, nil
}
