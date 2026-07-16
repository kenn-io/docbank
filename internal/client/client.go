// Package client is the typed HTTP client for the docbank daemon. It shares
// wire types with internal/api (same module), so the contract is checked at
// compile time; agents use the OpenAPI document instead.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"go.kenn.io/docbank/internal/jsontext"
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

// CopyVerified writes the response body and succeeds only after the complete
// stream agrees with the catalog size and SHA-256 identity and with the digest
// trailer computed by the daemon. Bytes may already have reached w when an
// error is returned, so callers publishing a file must write to private staging
// and publish only after this method succeeds.
func (s *ContentStream) CopyVerified(w io.Writer) (int64, error) {
	if s == nil || s.ReadCloser == nil {
		return 0, errors.New("copying content: nil stream")
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(w, hash), s)
	if err != nil {
		return written, fmt.Errorf("copying content: %w", err)
	}
	if written != s.Size {
		return written, fmt.Errorf("verifying content: received %d bytes, expected %d", written, s.Size)
	}
	computedHash := hex.EncodeToString(hash.Sum(nil))
	if computedHash != s.BlobHash {
		return written, fmt.Errorf("verifying content: computed SHA-256 %s, expected %s",
			computedHash, s.BlobHash)
	}
	wantDigest := "sha-256=:" + base64.StdEncoding.EncodeToString(hash.Sum(nil)) + ":"
	gotDigest := s.ContentDigest()
	if gotDigest == "" {
		return written, errors.New("verifying content: response lacks terminal Content-Digest")
	}
	if gotDigest != wantDigest {
		return written, fmt.Errorf("verifying content: terminal Content-Digest %q, expected %q",
			gotDigest, wantDigest)
	}
	return written, nil
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
	"invalid_tag":              store.ErrInvalidTag,
	"not_trashed":              store.ErrNotTrashed,
	"is_root":                  store.ErrIsRoot,
	"version_node_mismatch":    store.ErrVersionNodeMismatch,
	"version_already_current":  store.ErrVersionAlreadyCurrent,
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
		b, err := marshalJSONRequest(in)
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

// marshalJSONRequest preserves every Go string before encoding/json can
// replace invalid UTF-8 with U+FFFD. The post-marshal check also keeps custom
// encoders from introducing malformed surrogate escapes. All typed JSON
// request paths, including streaming operations, go through this boundary.
func marshalJSONRequest(in any) ([]byte, error) {
	if err := jsontext.ValidateValue(in, "JSON request"); err != nil {
		return nil, err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if err := jsontext.Validate(body, "JSON request"); err != nil {
		return nil, err
	}
	return body, nil
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
	stream, err := c.content(ctx, "/api/v1/versions/"+url.PathEscape(id)+"/content",
		"content version "+id)
	if err != nil {
		return nil, err
	}
	if stream.VersionID != id {
		_ = stream.Close()
		return nil, fmt.Errorf("content version %s returned version identity %s", id, stream.VersionID)
	}
	return stream, nil
}

// ContentReferences returns one bounded page of logical version references to
// canonical SHA-256 content. It never infers authority from physical files.
func (c *Client) ContentReferences(
	ctx context.Context, hash string, limit, offset int,
) (api.ContentReferencePage, error) {
	var page api.ContentReferencePage
	if !validSHA256Hex(hash) {
		return page, errors.New("content hash must be canonical lowercase SHA-256")
	}
	if limit < 1 || limit > 1000 {
		return page, errors.New("content-reference limit must be between 1 and 1000")
	}
	if offset < 0 {
		return page, errors.New("content-reference offset must not be negative")
	}
	path := fmt.Sprintf("/api/v1/content-references?sha256=%s&limit=%d&offset=%d",
		url.QueryEscape(hash), limit, offset)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &page); err != nil {
		return page, err
	}
	if err := validateContentReferencePage(page, hash, limit, offset); err != nil {
		return api.ContentReferencePage{}, err
	}
	return page, nil
}

func validateContentReferencePage(page api.ContentReferencePage, hash string, limit, offset int) error {
	if page.Limit != limit || page.Offset != offset || page.Total < 0 ||
		len(page.Items) > limit ||
		(len(page.Items) == 0 && offset < page.Total) ||
		(len(page.Items) > 0 && offset+len(page.Items) > page.Total) {
		return errors.New("content-reference response has inconsistent pagination")
	}
	seen := make(map[string]struct{}, len(page.Items))
	for i, ref := range page.Items {
		if !validUUIDv4(ref.Version.ID) || !validUUIDv4(ref.Version.IntroducedOperationID) ||
			!validUUIDv4(ref.Node.CurrentVersionID) || ref.Node.Kind != "file" ||
			ref.Version.NodeID != ref.Node.ID || ref.Version.BlobHash != hash ||
			!validSHA256Hex(ref.Node.BlobHash) || ref.Version.Size < 0 || ref.Node.Size < 0 {
			return fmt.Errorf("content-reference response item %d has inconsistent identity", i)
		}
		_, duplicate := seen[ref.Version.ID]
		if duplicate {
			return fmt.Errorf("content-reference response repeats version %s", ref.Version.ID)
		}
		seen[ref.Version.ID] = struct{}{}
		current := ref.Version.ID == ref.Node.CurrentVersionID
		if ref.IsCurrent != current {
			return fmt.Errorf("content-reference response item %d has inconsistent current state", i)
		}
		if current && (ref.Node.BlobHash != hash || ref.Node.Size != ref.Version.Size ||
			ref.Node.MimeType != ref.Version.MimeType) {
			return fmt.Errorf("content-reference response item %d has inconsistent current authority", i)
		}
		if (ref.Node.TrashedAt != "" && ref.Path != "") ||
			(ref.Node.TrashedAt == "" && !strings.HasPrefix(ref.Path, "/")) {
			return fmt.Errorf("content-reference response item %d has inconsistent path state", i)
		}
	}
	return nil
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

// Tags returns one bounded name-sorted page of tag definitions.
func (c *Client) Tags(ctx context.Context, limit, offset int) (api.TagPage, error) {
	var page api.TagPage
	path := fmt.Sprintf("/api/v1/tags?limit=%d&offset=%d", limit, offset)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &page); err != nil {
		return page, err
	}
	if err := validateTagPage(page, limit, offset); err != nil {
		return api.TagPage{}, err
	}
	return page, nil
}

// Tag returns one tag definition by stable ID.
func (c *Client) Tag(ctx context.Context, id string) (api.Tag, error) {
	var tag api.Tag
	if !validUUIDv4(id) {
		return tag, errors.New("tag ID must be a canonical UUIDv4")
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/tags/"+url.PathEscape(id), nil, nil, &tag); err != nil {
		return tag, err
	}
	if err := validateTag(tag); err != nil {
		return api.Tag{}, err
	}
	if tag.ID != id {
		return api.Tag{}, fmt.Errorf("tag response ID %s does not match request %s", tag.ID, id)
	}
	return tag, nil
}

// TagByName resolves one exact normalized tag name.
func (c *Client) TagByName(ctx context.Context, name string) (api.Tag, error) {
	var tag api.Tag
	normalized, err := store.NormalizeTagName(name)
	if err != nil {
		return tag, err
	}
	path := "/api/v1/tags/by-name?name=" + url.QueryEscape(normalized)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &tag); err != nil {
		return tag, err
	}
	if err := validateTag(tag); err != nil {
		return api.Tag{}, err
	}
	if tag.Name != normalized {
		return api.Tag{}, fmt.Errorf("tag response name %q does not match request %q", tag.Name, normalized)
	}
	return tag, nil
}

