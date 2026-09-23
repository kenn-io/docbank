package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

const maxPhotoResponseFiles = 256

func photoIfMatch(revision int64) string {
	return strconv.Quote(strconv.FormatInt(revision, 10))
}

// validatePhotoAssetResponse checks the identity, bounds, and ETag of a
// daemon response. Graph policy stays in the store; the client only refuses a
// response it cannot address or that disagrees with its own revision.
func validatePhotoAssetResponse(asset api.PhotoAsset, etag, requestedID string) error {
	if !validUUIDv4(asset.ID) || asset.Revision < 1 || asset.Kind != "photo" && asset.Kind != "video" {
		return errors.New("photo response has invalid identity, kind, or revision")
	}
	if requestedID != "" && asset.ID != requestedID {
		return fmt.Errorf("photo response ID %s does not match request %s", asset.ID, requestedID)
	}
	if len(asset.Files) > maxPhotoResponseFiles {
		return errors.New("photo response exceeds file bound")
	}
	for _, file := range asset.Files {
		if !validUUIDv4(file.ID) || file.AssetID != asset.ID || file.NodeID < 1 {
			return errors.New("photo response has invalid file identity")
		}
	}
	return validatePhotoETag(etag, asset.Revision)
}

func validatePhotoETag(etag string, revision int64) error {
	if etag == "" {
		return errors.New("photo response is missing ETag")
	}
	if etag != photoIfMatch(revision) {
		return fmt.Errorf("photo response ETag %q, expected %q", etag, photoIfMatch(revision))
	}
	return nil
}

func validatePhotoSettingsResponse(settings api.PhotoSettings, etag string) error {
	if settings.Revision < 1 || settings.Preference != nil && *settings.Preference != "raw" && *settings.Preference != "image" {
		return errors.New("photo settings response is invalid")
	}
	return validatePhotoETag(etag, settings.Revision)
}

// photoMutationRequestError classifies a failure after a 2xx status as a
// response-decode error so MCP treats the outcome as unknown instead of
// retrying a write the daemon may have committed.
func photoMutationRequestError(response *http.Response, err error) error {
	if err != nil && response != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		return &responseDecodeError{err: err}
	}
	return err
}

func photoMutationResponse(response *http.Response, asset *api.PhotoAsset, err error, requestedID string) (api.PhotoAsset, error) {
	if err != nil {
		return api.PhotoAsset{}, photoMutationRequestError(response, err)
	}
	if err := validatePhotoAssetResponse(*asset, response.Header.Get("ETag"), requestedID); err != nil {
		return api.PhotoAsset{}, &responseDecodeError{err: err}
	}
	return *asset, nil
}

func (c *Connection) PhotoAsset(ctx context.Context, id string) (api.PhotoAsset, error) {
	if !validUUIDv4(id) {
		return api.PhotoAsset{}, errors.New("photo asset ID must be a canonical UUIDv4")
	}
	var response *http.Response
	asset, err := c.apiWithResponse(&response).GetPhotoAsset(ctx, &apiclient.GetPhotoAssetRequestOptions{PathParams: &apiclient.GetPhotoAssetPath{AssetID: id}})
	if err != nil {
		return api.PhotoAsset{}, err
	}
	if err := validatePhotoAssetResponse(*asset, response.Header.Get("ETag"), id); err != nil {
		return api.PhotoAsset{}, err
	}
	return *asset, nil
}

func (c *Connection) PhotoAssetForNode(ctx context.Context, nodeID int64) (api.PhotoAsset, error) {
	if nodeID < 1 {
		return api.PhotoAsset{}, errors.New("photo node ID must be positive")
	}
	var response *http.Response
	asset, err := c.apiWithResponse(&response).GetPhotoAssetByNode(ctx, &apiclient.GetPhotoAssetByNodeRequestOptions{PathParams: &apiclient.GetPhotoAssetByNodePath{NodeID: nodeID}})
	if err != nil {
		return api.PhotoAsset{}, err
	}
	if err := validatePhotoAssetResponse(*asset, response.Header.Get("ETag"), ""); err != nil {
		return api.PhotoAsset{}, err
	}
	return *asset, nil
}

func (c *Connection) CreatePhotoAsset(ctx context.Context, nodeID int64, role, kind string) (api.PhotoAsset, error) {
	if nodeID < 1 {
		return api.PhotoAsset{}, errors.New("photo node ID must be positive")
	}
	var response *http.Response
	asset, err := c.apiWithResponse(&response).CreatePhotoAsset(ctx, &apiclient.CreatePhotoAssetRequestOptions{Body: &apiclient.CreatePhotoAssetBody{NodeID: nodeID, Role: role, Kind: kind}})
	return photoMutationResponse(response, asset, err, "")
}

func (c *Connection) AttachPhotoFile(ctx context.Context, assetID string, revision int64, nodeID int64, role string, sidecarOfID *string) (api.PhotoAsset, error) {
	if !validUUIDv4(assetID) || revision < 1 || nodeID < 1 {
		return api.PhotoAsset{}, errors.New("invalid photo attach identity or revision")
	}
	var response *http.Response
	asset, err := c.apiWithResponse(&response).AttachPhotoFile(ctx, &apiclient.AttachPhotoFileRequestOptions{
		PathParams: &apiclient.AttachPhotoFilePath{AssetID: assetID},
		Header:     &apiclient.AttachPhotoFileHeaders{IfMatch: photoIfMatch(revision)},
		Body:       &apiclient.AttachPhotoFileBody{NodeID: nodeID, Role: role, SidecarOfID: sidecarOfID},
	})
	return photoMutationResponse(response, asset, err, assetID)
}

