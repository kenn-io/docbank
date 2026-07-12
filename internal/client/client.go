// Package client is the typed HTTP client for the docbank daemon. It shares
// wire types with internal/api (same module), so the contract is checked at
// compile time; agents use the OpenAPI document instead.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type Client struct {
	base string
	key  string
	hc   *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{base: baseURL, key: apiKey, hc: &http.Client{Timeout: 0}}
}

// codeToStoreErr is the inverse of the server's FromStoreError mapping.
var codeToStoreErr = map[string]error{
	"not_found":      store.ErrNotFound,
	"exists":         store.ErrExists,
	"cycle":          store.ErrCycle,
	"stale_revision": store.ErrStaleRevision,
	"not_dir":        store.ErrNotDir,
	"not_file":       store.ErrNotFile,
	"invalid_name":   store.ErrInvalidName,
	"not_trashed":    store.ErrNotTrashed,
	"is_root":        store.ErrIsRoot,
}

func decodeError(resp *http.Response) error {
	var e api.Error
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &e); err != nil || e.Status == 0 {
		return fmt.Errorf("daemon returned %s: %s", resp.Status, string(body))
	}
	if target, ok := codeToStoreErr[e.Code]; ok {
		return fmt.Errorf("%s: %w", e.Detail, target)
	}
	return fmt.Errorf("daemon error (%d %s): %s", e.Status, e.Code, e.Detail)
}

// do issues one JSON round-trip. Non-nil out must be a pointer; a non-2xx
// status decodes the error envelope instead.
func (c *Client) do(ctx context.Context, method, path string, hdr map[string]string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding %s %s request: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("calling daemon (%s %s): %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}

func ifMatch(rev int64) map[string]string {
	return map[string]string{"If-Match": fmt.Sprintf("%q", strconv.FormatInt(rev, 10))}
}

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil, nil)
}

func (c *Client) Node(ctx context.Context, id int64) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/nodes/%d", id), nil, nil, &n)
	return n, err
}

func (c *Client) Stat(ctx context.Context, path string) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodGet, "/api/v1/path?path="+url.QueryEscape(path), nil, nil, &n)
	return n, err
}

// Children fetches every page. Callers that need streaming can add it when
// a real consumer appears (YAGNI).
func (c *Client) Children(ctx context.Context, id int64) ([]api.Node, error) {
	const pageSize = 1000
	var all []api.Node
	for offset := 0; ; offset += pageSize {
		var page struct {
			Items []api.Node `json:"items"`
			Total int        `json:"total"`
		}
		p := fmt.Sprintf("/api/v1/nodes/%d/children?limit=%d&offset=%d", id, pageSize, offset)
		if err := c.do(ctx, http.MethodGet, p, nil, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if offset+pageSize >= page.Total {
			return all, nil
		}
	}
}

func (c *Client) Content(ctx context.Context, id int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/nodes/%d/content", c.base, id), nil)
	if err != nil {
		return nil, fmt.Errorf("building content request: %w", err)
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching content of node %d: %w", id, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeError(resp)
	}
	return resp.Body, nil
}

func (c *Client) Search(ctx context.Context, query string, limit int) (api.SearchReport, error) {
	var out api.SearchReport
	p := fmt.Sprintf("/api/v1/search?q=%s&limit=%d", url.QueryEscape(query), limit)
	err := c.do(ctx, http.MethodGet, p, nil, nil, &out)
	return out, err
}

func (c *Client) Mkdir(ctx context.Context, parentID int64, name string) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodPost, "/api/v1/nodes", nil,
		map[string]any{"parent_id": parentID, "name": name, "kind": "dir"}, &n)
	return n, err
}

func (c *Client) Ingest(ctx context.Context, paths []string, dest string) (api.IngestReport, error) {
	var rep api.IngestReport
	err := c.do(ctx, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": paths, "dest": dest}, &rep)
	return rep, err
}

func (c *Client) Move(ctx context.Context, id, rev int64, newParentID *int64, newName *string) (api.Node, error) {
	var n api.Node
	body := map[string]any{}
	if newParentID != nil {
		body["new_parent_id"] = *newParentID
	}
	if newName != nil {
		body["new_name"] = *newName
	}
	err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", id), ifMatch(rev), body, &n)
	return n, err
}

func (c *Client) MovePath(ctx context.Context, srcPath, destPath string) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodPost, "/api/v1/path/move", nil,
		map[string]any{"src_path": srcPath, "dest_path": destPath}, &n)
	return n, err
}

func (c *Client) Trash(ctx context.Context, id, rev int64) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", id), ifMatch(rev), nil, &n)
	return n, err
}

func (c *Client) TrashPath(ctx context.Context, path string) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodPost, "/api/v1/path/trash", nil, map[string]any{"path": path}, &n)
	return n, err
}

func (c *Client) Restore(ctx context.Context, id, rev int64) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/restore", id), ifMatch(rev), nil, &n)
	return n, err
}

func (c *Client) TrashList(ctx context.Context) ([]api.Node, error) {
	var out struct {
		Items []api.Node `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/trash", nil, nil, &out)
	return out.Items, err
}

func (c *Client) TrashEmpty(ctx context.Context, olderThan string, run bool) (api.TrashEmptyReport, error) {
	var out api.TrashEmptyReport
	err := c.do(ctx, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"older_than": olderThan, "run": run}, &out)
	return out, err
}

func (c *Client) GC(ctx context.Context, run bool) (api.GCReport, error) {
	var rep api.GCReport
	err := c.do(ctx, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": run}, &rep)
	return rep, err
}

func (c *Client) StorageStatus(ctx context.Context) (api.StorageStatus, error) {
	var status api.StorageStatus
	err := c.do(ctx, http.MethodGet, "/api/v1/storage", nil, nil, &status)
	return status, err
}

func (c *Client) StoragePack(ctx context.Context, maxBytes int64) (api.StoragePackReport, error) {
	var report api.StoragePackReport
	err := c.do(ctx, http.MethodPost, "/api/v1/storage/pack", nil,
		map[string]any{"max_bytes": maxBytes}, &report)
	return report, err
}

func (c *Client) Verify(ctx context.Context) (api.VerifyReport, error) {
	var rep api.VerifyReport
	err := c.do(ctx, http.MethodPost, "/api/v1/verify", nil, nil, &rep)
	return rep, err
}

func (c *Client) Shutdown(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.do(ctx, http.MethodPost, "/api/daemon/shutdown",
		map[string]string{"X-Docbank-Daemon-Token": token}, nil, nil)
}
