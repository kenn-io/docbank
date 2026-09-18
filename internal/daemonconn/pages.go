package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"uuid"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

func (c *Connection) PageInventory(ctx context.Context, selection store.PageBinding) (api.PageInventoryResponse, error) {
	out, err := c.API().PageInventory(ctx, &apiclient.PageInventoryRequestOptions{Body: &api.PageSelectionRequest{Selection: selection}})
	if err != nil {
		return api.PageInventoryResponse{}, err
	}
	return *out, nil
}
func (c *Connection) CreatePageRenderJob(ctx context.Context, request api.PageRenderRequest) (store.PageRenderJob, error) {
	out, err := c.API().CreatePageRenderJob(ctx, &apiclient.CreatePageRenderJobRequestOptions{Body: &request})
	if err != nil {
		return store.PageRenderJob{}, err
	}
	return *out, nil
}
func (c *Connection) PageRenderJob(ctx context.Context, id string, selection store.PageBinding) (store.PageRenderJob, error) {
	return c.pageJob(ctx, id, selection, false)
}
func (c *Connection) CancelPageRenderJob(ctx context.Context, id string, selection store.PageBinding) (store.PageRenderJob, error) {
	return c.pageJob(ctx, id, selection, true)
}
func (c *Connection) pageJob(ctx context.Context, id string, selection store.PageBinding, cancel bool) (store.PageRenderJob, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return store.PageRenderJob{}, errors.New("invalid page job ID")
	}
	request := &api.PageSelectionRequest{Selection: selection}
	var out *store.PageRenderJob
	if cancel {
		out, err = c.API().CancelPageRenderJob(ctx, &apiclient.CancelPageRenderJobRequestOptions{PathParams: &apiclient.CancelPageRenderJobPath{ID: parsed}, Body: request})
	} else {
		out, err = c.API().GetPageRenderJob(ctx, &apiclient.GetPageRenderJobRequestOptions{PathParams: &apiclient.GetPageRenderJobPath{ID: parsed}, Body: request})
	}
	if err != nil {
		return store.PageRenderJob{}, err
	}
	return *out, nil
}

// ReadPageImage returns only bytes matching the caller's exact accepted
// receipt and the server's returned frame/recipe/hash headers.
func (c *Connection) ReadPageImage(ctx context.Context, selected api.PageImageRequest) ([]byte, error) {
	versionID, err := uuid.Parse(selected.VersionID)
	if err != nil || versionID.String() != selected.VersionID {
		return nil, errors.New("invalid page version ID")
	}
	var response *http.Response
	_, err = c.apiWithResponse(&response).ReadPageImage(runtime.WithStreamingResponse(ctx), &apiclient.ReadPageImageRequestOptions{Query: &apiclient.ReadPageImageQuery{
		NodeID: new(selected.NodeID), Revision: new(selected.Revision), VersionID: new(versionID),
		SourceSha256: new(selected.SourceSHA256), SourceSize: new(selected.SourceSize), Page: new(int64(selected.Page)),
		RecipeSha256: new(selected.RecipeSHA256), FrameSha256: new(selected.FrameSHA256), ImageSha256: new(selected.ImageSHA256),
	}})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, decodeError(response)
	}
	if response.ContentLength < 1 || response.ContentLength > document.MaxPageImageBytes || response.Header.Get("Content-Type") != "image/png" || response.Header.Get("X-Docbank-Page-Frame") != selected.FrameSHA256 || response.Header.Get("X-Docbank-Page-Recipe") != selected.RecipeSHA256 || response.Header.Get("X-Docbank-Page-Sha256") != selected.ImageSHA256 {
		return nil, errors.New("page image response identity mismatch")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, document.MaxPageImageBytes+1))
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	if int64(len(data)) != response.ContentLength || hex.EncodeToString(hash[:]) != selected.ImageSHA256 {
		return nil, errors.New("page image response bytes failed verification")
	}
	return data, nil
}
