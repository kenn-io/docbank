package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	provider                = providerutil.Provider("bridge")
	defaultRequestTimeout   = 30 * time.Second
	defaultTotalTimeout     = 10 * time.Minute
	defaultPollInterval     = time.Second
	defaultMaxPollAttempts  = 300
	defaultMaxResponseBytes = int64(512 << 20)
	maxBridgeTimeout        = 24 * time.Hour
	maxBridgePollAttempts   = 10_000
	maxBridgeResponseBytes  = int64(512 << 20)
	maxBridgeErrorMessage   = 1024
	maxBridgeCleanupTimeout = 5 * time.Second
)

var _ document.RenditionProvider = (*Client)(nil)

// rendering carries the state of one Render call between its stages.
type rendering struct {
	operation     *providerutil.Operation
	usage         providerutil.Usage
	authorization document.RenditionAuthorization
	jobID         string
	jobStatus     JobStatus
}

// New validates a fixed bridge profile and returns an isolated HTTP client.
func New(profile Profile, secrets SecretResolver, httpClient *http.Client) (*Client, error) {
	origin, err := provider.ValidateOrigin(profile.Origin, profile.Descriptor.TrustBoundary)
	if err != nil {
		return nil, err
	}
	descriptor, err := provider.CanonicalDescriptor(profile.Descriptor)
	if err != nil {
		return nil, err
	}
	credential := providerutil.BearerCredential(profile.SecretBinding, secrets)
	if err := credential.Validate(provider); err != nil {
		return nil, err
	}
	if httpClient == nil {
		return nil, errors.New("bridge: HTTP client is required")
	}
	if !providerutil.Bounded(&profile.RequestTimeout, defaultRequestTimeout, maxBridgeTimeout) ||
		!providerutil.Bounded(&profile.TotalTimeout, defaultTotalTimeout, maxBridgeTimeout) ||
		!providerutil.Bounded(&profile.PollInterval, defaultPollInterval, profile.TotalTimeout) ||
		!providerutil.Bounded(&profile.MaxPollAttempts, defaultMaxPollAttempts, maxBridgePollAttempts) ||
		!providerutil.Bounded(&profile.MaxResponseBytes, defaultMaxResponseBytes, maxBridgeResponseBytes) {
		return nil, errors.New("bridge: execution bounds are invalid")
	}
	return &Client{
		executor: providerutil.Executor{
			Provider: provider, HTTP: providerhttp.IsolateClient(httpClient), Origin: origin,
			RequestTimeout: profile.RequestTimeout, MaxResponseBytes: profile.MaxResponseBytes,
			Credential: credential, ResponseMediaType: jobMediaType,
		},
		descriptor: descriptor, totalTimeout: profile.TotalTimeout,
		pollInterval: profile.PollInterval, maxPollAttempts: profile.MaxPollAttempts,
	}, nil
}

// Descriptor returns the immutable provider identity fixed by the profile.
func (client *Client) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(client.descriptor)
}

// Render submits one exact upload and drives the bounded bridge job state machine.
func (client *Client) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("bridge: client is required")
	}
	metadata := upload.Metadata()
	if strings.ContainsAny(metadata.Filename, "\r\n") {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"bridge filename contains unsupported line breaks", nil)
	}
	operation, err := providerutil.NewOperation(ctx, provider, authorization.ExpiresAt, client.totalTimeout)
	if err != nil {
		return document.RenditionResult{}, err
	}
	defer operation.Cancel()
	manifest := AuthorizationManifest{
		ContractVersion: ContractVersion, Source: metadata, Authorization: authorization,
	}
	manifestJSON, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil {
		return document.RenditionResult{}, fmt.Errorf("bridge: encode authorization: %w", err)
	}
	run := &rendering{operation: operation, authorization: authorization}
	envelope, err := client.submit(run, upload, manifestJSON)
	defer client.cleanup(ctx, run)
	if err != nil {
		return document.RenditionResult{}, err
	}
	return client.awaitJob(run, envelope)
}

