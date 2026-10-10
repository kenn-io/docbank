package daemonconn

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func (c *Connection) PhotoRejects(ctx context.Context, request api.PhotoRejectsRequest, cookie string) (api.PhotoRejectsPreflight, error) {
	var result *api.PhotoRejectsPreflight
	var response *http.Response
	var err error
	editor := func(_ context.Context, request *http.Request) error {
		if cookie != "" {
			request.Header.Set("Cookie", cookie)
		}
		return nil
	}
	if request.Digest == "" {
		result, err = c.apiWithResponse(&response).PreflightPhotoRejects(ctx, &apiclient.PreflightPhotoRejectsRequestOptions{Body: &request}, editor)
	} else {
		result, err = c.apiWithResponse(&response).MovePhotoRejects(ctx, &apiclient.MovePhotoRejectsRequestOptions{Body: &request}, editor)
	}
	if err != nil {
		return api.PhotoRejectsPreflight{}, mutationRequestError(response, err)
	}
	if err := validatePhotoRejectsResponse(result, request.Digest); err != nil {
		if request.Digest != "" {
			return api.PhotoRejectsPreflight{}, &responseDecodeError{err: err}
		}
		return api.PhotoRejectsPreflight{}, err
	}
	return *result, nil
}

func validatePhotoRejectsResponse(result *api.PhotoRejectsPreflight, digest string) error {
	if result == nil {
		return errors.New("missing rejects response")
	}
	decoded, err := hex.DecodeString(result.Digest)
	if err != nil || len(decoded) != 32 || digest != "" && result.Digest != digest || result.Photos < 0 || result.Files < result.Photos || result.Files > 1000 || result.Unchanged < 0 || result.Photos+result.Unchanged > 1000 || result.CheckoutSkipped != 0 || len(result.Mixed) > result.Unchanged {
		return errors.New("invalid rejects response")
	}
	for _, mixed := range result.Mixed {
		if !validUUIDv4(mixed.AssetID) || len(mixed.Members) < 2 || len(mixed.Members) > maxPhotoResponseFiles {
			return errors.New("invalid mixed photo response")
		}
		for _, member := range mixed.Members {
			if !validUUIDv4(member.FileID) || member.Flag != "" && member.Flag != "pick" && member.Flag != "reject" {
				return errors.New("invalid mixed flag response")
			}
		}
	}
	return nil
}
