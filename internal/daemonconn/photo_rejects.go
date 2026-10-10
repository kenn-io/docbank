package daemonconn

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func (c *Connection) PhotoRejects(ctx context.Context, request api.PhotoRejectsRequest) (api.PhotoRejectsPreflight, error) {
	result, err := c.API().PreflightPhotoRejects(ctx, &apiclient.PreflightPhotoRejectsRequestOptions{Body: &request})
	if err != nil {
		return api.PhotoRejectsPreflight{}, err
	}
	if err := validatePhotoRejectsResponse(result); err != nil {
		return api.PhotoRejectsPreflight{}, err
	}
	return *result, nil
}

func (c *Connection) MovePhotoRejects(ctx context.Context, request api.MovePhotoRejectsRequest) (api.PhotoRejectsMoved, error) {
	var response *http.Response
	result, err := c.apiWithResponse(&response).MovePhotoRejects(ctx, &apiclient.MovePhotoRejectsRequestOptions{Body: &request})
	if err != nil {
		return api.PhotoRejectsMoved{}, mutationRequestError(response, err)
	}
	if result == nil || len(result.Moved) != len(request.Targets) {
		return api.PhotoRejectsMoved{}, &responseDecodeError{err: errors.New("invalid rejects move response")}
	}
	for i, id := range result.Moved {
		if id != request.Targets[i].AssetID {
			return api.PhotoRejectsMoved{}, &responseDecodeError{err: errors.New("invalid moved photo identity")}
		}
	}
	return *result, nil
}

func validatePhotoRejectsResponse(result *api.PhotoRejectsPreflight) error {
	if result == nil {
		return errors.New("missing rejects response")
	}
	if result.Photos < 0 || result.Files < result.Photos || result.Unchanged < 0 || result.MixedCount < len(result.Mixed) || result.MixedCount > result.Unchanged || len(result.Mixed) > store.MaxPhotoRejectsMixed || len(result.Targets) > result.Photos {
		return errors.New("invalid rejects response")
	}
	for _, mixed := range result.Mixed {
		if !validUUIDv4(mixed.AssetID) || len(mixed.Members) < 2 || len(mixed.Members) > maxPhotoResponseFiles {
			return errors.New("invalid mixed photo response")
		}
		for _, member := range mixed.Members {
			if !validUUIDv4(member.FileID) || !query.ValidPhotoFlag(member.Flag) {
				return errors.New("invalid mixed flag response")
			}
		}
	}
	return nil
}
