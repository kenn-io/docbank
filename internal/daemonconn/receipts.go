// Package daemonconn owns daemon connections and validates receipts returned
// by the generated API client. Endpoint definitions live in internal/apiclient.
package daemonconn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/docbank/internal/apiclient"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

type Connection struct {
	base string
	key  string
	hc   *http.Client
}

// WebSessionURL asks the ownership-proven daemon for a daemon-lifetime,
// scoped browser credential and a daemon-lifetime upload proof secret, then
// returns both in the local portal fragment. The master API key stays on the
// client's pinned connection and never enters the browser.
func (c *Connection) WebSessionURL(ctx context.Context) (string, error) {
	apiURL, err := url.Parse(c.base)
	if err != nil {
		return "", fmt.Errorf("parsing daemon web URL: %w", err)
	}
	host := apiURL.Hostname()
	ip := net.ParseIP(host)
	if apiURL.Scheme != "http" || apiURL.Host == "" || c.key == "" ||
		(!strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback())) {
		return "", errors.New("daemon client cannot produce an authenticated web URL")
	}
	var session struct {
		Token        string `json:"token"`
		UploadSecret string `json:"upload_secret"`
		URL          string `json:"url"`
	}
	apiResponse, err := c.API().CreateWebSession(ctx)
	if err != nil {
		return "", err
	}
	session = *apiResponse
	if session.Token == "" {
		return "", errors.New("daemon returned an empty browser session")
	}
	uploadSecret, err := base64.RawURLEncoding.DecodeString(session.UploadSecret)
	if err != nil || len(uploadSecret) != sha256.Size {
		return "", errors.New("daemon returned an invalid browser upload secret")
	}
	u, err := url.Parse(session.URL)
	if err != nil {
		return "", fmt.Errorf("parsing daemon browser origin: %w", err)
	}
	if u.Scheme != "http" || u.Host == "" || u.User != nil ||
		u.Path != "/" || u.RawQuery != "" || u.Fragment != "" ||
		!validBrowserOriginAddress(u.Host) {
		return "", errors.New("daemon returned an invalid browser origin")
	}
	if u.Host == apiURL.Host {
		return "", errors.New("daemon returned its reusable API origin for the browser")
	}
	u.Path = "/"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = url.Values{
		"web_session":       {session.Token},
		"web_upload_secret": {session.UploadSecret},
	}.Encode()
	return u.String(), nil
}

func validBrowserOriginAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return false
	}
	const prefix = "docbank-"
	const suffix = ".localhost"
	if !strings.HasPrefix(host, prefix) || !strings.HasSuffix(host, suffix) {
		return false
	}
	identity := strings.TrimSuffix(strings.TrimPrefix(host, prefix), suffix)
	decoded, err := hex.DecodeString(identity)
	return err == nil && len(decoded) == 16 && identity == strings.ToLower(identity)
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

type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// IsTransportError reports whether an HTTP request failed before the daemon
// returned a response. Long-lived clients may use this distinction to
// reconnect without retrying malformed or contract-invalid responses.
func IsTransportError(err error) bool {
	var transport *transportError
	return errors.As(err, &transport)
}

