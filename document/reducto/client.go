package reducto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const (
	apiHost    = "platform.reducto.ai"
	apiOrigin  = "https://" + apiHost
	uploadPath = "/upload"
	parsePath  = "/parse_async"
	providerID = "reducto.parse-v1"
	timeForm   = "2006-01-02T15:04:05.000000000Z"

	// The official Go SDK does not expose a selectable model. These constants
	// bind the adapter to its immutable reviewed parse operation and schema.
	sdkSchemaCommit = "a000fe10d8db34a4bde067f3c55a04f208c68e1e"
	modelIdentity   = "reducto-parse"
	profileID       = "go-sdk-" + sdkSchemaCommit + "-page-v2"

	defaultMaxUploadBytes   = int64(100 << 20)
	defaultRequestOverhead  = int64(1 << 20)
	defaultMaxControlBytes  = int64(64 << 10)
	defaultMaxResultBytes   = int64(64 << 20)
	defaultMaxArtifactBytes = int64(64 << 20)
	defaultMaxPolls         = 900
	defaultPollInterval     = 2 * time.Second
	defaultRequestTimeout   = 30 * time.Second
	defaultMaxWallTime      = 30 * time.Minute

	maxConfiguredBytes    = int64(512 << 20)
	maxConfiguredPolls    = 100_000
	maxConfiguredDuration = 24 * time.Hour
	maxCredentialBytes    = 4096
	maxUploadTokenBytes   = 256
	maxJobTokenBytes      = 120
	resumeHandlePrefix    = "r2."
	resumeHandleVersion   = "reducto-resume/v2"
	maxResumeUsageValue   = int64(1 << 50)
	maxNaturalUnits       = 100_000
	markdownUnitSeparator = "\n\n---\n\n"
)

var (
	_ document.RenditionProvider          = (*Client)(nil)
	_ document.ResumableRenditionProvider = (*Client)(nil)

	errResponseTooLarge = errors.New("reducto response exceeds byte limit")
)

// SecretResolver resolves the one credential named by the frozen profile.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, name string) (string, error)
}

// Profile freezes all request, polling, result, artifact, and wall-clock
// bounds. The provider origin, routes, model identity, and parse options are
// deliberately not configurable.
type Profile struct {
	SecretBinding    string
	MaxUploadBytes   int64
	MaxRequestBytes  int64
	MaxControlBytes  int64
	MaxPolls         int
	PollInterval     time.Duration
	RequestTimeout   time.Duration
	MaxResultBytes   int64
	MaxArtifactBytes int64
	MaxArtifacts     int
	MaxWallTime      time.Duration
	RetainStructured bool
}

// Client is a fixed-origin hosted Reducto rendition provider.
type Client struct {
	descriptor document.RenditionDescriptor
	profile    Profile
	secrets    SecretResolver
	transport  http.RoundTripper
}

// NewProvider constructs a provider around an injected hardened transport.
// It does not consult proxy environment variables, resolve credentials, or
// perform network access during construction.
func NewProvider(profile Profile, secrets SecretResolver, transport http.RoundTripper) (*Client, error) {
	profile = defaultProfile(profile)
	if err := validateProfile(profile); err != nil {
		return nil, err
	}
	if nilValue(secrets) {
		return nil, errors.New("reducto named credential resolver is required")
	}
	if nilValue(transport) {
		return nil, errors.New("reducto hardened transport is required")
	}
	identity := fmt.Sprintf(
		"reducto-profile/v1\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%t",
		modelIdentity, profileID, profile.SecretBinding, profile.MaxUploadBytes,
		profile.MaxRequestBytes, profile.MaxControlBytes, profile.MaxPolls,
		profile.PollInterval, profile.RequestTimeout, profile.MaxResultBytes,
		profile.MaxArtifactBytes, profile.MaxWallTime, profile.RetainStructured,
	)
	digest := sha256.Sum256([]byte(identity))
	roles := []document.EvidenceArtifactRole(nil)
	if profile.RetainStructured {
		roles = []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}
	}
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID:                providerID,
		ContractVersion:   document.RenditionProviderContractVersion,
		PolicyFingerprint: hex.EncodeToString(digest[:]),
		TrustBoundary:     document.RenditionTrustHostedProvider,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsMarkdown: true, ReturnsStructured: true, ArtifactRoles: roles,
	})
	if err != nil {
		return nil, fmt.Errorf("reducto descriptor: %w", err)
	}
	return &Client{
		descriptor: cloneDescriptor(descriptor), profile: profile, secrets: secrets,
		transport: fixedOriginTransport{base: transport},
	}, nil
}

// Descriptor returns a defensive copy of the immutable provider identity.
func (client *Client) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return cloneDescriptor(client.descriptor)
}

// Render starts and completes one hosted asynchronous parse operation.
func (client *Client) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	return client.RenderResumable(ctx, upload, authorization, nil, nil)
}