// cleanup cancels a job the bridge still considers live after Render fails.
func (client *Client) cleanup(ctx context.Context, run *rendering) {
	if run.jobID == "" || run.jobStatus == JobCompleted || run.jobStatus == JobFailed || run.jobStatus == JobCanceled {
		return
	}
	cleanupTimeout := min(client.executor.RequestTimeout, maxBridgeCleanupTimeout)
	cancelCtx, cancelRemote := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancelRemote()
	_ = client.cancelJob(cancelCtx, run.jobID)
}

func (client *Client) awaitJob(run *rendering, envelope jobEnvelope) (document.RenditionResult, error) {
	var retryDelay time.Duration
	for attempt := 0; ; attempt++ {
		run.jobStatus = envelope.Status
		switch envelope.Status {
		case JobCompleted:
			return client.decodeCompleted(run, envelope)
		case JobFailed, JobCanceled:
			return document.RenditionResult{}, providerErrorFromEnvelope(envelope)
		case JobQueued, JobRunning:
		default:
			return document.RenditionResult{}, provider.Malformed("bridge returned an invalid job status", nil)
		}
		if attempt >= client.maxPollAttempts {
			return document.RenditionResult{}, provider.AmbiguousJob(provider.Classified(
				document.RenditionErrorCapacity, "bridge polling limit reached", nil))
		}
		delay := client.pollInterval
		if retryDelay > 0 {
			delay = retryDelay
			retryDelay = 0
		} else if envelope.RetryAfterMillis > 0 {
			delay = time.Duration(envelope.RetryAfterMillis) * time.Millisecond
		}
		if err := run.operation.Wait(delay); err != nil {
			return document.RenditionResult{}, provider.KnownJobError(err)
		}
		var err error
		envelope, err = client.getJob(run, run.jobID)
		if err != nil {
			if operationErr := run.operation.Check(); operationErr != nil {
				return document.RenditionResult{}, provider.KnownJobError(operationErr)
			}
			if !document.IsRenditionProviderErrorRetryable(err) {
				return document.RenditionResult{}, err
			}
			if providerError, ok := errors.AsType[*document.RenditionProviderError](err); ok {
				retryDelay = providerError.RetryAfter()
			}
			run.usage.Retries++
			envelope = jobEnvelope{Status: JobRunning, JobID: run.jobID}
		}
	}
}

func (client *Client) submit(
	run *rendering, upload document.AuthorizedUpload, manifest []byte,
) (jobEnvelope, error) {
	metadata := upload.Metadata()
	idempotencyKey := providerutil.SHA256Hex(manifest)
	response, err := client.executor.Do(run.operation, &run.usage, providerutil.Request{
		Stage: providerutil.StageSubmission, Method: http.MethodPost, Path: jobsPath,
		Headers: map[string]string{"Idempotency-Key": idempotencyKey},
		Upload: &providerutil.MultipartUpload{
			Prologue: func(writer *multipart.Writer) error {
				header := make(textproto.MIMEHeader)
				header.Set("Content-Disposition", `form-data; name="`+authorizationPartName+`"`)
				header.Set("Content-Type", "application/vnd.docbank.rendition-authorization+json;version=1")
				part, err := writer.CreatePart(header)
				if err != nil {
					return fmt.Errorf("bridge: create authorization multipart part: %w", err)
				}
				_, err = part.Write(manifest)
				return err
			},
			FieldName: sourcePartName, Filename: metadata.Filename, MediaType: metadata.MediaType,
			Source: upload, Length: metadata.ByteLength,
			Interrupt: func() error { return document.InterruptAuthorizedUpload(upload) },
		},
	})
	if err != nil {
		if response.Success() {
			var accepted jobEnvelope
			if json.Unmarshal(response.Body, &accepted) == nil &&
				client.validateEnvelope(accepted, metadata) == nil {
				run.jobID, run.jobStatus = accepted.JobID, accepted.Status
			}
		}
		return jobEnvelope{}, err
	}
	if response.Status != http.StatusOK && response.Status != http.StatusAccepted {
		return jobEnvelope{}, client.responseError(providerutil.StageSubmission, response)
	}
	var envelope jobEnvelope
	if err := json.Unmarshal(response.Body, &envelope); err != nil {
		return jobEnvelope{}, provider.AmbiguousSubmission(provider.Malformed("bridge response JSON is invalid", err))
	}
	run.jobID, run.jobStatus = envelope.JobID, envelope.Status
	if err := client.validateEnvelope(envelope, metadata); err != nil {
		return envelope, err
	}
	if response.Status == http.StatusOK && envelope.Status != JobCompleted {
		return envelope, provider.Malformed("bridge synchronous response is not completed", nil)
	}
	if response.Status == http.StatusAccepted && envelope.Status != JobQueued && envelope.Status != JobRunning {
		return envelope, provider.Malformed("bridge accepted response has an invalid status", nil)
	}
	return envelope, nil
}

