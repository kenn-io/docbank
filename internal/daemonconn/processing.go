package daemonconn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/apiclient"
	"io"
	"math"
	"mime"
	"net/http"
	pathpkg "path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

var (
	ErrProcessingUnavailable = errors.New("document processing is unavailable")
	ErrProcessingPlanChanged = errors.New("document processing plan changed")
	ErrProcessingConsent     = errors.New("document processing consent is required")
)

// SourceFenceScopeTooLargeError preserves the daemon's full observed source
// population when an exact bounded fence cannot be returned.
type SourceFenceScopeTooLargeError struct {
	ObservedScopeCount int
	detail             string
}

func (e *SourceFenceScopeTooLargeError) Error() string {
	if e.detail != "" {
		return e.detail
	}
	return fmt.Sprintf("source scope contains %d current live content versions", e.ObservedScopeCount)
}

const (
	maxProcessingEventStreamBytes int64 = 64 << 10
	maxRenditionResponseBytes     int64 = 64 << 20
	maxDocumentSearchExcerptRunes       = 512
	maxDocumentSearchExcerptBytes       = 4 * maxDocumentSearchExcerptRunes
)

var errProcessingStreamTooLarge = errors.New("processing stream is too large")

// ResolveDocumentSourceFence captures exact current/live search authority in the daemon.
func (c *Connection) ResolveDocumentSourceFence(
	ctx context.Context, request api.DocumentSourceFenceResolveRequest,
) (api.DocumentSourceFenceResolution, error) {
	normalized, err := validateDocumentSourceFenceRequest(request)
	if err != nil {
		return api.DocumentSourceFenceResolution{}, fmt.Errorf("source fence request is invalid: %w", err)
	}
	var result api.DocumentSourceFenceResolution
	apiResponse, err := c.API().ResolveDocumentSourceFence(ctx, &apiclient.ResolveDocumentSourceFenceRequestOptions{Body: &normalized})
	if err != nil {
		return api.DocumentSourceFenceResolution{}, err
	}
	result = *apiResponse
	if err := validateDocumentSourceFenceResolution(normalized, result); err != nil {
		return api.DocumentSourceFenceResolution{}, fmt.Errorf("source fence response is invalid: %w", err)
	}
	return result, nil
}

func validateDocumentSourceFenceRequest(
	request api.DocumentSourceFenceResolveRequest,
) (api.DocumentSourceFenceResolveRequest, error) {
	explicit := len(request.ContentVersionIDs) != 0
	if explicit == (request.Filters != nil) {
		return api.DocumentSourceFenceResolveRequest{}, errors.New("select exactly one request mode")
	}
	if explicit {
		if len(request.ContentVersionIDs) > processing.MaxSourceFenceIDs {
			return api.DocumentSourceFenceResolveRequest{}, fmt.Errorf(
				"content version IDs exceed %d", processing.MaxSourceFenceIDs)
		}
		ids := slices.Clone(request.ContentVersionIDs)
		for _, id := range ids {
			if !validUUIDv4(id) {
				return api.DocumentSourceFenceResolveRequest{}, errors.New("content version ID is invalid")
			}
		}
		slices.Sort(ids)
		for index, id := range ids {
			if index > 0 && ids[index-1] == id {
				return api.DocumentSourceFenceResolveRequest{}, errors.New("content version IDs must be unique")
			}
		}
		request.ContentVersionIDs = ids
		return request, nil
	}
	filters := *request.Filters
	if filters.TagID != "" && !validUUIDv4(filters.TagID) {
		return api.DocumentSourceFenceResolveRequest{}, errors.New("tag ID is invalid")
	}
	if filters.UnderNodeID < 0 {
		return api.DocumentSourceFenceResolveRequest{}, errors.New("directory node ID must be positive")
	}
	var err error
	filters.MIMEType, err = store.NormalizeSearchMIMEType(filters.MIMEType)
	if err != nil {
		return api.DocumentSourceFenceResolveRequest{}, err
	}
	filters.ModifiedSince, filters.ModifiedBefore, err = store.NormalizeSearchTimeBounds(
		filters.ModifiedSince, filters.ModifiedBefore)
	if err != nil {
		return api.DocumentSourceFenceResolveRequest{}, err
	}
	request.Filters = &filters
	return request, nil
}

func validateDocumentSourceFenceResolution(
	request api.DocumentSourceFenceResolveRequest, result api.DocumentSourceFenceResolution,
) error {
	ids := result.Fence.ContentVersionIDs
	if ids == nil {
		return errors.New("content version IDs must be a non-null array")
	}
	if !validUUIDv4(result.Fence.VaultUID) || len(ids) > processing.MaxSourceFenceIDs ||
		result.ObservedScopeCount != len(ids) {
		return errors.New("fence authority is inconsistent")
	}
	for index, id := range ids {
		if !validUUIDv4(id) || (index > 0 && ids[index-1] >= id) {
			return errors.New("content version IDs are not sorted and unique")
		}
	}
	if len(request.ContentVersionIDs) != 0 {
		expected := slices.Clone(request.ContentVersionIDs)
		slices.Sort(expected)
		if !slices.Equal(expected, ids) {
			return errors.New("explicit source authority changed")
		}
	}
	fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{
		VaultUID: result.Fence.VaultUID, ContentVersionIDs: ids,
	})
	if err != nil || result.FenceFingerprint != fingerprint {
		return errors.New("fence fingerprint does not bind its authority")
	}
	return nil
}

