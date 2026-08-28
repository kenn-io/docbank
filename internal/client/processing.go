package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	pathpkg "path"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
)

var (
	ErrProcessingUnavailable = errors.New("document processing is unavailable")
	ErrProcessingPlanChanged = errors.New("document processing plan changed")
	ErrProcessingConsent     = errors.New("document processing consent is required")
)

const (
	maxProcessingEventStreamBytes int64 = 64 << 10
	maxRenditionResponseBytes     int64 = 64 << 20
	maxDocumentSearchPathBytes          = 16 << 10
	maxDocumentSearchExcerptRunes       = 512
	maxDocumentSearchExcerptBytes       = 4 * maxDocumentSearchExcerptRunes
)

var errProcessingStreamTooLarge = errors.New("processing stream is too large")

func (c *Client) ProcessingProfiles(ctx context.Context) ([]api.ProcessingProfileSummary, error) {
	var result []api.ProcessingProfileSummary
	err := c.do(ctx, http.MethodGet, "/api/v1/processing/profiles", nil, nil, &result)
	return result, err
}

func (c *Client) PlanProcessing(ctx context.Context, request api.ProcessingPlanRequest) (api.ProcessingPlan, error) {
	var result api.ProcessingPlan
	err := c.do(ctx, http.MethodPost, "/api/v1/processing/plans", nil, request, &result)
	return result, err
}

func (c *Client) StartProcessing(ctx context.Context, request api.StartProcessingRequest) (api.ProcessingJob, error) {
	stream, err := c.StartProcessingStream(ctx, request)
	if err != nil {
		return api.ProcessingJob{}, err
	}
	defer func() { _ = stream.Close() }()
	first, err := stream.Next()
	if err != nil {
		return api.ProcessingJob{}, err
	}
	if _, err := stream.Next(); err != nil {
		return api.ProcessingJob{}, err
	}
	return *first.Job, nil
}

// ProcessingEventStream incrementally validates the exact two-event processing
// stream. The durable job is returned before terminal delivery; the terminal
// event is returned only after end-of-stream has been verified.
type ProcessingEventStream struct {
	body     io.ReadCloser
	decoder  *jsontext.Decoder
	bounded  *boundedReadCloser
	sequence int
	jobID    string
	done     bool
}

// StartProcessingStream opens one cancellable processing progress stream.
func (c *Client) StartProcessingStream(ctx context.Context,
	request api.StartProcessingRequest,
) (*ProcessingEventStream, error) {
	body, err := marshalJSONRequest(request)
	if err != nil {
		return nil, fmt.Errorf("encoding processing request: %w", err)
	}
	const path = "/api/v1/processing/jobs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building processing request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &transportError{err: fmt.Errorf("starting processing: %w", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeError(resp)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-ndjson" {
		_ = resp.Body.Close()
		return nil, errors.New("processing stream returned an invalid content type")
	}
	bounded := &boundedReadCloser{body: resp.Body, remaining: maxProcessingEventStreamBytes}
	return &ProcessingEventStream{body: bounded, decoder: jsontext.NewDecoder(bounded), bounded: bounded}, nil
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
		if event.Sequence != 1 || event.Type != "job" || event.Job == nil || event.Status != nil || event.Terminal {
			_ = stream.Close()
			return api.ProcessingJobEvent{}, errors.New("processing stream returned malformed job event")
		}
		stream.sequence, stream.jobID = 1, event.Job.ID
		return event, nil
	}
	if event.Sequence != 2 || event.Type != "status" || event.Job != nil || event.Status == nil ||
		!event.Terminal || event.Status.JobID != stream.jobID {
		_ = stream.Close()
		return api.ProcessingJobEvent{}, errors.New("processing stream returned malformed terminal status")
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
		return api.ProcessingJobEvent{}, fmt.Errorf("closing processing stream: %w", err)
	}
	return event, nil
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

func (c *Client) GrantProcessingConsent(ctx context.Context,
	request api.ProcessingConsentGrantRequest,
) (api.ProcessingConsentGrant, error) {
	var result api.ProcessingConsentGrant
	err := c.do(ctx, http.MethodPost, "/api/v1/processing/consent/grants", nil, request, &result)
	return result, err
}

func (c *Client) RevokeProcessingConsent(ctx context.Context) (api.ProcessingConsentRevocation, error) {
	var result api.ProcessingConsentRevocation
	err := c.do(ctx, http.MethodPost, "/api/v1/processing/consent/revocations", nil,
		api.ProcessingConsentRevokeRequest{}, &result)
	return result, err
}

func (c *Client) PlanDerivativePurge(ctx context.Context,
	request api.DerivativePurgePlanRequest,
) (api.DerivativePurgePlan, error) {
	var result api.DerivativePurgePlan
	err := c.do(ctx, http.MethodPost, "/api/v1/derivatives/purge-plans", nil, request, &result)
	return result, err
}