// RenderResumable submits exact authorized bytes or resumes one known opaque
// job handle. A newly accepted job is checkpointed before its first poll.
func (client *Client) RenderResumable(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization, resume *document.RenditionResumeHandle,
	checkpoint document.RenditionResumeCheckpoint,
) (document.RenditionResult, error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("reducto client is required")
	}
	startedAt := time.Now().UTC()
	expiresAt, resumeFacts, err := client.validateInvocation(startedAt, upload, authorization, resume)
	if err != nil {
		return document.RenditionResult{}, err
	}
	operationExpiry := expiresAt
	if resume != nil {
		operationExpiry = time.Time{}
	}
	operationCtx, cancel := boundedOperationContext(ctx, operationExpiry, client.profile.MaxWallTime)
	defer cancel()
	state := operationState{startedAt: startedAt, inputBytes: authorization.SourceBytes}
	var jobID string
	if resume == nil {
		fileID, submitErr := client.upload(operationCtx, ctx, expiresAt, upload, &state)
		if submitErr != nil {
			return document.RenditionResult{}, submitErr
		}
		jobID, submitErr = client.submit(operationCtx, ctx, expiresAt, fileID, &state)
		if submitErr != nil {
			return document.RenditionResult{}, submitErr
		}
		checkpointedAt := time.Now().UTC()
		if !checkpointedAt.Before(expiresAt) {
			return document.RenditionResult{}, renditionError(document.RenditionErrorAmbiguousSubmission,
				"Reducto job was accepted after the durable authorization interval", nil)
		}
		state.submittedAt = checkpointedAt
		if checkpointErr := client.checkpoint(checkpoint, jobID, &state); checkpointErr != nil {
			return document.RenditionResult{}, checkpointErr
		}
	} else {
		jobID = resumeFacts.JobID
		state.startedAt, _ = time.Parse(timeForm, resumeFacts.StartedAt)
		state.submittedAt, _ = time.Parse(timeForm, resumeFacts.SubmittedAt)
		state.requests = resumeFacts.Requests
		state.retries = resumeFacts.Retries
		state.inputBytes = resumeFacts.InputBytes
		state.outputBytes = resumeFacts.OutputBytes
		state.pollDelay = time.Duration(resumeFacts.RetryDelayMillis) * time.Millisecond
		state.durableCheckpoint = true
	}
	result, err := client.poll(
		operationCtx, ctx, operationExpiry, jobID, authorization, checkpoint, &state)
	if err != nil {
		return document.RenditionResult{}, err
	}
	if err := operationCtx.Err(); err != nil {
		return document.RenditionResult{}, client.resultFailure(
			ctx, operationCtx, operationExpiry, state.durableCheckpoint,
			"Reducto operation ended after its lifecycle boundary", err)
	}
	completedAt := time.Now().UTC()
	if resume == nil && !completedAt.Before(expiresAt) {
		if state.durableCheckpoint {
			return document.RenditionResult{}, renditionError(document.RenditionErrorTransient,
				"Reducto durable result reached its authorization boundary", nil)
		}
		return document.RenditionResult{}, expiredError(nil)
	}
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, fmt.Errorf("reducto: fingerprint authorization: %w", err)
	}
	result.Receipt = document.RenditionReceipt{
		ProviderID: client.descriptor.ID, DescriptorFingerprint: client.descriptor.Fingerprint,
		PolicyFingerprint:           authorization.PolicyFingerprint,
		RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
		AuthorizationFingerprint:    authorizationFingerprint, SourceSHA256: authorization.SourceSHA256,
		OperationID: "reducto-" + jobID,
		StartedAt:   state.startedAt.Format(timeForm), CompletedAt: completedAt.Format(timeForm),
		Warnings: state.warnings,
		Usage: document.RenditionUsage{
			Requests: state.requests, Retries: state.retries, InputBytes: state.inputBytes,
			OutputBytes: state.outputBytes, Units: int64(len(result.Evidence.Units)),
		},
		RetryDelayMillis: state.pollDelay.Milliseconds(),
	}
	return result, nil
}

func (client *Client) validateInvocation(
	now time.Time, upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
	resume *document.RenditionResumeHandle,
) (time.Time, resumePayload, error) {
	expiresAt, err := time.Parse(timeForm, authorization.ExpiresAt)
	if err != nil {
		return time.Time{}, resumePayload{}, renditionError(document.RenditionErrorPolicyRejected,
			"Reducto authorization expiry is invalid", err)
	}
	if resume == nil {
		if nilValue(upload) {
			return time.Time{}, resumePayload{}, errors.New("reducto authorized upload is required for submission")
		}
		if _, err := document.ValidateRenditionProviderRequestAt(now, client, upload, authorization); err != nil {
			return time.Time{}, resumePayload{}, err
		}
		return expiresAt, resumePayload{}, nil
	}
	if !nilValue(upload) {
		return time.Time{}, resumePayload{}, errors.New("reducto resume must not receive source bytes")
	}
	facts, decodeErr := decodeResumeHandle(resume.Value, authorization)
	if decodeErr != nil {
		return time.Time{}, resumePayload{}, renditionError(document.RenditionErrorUnknownJob,
			"Reducto resume handle is invalid", decodeErr)
	}
	submittedAt, _ := time.Parse(timeForm, facts.SubmittedAt)
	sealed := resumeAuthorizationUpload{metadata: document.AuthorizedUploadMetadata{
		Filename: "resume.input", MediaFamily: authorization.MediaFamily,
		MediaType: authorization.MediaType, ByteLength: authorization.SourceBytes,
		SHA256:                   authorization.SourceSHA256,
		CapabilityRecordChecksum: authorization.CapabilityRecordChecksum,
		ProviderMetadataChecksum: authorization.ProviderMetadataChecksum,
		InputKind:                authorization.InputKind,
	}}
	if _, err := document.ValidateRenditionProviderRequestAt(submittedAt, client, sealed, authorization); err != nil {
		return time.Time{}, resumePayload{}, renditionError(document.RenditionErrorPolicyRejected,
			"Reducto resume authority is invalid", err)
	}
	if authorization.ProviderID != client.descriptor.ID ||
		authorization.DescriptorFingerprint != client.descriptor.Fingerprint ||
		authorization.PolicyFingerprint != client.descriptor.PolicyFingerprint {
		return time.Time{}, resumePayload{}, renditionError(document.RenditionErrorPolicyRejected,
			"Reducto resume authority changed", nil)
	}
	return expiresAt, facts, nil
}