// StartProcessing returns the durable job alongside any later execution or stream
// error, so callers can query its status without submitting another job.
func (c *Connection) StartProcessing(ctx context.Context, request api.StartProcessingRequest, profileFingerprint string) (api.ProcessingJob, error) {
	stream, err := c.StartProcessingStream(ctx, request, profileFingerprint)
	if err != nil {
		return api.ProcessingJob{}, err
	}
	defer func() { _ = stream.Close() }()
	first, err := stream.Next()
	if err != nil {
		return api.ProcessingJob{}, &responseDecodeError{err: err}
	}
	terminal, err := stream.Next()
	if terminal.Job != nil {
		first.Job = terminal.Job
	} else if terminal.Status != nil {
		first.Job.EmbeddingJobIDs = terminal.Status.EmbeddingJobIDs
	}
	return *first.Job, err
}

// EnqueueProcessing returns as soon as the daemon publishes the durable job
// identity. Closing the response stream does not cancel work that the daemon
// has already enqueued under its own lifecycle.
func (c *Connection) EnqueueProcessing(ctx context.Context, request api.StartProcessingRequest, profileFingerprint string) (api.ProcessingJob, error) {
	stream, err := c.StartProcessingStream(ctx, request, profileFingerprint)
	if err != nil {
		return api.ProcessingJob{}, err
	}
	defer func() { _ = stream.Close() }()
	first, err := stream.Next()
	if err != nil {
		return api.ProcessingJob{}, &responseDecodeError{err: err}
	}
	return *first.Job, nil
}

// ProcessingEventStream incrementally validates the exact two-event processing
// stream. The durable job is returned before terminal delivery; the terminal
// event is returned only after end-of-stream has been verified.
type ProcessingEventStream struct {
	body               io.ReadCloser
	decoder            *jsontext.Decoder
	bounded            *boundedReadCloser
	sequence           int
	jobID              string
	contentVersionID   string
	profileFingerprint string
	done               bool
}

// StartProcessingStream opens one cancellable processing progress stream.
// profileFingerprint is the profile fingerprint from the reviewed plan.
func (c *Connection) StartProcessingStream(ctx context.Context,
	request api.StartProcessingRequest, profileFingerprint string,
) (*ProcessingEventStream, error) {
	var apiResponseHTTP *http.Response
	_, err := c.apiWithResponse(&apiResponseHTTP).StartDocumentProcessing(runtime.WithStreamingResponse(ctx), &apiclient.StartDocumentProcessingRequestOptions{Body: new(request)})
	if err != nil {
		return nil, err
	}
	resp := apiResponseHTTP

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-ndjson" {
		_ = resp.Body.Close()
		return nil, &responseDecodeError{err: errors.New("processing stream returned an invalid content type")}
	}
	bounded := &boundedReadCloser{body: resp.Body, remaining: maxProcessingEventStreamBytes}
	return &ProcessingEventStream{body: bounded, decoder: jsontext.NewDecoder(bounded), bounded: bounded,
		contentVersionID: request.Selector.ContentVersionID, profileFingerprint: profileFingerprint}, nil
}

// Next returns the next strictly validated processing event.
func (stream *ProcessingEventStream) Next() (api.ProcessingJobEvent, error) {
	if stream == nil {
		return api.ProcessingJobEvent{}, errors.New("processing stream is unavailable")
	}
	if stream.done {
		return api.ProcessingJobEvent{}, io.EOF
	}
	if stream.body == nil || stream.decoder == nil {
		return api.ProcessingJobEvent{}, errors.New("processing stream is unavailable")
	}
	var event api.ProcessingJobEvent
	if err := json.UnmarshalDecode(stream.decoder, &event, json.RejectUnknownMembers(true)); err != nil {
		_ = stream.Close()
		if stream.bounded.exceeded {
			return api.ProcessingJobEvent{}, errProcessingStreamTooLarge
		}
		if stream.sequence == 0 {
			return api.ProcessingJobEvent{}, fmt.Errorf("decoding processing job event: %w", err)
		}
		return api.ProcessingJobEvent{}, fmt.Errorf("decoding processing status event: %w", err)
	}
	if stream.bounded.exceeded {
		_ = stream.Close()
		return api.ProcessingJobEvent{}, errProcessingStreamTooLarge
	}
	if stream.sequence == 0 {
		if event.Sequence != 1 || event.Type != "job" || event.Job == nil || event.Status != nil || event.Error != nil || event.Terminal ||
			!stream.validJob(*event.Job) {
			_ = stream.Close()
			return api.ProcessingJobEvent{}, errors.New("processing stream returned malformed job event")
		}
		stream.sequence, stream.jobID = 1, event.Job.ID
		return event, nil
	}
	if event.Sequence != 2 || !event.Terminal {
		_ = stream.Close()
		return api.ProcessingJobEvent{}, errors.New("processing stream returned malformed terminal event")
	}
	switch event.Type {
	case "status":
		if event.Job != nil || event.Status == nil || event.Error != nil || event.Status.JobID != stream.jobID ||
			!validProcessingStatus(*event.Status) {
			_ = stream.Close()
			return api.ProcessingJobEvent{}, errors.New("processing stream returned malformed terminal status")
		}
	case "error":
		if event.Job == nil || event.Status != nil || event.Error == nil || event.Job.ID != stream.jobID ||
			!stream.validJob(*event.Job) {
			_ = stream.Close()
			return api.ProcessingJobEvent{}, errors.New("processing stream returned malformed terminal error")
		}
	default:
		_ = stream.Close()
		return api.ProcessingJobEvent{}, errors.New("processing stream returned an unknown terminal event")
	}
	var extra api.ProcessingJobEvent
	if err := json.UnmarshalDecode(stream.decoder, &extra, json.RejectUnknownMembers(true)); !errors.Is(err, io.EOF) {
		_ = stream.Close()
		if stream.bounded.exceeded {
			return api.ProcessingJobEvent{}, errProcessingStreamTooLarge
		}
		return api.ProcessingJobEvent{}, errors.New("processing stream continued after its terminal status")
	}
	stream.sequence, stream.done = 2, true
	if err := stream.Close(); err != nil {
		return event, fmt.Errorf("closing processing stream: %w", err)
	}
	if event.Error != nil {
		return event, apiProblemError(*event.Error)
	}
	switch event.Status.State {
	case "failed", "abandoned", "operator_required":
		cause := codeToTypedErr[event.Status.FailureCode]
		if cause == nil {
			cause = fmt.Errorf("document processing %s: %s", event.Status.State, event.Status.FailureCode)
		}
		return event, &problemError{code: event.Status.FailureCode, err: cause}
	}
	return event, nil
}