func (c *Client) RunDerivativePurge(ctx context.Context,
	request api.DerivativePurgeJobRequest,
) (api.DerivativePurgeReceipt, error) {
	body, err := marshalJSONRequest(request)
	if err != nil {
		return api.DerivativePurgeReceipt{}, fmt.Errorf("encoding derivative purge request: %w", err)
	}
	const path = "/api/v1/derivatives/purge-jobs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return api.DerivativePurgeReceipt{}, fmt.Errorf("building derivative purge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return api.DerivativePurgeReceipt{}, &transportError{err: fmt.Errorf("running derivative purge: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return api.DerivativePurgeReceipt{}, decodeError(resp)
	}
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
	return *event.Receipt, nil
}

func (c *Client) ProcessingStatus(ctx context.Context, jobID string) (api.ProcessingStatus, error) {
	if !validSHA256Hex(jobID) {
		return api.ProcessingStatus{}, errors.New("processing job ID must be lowercase SHA-256")
	}
	var result api.ProcessingStatus
	err := c.do(ctx, http.MethodGet, "/api/v1/processing/jobs/"+jobID, nil, nil, &result)
	return result, err
}

func (c *Client) DocumentCoverage(ctx context.Context, profile string,
	fence api.DocumentSourceFence,
) (api.CoverageReport, error) {
	query := url.Values{"profile": {profile}, "vault_uid": {fence.VaultUID}}
	for _, id := range fence.ContentVersionIDs {
		query.Add("content_version_id", id)
	}
	var result api.CoverageReport
	err := c.do(ctx, http.MethodGet, "/api/v1/coverage?"+query.Encode(), nil, nil, &result)
	return result, err
}

func (c *Client) SearchDocuments(ctx context.Context, request api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
	var result api.DocumentSearchReport
	if err := c.do(ctx, http.MethodPost, "/api/v1/search", nil, request, &result); err != nil {
		return api.DocumentSearchReport{}, err
	}
	if err := validateDocumentSearchReport(request, result); err != nil {
		return api.DocumentSearchReport{}, fmt.Errorf("search response is invalid: %w", err)
	}
	return result, nil
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
		seenEvidence := make(map[api.DocumentEvidenceReference]struct{}, len(result.Evidence))
		for evidenceIndex, evidence := range result.Evidence {
			if _, duplicate := seenEvidence[evidence]; duplicate {
				return fmt.Errorf("result %d has duplicate evidence", index)
			}
			seenEvidence[evidence] = struct{}{}
			if err := validateDocumentEvidenceIdentity(evidence); err != nil {
				return fmt.Errorf("result %d evidence %d: %w", index, evidenceIndex, err)
			}
		}
	}
	return nil
}