func (client *Client) upload(
	operationCtx, callerCtx context.Context, expiresAt time.Time,
	upload document.AuthorizedUpload, state *operationState,
) (string, error) {
	metadata := upload.Metadata()
	if metadata.ByteLength > client.profile.MaxUploadBytes {
		return "", renditionError(document.RenditionErrorPolicyRejected,
			"Reducto upload exceeds profile limit", nil)
	}
	source, err := readExactUpload(
		operationCtx, callerCtx, expiresAt, upload, metadata, client.profile.MaxUploadBytes)
	if err != nil {
		return "", err
	}
	defer clear(source)
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name": "file", "filename": metadata.Filename,
	}))
	header.Set("Content-Type", metadata.MediaType)
	part, err := writer.CreatePart(header)
	if err == nil {
		_, err = part.Write(source)
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", renditionError(document.RenditionErrorTransient,
			"Reducto upload request could not be built", err)
	}
	if int64(body.Len()) > client.profile.MaxRequestBytes {
		return "", renditionError(document.RenditionErrorPolicyRejected,
			"Reducto upload request exceeds profile limit", nil)
	}
	requestBody := bytes.Clone(body.Bytes())
	defer clear(requestBody)
	raw, status, err := client.do(operationCtx, http.MethodPost, uploadPath,
		writer.FormDataContentType(), requestBody, client.profile.MaxControlBytes, state)
	if err != nil {
		return "", client.submissionFailure(callerCtx, operationCtx, expiresAt,
			"Reducto upload outcome is ambiguous", err)
	}
	if status < 200 || status >= 300 {
		return "", client.httpError(status, "upload")
	}
	var result uploadResponse
	if err := strictJSON(raw, &result); err != nil || validateUploadToken(result.FileID) != nil {
		return "", renditionError(document.RenditionErrorPolicyRejected,
			"Reducto upload schema changed", err)
	}
	return result.FileID, nil
}

func (client *Client) submit(
	operationCtx, callerCtx context.Context, expiresAt time.Time, fileID string,
	state *operationState,
) (string, error) {
	request := parseRequest{DocumentURL: fileID, Priority: false}
	request.AdvancedOptions.AddPageMarkers = true
	request.AdvancedOptions.ReturnOCRData = false
	request.Options.Chunking.ChunkMode = "page"
	request.Options.ForceURLResult = false
	body, err := json.Marshal(request)
	if err != nil {
		return "", renditionError(document.RenditionErrorTransient,
			"Reducto parse request could not be built", err)
	}
	if int64(len(body)) > client.profile.MaxRequestBytes {
		return "", renditionError(document.RenditionErrorPolicyRejected,
			"Reducto parse request exceeds profile limit", nil)
	}
	defer clear(body)
	raw, status, err := client.do(operationCtx, http.MethodPost, parsePath,
		"application/json", body, client.profile.MaxControlBytes, state)
	if err != nil {
		return "", client.submissionFailure(callerCtx, operationCtx, expiresAt,
			"Reducto parse submission outcome is ambiguous", err)
	}
	if status < 200 || status >= 300 {
		return "", client.httpError(status, "submission")
	}
	var result parseResponse
	if err := strictJSON(raw, &result); err != nil || validateJobToken(result.JobID) != nil {
		return "", renditionError(document.RenditionErrorAmbiguousSubmission,
			"Reducto submission schema changed", err)
	}
	return result.JobID, nil
}

func (client *Client) poll(
	operationCtx, callerCtx context.Context, expiresAt time.Time, jobID string,
	authorization document.RenditionAuthorization, checkpoint document.RenditionResumeCheckpoint,
	state *operationState,
) (document.RenditionResult, error) {
	limit := min64(client.profile.MaxResultBytes, int64(authorization.MaxTotalResultBytes))
	for attempt := range client.profile.MaxPolls {
		raw, status, err := client.do(operationCtx, http.MethodGet, jobPath(jobID), "", nil, limit, state)
		if checkpointErr := client.checkpoint(checkpoint, jobID, state); checkpointErr != nil {
			return document.RenditionResult{}, checkpointErr
		}
		if err != nil {
			return document.RenditionResult{}, client.resultFailure(
				callerCtx, operationCtx, expiresAt, state.durableCheckpoint,
				"Reducto job request failed", err)
		}
		if status < 200 || status >= 300 {
			return document.RenditionResult{}, client.httpError(status, "job")
		}
		var job jobResponse
		if err := requireMembers(raw, "status"); err != nil {
			return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
				"Reducto job schema changed", err)
		}
		if err := strictJSON(raw, &job); err != nil || !validProgress(job.Progress) {
			return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
				"Reducto job schema changed", err)
		}
		switch job.Status {
		case "Pending", "Idle":
			if !isNullJSON(job.Result) {
				return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
					"Reducto pending job schema changed", nil)
			}
			if attempt+1 == client.profile.MaxPolls {
				break
			}
			if err := waitContext(operationCtx, client.profile.PollInterval); err != nil {
				return document.RenditionResult{}, client.pollFailure(
					callerCtx, operationCtx, expiresAt, state.durableCheckpoint, err)
			}
			state.pollDelay += client.profile.PollInterval
			if checkpointErr := client.checkpoint(checkpoint, jobID, state); checkpointErr != nil {
				return document.RenditionResult{}, checkpointErr
			}
		case "Completed":
			return client.completed(job.Result, jobID, authorization)
		case "Failed":
			return document.RenditionResult{}, renditionError(document.RenditionErrorUnsupportedInput,
				"Reducto could not parse the input", nil)
		default:
			return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
				"Reducto job status schema changed", nil)
		}
	}
	return document.RenditionResult{}, renditionError(document.RenditionErrorTransient,
		"Reducto polling limit was reached", nil)
}

