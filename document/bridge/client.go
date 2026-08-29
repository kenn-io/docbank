package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	defaultRequestTimeout   = 30 * time.Second
	defaultTotalTimeout     = 10 * time.Minute
	defaultPollInterval     = time.Second
	defaultMaxPollAttempts  = 300
	defaultMaxResponseBytes = int64(512 << 20)
	maxBridgeTimeout        = 24 * time.Hour
	maxBridgePollAttempts   = 10_000
	maxBridgeResponseBytes  = int64(512 << 20)
	maxBridgeIdentifier     = 128
	maxBridgeSecret         = 64 << 10
)

var _ document.RenditionProvider = (*Client)(nil)

// New validates a fixed bridge profile and returns an isolated HTTP client.
func New(profile Profile, secrets SecretResolver, httpClient *http.Client) (*Client, error) {
	origin, err := validateOrigin(profile.Origin, profile.Descriptor.TrustBoundary)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewRenditionDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		if err == nil {
			err = errors.New("descriptor is not canonical")
		}
		return nil, fmt.Errorf("bridge: invalid descriptor: %w", err)
	}
	if profile.SecretBinding == "" {
		if secrets != nil {
			return nil, errors.New("bridge: secret resolver requires a named binding")
		}
	} else {
		if secrets == nil {
			return nil, errors.New("bridge: named secret binding requires a resolver")
		}
		if err := validateIdentifier(profile.SecretBinding, "secret binding"); err != nil {
			return nil, err
		}
	}
	if httpClient == nil {
		return nil, errors.New("bridge: HTTP client is required")
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = defaultRequestTimeout
	}
	if profile.TotalTimeout == 0 {
		profile.TotalTimeout = defaultTotalTimeout
	}
	if profile.PollInterval == 0 {
		profile.PollInterval = defaultPollInterval
	}
	if profile.MaxPollAttempts == 0 {
		profile.MaxPollAttempts = defaultMaxPollAttempts
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = defaultMaxResponseBytes
	}
	if profile.RequestTimeout <= 0 || profile.RequestTimeout > maxBridgeTimeout ||
		profile.TotalTimeout <= 0 || profile.TotalTimeout > maxBridgeTimeout ||
		profile.PollInterval <= 0 || profile.PollInterval > profile.TotalTimeout ||
		profile.MaxPollAttempts < 1 || profile.MaxPollAttempts > maxBridgePollAttempts ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maxBridgeResponseBytes {
		return nil, errors.New("bridge: execution bounds are invalid")
	}
	isolatedHTTP := *httpClient
	isolatedHTTP.CheckRedirect = providerhttp.RefuseRedirects
	isolatedHTTP.Jar = nil
	return &Client{
		origin: origin, descriptor: cloneDescriptor(descriptor),
		secretBinding: profile.SecretBinding, secrets: secrets, http: &isolatedHTTP,
		requestTimeout: profile.RequestTimeout, totalTimeout: profile.TotalTimeout,
		pollInterval: profile.PollInterval, maxPollAttempts: profile.MaxPollAttempts,
		maxResponseBytes: profile.MaxResponseBytes,
	}, nil
}

// Descriptor returns the immutable provider identity fixed by the profile.
func (client *Client) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return cloneDescriptor(client.descriptor)
}

