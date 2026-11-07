package daemonconn

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

// ErrNotePublicationUnavailable means this daemon cannot publish managed notes.
// Callers must retain managed semantics rather than falling back to archival push.
var ErrNotePublicationUnavailable = errors.New("daemon does not support managed note publication")

func notePublicationWireError(resp *http.Response, err error) error {
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return errors.Join(ErrNotePublicationUnavailable, err)
	}
	if err != nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &responseDecodeError{err: err}
	}
	return err
}
func badNoteResponse() error {
	return &responseDecodeError{err: errors.New("daemon returned an invalid note publication receipt")}
}
func validNoteScan(v api.NotePublicationScan) bool {
	return store.ValidatePushRunID(v.ScanID) == nil && store.ValidatePushRunID(v.DatabaseEpoch) == nil && validSHA256Hex(v.PolicyFingerprint) && v.Epoch > 0 && v.NamespaceRevision >= 0
}
func validNoteIdentity(v api.PublishedNoteIdentity, name string) bool {
	return v.Publication == name && store.ValidatePushRunID(v.VaultID) == nil && store.ValidatePushRef(v.SourceRef) == nil && v.NodeID > 0 && store.ValidatePushRunID(v.ContentVersionID) == nil && validSHA256Hex(v.Hash) && v.Size >= 0 && v.SourceRevision > 0 && v.NamespaceRevision > 0
}
func validNoteSource(v api.NotePublicationSource, name string) bool {
	if !validNoteIdentity(v.Identity, name) {
		return false
	}
	switch v.Presence {
	case "present", "absent", "unknown":
	default:
		return false
	}
	switch v.Availability {
	case "retained", "unavailable":
	default:
		return false
	}
	switch v.Conflict {
	case "", "diverged", "trashed", "unavailable":
	default:
		return false
	}
	if h := v.ArchiveHead; h != nil && (h.NodeID != v.Identity.NodeID || store.ValidatePushRunID(h.ContentVersionID) != nil || !validSHA256Hex(h.Hash) || h.Size < 0) {
		return false
	}
	if g := v.Recovery; g != nil && (v.Conflict != "diverged" || v.ArchiveHead == nil || g.SourceRef != v.Identity.SourceRef || g.SourceRevision != v.Identity.SourceRevision || g.ExpectedHeadVersionID != v.ArchiveHead.ContentVersionID) {
		return false
	}
	return true
}
func validNoteStatus(v api.NotePublicationStatus, name string) bool {
	if v.Name != name || store.ValidatePushRunID(v.DatabaseEpoch) != nil || v.NamespaceRevision < 0 {
		return false
	}
	for _, n := range []int64{v.Present, v.Absent, v.Unknown, v.Conflicts, v.Unavailable, v.RecoveryPending} {
		if n < 0 {
			return false
		}
	}
	for _, s := range []string{v.LastCompleteAt, v.LastAcceptedAt} {
		if s != "" {
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				return false
			}
		}
	}
	return v.ActiveScan == nil || (validNoteScan(*v.ActiveScan) && v.ActiveScan.DatabaseEpoch == v.DatabaseEpoch)
}
func validateNoteRoute(name, scan string) error {
	if err := store.ValidatePushName(name); err != nil {
		return err
	}
	return store.ValidatePushRunID(scan)
}