func (client *Client) completed(
	raw []byte, jobID string, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if isNullJSON(raw) {
		return document.RenditionResult{}, malformedError("Reducto completed without a result", nil)
	}
	if err := requireMembers(raw, "result", "usage", "duration", "job_id"); err != nil {
		return document.RenditionResult{}, malformedError("Reducto completed result schema changed", err)
	}
	var completed completedResult
	if err := strictJSON(raw, &completed); err != nil {
		return document.RenditionResult{}, malformedError("Reducto completed result schema changed", err)
	}
	if completed.JobID != jobID {
		return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
			"Reducto completed job identity changed", nil)
	}
	if math.IsNaN(completed.Duration) || math.IsInf(completed.Duration, 0) || completed.Duration < 0 {
		return document.RenditionResult{}, malformedError("Reducto result duration is invalid", nil)
	}
	if completed.Usage.NumPages == nil || *completed.Usage.NumPages < 0 {
		return document.RenditionResult{}, malformedError("Reducto page usage is invalid", nil)
	}
	if err := requireMembers(completed.Result, "type"); err != nil {
		return document.RenditionResult{}, malformedError("Reducto parse result schema changed", err)
	}
	var parsed parseResult
	if err := strictJSON(completed.Result, &parsed); err != nil {
		return document.RenditionResult{}, malformedError("Reducto parse result schema changed", err)
	}
	if parsed.Type == "url" {
		if err := requireMembers(completed.Result, "result_id", "url"); err != nil ||
			parsed.ResultID == "" || parsed.URL == "" {
			return document.RenditionResult{}, malformedError("Reducto URL result schema changed", err)
		}
		return document.RenditionResult{}, malformedError("Reducto returned a provider URL instead of inline output", nil)
	}
	if parsed.Type != "full" || len(parsed.Chunks) == 0 {
		return document.RenditionResult{}, malformedError("Reducto parse result type changed", nil)
	}
	if parsed.Custom != nil || parsed.OCR != nil {
		return document.RenditionResult{}, malformedError("Reducto returned unrequested nested output", nil)
	}
	var rawChunks []jsontext.Value
	if err := strictJSON(parsed.Chunks, &rawChunks); err != nil || len(rawChunks) == 0 {
		return document.RenditionResult{}, malformedError("Reducto chunk schema changed", err)
	}
	chunks := make([]resultChunk, len(rawChunks))
	for index, rawChunk := range rawChunks {
		if err := requireMembers(rawChunk, "blocks", "content", "embed", "enriched"); err != nil {
			return document.RenditionResult{}, malformedError("Reducto chunk schema changed", err)
		}
		if err := strictJSON(rawChunk, &chunks[index]); err != nil {
			return document.RenditionResult{}, malformedError("Reducto chunk schema changed", err)
		}
	}
	evidence, markdown, err := naturalEvidence(chunks, authorization.MediaFamily, *completed.Usage.NumPages)
	if err != nil {
		return document.RenditionResult{}, malformedError("Reducto output is partial or malformed", err)
	}
	if len(markdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, malformedError("Reducto Markdown exceeds authorization", nil)
	}
	result := document.RenditionResult{Evidence: evidence, ProviderMarkdown: markdown}
	if client.profile.RetainStructured && allowsStructured(authorization) {
		maximum := min64(client.profile.MaxArtifactBytes, int64(authorization.MaxArtifactBytes))
		if int64(len(raw)) > maximum {
			return document.RenditionResult{}, malformedError("Reducto structured result exceeds authorization", nil)
		}
		payload := bytes.Clone(raw)
		digest := sha256.Sum256(payload)
		checksum := hex.EncodeToString(digest[:])
		result.Artifacts = []document.RenditionArtifact{{
			Role: document.EvidenceArtifactStructured, MediaType: "application/json",
			Payload: payload, SHA256: checksum,
		}}
		result.Evidence.Artifacts = []document.SourceEvidenceArtifactV1{{
			Pointer: "result", ProviderID: "reducto-result", Role: document.EvidenceArtifactStructured,
			SHA256: checksum,
		}}
	}
	encodedEvidence, err := json.Marshal(result.Evidence)
	if err != nil {
		return document.RenditionResult{}, malformedError("Reducto evidence could not be bounded", err)
	}
	total := len(encodedEvidence) + len(result.ProviderMarkdown)
	for _, artifact := range result.Artifacts {
		total += len(artifact.Payload)
	}
	if total > authorization.MaxTotalResultBytes {
		return document.RenditionResult{}, malformedError("Reducto total result exceeds authorization", nil)
	}
	if err := document.ValidateSourceEvidenceV1(result.Evidence); err != nil {
		return document.RenditionResult{}, malformedError("Reducto evidence is unrepresentable", err)
	}
	return result, nil
}

func naturalEvidence(
	chunks []resultChunk, family string, reportedUnits int64,
) (document.SourceEvidenceV1, []byte, error) {
	if family == "spreadsheet" {
		return document.SourceEvidenceV1{}, nil, errors.New("provider did not report a stable sheet name")
	}
	unitKind, locatorKind, ok := familyUnit(family)
	if !ok || reportedUnits <= 0 {
		return document.SourceEvidenceV1{}, nil, errors.New("natural unit family or count is invalid")
	}
	if reportedUnits > maxNaturalUnits {
		return document.SourceEvidenceV1{}, nil, errors.New("natural unit count exceeds provider bound")
	}
	units := make([][]string, int(reportedUnits))
	lastPage := int64(0)
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk.Content) || len(chunk.Blocks) == 0 {
			return document.SourceEvidenceV1{}, nil, errors.New("chunk is incomplete")
		}
		chunkPage := int64(0)
		for _, block := range chunk.Blocks {
			if err := validateBlockLocation(block); err != nil {
				return document.SourceEvidenceV1{}, nil, err
			}
			if chunk.Content == "" && block.Content != "" {
				return document.SourceEvidenceV1{}, nil, errors.New("contentless chunk contains unassigned block text")
			}
			if chunk.Content != "" && (block.Content == "" || !utf8.ValidString(block.Content)) {
				return document.SourceEvidenceV1{}, nil, errors.New("block content is invalid")
			}
			page := *block.BBox.Page
			if chunkPage == 0 {
				chunkPage = page
			} else if page != chunkPage {
				return document.SourceEvidenceV1{}, nil, errors.New("page chunk spans multiple natural units")
			}
		}
		if chunkPage < lastPage || chunkPage > reportedUnits {
			return document.SourceEvidenceV1{}, nil, errors.New("block unit sequence is incomplete")
		}
		lastPage = chunkPage
		if chunk.Content != "" {
			units[chunkPage-1] = append(units[chunkPage-1], chunk.Content)
		}
	}
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: family, UnitKind: unitKind,
		Units: make([]document.SourceEvidenceUnitV1, 0, len(units)),
	}
	markdown := make([]string, 0, len(units))
	for index, parts := range units {
		if len(parts) == 0 {
			locator := document.SourceEvidenceLocatorV1{
				Kind: locatorKind, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: int64(index + 1), End: int64(index + 1),
			}
			evidence.Omissions = append(evidence.Omissions, document.SourceEvidenceOmissionV1{
				Kind: document.EvidenceOmissionUnit, Locator: &locator,
				Reason: "Reducto returned no content for the reported natural unit",
			})
			continue
		}
		text := strings.Join(parts, "\n\n")
		locator := document.SourceEvidenceLocatorV1{
			Kind: locatorKind, IndexOrigin: document.EvidenceIndexOriginOne,
			Start: int64(index + 1), End: int64(index + 1),
		}
		evidence.Units = append(evidence.Units, document.SourceEvidenceUnitV1{
			Order: len(evidence.Units), Text: text, Locator: locator,
		})
		markdown = append(markdown, text)
	}
	if len(evidence.Units) == 0 {
		return document.SourceEvidenceV1{}, nil, errors.New("all natural units are missing")
	}
	if len(evidence.Omissions) != 0 {
		evidence.Completeness = document.EvidencePartial
	}
	return evidence, []byte(strings.Join(markdown, markdownUnitSeparator)), nil
}