func (client *Client) getJob(run *rendering, jobID string) (jobEnvelope, error) {
	if err := provider.ValidatePathIdentifier(jobID, "job ID"); err != nil {
		return jobEnvelope{}, provider.Malformed("bridge returned an invalid job ID", err)
	}
	response, err := client.executor.Do(run.operation, &run.usage, providerutil.Request{
		Stage: providerutil.StageJob, Method: http.MethodGet, Path: jobsPath + "/" + jobID,
	})
	if err != nil {
		return jobEnvelope{}, err
	}
	if response.Status != http.StatusOK && response.Status != http.StatusAccepted {
		return jobEnvelope{}, client.responseError(providerutil.StageJob, response)
	}
	var envelope jobEnvelope
	if err := json.Unmarshal(response.Body, &envelope); err != nil {
		return jobEnvelope{}, provider.Malformed("bridge response JSON is invalid", err)
	}
	if err := client.validateEnvelope(envelope, document.AuthorizedUploadMetadata{
		SHA256: run.authorization.SourceSHA256,
	}); err != nil {
		return jobEnvelope{}, err
	}
	if envelope.JobID != jobID {
		return jobEnvelope{}, provider.Malformed("bridge job identity changed while polling", nil)
	}
	return envelope, nil
}

// responseError classifies a response outside the accepted statuses. A body
// must be a job envelope, whose stable error takes precedence over the
// status; the Retry-After header supplies the hint when the envelope has none.
func (client *Client) responseError(stage providerutil.Stage, response providerutil.Response) error {
	var envelope jobEnvelope
	if len(response.Body) != 0 {
		if err := provider.RequireMediaType(response.Header.Get("Content-Type"), jobMediaType); err != nil {
			return err
		}
		if err := json.Unmarshal(response.Body, &envelope); err != nil {
			return provider.Malformed("bridge response JSON is invalid", err)
		}
	}
	if envelope.RetryAfterMillis == 0 {
		envelope.RetryAfterMillis = response.RetryAfter.Milliseconds()
	}
	return client.statusError(stage, response.Status, envelope)
}

func (client *Client) validateEnvelope(
	envelope jobEnvelope, metadata document.AuthorizedUploadMetadata,
) error {
	if envelope.ContractVersion != ContractVersion {
		return provider.Malformed("bridge contract version is unsupported", nil)
	}
	if err := provider.ValidatePathIdentifier(envelope.JobID, "job ID"); err != nil {
		return provider.Malformed("bridge job ID is invalid", err)
	}
	if metadata.SHA256 != "" && envelope.SourceSHA256 != metadata.SHA256 {
		return provider.Malformed("bridge source identity does not match upload", nil)
	}
	if envelope.AdapterID != client.descriptor.ID ||
		envelope.DescriptorFingerprint != client.descriptor.Fingerprint ||
		envelope.PolicyFingerprint != client.descriptor.PolicyFingerprint {
		return provider.Malformed("bridge provider identity does not match profile", nil)
	}
	if envelope.RetryAfterMillis < 0 || envelope.RetryAfterMillis > int64(client.totalTimeout/time.Millisecond) {
		return provider.Malformed("bridge retry delay is outside bounds", nil)
	}
	return nil
}

