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
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/home"
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

type problemError struct {
	code string
	err  error
}

func (e *problemError) Error() string { return e.err.Error() }
func (e *problemError) Unwrap() error { return e.err }

// ProblemCode returns the daemon's stable RFC 7807 extension code when err
// came from a decoded HTTP response or progress-stream error event.
func ProblemCode(err error) (string, bool) {
	var problem *problemError
	if !errors.As(err, &problem) || problem.code == "" {
		return "", false
	}
	return problem.code, true
}

var (
	// ErrIntegrity marks content that failed terminal size, hash, or digest proof.
	ErrIntegrity = errors.New("content integrity verification failed")
	// ErrMaintenanceBusy marks a mutation rejected while exclusive maintenance
	// is running or queued. Callers may retry after the operator-visible work ends.
	ErrMaintenanceBusy = errors.New("vault maintenance is busy")
)

type integrityError struct{ message string }

func (e *integrityError) Error() string        { return e.message }
func (e *integrityError) Is(target error) bool { return target == ErrIntegrity }

func integrityErrorf(format string, args ...any) error {
	return &integrityError{message: fmt.Sprintf(format, args...)}
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
		return written, integrityErrorf("verifying content: received %d bytes, expected %d",
			written, s.Size)
	}
	computedHash := hex.EncodeToString(hash.Sum(nil))
	if computedHash != s.BlobHash {
		return written, integrityErrorf("verifying content: computed SHA-256 %s, expected %s",
			computedHash, s.BlobHash)
	}
	wantDigest := "sha-256=:" + base64.StdEncoding.EncodeToString(hash.Sum(nil)) + ":"
	gotDigest := s.ContentDigest()
	if gotDigest == "" {
		return written, integrityErrorf("verifying content: response lacks terminal Content-Digest")
	}
	if gotDigest != wantDigest {
		return written, integrityErrorf("verifying content: terminal Content-Digest %q, expected %q",
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
	"not_found":                    store.ErrNotFound,
	"exists":                       store.ErrExists,
	"cycle":                        store.ErrCycle,
	"stale_revision":               store.ErrStaleRevision,
	"not_dir":                      store.ErrNotDir,
	"not_file":                     store.ErrNotFile,
	"invalid_name":                 store.ErrInvalidName,
	"invalid_tag":                  store.ErrInvalidTag,
	"invalid_batch_move":           store.ErrInvalidBatchMove,
	"not_trashed":                  store.ErrNotTrashed,
	"is_root":                      store.ErrIsRoot,
	"version_node_mismatch":        store.ErrVersionNodeMismatch,
	"version_already_current":      store.ErrVersionAlreadyCurrent,
	"invalid_version_prune":        store.ErrInvalidVersionPrune,
	"audit_already_enabled":        store.ErrAuditAlreadyEnabled,
	"audit_preview_stale":          store.ErrAuditPreviewStale,
	"audit_not_enrolled":           store.ErrAuditNotEnrolled,
	"invalid_audit_cursor":         store.ErrInvalidAuditCursor,
	"backup_locked":                backup.ErrRepoLocked,
	"backup_restore_target_active": home.ErrVaultLocked,
	"pack_retirement_deferred":     packstore.ErrPackRetirementDeferred,
	"maintenance_busy":             ErrMaintenanceBusy,
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
	var cause error
	if target, ok := codeToTypedErr[e.Code]; ok {
		cause = fmt.Errorf("%s: %w", e.Detail, target)
	} else {
		cause = fmt.Errorf("daemon error (%d %s): %s", e.Status, e.Code, e.Detail)
	}
	return &problemError{code: e.Code, err: cause}
}

// do issues one JSON round-trip. Non-nil out must be a pointer; a non-2xx
// status decodes the error envelope instead.
func (c *Client) do(ctx context.Context, method, path string, hdr map[string]string, in, out any) error {
	_, err := c.doWithHeaders(ctx, method, path, hdr, in, out)
	return err
}

func (c *Client) doWithHeaders(
	ctx context.Context, method, path string, hdr map[string]string, in, out any,
) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := marshalJSONRequest(in)
		if err != nil {
			return nil, fmt.Errorf("encoding %s %s request: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, path, err)
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
		return nil, fmt.Errorf("calling daemon (%s %s): %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Header.Clone(), nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return resp.Header.Clone(), nil
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

// Children fetches every page. Callers that need bounded traversal should use
// ChildrenPage.
func (c *Client) Children(ctx context.Context, id int64) ([]api.Node, error) {
	const pageSize = 1000
	all := []api.Node{}
	for offset := 0; ; {
		page, err := c.ChildrenPage(ctx, id, pageSize, offset)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if len(all) >= page.Total {
			return all, nil
		}
		offset += len(page.Items)
	}
}

// ChildrenPage returns one bounded page of a directory's ordered live
// children. Recursive consumers should use this method instead of assembling
// an unbounded sibling list through Children.
func (c *Client) ChildrenPage(
	ctx context.Context, id int64, limit, offset int,
) (api.NodePage, error) {
	var page api.NodePage
	if id < 1 {
		return page, errors.New("directory node ID must be positive")
	}
	if limit < 1 || limit > 5000 {
		return page, errors.New("child-page limit must be between 1 and 5000")
	}
	if offset < 0 {
		return page, errors.New("child-page offset must not be negative")
	}
	p := fmt.Sprintf("/api/v1/nodes/%d/children?limit=%d&offset=%d", id, limit, offset)
	if err := c.do(ctx, http.MethodGet, p, nil, nil, &page); err != nil {
		return api.NodePage{}, err
	}
	if page.Limit != limit || page.Offset != offset || page.Total < 0 ||
		len(page.Items) > limit ||
		(len(page.Items) == 0 && offset < page.Total) ||
		(len(page.Items) > 0 && offset+len(page.Items) > page.Total) {
		return api.NodePage{}, errors.New("children response has inconsistent pagination")
	}
	return page, nil
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

// Provenance returns one bounded newest-ingest-first page of immutable origin
// facts for a stable file node.
func (c *Client) Provenance(
	ctx context.Context, nodeID int64, limit, offset int,
) (api.ProvenancePage, error) {
	var page api.ProvenancePage
	if nodeID < 1 {
		return page, errors.New("provenance node ID must be positive")
	}
	if limit < 1 || limit > store.MaxProvenancePageSize {
		return page, fmt.Errorf("provenance limit must be between 1 and %d", store.MaxProvenancePageSize)
	}
	if offset < 0 {
		return page, errors.New("provenance offset must not be negative")
	}
	path := fmt.Sprintf("/api/v1/nodes/%d/provenance?limit=%d&offset=%d", nodeID, limit, offset)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &page); err != nil {
		return api.ProvenancePage{}, err
	}
	if err := validateProvenancePage(page, nodeID, limit, offset); err != nil {
		return api.ProvenancePage{}, err
	}
	return page, nil
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
		return nil, integrityErrorf("content version %s returned version identity %s", id, stream.VersionID)
	}
	return stream, nil
}

// PruneContentVersions previews or executes one explicit version-history
// selector under an optimistic node-revision precondition.
func (c *Client) PruneContentVersions(
	ctx context.Context, nodeID, revision int64, request api.VersionPruneRequest,
) (api.VersionPruneReport, error) {
	var report api.VersionPruneReport
	if nodeID <= 0 {
		return report, errors.New("version-prune node ID must be positive")
	}
	if revision < 1 {
		return report, errors.New("version-prune revision must be positive")
	}
	if _, err := api.ParseVersionPruneRequest(request); err != nil {
		return report, err
	}
	headers, err := c.doWithHeaders(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/versions/prune", nodeID), ifMatch(revision), request, &report)
	if err != nil {
		return api.VersionPruneReport{}, err
	}
	if err := validateVersionPruneReport(report, headers.Get("ETag"), nodeID, revision, request.Run); err != nil {
		return api.VersionPruneReport{}, err
	}
	return report, nil
}

func validateVersionPruneReport(
	report api.VersionPruneReport, etag string, nodeID, revision int64, run bool,
) error {
	if err := validateVersionPruneBinding(report, etag, nodeID, revision, run); err != nil {
		return err
	}
	if err := validateVersionPruneCounts(report, run); err != nil {
		return err
	}
	if err := validateVersionPruneVersions(report, nodeID, run); err != nil {
		return err
	}
	return validateVersionPruneCheckpoint(report, nodeID, run)
}

func validateVersionPruneBinding(
	report api.VersionPruneReport, etag string, nodeID, revision int64, run bool,
) error {
	if report.Node.ID != nodeID || report.Run != run {
		return errors.New("version-prune receipt does not bind its request")
	}
	wantRevision := revision
	if report.Changed {
		if !run {
			return errors.New("version-prune dry run reports a mutation")
		}
		wantRevision++
	}
	if report.Node.Revision != wantRevision {
		return fmt.Errorf("version-prune node revision %d, expected %d", report.Node.Revision, wantRevision)
	}
	if etag != strconv.Quote(strconv.FormatInt(report.Node.Revision, 10)) {
		return fmt.Errorf("version-prune response ETag %q disagrees with node revision %d",
			etag, report.Node.Revision)
	}
	return nil
}

func validateVersionPruneCounts(report api.VersionPruneReport, run bool) error {
	if !run && (report.DeletedVersions != 0 || report.Checkpoint != nil) {
		return errors.New("version-prune dry run reports a mutation")
	}
	if run && report.DeletedVersions != len(report.Candidates) {
		return errors.New("version-prune receipt has inconsistent deletion counts")
	}
	if report.Changed != (report.DeletedVersions > 0) {
		return errors.New("version-prune receipt has inconsistent changed state")
	}
	if report.LogicalBytes < 0 || report.UniqueBlobs < 0 || report.SharedBlobs < 0 ||
		report.ReleasableBlobs < 0 || report.ReleasableBytes < 0 ||
		report.LooseBlobsPendingGC < 0 || report.LooseBytesPendingGC < 0 ||
		report.PackedBlobsPendingRepack < 0 || report.PackedBytesPendingRepack < 0 ||
		report.DeletedVersions < 0 {
		return errors.New("version-prune receipt contains negative counts")
	}
	if report.UniqueBlobs != report.SharedBlobs+report.ReleasableBlobs ||
		report.ReleasableBlobs != report.LooseBlobsPendingGC+report.PackedBlobsPendingRepack {
		return errors.New("version-prune receipt has inconsistent blob counts")
	}
	return nil
}

func validateVersionPruneVersions(report api.VersionPruneReport, nodeID int64, run bool) error {
	logicalBytes := int64(0)
	uniqueBlobs := make(map[string]bool, len(report.Candidates))
	seenVersions := make(map[string]bool, len(report.Candidates)+len(report.DependencyRetained))
	checkpointPreviewIncludesCurrent := false
	for _, version := range report.Candidates {
		if err := validateVersionPruneReceiptVersion(version, nodeID, seenVersions); err != nil {
			return err
		}
		currentCheckpointPreview := !run && report.CheckpointRequired &&
			version.ID == report.Node.CurrentVersionID
		checkpointPreviewIncludesCurrent = checkpointPreviewIncludesCurrent || currentCheckpointPreview
		if version.ID == report.Node.CurrentVersionID && !currentCheckpointPreview {
			return errors.New("version-prune receipt selects invalid current history")
		}
		if version.Size > math.MaxInt64-logicalBytes {
			return errors.New("version-prune receipt logical size overflows")
		}
		logicalBytes += version.Size
		uniqueBlobs[version.BlobHash] = true
	}
	for _, version := range report.DependencyRetained {
		if err := validateVersionPruneReceiptVersion(version, nodeID, seenVersions); err != nil {
			return err
		}
	}
	if report.LogicalBytes != logicalBytes || report.UniqueBlobs != len(uniqueBlobs) {
		return errors.New("version-prune receipt has inconsistent logical inventory")
	}
	if !run && report.CheckpointRequired && !checkpointPreviewIncludesCurrent {
		return errors.New("version-prune preview omits its checkpointed current version")
	}
	return nil
}

func validateVersionPruneReceiptVersion(
	version api.ContentVersion, nodeID int64, seen map[string]bool,
) error {
	if version.NodeID != nodeID || !validVersionPruneVersion(version) {
		return errors.New("version-prune receipt contains an invalid version")
	}
	if seen[version.ID] {
		return errors.New("version-prune receipt repeats a version")
	}
	seen[version.ID] = true
	return nil
}

func validateVersionPruneCheckpoint(report api.VersionPruneReport, nodeID int64, run bool) error {
	if report.Checkpoint != nil {
		if !run || !report.Changed || !report.CheckpointRequired ||
			report.Checkpoint.NodeID != nodeID || report.Node.CurrentVersionID != report.Checkpoint.ID ||
			report.Checkpoint.NodeRevision != report.Node.Revision ||
			report.Checkpoint.BlobHash != report.Node.BlobHash ||
			report.Checkpoint.Size != report.Node.Size || report.Checkpoint.MimeType != report.Node.MimeType ||
			report.Checkpoint.TransitionKind != "content_replace" || report.Checkpoint.SourceVersionID != nil ||
			!validVersionPruneVersion(*report.Checkpoint) {
			return errors.New("version-prune receipt contains an invalid checkpoint")
		}
	} else if run && report.Changed && report.CheckpointRequired {
		return errors.New("version-prune receipt omits its required checkpoint")
	}
	return nil
}

func validVersionPruneVersion(version api.ContentVersion) bool {
	if !validUUIDv4(version.ID) || !validUUIDv4(version.IntroducedOperationID) ||
		!validSHA256Hex(version.BlobHash) || version.Size < 0 || version.NodeRevision < 1 {
		return false
	}
	switch version.TransitionKind {
	case "content_create", "content_replace":
		return version.SourceVersionID == nil
	case "content_revert":
		return version.SourceVersionID != nil && validUUIDv4(*version.SourceVersionID)
	default:
		return false
	}
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
		return nil, integrityErrorf("%s returned invalid %s %q",
			identity, api.BlobSizeHeader, resp.Header.Get(api.BlobSizeHeader))
	}
	hash := resp.Header.Get(api.BlobHashHeader)
	if !validSHA256Hex(hash) {
		_ = resp.Body.Close()
		return nil, integrityErrorf("%s returned invalid %s %q", identity, api.BlobHashHeader, hash)
	}
	versionID := resp.Header.Get(api.ContentVersionHeader)
	if !validUUIDv4(versionID) {
		_ = resp.Body.Close()
		return nil, integrityErrorf("%s returned invalid %s %q",
			identity, api.ContentVersionHeader, versionID)
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

// IsCanonicalUUIDv4 reports whether value is a lowercase RFC 4122 UUIDv4.
// CLI selector dispatch uses the same rule as client request validation so a
// durable identity can never be reinterpreted as a display name.
func IsCanonicalUUIDv4(value string) bool {
	return validUUIDv4(value)
}

func (c *Client) VerifyNodeContent(ctx context.Context, id, revision int64) (api.ContentVerification, error) {
	var report api.ContentVerification
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/verify", id),
		ifMatch(revision), nil, &report)
	return report, err
}

// SearchOptions narrows ranked search by stable tag, current media type, one
// live directory's descendants, and an optional half-open modification-time
// interval.
type SearchOptions struct {
	TagID          string
	MIMEType       string
	UnderNodeID    int64
	ModifiedSince  string
	ModifiedBefore string
}

func (c *Client) Search(ctx context.Context, query string, limit int) (api.SearchReport, error) {
	return c.SearchWithOptions(ctx, query, limit, SearchOptions{})
}

// SearchWithOptions returns one bounded ranked result set.
func (c *Client) SearchWithOptions(
	ctx context.Context, query string, limit int, opts SearchOptions,
) (api.SearchReport, error) {
	var out api.SearchReport
	if opts.TagID != "" && !validUUIDv4(opts.TagID) {
		return out, errors.New("search tag ID must be a canonical UUIDv4")
	}
	mimeType, err := store.NormalizeSearchMIMEType(opts.MIMEType)
	if err != nil {
		return out, err
	}
	if opts.UnderNodeID < 0 {
		return out, errors.New("search directory node ID must be positive")
	}
	modifiedSince, modifiedBefore, err := store.NormalizeSearchTimeBounds(
		opts.ModifiedSince, opts.ModifiedBefore,
	)
	if err != nil {
		return out, err
	}
	queryValues := url.Values{}
	queryValues.Set("q", query)
	queryValues.Set("limit", strconv.Itoa(limit))
	if opts.TagID != "" {
		queryValues.Set("tag_id", opts.TagID)
	}
	if mimeType != "" {
		queryValues.Set("mime_type", mimeType)
	}
	if opts.UnderNodeID != 0 {
		queryValues.Set("under_node_id", strconv.FormatInt(opts.UnderNodeID, 10))
	}
	if modifiedSince != "" {
		queryValues.Set("modified_since", modifiedSince)
	}
	if modifiedBefore != "" {
		queryValues.Set("modified_before", modifiedBefore)
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/search?"+queryValues.Encode(), nil, nil, &out); err != nil {
		return out, err
	}
	if out.TagID != opts.TagID || out.MIMEType != mimeType ||
		out.UnderNodeID != opts.UnderNodeID || out.ModifiedSince != modifiedSince ||
		out.ModifiedBefore != modifiedBefore {
		return api.SearchReport{}, errors.New("search response has inconsistent filter authority")
	}
	return out, nil
}

// AuditPreviewOptions selects one live directory for permanent first-scope
// enrollment. Exactly one of Path or NodeID must be supplied.
type AuditPreviewOptions struct {
	Path       string
	NodeID     int64
	AgentLabel string
}

// PreviewAudit derives the exact permanent-retention boundary without
// changing the vault and returns a short-lived, daemon-local execution token.
func (c *Client) PreviewAudit(
	ctx context.Context, opts AuditPreviewOptions,
) (api.AuditEnrollmentPreview, error) {
	var preview api.AuditEnrollmentPreview
	if (opts.Path == "") == (opts.NodeID == 0) {
		return preview, errors.New("audit preview requires exactly one path or node ID")
	}
	if opts.Path != "" && !strings.HasPrefix(opts.Path, "/") {
		return preview, errors.New("audit preview path must be absolute")
	}
	if opts.NodeID < 0 {
		return preview, errors.New("audit preview node ID must be positive")
	}
	body := map[string]any{}
	if opts.Path != "" {
		body["path"] = opts.Path
	} else {
		body["node_id"] = opts.NodeID
	}
	if opts.AgentLabel != "" {
		body["agent_label"] = opts.AgentLabel
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/audit/preview", nil, body, &preview)
	if err != nil {
		return preview, err
	}
	if err := validateAuditPreview(preview); err != nil {
		return api.AuditEnrollmentPreview{}, err
	}
	return preview, nil
}

// EnableAudit consumes one preview after the caller has explicitly accepted
// permanent retention. A token is one-use even when execution rolls back.
func (c *Client) EnableAudit(
	ctx context.Context, previewToken string, acknowledgePermanentRetention bool,
) (api.AuditStatus, error) {
	var status api.AuditStatus
	if previewToken == "" {
		return status, errors.New("audit enable requires a preview token")
	}
	if !acknowledgePermanentRetention {
		return status, errors.New("audit enable requires permanent-retention acknowledgment")
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/audit/enable", nil, map[string]any{
		"preview_token":                   previewToken,
		"acknowledge_permanent_retention": acknowledgePermanentRetention,
	}, &status)
	if err != nil {
		return status, err
	}
	if err := validateAuditStatus(status); err != nil {
		return api.AuditStatus{}, err
	}
	if !status.Enabled {
		return api.AuditStatus{}, errors.New("audit enable response reports dormant authority")
	}
	return status, nil
}

// AuditStatus returns vault-wide authority and, when selected, one node's
// sticky protection bindings. At most one of path and nodeID may be supplied.
func (c *Client) AuditStatus(
	ctx context.Context, path string, nodeID int64,
) (api.AuditStatus, error) {
	var status api.AuditStatus
	if path != "" && nodeID != 0 {
		return status, errors.New("audit status accepts at most one path or node ID")
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		return status, errors.New("audit status path must be absolute")
	}
	if nodeID < 0 {
		return status, errors.New("audit status node ID must be positive")
	}
	query := url.Values{}
	if path != "" {
		query.Set("path", path)
	}
	if nodeID != 0 {
		query.Set("node_id", strconv.FormatInt(nodeID, 10))
	}
	requestPath := "/api/v1/audit/status"
	if encoded := query.Encode(); encoded != "" {
		requestPath += "?" + encoded
	}
	if err := c.do(ctx, http.MethodGet, requestPath, nil, nil, &status); err != nil {
		return status, err
	}
	if err := validateAuditStatus(status); err != nil {
		return api.AuditStatus{}, err
	}
	return status, nil
}

// VerifyAudit independently replays audit authority and verifies every unique
// blob retained by protected history. When expected is non-nil, the report
// also proves whether current authority extends that exact recorded prefix.
func (c *Client) VerifyAudit(
	ctx context.Context, expected *api.AuditEvidence,
) (api.AuditVerifyReport, error) {
	var report api.AuditVerifyReport
	if expected != nil {
		if err := ValidateAuditEvidence(*expected); err != nil {
			return report, fmt.Errorf("invalid expected audit evidence: %w", err)
		}
	}
	request := api.AuditVerifyRequest{Expected: expected}
	if err := c.do(ctx, http.MethodPost, "/api/v1/audit/verify", nil, request, &report); err != nil {
		return report, err
	}
	if err := validateAuditVerifyReport(report); err != nil {
		return api.AuditVerifyReport{}, err
	}
	if len(report.MetadataProblems) == 0 &&
		(expected != nil) != (report.EvidenceCheck != nil) {
		return api.AuditVerifyReport{}, errors.New(
			"audit verification response omitted or invented the expected-evidence check",
		)
	}
	return report, nil
}

// AuditHistory returns one stable newest-first page of canonical events for
// exactly one audited node selected by live path or stable ID.
func (c *Client) AuditHistory(
	ctx context.Context, path string, nodeID int64, limit int, cursor string,
) (api.AuditEventPage, error) {
	var page api.AuditEventPage
	if (path == "") == (nodeID == 0) {
		return page, errors.New("audit history requires exactly one path or node ID")
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		return page, errors.New("audit history path must be absolute")
	}
	if nodeID < 0 {
		return page, errors.New("audit history node ID must be positive")
	}
	if limit < 1 || limit > 500 {
		return page, errors.New("audit history limit must be between 1 and 500")
	}
	if nodeID != 0 {
		if err := store.ValidateAuditHistoryCursor(cursor, nodeID); err != nil {
			return page, err
		}
	}
	query := url.Values{}
	if path != "" {
		query.Set("path", path)
	} else {
		query.Set("node_id", strconv.FormatInt(nodeID, 10))
	}
	query.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/audit/history?"+query.Encode(),
		nil, nil, &page); err != nil {
		return page, err
	}
	if err := validateAuditEventPage(page, nodeID, limit, cursor); err != nil {
		return api.AuditEventPage{}, err
	}
	return page, nil
}

// AuditScopeHistory returns one stable newest-first page across every member
// of one permanent audit scope.
func (c *Client) AuditScopeHistory(
	ctx context.Context, scopeID string, limit int, cursor string,
) (api.AuditScopeEventPage, error) {
	var page api.AuditScopeEventPage
	if !validUUIDv4(scopeID) {
		return page, errors.New("audit scope history requires a canonical UUIDv4 scope ID")
	}
	if limit < 1 || limit > 500 {
		return page, errors.New("audit scope history limit must be between 1 and 500")
	}
	if err := store.ValidateAuditScopeHistoryCursor(cursor, scopeID); err != nil {
		return page, err
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	requestPath := "/api/v1/audit/scopes/" + url.PathEscape(scopeID) +
		"/history?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, requestPath, nil, nil, &page); err != nil {
		return page, err
	}
	if err := validateAuditScopeEventPage(page, scopeID, limit, cursor); err != nil {
		return api.AuditScopeEventPage{}, err
	}
	return page, nil
}

func validateAuditEventPage(page api.AuditEventPage, requestedNodeID int64, limit int, cursor string) error {
	if page.Node.ID < 1 || (requestedNodeID != 0 && page.Node.ID != requestedNodeID) ||
		page.Limit != limit || page.Cursor != cursor || page.Total < len(page.Items) ||
		len(page.Items) > limit || (page.Node.TrashedAt == "") != strings.HasPrefix(page.Path, "/") {
		return errors.New("audit history response has inconsistent node or pagination")
	}
	if err := store.ValidateAuditHistoryCursor(cursor, page.Node.ID); err != nil {
		return err
	}
	if err := store.ValidateAuditHistoryCursor(page.NextCursor, page.Node.ID); err != nil {
		return err
	}
	if page.NextCursor != "" && len(page.Items) != limit {
		return errors.New("audit history response has a premature next cursor")
	}
	for i, event := range page.Items {
		if err := validateAuditEvent(event, page.Node.ID); err != nil {
			return fmt.Errorf("audit history item %d: %w", i, err)
		}
		if i > 0 && !auditEventPrecedes(page.Items[i-1], event) {
			return errors.New("audit history response is not newest first")
		}
	}
	return nil
}

func validateAuditScopeEventPage(
	page api.AuditScopeEventPage, scopeID string, limit int, cursor string,
) error {
	if page.Scope.ID != scopeID || page.Limit != limit || page.Cursor != cursor ||
		page.Total < len(page.Items) || len(page.Items) > limit {
		return errors.New("audit scope history response has inconsistent scope or pagination")
	}
	if err := validateAuditScopeStatus(page.Scope); err != nil {
		return fmt.Errorf("audit scope history response: %w", err)
	}
	if err := store.ValidateAuditScopeHistoryCursor(cursor, scopeID); err != nil {
		return err
	}
	if err := store.ValidateAuditScopeHistoryCursor(page.NextCursor, scopeID); err != nil {
		return err
	}
	if page.NextCursor != "" && len(page.Items) != limit {
		return errors.New("audit scope history response has a premature next cursor")
	}
	for i, event := range page.Items {
		if event.ScopeID != scopeID {
			return fmt.Errorf("audit scope history item %d belongs to another scope", i)
		}
		if err := validateAuditEvent(event, event.NodeID); err != nil {
			return fmt.Errorf("audit scope history item %d: %w", i, err)
		}
		if i > 0 && !auditEventPrecedes(page.Items[i-1], event) {
			return errors.New("audit scope history response is not newest first")
		}
	}
	return nil
}

func validateAuditEvent(event api.AuditEvent, nodeID int64) error {
	recordedAt, timeErr := time.Parse(time.RFC3339Nano, event.RecordedAt)
	if !validSHA256Hex(event.ID) || !validUUIDv4(event.OperationID) ||
		event.OperationSequence < 1 || event.Ordinal < 0 || event.NodeID < 1 || event.NodeID != nodeID ||
		event.Kind == "" || !validUUIDv4(event.ScopeID) || event.Origin == "" ||
		timeErr != nil || recordedAt.Location() != time.UTC ||
		event.PriorNodeRevision < 0 || event.ResultingNodeRevision < 0 ||
		!validOptionalUUID(event.PriorCurrentVersionID) ||
		!validOptionalUUID(event.ResultingCurrentVersionID) ||
		!validOptionalUUID(event.SourceVersionID) ||
		(event.TargetNodeID != nil && *event.TargetNodeID < 1) ||
		(event.BaselineDigest != nil && !validSHA256Hex(*event.BaselineDigest)) {
		return errors.New("event has invalid canonical fields")
	}
	if event.Kind == "node_path" {
		if event.OldPath == nil || event.NewPath == nil {
			return errors.New("path event lacks old and new path states")
		}
		if err := store.ValidateAuditPathState(event.OldPath.Path, event.OldPath.State); err != nil {
			return fmt.Errorf("invalid old path state: %w", err)
		}
		if err := store.ValidateAuditPathState(event.NewPath.Path, event.NewPath.State); err != nil {
			return fmt.Errorf("invalid new path state: %w", err)
		}
	} else if event.OldPath != nil || event.NewPath != nil {
		return errors.New("non-path event contains path states")
	}
	return validateAuditAttachment(event.Kind, event.Attachment)
}

func validateAuditAttachment(eventKind string, change *api.AuditAttachmentChange) error {
	wantKind := ""
	switch eventKind {
	case "tag_define", "tag_rename", "tag_delete":
		wantKind = "tag_definition"
	case "tag_assign", "tag_unassign":
		wantKind = "tag_assignment"
	case "provenance_add", "provenance_supersede":
		wantKind = "provenance"
	}
	if wantKind == "" {
		if change != nil {
			return errors.New("event kind cannot carry an attachment")
		}
		return nil
	}
	if change == nil || change.Kind != wantKind ||
		(change.Before == nil && change.After == nil) {
		return errors.New("attachment event lacks a complete typed change")
	}
	if err := validateAuditAttachmentIdentity(change.Kind, change.Identity); err != nil {
		return err
	}
	for _, state := range []*api.AuditAttachmentState{change.Before, change.After} {
		if state == nil {
			continue
		}
		if err := validateAuditAttachmentState(change.Kind, change.Identity, *state); err != nil {
			return err
		}
	}
	switch eventKind {
	case "tag_rename":
		if change.Before != nil && change.After != nil {
			return nil
		}
	case "tag_delete", "tag_unassign":
		if change.Before != nil && change.After == nil {
			return nil
		}
	case "tag_define", "tag_assign", "provenance_add":
		if change.Before == nil && change.After != nil &&
			(eventKind != "provenance_add" ||
				change.After.ProvenanceID == change.Identity.ProvenanceID) {
			return nil
		}
	case "provenance_supersede":
		if change.Before != nil && change.After != nil &&
			change.After.ProvenanceID == change.Identity.ProvenanceID &&
			change.After.Supersedes != nil &&
			*change.After.Supersedes == change.Before.ProvenanceID {
			return nil
		}
	}
	return errors.New("audit attachment has an invalid transition shape")
}

func validateAuditAttachmentIdentity(kind string, identity api.AuditAttachmentIdentity) error {
	switch kind {
	case "tag_definition":
		if validUUIDv4(identity.TagID) && identity.NodeID == 0 && identity.ProvenanceID == "" {
			return nil
		}
	case "tag_assignment":
		if validUUIDv4(identity.TagID) && identity.NodeID > 0 && identity.ProvenanceID == "" {
			return nil
		}
	case "provenance":
		if identity.TagID == "" && identity.NodeID == 0 && validSHA256Hex(identity.ProvenanceID) {
			return nil
		}
	}
	return errors.New("audit attachment has an invalid stable identity")
}

func validateAuditAttachmentState(
	kind string, identity api.AuditAttachmentIdentity, state api.AuditAttachmentState,
) error {
	switch kind {
	case "tag_definition":
		if state.TagID == identity.TagID && state.TagName != "" && state.NodeID == 0 &&
			state.ProvenanceID == "" && state.IngestID == "" && state.OriginalPath == nil &&
			state.OriginalMTime == nil && state.Supersedes == nil {
			return nil
		}
	case "tag_assignment":
		if state.TagID == identity.TagID && state.NodeID == identity.NodeID &&
			state.TagName == "" && state.ProvenanceID == "" && state.IngestID == "" &&
			state.OriginalPath == nil && state.OriginalMTime == nil && state.Supersedes == nil {
			return nil
		}
	case "provenance":
		mtimeValid := true
		if state.OriginalMTime != nil {
			parsed, err := time.Parse(time.RFC3339Nano, *state.OriginalMTime)
			mtimeValid = err == nil && parsed.Location() == time.UTC
		}
		supersedesValid := state.Supersedes == nil || validSHA256Hex(*state.Supersedes)
		if state.TagID == "" && state.TagName == "" && state.NodeID > 0 &&
			validSHA256Hex(state.ProvenanceID) && validUUIDv4(state.IngestID) &&
			mtimeValid && supersedesValid {
			return nil
		}
	}
	return errors.New("audit attachment has an invalid before or after state")
}

func validOptionalUUID(value *string) bool {
	return value == nil || validUUIDv4(*value)
}

func auditEventPrecedes(newer, older api.AuditEvent) bool {
	return newer.OperationSequence > older.OperationSequence ||
		(newer.OperationSequence == older.OperationSequence && newer.Ordinal > older.Ordinal) ||
		(newer.OperationSequence == older.OperationSequence && newer.Ordinal == older.Ordinal &&
			newer.ID > older.ID)
}

func validateAuditPreview(preview api.AuditEnrollmentPreview) error {
	secret, tokenErr := base64.RawURLEncoding.Strict().DecodeString(preview.PreviewToken)
	expiresAt, timeErr := time.Parse(time.RFC3339Nano, preview.ExpiresAt)
	if !validUUIDv4(preview.VaultID) || !validUUIDv4(preview.ScopeID) ||
		!validUUIDv4(preview.OperationID) || preview.TargetNodeID < 1 ||
		!strings.HasPrefix(preview.TargetPath, "/") || !validSHA256Hex(preview.BaselineDigest) ||
		preview.MemberCount < 1 || preview.FileCount < 0 || preview.DirectoryCount < 1 ||
		preview.FileCount+preview.DirectoryCount != preview.MemberCount ||
		preview.VersionCount < 0 || preview.LogicalVersionBytes < 0 ||
		preview.VersionCount < preview.FileCount ||
		preview.UniqueBlobs < 0 || preview.UniqueBlobs > preview.VersionCount ||
		preview.UniqueBlobBytes < 0 || preview.UniqueBlobBytes > preview.LogicalVersionBytes ||
		preview.UnresolvedTrashOrigins < 0 || preview.UnresolvedTrashOrigins > preview.MemberCount ||
		preview.VaultTopologyNodes < preview.MemberCount || preview.VaultAttachmentRecords < 0 ||
		preview.AuthorityJSONBytes < 1 || tokenErr != nil || len(secret) != 32 ||
		timeErr != nil || expiresAt.Location() != time.UTC {
		return errors.New("audit preview response has inconsistent authority or inventory")
	}
	return nil
}

func validateAuditStatus(status api.AuditStatus) error {
	if !validUUIDv4(status.VaultID) || status.OperationSequenceHighWater < 0 ||
		status.AllocationEntryCount < 0 {
		return errors.New("audit status has invalid vault identity or counters")
	}
	if !status.Enabled {
		if status.LineageID != "" || status.OperationSequenceHighWater != 0 ||
			status.AllocationEntryCount != 0 || status.AllocationHead != "" || len(status.Scopes) != 0 {
			return errors.New("dormant audit status contains active authority")
		}
	} else if !validUUIDv4(status.LineageID) || status.OperationSequenceHighWater < 1 ||
		status.AllocationEntryCount < 1 || !validSHA256Hex(status.AllocationHead) ||
		len(status.Scopes) == 0 {
		return errors.New("active audit status lacks complete authority")
	}
	type scopeMembershipAuthority struct {
		targetNodeID   int64
		baselineDigest string
	}
	seen := make(map[string]scopeMembershipAuthority, len(status.Scopes))
	previousScopeID := ""
	for index, scope := range status.Scopes {
		_, duplicate := seen[scope.ID]
		if duplicate || (previousScopeID != "" && scope.ID <= previousScopeID) ||
			validateAuditScopeStatus(scope) != nil {
			return fmt.Errorf("audit status scope %d has invalid authority", index)
		}
		seen[scope.ID] = scopeMembershipAuthority{
			targetNodeID: scope.TargetNodeID, baselineDigest: scope.BaselineDigest,
		}
		previousScopeID = scope.ID
	}
	if status.Membership != nil {
		member := status.Membership
		if member.NodeID < 1 || (!member.Trashed && !strings.HasPrefix(member.Path, "/")) ||
			(member.Trashed && member.Path != "") || member.Protected != (len(member.ScopeIDs) != 0) ||
			(member.Protected && !status.Enabled) ||
			len(member.ScopeIDs) != len(member.BaselineDigests) {
			return errors.New("audit status has invalid node membership")
		}
		previousScopeID = ""
		for index, scopeID := range member.ScopeIDs {
			digest := member.BaselineDigests[index]
			scope, knownScope := seen[scopeID]
			if !validUUIDv4(scopeID) || !validSHA256Hex(digest) ||
				!knownScope ||
				(member.NodeID == scope.targetNodeID && digest != scope.baselineDigest) ||
				(previousScopeID != "" && scopeID <= previousScopeID) {
				return errors.New("audit status has invalid node membership binding")
			}
			previousScopeID = scopeID
		}
	}
	return nil
}

func validateAuditScopeStatus(scope api.AuditScopeStatus) error {
	if !validUUIDv4(scope.ID) || scope.TargetNodeID < 1 ||
		(!scope.TargetTrashed && !strings.HasPrefix(scope.TargetPath, "/")) ||
		(scope.TargetTrashed && scope.TargetPath != "") ||
		!validUUIDv4(scope.EnableOperationID) || !validSHA256Hex(scope.BaselineDigest) ||
		scope.MemberCount < 1 || scope.EntryCount < 1 || !validSHA256Hex(scope.ChainHead) {
		return errors.New("audit scope has invalid authority")
	}
	return nil
}

func validateAuditVerifyReport(report api.AuditVerifyReport) error {
	if report.ProtectedBlobs < 0 || report.ProtectedBytes < 0 || report.VerifiedBlobs < 0 ||
		report.VerifiedBlobs+len(report.Problems) != report.ProtectedBlobs {
		return errors.New("audit verification has inconsistent blob totals")
	}
	for index, problem := range report.MetadataProblems {
		if problem == "" {
			return fmt.Errorf("audit verification metadata problem %d is empty", index)
		}
	}
	previousHash := ""
	for index, problem := range report.Problems {
		if !validSHA256Hex(problem.Hash) ||
			(problem.Problem != "missing" && problem.Problem != "corrupt" &&
				problem.Problem != "unreadable") ||
			(previousHash != "" && problem.Hash <= previousHash) {
			return fmt.Errorf("audit verification blob problem %d is invalid", index)
		}
		previousHash = problem.Hash
	}
	if report.EvidenceCheck != nil {
		if err := validateAuditEvidenceCheck(*report.EvidenceCheck); err != nil {
			return err
		}
	}
	if len(report.MetadataProblems) != 0 {
		if report.Enabled || report.Evidence != nil || report.ProtectedBlobs != 0 ||
			report.ProtectedBytes != 0 || report.VerifiedBlobs != 0 || len(report.Problems) != 0 ||
			report.EvidenceCheck != nil {
			return errors.New("failed audit metadata verification contains trusted evidence")
		}
		return nil
	}
	if !report.Enabled {
		if report.Evidence != nil || report.ProtectedBlobs != 0 || report.ProtectedBytes != 0 {
			return errors.New("dormant audit verification contains active evidence")
		}
		return nil
	}
	if report.Evidence == nil {
		return errors.New("active audit verification lacks terminal evidence")
	}
	return ValidateAuditEvidence(*report.Evidence)
}

// ValidateAuditEvidence validates one externally recordable terminal bundle.
func ValidateAuditEvidence(evidence api.AuditEvidence) error {
	if !validUUIDv4(evidence.VaultID) || !validUUIDv4(evidence.LineageID) ||
		evidence.OperationSequenceHighWater < 1 ||
		evidence.AllocationEntryCount != evidence.OperationSequenceHighWater ||
		!validSHA256Hex(evidence.AllocationHead) || len(evidence.Scopes) == 0 ||
		len(evidence.Scopes) > store.MaxAuditEvidenceScopes {
		return errors.New("audit evidence lacks complete allocation authority")
	}
	previousScopeID := ""
	for index, scope := range evidence.Scopes {
		if !validUUIDv4(scope.ID) || scope.EntryCount < 1 ||
			scope.EntryCount > evidence.AllocationEntryCount || !validSHA256Hex(scope.ChainHead) ||
			(previousScopeID != "" && scope.ID <= previousScopeID) {
			return fmt.Errorf("audit evidence scope %d is invalid", index)
		}
		previousScopeID = scope.ID
	}
	return nil
}

func validateAuditEvidenceCheck(check api.AuditEvidenceCheck) error {
	if check.Extends != (len(check.Problems) == 0) {
		return errors.New("audit evidence check has an inconsistent result")
	}
	for index, problem := range check.Problems {
		scopeProblem := problem.Code == "scope_missing" || problem.Code == "scope_shorter" ||
			problem.Code == "scope_diverged"
		validCode := problem.Code == "audit_not_enabled" || problem.Code == "vault_mismatch" ||
			problem.Code == "lineage_mismatch" || problem.Code == "allocation_shorter" ||
			problem.Code == "allocation_diverged" || scopeProblem
		if !validCode || problem.Message == "" ||
			(scopeProblem && !validUUIDv4(problem.ScopeID)) ||
			(!scopeProblem && problem.ScopeID != "") {
			return fmt.Errorf("audit evidence problem %d is invalid", index)
		}
	}
	return nil
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
	headers, err := c.doWithHeaders(
		ctx, http.MethodGet, "/api/v1/tags/"+url.PathEscape(id), nil, nil, &tag,
	)
	if err != nil {
		return tag, err
	}
	if err := validateTagResponse(tag, headers.Get("ETag")); err != nil {
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
	headers, err := c.doWithHeaders(ctx, http.MethodGet, path, nil, nil, &tag)
	if err != nil {
		return tag, err
	}
	if err := validateTagResponse(tag, headers.Get("ETag")); err != nil {
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
	headers, err := c.doWithHeaders(ctx, http.MethodPost, "/api/v1/tags", nil,
		map[string]string{"name": normalized}, &tag)
	if err != nil {
		return tag, err
	}
	if err := validateTagResponse(tag, headers.Get("ETag")); err != nil {
		return api.Tag{}, err
	}
	if tag.Name != normalized || tag.Revision != 1 || tag.AssignmentCount != 0 {
		return api.Tag{}, errors.New("created tag response has inconsistent authority")
	}
	return tag, nil
}

// RenameTag changes a tag's display name without changing its stable ID.
func (c *Client) RenameTag(ctx context.Context, id string, revision int64, name string) (api.Tag, error) {
	var tag api.Tag
	if !validUUIDv4(id) {
		return tag, errors.New("tag ID must be a canonical UUIDv4")
	}
	if revision < 1 {
		return tag, errors.New("tag revision must be positive")
	}
	normalized, err := store.NormalizeTagName(name)
	if err != nil {
		return tag, err
	}
	headers, err := c.doWithHeaders(ctx, http.MethodPatch, "/api/v1/tags/"+url.PathEscape(id),
		ifMatch(revision), map[string]string{"name": normalized}, &tag)
	if err != nil {
		return tag, err
	}
	if err := validateTagResponse(tag, headers.Get("ETag")); err != nil {
		return api.Tag{}, err
	}
	if tag.ID != id || tag.Name != normalized {
		return api.Tag{}, errors.New("renamed tag response has inconsistent authority")
	}
	return tag, nil
}

// DeleteTag removes one tag definition and its complete assignment set.
func (c *Client) DeleteTag(
	ctx context.Context, id string, revision int64,
) (api.TagDeletionReceipt, error) {
	var receipt api.TagDeletionReceipt
	if !validUUIDv4(id) {
		return receipt, errors.New("tag ID must be a canonical UUIDv4")
	}
	if revision < 1 {
		return receipt, errors.New("tag revision must be positive")
	}
	headers, err := c.doWithHeaders(ctx, http.MethodDelete, "/api/v1/tags/"+url.PathEscape(id),
		ifMatch(revision), nil, &receipt)
	if err != nil {
		return receipt, err
	}
	if err := validateTagResponse(receipt.Tag, headers.Get("ETag")); err != nil {
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
	if revision < 1 {
		return receipt, errors.New("tagged node revision must be positive")
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
	// The ID-addressed assignment contract advances the node exactly once for
	// a real change and not at all for idempotent convergence. Treat divergence
	// as incompatible response authority; a future policy change must update
	// both protocol sides rather than silently weakening this check.
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
	responseHeaders, err := c.doWithHeaders(ctx, method, path, headers, in, &receipt)
	if err != nil {
		return receipt, "", err
	}
	return receipt, responseHeaders.Get("ETag"), nil
}

func validateTag(tag api.Tag) error {
	if !validUUIDv4(tag.ID) || tag.Revision < 1 || tag.AssignmentCount < 0 {
		return errors.New("tag response has invalid identity, revision, or assignment count")
	}
	normalized, err := store.NormalizeTagName(tag.Name)
	if err != nil || normalized != tag.Name {
		return errors.New("tag response has invalid or non-canonical name")
	}
	return nil
}

func validateTagResponse(tag api.Tag, etag string) error {
	if err := validateTag(tag); err != nil {
		return err
	}
	want := fmt.Sprintf("%q", strconv.FormatInt(tag.Revision, 10))
	if etag != want {
		return fmt.Errorf("tag response ETag %q, expected %q", etag, want)
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

func validateProvenancePage(page api.ProvenancePage, nodeID int64, limit, offset int) error {
	if page.Node.ID != nodeID || page.Node.Kind != "file" || page.Node.Revision < 1 ||
		(page.Node.TrashedAt == "" && !strings.HasPrefix(page.Node.Path, "/")) ||
		(page.Node.TrashedAt != "" && page.Node.Path != "") {
		return errors.New("provenance response has inconsistent node authority")
	}
	if limit < 1 || limit > store.MaxProvenancePageSize || offset < 0 ||
		page.Limit != limit || page.Offset != offset || page.Total < 0 || len(page.Items) > limit ||
		(len(page.Items) == 0 && offset < page.Total) ||
		(len(page.Items) > 0 && offset+len(page.Items) > page.Total) {
		return errors.New("provenance response has inconsistent pagination")
	}
	seen := make(map[string]struct{}, len(page.Items))
	for i, fact := range page.Items {
		startedAt, startedErr := time.Parse(time.RFC3339Nano, fact.IngestStartedAt)
		mtimeValid := true
		if fact.OriginalMTime != nil {
			parsed, err := time.Parse(time.RFC3339Nano, *fact.OriginalMTime)
			mtimeValid = err == nil && parsed.Location() == time.UTC
		}
		if !validSHA256Hex(fact.Identity) || fact.NodeID != nodeID ||
			!validUUIDv4(fact.IngestID) || startedErr != nil || startedAt.Location() != time.UTC ||
			fact.SourceKind == "" || fact.SourceDescription == "" || fact.OriginalPath == "" ||
			!mtimeValid || (fact.Supersedes != nil && !validSHA256Hex(*fact.Supersedes)) {
			return fmt.Errorf("provenance response item %d has invalid authority", i)
		}
		if _, duplicate := seen[fact.Identity]; duplicate {
			return fmt.Errorf("provenance response repeats identity %s", fact.Identity)
		}
		seen[fact.Identity] = struct{}{}
		if i > 0 {
			prior := page.Items[i-1]
			priorTime, _ := time.Parse(time.RFC3339Nano, prior.IngestStartedAt)
			if priorTime.Before(startedAt) ||
				(priorTime.Equal(startedAt) && prior.Identity <= fact.Identity) {
				return errors.New("provenance response is not newest-ingest-first")
			}
		}
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

// MkdirPath creates one directory at an exact absolute virtual coordinate.
// The daemon resolves the existing parent and performs the creation in one
// transaction.
func (c *Client) MkdirPath(ctx context.Context, path string) (api.Node, error) {
	canonical, err := canonicalDirectoryPath(path)
	if err != nil {
		return api.Node{}, err
	}
	var node api.Node
	headers, err := c.doWithHeaders(ctx, http.MethodPost, "/api/v1/path/mkdir", nil,
		map[string]string{"path": path}, &node)
	if err != nil {
		return api.Node{}, err
	}
	expectedName := canonical[strings.LastIndex(canonical, "/")+1:]
	if node.ID < 1 || node.ParentID == nil || node.Kind != "dir" || node.Name == "" ||
		node.Name != expectedName || node.Path != canonical || node.Revision != 1 || node.TrashedAt != "" ||
		node.CurrentVersionID != "" || node.BlobHash != "" || node.Size != 0 || node.MimeType != "" {
		return api.Node{}, errors.New("mkdir response has inconsistent directory authority")
	}
	wantETag := fmt.Sprintf("%q", strconv.FormatInt(node.Revision, 10))
	if headers.Get("ETag") != wantETag {
		return api.Node{}, fmt.Errorf("mkdir response ETag %q, expected %q", headers.Get("ETag"), wantETag)
	}
	return node, nil
}

func canonicalDirectoryPath(path string) (string, error) {
	if !utf8.ValidString(path) {
		return "", errors.New("directory path is not valid UTF-8")
	}
	if !strings.HasPrefix(path, "/") {
		return "", errors.New("directory path must be absolute")
	}
	segments := make([]string, 0)
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "" {
			continue
		}
		normalized, err := store.NormalizeName(segment)
		if err != nil {
			return "", fmt.Errorf("directory path %q: %w", path, err)
		}
		segments = append(segments, normalized)
	}
	if len(segments) == 0 {
		return "", errors.New("directory path must not be the vault root")
	}
	return "/" + strings.Join(segments, "/"), nil
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

// BatchMove applies one bounded all-or-nothing reorganization. Path sources
// are resolved by the daemon inside the transaction; ID sources require the
// caller's inspected revision.
func (c *Client) BatchMove(
	ctx context.Context, moves []api.BatchMoveItem,
) (api.BatchMoveReport, error) {
	var report api.BatchMoveReport
	if len(moves) == 0 || len(moves) > store.MaxBatchMoves {
		return report, fmt.Errorf("batch move requires 1-%d items: %w",
			store.MaxBatchMoves, store.ErrInvalidBatchMove)
	}
	for index, move := range moves {
		byPath, byID := move.SourcePath != "", move.NodeID != 0
		if byPath == byID {
			return report, fmt.Errorf("batch move item %d source must use exactly one of path or node ID: %w",
				index, store.ErrInvalidBatchMove)
		}
		if byPath {
			if !utf8.ValidString(move.SourcePath) || !strings.HasPrefix(move.SourcePath, "/") || move.Revision != 0 {
				return report, fmt.Errorf("batch move item %d has an invalid path source: %w",
					index, store.ErrInvalidBatchMove)
			}
		} else if move.NodeID < 1 || move.Revision < 1 {
			return report, fmt.Errorf("batch move item %d requires a positive node ID and revision: %w",
				index, store.ErrInvalidBatchMove)
		}
		if !utf8.ValidString(move.DestinationPath) || !strings.HasPrefix(move.DestinationPath, "/") {
			return report, fmt.Errorf("batch move item %d destination must be an absolute UTF-8 path: %w",
				index, store.ErrInvalidBatchMove)
		}
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/batch/move", nil,
		api.BatchMoveRequest{Moves: moves}, &report); err != nil {
		return api.BatchMoveReport{}, err
	}
	if len(report.Items) != len(moves) {
		return api.BatchMoveReport{}, fmt.Errorf(
			"batch move receipt has %d items, expected %d", len(report.Items), len(moves))
	}
	for index, receipt := range report.Items {
		if receipt.Node.ID < 1 || receipt.Node.Revision < 1 ||
			receipt.FromPath == "" || receipt.Node.Path == "" {
			return api.BatchMoveReport{}, fmt.Errorf("batch move receipt item %d is incomplete", index)
		}
		if moves[index].NodeID != 0 && receipt.Node.ID != moves[index].NodeID {
			return api.BatchMoveReport{}, fmt.Errorf(
				"batch move receipt item %d targets node %d, expected %d",
				index, receipt.Node.ID, moves[index].NodeID)
		}
	}
	return report, nil
}

// MoveToPath moves a stable node identity to an absolute virtual destination
// under an optimistic revision precondition.
func (c *Client) MoveToPath(
	ctx context.Context, id, rev int64, destPath string,
) (api.Node, error) {
	var n api.Node
	if id < 1 {
		return n, errors.New("move node ID must be positive")
	}
	if rev < 1 {
		return n, errors.New("move revision must be positive")
	}
	if !strings.HasPrefix(destPath, "/") {
		return n, errors.New("move destination path must be absolute")
	}
	err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", id),
		ifMatch(rev), map[string]any{"dest_path": destPath}, &n)
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

// Info identifies the selected vault and summarizes its logical and physical contents.
func (c *Client) Info(ctx context.Context) (api.VaultInfo, error) {
	var info api.VaultInfo
	err := c.do(ctx, http.MethodGet, "/api/v1/info", nil, nil, &info)
	return info, err
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
