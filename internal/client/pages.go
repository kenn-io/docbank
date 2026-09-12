package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func (c *Client) PageInventory(ctx context.Context, selection store.PageBinding) (api.PageInventoryResponse, error) {
	var out api.PageInventoryResponse
	err := c.do(ctx, http.MethodPost, "/api/v1/pages/inventory", nil, api.PageSelectionRequest{Selection: selection}, &out)
	return out, err
}
func (c *Client) CreatePageRenderJob(ctx context.Context, request api.PageRenderRequest) (store.PageRenderJob, error) {
	var out store.PageRenderJob
	err := c.do(ctx, http.MethodPost, "/api/v1/pages/jobs", nil, request, &out)
	return out, err
}
func (c *Client) PageRenderJob(ctx context.Context, id string, selection store.PageBinding) (store.PageRenderJob, error) {
	return c.pageJob(ctx, id, selection, false)
}
func (c *Client) CancelPageRenderJob(ctx context.Context, id string, selection store.PageBinding) (store.PageRenderJob, error) {
	return c.pageJob(ctx, id, selection, true)
}
func (c *Client) pageJob(ctx context.Context, id string, selection store.PageBinding, cancel bool) (store.PageRenderJob, error) {
	var out store.PageRenderJob
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return out, errors.New("invalid page job ID")
	}
	path := "/api/v1/pages/jobs/" + id
	if cancel {
		path += "/cancel"
	}
	err = c.do(ctx, http.MethodPost, path, nil, api.PageSelectionRequest{Selection: selection}, &out)
	return out, err
}

// ReadPageImage returns only bytes matching the caller's exact accepted
// receipt and the server's returned frame/recipe/hash headers.
func (c *Client) ReadPageImage(ctx context.Context, selected api.PageImageRequest) ([]byte, error) {
	values := url.Values{"node_id": {strconv.FormatInt(selected.NodeID, 10)}, "revision": {strconv.FormatInt(selected.Revision, 10)}, "version_id": {selected.VersionID}, "source_sha256": {selected.SourceSHA256}, "source_size": {strconv.FormatInt(selected.SourceSize, 10)}, "page": {strconv.Itoa(selected.Page)}, "recipe_sha256": {selected.RecipeSHA256}, "frame_sha256": {selected.FrameSHA256}, "image_sha256": {selected.ImageSHA256}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/pages/image?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Api-Key", c.key)
	response, err := c.hc.Do(request)
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
