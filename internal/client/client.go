// Package client is the typed HTTP client for the docbank daemon. It shares
// wire types with internal/api (same module), so the contract is checked at
// compile time; agents use the OpenAPI document instead.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type Client struct {
	base string
	key  string
	hc   *http.Client
}

// responseError distinguishes an HTTP response from transport failure while
// preserving the existing typed API error through Unwrap.
type responseError struct {
	status int
	err    error
}

func (e *responseError) Error() string { return e.err.Error() }
func (e *responseError) Unwrap() error { return e.err }

func responseStatus(err error) (int, bool) {
	var response *responseError
	if !errors.As(err, &response) {
		return 0, false
	}
	return response.status, true
}

// ContentStream exposes the catalog identity available before a download and
// the RFC 9530 digest trailer available after Body reaches EOF. Callers prove
// the transfer by comparing ContentDigest with the SHA-256 they compute while
// reading; BlobHash is the vault's expected immutable identity.
type ContentStream struct {
	io.ReadCloser

	VersionID string
	BlobHash  string
	Size      int64
	trailer   http.Header
}

// ContentDigest returns the digest of bytes actually streamed. HTTP trailers
// are populated only after the response body has been read to EOF.
func (s *ContentStream) ContentDigest() string {
	if s == nil {
		return ""
	}
	return s.trailer.Get("Content-Digest")
}

func New(baseURL, apiKey string) *Client {
	return &Client{base: baseURL, key: apiKey, hc: &http.Client{Timeout: 0}}
}

// codeToTypedErr preserves server problem codes that have a stable local
// sentinel for callers using errors.Is.
var codeToTypedErr = map[string]error{
	"not_found":                store.ErrNotFound,
	"exists":                   store.ErrExists,
	"cycle":                    store.ErrCycle,
	"stale_revision":           store.ErrStaleRevision,
	"not_dir":                  store.ErrNotDir,
	"not_file":                 store.ErrNotFile,
	"invalid_name":             store.ErrInvalidName,
	"not_trashed":              store.ErrNotTrashed,
	"is_root":                  store.ErrIsRoot,
	"backup_locked":            backup.ErrRepoLocked,
	"pack_retirement_deferred": packstore.ErrPackRetirementDeferred,
}

func decodeError(resp *http.Response) error {
	var e api.Error
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &e); err != nil || e.Status == 0 {
		return &responseError{status: resp.StatusCode,
			err: fmt.Errorf("daemon returned %s: %s", resp.Status, string(body))}
	}
	return &responseError{status: resp.StatusCode, err: apiProblemError(e)}
}

func apiProblemError(e api.Error) error {
	if target, ok := codeToTypedErr[e.Code]; ok {
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

func (c *Client) Content(ctx context.Context, id int64) (*ContentStream, error) {
	return c.content(ctx, fmt.Sprintf("/api/v1/nodes/%d/content", id),
		fmt.Sprintf("content of node %d", id))
}

// Versions returns one bounded newest-first page of a node's content history.
func (c *Client) Versions(
	ctx context.Context, nodeID int64, limit, offset int,
) (api.ContentVersionPage, error) {
	var page api.ContentVersionPage
	path := fmt.Sprintf("/api/v1/nodes/%d/versions?limit=%d&offset=%d", nodeID, limit, offset)
	err := c.do(ctx, http.MethodGet, path, nil, nil, &page)
	return page, err
}

// Version returns immutable version metadata by stable ID.
func (c *Client) Version(ctx context.Context, id string) (api.ContentVersion, error) {
	var version api.ContentVersion
	err := c.do(ctx, http.MethodGet, "/api/v1/versions/"+url.PathEscape(id), nil, nil, &version)
	return version, err
}

// VersionContent streams immutable bytes by stable version ID.
func (c *Client) VersionContent(ctx context.Context, id string) (*ContentStream, error) {
	return c.content(ctx, "/api/v1/versions/"+url.PathEscape(id)+"/content",
		"content version "+id)
}

func (c *Client) content(ctx context.Context, path, identity string) (*ContentStream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("building content request: %w", err)
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", identity, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeError(resp)
	}
	size, err := strconv.ParseInt(resp.Header.Get(api.BlobSizeHeader), 10, 64)
	if err != nil || size < 0 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s returned invalid %s %q",
			identity, api.BlobSizeHeader, resp.Header.Get(api.BlobSizeHeader))
	}
	hash := resp.Header.Get(api.BlobHashHeader)
	if !validSHA256Hex(hash) {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s returned invalid %s %q", identity, api.BlobHashHeader, hash)
	}
	versionID := resp.Header.Get(api.ContentVersionHeader)
	if !validUUIDv4(versionID) {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s returned invalid %s %q", identity, api.ContentVersionHeader, versionID)
	}
	return &ContentStream{ReadCloser: resp.Body, VersionID: versionID,
		BlobHash: hash, Size: size, trailer: resp.Trailer}, nil
}