func (c *Connection) DetachPhotoFile(ctx context.Context, assetID string, revision int64, fileID string, clearDependentSidecars bool) (api.PhotoAsset, error) {
	if !validUUIDv4(assetID) || !validUUIDv4(fileID) || revision < 1 {
		return api.PhotoAsset{}, errors.New("invalid photo detach identity or revision")
	}
	var response *http.Response
	asset, err := c.apiWithResponse(&response).DetachPhotoFile(ctx, &apiclient.DetachPhotoFileRequestOptions{
		PathParams: &apiclient.DetachPhotoFilePath{AssetID: assetID, FileID: fileID},
		Header:     &apiclient.DetachPhotoFileHeaders{IfMatch: photoIfMatch(revision)},
		Query:      &apiclient.DetachPhotoFileQuery{ClearDependentSidecars: &clearDependentSidecars},
	})
	return photoMutationResponse(response, asset, err, assetID)
}

func (c *Connection) ExcludePhotoAsset(ctx context.Context, assetID string, revision int64, excluded bool) (api.PhotoAsset, error) {
	if !validUUIDv4(assetID) || revision < 1 {
		return api.PhotoAsset{}, errors.New("invalid photo exclusion identity or revision")
	}
	var response *http.Response
	asset, err := c.apiWithResponse(&response).ExcludePhotoAsset(ctx, &apiclient.ExcludePhotoAssetRequestOptions{
		PathParams: &apiclient.ExcludePhotoAssetPath{AssetID: assetID},
		Header:     &apiclient.ExcludePhotoAssetHeaders{IfMatch: photoIfMatch(revision)},
		Body:       &apiclient.ExcludePhotoAssetBody{Excluded: excluded},
	})
	return photoMutationResponse(response, asset, err, assetID)
}

func (c *Connection) PromotePhotoNode(ctx context.Context, nodeID int64, expectedRevision *int64, role, kind string) (api.PhotoAsset, error) {
	if nodeID < 1 {
		return api.PhotoAsset{}, errors.New("photo node ID must be positive")
	}
	var request apiclient.PromotePhotoNodeBody
	if role != "" {
		value := apiclient.PromotePhotoNodeRequestRole(role)
		request.Role = &value
	}
	if kind != "" {
		value := apiclient.PromotePhotoNodeRequestKind(kind)
		request.Kind = &value
	}
	options := &apiclient.PromotePhotoNodeRequestOptions{PathParams: &apiclient.PromotePhotoNodePath{NodeID: nodeID}, Body: &request}
	if expectedRevision != nil {
		ifMatch := photoIfMatch(*expectedRevision)
		options.Header = &apiclient.PromotePhotoNodeHeaders{IfMatch: &ifMatch}
	}
	var response *http.Response
	asset, err := c.apiWithResponse(&response).PromotePhotoNode(ctx, options)
	return photoMutationResponse(response, asset, err, "")
}

func (c *Connection) SetPhotoDisplay(ctx context.Context, assetID string, revision int64, fileID *string) (api.PhotoAsset, error) {
	if !validUUIDv4(assetID) || revision < 1 || fileID != nil && !validUUIDv4(*fileID) {
		return api.PhotoAsset{}, errors.New("invalid photo display identity or revision")
	}
	var response *http.Response
	asset, err := c.apiWithResponse(&response).SetPhotoDisplay(ctx, &apiclient.SetPhotoDisplayRequestOptions{
		PathParams: &apiclient.SetPhotoDisplayPath{AssetID: assetID},
		Header:     &apiclient.SetPhotoDisplayHeaders{IfMatch: photoIfMatch(revision)},
		Body:       &apiclient.SetPhotoDisplayBody{FileID: fileID},
	})
	return photoMutationResponse(response, asset, err, assetID)
}

func (c *Connection) PhotoSettings(ctx context.Context) (api.PhotoSettings, error) {
	var response *http.Response
	settings, err := c.apiWithResponse(&response).GetPhotoSettings(ctx)
	if err != nil {
		return api.PhotoSettings{}, err
	}
	if err := validatePhotoSettingsResponse(*settings, response.Header.Get("ETag")); err != nil {
		return api.PhotoSettings{}, err
	}
	return *settings, nil
}

func (c *Connection) SetPhotoSettings(ctx context.Context, revision int64, preference *string) (api.PhotoSettings, error) {
	if revision < 1 || preference != nil && *preference != "raw" && *preference != "image" {
		return api.PhotoSettings{}, errors.New("invalid photo settings revision or preference")
	}
	var response *http.Response
	settings, err := c.apiWithResponse(&response).SetPhotoSettings(ctx, &apiclient.SetPhotoSettingsRequestOptions{
		Header: &apiclient.SetPhotoSettingsHeaders{IfMatch: photoIfMatch(revision)},
		Body:   &apiclient.SetPhotoSettingsBody{Preference: preference},
	})
	if err != nil {
		return api.PhotoSettings{}, photoMutationRequestError(response, err)
	}
	if err := validatePhotoSettingsResponse(*settings, response.Header.Get("ETag")); err != nil {
		return api.PhotoSettings{}, &responseDecodeError{err: err}
	}
	return *settings, nil
}