func (client *Client) decodeCompleted(run *rendering, envelope jobEnvelope) (document.RenditionResult, error) {
	if len(envelope.Result) == 0 {
		return document.RenditionResult{}, provider.Malformed("bridge completed response lacks a result", nil)
	}
	var wire completedResult
	if err := json.Unmarshal(envelope.Result, &wire, json.RejectUnknownMembers(true)); err != nil {
		return document.RenditionResult{}, provider.Malformed(
			"bridge completed result has an unknown member or invalid value", err)
	}
	evidence, err := decodeEvidence(wire.Evidence)
	if err != nil {
		return document.RenditionResult{}, err
	}
	var markdown []byte
	if wire.ProviderMarkdown != nil {
		markdown, err = decodeInlinePayload(*wire.ProviderMarkdown, run.authorization.MaxProviderMarkdownBytes)
		if err != nil {
			return document.RenditionResult{}, provider.Malformed("bridge provider Markdown is invalid", err)
		}
		if providerutil.InjectsDocbankFrontmatter(markdown) {
			return document.RenditionResult{}, provider.Malformed(
				"bridge provider Markdown attempts Docbank frontmatter injection", nil)
		}
	}
	artifactByteBudget, err := preflightArtifacts(
		evidence, markdown, wire, run.authorization, client.executor.MaxResponseBytes)
	if err != nil {
		return document.RenditionResult{}, err
	}
	artifacts := make([]document.RenditionArtifact, 0, len(wire.Artifacts))
	for _, artifact := range wire.Artifacts {
		payload, err := client.resolveArtifact(run, envelope.JobID, artifact,
			min(run.authorization.MaxArtifactBytes, int(artifactByteBudget)))
		if err != nil {
			return document.RenditionResult{}, err
		}
		if artifact.Role == document.EvidenceArtifactMarkdown && providerutil.InjectsDocbankFrontmatter(payload) {
			return document.RenditionResult{}, provider.Malformed(
				"bridge provider Markdown artifact attempts Docbank frontmatter injection", nil)
		}
		artifactByteBudget -= int64(len(payload))
		artifacts = append(artifacts, document.RenditionArtifact{
			Role: artifact.Role, MediaType: artifact.MediaType, Payload: payload, SHA256: artifact.SHA256,
		})
	}
	result := document.RenditionResult{
		Evidence: evidence, ProviderMarkdown: markdown, Artifacts: artifacts, Receipt: wire.Receipt,
	}
	if err := document.ValidateRenditionResult(client.descriptor, run.authorization, result); err != nil {
		return document.RenditionResult{}, provider.Malformed("bridge completed result failed validation", err)
	}
	return result, nil
}

func preflightArtifacts(
	evidence document.SourceEvidenceV1, markdown []byte, wire completedResult,
	authorization document.RenditionAuthorization, maxResponseBytes int64,
) (int64, error) {
	artifacts := wire.Artifacts
	if len(artifacts) > authorization.MaxArtifacts {
		return 0, provider.Malformed("bridge artifact count exceeds authorization", nil)
	}
	projected := document.RenditionResult{
		Evidence: evidence, ProviderMarkdown: markdown, Receipt: wire.Receipt,
		Artifacts: make([]document.RenditionArtifact, 0, len(artifacts)),
	}
	for _, artifact := range artifacts {
		if !slices.Contains(authorization.AllowedArtifactRoles, artifact.Role) {
			return 0, provider.Malformed("bridge artifact role is not authorized", nil)
		}
		if artifact.ByteLength < 0 || artifact.ByteLength > int64(authorization.MaxArtifactBytes) {
			return 0, provider.Malformed("bridge artifact length is outside authorization", nil)
		}
		if artifact.ByteLength > maxResponseBytes {
			return 0, provider.Malformed("bridge artifact length exceeds response byte limit", nil)
		}
		projected.Artifacts = append(projected.Artifacts, document.RenditionArtifact{
			Role: artifact.Role, MediaType: artifact.MediaType, Payload: []byte{}, SHA256: artifact.SHA256,
		})
	}
	encoded, err := json.Marshal(projected, json.Deterministic(true))
	if err != nil {
		return 0, provider.Malformed("bridge completed result cannot be encoded", err)
	}
	remainingEncodedBytes := int64(authorization.MaxTotalResultBytes) - int64(len(encoded))
	for _, artifact := range artifacts {
		encodedPayloadBytes := ((artifact.ByteLength + 2) / 3) * 4
		if encodedPayloadBytes > remainingEncodedBytes {
			return 0, provider.Malformed("bridge total result bytes exceed authorization", nil)
		}
		remainingEncodedBytes -= encodedPayloadBytes
	}
	return int64(authorization.MaxTotalResultBytes), nil
}