func validSHA256Hex(hash string) bool {
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == hash
}

func validUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' ||
		value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	if !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func (c *Client) VerifyNodeContent(ctx context.Context, id, revision int64) (api.ContentVerification, error) {
	var report api.ContentVerification
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/verify", id),
		ifMatch(revision), nil, &report)
	return report, err
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
	return c.IngestWithOptions(ctx, paths, dest, nil)
}

// IngestWithOptions imports server-side paths with the same exclusion rules
// accepted by PreflightIngest.
func (c *Client) IngestWithOptions(
	ctx context.Context,
	paths []string,
	dest string,
	exclude []string,
) (api.IngestReport, error) {
	var rep api.IngestReport
	err := c.do(ctx, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": paths, "dest": dest, "exclude": exclude}, &rep)
	return rep, err
}

// IngestStream imports server-side paths while delivering structured scan and
// ingest progress. Success requires a terminal report event; an HTTP 200 only
// means that the stream started.
func (c *Client) IngestStream(
	ctx context.Context,
	paths []string,
	dest string,
	exclude []string,
	progress func(api.IngestProgress),
) (api.IngestReport, error) {
	body, err := json.Marshal(map[string]any{
		"paths": paths, "dest": dest, "exclude": exclude,
	})
	if err != nil {
		return api.IngestReport{}, fmt.Errorf("encoding ingest request: %w", err)
	}
	const path = "/api/v1/ingest/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return api.IngestReport{}, fmt.Errorf("building ingest stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return api.IngestReport{}, fmt.Errorf("calling daemon (POST %s): %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return api.IngestReport{}, decodeError(resp)
	}

	decoder := json.NewDecoder(resp.Body)
	var result *api.IngestReport
	for {
		var event api.IngestEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				if result == nil {
					return api.IngestReport{}, errors.New("ingest progress stream ended without a result")
				}
				return *result, nil
			}
			return api.IngestReport{}, fmt.Errorf("decoding ingest progress: %w", err)
		}
		if result != nil {
			return api.IngestReport{}, errors.New("ingest progress stream continued after its result")
		}
		switch event.Type {
		case "progress":
			if event.Progress == nil || event.Report != nil || event.Error != nil {
				return api.IngestReport{}, errors.New("ingest returned malformed progress event")
			}
			if progress != nil {
				progress(*event.Progress)
			}
		case "result":
			if event.Report == nil || event.Progress != nil || event.Error != nil {
				return api.IngestReport{}, errors.New("ingest returned malformed result event")
			}
			report := *event.Report
			result = &report
		case "error":
			if event.Error == nil || event.Progress != nil || event.Report != nil {
				return api.IngestReport{}, errors.New("ingest returned malformed error event")
			}
			return api.IngestReport{}, apiProblemError(*event.Error)
		default:
			return api.IngestReport{}, fmt.Errorf(
				"ingest returned unknown progress event type %q", event.Type)
		}
	}
}

// PreflightIngest inventories server-side paths without opening file content
// or mutating the vault.
func (c *Client) PreflightIngest(
	ctx context.Context,
	paths []string,
	exclude []string,
) (api.IngestPreflightReport, error) {
	var rep api.IngestPreflightReport
	err := c.do(ctx, http.MethodPost, "/api/v1/ingest/preflight", nil,
		map[string]any{"paths": paths, "exclude": exclude}, &rep)
	return rep, err
}

