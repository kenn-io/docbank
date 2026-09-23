package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func photoIfMatch(revision int64) string {
	return strconv.Quote(strconv.FormatInt(revision, 10))
}

func validatePhotoAssetResponse(asset api.PhotoAsset, etag, requestedID string) error {
	if !validUUIDv4(asset.ID) || asset.Revision < 1 || asset.Kind != "photo" && asset.Kind != "video" {
		return errors.New("photo response has invalid identity, kind, or revision")
	}
	if requestedID != "" && asset.ID != requestedID {
		return fmt.Errorf("photo response ID %s does not match request %s", asset.ID, requestedID)
	}
	if asset.DisplaySource != "asset" && asset.DisplaySource != "vault" && asset.DisplaySource != "default" && asset.DisplaySource != "none" {
		return errors.New("photo response has invalid display source")
	}
	if len(asset.Files) > 256 {
		return errors.New("photo response exceeds file bound")
	}
	if _, err := time.Parse(time.RFC3339Nano, asset.CreatedAt); err != nil {
		return errors.New("photo response has invalid created_at")
	}
	if _, err := time.Parse(time.RFC3339Nano, asset.UpdatedAt); err != nil {
		return errors.New("photo response has invalid updated_at")
	}
	files := make(map[string]api.PhotoFile, len(asset.Files))
	nodes := make(map[int64]struct{}, len(asset.Files))
	for _, file := range asset.Files {
		if !validUUIDv4(file.ID) || file.AssetID != asset.ID || file.NodeID < 1 || (file.Role != "raw" && file.Role != "image" && file.Role != "video" && file.Role != "sidecar") {
			return errors.New("photo response has invalid file identity")
		}
		if _, ok := files[file.ID]; ok {
			return errors.New("photo response repeats a file")
		}
		if _, ok := nodes[file.NodeID]; ok {
			return errors.New("photo response repeats a node")
		}
		files[file.ID] = file
		nodes[file.NodeID] = struct{}{}
		if _, err := time.Parse(time.RFC3339Nano, file.CreatedAt); err != nil {
			return errors.New("photo response has invalid file created_at")
		}
	}
	for _, file := range files {
		if file.SidecarOfID != nil {
			target, ok := files[*file.SidecarOfID]
			if !ok || file.Role != "sidecar" || target.Role != "raw" || target.AssetID != asset.ID {
				return errors.New("photo response has invalid sidecar pointer")
			}
		}
	}
	for _, id := range []*string{asset.DisplayFileID, asset.DisplayOverrideFileID} {
		if id == nil {
			continue
		}
		file, ok := files[*id]
		if !ok || file.Role == "sidecar" {
			return errors.New("photo response has invalid display pointer")
		}
	}
	if etag == "" {
		return errors.New("photo response is missing ETag")
	}
	if etag != photoIfMatch(asset.Revision) {
		return fmt.Errorf("photo response ETag %q, expected %q", etag, photoIfMatch(asset.Revision))
	}
	if asset.DisplaySource == "none" && asset.DisplayFileID != nil {
		return errors.New("photo response has a display file with source none")
	}
	return nil
}

func photoMutationResponseError(err error) error {
	if err == nil {
		return nil
	}
	return &responseDecodeError{err: err}
}

func photoMutationRequestError(response *http.Response, err error) error {
	if err != nil && response != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		return photoMutationResponseError(err)
	}
	return err
}

func photoResponseMember(asset api.PhotoAsset, nodeID int64) (api.PhotoFile, bool) {
	for _, file := range asset.Files {
		if file.NodeID == nodeID {
			return file, true
		}
	}
	return api.PhotoFile{}, false
}

func validatePhotoMemberResponse(asset api.PhotoAsset, nodeID int64, role string, sidecarOfID *string, checkSidecar bool) error {
	file, ok := photoResponseMember(asset, nodeID)
	if !ok {
		return errors.New("photo response does not contain requested node")
	}
	if role != "" && file.Role != role {
		return errors.New("photo response member has unexpected role")
	}
	if checkSidecar && !equalPhotoPointer(file.SidecarOfID, sidecarOfID) {
		return errors.New("photo response member has unexpected sidecar target")
	}
	return nil
}