func (stream *ProcessingEventStream) validJob(job api.ProcessingJob) bool {
	return validSHA256Hex(job.ID) && validUUIDv4(job.ContentVersionID) &&
		job.ContentVersionID == stream.contentVersionID &&
		validSHA256Hex(job.ProfileFingerprint) && job.ProfileFingerprint == stream.profileFingerprint &&
		(job.RenditionJobID == "" || validSHA256Hex(job.RenditionJobID)) &&
		(job.AttachmentID == "" || validSHA256Hex(job.AttachmentID)) &&
		validProcessingEmbeddingIDs(job.EmbeddingJobIDs)
}

func validProcessingEmbeddingIDs(ids []string) bool {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !validSHA256Hex(id) {
			return false
		}
		if _, duplicate := seen[id]; duplicate {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

// Stream and standalone status responses use the same aggregate contract.
func validProcessingStatus(status api.ProcessingStatus) bool {
	if !validSHA256Hex(status.JobID) || !validProcessingEmbeddingIDs(status.EmbeddingJobIDs) ||
		!validBoundedSearchIdentity(status.Phase, 128) || strings.TrimSpace(status.Phase) == "" ||
		status.CompletedBindings < 0 || status.CompletedBindings > len(status.EmbeddingJobIDs) ||
		(status.FailureCode != "" && (!validBoundedSearchIdentity(status.FailureCode, 128) || strings.TrimSpace(status.FailureCode) == "")) {
		return false
	}
	switch status.State {
	case "completed":
		return status.FailureCode == "" && status.CompletedBindings == len(status.EmbeddingJobIDs)
	case "failed", "operator_required", "retry_wait":
		return status.FailureCode != ""
	case "queued", "running", "abandoned", "partial":
		// A reclaimed embedding job can retain its previous failure code;
		// abandoning obsolete work need not publish a failure at all.
		return true
	default:
		return false
	}
}

type boundedReadCloser struct {
	body      io.ReadCloser
	remaining int64
	exceeded  bool
}

func (reader *boundedReadCloser) Read(p []byte) (int, error) {
	if reader.exceeded {
		return 0, errProcessingStreamTooLarge
	}
	if int64(len(p)) > reader.remaining+1 {
		p = p[:reader.remaining+1]
	}
	n, err := reader.body.Read(p)
	reader.remaining -= int64(n)
	if reader.remaining < 0 {
		reader.exceeded = true
		return n, errProcessingStreamTooLarge
	}
	return n, err
}

func (reader *boundedReadCloser) Close() error { return reader.body.Close() }

// Close cancels further reads and releases the response body.
func (stream *ProcessingEventStream) Close() error {
	if stream == nil || stream.body == nil {
		return nil
	}
	body := stream.body
	stream.body = nil
	return body.Close()
}

// RunDerivativePurge returns the committed catalog receipt even when physical
// cleanup fails. A non-nil error with a receipt means cleanup remains pending.
func (c *Connection) RunDerivativePurge(ctx context.Context,
	request api.DerivativePurgeJobRequest,
) (api.DerivativePurgeReceipt, error) {
	var apiResponseHTTP *http.Response
	_, err := c.apiWithResponse(&apiResponseHTTP).RunDerivativePurge(runtime.WithStreamingResponse(ctx), &apiclient.RunDerivativePurgeRequestOptions{Body: new(request)})
	if err != nil {
		return api.DerivativePurgeReceipt{}, err
	}
	resp := apiResponseHTTP
	defer func() { _ = resp.Body.Close() }()

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-ndjson" {
		return api.DerivativePurgeReceipt{}, errors.New("derivative purge stream returned an invalid content type")
	}
	decoder := jsontext.NewDecoder(resp.Body)
	var event api.DerivativePurgeEvent
	if err := json.UnmarshalDecode(decoder, &event, json.RejectUnknownMembers(true)); err != nil {
		return api.DerivativePurgeReceipt{}, fmt.Errorf("decoding derivative purge receipt: %w", err)
	}
	if event.Sequence != 1 || event.Type != "result" || event.Receipt == nil || !event.Terminal {
		return api.DerivativePurgeReceipt{}, errors.New("derivative purge stream returned malformed terminal receipt")
	}
	var extra api.DerivativePurgeEvent
	if err := json.UnmarshalDecode(decoder, &extra, json.RejectUnknownMembers(true)); !errors.Is(err, io.EOF) {
		return api.DerivativePurgeReceipt{}, errors.New("derivative purge stream continued after its terminal receipt")
	}
	if event.Error != nil {
		return *event.Receipt, apiProblemError(*event.Error)
	}
	return *event.Receipt, nil
}

func (c *Connection) ProcessingStatus(ctx context.Context, jobID string) (api.ProcessingStatus, error) {
	if !validSHA256Hex(jobID) {
		return api.ProcessingStatus{}, errors.New("processing job ID must be lowercase SHA-256")
	}
	var result api.ProcessingStatus
	apiResponse, err := c.API().GetDocumentProcessingJob(ctx, &apiclient.GetDocumentProcessingJobRequestOptions{PathParams: &apiclient.GetDocumentProcessingJobPath{ID: jobID}})
	if err != nil {
		return api.ProcessingStatus{}, err
	}
	result = *apiResponse
	if result.JobID != jobID || !validProcessingStatus(result) {
		return api.ProcessingStatus{}, errors.New("processing response returned malformed status")
	}
	return result, nil
}

func (c *Connection) SearchDocuments(ctx context.Context, request api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
	var result api.DocumentSearchReport
	apiResponse, err := c.API().SearchDocuments(ctx, &apiclient.SearchDocumentsRequestOptions{Body: &request})
	if err != nil {
		return api.DocumentSearchReport{}, err
	}
	result = *apiResponse
	if err := validateDocumentSearchReport(request, result); err != nil {
		return api.DocumentSearchReport{}, fmt.Errorf("search response is invalid: %w", err)
	}
	return result, nil
}

func (c *Connection) SimilarDocuments(ctx context.Context, request api.DocumentSimilarRequest) (api.DocumentSimilarReport, error) {
	report, err := c.API().FindSimilarDocuments(ctx, &apiclient.FindSimilarDocumentsRequestOptions{Body: &request})
	if err != nil {
		return api.DocumentSimilarReport{}, err
	}
	if err := validateDocumentSimilarReport(request, *report); err != nil {
		return api.DocumentSimilarReport{}, fmt.Errorf("similar response is invalid: %w", err)
	}
	return *report, nil
}

func validateDocumentSimilarReport(request api.DocumentSimilarRequest, report api.DocumentSimilarReport) error {
	if report.Source.NodeID != request.Selector.NodeID || report.Source.NodeID < 1 ||
		report.Source.ContentVersionID != request.Selector.ContentVersionID || !slices.Contains(request.Fence.ContentVersionIDs, report.Source.ContentVersionID) ||
		!validBoundedSearchIdentity(report.BindingID, 128) || request.BindingID != "" && report.BindingID != request.BindingID {
		return errors.New("similar source or binding identity is inconsistent")
	}
	switch report.State {
	case "ready":
		if report.MissingCoverage != nil || report.Coverage.State == "unknown" {
			return errors.New("ready similar report has missing coverage")
		}
	case "unavailable":
		missing := report.MissingCoverage
		if missing == nil || missing.Kind != "embedding" || missing.BindingID != report.BindingID ||
			missing.ContentVersionID != report.Source.ContentVersionID || !validSHA256Hex(missing.ProfileFingerprint) ||
			len(report.Results) != 0 || report.Truncated || report.Coverage.State != "unknown" ||
			report.Coverage.CompleteDocuments != 0 || report.Coverage.ScopedDocuments != 0 {
			return errors.New("unavailable similar report is inconsistent")
		}
	default:
		return errors.New("similar state is invalid")
	}
	converted := api.DocumentSearchReport{RequestedMode: "semantic", ActualMode: "semantic", Coverage: report.Coverage}
	hashes, nodes := make(map[string]bool), make(map[int64]bool)
	for i, result := range report.Results {
		if result.NodeID == report.Source.NodeID || nodes[result.NodeID] || !validSHA256Hex(result.BlobHash) || hashes[result.BlobHash] ||
			result.DuplicateCount < 0 || result.DuplicateCount >= report.Coverage.CompleteDocuments ||
			i > 0 && result.Score > report.Results[i-1].Score || len(result.Evidence) != 1 || result.Evidence[0].Kind != "embedding" || result.Evidence[0].TimeSpan != nil {
			return errors.New("similar result grouping or score is inconsistent")
		}
		hashes[result.BlobHash], nodes[result.NodeID] = true, true
		converted.Results = append(converted.Results, api.DocumentSearchResult{VaultUID: result.VaultUID, NodeID: result.NodeID,
			ContentVersionID: result.ContentVersionID, Rank: result.Rank, SemanticRank: result.Rank, Score: result.Score, Path: result.Path, Evidence: result.Evidence})
	}
	return validateDocumentSearchReport(api.DocumentSearchRequest{Mode: "semantic", Limit: request.Limit, Fence: request.Fence}, converted)
}

// ValidateDocumentSearch asks the daemon to apply ordinary search semantics
// without executing a search. It is used only for an exact empty source fence.
func (c *Connection) ValidateDocumentSearch(
	ctx context.Context, request api.DocumentSearchValidationRequest,
) error {
	var result api.DocumentSearchValidation
	apiResponse, err := c.API().ValidateDocumentSearch(ctx, &apiclient.ValidateDocumentSearchRequestOptions{Body: &request})
	if err != nil {
		return err
	}
	result = *apiResponse
	if !result.Valid {
		return errors.New("search validation response is invalid")
	}
	return nil
}

func validateDocumentSearchReport(request api.DocumentSearchRequest, report api.DocumentSearchReport) error {
	if !validUUIDv4(request.Fence.VaultUID) || len(request.Fence.ContentVersionIDs) < 1 ||
		len(request.Fence.ContentVersionIDs) > 4096 {
		return errors.New("request fence identity is invalid")
	}
	versions := make(map[string]struct{}, len(request.Fence.ContentVersionIDs))
	for _, versionID := range request.Fence.ContentVersionIDs {
		if !validUUIDv4(versionID) {
			return errors.New("request fence version identity is invalid")
		}
		if _, duplicate := versions[versionID]; duplicate {
			return errors.New("request fence contains duplicate versions")
		}
		versions[versionID] = struct{}{}
	}
	requestedMode := request.Mode
	if requestedMode == "" {
		requestedMode = "auto"
	}
	if report.RequestedMode != requestedMode || !validDocumentSearchActualMode(requestedMode, report.ActualMode) {
		return errors.New("retrieval mode authority is inconsistent")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 || len(report.Results) > limit {
		return errors.New("result count exceeds the requested bound")
	}
	if report.Coverage.ScopedDocuments < 0 || report.Coverage.ScopedDocuments > len(versions) ||
		report.Coverage.CompleteDocuments < 0 ||
		report.Coverage.CompleteDocuments > report.Coverage.ScopedDocuments ||
		(report.Coverage.State != "unknown" && report.Coverage.State != "complete" &&
			report.Coverage.State != "incomplete") ||
		report.Coverage.State == "complete" &&
			report.Coverage.CompleteDocuments != report.Coverage.ScopedDocuments {
		return errors.New("coverage authority is inconsistent")
	}
	for _, degradation := range report.Degradations {
		if !validBoundedSearchIdentity(degradation, 128) {
			return errors.New("degradation identity is invalid")
		}
	}
	if !request.Explain && len(report.Trace) != 0 {
		return errors.New("unrequested retrieval trace was returned")
	}
	for _, event := range report.Trace {
		if !validBoundedSearchIdentity(event.Code, 128) || event.Count < 0 {
			return errors.New("retrieval trace is invalid")
		}
	}
	seenDocuments := make(map[string]struct{}, len(report.Results))
	seenLexicalRanks := make(map[int]struct{}, len(report.Results))
	seenSemanticRanks := make(map[int]struct{}, len(report.Results))
	for index, result := range report.Results {
		if result.VaultUID != request.Fence.VaultUID || !validUUIDv4(result.VaultUID) {
			return fmt.Errorf("result %d escaped the vault fence", index)
		}
		if _, allowed := versions[result.ContentVersionID]; !allowed || !validUUIDv4(result.ContentVersionID) {
			return fmt.Errorf("result %d escaped the content-version fence", index)
		}
		if result.NodeID < 1 || result.Rank != index+1 || math.IsNaN(result.Score) || math.IsInf(result.Score, 0) {
			return fmt.Errorf("result %d has invalid rank or document identity", index)
		}
		if !validDocumentSearchPath(result.Path) || !validDocumentSearchExcerpt(result.Excerpt) {
			return fmt.Errorf("result %d has invalid path or excerpt", index)
		}
		if _, duplicate := seenDocuments[result.ContentVersionID]; duplicate {
			return fmt.Errorf("result %d duplicates a document identity", index)
		}
		seenDocuments[result.ContentVersionID] = struct{}{}
		if err := validateDocumentLaneRanks(report.ActualMode, result, seenLexicalRanks, seenSemanticRanks); err != nil {
			return fmt.Errorf("result %d: %w", index, err)
		}
		if len(result.Evidence) < 1 || len(result.Evidence) > 32 {
			return fmt.Errorf("result %d has invalid evidence count", index)
		}
		type evidenceIdentity struct {
			reference api.DocumentEvidenceReference
			span      api.MediaTimeSpan
		}
		seenEvidence := make(map[evidenceIdentity]struct{}, len(result.Evidence))
		for evidenceIndex, evidence := range result.Evidence {
			identity := evidenceIdentity{reference: evidence}
			if evidence.TimeSpan != nil {
				identity.span = *evidence.TimeSpan
				identity.reference.TimeSpan = nil
			}
			if _, duplicate := seenEvidence[identity]; duplicate {
				return fmt.Errorf("result %d has duplicate evidence", index)
			}
			seenEvidence[identity] = struct{}{}
			if err := validateDocumentEvidenceIdentity(evidence); err != nil {
				return fmt.Errorf("result %d evidence %d: %w", index, evidenceIndex, err)
			}
		}
	}
	return nil
}

func validDocumentSearchPath(value string) bool {
	return len(value) >= 2 && utf8.ValidString(value) &&
		strings.HasPrefix(value, "/") && pathpkg.Clean(value) == value
}

func validDocumentSearchExcerpt(value string) bool {
	return utf8.ValidString(value) && len(value) <= maxDocumentSearchExcerptBytes &&
		utf8.RuneCountInString(value) <= maxDocumentSearchExcerptRunes
}

func validDocumentSearchActualMode(requested, actual string) bool {
	if actual != "lexical" && actual != "semantic" && actual != "hybrid" {
		return false
	}
	return requested == "auto" || requested == actual
}

func validateDocumentLaneRanks(actualMode string, result api.DocumentSearchResult,
	seenLexical, seenSemantic map[int]struct{},
) error {
	if result.LexicalRank < 0 || result.LexicalRank > document.MaxRetrievalCandidateLimit ||
		result.SemanticRank < 0 || result.SemanticRank > document.MaxRetrievalCandidateLimit {
		return errors.New("lane rank is outside its bound")
	}
	if actualMode == "lexical" && (result.LexicalRank == 0 || result.SemanticRank != 0) ||
		actualMode == "semantic" && (result.SemanticRank == 0 || result.LexicalRank != 0) ||
		actualMode == "hybrid" && result.LexicalRank == 0 && result.SemanticRank == 0 {
		return errors.New("lane ranks are inconsistent with the actual mode")
	}
	for _, lane := range []struct {
		rank int
		seen map[int]struct{}
	}{{result.LexicalRank, seenLexical}, {result.SemanticRank, seenSemantic}} {
		rank, seen := lane.rank, lane.seen
		if rank == 0 {
			continue
		}
		if _, duplicate := seen[rank]; duplicate {
			return errors.New("lane rank is duplicated")
		}
		seen[rank] = struct{}{}
	}
	return nil
}

func validateDocumentEvidenceIdentity(evidence api.DocumentEvidenceReference) error {
	if evidence.TimeSpan != nil && (evidence.TimeSpan.StartMS < 0 || evidence.TimeSpan.EndMS <= evidence.TimeSpan.StartMS) {
		return errors.New("media evidence interval is invalid")
	}
	embeddingEmpty := evidence.VectorSpaceID == "" && evidence.EmbeddingSetID == "" &&
		evidence.InputGenerationID == "" && evidence.InputID == "" && evidence.InputKind == "" &&
		evidence.SourceManifestChecksum == ""
	renditionEmpty := evidence.BuildID == "" && evidence.SegmentID == ""
	switch evidence.Kind {
	case "node_name", "content_blob":
		if !embeddingEmpty || !renditionEmpty || evidence.TimeSpan != nil {
			return errors.New("metadata evidence carries derivative identity")
		}
	case "rendition_segment":
		if !embeddingEmpty || !validSHA256Hex(evidence.BuildID) ||
			!validBoundedSearchIdentity(evidence.SegmentID, 1024) {
			return errors.New("rendition evidence identity is incomplete or inconsistent")
		}
	case "embedding":
		if evidence.SegmentID != "" || !validSHA256Hex(evidence.VectorSpaceID) ||
			!validSHA256Hex(evidence.EmbeddingSetID) || !validSHA256Hex(evidence.InputGenerationID) ||
			!validBoundedSearchIdentity(evidence.InputID, 1024) ||
			(evidence.InputKind != "rendition_chunk" && evidence.InputKind != "original_file") ||
			(evidence.InputKind == "rendition_chunk" && !validSHA256Hex(evidence.BuildID)) ||
			(evidence.InputKind == "original_file" && (evidence.BuildID != "" || evidence.TimeSpan != nil)) ||
			!validSHA256Hex(evidence.SourceManifestChecksum) {
			return errors.New("embedding evidence identity is incomplete or inconsistent")
		}
	default:
		return errors.New("evidence kind is unrecognized")
	}
	return nil
}

func validBoundedSearchIdentity(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value)
}

// RenditionStream carries the immutable rendition and source identities that
// must agree with the complete verified Markdown transfer.
type RenditionStream struct {
	*ContentStream

	AttachmentID       string
	BuildID            string
	ArtifactID         string
	ProfileFingerprint string
	Completeness       string
	Warnings           []string
	FrontMatter        document.RenditionFrontMatterV1
	maxBytes           int64
}

// RenditionRangeStream is a verified byte range of the complete immutable
// rendition. Start and End are whole-artifact offsets; frontmatter navigation
// offsets remain relative to the Markdown body.
type RenditionRangeStream struct {
	io.ReadCloser

	AttachmentID       string
	BuildID            string
	ArtifactID         string
	VersionID          string
	BlobHash           string
	ProfileFingerprint string
	Completeness       string
	Warnings           []string
	Start              int64
	End                int64
	TotalSize          int64
	trailer            http.Header
}

func (stream *RenditionRangeStream) CopyVerified(w io.Writer) (int64, error) {
	if stream == nil || stream.ReadCloser == nil || w == nil {
		return 0, errors.New("copying rendition range: nil stream or destination")
	}
	expected := stream.End - stream.Start
	if expected < 1 || expected == math.MaxInt64 {
		_ = stream.Close()
		return 0, integrityErrorf("verifying rendition range: invalid bounded size %d", expected)
	}
	var buffered bytes.Buffer
	buffered.Grow(int(expected))
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(&buffered, hash), io.LimitReader(stream, expected+1))
	if err != nil {
		_ = stream.Close()
		return written, fmt.Errorf("copying rendition range: %w", err)
	}
	if written != expected {
		if written > expected {
			_ = stream.Close()
		}
		return written, integrityErrorf("verifying rendition range: received %d bytes, expected %d", written, expected)
	}
	wantDigest := "sha-256=:" + base64.StdEncoding.EncodeToString(hash.Sum(nil)) + ":"
	if got := stream.trailer.Get("Content-Digest"); got == "" || got != wantDigest {
		return written, integrityErrorf("verifying rendition range: terminal Content-Digest %q, expected %q",
			got, wantDigest)
	}
	published, err := io.Copy(w, &buffered)
	if err != nil {
		return published, fmt.Errorf("publishing verified rendition range: %w", err)
	}
	return published, nil
}

