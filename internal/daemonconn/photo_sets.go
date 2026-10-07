package daemonconn

import (
	"context"
	"errors"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"net/http"
)

func validatePhotoAlbumIdentity(album api.PhotoAlbum, id string) error {
	if !validUUIDv4(album.ID) || album.Revision < 1 || id != "" && album.ID != id {
		return errors.New("album response identity or revision mismatch")
	}
	if album.CoverAssetID != nil && !validUUIDv4(*album.CoverAssetID) {
		return errors.New("album response has invalid cover identity")
	}
	return nil
}

func validatePhotoAlbumResponse(album api.PhotoAlbum, etag, id string) error {
	if err := validatePhotoAlbumIdentity(album, id); err != nil {
		return err
	}
	if etag != revisionIfMatch(album.Revision) {
		return errors.New("album response ETag mismatch")
	}
	return nil
}

func photoAlbumMutationResponse(response *http.Response, album *api.PhotoAlbum, err error, id string) (api.PhotoAlbum, error) {
	if err != nil {
		return api.PhotoAlbum{}, mutationRequestError(response, err)
	}
	if album == nil || response == nil {
		return api.PhotoAlbum{}, &responseDecodeError{err: errors.New("missing album mutation response")}
	}
	if err := validatePhotoAlbumResponse(*album, response.Header.Get("ETag"), id); err != nil {
		return api.PhotoAlbum{}, &responseDecodeError{err: err}
	}
	return *album, nil
}

func (c *Connection) PhotoAlbums(ctx context.Context) ([]api.PhotoAlbumSummary, error) {
	albums, err := c.API().ListPhotoAlbums(ctx)
	if err != nil {
		return nil, err
	}
	for _, album := range *albums {
		if err := validatePhotoAlbumIdentity(album.PhotoAlbum, ""); err != nil {
			return nil, err
		}
	}
	return *albums, nil
}

func (c *Connection) PhotoAlbum(ctx context.Context, id string) (api.PhotoAlbumSummary, error) {
	if !validUUIDv4(id) {
		return api.PhotoAlbumSummary{}, errors.New("invalid album ID")
	}
	var response *http.Response
	album, err := c.apiWithResponse(&response).GetPhotoAlbum(ctx, &apiclient.GetPhotoAlbumRequestOptions{PathParams: &apiclient.GetPhotoAlbumPath{SetID: id}})
	if err != nil {
		return api.PhotoAlbumSummary{}, err
	}
	if err := validatePhotoAlbumResponse(album.PhotoAlbum, response.Header.Get("ETag"), id); err != nil {
		return api.PhotoAlbumSummary{}, err
	}
	return *album, nil
}

func (c *Connection) CreatePhotoAlbum(ctx context.Context, name string) (api.PhotoAlbum, error) {
	var response *http.Response
	album, err := c.apiWithResponse(&response).CreatePhotoAlbum(ctx, &apiclient.CreatePhotoAlbumRequestOptions{Body: &api.PhotoAlbumNameRequest{Name: name}})
	return photoAlbumMutationResponse(response, album, err, "")
}

func (c *Connection) UpdatePhotoAlbum(ctx context.Context, id string, revision int64, body api.UpdatePhotoAlbumRequest) (api.PhotoAlbum, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.PhotoAlbum{}, errors.New("invalid album identity or revision")
	}
	var response *http.Response
	album, err := c.apiWithResponse(&response).UpdatePhotoAlbum(ctx, &apiclient.UpdatePhotoAlbumRequestOptions{PathParams: &apiclient.UpdatePhotoAlbumPath{SetID: id}, Header: &apiclient.UpdatePhotoAlbumHeaders{IfMatch: revisionIfMatch(revision)}, Body: &body})
	return photoAlbumMutationResponse(response, album, err, id)
}

