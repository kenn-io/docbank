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
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
)

var (
	ErrProcessingUnavailable = errors.New("document processing is unavailable")
	ErrProcessingPlanChanged = errors.New("document processing plan changed")
	ErrProcessingConsent     = errors.New("document processing consent is required")
)

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
	body, err := marshalJSONRequest(request)
	if err != nil {
		return api.ProcessingJob{}, fmt.Errorf("encoding processing request: %w", err)
	}
	const path = "/api/v1/processing/jobs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return api.ProcessingJob{}, fmt.Errorf("building processing request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return api.ProcessingJob{}, &transportError{err: fmt.Errorf("starting processing: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return api.ProcessingJob{}, decodeError(resp)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-ndjson" {
		return api.ProcessingJob{}, errors.New("processing stream returned an invalid content type")
	}
	decoder := jsontext.NewDecoder(resp.Body)
	var first, second api.ProcessingJobEvent
	if err := json.UnmarshalDecode(decoder, &first, json.RejectUnknownMembers(true)); err != nil {
		return api.ProcessingJob{}, fmt.Errorf("decoding processing job event: %w", err)
	}
	if first.Sequence != 1 || first.Type != "job" || first.Job == nil || first.Status != nil || first.Terminal {
		return api.ProcessingJob{}, errors.New("processing stream returned malformed job event")
	}
	if err := json.UnmarshalDecode(decoder, &second, json.RejectUnknownMembers(true)); err != nil {
		return api.ProcessingJob{}, fmt.Errorf("decoding processing status event: %w", err)
	}
	if second.Sequence != 2 || second.Type != "status" || second.Job != nil || second.Status == nil ||
		!second.Terminal || second.Status.JobID != first.Job.ID {
		return api.ProcessingJob{}, errors.New("processing stream returned malformed terminal status")
	}
	var extra api.ProcessingJobEvent
	if err := json.UnmarshalDecode(decoder, &extra, json.RejectUnknownMembers(true)); !errors.Is(err, io.EOF) {
		return api.ProcessingJob{}, errors.New("processing stream continued after its terminal status")
	}
	return *first.Job, nil
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
	err := c.do(ctx, http.MethodPost, "/api/v1/search", nil, request, &result)
	return result, err
}

// RenditionStream carries the immutable rendition and source identities that
// must agree with the complete verified Markdown transfer.
type RenditionStream struct {
	*ContentStream

	AttachmentID string
	BuildID      string
	ArtifactID   string
	FrontMatter  document.RenditionFrontMatterV1
}

// RenditionRangeStream is a verified byte range of the complete immutable
// rendition. Start and End are whole-artifact offsets; frontmatter navigation
// offsets remain relative to the Markdown body.
type RenditionRangeStream struct {
	io.ReadCloser

	AttachmentID string
	BuildID      string
	ArtifactID   string
	VersionID    string
	BlobHash     string
	Start        int64
	End          int64
	TotalSize    int64
	trailer      http.Header
}

func (stream *RenditionRangeStream) CopyVerified(w io.Writer) (int64, error) {
	if stream == nil || stream.ReadCloser == nil || w == nil {
		return 0, errors.New("copying rendition range: nil stream or destination")
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(w, hash), stream)
	if err != nil {
		return written, fmt.Errorf("copying rendition range: %w", err)
	}
	expected := stream.End - stream.Start
	if expected < 1 || written != expected {
		return written, integrityErrorf("verifying rendition range: received %d bytes, expected %d", written, expected)
	}
	wantDigest := "sha-256=:" + base64.StdEncoding.EncodeToString(hash.Sum(nil)) + ":"
	if got := stream.trailer.Get("Content-Digest"); got == "" || got != wantDigest {
		return written, integrityErrorf("verifying rendition range: terminal Content-Digest %q, expected %q",
			got, wantDigest)
	}
	return written, nil
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
	if stream.Size < 1 || stream.Size > 64<<20 {
		return 0, integrityErrorf("verifying rendition: invalid bounded size %d", stream.Size)
	}
	var buffered bytes.Buffer
	buffered.Grow(int(stream.Size))
	written, err := stream.ContentStream.CopyVerified(&buffered)
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
		if maxBytes < 1 || maxBytes > 64<<20 {
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
	size, err := strconv.ParseInt(resp.Header.Get(api.BlobSizeHeader), 10, 64)
	if err != nil || size < 0 || (maxBytes != 0 && size > maxBytes) {
		return fail("rendition returned invalid size %q", resp.Header.Get(api.BlobSizeHeader))
	}
	hash, versionID := resp.Header.Get(api.BlobHashHeader), resp.Header.Get(api.ContentVersionHeader)
	if !validSHA256Hex(hash) || !validUUIDv4(versionID) {
		return fail("rendition returned invalid blob or content-version identity")
	}
	return &RenditionStream{ContentStream: &ContentStream{ReadCloser: resp.Body,
		VersionID: versionID, BlobHash: hash, Size: size, trailer: resp.Trailer},
		AttachmentID: attachmentID, BuildID: buildID, ArtifactID: artifactID}, nil
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
		Start: gotStart, End: gotEnd, TotalSize: total, trailer: resp.Trailer}, nil
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