// CopyVerified validates the complete transport and the retained Markdown's
// canonical frontmatter, body checksum, navigation bounds, and build identity
// before publishing any bytes to w.
func (stream *RenditionStream) CopyVerified(w io.Writer) (int64, error) {
	if stream == nil || stream.ContentStream == nil {
		return 0, errors.New("copying rendition: nil stream")
	}
	if w == nil {
		return 0, errors.New("copying rendition: nil destination")
	}
	maxBytes := stream.maxBytes
	if maxBytes == 0 {
		maxBytes = maxRenditionResponseBytes
	}
	if stream.Size < 1 || stream.Size > maxBytes {
		_ = stream.Close()
		return 0, integrityErrorf("verifying rendition: invalid bounded size %d", stream.Size)
	}
	var buffered bytes.Buffer
	buffered.Grow(int(stream.Size))
	written, err := stream.copyVerified(&buffered, maxBytes)
	if err != nil {
		return written, err
	}
	frontmatter, _, err := document.ParseRenditionFrontMatterV1(buffered.Bytes())
	if err != nil {
		return written, integrityErrorf("verifying rendition frontmatter: %v", err)
	}
	if frontmatter.Rendition.BuildID != stream.BuildID {
		return written, integrityErrorf("verifying rendition: frontmatter build %q differs from transport %q",
			frontmatter.Rendition.BuildID, stream.BuildID)
	}
	if string(frontmatter.Rendition.Completeness) != stream.Completeness {
		return written, integrityErrorf("verifying rendition: frontmatter completeness %q differs from transport %q",
			frontmatter.Rendition.Completeness, stream.Completeness)
	}
	stream.FrontMatter = frontmatter
	published, err := io.Copy(w, &buffered)
	if err != nil {
		return published, fmt.Errorf("publishing verified rendition: %w", err)
	}
	return published, nil
}