func (c *Connection) SetPhotoAlbumCover(ctx context.Context, id string, revision int64, assetID *string) (api.PhotoAlbum, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.PhotoAlbum{}, errors.New("invalid album identity or revision")
	}
	var response *http.Response
	album, err := c.apiWithResponse(&response).SetPhotoAlbumCover(ctx, &apiclient.SetPhotoAlbumCoverRequestOptions{PathParams: &apiclient.SetPhotoAlbumCoverPath{SetID: id}, Header: &apiclient.SetPhotoAlbumCoverHeaders{IfMatch: revisionIfMatch(revision)}, Body: &api.PhotoAlbumCoverRequest{AssetID: assetID}})
	return photoAlbumMutationResponse(response, album, err, id)
}

func (c *Connection) DuplicatePhotoAlbum(ctx context.Context, id string, revision int64, name string) (api.PhotoAlbum, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.PhotoAlbum{}, errors.New("invalid album identity or revision")
	}
	var response *http.Response
	album, err := c.apiWithResponse(&response).DuplicatePhotoAlbum(ctx, &apiclient.DuplicatePhotoAlbumRequestOptions{PathParams: &apiclient.DuplicatePhotoAlbumPath{SetID: id}, Header: &apiclient.DuplicatePhotoAlbumHeaders{IfMatch: revisionIfMatch(revision)}, Body: &api.PhotoAlbumNameRequest{Name: name}})
	return photoAlbumMutationResponse(response, album, err, "")
}

func (c *Connection) DeletePhotoAlbum(ctx context.Context, id string, revision int64) (api.PhotoAlbum, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.PhotoAlbum{}, errors.New("invalid album identity or revision")
	}
	var response *http.Response
	album, err := c.apiWithResponse(&response).DeletePhotoAlbum(ctx, &apiclient.DeletePhotoAlbumRequestOptions{PathParams: &apiclient.DeletePhotoAlbumPath{SetID: id}, Header: &apiclient.DeletePhotoAlbumHeaders{IfMatch: revisionIfMatch(revision)}})
	return photoAlbumMutationResponse(response, album, err, id)
}

func (c *Connection) AddPhotoAlbumMembers(ctx context.Context, id string, revision int64, body api.PhotoAlbumMembersRequest) (api.PhotoAlbum, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.PhotoAlbum{}, errors.New("invalid album identity or revision")
	}
	var response *http.Response
	album, err := c.apiWithResponse(&response).AddPhotoAlbumMembers(ctx, &apiclient.AddPhotoAlbumMembersRequestOptions{PathParams: &apiclient.AddPhotoAlbumMembersPath{SetID: id}, Header: &apiclient.AddPhotoAlbumMembersHeaders{IfMatch: revisionIfMatch(revision)}, Body: &body})
	return photoAlbumMutationResponse(response, album, err, id)
}

func (c *Connection) RemovePhotoAlbumMembers(ctx context.Context, id string, revision int64, body api.PhotoAlbumMembersRequest) (api.PhotoAlbum, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.PhotoAlbum{}, errors.New("invalid album identity or revision")
	}
	var response *http.Response
	album, err := c.apiWithResponse(&response).RemovePhotoAlbumMembers(ctx, &apiclient.RemovePhotoAlbumMembersRequestOptions{PathParams: &apiclient.RemovePhotoAlbumMembersPath{SetID: id}, Header: &apiclient.RemovePhotoAlbumMembersHeaders{IfMatch: revisionIfMatch(revision)}, Body: &body})
	return photoAlbumMutationResponse(response, album, err, id)
}

func (c *Connection) BrowsePhotoAlbum(ctx context.Context, request api.PhotoBrowseRequest) (api.PhotoBrowsePage, error) {
	if !validUUIDv4(request.SetID) {
		return api.PhotoBrowsePage{}, errors.New("invalid album ID")
	}
	page, err := c.API().ListPhotoAssets(ctx, &apiclient.ListPhotoAssetsRequestOptions{Body: &request})
	if err != nil {
		return api.PhotoBrowsePage{}, err
	}
	return *page, nil
}