func (c *Connection) PhotoAsset(ctx context.Context, id string) (api.PhotoAsset, error) {
	var asset api.PhotoAsset
	if !validUUIDv4(id) {
		return asset, errors.New("photo asset ID must be a canonical UUIDv4")
	}
	var response *http.Response
	apiResponse, err := c.apiWithResponse(&response).GetPhotoAsset(ctx, &apiclient.GetPhotoAssetRequestOptions{PathParams: &apiclient.GetPhotoAssetPath{AssetID: id}})
	if err != nil {
		return asset, err
	}
	asset = *apiResponse
	if err := validatePhotoAssetResponse(asset, response.Header.Get("ETag"), id); err != nil {
		return api.PhotoAsset{}, err
	}
	return asset, nil
}

func (c *Connection) PhotoAssetForNode(ctx context.Context, nodeID int64) (api.PhotoAsset, error) {
	if nodeID < 1 {
		return api.PhotoAsset{}, errors.New("photo node ID must be positive")
	}
	var response *http.Response
	apiResponse, err := c.apiWithResponse(&response).GetPhotoAssetByNode(ctx, &apiclient.GetPhotoAssetByNodeRequestOptions{PathParams: &apiclient.GetPhotoAssetByNodePath{NodeID: nodeID}})
	if err != nil {
		return api.PhotoAsset{}, err
	}
	asset := *apiResponse
	if err := validatePhotoAssetResponse(asset, response.Header.Get("ETag"), ""); err != nil {
		return api.PhotoAsset{}, err
	}
	found := false
	for _, file := range asset.Files {
		if file.NodeID == nodeID {
			found = true
			break
		}
	}
	if !found {
		return api.PhotoAsset{}, errors.New("photo response does not contain requested node")
	}
	return asset, nil
}

func (c *Connection) CreatePhotoAsset(ctx context.Context, nodeID int64, role, kind string) (api.PhotoAsset, error) {
	if nodeID < 1 {
		return api.PhotoAsset{}, errors.New("photo node ID must be positive")
	}
	request := &apiclient.CreatePhotoAssetBody{NodeID: nodeID, Role: role, Kind: kind}
	var response *http.Response
	apiResponse, err := c.apiWithResponse(&response).CreatePhotoAsset(ctx, &apiclient.CreatePhotoAssetRequestOptions{Body: request})
	if err != nil {
		return api.PhotoAsset{}, photoMutationRequestError(response, err)
	}
	asset := *apiResponse
	if err := validatePhotoAssetResponse(asset, response.Header.Get("ETag"), ""); err != nil {
		return api.PhotoAsset{}, photoMutationResponseError(err)
	}
	if asset.Revision != 1 {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("created photo asset must start at revision one"))
	}
	if kind != "" && asset.Kind != kind {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("created photo asset has unexpected kind"))
	}
	if err := validatePhotoMemberResponse(asset, nodeID, role, nil, true); err != nil {
		return api.PhotoAsset{}, photoMutationResponseError(err)
	}
	return asset, nil
}

func (c *Connection) AttachPhotoFile(ctx context.Context, assetID string, revision int64, nodeID int64, role string, sidecarOfID *string) (api.PhotoAsset, error) {
	if !validUUIDv4(assetID) || revision < 1 || nodeID < 1 {
		return api.PhotoAsset{}, errors.New("invalid photo attach identity or revision")
	}
	var response *http.Response
	apiResponse, err := c.apiWithResponse(&response).AttachPhotoFile(ctx, &apiclient.AttachPhotoFileRequestOptions{
		PathParams: &apiclient.AttachPhotoFilePath{AssetID: assetID},
		Header:     &apiclient.AttachPhotoFileHeaders{IfMatch: photoIfMatch(revision)},
		Body:       &apiclient.AttachPhotoFileBody{NodeID: nodeID, Role: role, SidecarOfID: sidecarOfID},
	})
	if err != nil {
		return api.PhotoAsset{}, photoMutationRequestError(response, err)
	}
	asset := *apiResponse
	if err := validatePhotoAssetResponse(asset, response.Header.Get("ETag"), assetID); err != nil {
		return api.PhotoAsset{}, photoMutationResponseError(err)
	}
	if asset.Revision != revision+1 {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("attached photo response did not advance one revision"))
	}
	if err := validatePhotoMemberResponse(asset, nodeID, role, sidecarOfID, true); err != nil {
		return api.PhotoAsset{}, photoMutationResponseError(err)
	}
	return asset, nil
}