func (c *Connection) Rendition(ctx context.Context, attachmentID string, maxBytes int64) (*RenditionStream, error) {
	if !validSHA256Hex(attachmentID) {
		return nil, errors.New("rendition attachment ID must be lowercase SHA-256")
	}
	params := apiclient.GetDocumentRenditionQuery{}
	if maxBytes != 0 {
		if maxBytes < 1 || maxBytes > maxRenditionResponseBytes {
			return nil, errors.New("rendition max bytes must be between 1 and 67108864")
		}
		params.MaxBytes = &maxBytes
	}
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).GetDocumentRendition(runtime.WithStreamingResponse(ctx), &apiclient.GetDocumentRenditionRequestOptions{PathParams: &apiclient.GetDocumentRenditionPath{AttachmentID: attachmentID}, Query: &params})
	if err != nil {
		return nil, err
	}
	resp := responseHTTP
	stream, err := decodeRenditionResponse(resp, attachmentID, maxBytes)
	if err != nil {
		_ = resp.Body.Close()
	}
	return stream, err
}

// RenditionForSelector reads the active rendition for one exact immutable
// source selector. The daemon resolves the live attachment internally.
func (c *Connection) RenditionForSelector(ctx context.Context, selector api.ProcessingSelector, maxBytes int64) (*RenditionStream, error) {
	if maxBytes < 1 || maxBytes > maxRenditionResponseBytes {
		return nil, errors.New("rendition max bytes must be between 1 and 67108864")
	}
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).ReadDocumentRenditionBySelector(runtime.WithStreamingResponse(ctx), &apiclient.ReadDocumentRenditionBySelectorRequestOptions{Body: &api.RenditionSelectorRequest{Selector: selector, MaxBytes: maxBytes}})
	if err != nil {
		return nil, err
	}
	resp := responseHTTP
	stream, err := decodeRenditionResponse(resp, "", maxBytes)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if stream.VersionID != selector.ContentVersionID {
		_ = resp.Body.Close()
		return nil, integrityErrorf("rendition returned content version %q, expected %q",
			stream.VersionID, selector.ContentVersionID)
	}
	return stream, nil
}