func validateBlockLocation(block resultBlock) error {
	if !knownBlockType(block.Type) {
		return errors.New("block content or type is invalid")
	}
	box := block.BBox
	if box.Height == nil || box.Left == nil || box.Page == nil || box.Top == nil || box.Width == nil ||
		!finite(*box.Height) || !finite(*box.Left) || !finite(*box.Top) || !finite(*box.Width) ||
		*box.Height < 0 || *box.Width < 0 || *box.Page <= 0 ||
		(box.OriginalPage != nil && *box.OriginalPage <= 0) {
		return errors.New("block bounding box is invalid")
	}
	return nil
}

func knownBlockType(value string) bool {
	switch value {
	case "Header", "Footer", "Title", "Section Header", "Page Number", "List Item",
		"Figure", "Table", "Key Value", "Text", "Comment", "Discard":
		return true
	default:
		return false
	}
}

func (client *Client) do(
	ctx context.Context, method, path, contentType string, body []byte, limit int64,
	state *operationState,
) ([]byte, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, &preEgressContextError{cause: err}
	}
	credential, err := client.secrets.ResolveSecret(ctx, client.profile.SecretBinding)
	if err != nil && ctx.Err() != nil {
		return nil, 0, &preEgressContextError{cause: ctx.Err()}
	}
	if err != nil || !validCredential(credential) {
		return nil, 0, renditionError(document.RenditionErrorAuthentication,
			"Reducto credential is unavailable", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, apiOrigin+path, reader)
	if err != nil {
		return nil, 0, &preEgressContextError{cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	state.requests++
	response, err := client.transport.RoundTrip(request)
	if err != nil {
		return nil, 0, err
	}
	if response == nil || response.Body == nil {
		return nil, 0, errors.New("reducto returned an empty HTTP response")
	}
	raw, readErr := readBounded(response.Body, limit)
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, response.StatusCode, readErr
	}
	if closeErr != nil {
		return nil, response.StatusCode, fmt.Errorf("close Reducto response: %w", closeErr)
	}
	state.outputBytes += int64(len(raw))
	return raw, response.StatusCode, nil
}

func (client *Client) httpError(status int, stage string) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return renditionError(document.RenditionErrorAuthentication,
			"Reducto authentication was rejected", nil)
	case http.StatusTooManyRequests:
		return renditionError(document.RenditionErrorRateLimited,
			"Reducto rate limit was exhausted", nil)
	case http.StatusServiceUnavailable, http.StatusInsufficientStorage:
		return renditionError(document.RenditionErrorCapacity,
			"Reducto capacity is unavailable", nil)
	case http.StatusNotFound, http.StatusGone:
		if stage == "job" {
			return renditionError(document.RenditionErrorUnknownJob,
				"Reducto job is unknown or expired", nil)
		}
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusUnsupportedMediaType,
		http.StatusRequestEntityTooLarge:
		return renditionError(document.RenditionErrorUnsupportedInput,
			"Reducto rejected the input", nil)
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusGatewayTimeout:
		return renditionError(document.RenditionErrorTransient,
			"Reducto service request failed", nil)
	}
	if status >= 300 && status < 400 {
		return renditionError(document.RenditionErrorPolicyRejected,
			"Reducto redirects are refused", nil)
	}
	if status >= 500 {
		return renditionError(document.RenditionErrorTransient,
			"Reducto service request failed", nil)
	}
	return renditionError(document.RenditionErrorPolicyRejected,
		"Reducto rejected the request", nil)
}

func (client *Client) submissionFailure(
	callerCtx, operationCtx context.Context, expiresAt time.Time, message string, cause error,
) error {
	if local, ok := errors.AsType[*preEgressContextError](cause); ok {
		return classifyContextFailure(callerCtx, operationCtx, expiresAt, local.cause)
	}
	if providerError, ok := errors.AsType[*document.RenditionProviderError](cause); ok {
		return providerError
	}
	return renditionError(document.RenditionErrorAmbiguousSubmission, message, cause)
}