// Render submits one exact upload and drives the bounded bridge job state machine.
func (client *Client) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (_ document.RenditionResult, retErr error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("bridge: client is required")
	}
	ctx, cancel := context.WithTimeout(ctx, client.totalTimeout)
	defer cancel()
	manifest := AuthorizationManifest{
		ContractVersion: ContractVersion, Source: upload.Metadata(), Authorization: authorization,
	}
	manifestJSON, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil {
		return document.RenditionResult{}, fmt.Errorf("bridge: encode authorization: %w", err)
	}
	idempotencyDigest := sha256.Sum256(manifestJSON)
	idempotencyKey := hex.EncodeToString(idempotencyDigest[:])
	envelope, err := client.submit(ctx, upload, manifestJSON, idempotencyKey)
	if err != nil {
		return document.RenditionResult{}, err
	}
	jobID := envelope.JobID
	completed := false
	defer func() {
		if jobID != "" && !completed {
			cancelCtx, cancelRemote := context.WithTimeout(context.WithoutCancel(ctx), client.requestTimeout)
			defer cancelRemote()
			_ = client.cancelJob(cancelCtx, jobID)
		}
	}()

	var retryDelay time.Duration
	for attempt := 0; ; attempt++ {
		switch envelope.Status {
		case JobCompleted:
			result, err := client.decodeCompleted(ctx, envelope, authorization)
			if err != nil {
				return document.RenditionResult{}, err
			}
			completed = true
			return result, nil
		case JobFailed, JobCanceled:
			return document.RenditionResult{}, providerErrorFromEnvelope(envelope)
		case JobQueued, JobRunning:
		default:
			return document.RenditionResult{}, malformedError("bridge returned an invalid job status", nil)
		}
		if attempt >= client.maxPollAttempts {
			return document.RenditionResult{}, classifiedError(
				document.RenditionErrorCapacity, "bridge polling limit reached", 0, nil)
		}
		delay := client.pollInterval
		if retryDelay > 0 {
			delay = min(retryDelay, client.requestTimeout)
			retryDelay = 0
		} else if envelope.RetryAfterMillis > 0 {
			delay = min(time.Duration(envelope.RetryAfterMillis)*time.Millisecond, client.requestTimeout)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return document.RenditionResult{}, ctx.Err()
		case <-timer.C:
		}
		envelope, err = client.getJob(ctx, jobID, authorization.SourceSHA256)
		if err != nil {
			if document.IsRenditionProviderErrorRetryable(err) {
				if providerError, ok := errors.AsType[*document.RenditionProviderError](err); ok {
					retryDelay = providerError.RetryAfter()
				}
				envelope = jobEnvelope{Status: JobRunning, JobID: jobID}
				continue
			}
			return document.RenditionResult{}, err
		}
	}
}

func (client *Client) submit(
	ctx context.Context, upload document.AuthorizedUpload, manifest []byte, idempotencyKey string,
) (jobEnvelope, error) {
	bodyReader, bodyWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(bodyWriter)
	contentType := multipartWriter.FormDataContentType()
	go func() {
		writeErr := writeMultipartUpload(multipartWriter, manifest, upload)
		if closeErr := multipartWriter.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = bodyWriter.CloseWithError(writeErr)
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.origin+jobsPath, bodyReader)
	if err != nil {
		_ = bodyReader.Close()
		return jobEnvelope{}, fmt.Errorf("bridge: create submission: %w", err)
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", jobMediaType)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	envelope, status, err := client.doJobRequest(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return jobEnvelope{}, contextErr
		}
		if _, ok := errors.AsType[*document.RenditionProviderError](err); ok {
			return jobEnvelope{}, err
		}
		return jobEnvelope{}, classifiedError(document.RenditionErrorAmbiguousSubmission,
			"bridge submission outcome is ambiguous", 0, err)
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return jobEnvelope{}, statusError(status, envelope)
	}
	if err := client.validateEnvelope(envelope, upload.Metadata()); err != nil {
		return jobEnvelope{}, err
	}
	if status == http.StatusOK && envelope.Status != JobCompleted {
		return jobEnvelope{}, malformedError("bridge synchronous response is not completed", nil)
	}
	if status == http.StatusAccepted && envelope.Status != JobQueued && envelope.Status != JobRunning {
		return jobEnvelope{}, malformedError("bridge accepted response has an invalid status", nil)
	}
	return envelope, nil
}

func writeMultipartUpload(
	writer *multipart.Writer, manifest []byte, upload document.AuthorizedUpload,
) error {
	manifestHeader := make(textproto.MIMEHeader)
	manifestHeader.Set("Content-Disposition", `form-data; name="`+authorizationPartName+`"`)
	manifestHeader.Set("Content-Type", "application/vnd.docbank.rendition-authorization+json;version=1")
	manifestPart, err := writer.CreatePart(manifestHeader)
	if err != nil {
		return fmt.Errorf("bridge: create authorization multipart part: %w", err)
	}
	if _, err := manifestPart.Write(manifest); err != nil {
		return err
	}
	metadata := upload.Metadata()
	sourceHeader := make(textproto.MIMEHeader)
	sourceHeader.Set("Content-Disposition", multipart.FileContentDisposition(sourcePartName, metadata.Filename))
	sourceHeader.Set("Content-Type", metadata.MediaType)
	sourcePart, err := writer.CreatePart(sourceHeader)
	if err != nil {
		return fmt.Errorf("bridge: create source multipart part: %w", err)
	}
	written, err := io.Copy(sourcePart, io.LimitReader(upload, metadata.ByteLength+1))
	if err != nil {
		return err
	}
	if written != metadata.ByteLength {
		return errors.New("bridge: upload length changed during submission")
	}
	return nil
}