func decodeRenditionResponse(
	resp *http.Response, expectedAttachmentID string, maxBytes int64,
) (*RenditionStream, error) {
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	mediaType, parameters, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/markdown" || parameters["charset"] != "utf-8" {
		return nil, integrityErrorf("rendition returned invalid content type %q", resp.Header.Get("Content-Type"))
	}
	attachmentID := resp.Header.Get(api.RenditionAttachmentHeader)
	if expectedAttachmentID != "" && attachmentID != expectedAttachmentID {
		return nil, integrityErrorf("rendition returned attachment %q, expected %q", attachmentID, expectedAttachmentID)
	}
	buildID, artifactID := resp.Header.Get(api.RenditionBuildHeader), resp.Header.Get(api.RenditionArtifactHeader)
	if !validSHA256Hex(attachmentID) || !validSHA256Hex(buildID) || !validSHA256Hex(artifactID) {
		return nil, integrityErrorf("rendition returned invalid build or artifact identity")
	}
	profileFingerprint, completeness, warnings, err := renditionMetadataHeaders(resp.Header)
	if err != nil {
		return nil, integrityErrorf("rendition returned invalid metadata: %v", err)
	}
	if maxBytes == 0 {
		maxBytes = maxRenditionResponseBytes
	}
	size, err := strconv.ParseInt(resp.Header.Get(api.BlobSizeHeader), 10, 64)
	if err != nil || size < 1 || size > maxBytes {
		return nil, integrityErrorf("rendition returned invalid size %q", resp.Header.Get(api.BlobSizeHeader))
	}
	hash, versionID := resp.Header.Get(api.BlobHashHeader), resp.Header.Get(api.ContentVersionHeader)
	if !validSHA256Hex(hash) || !validUUIDv4(versionID) {
		return nil, integrityErrorf("rendition returned invalid blob or content-version identity")
	}
	return &RenditionStream{ContentStream: &ContentStream{ReadCloser: resp.Body,
		VersionID: versionID, BlobHash: hash, Size: size, trailer: resp.Trailer},
		AttachmentID: attachmentID, BuildID: buildID, ArtifactID: artifactID,
		ProfileFingerprint: profileFingerprint, Completeness: completeness, Warnings: warnings, maxBytes: maxBytes}, nil
}