func decodeEvidence(payload evidencePayload) (document.SourceEvidenceV1, error) {
	if payload.MediaType != evidenceMediaType {
		return document.SourceEvidenceV1{}, provider.Malformed("bridge evidence content type is invalid", nil)
	}
	if payload.ByteLength != int64(len(payload.Inline)) {
		return document.SourceEvidenceV1{}, provider.Malformed("bridge evidence length does not match declaration", nil)
	}
	if payload.SHA256 != providerutil.SHA256Hex(payload.Inline) {
		return document.SourceEvidenceV1{}, provider.Malformed("bridge evidence checksum does not match declaration", nil)
	}
	var evidence document.SourceEvidenceV1
	if err := json.Unmarshal(payload.Inline, &evidence, json.RejectUnknownMembers(true)); err != nil {
		return document.SourceEvidenceV1{}, provider.Malformed("bridge source evidence is invalid", err)
	}
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		return document.SourceEvidenceV1{}, provider.Malformed("bridge source evidence failed validation", err)
	}
	return evidence, nil
}

func (client *Client) resolveArtifact(
	run *rendering, artifactJobID string, artifact artifactPayload, maxBytes int,
) ([]byte, error) {
	if artifact.ByteLength < 0 || artifact.ByteLength > int64(maxBytes) {
		return nil, provider.Malformed("bridge artifact length is outside authorization", nil)
	}
	if artifact.Location == "inline" {
		if artifact.ArtifactID != "" {
			return nil, provider.Malformed("bridge inline artifact has a result identity", nil)
		}
		return decodeInlinePayload(binaryPayloadRecord{
			MediaType: artifact.MediaType, ByteLength: artifact.ByteLength,
			SHA256: artifact.SHA256, InlineBase64: artifact.InlineBase64,
		}, maxBytes)
	}
	if artifact.Location != "result" || artifact.InlineBase64 != "" {
		return nil, provider.Malformed("bridge artifact location is invalid", nil)
	}
	if err := provider.ValidatePathIdentifier(artifact.ArtifactID, "artifact ID"); err != nil {
		return nil, provider.Malformed("bridge artifact ID is invalid", err)
	}
	return client.fetchArtifact(run, artifactJobID, artifact)
}

func (client *Client) fetchArtifact(run *rendering, jobID string, artifact artifactPayload) ([]byte, error) {
	// The declared length is the read bound: a provider that sends more than
	// it declared is refused after one extra byte, not after the profile's
	// whole response allowance. A declared empty artifact still bounds the
	// read at one byte, because zero would mean "no bound" to the executor.
	response, err := client.executor.Do(run.operation, &run.usage, providerutil.Request{
		Stage: providerutil.StageJob, Method: http.MethodGet,
		Path:              jobsPath + "/" + jobID + "/artifacts/" + artifact.ArtifactID,
		ResponseMediaType: artifact.MediaType, MaxResponseBytes: max(artifact.ByteLength, 1),
	})
	if err != nil {
		return nil, err
	}
	if response.Status != http.StatusOK {
		return nil, client.responseError(providerutil.StageJob, response)
	}
	if response.ContentLength >= 0 && response.ContentLength != artifact.ByteLength {
		return nil, provider.Malformed("bridge artifact HTTP length does not match declaration", nil)
	}
	if int64(len(response.Body)) != artifact.ByteLength {
		return nil, provider.Malformed("bridge artifact length does not match declaration", nil)
	}
	if providerutil.SHA256Hex(response.Body) != artifact.SHA256 {
		return nil, provider.Malformed("bridge artifact checksum does not match declaration", nil)
	}
	return response.Body, nil
}