func (client *Client) getJob(ctx context.Context, jobID, sourceSHA256 string) (jobEnvelope, error) {
	if err := validateIdentifier(jobID, "job ID"); err != nil {
		return jobEnvelope{}, malformedError("bridge returned an invalid job ID", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.origin+jobsPath+"/"+jobID, nil)
	if err != nil {
		return jobEnvelope{}, err
	}
	request.Header.Set("Accept", jobMediaType)
	envelope, status, err := client.doJobRequest(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return jobEnvelope{}, contextErr
		}
		if _, ok := errors.AsType[*document.RenditionProviderError](err); ok {
			return jobEnvelope{}, err
		}
		return jobEnvelope{}, classifiedError(
			document.RenditionErrorTransient, "bridge polling request failed", 0, err)
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return jobEnvelope{}, statusError(status, envelope)
	}
	if err := client.validateEnvelope(envelope, document.AuthorizedUploadMetadata{
		SHA256: sourceSHA256,
	}); err != nil {
		return jobEnvelope{}, err
	}
	if envelope.JobID != jobID {
		return jobEnvelope{}, malformedError("bridge job identity changed while polling", nil)
	}
	return envelope, nil
}

func (client *Client) doJobRequest(request *http.Request) (jobEnvelope, int, error) {
	parentCtx := request.Context()
	requestCtx, cancel := context.WithTimeout(parentCtx, client.requestTimeout)
	defer cancel()
	request = request.Clone(requestCtx)
	if err := client.authorizeRequest(request); err != nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return jobEnvelope{}, 0, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := parentCtx.Err(); contextErr != nil {
			return jobEnvelope{}, 0, contextErr
		}
		return jobEnvelope{}, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		return jobEnvelope{}, response.StatusCode, nil
	}
	if err := requireMediaType(response.Header.Get("Content-Type"), jobMediaType); err != nil {
		return jobEnvelope{}, response.StatusCode, err
	}
	body, err := readBounded(response.Body, client.maxResponseBytes)
	if err != nil {
		return jobEnvelope{}, response.StatusCode, err
	}
	var envelope jobEnvelope
	if len(body) != 0 {
		if err := json.Unmarshal(body, &envelope); err != nil {
			return jobEnvelope{}, response.StatusCode, malformedError("bridge response JSON is invalid", err)
		}
	}
	return envelope, response.StatusCode, nil
}

func (client *Client) validateEnvelope(
	envelope jobEnvelope, metadata document.AuthorizedUploadMetadata,
) error {
	if envelope.ContractVersion != ContractVersion {
		return malformedError("bridge contract version is unsupported", nil)
	}
	if err := validateIdentifier(envelope.JobID, "job ID"); err != nil {
		return malformedError("bridge job ID is invalid", err)
	}
	if metadata.SHA256 != "" && envelope.SourceSHA256 != metadata.SHA256 {
		return malformedError("bridge source identity does not match upload", nil)
	}
	if envelope.AdapterID != client.descriptor.ID ||
		envelope.DescriptorFingerprint != client.descriptor.Fingerprint ||
		envelope.PolicyFingerprint != client.descriptor.PolicyFingerprint {
		return malformedError("bridge provider identity does not match profile", nil)
	}
	if envelope.RetryAfterMillis < 0 || envelope.RetryAfterMillis > int64(client.totalTimeout/time.Millisecond) {
		return malformedError("bridge retry delay is outside bounds", nil)
	}
	return nil
}