// Upload streams one remote file as a digest-checked multipart request. The
// reader is consumed once; callers retain responsibility for closing it when
// it also implements io.Closer.
func (c *Client) Upload(
	ctx context.Context, parentID int64, name, mimeType, expectedHash string,
	expectedSize int64, content io.Reader,
) (api.UploadReceipt, error) {
	var receipt api.UploadReceipt
	if parentID <= 0 {
		return receipt, errors.New("upload parent ID must be positive")
	}
	if _, err := store.NormalizeName(name); err != nil {
		return receipt, fmt.Errorf("upload name: %w", err)
	}
	if !validSHA256Hex(expectedHash) {
		return receipt, errors.New("upload hash must be canonical lowercase SHA-256")
	}
	if expectedSize < 0 {
		return receipt, errors.New("upload size must not be negative")
	}
	if content == nil {
		return receipt, errors.New("upload content reader is nil")
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	} else {
		mediaType, params, err := mime.ParseMediaType(mimeType)
		if err != nil {
			return receipt, fmt.Errorf("upload media type %q: %w", mimeType, err)
		}
		mimeType = mime.FormatMediaType(mediaType, params)
	}

	pipeReader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	writeDone := make(chan error, 1)
	go func() {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", multipart.FileContentDisposition("file", name))
		header.Set("Content-Type", mimeType)
		part, err := multipartWriter.CreatePart(header)
		if err == nil {
			_, err = io.Copy(part, content)
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		if err != nil {
			_ = pipeWriter.CloseWithError(err)
		} else {
			err = pipeWriter.Close()
		}
		writeDone <- err
	}()

	query := url.Values{"parent_id": {strconv.FormatInt(parentID, 10)}, "name": {name}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/v1/uploads?"+query.Encode(), pipeReader)
	if err != nil {
		_ = pipeReader.Close()
		return receipt, fmt.Errorf("building upload request: %w", err)
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set(api.BlobHashHeader, expectedHash)
	req.Header.Set(api.BlobSizeHeader, strconv.FormatInt(expectedSize, 10))
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, callErr := c.hc.Do(req)
	if callErr != nil {
		_ = pipeReader.CloseWithError(callErr)
		<-writeDone
		return receipt, fmt.Errorf("uploading %q: %w", name, callErr)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = pipeReader.Close()
		<-writeDone
		return receipt, decodeError(resp)
	}
	writerErr := <-writeDone
	if writerErr != nil {
		return receipt, fmt.Errorf("streaming upload %q: %w", name, writerErr)
	}
	if err := json.NewDecoder(resp.Body).Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("decoding upload response: %w", err)
	}
	return receipt, nil
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

func (c *Client) StorageRepack(ctx context.Context, maxBytes int64, minAge time.Duration,
	minDeadBytes int64) (api.StorageRepackReport, error) {
	var report api.StorageRepackReport
	err := c.do(ctx, http.MethodPost, "/api/v1/storage/repack", nil, map[string]any{
		"max_bytes": maxBytes, "min_age": minAge.String(), "min_dead_bytes": minDeadBytes,
	}, &report)
	return report, err
}

func (c *Client) Verify(ctx context.Context) (api.VerifyReport, error) {
	var rep api.VerifyReport
	err := c.do(ctx, http.MethodPost, "/api/v1/verify", nil, nil, &rep)
	return rep, err
}

func (c *Client) BackupInit(ctx context.Context, repo string) (api.BackupRepository, error) {
	var out api.BackupRepository
	err := c.do(ctx, http.MethodPost, "/api/v1/backup/init", nil,
		map[string]any{"repo": repo}, &out)
	return out, err
}

type BackupCreateOptions struct {
	Repo        string
	Tag         string
	Jobs        int
	ForceUnlock bool
}

func (c *Client) BackupCreate(
	ctx context.Context, opts BackupCreateOptions,
) (api.BackupSnapshot, error) {
	var out api.BackupSnapshot
	err := c.do(ctx, http.MethodPost, "/api/v1/backup/snapshots", nil,
		backupCreateRequest(opts), &out)
	return out, err
}

// BackupCreateStream creates a snapshot while delivering structured progress
// events. A successful HTTP status only starts the stream; the method returns
// success only after receiving its terminal result event.
func (c *Client) BackupCreateStream(
	ctx context.Context,
	opts BackupCreateOptions,
	progress func(api.BackupProgress),
) (api.BackupSnapshot, error) {
	body, err := json.Marshal(backupCreateRequest(opts))
	if err != nil {
		return api.BackupSnapshot{}, fmt.Errorf("encoding backup create request: %w", err)
	}
	const path = "/api/v1/backup/snapshots/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return api.BackupSnapshot{}, fmt.Errorf("building backup create stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return api.BackupSnapshot{}, fmt.Errorf("calling daemon (POST %s): %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return api.BackupSnapshot{}, decodeError(resp)
	}

	decoder := json.NewDecoder(resp.Body)
	var result *api.BackupSnapshot
	for {
		var event api.BackupCreateEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				if result == nil {
					return api.BackupSnapshot{}, errors.New(
						"backup create progress stream ended without a result")
				}
				return *result, nil
			}
			return api.BackupSnapshot{}, fmt.Errorf("decoding backup create progress: %w", err)
		}
		if result != nil {
			return api.BackupSnapshot{}, errors.New(
				"backup create progress stream continued after its result")
		}
		switch event.Type {
		case "progress":
			if event.Progress == nil || event.Snapshot != nil || event.Error != nil {
				return api.BackupSnapshot{}, errors.New("backup create returned malformed progress event")
			}
			if progress != nil {
				progress(*event.Progress)
			}
		case "result":
			if event.Snapshot == nil || event.Progress != nil || event.Error != nil {
				return api.BackupSnapshot{}, errors.New("backup create returned malformed result event")
			}
			snapshot := *event.Snapshot
			result = &snapshot
		case "error":
			if event.Error == nil || event.Progress != nil || event.Snapshot != nil {
				return api.BackupSnapshot{}, errors.New("backup create returned malformed error event")
			}
			return api.BackupSnapshot{}, apiProblemError(*event.Error)
		default:
			return api.BackupSnapshot{}, fmt.Errorf(
				"backup create returned unknown progress event type %q", event.Type)
		}
	}
}

func backupCreateRequest(opts BackupCreateOptions) map[string]any {
	return map[string]any{
		"repo": opts.Repo, "tag": opts.Tag, "jobs": opts.Jobs, "force_unlock": opts.ForceUnlock,
	}
}

func (c *Client) BackupList(ctx context.Context, repo string) ([]api.BackupSnapshot, error) {
	var out api.BackupSnapshotList
	query := url.Values{}
	if repo != "" {
		query.Set("repo", repo)
	}
	path := "/api/v1/backup/snapshots"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	err := c.do(ctx, http.MethodGet, path, nil, nil, &out)
	return out.Items, err
}

// Jobs returns the stable, name-sorted snapshot of daemon background work.
func (c *Client) Jobs(ctx context.Context) ([]api.Job, error) {
	var out api.JobList
	err := c.do(ctx, http.MethodGet, "/api/v1/jobs", nil, nil, &out)
	return out.Items, err
}

type BackupVerifyOptions struct {
	Repo        string
	SnapshotID  string
	All         bool
	Quick       bool
	Jobs        int
	ForceUnlock bool
}

func (c *Client) BackupVerify(
	ctx context.Context, opts BackupVerifyOptions,
) (api.BackupVerifyReport, error) {
	var out api.BackupVerifyReport
	err := c.do(ctx, http.MethodPost, "/api/v1/backup/verify", nil,
		backupVerifyRequest(opts), &out)
	return out, err
}

// BackupVerifyStream verifies a repository while delivering structured
// progress. A successful HTTP status only starts the stream; the method
// returns success only after receiving its terminal report event.
func (c *Client) BackupVerifyStream(
	ctx context.Context,
	opts BackupVerifyOptions,
	progress func(api.BackupProgress),
) (api.BackupVerifyReport, error) {
	body, err := json.Marshal(backupVerifyRequest(opts))
	if err != nil {
		return api.BackupVerifyReport{}, fmt.Errorf("encoding backup verify request: %w", err)
	}
	const path = "/api/v1/backup/verify/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return api.BackupVerifyReport{}, fmt.Errorf("building backup verify stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return api.BackupVerifyReport{}, fmt.Errorf("calling daemon (POST %s): %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return api.BackupVerifyReport{}, decodeError(resp)
	}

	decoder := json.NewDecoder(resp.Body)
	var result *api.BackupVerifyReport
	for {
		var event api.BackupVerifyEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				if result == nil {
					return api.BackupVerifyReport{}, errors.New(
						"backup verify progress stream ended without a result")
				}
				return *result, nil
			}
			return api.BackupVerifyReport{}, fmt.Errorf("decoding backup verify progress: %w", err)
		}
		if result != nil {
			return api.BackupVerifyReport{}, errors.New(
				"backup verify progress stream continued after its result")
		}
		switch event.Type {
		case "progress":
			if event.Progress == nil || event.Report != nil || event.Error != nil {
				return api.BackupVerifyReport{}, errors.New(
					"backup verify returned malformed progress event")
			}
			if progress != nil {
				progress(*event.Progress)
			}
		case "result":
			if event.Report == nil || event.Progress != nil || event.Error != nil {
				return api.BackupVerifyReport{}, errors.New(
					"backup verify returned malformed result event")
			}
			report := *event.Report
			result = &report
		case "error":
			if event.Error == nil || event.Progress != nil || event.Report != nil {
				return api.BackupVerifyReport{}, errors.New(
					"backup verify returned malformed error event")
			}
			return api.BackupVerifyReport{}, apiProblemError(*event.Error)
		default:
			return api.BackupVerifyReport{}, fmt.Errorf(
				"backup verify returned unknown progress event type %q", event.Type)
		}
	}
}