func (client *Client) cancelJob(ctx context.Context, jobID string) error {
	if err := provider.ValidatePathIdentifier(jobID, "job ID"); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, client.executor.Origin+jobsPath+"/"+jobID, nil)
	if err != nil {
		return err
	}
	if err := client.executor.Credential.Authorize(provider, request); err != nil {
		return err
	}
	response, err := client.executor.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent &&
		response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusGone {
		return fmt.Errorf("bridge cancellation returned HTTP %d", response.StatusCode)
	}
	return nil
}

func decodeInlinePayload(value binaryPayloadRecord, maxBytes int) ([]byte, error) {
	if value.ByteLength < 0 || value.ByteLength > int64(maxBytes) {
		return nil, provider.Malformed("inline payload length is outside authorization", nil)
	}
	if len(value.InlineBase64) != base64.StdEncoding.EncodedLen(int(value.ByteLength)) ||
		strings.ContainsAny(value.InlineBase64, "\r\n") {
		return nil, provider.Malformed("inline payload base64 encoding is invalid", nil)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(value.InlineBase64)
	if err != nil {
		return nil, provider.Malformed("inline payload base64 is invalid", err)
	}
	if int64(len(payload)) != value.ByteLength {
		return nil, provider.Malformed("inline payload length does not match declaration", nil)
	}
	if providerutil.SHA256Hex(payload) != value.SHA256 {
		return nil, provider.Malformed("inline payload checksum does not match declaration", nil)
	}
	if _, _, err := mime.ParseMediaType(value.MediaType); err != nil {
		return nil, provider.Malformed("inline payload media type is invalid", err)
	}
	return payload, nil
}

func providerErrorFromEnvelope(envelope jobEnvelope) error {
	if envelope.Status == JobCanceled && len(envelope.Error) == 0 {
		return provider.Classified(document.RenditionErrorCanceled, "bridge job was canceled", nil)
	}
	if len(envelope.Error) == 0 {
		return provider.Malformed("bridge failed response lacks a stable error", nil)
	}
	var providerError bridgeError
	if err := json.Unmarshal(envelope.Error, &providerError, json.RejectUnknownMembers(true)); err != nil {
		return provider.Malformed("bridge failed response has an unknown member or invalid value", err)
	}
	if providerError.Message == "" || utf8.RuneCountInString(providerError.Message) > maxBridgeErrorMessage {
		return provider.Malformed("bridge error message is outside bounds", nil)
	}
	if providerError.RetryAfterMillis < 0 ||
		providerError.RetryAfterMillis > int64(maxBridgeTimeout/time.Millisecond) {
		return provider.Malformed("bridge error retry delay is outside bounds", nil)
	}
	retry := time.Duration(providerError.RetryAfterMillis) * time.Millisecond
	return providerutil.ClassifiedError(string(provider), providerError.Code, providerError.Message, retry, nil)
}

// statusError classifies a non-accepted status. A stable envelope error wins;
// otherwise the shared status table applies with the envelope's retry delay.
func (client *Client) statusError(stage providerutil.Stage, status int, envelope jobEnvelope) error {
	if len(envelope.Error) != 0 {
		return providerErrorFromEnvelope(envelope)
	}
	if envelope.RetryAfterMillis < 0 || envelope.RetryAfterMillis > int64(client.totalTimeout/time.Millisecond) {
		return provider.Malformed("bridge retry delay is outside bounds", nil)
	}
	return provider.StatusError(stage, status, time.Duration(envelope.RetryAfterMillis)*time.Millisecond, nil)
}