func (c *Connection) RenditionRange(ctx context.Context, attachmentID string,
	start, end int64,
) (*RenditionRangeStream, error) {
	if !validSHA256Hex(attachmentID) {
		return nil, errors.New("rendition attachment ID must be lowercase SHA-256")
	}
	if start < 0 || end <= start || end > 64<<20 {
		return nil, errors.New("rendition range must be a non-empty half-open interval within 67108864 bytes")
	}
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).GetDocumentRendition(runtime.WithStreamingResponse(ctx), &apiclient.GetDocumentRenditionRequestOptions{PathParams: &apiclient.GetDocumentRenditionPath{AttachmentID: attachmentID}, Header: &apiclient.GetDocumentRenditionHeaders{Range: new(fmt.Sprintf("bytes=%d-%d", start, end-1))}})
	if err != nil {
		return nil, err
	}
	resp := responseHTTP
	if resp.StatusCode != http.StatusPartialContent {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeError(resp)
	}
	fail := func(format string, args ...any) (*RenditionRangeStream, error) {
		_ = resp.Body.Close()
		return nil, integrityErrorf(format, args...)
	}
	mediaType, parameters, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/markdown" || parameters["charset"] != "utf-8" {
		return fail("rendition range returned invalid content type %q", resp.Header.Get("Content-Type"))
	}
	buildID, artifactID := resp.Header.Get(api.RenditionBuildHeader), resp.Header.Get(api.RenditionArtifactHeader)
	versionID, blobHash := resp.Header.Get(api.ContentVersionHeader), resp.Header.Get(api.BlobHashHeader)
	if resp.Header.Get(api.RenditionAttachmentHeader) != attachmentID || !validSHA256Hex(buildID) ||
		!validSHA256Hex(artifactID) || !validUUIDv4(versionID) || !validSHA256Hex(blobHash) {
		return fail("rendition range returned invalid immutable identity")
	}
	profileFingerprint, completeness, warnings, err := renditionMetadataHeaders(resp.Header)
	if err != nil {
		return fail("rendition range returned invalid metadata: %v", err)
	}
	total, err := strconv.ParseInt(resp.Header.Get(api.BlobSizeHeader), 10, 64)
	if err != nil || total < 1 || total > 64<<20 {
		return fail("rendition range returned invalid total size")
	}
	gotStart, gotEnd, gotTotal, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil || gotTotal != total || gotStart != start || gotEnd != min(end, total) {
		return fail("rendition range returned invalid Content-Range %q", resp.Header.Get("Content-Range"))
	}
	return &RenditionRangeStream{ReadCloser: resp.Body, AttachmentID: attachmentID,
		BuildID: buildID, ArtifactID: artifactID, VersionID: versionID, BlobHash: blobHash,
		ProfileFingerprint: profileFingerprint, Completeness: completeness, Warnings: warnings,
		Start: gotStart, End: gotEnd, TotalSize: total, trailer: resp.Trailer}, nil
}