func classifyRequestFailure(resp *http.Response, err error) error {
	if resp == nil {
		return &transportError{err: err}
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	return &responseError{status: resp.StatusCode, err: err}
}

type responseDecodeError struct{ err error }

func (e *responseDecodeError) Error() string { return e.err.Error() }
func (e *responseDecodeError) Unwrap() error { return e.err }

// IsResponseDecodeError reports whether the daemon returned a successful HTTP
// status but the client could not decode the response body. Mutation callers
// must treat this as an unknown outcome because the daemon may have committed
// before the response was truncated or malformed.
func IsResponseDecodeError(err error) bool {
	var decode *responseDecodeError
	_, generated := errors.AsType[*runtime.ResponseDecodeError](err)
	return errors.As(err, &decode) || generated
}

type problemError struct {
	code               string
	observedScopeCount int
	err                error
}

func (e *problemError) Error() string { return e.err.Error() }
func (e *problemError) Unwrap() error { return e.err }

// ProblemCode returns the daemon's stable RFC 7807 extension code when err
// came from a decoded HTTP response or progress-stream error event.
func ProblemCode(err error) (string, bool) {
	facts, ok := ExtractProblemFacts(err)
	if !ok {
		return "", false
	}
	return facts.Code, true
}

// ProblemFacts is the safe, stable subset of a decoded daemon problem.
// MappedError is one package sentinel from the client's problem-code map;
// detailed server text and the original error are never returned.
type ProblemFacts struct {
	Code               string
	MappedError        error
	ObservedScopeCount int
}

// ExtractProblemFacts copies stable problem metadata without exposing the
// daemon's detailed error text to longer-lived protocol boundaries.
func ExtractProblemFacts(err error) (ProblemFacts, bool) {
	var problem *problemError
	if !errors.As(err, &problem) || !validProblemCode(problem.code) {
		return ProblemFacts{}, false
	}
	return ProblemFacts{
		Code: problem.code, MappedError: codeToTypedErr[problem.code],
		ObservedScopeCount: problem.observedScopeCount,
	}, true
}

func validProblemCode(code string) bool {
	if len(code) == 0 || len(code) > 64 || code[0] < 'a' || code[0] > 'z' {
		return false
	}
	for i := 1; i < len(code); i++ {
		b := code[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '_' {
			return false
		}
	}
	return true
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
	if s == nil {
		return 0, errors.New("copying content: nil stream")
	}
	return s.copyVerified(w, s.Size)
}

func (s *ContentStream) copyVerified(w io.Writer, maxBytes int64) (int64, error) {
	if s == nil || s.ReadCloser == nil {
		return 0, errors.New("copying content: nil stream")
	}
	if w == nil {
		return 0, errors.New("copying content: nil destination")
	}
	if s.Size < 0 || maxBytes < 0 || s.Size > maxBytes || s.Size == math.MaxInt64 {
		_ = s.Close()
		return 0, integrityErrorf("verifying content: declared size %d exceeds bounded limit %d",
			s.Size, maxBytes)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(w, hash), io.LimitReader(s, s.Size+1))
	if err != nil {
		_ = s.Close()
		return written, fmt.Errorf("copying content: %w", err)
	}
	if written != s.Size {
		if written > s.Size {
			_ = s.Close()
		}
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

func New(baseURL, apiKey string) *Connection {
	return &Connection{base: baseURL, key: apiKey, hc: &http.Client{Timeout: 0}}
}

// APIKeyExclusionPolicy is an opaque, fixed policy that refuses one API key.
// It retains only a hash of the forbidden value and reveals neither the value
// nor its hash to callers.
type APIKeyExclusionPolicy func(*Connection) bool

// NewAPIKeyExclusionPolicy builds a fixed policy for an independently bound
// credential. The raw forbidden value is not captured by the returned policy.
func NewAPIKeyExclusionPolicy(forbidden string) APIKeyExclusionPolicy {
	forbiddenHash := sha256.Sum256([]byte(forbidden))
	return func(c *Connection) bool {
		if c == nil {
			return false
		}
		credential := c.key
		if credential == "" && c.hc != nil {
			if scoped, ok := c.hc.Transport.(*agentSessionTransport); ok {
				credential = scoped.token
			}
		}
		if credential == "" {
			return false
		}
		keyHash := sha256.Sum256([]byte(credential))
		return subtle.ConstantTimeCompare(forbiddenHash[:], keyHash[:]) != 1
	}
}

// Allows reports whether the client uses a non-empty daemon or agent-session
// credential distinct from the policy's forbidden credential.
func (policy APIKeyExclusionPolicy) Allows(c *Connection) bool {
	return policy != nil && policy(c)
}

// Close releases idle transport connections owned by this client. It is most
// useful to long-running callers that periodically reacquire a daemon client
// after idle shutdown or process replacement.
func (c *Connection) Close() error {
	if c != nil && c.hc != nil && c.hc.Transport != nil {
		if transport, ok := c.hc.Transport.(interface{ CloseIdleConnections() }); ok {
			transport.CloseIdleConnections()
		}
	}
	return nil
}

// codeToTypedErr preserves server problem codes that have a stable local
// sentinel for callers using errors.Is.
var codeToTypedErr = map[string]error{
	"rendition_failed":              store.ErrRenditionJobTerminal,
	"rendition_operator_required":   store.ErrRenditionJobOperatorRequired,
	"search_query_required":         store.ErrSearchQueryRequired,
	"not_found":                     store.ErrNotFound,
	"exists":                        store.ErrExists,
	"cycle":                         store.ErrCycle,
	"stale_revision":                store.ErrStaleRevision,
	"not_dir":                       store.ErrNotDir,
	"not_file":                      store.ErrNotFile,
	"provenance_mismatch":           store.ErrProvenanceMismatch,
	"invalid_provenance_time":       store.ErrInvalidProvenanceTime,
	"invalid_name":                  store.ErrInvalidName,
	"invalid_tag":                   store.ErrInvalidTag,
	"invalid_saved_query":           store.ErrInvalidSavedQuery,
	"invalid_batch_move":            store.ErrInvalidBatchMove,
	"not_trashed":                   store.ErrNotTrashed,
	"is_root":                       store.ErrIsRoot,
	"version_node_mismatch":         store.ErrVersionNodeMismatch,
	"version_already_current":       store.ErrVersionAlreadyCurrent,
	"invalid_version_prune":         store.ErrInvalidVersionPrune,
	"audit_already_enabled":         store.ErrAuditAlreadyEnabled,
	"audit_scope_overlap":           store.ErrAuditScopeOverlap,
	"audit_scope_limit":             store.ErrAuditScopeLimit,
	"audit_preview_stale":           store.ErrAuditPreviewStale,
	"audit_not_enrolled":            store.ErrAuditNotEnrolled,
	"audit_mutation_unsupported":    store.ErrAuditMutationUnsupported,
	"invalid_audit_cursor":          store.ErrInvalidAuditCursor,
	"invalid_document_query":        store.ErrInvalidDocumentQuery,
	"invalid_document_cursor":       store.ErrInvalidDocumentCursor,
	"cursor_expired":                store.ErrDocumentCursorExpired,
	"backup_locked":                 backup.ErrRepoLocked,
	"backup_restore_target_active":  home.ErrVaultLocked,
	"pack_retirement_deferred":      packstore.ErrPackRetirementDeferred,
	"maintenance_busy":              ErrMaintenanceBusy,
	"processing_unavailable":        ErrProcessingUnavailable,
	"processing_plan_changed":       ErrProcessingPlanChanged,
	"processing_consent_required":   fmt.Errorf("%w: %w", ErrProcessingConsent, store.ErrProcessingConsentRequired),
	"processing_consent_expired":    fmt.Errorf("%w: %w", ErrProcessingConsent, store.ErrProcessingConsentExpired),
	"processing_consent_revoked":    fmt.Errorf("%w: %w", ErrProcessingConsent, store.ErrProcessingConsentRevoked),
	"derivative_purge_plan_changed": ErrProcessingPlanChanged,
	"email_document_conflict":       store.ErrEmailDocumentConflict,
	"email_pending":                 store.ErrEmailPending,
	"email_not_supported":           store.ErrEmailNotSupported,
	"email_derivative_suppressed":   store.ErrEmailDerivativeSuppressed,
	"email_corrupt":                 store.ErrEmailCorrupt,
	"email_part_unavailable":        store.ErrEmailPartUnavailable,
	"invalid_email_part":            store.ErrInvalidEmailPart,
	"snapshot_gone":                 store.ErrSnapshotGone,
	"invalid_cursor":                store.ErrSnapshotCursor,
	"snapshot_capacity":             store.ErrSnapshotAdmission,
	"snapshot_busy":                 store.ErrSnapshotBusy,
	"snapshot_too_large":            store.ErrQuerySnapshotTooLarge,
	"invalid_saved_query_run":       store.ErrInvalidSavedQueryRun,
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
	observedScopeCount := 0
	if e.Code == "scope_too_large" && e.ObservedScopeCount > processing.MaxSourceFenceIDs {
		cause = &SourceFenceScopeTooLargeError{ObservedScopeCount: e.ObservedScopeCount, detail: e.Detail}
		observedScopeCount = e.ObservedScopeCount
	} else if target, ok := codeToTypedErr[e.Code]; ok {
		cause = fmt.Errorf("%s: %w", e.Detail, target)
	} else {
		cause = fmt.Errorf("daemon error (%d %s): %s", e.Status, e.Code, e.Detail)
	}
	return &problemError{code: e.Code, observedScopeCount: observedScopeCount, err: cause}
}

// Children fetches every page. Callers that need bounded traversal should use
// ChildrenPage.
func (c *Connection) Children(ctx context.Context, id int64) ([]api.Node, error) {
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
func (c *Connection) ChildrenPage(
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
	apiResponse, err := c.API().ListChildren(ctx, &apiclient.ListChildrenRequestOptions{PathParams: &apiclient.ListChildrenPath{ID: id}, Query: &apiclient.ListChildrenQuery{Limit: new(int64(limit)), Offset: new(int64(offset))}})
	if err != nil {
		return api.NodePage{}, err
	}
	page = *apiResponse
	if page.Limit != limit || page.Offset != offset || page.Total < 0 ||
		page.Directory.ID != id || page.Directory.Kind != "dir" ||
		page.Directory.TrashedAt != "" || !strings.HasPrefix(page.Directory.Path, "/") ||
		len(page.Items) > limit ||
		(len(page.Items) == 0 && offset < page.Total) ||
		(len(page.Items) > 0 && offset+len(page.Items) > page.Total) {
		return api.NodePage{}, errors.New("children response has inconsistent pagination")
	}
	return page, nil
}

func (c *Connection) Content(ctx context.Context, id int64) (*ContentStream, error) {
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).GetNodeContent(runtime.WithStreamingResponse(ctx), &apiclient.GetNodeContentRequestOptions{PathParams: &apiclient.GetNodeContentPath{ID: id}})
	if err != nil {
		return nil, err
	}
	return decodeContent(responseHTTP, fmt.Sprintf("content of node %d", id))
}

// Provenance returns one bounded newest-ingest-first page of immutable origin
// facts for a stable file node.
func (c *Connection) Provenance(
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
	apiResponse, err := c.API().ListNodeProvenance(ctx, &apiclient.ListNodeProvenanceRequestOptions{PathParams: &apiclient.ListNodeProvenancePath{ID: nodeID}, Query: &apiclient.ListNodeProvenanceQuery{Limit: new(int64(limit)), Offset: new(int64(offset))}})
	if err != nil {
		return api.ProvenancePage{}, err
	}
	page = *apiResponse
	if err := validateProvenancePage(page, nodeID, limit, offset); err != nil {
		return api.ProvenancePage{}, err
	}
	return page, nil
}

// AppendProvenance appends one origin fact to a file node under the caller's
// inspected node revision.
func (c *Connection) AppendProvenance(
	ctx context.Context, nodeID, revision int64, request api.ProvenanceAppendRequest,
) (api.ProvenanceAppendReceipt, error) {
	var receipt api.ProvenanceAppendReceipt
	if nodeID < 1 {
		return receipt, errors.New("provenance node ID must be positive")
	}
	if revision < 1 {
		return receipt, errors.New("provenance revision must be positive")
	}
	if request.SourceKind == "" || request.SourceDescription == "" || request.OriginalPath == "" {
		return receipt, errors.New("provenance source kind, description, and path are required")
	}
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).AppendNodeProvenance(ctx, &apiclient.AppendNodeProvenanceRequestOptions{PathParams: &apiclient.AppendNodeProvenancePath{ID: nodeID}, Header: &apiclient.AppendNodeProvenanceHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}, Body: &request})
	if err == nil {
		receipt = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
	if err != nil {
		return api.ProvenanceAppendReceipt{}, err
	}
	if err := validateProvenanceAppendReceipt(receipt, headers.Get("ETag"), nodeID, revision, request); err != nil {
		return api.ProvenanceAppendReceipt{}, err
	}
	return receipt, nil
}

// validateProvenanceAppendReceipt checks that the receipt binds the node and
// fact to the request and that the response ETag carries the advanced node
// revision, mirroring validateVersionPruneBinding. The daemon owns the fact
// identity; only structural binding is verified here.
func validateProvenanceAppendReceipt(
	receipt api.ProvenanceAppendReceipt, etag string, nodeID, revision int64,
	request api.ProvenanceAppendRequest,
) error {
	if receipt.Node.ID != nodeID || receipt.Fact.NodeID != nodeID {
		return errors.New("provenance receipt does not bind its node")
	}
	if receipt.Node.Revision != revision+1 {
		return fmt.Errorf("provenance receipt node revision %d, expected %d",
			receipt.Node.Revision, revision+1)
	}
	if etag != strconv.Quote(strconv.FormatInt(receipt.Node.Revision, 10)) {
		return fmt.Errorf("provenance response ETag %q disagrees with node revision %d",
			etag, receipt.Node.Revision)
	}
	if receipt.Fact.SourceKind != request.SourceKind ||
		receipt.Fact.SourceDescription != request.SourceDescription ||
		receipt.Fact.OriginalPath != request.OriginalPath ||
		!equalOptionalString(receipt.Fact.OriginalMTime, request.OriginalMTime) ||
		!equalOptionalString(receipt.Fact.Supersedes, request.Supersedes) {
		return errors.New("provenance receipt fact does not bind its request")
	}
	if !receipt.Fact.Active {
		return errors.New("provenance receipt fact is not active")
	}
	return nil
}

func equalOptionalString(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// VersionContent streams immutable bytes by stable version ID.
func (c *Connection) VersionContent(ctx context.Context, id string) (*ContentStream, error) {
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).GetContentVersionBytes(runtime.WithStreamingResponse(ctx), &apiclient.GetContentVersionBytesRequestOptions{PathParams: &apiclient.GetContentVersionBytesPath{VersionID: id}})
	if err != nil {
		return nil, err
	}
	stream, err := decodeContent(responseHTTP, "content version "+id)
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
func (c *Connection) PruneContentVersions(
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
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).PruneNodeContentVersions(ctx, &apiclient.PruneNodeContentVersionsRequestOptions{PathParams: &apiclient.PruneNodeContentVersionsPath{ID: nodeID}, Header: &apiclient.PruneNodeContentVersionsHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}, Body: &request})
	if err == nil {
		report = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
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
		report.MixedBlobsPendingMaintenance < 0 ||
		report.DeletedVersions < 0 {
		return errors.New("version-prune receipt contains negative counts")
	}
	if report.UniqueBlobs != report.SharedBlobs+report.ReleasableBlobs ||
		report.ReleasableBlobs != report.LooseBlobsPendingGC+
			report.PackedBlobsPendingRepack-report.MixedBlobsPendingMaintenance ||
		report.MixedBlobsPendingMaintenance > report.LooseBlobsPendingGC ||
		report.MixedBlobsPendingMaintenance > report.PackedBlobsPendingRepack {
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
	if report.LogicalBytes != logicalBytes || report.UniqueBlobs < len(uniqueBlobs) {
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
func (c *Connection) ContentReferences(
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
	apiResponse, err := c.API().LookupContentReferences(ctx, &apiclient.LookupContentReferencesRequestOptions{Query: &apiclient.LookupContentReferencesQuery{Sha256: hash, Limit: new(int64(limit)), Offset: new(int64(offset))}})
	if err != nil {
		return page, err
	}
	page = *apiResponse
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

func decodeContent(resp *http.Response, identity string) (*ContentStream, error) {
	size, err := strconv.ParseInt(resp.Header.Get(api.BlobSizeHeader), 10, 64)
	if err != nil || size < 0 || size == math.MaxInt64 {
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

func (c *Connection) Search(ctx context.Context, query string, limit int) (api.SearchReport, error) {
	return c.SearchWithOptions(ctx, query, limit, SearchOptions{})
}

// SearchWithOptions returns one bounded ranked or filter-only result set.
func (c *Connection) SearchWithOptions(
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
	params := apiclient.SearchQuery{}
	params.Q = new(query)
	params.Limit = new(int64(limit))
	if opts.TagID != "" {
		params.TagID = new(opts.TagID)
	}
	if mimeType != "" {
		params.MimeType = new(mimeType)
	}
	if opts.UnderNodeID != 0 {
		params.UnderNodeID = new(opts.UnderNodeID)
	}
	if modifiedSince != "" {
		params.ModifiedSince = new(modifiedSince)
	}
	if modifiedBefore != "" {
		params.ModifiedBefore = new(modifiedBefore)
	}
	apiResponse, err := c.API().Search(ctx, &apiclient.SearchRequestOptions{Query: &params})
	if err != nil {
		return out, err
	}
	out = *apiResponse
	if out.TagID != opts.TagID || out.MIMEType != mimeType ||
		out.UnderNodeID != opts.UnderNodeID || out.ModifiedSince != modifiedSince ||
		out.ModifiedBefore != modifiedBefore {
		return api.SearchReport{}, errors.New("search response has inconsistent filter authority")
	}
	return out, nil
}

// AuditPreviewOptions selects one live directory for permanent enrollment.
// Exactly one of Path or NodeID must be supplied.
type AuditPreviewOptions struct {
	Path       string
	NodeID     int64
	AgentLabel string
}

// PreviewAudit derives the exact permanent-retention boundary without
// changing the vault and returns a short-lived, daemon-local execution token.
func (c *Connection) PreviewAudit(
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
	body := apiclient.PreviewAuditEnrollmentBody{}
	if opts.Path != "" {
		body.Path = &opts.Path
	} else {
		body.NodeID = &opts.NodeID
	}
	if opts.AgentLabel != "" {
		body.AgentLabel = &opts.AgentLabel
	}
	apiResponse, err := c.API().PreviewAuditEnrollment(ctx, &apiclient.PreviewAuditEnrollmentRequestOptions{Body: &body})
	if err == nil {
		preview = *apiResponse
	}
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
func (c *Connection) EnableAudit(
	ctx context.Context, previewToken string, acknowledgePermanentRetention bool,
) (api.AuditStatus, error) {
	var status api.AuditStatus
	if previewToken == "" {
		return status, errors.New("audit enable requires a preview token")
	}
	if !acknowledgePermanentRetention {
		return status, errors.New("audit enable requires permanent-retention acknowledgment")
	}
	apiResponse, err := c.API().EnableAudit(ctx, &apiclient.EnableAuditRequestOptions{Body: &apiclient.EnableAuditBody{PreviewToken: previewToken, AcknowledgePermanentRetention: acknowledgePermanentRetention}})
	if err == nil {
		status = *apiResponse
	}
	if err != nil {
		return status, err
	}
	if err := validateAuditStatus(status); err != nil {
		return api.AuditStatus{}, err
	}
	if !status.Enabled {
		return api.AuditStatus{}, errors.New("audit enable response reports dormant authority")
	}
	if status.EnabledScopeID == "" {
		return api.AuditStatus{}, errors.New("audit enable response lacks enabled scope identity")
	}
	return status, nil
}

// AuditStatus returns vault-wide authority and, when selected, one node's
// sticky protection bindings. At most one of path and nodeID may be supplied.
func (c *Connection) AuditStatus(
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
	params := apiclient.AuditStatusQuery{}
	if path != "" {
		params.Path = new(path)
	}
	if nodeID != 0 {
		params.NodeID = new(nodeID)
	}
	apiResponse, err := c.API().AuditStatus(ctx, &apiclient.AuditStatusRequestOptions{Query: &params})
	if err != nil {
		return status, err
	}
	status = *apiResponse
	if err := validateAuditStatus(status); err != nil {
		return api.AuditStatus{}, err
	}
	return status, nil
}

// VerifyAudit independently replays audit authority and verifies every unique
// blob retained by protected history. When expected is non-nil, the report
// also proves whether current authority extends that exact recorded prefix.
func (c *Connection) VerifyAudit(
	ctx context.Context, expected *api.AuditEvidence,
) (api.AuditVerifyReport, error) {
	var report api.AuditVerifyReport
	if expected != nil {
		if err := ValidateAuditEvidence(*expected); err != nil {
			return report, fmt.Errorf("invalid expected audit evidence: %w", err)
		}
	}
	request := api.AuditVerifyRequest{Expected: expected}
	apiResponse, err := c.API().VerifyAudit(ctx, &apiclient.VerifyAuditRequestOptions{Body: &request})
	if err != nil {
		return report, err
	}
	report = *apiResponse
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
func (c *Connection) AuditHistory(
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
	params := apiclient.AuditNodeHistoryQuery{}
	if path != "" {
		params.Path = new(path)
	}
	if nodeID != 0 {
		params.NodeID = new(nodeID)
	}
	params.Limit = new(int64(limit))
	if cursor != "" {
		params.Cursor = new(cursor)
	}
	apiResponse, err := c.API().AuditNodeHistory(ctx, &apiclient.AuditNodeHistoryRequestOptions{Query: &params})
	if err != nil {
		return page, err
	}
	page = *apiResponse
	if err := validateAuditEventPage(page, nodeID, limit, cursor); err != nil {
		return api.AuditEventPage{}, err
	}
	return page, nil
}

// TimelineRebuild starts or replays one durable timeline rebuild.
func (c *Connection) TimelineRebuild(
	ctx context.Context, operationID string,
) (api.TimelineBuild, error) {
	var build api.TimelineBuild
	if !validUUIDv4(operationID) {
		return build, errors.New("timeline rebuild requires a canonical UUIDv4 operation ID")
	}
	apiResponse, err := c.API().CreateTimelineRebuild(ctx, &apiclient.CreateTimelineRebuildRequestOptions{Body: &api.TimelineRebuildRequest{OperationID: operationID}})
	if err != nil {
		return build, err
	}
	build = *apiResponse
	if build.OperationID != operationID {
		return api.TimelineBuild{}, errors.New("timeline rebuild response has an unexpected operation ID")
	}
	return build, nil
}

// TimelineRebuildStatus reads one durable timeline rebuild receipt.
func (c *Connection) TimelineRebuildStatus(
	ctx context.Context, operationID string,
) (api.TimelineBuild, error) {
	var build api.TimelineBuild
	if !validUUIDv4(operationID) {
		return build, errors.New("timeline rebuild status requires a canonical UUIDv4 operation ID")
	}
	apiResponse, err := c.API().ReadTimelineRebuild(ctx, &apiclient.ReadTimelineRebuildRequestOptions{PathParams: &apiclient.ReadTimelineRebuildPath{OperationID: uuid.MustParse(operationID)}})
	if err != nil {
		return build, err
	}
	build = *apiResponse
	if build.OperationID != operationID {
		return api.TimelineBuild{}, errors.New("timeline rebuild status response has an unexpected operation ID")
	}
	return build, nil
}

// AuditScopeHistory returns one stable newest-first page across every member
// of one permanent audit scope.
func (c *Connection) AuditScopeHistory(
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
	params := apiclient.AuditScopeHistoryQuery{}
	params.Limit = new(int64(limit))
	if cursor != "" {
		params.Cursor = new(cursor)
	}
	apiResponse, err := c.API().AuditScopeHistory(ctx, &apiclient.AuditScopeHistoryRequestOptions{Query: &params, PathParams: &apiclient.AuditScopeHistoryPath{ScopeID: scopeID}})
	if err != nil {
		return page, err
	}
	page = *apiResponse
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
	if err := validateAuditAttachment(event.Kind, event.Attachment); err != nil {
		return err
	}
	if event.Kind == "ingest_observe" {
		return validateAuditIngestObservation(event)
	}
	return nil
}

func validateAuditAttachment(eventKind string, change *api.AuditAttachmentChange) error {
	wantKind := ""
	switch eventKind {
	case "tag_define", "tag_rename", "tag_delete":
		wantKind = "tag_definition"
	case "tag_assign", "tag_unassign":
		wantKind = "tag_assignment"
	case "ingest_observe", "provenance_add", "provenance_supersede":
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
	case "tag_define", "tag_assign", "ingest_observe", "provenance_add":
		if change.Before == nil && change.After != nil &&
			((eventKind != "ingest_observe" && eventKind != "provenance_add") ||
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

func validateAuditIngestObservation(event api.AuditEvent) error {
	change := event.Attachment
	if event.PriorNodeRevision < 1 ||
		event.ResultingNodeRevision != event.PriorNodeRevision+1 ||
		event.PriorCurrentVersionID == nil || event.ResultingCurrentVersionID == nil ||
		*event.PriorCurrentVersionID != *event.ResultingCurrentVersionID ||
		event.SourceVersionID != nil || event.TargetNodeID != nil ||
		change == nil || change.Before != nil || change.After == nil ||
		change.After.NodeID != event.NodeID ||
		change.After.ProvenanceID != change.Identity.ProvenanceID ||
		change.After.Supersedes != nil {
		return errors.New("ingest observation has an invalid authority transition")
	}
	return nil
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
		preview.VaultTopologyNodes < 0 || preview.VaultAttachmentRecords < 0 ||
		(preview.InitialAuthority && preview.VaultTopologyNodes < preview.MemberCount) ||
		(!preview.InitialAuthority && (preview.VaultTopologyNodes != 0 ||
			preview.VaultAttachmentRecords != 0)) ||
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
		if status.EnabledScopeID != "" || status.LineageID != "" || status.OperationSequenceHighWater != 0 ||
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
	if status.EnabledScopeID != "" {
		if !validUUIDv4(status.EnabledScopeID) {
			return errors.New("audit status has invalid enabled scope identity")
		}
		if _, ok := seen[status.EnabledScopeID]; !ok {
			return errors.New("audit status enabled scope is absent from authority")
		}
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
	if report.ProtectedBlobs < 0 || report.ProtectedBytes < 0 || report.VerifiedBlobs < 0 {
		return errors.New("audit verification has inconsistent blob totals")
	}
	if err := validateAuditVerifyBlobProblems(report); err != nil {
		return err
	}
	for index, problem := range report.MetadataProblems {
		if problem == "" {
			return fmt.Errorf("audit verification metadata problem %d is empty", index)
		}
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

func validateAuditVerifyBlobProblems(report api.AuditVerifyReport) error {
	affected := 0
	previousHash := ""
	seenLocations := make(map[string]struct{}, len(report.Problems))
	storelessHashes := make(map[string]bool)
	scopedHashes := make(map[string]bool)
	for index, problem := range report.Problems {
		if !validSHA256Hex(problem.Hash) ||
			(problem.StoreID != "" && !validUUIDv4(problem.StoreID)) ||
			(problem.Problem != "missing" && problem.Problem != "corrupt" &&
				problem.Problem != "unreadable") ||
			(previousHash != "" && problem.Hash < previousHash) {
			return fmt.Errorf("audit verification blob problem %d is invalid", index)
		}
		locationKey := problem.Hash + "\x00" + problem.StoreID
		if _, duplicate := seenLocations[locationKey]; duplicate {
			return fmt.Errorf(
				"audit verification blob problem %d duplicates location evidence",
				index,
			)
		}
		seenLocations[locationKey] = struct{}{}
		if problem.StoreID == "" {
			if problem.Problem != "missing" || scopedHashes[problem.Hash] {
				return fmt.Errorf(
					"audit verification blob problem %d has contradictory storeless evidence",
					index,
				)
			}
			storelessHashes[problem.Hash] = true
		} else {
			if storelessHashes[problem.Hash] {
				return fmt.Errorf(
					"audit verification blob problem %d mixes storeless and scoped evidence",
					index,
				)
			}
			scopedHashes[problem.Hash] = true
		}
		if problem.Hash != previousHash {
			affected++
			previousHash = problem.Hash
		}
	}
	if report.VerifiedBlobs+affected != report.ProtectedBlobs {
		return errors.New("audit verification has inconsistent blob totals")
	}
	return nil
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

// SavedQueries returns one bounded, name-sorted page of saved definitions.
func (c *Connection) SavedQueries(
	ctx context.Context, kind string, limit, offset int,
) (api.SavedQueryPage, error) {
	var page api.SavedQueryPage
	if kind != "" && kind != store.SavedQueryKindQuery &&
		kind != store.SavedQueryKindHighlightSet {
		return page, errors.New("saved query kind is unknown")
	}
	if limit < 1 || limit > 1000 || offset < 0 {
		return page, errors.New("saved query page is outside the supported bounds")
	}
	params := apiclient.ListSavedQueriesQuery{}
	if kind != "" {
		params.Kind = new(apiclient.ListSavedQueriesQueryKind(kind))
	}
	params.Limit = new(int64(limit))
	params.Offset = new(int64(offset))
	apiResponse, err := c.API().ListSavedQueries(ctx, &apiclient.ListSavedQueriesRequestOptions{Query: &params})
	if err != nil {
		return page, err
	}
	page = *apiResponse
	if err := validateSavedQueryPage(page, kind, limit, offset); err != nil {
		return api.SavedQueryPage{}, err
	}
	return page, nil
}

// SavedQuery returns one saved definition by stable ID.
func (c *Connection) SavedQuery(ctx context.Context, id string) (api.SavedQuery, error) {
	var saved api.SavedQuery
	if !validUUIDv4(id) {
		return saved, errors.New("saved query ID must be a canonical UUIDv4")
	}
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).GetSavedQuery(ctx, &apiclient.GetSavedQueryRequestOptions{PathParams: &apiclient.GetSavedQueryPath{SavedQueryID: id}})
	if err == nil {
		saved = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
	if err != nil {
		return saved, err
	}
	if err := validateSavedQueryResponse(saved, headers.Get("ETag")); err != nil {
		return api.SavedQuery{}, err
	}
	if saved.ID != id {
		return api.SavedQuery{}, errors.New("saved query response does not match requested ID")
	}
	return saved, nil
}

// CreateSavedQuery stores one complete query or literal highlight definition.
func (c *Connection) CreateSavedQuery(
	ctx context.Context, request api.SavedQueryCreateRequest,
) (api.SavedQuery, error) {
	var saved api.SavedQuery
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).CreateSavedQuery(ctx, &apiclient.CreateSavedQueryRequestOptions{Body: &request})
	if err == nil {
		saved = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
	if err != nil {
		return saved, err
	}
	if err := validateSavedQueryResponse(saved, headers.Get("ETag")); err != nil {
		return api.SavedQuery{}, err
	}
	if saved.Revision != 1 || saved.Name == "" || saved.Kind != request.Kind {
		return api.SavedQuery{}, errors.New("created saved query response has inconsistent authority")
	}
	return saved, nil
}

// UpdateSavedQuery applies supplied mutable fields under revision fencing.
func (c *Connection) UpdateSavedQuery(
	ctx context.Context, id string, revision int64, patch api.SavedQueryPatch,
) (api.SavedQuery, error) {
	var saved api.SavedQuery
	if !validUUIDv4(id) {
		return saved, errors.New("saved query ID must be a canonical UUIDv4")
	}
	if revision < 1 {
		return saved, errors.New("saved query revision must be positive")
	}
	if patch.Name == nil && patch.Description == nil && patch.Payload == nil {
		return saved, errors.New("saved query patch must set at least one field")
	}
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).UpdateSavedQuery(ctx, &apiclient.UpdateSavedQueryRequestOptions{PathParams: &apiclient.UpdateSavedQueryPath{SavedQueryID: id}, Header: &apiclient.UpdateSavedQueryHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}, Body: &patch})
	if err == nil {
		saved = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
	if err != nil {
		return saved, err
	}
	if err := validateSavedQueryResponse(saved, headers.Get("ETag")); err != nil {
		return api.SavedQuery{}, err
	}
	if saved.ID != id || (saved.Revision != revision && saved.Revision != revision+1) {
		return api.SavedQuery{}, errors.New("updated saved query response has inconsistent authority")
	}
	return saved, nil
}

// DeleteSavedQuery removes one definition under revision fencing.
func (c *Connection) DeleteSavedQuery(
	ctx context.Context, id string, revision int64,
) (api.SavedQuery, error) {
	var saved api.SavedQuery
	if !validUUIDv4(id) {
		return saved, errors.New("saved query ID must be a canonical UUIDv4")
	}
	if revision < 1 {
		return saved, errors.New("saved query revision must be positive")
	}
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).DeleteSavedQuery(ctx, &apiclient.DeleteSavedQueryRequestOptions{PathParams: &apiclient.DeleteSavedQueryPath{SavedQueryID: id}, Header: &apiclient.DeleteSavedQueryHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}})
	if err == nil {
		saved = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
	if err != nil {
		return saved, err
	}
	if err := validateSavedQueryResponse(saved, headers.Get("ETag")); err != nil {
		return api.SavedQuery{}, err
	}
	if saved.ID != id || saved.Revision != revision {
		return api.SavedQuery{}, errors.New("deleted saved query response has inconsistent authority")
	}
	return saved, nil
}

func validateSavedQueryPage(page api.SavedQueryPage, kind string, limit, offset int) error {
	expectedItems := 0
	if offset < page.Total {
		expectedItems = min(limit, page.Total-offset)
	}
	if page.Limit != limit || page.Offset != offset || page.Total < 0 ||
		len(page.Items) != expectedItems {
		return errors.New("saved query page has inconsistent bounds")
	}
	for index, saved := range page.Items {
		if err := validateSavedQueryRecord(saved); err != nil {
			return fmt.Errorf("saved query page item %d: %w", index, err)
		}
		if kind != "" && saved.Kind != kind {
			return errors.New("saved query page contains the wrong kind")
		}
		if index > 0 {
			previous := page.Items[index-1]
			if previous.Name > saved.Name ||
				(previous.Name == saved.Name && previous.ID >= saved.ID) {
				return errors.New("saved query page is not strictly name-sorted")
			}
		}
	}
	return nil
}

func validateSavedQueryResponse(saved api.SavedQuery, etag string) error {
	if etag != strconv.Quote(strconv.FormatInt(saved.Revision, 10)) {
		return errors.New("saved query response ETag disagrees with its revision")
	}
	return validateSavedQueryRecord(saved)
}

func validateSavedQueryRecord(saved api.SavedQuery) error {
	if !validUUIDv4(saved.ID) || saved.Name == "" || saved.Revision < 1 {
		return errors.New("saved query response lacks valid identity authority")
	}
	created, err := time.Parse(time.RFC3339Nano, saved.CreatedAt)
	if err != nil {
		return errors.New("saved query response has an invalid creation timestamp")
	}
	updated, err := time.Parse(time.RFC3339Nano, saved.UpdatedAt)
	if err != nil || updated.Before(created) {
		return errors.New("saved query response has an invalid update timestamp")
	}
	var canonical []byte
	var fingerprint string
	switch saved.Kind {
	case store.SavedQueryKindQuery:
		value, parseErr := query.Parse(saved.Payload)
		if parseErr != nil {
			return fmt.Errorf("saved query response payload: %w", parseErr)
		}
		canonical, err = query.Canonical(value)
		if err == nil {
			fingerprint, err = query.Fingerprint(value)
		}
	case store.SavedQueryKindHighlightSet:
		value, parseErr := query.ParseHighlightSet(saved.Payload)
		if parseErr != nil {
			return fmt.Errorf("saved highlight response payload: %w", parseErr)
		}
		canonical, err = query.CanonicalHighlightSet(value)
		if err == nil {
			fingerprint, err = query.HighlightSetFingerprint(value)
		}
	default:
		return errors.New("saved query response has an unknown kind")
	}
	if err != nil || !bytes.Equal(canonical, saved.Payload) || fingerprint != saved.Fingerprint {
		return errors.New("saved query response payload authority is inconsistent")
	}
	return nil
}

// Tags returns one bounded name-sorted page of tag definitions.
func (c *Connection) Tags(ctx context.Context, limit, offset int) (api.TagPage, error) {
	var page api.TagPage
	apiResponse, err := c.API().ListTags(ctx, &apiclient.ListTagsRequestOptions{Query: &apiclient.ListTagsQuery{Limit: new(int64(limit)), Offset: new(int64(offset))}})
	if err != nil {
		return page, err
	}
	page = *apiResponse
	if err := validateTagPage(page, limit, offset); err != nil {
		return api.TagPage{}, err
	}
	return page, nil
}

// Tag returns one tag definition by stable ID.
func (c *Connection) Tag(ctx context.Context, id string) (api.Tag, error) {
	var tag api.Tag
	if !validUUIDv4(id) {
		return tag, errors.New("tag ID must be a canonical UUIDv4")
	}
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).GetTag(ctx, &apiclient.GetTagRequestOptions{PathParams: &apiclient.GetTagPath{TagID: id}})
	if err == nil {
		tag = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
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
func (c *Connection) TagByName(ctx context.Context, name string) (api.Tag, error) {
	var tag api.Tag
	normalized, err := store.NormalizeTagName(name)
	if err != nil {
		return tag, err
	}
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).ResolveTagByName(ctx, &apiclient.ResolveTagByNameRequestOptions{Query: &apiclient.ResolveTagByNameQuery{Name: normalized}})
	if err == nil {
		tag = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
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
func (c *Connection) CreateTag(ctx context.Context, name string) (api.Tag, error) {
	var tag api.Tag
	normalized, err := store.NormalizeTagName(name)
	if err != nil {
		return tag, err
	}
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).CreateTag(ctx, &apiclient.CreateTagRequestOptions{Body: &apiclient.CreateTagBody{Name: normalized}})
	if err == nil {
		tag = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
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
func (c *Connection) RenameTag(ctx context.Context, id string, revision int64, name string) (api.Tag, error) {
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
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).RenameTag(ctx, &apiclient.RenameTagRequestOptions{PathParams: &apiclient.RenameTagPath{TagID: id}, Header: &apiclient.RenameTagHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}, Body: &apiclient.RenameTagBody{Name: normalized}})
	if err == nil {
		tag = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
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
func (c *Connection) DeleteTag(
	ctx context.Context, id string, revision int64,
) (api.TagDeletionReceipt, error) {
	var receipt api.TagDeletionReceipt
	if !validUUIDv4(id) {
		return receipt, errors.New("tag ID must be a canonical UUIDv4")
	}
	if revision < 1 {
		return receipt, errors.New("tag revision must be positive")
	}
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).DeleteTag(ctx, &apiclient.DeleteTagRequestOptions{PathParams: &apiclient.DeleteTagPath{TagID: id}, Header: &apiclient.DeleteTagHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}})
	if err == nil {
		receipt = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
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
func (c *Connection) NodeTags(
	ctx context.Context, nodeID int64, limit, offset int,
) (api.TagPage, error) {
	var page api.TagPage
	if nodeID <= 0 {
		return page, errors.New("tagged node ID must be positive")
	}
	apiResponse, err := c.API().ListNodeTags(ctx, &apiclient.ListNodeTagsRequestOptions{PathParams: &apiclient.ListNodeTagsPath{ID: nodeID}, Query: &apiclient.ListNodeTagsQuery{Limit: new(int64(limit)), Offset: new(int64(offset))}})
	if err != nil {
		return page, err
	}
	page = *apiResponse
	if err := validateTagPage(page, limit, offset); err != nil {
		return api.TagPage{}, err
	}
	return page, nil
}

// TaggedNodes returns one bounded page of live or trashed nodes carrying tagID.
func (c *Connection) TaggedNodes(
	ctx context.Context, tagID string, limit, offset int,
) (api.TaggedNodePage, error) {
	var page api.TaggedNodePage
	if !validUUIDv4(tagID) {
		return page, errors.New("tag ID must be a canonical UUIDv4")
	}
	apiResponse, err := c.API().ListTagNodes(ctx, &apiclient.ListTagNodesRequestOptions{PathParams: &apiclient.ListTagNodesPath{TagID: tagID}, Query: &apiclient.ListTagNodesQuery{Limit: new(int64(limit)), Offset: new(int64(offset))}})
	if err != nil {
		return page, err
	}
	page = *apiResponse
	if err := validateTaggedNodePage(page, limit, offset); err != nil {
		return api.TaggedNodePage{}, err
	}
	return page, nil
}

// AssignTag attaches tagID to nodeID under an optimistic node revision.
func (c *Connection) AssignTag(
	ctx context.Context, tagID string, nodeID, revision int64,
) (api.TagAssignmentReceipt, error) {
	return c.changeTagAssignment(ctx, http.MethodPut, tagID, nodeID, revision)
}

// UnassignTag removes tagID from nodeID under an optimistic node revision.
func (c *Connection) UnassignTag(
	ctx context.Context, tagID string, nodeID, revision int64,
) (api.TagAssignmentReceipt, error) {
	return c.changeTagAssignment(ctx, http.MethodDelete, tagID, nodeID, revision)
}

// AssignTagPath resolves a live virtual path and assigns tagID in one daemon
// transaction, so ancestor moves cannot retarget the operation between calls.
func (c *Connection) AssignTagPath(
	ctx context.Context, tagID, path string,
) (api.TagAssignmentReceipt, error) {
	return c.changeTagAssignmentPath(ctx, http.MethodPut, tagID, path)
}

// UnassignTagPath resolves a live virtual path and removes tagID in one daemon
// transaction, so ancestor moves cannot retarget the operation between calls.
func (c *Connection) UnassignTagPath(
	ctx context.Context, tagID, path string,
) (api.TagAssignmentReceipt, error) {
	return c.changeTagAssignmentPath(ctx, http.MethodDelete, tagID, path)
}

func (c *Connection) changeTagAssignment(
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
	var etag string
	if method == http.MethodPut {
		var responseHTTP *http.Response
		response, err := c.apiWithResponse(&responseHTTP).AssignTag(ctx, &apiclient.AssignTagRequestOptions{PathParams: &apiclient.AssignTagPath{ID: nodeID, TagID: tagID}, Header: &apiclient.AssignTagHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}})
		if err != nil {
			return receipt, err
		}
		receipt = *response
		etag = responseHTTP.Header.Get("ETag")
	} else {
		var responseHTTP *http.Response
		response, err := c.apiWithResponse(&responseHTTP).UnassignTag(ctx, &apiclient.UnassignTagRequestOptions{PathParams: &apiclient.UnassignTagPath{ID: nodeID, TagID: tagID}, Header: &apiclient.UnassignTagHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}})
		if err != nil {
			return receipt, err
		}
		receipt = *response
		etag = responseHTTP.Header.Get("ETag")
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

func (c *Connection) changeTagAssignmentPath(
	ctx context.Context, method, tagID, path string,
) (api.TagAssignmentReceipt, error) {
	var receipt api.TagAssignmentReceipt
	if !validUUIDv4(tagID) {
		return receipt, errors.New("tag ID must be a canonical UUIDv4")
	}
	if !strings.HasPrefix(path, "/") {
		return receipt, errors.New("tag assignment path must be absolute")
	}
	var etag string
	if method == http.MethodPut {
		var responseHTTP *http.Response
		response, err := c.apiWithResponse(&responseHTTP).AssignTagPath(ctx, &apiclient.AssignTagPathRequestOptions{PathParams: &apiclient.AssignTagPathPath{TagID: tagID}, Body: &apiclient.AssignTagPathBody{Path: path}})
		if err != nil {
			return receipt, err
		}
		receipt = *response
		etag = responseHTTP.Header.Get("ETag")
	} else {
		var responseHTTP *http.Response
		response, err := c.apiWithResponse(&responseHTTP).UnassignTagPath(ctx, &apiclient.UnassignTagPathRequestOptions{PathParams: &apiclient.UnassignTagPathPath{TagID: tagID}, Body: &apiclient.UnassignTagPathBody{Path: path}})
		if err != nil {
			return receipt, err
		}
		receipt = *response
		etag = responseHTTP.Header.Get("ETag")
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
		page.Offset != offset || page.Total < 0 || page.OmittedTrashed < 0 ||
		len(page.Items) > limit ||
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

// MkdirPath creates one directory at an exact absolute virtual coordinate.
// The daemon resolves the existing parent and performs the creation in one
// transaction.
func (c *Connection) MkdirPath(ctx context.Context, path string) (api.Node, error) {
	canonical, err := canonicalDirectoryPath(path)
	if err != nil {
		return api.Node{}, err
	}
	var node api.Node
	var headers http.Header
	var apiResponseHTTP *http.Response
	apiResponse, err := c.apiWithResponse(&apiResponseHTTP).MkdirPath(ctx, &apiclient.MkdirPathRequestOptions{Body: &apiclient.MkdirPathBody{Path: path}})
	if err == nil {
		node = *apiResponse
		headers = apiResponseHTTP.Header.Clone()
	}
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

// IngestOptions selects source filters, destination policy, and an optional
// initial collection label for a server-side import. The zero value keeps
// ordinary suffixing semantics without a label.
type IngestOptions struct {
	Include         []string
	Exclude         []string
	Replace         bool
	CollectionLabel *string
}

func (o IngestOptions) requestBody(paths []string, dest string) api.IngestRequest {
	return api.IngestRequest{Paths: paths, Dest: dest, Include: o.Include, Exclude: o.Exclude, Replace: o.Replace, CollectionLabel: o.CollectionLabel}
}

// IngestStream imports server-side paths while delivering structured scan and
// ingest progress. Success requires a terminal report event; an HTTP 200 only
// means that the stream started.
func (c *Connection) IngestStream(
	ctx context.Context,
	paths []string,
	dest string,
	opts IngestOptions,
	progress func(api.IngestProgress),
) (api.IngestReport, error) {
	var apiResponseHTTP *http.Response
	_, err := c.apiWithResponse(&apiResponseHTTP).StreamIngest(runtime.WithStreamingResponse(ctx), &apiclient.StreamIngestRequestOptions{Body: new(opts.requestBody(paths, dest))})
	if err != nil {
		return api.IngestReport{}, err
	}
	resp := apiResponseHTTP
	defer func() { _ = resp.Body.Close() }()

	decoder := jsontext.NewDecoder(resp.Body)
	var result *api.IngestReport
	for {
		var event api.IngestEvent
		if err := json.UnmarshalDecode(decoder, &event); err != nil {
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

// Upload streams one remote file as a digest-checked multipart request. The
// reader is consumed once; callers retain responsibility for closing it when
// it also implements io.Closer.
func (c *Connection) Upload(
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

	var responseHTTP *http.Response
	_, callErr := c.apiWithResponse(&responseHTTP).UploadFile(runtime.WithStreamingResponse(ctx), &apiclient.UploadFileRequestOptions{Query: &apiclient.UploadFileQuery{ParentID: parentID, Name: name}, Header: &apiclient.UploadFileHeaders{XDocbankBlobHash: expectedHash, XDocbankBlobSize: expectedSize}}, func(_ context.Context, req *http.Request) error {
		req.Body = pipeReader
		req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
		return nil
	})
	if callErr != nil {
		_ = pipeReader.CloseWithError(callErr)
		<-writeDone
		return receipt, fmt.Errorf("uploading %q: %w", name, callErr)
	}
	resp := responseHTTP
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
	if err := json.UnmarshalRead(resp.Body, &receipt); err != nil {
		return receipt, fmt.Errorf("decoding upload response: %w", err)
	}
	return receipt, nil
}

// ReplaceContent streams raw bytes into a new immutable head under an
// optimistic node-revision precondition. Success requires the daemon's receipt
// and ETag to agree with the caller-declared byte identity and the requested
// node; callers retain responsibility for closing content when applicable.
func (c *Connection) ReplaceContent(
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

	var responseHTTP *http.Response
	response, err := c.apiWithResponse(&responseHTTP).ReplaceNodeContent(ctx, &apiclient.ReplaceNodeContentRequestOptions{PathParams: &apiclient.ReplaceNodeContentPath{ID: nodeID}, Header: &apiclient.ReplaceNodeContentHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10)), XDocbankBlobHash: expectedHash, XDocbankBlobSize: expectedSize}}, func(_ context.Context, req *http.Request) error {
		req.Body = io.NopCloser(content)
		req.ContentLength = expectedSize
		req.Header.Set("Content-Type", mimeType)
		req.Header.Set("Expect", "100-continue")
		return nil
	})
	if err != nil {
		return receipt, err
	}
	receipt = *response
	resp := responseHTTP
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
func (c *Connection) RevertContent(
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
	var responseHTTP *http.Response
	response, err := c.apiWithResponse(&responseHTTP).RevertNodeContent(ctx, &apiclient.RevertNodeContentRequestOptions{PathParams: &apiclient.RevertNodeContentPath{ID: nodeID}, Header: &apiclient.RevertNodeContentHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}, Body: &apiclient.RevertNodeContentBody{SourceVersionID: uuid.MustParse(sourceVersionID)}})
	if err != nil {
		return receipt, err
	}
	receipt = *response
	resp := responseHTTP
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

// BatchMove applies one bounded all-or-nothing reorganization. Path sources
// are resolved by the daemon inside the transaction; ID sources require the
// caller's inspected revision.
func (c *Connection) BatchMove(
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
	apiResponse, err := c.API().BatchMove(ctx, &apiclient.BatchMoveRequestOptions{Body: &api.BatchMoveRequest{Moves: moves}})
	if err != nil {
		return api.BatchMoveReport{}, err
	}
	report = *apiResponse
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
func (c *Connection) MoveToPath(
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
	apiResponse, err := c.API().MoveNode(ctx, &apiclient.MoveNodeRequestOptions{PathParams: &apiclient.MoveNodePath{ID: id}, Header: &apiclient.MoveNodeHeaders{IfMatch: strconv.Quote(strconv.FormatInt(rev, 10))}, Body: &apiclient.MoveNodeBody{DestPath: new(destPath)}})
	if err == nil {
		n = *apiResponse
	}
	return n, err
}

// FormatCapabilities reads and validates the daemon's per-format capability
// inventory. Selectors are passed through unchanged.
func (c *Connection) FormatCapabilities(
	ctx context.Context, family, format, extension string,
) (api.FormatCoverageResponse, error) {
	params := apiclient.ReadFormatCapabilitiesQuery{}
	if family != "" {
		params.Family = &family
	}
	if format != "" {
		params.Format = &format
	}
	if extension != "" {
		params.Extension = &extension
	}

	var apiResponseHTTP *http.Response
	_, err := c.apiWithResponse(&apiResponseHTTP).ReadFormatCapabilities(runtime.WithStreamingResponse(ctx), &apiclient.ReadFormatCapabilitiesRequestOptions{Query: &params})
	if err != nil {
		return api.FormatCoverageResponse{}, err
	}
	defer func() { _ = apiResponseHTTP.Body.Close() }()
	raw, err := io.ReadAll(apiResponseHTTP.Body)
	if err != nil {
		return api.FormatCoverageResponse{}, &responseDecodeError{err: err}
	}
	var transport struct {
		api.FormatCoverageResponse

		Schema string `json:"$schema,omitzero"`
	}
	if err := json.Unmarshal(raw, &transport, json.RejectUnknownMembers(true)); err != nil {
		return api.FormatCoverageResponse{}, &responseDecodeError{err: fmt.Errorf(
			"decoding GET /api/v1/formats/capabilities response: %w", err)}
	}
	response := transport.FormatCoverageResponse
	selector := format
	if selector == "" {
		selector = extension
	}
	if err := validateFormatCapabilitiesResponse(response, selector); err != nil {
		return api.FormatCoverageResponse{}, &responseDecodeError{err: fmt.Errorf(
			"validating GET /api/v1/formats/capabilities response: %w", err)}
	}
	return response, nil
}

func validateFormatCapabilitiesResponse(
	response api.FormatCoverageResponse, selector string,
) error {
	if err := document.ValidateFormatCoverageV1(response.FormatCoverageV1); err != nil {
		return err
	}
	if selector == "" {
		if response.Lookup != nil {
			return errors.New("format lookup is present without a selector")
		}
		return nil
	}
	if response.Lookup == nil {
		return errors.New("format lookup is missing for selector")
	}
	lookup := response.Lookup
	if lookup.Query != selector {
		return fmt.Errorf("format lookup query %q does not match selector %q", lookup.Query, selector)
	}
	check := response.FormatCoverageV1
	check.Formats = nil
	check.Pending = nil
	switch lookup.Match {
	case document.FormatLookupFormat:
		if lookup.Format == nil || lookup.Pending != nil {
			return errors.New("format lookup payload does not match format result")
		}
		check.Formats = []document.FormatCapabilityV1{*lookup.Format}
	case document.FormatLookupPending:
		if lookup.Pending == nil || lookup.Format != nil {
			return errors.New("format lookup payload does not match pending result")
		}
		check.Pending = []document.PendingFormatV1{*lookup.Pending}
	case document.FormatLookupUnknown:
		if lookup.Format != nil || lookup.Pending != nil {
			return errors.New("unknown format lookup carries a matched payload")
		}
	default:
		return fmt.Errorf("format lookup has unknown match %q", lookup.Match)
	}
	return document.ValidateFormatCoverageV1(check)
}

type BackupCreateOptions struct {
	Repo        string
	Tag         string
	Jobs        int
	ForceUnlock bool
}

// BackupCreateStream creates a snapshot while delivering structured progress
// events. A successful HTTP status only starts the stream; the method returns
// success only after receiving its terminal result event.
func (c *Connection) BackupCreateStream(
	ctx context.Context,
	opts BackupCreateOptions,
	progress func(api.BackupProgress),
) (api.BackupSnapshot, error) {
	var apiResponseHTTP *http.Response
	_, err := c.apiWithResponse(&apiResponseHTTP).StreamBackupSnapshotCreation(runtime.WithStreamingResponse(ctx), &apiclient.StreamBackupSnapshotCreationRequestOptions{Body: new(backupCreateRequest(opts))})
	if err != nil {
		return api.BackupSnapshot{}, err
	}
	resp := apiResponseHTTP
	defer func() { _ = resp.Body.Close() }()

	decoder := jsontext.NewDecoder(resp.Body)
	var result *api.BackupSnapshot
	for {
		var event api.BackupCreateEvent
		if err := json.UnmarshalDecode(decoder, &event); err != nil {
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

func backupCreateRequest(opts BackupCreateOptions) apiclient.BackupCreateRequest {
	return apiclient.BackupCreateRequest{Repo: new(opts.Repo), Tag: new(opts.Tag), Jobs: new(int64(opts.Jobs)), ForceUnlock: new(opts.ForceUnlock)}
}

type BackupVerifyOptions struct {
	Repo        string
	SnapshotID  string
	All         bool
	Quick       bool
	Jobs        int
	ForceUnlock bool
}

// BackupVerifyStream verifies a repository while delivering structured
// progress. A successful HTTP status only starts the stream; the method
// returns success only after receiving its terminal report event.
func (c *Connection) BackupVerifyStream(
	ctx context.Context,
	opts BackupVerifyOptions,
	progress func(api.BackupProgress),
) (api.BackupVerifyReport, error) {
	var apiResponseHTTP *http.Response
	_, err := c.apiWithResponse(&apiResponseHTTP).StreamBackupRepositoryVerification(runtime.WithStreamingResponse(ctx), &apiclient.StreamBackupRepositoryVerificationRequestOptions{Body: new(backupVerifyRequest(opts))})
	if err != nil {
		return api.BackupVerifyReport{}, err
	}
	resp := apiResponseHTTP
	defer func() { _ = resp.Body.Close() }()

	decoder := jsontext.NewDecoder(resp.Body)
	var result *api.BackupVerifyReport
	for {
		var event api.BackupVerifyEvent
		if err := json.UnmarshalDecode(decoder, &event); err != nil {
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

func backupVerifyRequest(opts BackupVerifyOptions) apiclient.BackupVerifyRequest {
	return apiclient.BackupVerifyRequest{Repo: new(opts.Repo), SnapshotID: new(opts.SnapshotID), All: new(opts.All), Quick: new(opts.Quick), Jobs: new(int64(opts.Jobs)), ForceUnlock: new(opts.ForceUnlock)}
}

type BackupRestoreOptions struct {
	Repo        string
	Target      string
	SnapshotID  string
	Overwrite   bool
	Jobs        int
	ForceUnlock bool
	StoreMap    string
}

// BackupRestoreStream restores and proves a snapshot while delivering
// structured progress. Success requires a terminal report event, not merely
// successful HTTP headers.
func (c *Connection) BackupRestoreStream(
	ctx context.Context,
	opts BackupRestoreOptions,
	progress func(api.BackupProgress),
) (api.BackupRestoreReport, error) {
	var apiResponseHTTP *http.Response
	_, err := c.apiWithResponse(&apiResponseHTTP).StreamBackupSnapshotRestore(runtime.WithStreamingResponse(ctx), &apiclient.StreamBackupSnapshotRestoreRequestOptions{Body: new(backupRestoreRequest(opts))})
	if err != nil {
		return api.BackupRestoreReport{}, err
	}
	resp := apiResponseHTTP
	defer func() { _ = resp.Body.Close() }()

	decoder := jsontext.NewDecoder(resp.Body)
	var result *api.BackupRestoreReport
	for {
		var event api.BackupRestoreEvent
		if err := json.UnmarshalDecode(decoder, &event); err != nil {
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

func backupRestoreRequest(opts BackupRestoreOptions) apiclient.BackupRestoreRequest {
	return apiclient.BackupRestoreRequest{Repo: new(opts.Repo), Target: opts.Target, SnapshotID: new(opts.SnapshotID), Overwrite: new(opts.Overwrite), Jobs: new(int64(opts.Jobs)), ForceUnlock: new(opts.ForceUnlock), StoreMap: new(opts.StoreMap)}
}

func (c *Connection) Shutdown(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.API().ShutdownDaemon(ctx, &apiclient.ShutdownDaemonRequestOptions{Header: &apiclient.ShutdownDaemonHeaders{XDocbankDaemonToken: token}})
	return err
}