func (c *Connection) BeginNotePublicationScan(ctx context.Context, r api.NotePublicationBeginRequest) (api.NotePublicationScan, error) {
	var zero api.NotePublicationScan
	if err := validateNoteRoute(r.Name, r.OperationID); err != nil {
		return zero, err
	}
	if err := store.ValidatePushDestination(r.Destination); err != nil {
		return zero, err
	}
	if !validSHA256Hex(r.PolicyFingerprint) {
		return zero, errors.New("invalid publication policy fingerprint")
	}
	var resp *http.Response
	v, err := c.apiWithResponse(&resp).BeginNotePublicationScan(runtime.WithStreamingResponse(ctx), &apiclient.BeginNotePublicationScanRequestOptions{PathParams: &apiclient.BeginNotePublicationScanPath{Name: r.Name}, Body: &r})
	v, err = readNotePublicationResponse[api.NotePublicationScan](resp, err, 64<<10)
	if err != nil {
		return zero, err
	}
	if v == nil || !validNoteScan(*v) || v.PolicyFingerprint != r.PolicyFingerprint {
		return zero, badNoteResponse()
	}
	return *v, nil
}
func (c *Connection) NotePublicationSources(ctx context.Context, r api.NotePublicationSourcesRequest) (api.NotePublicationSourcesPage, error) {
	var zero api.NotePublicationSourcesPage
	if err := validateNoteRoute(r.Name, r.ScanID); err != nil {
		return zero, err
	}
	if r.Limit < 1 || r.Limit > 1000 || (r.After != "" && store.ValidatePushRef(r.After) != nil) {
		return zero, errors.New("invalid publication cursor request")
	}
	q := &apiclient.ListNotePublicationSourcesQuery{ScanID: r.ScanID, Limit: new(int64(r.Limit))}
	if r.After != "" {
		q.After = &r.After
	}
	var resp *http.Response
	v, err := c.apiWithResponse(&resp).ListNotePublicationSources(runtime.WithStreamingResponse(ctx), &apiclient.ListNotePublicationSourcesRequestOptions{PathParams: &apiclient.ListNotePublicationSourcesPath{Name: r.Name}, Query: q})
	v, err = readNotePublicationResponse[api.NotePublicationSourcesPage](resp, err, int64(r.Limit+1)*(64<<10))
	if err != nil {
		return zero, err
	}
	if v == nil || !validNoteScan(v.Scan) || v.Scan.ScanID != r.ScanID || len(v.Items) > r.Limit {
		return zero, badNoteResponse()
	}
	previous := r.After
	for _, source := range v.Items {
		if !validNoteSource(source, r.Name) || source.Identity.SourceRef <= previous {
			return zero, badNoteResponse()
		}
		previous = source.Identity.SourceRef
	}
	if v.Next != "" && (len(v.Items) == 0 || v.Next != previous) {
		return zero, badNoteResponse()
	}
	return *v, nil
}
func (c *Connection) UploadPublishedNote(ctx context.Context, r api.NotePublicationUploadRequest, body io.Reader) (api.NotePublicationReceipt, error) {
	var receipt api.NotePublicationReceipt
	if err := validateNoteRoute(r.Name, r.ScanID); err != nil {
		return receipt, err
	}
	if err := store.ValidatePushSource(store.PushSource{Name: r.Name, Ref: r.SourceRef, ModifiedAt: r.ModifiedAt}); err != nil {
		return receipt, err
	}
	if err := store.ValidatePushRunID(r.OperationID); err != nil {
		return receipt, err
	}
	if err := store.ValidatePushDestination(r.ParentPath); err != nil {
		return receipt, err
	}
	if r.ExpectedSourceRevision < 0 {
		return receipt, errors.New("invalid expected source revision")
	}
	mime, err := validateUploadRequest(r.FileName, "text/markdown", r.Hash, r.Size, body)
	if err != nil {
		return receipt, err
	}
	q := &apiclient.UploadPublishedNoteQuery{Name: r.FileName, ScanID: r.ScanID, SourceRef: r.SourceRef, ParentPath: r.ParentPath, OperationID: r.OperationID, ExpectedSourceRevision: r.ExpectedSourceRevision}
	if r.ModifiedAt != "" {
		q.ModifiedAt = &r.ModifiedAt
	}
	resp, err := streamUpload(ctx, r.FileName, mime, body, func(ctx context.Context, response **http.Response, editor runtime.RequestEditorFn) error {
		_, err := c.apiWithResponse(response).UploadPublishedNote(ctx, &apiclient.UploadPublishedNoteRequestOptions{PathParams: &apiclient.UploadPublishedNotePath{Name: r.Name}, Query: q, Header: &apiclient.UploadPublishedNoteHeaders{XDocbankBlobHash: r.Hash, XDocbankBlobSize: r.Size}}, editor)
		return notePublicationWireError(*response, err)
	})
	if err != nil {
		return receipt, err
	}
	value, err := readNotePublicationResponse[api.NotePublicationReceipt](resp, nil, 64<<10)
	if err != nil {
		return receipt, err
	}
	receipt = *value
	expectedStatus := http.StatusOK
	if receipt.Outcome == "added" {
		expectedStatus = http.StatusCreated
	}
	if !validNoteScan(receipt.Scan) || receipt.Scan.ScanID != r.ScanID || !validNoteSource(receipt.Source, r.Name) || receipt.Source.Identity.SourceRef != r.SourceRef || receipt.Source.Identity.Hash != r.Hash || receipt.Source.Identity.Size != r.Size || resp.StatusCode != expectedStatus {
		return api.NotePublicationReceipt{}, badNoteResponse()
	}
	switch receipt.Outcome {
	case "added", "updated", "skipped":
	default:
		return api.NotePublicationReceipt{}, badNoteResponse()
	}
	return receipt, nil
}
func (c *Connection) AcknowledgePublishedNotes(ctx context.Context, r api.NotePublicationSeenRequest) (api.NotePublicationAck, error) {
	var zero api.NotePublicationAck
	if err := validateNoteRoute(r.Name, r.ScanID); err != nil {
		return zero, err
	}
	if len(r.Bindings) < 1 || len(r.Bindings) > 200 {
		return zero, errors.New("invalid publication seen batch size")
	}
	seen := make(map[string]bool, len(r.Bindings))
	for _, v := range r.Bindings {
		if !validNoteIdentity(v, r.Name) || seen[v.SourceRef] {
			return zero, errors.New("invalid publication seen identity")
		}
		seen[v.SourceRef] = true
	}
	// Count the complete JSON envelope before transport allocates its body.
	budget := notePublicationBodyBudget{remaining: 1 << 20}
	if err := json.MarshalWrite(&budget, r); err != nil {
		return zero, err
	}
	var resp *http.Response
	v, err := c.apiWithResponse(&resp).AcknowledgePublishedNotes(runtime.WithStreamingResponse(ctx), &apiclient.AcknowledgePublishedNotesRequestOptions{PathParams: &apiclient.AcknowledgePublishedNotesPath{Name: r.Name, ScanID: r.ScanID}, Body: &r})
	v, err = readNotePublicationResponse[api.NotePublicationAck](resp, err, 2<<20)
	if err != nil {
		return zero, err
	}
	if v == nil || !validNoteScan(v.Scan) || v.Scan.ScanID != r.ScanID || len(v.Sources) != len(r.Bindings) {
		return zero, badNoteResponse()
	}
	for i, binding := range v.Sources {
		if !validNoteAcknowledgement(binding, r.Bindings[i], r.Name) {
			return zero, badNoteResponse()
		}
	}
	return *v, nil
}
func (c *Connection) CompleteNotePublicationScan(ctx context.Context, r api.NotePublicationCompleteRequest) (api.NotePublicationStatus, error) {
	var zero api.NotePublicationStatus
	if err := validateNoteRoute(r.Name, r.ScanID); err != nil {
		return zero, err
	}
	if !validSHA256Hex(r.PolicyFingerprint) {
		return zero, errors.New("invalid publication policy fingerprint")
	}
	var resp *http.Response
	v, err := c.apiWithResponse(&resp).CompleteNotePublicationScan(runtime.WithStreamingResponse(ctx), &apiclient.CompleteNotePublicationScanRequestOptions{PathParams: &apiclient.CompleteNotePublicationScanPath{Name: r.Name, ScanID: r.ScanID}, Body: &r})
	v, err = readNotePublicationResponse[api.NotePublicationStatus](resp, err, 64<<10)
	if err != nil {
		return zero, err
	}
	if v == nil || !validNoteStatus(*v, r.Name) {
		return zero, badNoteResponse()
	}
	return *v, nil
}
func (c *Connection) NotePublicationStatus(ctx context.Context, name string) (api.NotePublicationStatus, error) {
	var zero api.NotePublicationStatus
	if err := store.ValidatePushName(name); err != nil {
		return zero, err
	}
	var resp *http.Response
	v, err := c.apiWithResponse(&resp).GetNotePublicationStatus(runtime.WithStreamingResponse(ctx), &apiclient.GetNotePublicationStatusRequestOptions{PathParams: &apiclient.GetNotePublicationStatusPath{Name: name}})
	v, err = readNotePublicationResponse[api.NotePublicationStatus](resp, err, 64<<10)
	if err != nil {
		return zero, err
	}
	if v == nil || !validNoteStatus(*v, name) {
		return zero, badNoteResponse()
	}
	return *v, nil
}