func (client *Client) resultFailure(
	callerCtx, operationCtx context.Context, expiresAt time.Time, durableCheckpoint bool,
	message string, cause error,
) error {
	if errors.Is(callerCtx.Err(), context.Canceled) {
		return renditionError(document.RenditionErrorCanceled, "Reducto rendering was canceled", callerCtx.Err())
	}
	if callerCtx.Err() != nil {
		return renditionError(document.RenditionErrorTransient, "Reducto caller deadline was reached", cause)
	}
	if !expiresAt.IsZero() && !time.Now().UTC().Before(expiresAt) {
		if durableCheckpoint {
			return renditionError(document.RenditionErrorTransient,
				"Reducto durable operation reached its authorization boundary", cause)
		}
		return expiredError(cause)
	}
	if providerError, ok := errors.AsType[*document.RenditionProviderError](cause); ok {
		return providerError
	}
	if errors.Is(cause, errResponseTooLarge) {
		return malformedError("Reducto job response exceeds profile limit", cause)
	}
	if operationCtx.Err() != nil {
		return renditionError(document.RenditionErrorTransient, "Reducto operation timed out", cause)
	}
	return renditionError(document.RenditionErrorTransient, message, cause)
}

func (client *Client) pollFailure(
	callerCtx, operationCtx context.Context, expiresAt time.Time, durableCheckpoint bool, cause error,
) error {
	return client.resultFailure(
		callerCtx, operationCtx, expiresAt, durableCheckpoint, "Reducto polling failed", cause)
}

type operationState struct {
	startedAt         time.Time
	submittedAt       time.Time
	durableCheckpoint bool
	requests          int64
	retries           int64
	inputBytes        int64
	outputBytes       int64
	pollDelay         time.Duration
	warnings          []string
}

type preEgressContextError struct{ cause error }

func (failure *preEgressContextError) Error() string { return "reducto request stopped before egress" }
func (failure *preEgressContextError) Unwrap() error { return failure.cause }

type resumePayload struct {
	Version          string `json:"v"`
	JobID            string `json:"j"`
	StartedAt        string `json:"s"`
	SubmittedAt      string `json:"a"`
	Requests         int64  `json:"q"`
	Retries          int64  `json:"r"`
	InputBytes       int64  `json:"i"`
	OutputBytes      int64  `json:"o"`
	RetryDelayMillis int64  `json:"d"`
}

func (client *Client) checkpoint(
	checkpoint document.RenditionResumeCheckpoint, jobID string, state *operationState,
) error {
	if checkpoint == nil {
		return nil
	}
	resumeValue, err := encodeResumeHandle(resumePayload{
		Version: resumeHandleVersion, JobID: jobID,
		StartedAt: state.startedAt.Format(timeForm), SubmittedAt: state.submittedAt.Format(timeForm),
		Requests: state.requests, Retries: state.retries, InputBytes: state.inputBytes,
		OutputBytes: state.outputBytes, RetryDelayMillis: state.pollDelay.Milliseconds(),
	})
	if err != nil {
		return renditionError(document.RenditionErrorAmbiguousSubmission,
			"Reducto job resume state could not be encoded", err)
	}
	if err := checkpoint(document.RenditionResumeHandle{Value: resumeValue}); err != nil {
		return renditionError(document.RenditionErrorAmbiguousSubmission,
			"Reducto job could not be durably checkpointed", err)
	}
	state.durableCheckpoint = true
	return nil
}

type uploadResponse struct {
	FileID       string  `json:"file_id"`
	PresignedURL *string `json:"presigned_url"`
}

type parseRequest struct {
	DocumentURL     string                 `json:"document_url"`
	AdvancedOptions parseAdvancedOptions   `json:"advanced_options"`
	Options         parseProcessingOptions `json:"options"`
	Priority        bool                   `json:"priority"`
}

type parseAdvancedOptions struct {
	AddPageMarkers bool `json:"add_page_markers"`
	ReturnOCRData  bool `json:"return_ocr_data"`
}

type parseProcessingOptions struct {
	Chunking       parseChunking `json:"chunking"`
	ForceURLResult bool          `json:"force_url_result"`
}

type parseChunking struct {
	ChunkMode string `json:"chunk_mode"`
}

type parseResponse struct {
	JobID string `json:"job_id"`
}

type jobResponse struct {
	Status   string         `json:"status"`
	Progress *float64       `json:"progress"`
	Reason   *string        `json:"reason"`
	Result   jsontext.Value `json:"result"`
}

type completedResult struct {
	Result   jsontext.Value `json:"result"`
	Usage    parseUsage     `json:"usage"`
	Duration float64        `json:"duration"`
	JobID    string         `json:"job_id"`
	PDFURL   *string        `json:"pdf_url"`
}

type parseUsage struct {
	NumPages *int64 `json:"num_pages"`
}

type parseResult struct {
	Type     string         `json:"type"`
	Chunks   jsontext.Value `json:"chunks"`
	Custom   any            `json:"custom"`
	OCR      *parseOCR      `json:"ocr"`
	ResultID string         `json:"result_id"`
	URL      string         `json:"url"`
}

type resultChunk struct {
	Blocks            []resultBlock `json:"blocks"`
	Content           string        `json:"content"`
	Embed             string        `json:"embed"`
	Enriched          *string       `json:"enriched"`
	EnrichmentSuccess bool          `json:"enrichment_success"`
}

type resultBlock struct {
	BBox     resultBoundingBox `json:"bbox"`
	Content  string            `json:"content"`
	Type     string            `json:"type"`
	ImageURL *string           `json:"image_url"`
}

type resultBoundingBox struct {
	Height       *float64 `json:"height"`
	Left         *float64 `json:"left"`
	Page         *int64   `json:"page"`
	Top          *float64 `json:"top"`
	Width        *float64 `json:"width"`
	OriginalPage *int64   `json:"original_page"`
}

type parseOCR struct {
	Lines []parseOCRLine `json:"lines"`
	Words []parseOCRWord `json:"words"`
}

type parseOCRLine struct {
	BBox resultBoundingBox `json:"bbox"`
	Text string            `json:"text"`
}

type parseOCRWord struct {
	BBox resultBoundingBox `json:"bbox"`
	Text string            `json:"text"`
}

type fixedOriginTransport struct{ base http.RoundTripper }