func backupVerifyRequest(opts BackupVerifyOptions) map[string]any {
	return map[string]any{
		"repo": opts.Repo, "snapshot_id": opts.SnapshotID, "all": opts.All,
		"quick": opts.Quick, "jobs": opts.Jobs, "force_unlock": opts.ForceUnlock,
	}
}

type BackupRestoreOptions struct {
	Repo        string
	Target      string
	SnapshotID  string
	Overwrite   bool
	Jobs        int
	ForceUnlock bool
}

func (c *Client) BackupRestore(
	ctx context.Context, opts BackupRestoreOptions,
) (api.BackupRestoreReport, error) {
	var out api.BackupRestoreReport
	err := c.do(ctx, http.MethodPost, "/api/v1/backup/restore", nil,
		backupRestoreRequest(opts), &out)
	return out, err
}

// BackupRestoreStream restores and proves a snapshot while delivering
// structured progress. Success requires a terminal report event, not merely
// successful HTTP headers.
func (c *Client) BackupRestoreStream(
	ctx context.Context,
	opts BackupRestoreOptions,
	progress func(api.BackupProgress),
) (api.BackupRestoreReport, error) {
	body, err := json.Marshal(backupRestoreRequest(opts))
	if err != nil {
		return api.BackupRestoreReport{}, fmt.Errorf("encoding backup restore request: %w", err)
	}
	const path = "/api/v1/backup/restore/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return api.BackupRestoreReport{}, fmt.Errorf("building backup restore stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return api.BackupRestoreReport{}, fmt.Errorf("calling daemon (POST %s): %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return api.BackupRestoreReport{}, decodeError(resp)
	}

	decoder := json.NewDecoder(resp.Body)
	var result *api.BackupRestoreReport
	for {
		var event api.BackupRestoreEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				if result == nil {
					return api.BackupRestoreReport{}, errors.New(
						"backup restore progress stream ended without a result")
				}
				return *result, nil
			}
			return api.BackupRestoreReport{}, fmt.Errorf("decoding backup restore progress: %w", err)
		}
		if result != nil {
			return api.BackupRestoreReport{}, errors.New(
				"backup restore progress stream continued after its result")
		}
		switch event.Type {
		case "progress":
			if event.Progress == nil || event.Report != nil || event.Error != nil {
				return api.BackupRestoreReport{}, errors.New(
					"backup restore returned malformed progress event")
			}
			if progress != nil {
				progress(*event.Progress)
			}
		case "result":
			if event.Report == nil || event.Progress != nil || event.Error != nil {
				return api.BackupRestoreReport{}, errors.New(
					"backup restore returned malformed result event")
			}
			report := *event.Report
			result = &report
		case "error":
			if event.Error == nil || event.Progress != nil || event.Report != nil {
				return api.BackupRestoreReport{}, errors.New(
					"backup restore returned malformed error event")
			}
			return api.BackupRestoreReport{}, apiProblemError(*event.Error)
		default:
			return api.BackupRestoreReport{}, fmt.Errorf(
				"backup restore returned unknown progress event type %q", event.Type)
		}
	}
}

func backupRestoreRequest(opts BackupRestoreOptions) map[string]any {
	return map[string]any{
		"repo": opts.Repo, "target": opts.Target, "snapshot_id": opts.SnapshotID,
		"overwrite": opts.Overwrite, "jobs": opts.Jobs, "force_unlock": opts.ForceUnlock,
	}
}

func (c *Client) Shutdown(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.do(ctx, http.MethodPost, "/api/daemon/shutdown",
		map[string]string{"X-Docbank-Daemon-Token": token}, nil, nil)
}