// Seen may revive absent membership, advancing its source revision by one.
// Unrelated accepted sources can also advance the namespace revision.
func validNoteAcknowledgement(got, expected api.PublishedNoteIdentity, name string) bool {
	if !validNoteIdentity(got, name) || got.NamespaceRevision < expected.NamespaceRevision || got.SourceRevision < expected.SourceRevision || got.SourceRevision-expected.SourceRevision > 1 {
		return false
	}
	got.NamespaceRevision = expected.NamespaceRevision
	got.SourceRevision = expected.SourceRevision
	return got == expected
}

// Generated route declarations still own request construction. A bounded owned
// stream prevents a malformed success from allocating an unbounded cursor page.
func readNotePublicationResponse[T any](resp *http.Response, callErr error, limit int64) (*T, error) {
	if err := notePublicationWireError(resp, callErr); err != nil {
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		return nil, badNoteResponse()
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPushResponseInterrupted, err)
	}
	if int64(len(b)) > limit {
		return nil, badNoteResponse()
	}
	value := new(T)
	if err := json.Unmarshal(b, value); err != nil {
		return nil, &responseDecodeError{err: err}
	}
	return value, nil
}

type notePublicationBodyBudget struct{ remaining int }

func (w *notePublicationBodyBudget) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		return 0, errors.New("publication seen batch exceeds 1 MiB")
	}
	w.remaining -= len(b)
	return len(b), nil
}