func (transport fixedOriginTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.URL.Scheme != "https" ||
		request.URL.Host != apiHost || request.URL.User != nil || request.URL.RawQuery != "" ||
		request.URL.Fragment != "" || request.Host != "" && request.Host != apiHost ||
		!fixedRequestPath(request.Method, request.URL.Path) {
		return nil, errors.New("reducto request destination is not fixed")
	}
	return transport.base.RoundTrip(request)
}

func fixedRequestPath(method, path string) bool {
	if method == http.MethodPost && (path == uploadPath || path == parsePath) {
		return true
	}
	if method != http.MethodGet || !strings.HasPrefix(path, "/job/") {
		return false
	}
	return validateJobToken(strings.TrimPrefix(path, "/job/")) == nil
}

func jobPath(jobID string) string { return "/job/" + jobID }

func defaultProfile(profile Profile) Profile {
	if profile.MaxUploadBytes == 0 {
		profile.MaxUploadBytes = defaultMaxUploadBytes
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = profile.MaxUploadBytes + defaultRequestOverhead
	}
	if profile.MaxControlBytes == 0 {
		profile.MaxControlBytes = defaultMaxControlBytes
	}
	if profile.MaxPolls == 0 {
		profile.MaxPolls = defaultMaxPolls
	}
	if profile.PollInterval == 0 {
		profile.PollInterval = defaultPollInterval
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = defaultRequestTimeout
	}
	if profile.MaxResultBytes == 0 {
		profile.MaxResultBytes = defaultMaxResultBytes
	}
	if profile.MaxArtifactBytes == 0 {
		profile.MaxArtifactBytes = defaultMaxArtifactBytes
	}
	if profile.MaxWallTime == 0 {
		profile.MaxWallTime = defaultMaxWallTime
	}
	if profile.RetainStructured && profile.MaxArtifacts == 0 {
		profile.MaxArtifacts = 1
	}
	return profile
}

func validateProfile(profile Profile) error {
	if err := validateCanonicalToken(profile.SecretBinding, 128); err != nil {
		return fmt.Errorf("reducto credential binding: %w", err)
	}
	if profile.MaxUploadBytes <= 0 || profile.MaxUploadBytes > defaultMaxUploadBytes ||
		profile.MaxRequestBytes <= profile.MaxUploadBytes ||
		profile.MaxRequestBytes > maxConfiguredBytes+defaultRequestOverhead ||
		profile.MaxControlBytes <= 0 || profile.MaxControlBytes > 1<<20 ||
		profile.MaxResultBytes <= 0 || profile.MaxResultBytes > maxConfiguredBytes ||
		profile.MaxArtifactBytes <= 0 || profile.MaxArtifactBytes > maxConfiguredBytes ||
		profile.MaxPolls <= 0 || profile.MaxPolls > maxConfiguredPolls ||
		profile.PollInterval <= 0 || profile.PollInterval > time.Hour ||
		profile.RequestTimeout <= 0 || profile.RequestTimeout > maxConfiguredDuration ||
		profile.MaxWallTime <= 0 || profile.MaxWallTime > maxConfiguredDuration {
		return errors.New("reducto profile bounds are invalid")
	}
	if profile.RetainStructured && profile.MaxArtifacts != 1 ||
		!profile.RetainStructured && profile.MaxArtifacts != 0 {
		return errors.New("reducto structured artifact count is invalid")
	}
	return nil
}

func validateUploadToken(value string) error {
	if len(value) <= len("reducto://") || len(value) > maxUploadTokenBytes ||
		!strings.HasPrefix(value, "reducto://") {
		return errors.New("upload token is invalid")
	}
	return validateOpaqueToken(strings.TrimPrefix(value, "reducto://"), maxUploadTokenBytes-len("reducto://"))
}

func validateJobToken(value string) error {
	if value == "" || len(value) > maxJobTokenBytes || value == "." || value == ".." {
		return errors.New("job token is invalid")
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || strings.ContainsRune("-._", char) {
			continue
		}
		return errors.New("job token contains unsupported characters")
	}
	return nil
}

func encodeResumeHandle(payload resumePayload) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	value := resumeHandlePrefix + base64.RawURLEncoding.EncodeToString(raw)
	if len(value) > 512 {
		return "", errors.New("resume handle exceeds core bound")
	}
	if err := validateOpaqueToken(value, 512); err != nil {
		return "", err
	}
	return value, nil
}

func decodeResumeHandle(value string, authorization document.RenditionAuthorization) (resumePayload, error) {
	if err := validateOpaqueToken(value, 512); err != nil || !strings.HasPrefix(value, resumeHandlePrefix) {
		return resumePayload{}, errors.New("resume handle envelope is invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, resumeHandlePrefix))
	if err != nil {
		return resumePayload{}, errors.New("resume handle payload is invalid")
	}
	var payload resumePayload
	if err := strictJSON(raw, &payload); err != nil {
		return resumePayload{}, errors.New("resume handle schema is invalid")
	}
	canonical, err := json.Marshal(payload)
	if err != nil || !bytes.Equal(raw, canonical) || payload.Version != resumeHandleVersion ||
		validateJobToken(payload.JobID) != nil {
		return resumePayload{}, errors.New("resume handle identity is invalid")
	}
	startedAt, err := parseCanonicalTimestamp(payload.StartedAt)
	if err != nil {
		return resumePayload{}, err
	}
	submittedAt, err := parseCanonicalTimestamp(payload.SubmittedAt)
	if err != nil || submittedAt.Before(startedAt) {
		return resumePayload{}, errors.New("resume handle submission is invalid")
	}
	authorizedAt, err := parseCanonicalTimestamp(authorization.AuthorizedAt)
	if err != nil {
		return resumePayload{}, errors.New("resume authorization start is invalid")
	}
	expiresAt, err := parseCanonicalTimestamp(authorization.ExpiresAt)
	if err != nil || startedAt.Before(authorizedAt) || !startedAt.Before(expiresAt) ||
		!submittedAt.Before(expiresAt) {
		return resumePayload{}, errors.New("resume handle is outside the sealed authorization interval")
	}
	if payload.Requests < 2 || payload.Requests > maxResumeUsageValue ||
		payload.Retries < 0 || payload.Retries > payload.Requests ||
		payload.InputBytes != authorization.SourceBytes ||
		payload.OutputBytes < 0 || payload.OutputBytes > maxResumeUsageValue ||
		payload.RetryDelayMillis < 0 || payload.RetryDelayMillis > int64((24*time.Hour)/time.Millisecond) {
		return resumePayload{}, errors.New("resume handle accounting is invalid")
	}
	return payload, nil
}

func parseCanonicalTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(timeForm, value)
	if err != nil || parsed.Format(timeForm) != value {
		return time.Time{}, errors.New("timestamp is not canonical UTC RFC3339Nano")
	}
	return parsed, nil
}

func validateCanonicalToken(value string, maximum int) error {
	if value == "" || len(value) > maximum || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return errors.New("value is not a canonical token")
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("_.-", char) {
			continue
		}
		return errors.New("value contains unsupported token characters")
	}
	return nil
}