func (c *Connection) DetachPhotoFile(ctx context.Context, assetID string, revision int64, fileID string, clearDependentSidecars bool) (api.PhotoAsset, error) {
	if !validUUIDv4(assetID) || !validUUIDv4(fileID) || revision < 1 {
		return api.PhotoAsset{}, errors.New("invalid photo detach identity or revision")
	}
	var response *http.Response
	apiResponse, err := c.apiWithResponse(&response).DetachPhotoFile(ctx, &apiclient.DetachPhotoFileRequestOptions{PathParams: &apiclient.DetachPhotoFilePath{AssetID: assetID, FileID: fileID}, Header: &apiclient.DetachPhotoFileHeaders{IfMatch: photoIfMatch(revision)}, Query: &apiclient.DetachPhotoFileQuery{ClearDependentSidecars: &clearDependentSidecars}})
	if err != nil {
		return api.PhotoAsset{}, photoMutationRequestError(response, err)
	}
	asset := *apiResponse
	if err := validatePhotoAssetResponse(asset, response.Header.Get("ETag"), assetID); err != nil {
		return api.PhotoAsset{}, photoMutationResponseError(err)
	}
	if asset.Revision != revision+1 {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("detached photo response did not advance one revision"))
	}
	for _, file := range asset.Files {
		if file.ID == fileID || file.SidecarOfID != nil && *file.SidecarOfID == fileID {
			return api.PhotoAsset{}, photoMutationResponseError(errors.New("detached photo response retains removed membership"))
		}
	}
	return asset, nil
}

func (c *Connection) ExcludePhotoAsset(ctx context.Context, assetID string, revision int64, excluded bool) (api.PhotoAsset, error) {
	if !validUUIDv4(assetID) || revision < 1 {
		return api.PhotoAsset{}, errors.New("invalid photo exclusion identity or revision")
	}
	var response *http.Response
	apiResponse, err := c.apiWithResponse(&response).ExcludePhotoAsset(ctx, &apiclient.ExcludePhotoAssetRequestOptions{PathParams: &apiclient.ExcludePhotoAssetPath{AssetID: assetID}, Header: &apiclient.ExcludePhotoAssetHeaders{IfMatch: photoIfMatch(revision)}, Body: &apiclient.ExcludePhotoAssetBody{Excluded: excluded}})
	if err != nil {
		return api.PhotoAsset{}, photoMutationRequestError(response, err)
	}
	asset := *apiResponse
	if err := validatePhotoAssetResponse(asset, response.Header.Get("ETag"), assetID); err != nil {
		return api.PhotoAsset{}, photoMutationResponseError(err)
	}
	if asset.Revision != revision && asset.Revision != revision+1 {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("excluded photo response has an invalid revision"))
	}
	if (asset.ExcludedAt != nil) != excluded {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("excluded photo response has invalid state"))
	}
	return asset, nil
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
	var response *http.Response
	options := &apiclient.PromotePhotoNodeRequestOptions{PathParams: &apiclient.PromotePhotoNodePath{NodeID: nodeID}, Body: &request}
	if expectedRevision != nil {
		ifMatch := photoIfMatch(*expectedRevision)
		options.Header = &apiclient.PromotePhotoNodeHeaders{IfMatch: &ifMatch}
	}
	apiResponse, err := c.apiWithResponse(&response).PromotePhotoNode(ctx, options)
	if err != nil {
		return api.PhotoAsset{}, photoMutationRequestError(response, err)
	}
	asset := *apiResponse
	if err := validatePhotoAssetResponse(asset, response.Header.Get("ETag"), ""); err != nil {
		return api.PhotoAsset{}, photoMutationResponseError(err)
	}
	if expectedRevision != nil && asset.Revision != *expectedRevision && asset.Revision != *expectedRevision+1 {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("promoted photo response has an invalid revision"))
	}
	if expectedRevision != nil && asset.ExcludedAt != nil {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("promoted photo response remains excluded"))
	}
	if expectedRevision == nil && asset.Revision != 1 {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("newly promoted photo response must start at revision one"))
	}
	if kind != "" && asset.Kind != kind {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("promoted photo response has unexpected kind"))
	}
	if err := validatePhotoMemberResponse(asset, nodeID, role, nil, false); err != nil {
		return api.PhotoAsset{}, photoMutationResponseError(err)
	}
	return asset, nil
}