func (client *Client) decodeCompleted(
	ctx context.Context, envelope jobEnvelope, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if len(envelope.Result) == 0 {
		return document.RenditionResult{}, malformedError("bridge completed response lacks a result", nil)
	}
	var wire completedResult
	if err := json.Unmarshal(envelope.Result, &wire, json.RejectUnknownMembers(true)); err != nil {
		return document.RenditionResult{}, malformedError("bridge completed result has an unknown member or invalid value", err)
	}
	evidence, err := decodeEvidence(wire.Evidence)
	if err != nil {
		return document.RenditionResult{}, err
	}
	var markdown []byte
	if wire.ProviderMarkdown != nil {
		markdown, err = decodeInlinePayload(*wire.ProviderMarkdown, authorization.MaxProviderMarkdownBytes)
		if err != nil {
			return document.RenditionResult{}, malformedError("bridge provider Markdown is invalid", err)
		}
		if injectsDocbankFrontmatter(markdown) {
			return document.RenditionResult{}, malformedError("bridge provider Markdown attempts Docbank frontmatter injection", nil)
		}
	}
	artifactByteBudget, err := preflightArtifacts(
		evidence, markdown, wire.Artifacts, wire.Receipt, authorization, client.maxResponseBytes)
	if err != nil {
		return document.RenditionResult{}, err
	}
	artifacts := make([]document.RenditionArtifact, 0, len(wire.Artifacts))
	for _, artifact := range wire.Artifacts {
		payload, err := client.resolveArtifact(ctx, envelope.JobID, artifact,
			min(authorization.MaxArtifactBytes, int(artifactByteBudget)))
		if err != nil {
			return document.RenditionResult{}, err
		}
		artifactByteBudget -= int64(len(payload))
		artifacts = append(artifacts, document.RenditionArtifact{
			Role: artifact.Role, MediaType: artifact.MediaType, Payload: payload, SHA256: artifact.SHA256,
		})
	}
	result := document.RenditionResult{
		Evidence: evidence, ProviderMarkdown: markdown, Artifacts: artifacts, Receipt: wire.Receipt,
	}
	if err := document.ValidateRenditionResult(client.descriptor, authorization, result); err != nil {
		return document.RenditionResult{}, malformedError("bridge completed result failed validation", err)
	}
	return result, nil
}

func preflightArtifacts(
	evidence document.SourceEvidenceV1, markdown []byte, artifacts []artifactPayload,
	receipt document.RenditionReceipt, authorization document.RenditionAuthorization, maxResponseBytes int64,
) (int64, error) {
	if len(artifacts) > authorization.MaxArtifacts {
		return 0, malformedError("bridge artifact count exceeds authorization", nil)
	}
	projected := document.RenditionResult{
		Evidence: evidence, ProviderMarkdown: markdown, Receipt: receipt,
		Artifacts: make([]document.RenditionArtifact, 0, len(artifacts)),
	}
	for _, artifact := range artifacts {
		if !slices.Contains(authorization.AllowedArtifactRoles, artifact.Role) {
			return 0, malformedError("bridge artifact role is not authorized", nil)
		}
		if artifact.ByteLength < 0 || artifact.ByteLength > int64(authorization.MaxArtifactBytes) {
			return 0, malformedError("bridge artifact length is outside authorization", nil)
		}
		if artifact.ByteLength > maxResponseBytes {
			return 0, malformedError("bridge artifact length exceeds response byte limit", nil)
		}
		projected.Artifacts = append(projected.Artifacts, document.RenditionArtifact{
			Role: artifact.Role, MediaType: artifact.MediaType, Payload: []byte{}, SHA256: artifact.SHA256,
		})
	}
	encoded, err := json.Marshal(projected, json.Deterministic(true))
	if err != nil {
		return 0, malformedError("bridge completed result cannot be encoded", err)
	}
	remainingEncodedBytes := int64(authorization.MaxTotalResultBytes) - int64(len(encoded))
	for _, artifact := range artifacts {
		encodedPayloadBytes := ((artifact.ByteLength + 2) / 3) * 4
		if encodedPayloadBytes > remainingEncodedBytes {
			return 0, malformedError("bridge total result bytes exceed authorization", nil)
		}
		remainingEncodedBytes -= encodedPayloadBytes
	}
	return int64(authorization.MaxTotalResultBytes), nil
}

func decodeEvidence(payload evidencePayload) (document.SourceEvidenceV1, error) {
	if payload.MediaType != evidenceMediaType {
		return document.SourceEvidenceV1{}, malformedError("bridge evidence content type is invalid", nil)
	}
	if payload.ByteLength != int64(len(payload.Inline)) {
		return document.SourceEvidenceV1{}, malformedError("bridge evidence length does not match declaration", nil)
	}
	if payload.SHA256 != sha256Hex(payload.Inline) {
		return document.SourceEvidenceV1{}, malformedError("bridge evidence checksum does not match declaration", nil)
	}
	var evidence document.SourceEvidenceV1
	if err := json.Unmarshal(payload.Inline, &evidence, json.RejectUnknownMembers(true)); err != nil {
		return document.SourceEvidenceV1{}, malformedError("bridge source evidence is invalid", err)
	}
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		return document.SourceEvidenceV1{}, malformedError("bridge source evidence failed validation", err)
	}
	return evidence, nil
}