// CreateTag defines a tag with a server-allocated stable ID.
func (c *Client) CreateTag(ctx context.Context, name string) (api.Tag, error) {
	var tag api.Tag
	normalized, err := store.NormalizeTagName(name)
	if err != nil {
		return tag, err
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/tags", nil,
		map[string]string{"name": normalized}, &tag); err != nil {
		return tag, err
	}
	if err := validateTag(tag); err != nil {
		return api.Tag{}, err
	}
	if tag.Name != normalized || tag.AssignmentCount != 0 {
		return api.Tag{}, errors.New("created tag response has inconsistent authority")
	}
	return tag, nil
}

// RenameTag changes a tag's display name without changing its stable ID.
func (c *Client) RenameTag(ctx context.Context, id, name string) (api.Tag, error) {
	var tag api.Tag
	if !validUUIDv4(id) {
		return tag, errors.New("tag ID must be a canonical UUIDv4")
	}
	normalized, err := store.NormalizeTagName(name)
	if err != nil {
		return tag, err
	}
	if err := c.do(ctx, http.MethodPatch, "/api/v1/tags/"+url.PathEscape(id), nil,
		map[string]string{"name": normalized}, &tag); err != nil {
		return tag, err
	}
	if err := validateTag(tag); err != nil {
		return api.Tag{}, err
	}
	if tag.ID != id || tag.Name != normalized {
		return api.Tag{}, errors.New("renamed tag response has inconsistent authority")
	}
	return tag, nil
}