func validateOpaqueToken(value string, maximum int) error {
	if value == "" || len(value) > maximum || value == "." || value == ".." ||
		value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return errors.New("opaque token is invalid")
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("-._~", char) {
			continue
		}
		return errors.New("opaque token contains unsupported characters")
	}
	return nil
}

func validCredential(value string) bool {
	if value == "" || len(value) > maxCredentialBytes || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char == 0x7f {
			return false
		}
	}
	return true
}

func readExactUpload(
	ctx, callerCtx context.Context, expiresAt time.Time,
	upload document.AuthorizedUpload, metadata document.AuthorizedUploadMetadata, limit int64,
) ([]byte, error) {
	readDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = upload.Close()
		case <-readDone:
		}
	}()
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: upload}, limit+1))
	close(readDone)
	if ctx.Err() != nil {
		clear(data)
		return nil, classifyContextFailure(callerCtx, ctx, expiresAt, ctx.Err())
	}
	if err != nil {
		return nil, renditionError(document.RenditionErrorTransient,
			"Reducto upload could not be read", err)
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != metadata.ByteLength || int64(len(data)) > limit ||
		hex.EncodeToString(digest[:]) != metadata.SHA256 {
		clear(data)
		return nil, renditionError(document.RenditionErrorPolicyRejected,
			"Reducto upload identity changed", nil)
	}
	return data, nil
}

func classifyContextFailure(
	callerCtx, operationCtx context.Context, expiresAt time.Time, cause error,
) error {
	if errors.Is(callerCtx.Err(), context.Canceled) {
		return renditionError(document.RenditionErrorCanceled, "Reducto rendering was canceled", callerCtx.Err())
	}
	if callerCtx.Err() != nil {
		return renditionError(document.RenditionErrorTransient, "Reducto caller deadline was reached", cause)
	}
	if !expiresAt.IsZero() && !time.Now().UTC().Before(expiresAt) {
		return expiredError(cause)
	}
	if operationCtx.Err() != nil {
		return renditionError(document.RenditionErrorTransient, "Reducto operation timed out", cause)
	}
	return renditionError(document.RenditionErrorTransient, "Reducto operation context failed", cause)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

type resumeAuthorizationUpload struct {
	metadata document.AuthorizedUploadMetadata
}

func (upload resumeAuthorizationUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload.metadata
}

func (resumeAuthorizationUpload) Read([]byte) (int, error) { return 0, io.EOF }
func (resumeAuthorizationUpload) Close() error             { return nil }

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errResponseTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errResponseTooLarge
	}
	return data, nil
}

func strictJSON(raw []byte, target any) error {
	return json.Unmarshal(raw, target, json.RejectUnknownMembers(true))
}

func requireMembers(raw []byte, names ...string) error {
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	for _, name := range names {
		if _, ok := members[name]; !ok {
			return fmt.Errorf("required JSON member %q is missing", name)
		}
	}
	return nil
}

func validProgress(value *float64) bool {
	return value == nil || finite(*value) && *value >= 0 && *value <= 1
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func isNullJSON(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func familyUnit(family string) (document.EvidenceUnitKind, document.EvidenceLocatorKind, bool) {
	switch family {
	case "pdf":
		return document.EvidenceUnitPage, document.EvidenceLocatorPage, true
	case "presentation":
		return document.EvidenceUnitSlide, document.EvidenceLocatorSlide, true
	case "spreadsheet":
		return document.EvidenceUnitSheet, document.EvidenceLocatorSheet, true
	default:
		return "", "", false
	}
}

func allowsStructured(authorization document.RenditionAuthorization) bool {
	return authorization.MaxArtifacts >= 1 && authorization.MaxArtifactBytes > 0 &&
		slices.Contains(authorization.AllowedArtifactRoles, document.EvidenceArtifactStructured)
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func boundedOperationContext(
	ctx context.Context, expiresAt time.Time, wall time.Duration,
) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(wall)
	if !expiresAt.IsZero() && expiresAt.Before(deadline) {
		deadline = expiresAt
	}
	return context.WithDeadline(ctx, deadline)
}

func renditionError(code document.RenditionErrorCode, message string, cause error) error {
	providerError, err := document.NewRenditionProviderError(code, message, 0, cause)
	if err != nil {
		return fmt.Errorf("reducto error classification failed: %w", err)
	}
	return providerError
}

func malformedError(message string, cause error) error {
	return renditionError(document.RenditionErrorMalformedEvidence, message, cause)
}

func expiredError(cause error) error {
	return renditionError(document.RenditionErrorPolicyRejected, "Reducto authorization expired", cause)
}

func cloneDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = slices.Clone(value.SupportedFormats)
	value.ArtifactRoles = slices.Clone(value.ArtifactRoles)
	return value
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func min64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}