func renditionMetadataHeaders(header http.Header) (string, string, []string, error) {
	profileFingerprint := header.Get(api.RenditionProfileHeader)
	if !validSHA256Hex(profileFingerprint) {
		return "", "", nil, errors.New("profile fingerprint is invalid")
	}
	completeness := header.Get(api.RenditionCompletenessHeader)
	if completeness != string(document.EvidenceComplete) && completeness != string(document.EvidencePartial) &&
		completeness != string(document.EvidenceDegradedProvenance) {
		return "", "", nil, errors.New("completeness is invalid")
	}
	rawWarnings := header.Get(api.RenditionWarningsHeader)
	if rawWarnings == "" {
		return profileFingerprint, completeness, []string{}, nil
	}
	warnings := strings.Split(rawWarnings, ",")
	if len(warnings) > 64 {
		return "", "", nil, errors.New("warning list is too large")
	}
	seen := make(map[string]struct{}, len(warnings))
	for _, warning := range warnings {
		if warning == "" || len(warning) > 63 {
			return "", "", nil, errors.New("warning is invalid")
		}
		for _, char := range warning {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.' {
				continue
			}
			return "", "", nil, errors.New("warning is invalid")
		}
		if _, exists := seen[warning]; exists {
			return "", "", nil, errors.New("warning list contains a duplicate")
		}
		seen[warning] = struct{}{}
	}
	return profileFingerprint, completeness, warnings, nil
}

func parseContentRange(value string) (int64, int64, int64, error) {
	rangeText, totalText, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !ok || !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, errors.New("invalid content range")
	}
	startText, inclusiveText, ok := strings.Cut(rangeText, "-")
	if !ok {
		return 0, 0, 0, errors.New("invalid content range")
	}
	start, startErr := strconv.ParseInt(startText, 10, 64)
	inclusive, endErr := strconv.ParseInt(inclusiveText, 10, 64)
	total, totalErr := strconv.ParseInt(totalText, 10, 64)
	if startErr != nil || endErr != nil || totalErr != nil || start < 0 || inclusive < start || inclusive >= total {
		return 0, 0, 0, errors.New("invalid content range")
	}
	return start, inclusive + 1, total, nil
}