func (client *Client) resolveArtifact(
	ctx context.Context, artifactJobID string, artifact artifactPayload, maxBytes int,
) ([]byte, error) {
	if artifact.ByteLength < 0 || artifact.ByteLength > int64(maxBytes) {
		return nil, malformedError("bridge artifact length is outside authorization", nil)
	}
	if artifact.Location == "inline" {
		if artifact.ArtifactID != "" {
			return nil, malformedError("bridge inline artifact has a result identity", nil)
		}
		return decodeInlinePayload(binaryPayloadRecord{
			MediaType: artifact.MediaType, ByteLength: artifact.ByteLength,
			SHA256: artifact.SHA256, InlineBase64: artifact.InlineBase64,
		}, maxBytes)
	}
	if artifact.Location != "result" || artifact.InlineBase64 != "" {
		return nil, malformedError("bridge artifact location is invalid", nil)
	}
	if err := validateIdentifier(artifact.ArtifactID, "artifact ID"); err != nil {
		return nil, malformedError("bridge artifact ID is invalid", err)
	}
	return client.fetchArtifact(ctx, artifactJobID, artifact)
}

func (client *Client) fetchArtifact(
	ctx context.Context, jobID string, artifact artifactPayload,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		client.origin+jobsPath+"/"+jobID+"/artifacts/"+artifact.ArtifactID, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", artifact.MediaType)
	parentCtx := request.Context()
	requestCtx, cancel := context.WithTimeout(parentCtx, client.requestTimeout)
	defer cancel()
	request = request.Clone(requestCtx)
	if err := client.authorizeRequest(request); err != nil {
		return nil, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := parentCtx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, classifiedError(
			document.RenditionErrorTransient, "bridge artifact request failed", 0, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, statusError(response.StatusCode, jobEnvelope{})
	}
	if err := requireMediaType(response.Header.Get("Content-Type"), artifact.MediaType); err != nil {
		return nil, err
	}
	if response.ContentLength >= 0 && response.ContentLength != artifact.ByteLength {
		return nil, malformedError("bridge artifact HTTP length does not match declaration", nil)
	}
	payload, err := readBounded(response.Body, min(artifact.ByteLength, client.maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) != artifact.ByteLength {
		return nil, malformedError("bridge artifact length does not match declaration", nil)
	}
	if sha256Hex(payload) != artifact.SHA256 {
		return nil, malformedError("bridge artifact checksum does not match declaration", nil)
	}
	return payload, nil
}

func (client *Client) cancelJob(ctx context.Context, jobID string) error {
	if err := validateIdentifier(jobID, "job ID"); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, client.origin+jobsPath+"/"+jobID, nil)
	if err != nil {
		return err
	}
	if err := client.authorizeRequest(request); err != nil {
		return err
	}
	response, err := client.http.Do(request)
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

func (client *Client) authorizeRequest(request *http.Request) error {
	if client.secretBinding == "" {
		return nil
	}
	secret, err := client.secrets.ResolveSecret(request.Context(), client.secretBinding)
	if err != nil {
		return classifiedError(document.RenditionErrorAuthentication,
			"bridge credential is unavailable", 0, err)
	}
	if secret == "" || len(secret) > maxBridgeSecret || strings.ContainsAny(secret, "\r\n\x00") {
		return classifiedError(document.RenditionErrorAuthentication,
			"bridge credential is invalid", 0, nil)
	}
	request.Header.Set("Authorization", "Bearer "+secret)
	return nil
}

func validateOrigin(raw string, trust document.RenditionTrustBoundary) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Opaque != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("bridge: origin must be one absolute origin without path, credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || trust != document.RenditionTrustOperatorNetwork) {
		return "", errors.New("bridge: hosted origins require HTTPS; HTTP is operator-network only")
	}
	if trust != document.RenditionTrustOperatorNetwork && trust != document.RenditionTrustHostedProvider {
		return "", errors.New("bridge: network origin requires an operator-network or hosted trust boundary")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func validateIdentifier(value, subject string) error {
	if value == "" || len(value) > maxBridgeIdentifier || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("bridge: %s is invalid", subject)
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return fmt.Errorf("bridge: %s is invalid", subject)
		}
	}
	return nil
}

func cloneDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = append([]document.RenditionFormatCapability(nil), value.SupportedFormats...)
	value.ArtifactRoles = append([]document.EvidenceArtifactRole(nil), value.ArtifactRoles...)
	return value
}