func (c *Connection) SetPhotoDisplay(ctx context.Context, assetID string, revision int64, fileID *string) (api.PhotoAsset, error) {
	if !validUUIDv4(assetID) || revision < 1 || fileID != nil && !validUUIDv4(*fileID) {
		return api.PhotoAsset{}, errors.New("invalid photo display identity or revision")
	}
	var response *http.Response
	apiResponse, err := c.apiWithResponse(&response).SetPhotoDisplay(ctx, &apiclient.SetPhotoDisplayRequestOptions{PathParams: &apiclient.SetPhotoDisplayPath{AssetID: assetID}, Header: &apiclient.SetPhotoDisplayHeaders{IfMatch: photoIfMatch(revision)}, Body: &apiclient.SetPhotoDisplayBody{FileID: fileID}})
	if err != nil {
		return api.PhotoAsset{}, photoMutationRequestError(response, err)
	}
	asset := *apiResponse
	if err := validatePhotoAssetResponse(asset, response.Header.Get("ETag"), assetID); err != nil {
		return api.PhotoAsset{}, photoMutationResponseError(err)
	}
	if asset.Revision != revision && asset.Revision != revision+1 {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("display photo response has an invalid revision"))
	}
	if !equalPhotoPointer(asset.DisplayOverrideFileID, fileID) {
		return api.PhotoAsset{}, photoMutationResponseError(errors.New("display photo response has invalid override state"))
	}
	return asset, nil
}

func (c *Connection) PhotoSettings(ctx context.Context) (api.PhotoSettings, error) {
	var response *http.Response
	apiResponse, err := c.apiWithResponse(&response).GetPhotoSettings(ctx)
	if err != nil {
		return api.PhotoSettings{}, err
	}
	settings := *apiResponse
	if settings.Revision < 1 || settings.Preference != nil && *settings.Preference != "raw" && *settings.Preference != "image" {
		return api.PhotoSettings{}, errors.New("photo settings response is invalid")
	}
	if etag := response.Header.Get("ETag"); etag == "" {
		return api.PhotoSettings{}, errors.New("photo settings response is missing ETag")
	} else if etag != photoIfMatch(settings.Revision) {
		return api.PhotoSettings{}, fmt.Errorf("photo settings response ETag %q, expected %q", etag, photoIfMatch(settings.Revision))
	}
	return settings, nil
}

func (c *Connection) SetPhotoSettings(ctx context.Context, revision int64, preference *string) (api.PhotoSettings, error) {
	if revision < 1 || preference != nil && *preference != "raw" && *preference != "image" {
		return api.PhotoSettings{}, errors.New("invalid photo settings revision or preference")
	}
	var response *http.Response
	apiResponse, err := c.apiWithResponse(&response).SetPhotoSettings(ctx, &apiclient.SetPhotoSettingsRequestOptions{Header: &apiclient.SetPhotoSettingsHeaders{IfMatch: photoIfMatch(revision)}, Body: &apiclient.SetPhotoSettingsBody{Preference: preference}})
	if err != nil {
		return api.PhotoSettings{}, photoMutationRequestError(response, err)
	}
	settings := *apiResponse
	if settings.Revision != revision && settings.Revision != revision+1 || settings.Preference != nil && *settings.Preference != "raw" && *settings.Preference != "image" {
		return api.PhotoSettings{}, photoMutationResponseError(errors.New("photo settings response has invalid revision"))
	}
	if !equalPhotoPointer(settings.Preference, preference) {
		return api.PhotoSettings{}, photoMutationResponseError(errors.New("photo settings response has invalid preference"))
	}
	if response.Header.Get("ETag") != photoIfMatch(settings.Revision) {
		return api.PhotoSettings{}, photoMutationResponseError(errors.New("photo settings response ETag is inconsistent"))
	}
	return settings, nil
}

func equalPhotoPointer(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