func validDocumentSearchPath(value string) bool {
	return len(value) >= 2 && len(value) <= maxDocumentSearchPathBytes && utf8.ValidString(value) &&
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
	embeddingEmpty := evidence.VectorSpaceID == "" && evidence.EmbeddingSetID == "" &&
		evidence.InputGenerationID == "" && evidence.InputID == "" && evidence.InputKind == "" &&
		evidence.SourceManifestChecksum == ""
	renditionEmpty := evidence.BuildID == "" && evidence.SegmentID == ""
	switch evidence.Kind {
	case "node_name", "content_blob":
		if !embeddingEmpty || !renditionEmpty {
			return errors.New("metadata evidence carries derivative identity")
		}
	case "rendition_segment":
		if !embeddingEmpty || !validSHA256Hex(evidence.BuildID) ||
			!validBoundedSearchIdentity(evidence.SegmentID, 1024) {
			return errors.New("rendition evidence identity is incomplete or inconsistent")
		}
	case "embedding":
		if !renditionEmpty || !validSHA256Hex(evidence.VectorSpaceID) ||
			!validSHA256Hex(evidence.EmbeddingSetID) || !validSHA256Hex(evidence.InputGenerationID) ||
			!validBoundedSearchIdentity(evidence.InputID, 1024) ||
			(evidence.InputKind != "rendition_chunk" && evidence.InputKind != "original_file") ||
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

func (c *Client) Rendition(ctx context.Context, attachmentID string, maxBytes int64) (*RenditionStream, error) {
	if !validSHA256Hex(attachmentID) {
		return nil, errors.New("rendition attachment ID must be lowercase SHA-256")
	}
	path := "/api/v1/renditions/" + attachmentID
	if maxBytes != 0 {
		if maxBytes < 1 || maxBytes > maxRenditionResponseBytes {
			return nil, errors.New("rendition max bytes must be between 1 and 67108864")
		}
		path += "?max_bytes=" + strconv.FormatInt(maxBytes, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("building rendition request: %w", err)
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &transportError{err: fmt.Errorf("fetching rendition: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeError(resp)
	}
	fail := func(format string, args ...any) (*RenditionStream, error) {
		_ = resp.Body.Close()
		return nil, integrityErrorf(format, args...)
	}
	mediaType, parameters, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/markdown" || parameters["charset"] != "utf-8" {
		return fail("rendition returned invalid content type %q", resp.Header.Get("Content-Type"))
	}
	if got := resp.Header.Get(api.RenditionAttachmentHeader); got != attachmentID {
		return fail("rendition returned attachment %q, expected %q", got, attachmentID)
	}
	buildID, artifactID := resp.Header.Get(api.RenditionBuildHeader), resp.Header.Get(api.RenditionArtifactHeader)
	if !validSHA256Hex(buildID) || !validSHA256Hex(artifactID) {
		return fail("rendition returned invalid build or artifact identity")
	}
	profileFingerprint, completeness, warnings, err := renditionMetadataHeaders(resp.Header)
	if err != nil {
		return fail("rendition returned invalid metadata: %v", err)
	}
	effectiveMaxBytes := maxBytes
	if effectiveMaxBytes == 0 {
		effectiveMaxBytes = maxRenditionResponseBytes
	}
	size, err := strconv.ParseInt(resp.Header.Get(api.BlobSizeHeader), 10, 64)
	if err != nil || size < 1 || size > effectiveMaxBytes {
		return fail("rendition returned invalid size %q", resp.Header.Get(api.BlobSizeHeader))
	}
	hash, versionID := resp.Header.Get(api.BlobHashHeader), resp.Header.Get(api.ContentVersionHeader)
	if !validSHA256Hex(hash) || !validUUIDv4(versionID) {
		return fail("rendition returned invalid blob or content-version identity")
	}
	return &RenditionStream{ContentStream: &ContentStream{ReadCloser: resp.Body,
		VersionID: versionID, BlobHash: hash, Size: size, trailer: resp.Trailer},
		AttachmentID: attachmentID, BuildID: buildID, ArtifactID: artifactID,
		ProfileFingerprint: profileFingerprint, Completeness: completeness, Warnings: warnings,
		maxBytes: effectiveMaxBytes}, nil
}

// RenditionForSelector reads the active rendition for one exact immutable
// source selector. The daemon resolves the live attachment internally.
func (c *Client) RenditionForSelector(ctx context.Context, selector api.ProcessingSelector, maxBytes int64) (*RenditionStream, error) {
	if maxBytes < 1 || maxBytes > maxRenditionResponseBytes {
		return nil, errors.New("rendition max bytes must be between 1 and 67108864")
	}
	body, err := marshalJSONRequest(api.RenditionSelectorRequest{Selector: selector, MaxBytes: maxBytes})
	if err != nil {
		return nil, fmt.Errorf("encoding rendition selector: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/renditions/select", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building rendition selector request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &transportError{err: fmt.Errorf("fetching rendition: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeError(resp)
	}
	fail := func(format string, args ...any) (*RenditionStream, error) {
		_ = resp.Body.Close()
		return nil, integrityErrorf(format, args...)
	}
	mediaType, parameters, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/markdown" || parameters["charset"] != "utf-8" {
		return fail("rendition returned invalid content type %q", resp.Header.Get("Content-Type"))
	}
	attachmentID, buildID, artifactID := resp.Header.Get(api.RenditionAttachmentHeader), resp.Header.Get(api.RenditionBuildHeader), resp.Header.Get(api.RenditionArtifactHeader)
	if !validSHA256Hex(attachmentID) || !validSHA256Hex(buildID) || !validSHA256Hex(artifactID) {
		return fail("rendition returned invalid immutable identity")
	}
	profileFingerprint, completeness, warnings, err := renditionMetadataHeaders(resp.Header)
	if err != nil {
		return fail("rendition returned invalid metadata: %v", err)
	}
	size, err := strconv.ParseInt(resp.Header.Get(api.BlobSizeHeader), 10, 64)
	if err != nil || size < 1 || size > maxBytes {
		return fail("rendition returned invalid size %q", resp.Header.Get(api.BlobSizeHeader))
	}
	hash, versionID := resp.Header.Get(api.BlobHashHeader), resp.Header.Get(api.ContentVersionHeader)
	if !validSHA256Hex(hash) || !validUUIDv4(versionID) {
		return fail("rendition returned invalid blob or content-version identity")
	}
	return &RenditionStream{ContentStream: &ContentStream{ReadCloser: resp.Body, VersionID: versionID, BlobHash: hash, Size: size, trailer: resp.Trailer}, AttachmentID: attachmentID, BuildID: buildID, ArtifactID: artifactID, ProfileFingerprint: profileFingerprint, Completeness: completeness, Warnings: warnings, maxBytes: maxBytes}, nil
}

func (c *Client) RenditionRange(ctx context.Context, attachmentID string,
	start, end int64,
) (*RenditionRangeStream, error) {
	if !validSHA256Hex(attachmentID) {
		return nil, errors.New("rendition attachment ID must be lowercase SHA-256")
	}
	if start < 0 || end <= start || end > 64<<20 {
		return nil, errors.New("rendition range must be a non-empty half-open interval within 67108864 bytes")
	}
	path := "/api/v1/renditions/" + attachmentID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("building rendition range request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &transportError{err: fmt.Errorf("fetching rendition range: %w", err)}
	}
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