func decodeInlinePayload(value binaryPayloadRecord, maxBytes int) ([]byte, error) {
	if value.ByteLength < 0 || value.ByteLength > int64(maxBytes) {
		return nil, malformedError("inline payload length is outside authorization", nil)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(value.InlineBase64)
	if err != nil {
		return nil, malformedError("inline payload base64 is invalid", err)
	}
	if int64(len(payload)) != value.ByteLength {
		return nil, malformedError("inline payload length does not match declaration", nil)
	}
	if sha256Hex(payload) != value.SHA256 {
		return nil, malformedError("inline payload checksum does not match declaration", nil)
	}
	if _, _, err := mime.ParseMediaType(value.MediaType); err != nil {
		return nil, malformedError("inline payload media type is invalid", err)
	}
	return payload, nil
}

func injectsDocbankFrontmatter(markdown []byte) bool {
	prefix := markdown
	if len(prefix) > 4096 {
		prefix = prefix[:4096]
	}
	return bytes.HasPrefix(prefix, []byte("---\n")) &&
		bytes.Contains(prefix, []byte("docbank-sanitized-markdown/v1"))
}

func requireMediaType(got, want string) error {
	gotType, gotParams, err := mime.ParseMediaType(got)
	if err != nil {
		return malformedError("bridge response content type is invalid", err)
	}
	wantType, wantParams, err := mime.ParseMediaType(want)
	if err != nil || gotType != wantType || !reflect.DeepEqual(gotParams, wantParams) {
		return malformedError("bridge response content type does not match protocol", err)
	}
	return nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	if maximum < 0 {
		return nil, errors.New("bridge: negative response bound")
	}
	value, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > maximum {
		return nil, malformedError("bridge response exceeds byte limit", nil)
	}
	return value, nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func providerErrorFromEnvelope(envelope jobEnvelope) error {
	if envelope.Status == JobCanceled && len(envelope.Error) == 0 {
		return classifiedError(document.RenditionErrorCanceled, "bridge job was canceled", 0, nil)
	}
	if len(envelope.Error) == 0 {
		return malformedError("bridge failed response lacks a stable error", nil)
	}
	var providerError bridgeError
	if err := json.Unmarshal(envelope.Error, &providerError, json.RejectUnknownMembers(true)); err != nil {
		return malformedError("bridge failed response has an unknown member or invalid value", err)
	}
	if providerError.RetryAfterMillis < 0 ||
		providerError.RetryAfterMillis > int64(maxBridgeTimeout/time.Millisecond) {
		return malformedError("bridge error retry delay is outside bounds", nil)
	}
	retry := time.Duration(providerError.RetryAfterMillis) * time.Millisecond
	return classifiedError(providerError.Code, providerError.Message, retry, nil)
}

func statusError(status int, envelope jobEnvelope) error {
	if len(envelope.Error) != 0 {
		return providerErrorFromEnvelope(envelope)
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return classifiedError(document.RenditionErrorAuthentication, "bridge authentication failed", 0, nil)
	case http.StatusNotFound, http.StatusGone:
		return classifiedError(document.RenditionErrorUnknownJob, "bridge job is unknown or expired", 0, nil)
	case http.StatusTooManyRequests:
		return classifiedError(document.RenditionErrorRateLimited, "bridge rate limit", 0, nil)
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return classifiedError(document.RenditionErrorTransient, "bridge is temporarily unavailable", 0, nil)
	default:
		return malformedError("bridge returned unexpected HTTP status "+strconv.Itoa(status), nil)
	}
}

func malformedError(message string, cause error) error {
	return classifiedError(document.RenditionErrorMalformedEvidence, message, 0, cause)
}

func classifiedError(
	code document.RenditionErrorCode, message string, retry time.Duration, cause error,
) error {
	if cause == nil {
		cause = errors.New(message)
	} else {
		cause = fmt.Errorf("%s: %w", message, cause)
	}
	providerError, err := document.NewRenditionProviderError(code, retry, cause)
	if err != nil {
		fallback, fallbackErr := document.NewRenditionProviderError(
			document.RenditionErrorMalformedEvidence, 0,
			fmt.Errorf("bridge returned an invalid error: %w", err))
		if fallbackErr == nil {
			return fallback
		}
		return errors.Join(err, fallbackErr)
	}
	return providerError
}