// DeleteTag removes one tag definition and its complete assignment set.
func (c *Client) DeleteTag(ctx context.Context, id string) (api.TagDeletionReceipt, error) {
	var receipt api.TagDeletionReceipt
	if !validUUIDv4(id) {
		return receipt, errors.New("tag ID must be a canonical UUIDv4")
	}
	if err := c.do(ctx, http.MethodDelete, "/api/v1/tags/"+url.PathEscape(id), nil,
		nil, &receipt); err != nil {
		return receipt, err
	}
	if err := validateTag(receipt.Tag); err != nil {
		return api.TagDeletionReceipt{}, err
	}
	if receipt.Tag.ID != id || receipt.RemovedAssignments != receipt.Tag.AssignmentCount {
		return api.TagDeletionReceipt{}, errors.New("deleted tag response has inconsistent authority")
	}
	return receipt, nil
}

// NodeTags returns one bounded page of tags attached to nodeID.
func (c *Client) NodeTags(
	ctx context.Context, nodeID int64, limit, offset int,
) (api.TagPage, error) {
	var page api.TagPage
	if nodeID <= 0 {
		return page, errors.New("tagged node ID must be positive")
	}
	path := fmt.Sprintf("/api/v1/nodes/%d/tags?limit=%d&offset=%d", nodeID, limit, offset)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &page); err != nil {
		return page, err
	}
	if err := validateTagPage(page, limit, offset); err != nil {
		return api.TagPage{}, err
	}
	return page, nil
}

// TaggedNodes returns one bounded page of live or trashed nodes carrying tagID.
func (c *Client) TaggedNodes(
	ctx context.Context, tagID string, limit, offset int,
) (api.TaggedNodePage, error) {
	var page api.TaggedNodePage
	if !validUUIDv4(tagID) {
		return page, errors.New("tag ID must be a canonical UUIDv4")
	}
	path := fmt.Sprintf("/api/v1/tags/%s/nodes?limit=%d&offset=%d",
		url.PathEscape(tagID), limit, offset)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &page); err != nil {
		return page, err
	}
	if err := validateTaggedNodePage(page, limit, offset); err != nil {
		return api.TaggedNodePage{}, err
	}
	return page, nil
}

// AssignTag attaches tagID to nodeID under an optimistic node revision.
func (c *Client) AssignTag(
	ctx context.Context, tagID string, nodeID, revision int64,
) (api.TagAssignmentReceipt, error) {
	return c.changeTagAssignment(ctx, http.MethodPut, tagID, nodeID, revision)
}

// UnassignTag removes tagID from nodeID under an optimistic node revision.
func (c *Client) UnassignTag(
	ctx context.Context, tagID string, nodeID, revision int64,
) (api.TagAssignmentReceipt, error) {
	return c.changeTagAssignment(ctx, http.MethodDelete, tagID, nodeID, revision)
}

// AssignTagPath resolves a live virtual path and assigns tagID in one daemon
// transaction, so ancestor moves cannot retarget the operation between calls.
func (c *Client) AssignTagPath(
	ctx context.Context, tagID, path string,
) (api.TagAssignmentReceipt, error) {
	return c.changeTagAssignmentPath(ctx, http.MethodPut, tagID, path)
}

// UnassignTagPath resolves a live virtual path and removes tagID in one daemon
// transaction, so ancestor moves cannot retarget the operation between calls.
func (c *Client) UnassignTagPath(
	ctx context.Context, tagID, path string,
) (api.TagAssignmentReceipt, error) {
	return c.changeTagAssignmentPath(ctx, http.MethodDelete, tagID, path)
}

func (c *Client) changeTagAssignment(
	ctx context.Context, method, tagID string, nodeID, revision int64,
) (api.TagAssignmentReceipt, error) {
	var receipt api.TagAssignmentReceipt
	if !validUUIDv4(tagID) {
		return receipt, errors.New("tag ID must be a canonical UUIDv4")
	}
	if nodeID <= 0 {
		return receipt, errors.New("tagged node ID must be positive")
	}
	if revision < 0 {
		return receipt, errors.New("tagged node revision must not be negative")
	}
	path := fmt.Sprintf("/api/v1/nodes/%d/tags/%s", nodeID, url.PathEscape(tagID))
	receipt, etag, err := c.doTagAssignment(
		ctx, method, path, ifMatch(revision), nil,
	)
	if err != nil {
		return receipt, err
	}
	if err := validateTagAssignmentReceipt(receipt, etag, tagID); err != nil {
		return api.TagAssignmentReceipt{}, err
	}
	if receipt.Node.ID != nodeID {
		return api.TagAssignmentReceipt{}, errors.New(
			"tag assignment response has inconsistent node identity",
		)
	}
	wantRevision := revision
	if receipt.Changed {
		wantRevision++
	}
	if receipt.Node.Revision != wantRevision {
		return api.TagAssignmentReceipt{}, fmt.Errorf(
			"tag assignment response revision %d, expected %d",
			receipt.Node.Revision, wantRevision,
		)
	}
	return receipt, nil
}

func (c *Client) changeTagAssignmentPath(
	ctx context.Context, method, tagID, path string,
) (api.TagAssignmentReceipt, error) {
	var receipt api.TagAssignmentReceipt
	if !validUUIDv4(tagID) {
		return receipt, errors.New("tag ID must be a canonical UUIDv4")
	}
	if !strings.HasPrefix(path, "/") {
		return receipt, errors.New("tag assignment path must be absolute")
	}
	requestPath := "/api/v1/path/tags/" + url.PathEscape(tagID)
	receipt, etag, err := c.doTagAssignment(
		ctx, method, requestPath, nil, map[string]string{"path": path},
	)
	if err != nil {
		return receipt, err
	}
	if err := validateTagAssignmentReceipt(receipt, etag, tagID); err != nil {
		return api.TagAssignmentReceipt{}, err
	}
	if receipt.Node.TrashedAt != "" {
		return api.TagAssignmentReceipt{}, errors.New(
			"path tag assignment returned a trashed node",
		)
	}
	return receipt, nil
}

func (c *Client) doTagAssignment(
	ctx context.Context,
	method, path string,
	headers map[string]string,
	in any,
) (api.TagAssignmentReceipt, string, error) {
	var receipt api.TagAssignmentReceipt
	var body io.Reader
	if in != nil {
		encoded, err := marshalJSONRequest(in)
		if err != nil {
			return receipt, "", fmt.Errorf("encoding tag assignment request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return receipt, "", fmt.Errorf("building tag assignment request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return receipt, "", fmt.Errorf("changing tag assignment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return receipt, "", decodeError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&receipt); err != nil {
		return receipt, "", fmt.Errorf("decoding tag assignment response: %w", err)
	}
	return receipt, resp.Header.Get("ETag"), nil
}

func validateTag(tag api.Tag) error {
	if !validUUIDv4(tag.ID) || tag.AssignmentCount < 0 {
		return errors.New("tag response has invalid identity or assignment count")
	}
	normalized, err := store.NormalizeTagName(tag.Name)
	if err != nil || normalized != tag.Name {
		return errors.New("tag response has invalid or non-canonical name")
	}
	return nil
}

func validateTagPage(page api.TagPage, limit, offset int) error {
	if limit < 1 || limit > 1000 || offset < 0 || page.Limit != limit ||
		page.Offset != offset || page.Total < 0 || len(page.Items) > limit ||
		(len(page.Items) == 0 && offset < page.Total) ||
		(len(page.Items) > 0 && offset+len(page.Items) > page.Total) {
		return errors.New("tag response has inconsistent pagination")
	}
	seen := make(map[string]struct{}, len(page.Items))
	for i, tag := range page.Items {
		if err := validateTag(tag); err != nil {
			return fmt.Errorf("tag response item %d: %w", i, err)
		}
		if _, duplicate := seen[tag.ID]; duplicate {
			return fmt.Errorf("tag response repeats tag %s", tag.ID)
		}
		seen[tag.ID] = struct{}{}
		if i > 0 && (page.Items[i-1].Name > tag.Name ||
			(page.Items[i-1].Name == tag.Name && page.Items[i-1].ID >= tag.ID)) {
			return errors.New("tag response is not canonically ordered")
		}
	}
	return nil
}

func validateTaggedNodePage(page api.TaggedNodePage, limit, offset int) error {
	if limit < 1 || limit > 1000 || offset < 0 || page.Limit != limit ||
		page.Offset != offset || page.Total < 0 || len(page.Items) > limit ||
		(len(page.Items) == 0 && offset < page.Total) ||
		(len(page.Items) > 0 && offset+len(page.Items) > page.Total) {
		return errors.New("tagged-node response has inconsistent pagination")
	}
	var previous int64
	for i, item := range page.Items {
		if item.Node.ID <= previous || item.Node.Revision < 1 ||
			(item.Node.Kind != "file" && item.Node.Kind != "dir") {
			return fmt.Errorf("tagged-node response item %d has inconsistent identity", i)
		}
		if (item.Node.TrashedAt == "" && !strings.HasPrefix(item.Path, "/")) ||
			(item.Node.TrashedAt != "" && item.Path != "") {
			return fmt.Errorf("tagged-node response item %d has inconsistent path state", i)
		}
		previous = item.Node.ID
	}
	return nil
}

func validateTagAssignmentReceipt(
	receipt api.TagAssignmentReceipt, etag, tagID string,
) error {
	if err := validateTag(receipt.Tag); err != nil {
		return err
	}
	if receipt.Tag.ID != tagID || receipt.Node.ID <= 0 || receipt.Node.Revision < 1 {
		return errors.New("tag assignment response has inconsistent identities")
	}
	if (receipt.Node.TrashedAt == "" && !strings.HasPrefix(receipt.Node.Path, "/")) ||
		(receipt.Node.TrashedAt != "" && receipt.Node.Path != "") {
		return errors.New("tag assignment response has inconsistent path state")
	}
	wantETag := fmt.Sprintf("%q", strconv.FormatInt(receipt.Node.Revision, 10))
	if etag != wantETag {
		return fmt.Errorf("tag assignment response ETag %q, expected %q", etag, wantETag)
	}
	return nil
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
	body, err := marshalJSONRequest(map[string]any{
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

// ReplaceContent streams raw bytes into a new immutable head under an
// optimistic node-revision precondition. Success requires the daemon's receipt
// and ETag to agree with the caller-declared byte identity and the requested
// node; callers retain responsibility for closing content when applicable.
func (c *Client) ReplaceContent(
	ctx context.Context, nodeID, revision int64, mimeType, expectedHash string,
	expectedSize int64, content io.Reader,
) (api.ContentReplacementReceipt, error) {
	var receipt api.ContentReplacementReceipt
	if nodeID <= 0 {
		return receipt, errors.New("replacement node ID must be positive")
	}
	if revision < 0 {
		return receipt, errors.New("replacement revision must not be negative")
	}
	if !validSHA256Hex(expectedHash) {
		return receipt, errors.New("replacement hash must be canonical lowercase SHA-256")
	}
	if expectedSize < 0 {
		return receipt, errors.New("replacement size must not be negative")
	}
	if content == nil {
		return receipt, errors.New("replacement content reader is nil")
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	} else {
		mediaType, params, err := mime.ParseMediaType(mimeType)
		if err != nil {
			return receipt, fmt.Errorf("replacement media type %q: %w", mimeType, err)
		}
		mimeType = mime.FormatMediaType(mediaType, params)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/api/v1/nodes/%d/content", c.base, nodeID), io.NopCloser(content))
	if err != nil {
		return receipt, fmt.Errorf("building replacement request: %w", err)
	}
	req.ContentLength = expectedSize
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("Expect", "100-continue")
	req.Header.Set("If-Match", fmt.Sprintf("%q", strconv.FormatInt(revision, 10)))
	req.Header.Set(api.BlobHashHeader, expectedHash)
	req.Header.Set(api.BlobSizeHeader, strconv.FormatInt(expectedSize, 10))
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return receipt, fmt.Errorf("replacing content of node %d: %w", nodeID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return receipt, decodeError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("decoding content replacement response: %w", err)
	}
	if err := validateReplacementReceipt(
		receipt, resp.Header.Get("ETag"), nodeID, revision, mimeType, expectedHash, expectedSize,
	); err != nil {
		return api.ContentReplacementReceipt{}, err
	}
	return receipt, nil
}

func validateReplacementReceipt(
	receipt api.ContentReplacementReceipt, etag string, nodeID, revision int64,
	mimeType, expectedHash string, expectedSize int64,
) error {
	if receipt.ComputedHash != expectedHash || receipt.ComputedSize != expectedSize {
		return fmt.Errorf("content replacement receipt computed identity %s/%d, expected %s/%d",
			receipt.ComputedHash, receipt.ComputedSize, expectedHash, expectedSize)
	}
	if receipt.Node.ID != nodeID || receipt.Version.NodeID != nodeID {
		return fmt.Errorf("content replacement receipt targets node %d/%d, expected %d",
			receipt.Node.ID, receipt.Version.NodeID, nodeID)
	}
	if receipt.Node.Revision != revision+1 || receipt.Version.NodeRevision != receipt.Node.Revision {
		return fmt.Errorf("content replacement receipt revision %d/%d, expected %d",
			receipt.Node.Revision, receipt.Version.NodeRevision, revision+1)
	}
	if receipt.Version.ID == "" || receipt.Node.CurrentVersionID != receipt.Version.ID {
		return errors.New("content replacement receipt does not install its returned version as current")
	}
	if receipt.Version.TransitionKind != "content_replace" || receipt.Version.SourceVersionID != nil {
		return errors.New("content replacement receipt does not describe a content_replace head")
	}
	if receipt.Node.BlobHash != expectedHash || receipt.Node.Size != expectedSize ||
		receipt.Version.BlobHash != expectedHash || receipt.Version.Size != expectedSize {
		return errors.New("content replacement receipt authority disagrees with declared bytes")
	}
	if receipt.Node.MimeType != mimeType || receipt.Version.MimeType != mimeType {
		return fmt.Errorf("content replacement receipt media type %q/%q, expected %q",
			receipt.Node.MimeType, receipt.Version.MimeType, mimeType)
	}
	expectedETag := fmt.Sprintf("%q", strconv.FormatInt(receipt.Node.Revision, 10))
	if etag != expectedETag {
		return fmt.Errorf("content replacement response ETag %q, expected %q", etag, expectedETag)
	}
	return nil
}

// RevertContent creates a new immutable head from one prior version under an
// optimistic node-revision precondition. The response must bind the requested
// source, new history row, current node authority, and resulting ETag.
func (c *Client) RevertContent(
	ctx context.Context, nodeID, revision int64, sourceVersionID string,
) (api.ContentReversionReceipt, error) {
	var receipt api.ContentReversionReceipt
	if nodeID <= 0 {
		return receipt, errors.New("reversion node ID must be positive")
	}
	if revision < 0 {
		return receipt, errors.New("reversion revision must not be negative")
	}
	if !validUUIDv4(sourceVersionID) {
		return receipt, errors.New("reversion source must be a canonical UUIDv4")
	}
	headers := ifMatch(revision)
	body, err := marshalJSONRequest(map[string]string{"source_version_id": sourceVersionID})
	if err != nil {
		return receipt, fmt.Errorf("encoding content reversion request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/nodes/%d/revert", c.base, nodeID), bytes.NewReader(body))
	if err != nil {
		return receipt, fmt.Errorf("building content reversion request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return receipt, fmt.Errorf("reverting content of node %d: %w", nodeID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return receipt, decodeError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("decoding content reversion response: %w", err)
	}
	if err := validateReversionReceipt(
		receipt, resp.Header.Get("ETag"), nodeID, revision, sourceVersionID,
	); err != nil {
		return api.ContentReversionReceipt{}, err
	}
	return receipt, nil
}

func validateReversionReceipt(
	receipt api.ContentReversionReceipt, etag string, nodeID, revision int64, sourceVersionID string,
) error {
	if receipt.Node.ID != nodeID || receipt.Version.NodeID != nodeID ||
		receipt.SourceVersion.NodeID != nodeID {
		return fmt.Errorf("content reversion receipt targets node %d/%d/%d, expected %d",
			receipt.Node.ID, receipt.Version.NodeID, receipt.SourceVersion.NodeID, nodeID)
	}
	if receipt.Node.Revision != revision+1 || receipt.Version.NodeRevision != receipt.Node.Revision {
		return fmt.Errorf("content reversion receipt revision %d/%d, expected %d",
			receipt.Node.Revision, receipt.Version.NodeRevision, revision+1)
	}
	if !validUUIDv4(receipt.Version.ID) || receipt.Version.ID == sourceVersionID ||
		receipt.Node.CurrentVersionID != receipt.Version.ID {
		return errors.New("content reversion receipt does not install a valid returned version as current")
	}
	if receipt.SourceVersion.ID != sourceVersionID || !validUUIDv4(receipt.SourceVersion.ID) {
		return fmt.Errorf("content reversion receipt source %q, expected %q",
			receipt.SourceVersion.ID, sourceVersionID)
	}
	if receipt.Version.TransitionKind != "content_revert" ||
		receipt.Version.SourceVersionID == nil ||
		*receipt.Version.SourceVersionID != sourceVersionID {
		return errors.New("content reversion receipt does not bind its content_revert source")
	}
	if receipt.SourceVersion.NodeRevision >= receipt.Version.NodeRevision {
		return errors.New("content reversion receipt source is not older than its new head")
	}
	if receipt.Node.BlobHash != receipt.SourceVersion.BlobHash ||
		receipt.Node.Size != receipt.SourceVersion.Size ||
		receipt.Node.MimeType != receipt.SourceVersion.MimeType ||
		receipt.Version.BlobHash != receipt.SourceVersion.BlobHash ||
		receipt.Version.Size != receipt.SourceVersion.Size ||
		receipt.Version.MimeType != receipt.SourceVersion.MimeType {
		return errors.New("content reversion receipt authority disagrees with its source version")
	}
	expectedETag := fmt.Sprintf("%q", strconv.FormatInt(receipt.Node.Revision, 10))
	if etag != expectedETag {
		return fmt.Errorf("content reversion response ETag %q, expected %q", etag, expectedETag)
	}
	return nil
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
	body, err := marshalJSONRequest(backupCreateRequest(opts))
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
	body, err := marshalJSONRequest(backupVerifyRequest(opts))
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
	body, err := marshalJSONRequest(backupRestoreRequest(opts))
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
