package reducto

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	apiHost    = "platform.reducto.ai"
	apiOrigin  = "https://" + apiHost
	uploadPath = "/upload"
	parsePath  = "/parse_async"
	providerID = "reducto.parse-v1"
	timeForm   = providerutil.TimestampForm

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
	maxUploadTokenBytes   = 256
	maxJobTokenBytes      = 120
	resumeHandlePrefix    = "r3."
	maxResumeUsageValue   = int64(1 << 50)
	maxNaturalUnits       = 100_000
	markdownUnitSeparator = "\n\n---\n\n"
)

var (
	_ document.RenditionProvider          = (*Client)(nil)
	_ document.ResumableRenditionProvider = (*Client)(nil)
)

var provider = providerutil.Provider("Reducto")

// SecretResolver resolves the one credential named by the frozen profile.
type SecretResolver = providerutil.SecretResolver

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
	executor   providerutil.Executor
}

// NewProvider constructs a provider around an injected hardened transport.
// It does not consult proxy environment variables, resolve credentials, or
// perform network access during construction.
func NewProvider(profile Profile, secrets SecretResolver, transport http.RoundTripper) (*Client, error) {
	profile = defaultProfile(profile)
	if err := validateProfile(profile); err != nil {
		return nil, err
	}
	if providerutil.IsNil(secrets) {
		return nil, errors.New("reducto named credential resolver is required")
	}
	if providerutil.IsNil(transport) {
		return nil, errors.New("reducto hardened transport is required")
	}
	credential := providerutil.BearerCredential(profile.SecretBinding, secrets)
	if err := credential.Validate(provider); err != nil {
		return nil, err
	}
	identity := fmt.Sprintf(
		"reducto-profile/v1\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%t",
		modelIdentity, profileID, profile.SecretBinding, profile.MaxUploadBytes,
		profile.MaxRequestBytes, profile.MaxControlBytes, profile.MaxPolls,
		profile.PollInterval, profile.RequestTimeout, profile.MaxResultBytes,
		profile.MaxArtifactBytes, profile.MaxWallTime, profile.RetainStructured,
	)
	roles := []document.EvidenceArtifactRole(nil)
	if profile.RetainStructured {
		roles = []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}
	}
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID:                providerID,
		ContractVersion:   document.RenditionProviderContractVersion,
		PolicyFingerprint: providerutil.SHA256Hex([]byte(identity)),
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
		descriptor: providerutil.CloneDescriptor(descriptor), profile: profile,
		executor: providerutil.Executor{
			Provider: provider, HTTP: providerhttp.IsolateClient(&http.Client{Transport: transport}),
			Origin: apiOrigin, RequestTimeout: profile.RequestTimeout,
			MaxResponseBytes: max(profile.MaxControlBytes, profile.MaxResultBytes, profile.MaxArtifactBytes),
			Credential:       credential,
		},
	}, nil
}

// Descriptor returns a defensive copy of the immutable provider identity.
func (client *Client) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(client.descriptor)
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
	resumeFacts, err := client.validateInvocation(upload, authorization, resume)
	if err != nil {
		return document.RenditionResult{}, err
	}
	var operation *providerutil.Operation
	if resume == nil {
		operation, err = providerutil.NewOperation(ctx, provider, authorization.ExpiresAt, client.profile.MaxWallTime)
		if err != nil {
			return document.RenditionResult{}, err
		}
	} else {
		operation = providerutil.NewResumedOperation(ctx, provider, client.profile.MaxWallTime)
	}
	defer operation.Cancel()
	state := operationState{
		operation: operation, authorizationFingerprint: resumeFacts.AuthorizationFingerprint,
		startedAt: startedAt, inputBytes: authorization.SourceBytes,
	}
	var jobID string
	if resume == nil {
		fileID, submitErr := client.upload(upload, &state)
		if submitErr != nil {
			return document.RenditionResult{}, submitErr
		}
		jobID, submitErr = client.submit(fileID, checkpoint, &state)
		if submitErr != nil {
			return document.RenditionResult{}, submitErr
		}
	} else {
		jobID = resumeFacts.JobID
		state.startedAt, _ = time.Parse(timeForm, resumeFacts.StartedAt)
		state.submittedAt, _ = time.Parse(timeForm, resumeFacts.SubmittedAt)
		state.completedAt = state.submittedAt
		state.usage.Requests = resumeFacts.Requests
		state.usage.Retries = resumeFacts.Retries
		state.inputBytes = resumeFacts.InputBytes
		state.usage.OutputBytes = resumeFacts.OutputBytes
		state.pollDelay = time.Duration(resumeFacts.RetryDelayMillis) * time.Millisecond
	}
	result, err := client.poll(jobID, authorization, checkpoint, &state)
	if err != nil {
		return document.RenditionResult{}, err
	}
	if err := operation.Check(); err != nil {
		return document.RenditionResult{}, provider.KnownJobError(err)
	}
	completedAt := state.completedAt
	if resume == nil {
		completedAt = time.Now().UTC()
	}
	result.Receipt, err = providerutil.NewReceipt(provider, providerutil.Receipt{
		Descriptor: client.descriptor, Authorization: authorization, SourceSHA256: authorization.SourceSHA256,
		OperationID: "reducto-" + jobID, StartedAt: state.startedAt, CompletedAt: completedAt,
		Warnings: state.warnings,
		Usage:    state.usage.Rendition(state.inputBytes, int64(len(result.Evidence.Units))), RetryDelay: state.pollDelay,
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return result, nil
}

func (client *Client) validateInvocation(
	upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
	resume *document.RenditionResumeHandle,
) (resumePayload, error) {
	expiresAt, err := time.Parse(timeForm, authorization.ExpiresAt)
	if err != nil {
		return resumePayload{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"Reducto authorization expiry is invalid", err)
	}
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return resumePayload{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"Reducto authorization fingerprint is invalid", err)
	}
	if resume == nil {
		if providerutil.IsNil(upload) {
			return resumePayload{}, errors.New("reducto authorized upload is required for submission")
		}
		return resumePayload{AuthorizationFingerprint: authorizationFingerprint}, nil
	}
	if !providerutil.IsNil(upload) {
		return resumePayload{}, errors.New("reducto resume must not receive source bytes")
	}
	facts, decodeErr := decodeResumeHandle(resume.Value, authorization)
	if decodeErr != nil {
		return resumePayload{}, provider.Classified(document.RenditionErrorUnknownJob,
			"Reducto resume handle is invalid", decodeErr)
	}
	submittedAt, _ := time.Parse(timeForm, facts.SubmittedAt)
	authorizedAt, authErr := time.Parse(timeForm, authorization.AuthorizedAt)
	if authErr != nil || submittedAt.Before(authorizedAt) || !submittedAt.Before(expiresAt) ||
		facts.AuthorizationFingerprint != authorizationFingerprint ||
		authorization.ProviderID != client.descriptor.ID ||
		authorization.DescriptorFingerprint != client.descriptor.Fingerprint ||
		authorization.PolicyFingerprint != client.descriptor.PolicyFingerprint {
		return resumePayload{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"Reducto resume authority changed", authErr)
	}
	return facts, nil
}

func (client *Client) upload(upload document.AuthorizedUpload, state *operationState) (string, error) {
	metadata := upload.Metadata()
	if metadata.ByteLength > client.profile.MaxUploadBytes {
		return "", provider.Classified(document.RenditionErrorPolicyRejected,
			"Reducto upload exceeds profile limit", nil)
	}
	filename := metadata.Filename
	if filename == "" {
		filename = defaultFilename(metadata.MediaType)
	}
	if err := providerutil.ValidateMultipartFilename(filename); err != nil {
		return "", provider.Classified(document.RenditionErrorPolicyRejected,
			"Reducto upload filename contains a line break", err)
	}
	source, err := state.operation.ReadUpload(upload)
	if err != nil {
		return "", err
	}
	defer clear(source)
	body := &providerutil.MultipartUpload{
		FieldName: "file", Filename: filename, MediaType: metadata.MediaType,
		Source: bytes.NewReader(source), Length: int64(len(source)),
	}
	requestBytes, err := body.EncodedLength()
	if err != nil {
		return "", provider.Classified(document.RenditionErrorTransient,
			"Reducto upload request could not be built", err)
	}
	if requestBytes > client.profile.MaxRequestBytes {
		return "", provider.Classified(document.RenditionErrorPolicyRejected,
			"Reducto upload request exceeds profile limit", nil)
	}
	response, err := client.executor.Do(state.operation, &state.usage, providerutil.Request{
		Stage: providerutil.StageSubmission, Method: http.MethodPost, Path: uploadPath,
		Upload: body, MaxResponseBytes: client.profile.MaxControlBytes,
	})
	if err != nil {
		return "", err
	}
	if !response.Success() {
		return "", provider.StatusError(
			providerutil.StageSubmission, response.Status, response.RetryAfter, nil)
	}
	var result uploadResponse
	if err := strictJSON(response.Body, &result); err != nil || validateUploadToken(result.FileID) != nil {
		return "", provider.AmbiguousSubmission(provider.Malformed("Reducto upload schema changed", err))
	}
	return result.FileID, nil
}

func (client *Client) submit(
	fileID string, checkpoint document.RenditionResumeCheckpoint, state *operationState,
) (string, error) {
	request := parseRequest{DocumentURL: fileID, Priority: false}
	request.AdvancedOptions.AddPageMarkers = true
	request.AdvancedOptions.ReturnOCRData = false
	request.Options.Chunking.ChunkMode = "page"
	request.Options.ForceURLResult = false
	body, err := json.Marshal(request)
	if err != nil {
		return "", provider.Classified(document.RenditionErrorTransient,
			"Reducto parse request could not be built", err)
	}
	if int64(len(body)) > client.profile.MaxRequestBytes {
		return "", provider.Classified(document.RenditionErrorPolicyRejected,
			"Reducto parse request exceeds profile limit", nil)
	}
	defer clear(body)
	response, err := client.executor.Do(state.operation, &state.usage, providerutil.Request{
		Stage: providerutil.StageSubmission, Method: http.MethodPost, Path: parsePath,
		ContentType: providerutil.JSONMediaType, Body: body, MaxResponseBytes: client.profile.MaxControlBytes,
	})
	if err != nil {
		return "", err
	}
	if !response.Success() {
		return "", provider.StatusError(
			providerutil.StageSubmission, response.Status, response.RetryAfter, nil)
	}
	var result parseResponse
	if err := json.Unmarshal(response.Body, &result); err != nil || validateJobToken(result.JobID) != nil {
		return "", provider.AmbiguousSubmission(provider.Malformed("Reducto submission schema changed", err))
	}
	state.submittedAt = time.Now().UTC()
	if err := client.checkpoint(checkpoint, result.JobID, state); err != nil {
		return "", err
	}
	if err := strictJSON(response.Body, &result); err != nil {
		return "", provider.AmbiguousJob(provider.Malformed("Reducto submission schema changed", err))
	}
	return result.JobID, nil
}

func (client *Client) poll(
	jobID string, authorization document.RenditionAuthorization,
	checkpoint document.RenditionResumeCheckpoint, state *operationState,
) (document.RenditionResult, error) {
	limit := min(client.profile.MaxResultBytes, int64(authorization.MaxTotalResultBytes))
	for attempt := range client.profile.MaxPolls {
		response, err := client.executor.Do(state.operation, &state.usage, providerutil.Request{
			Stage: providerutil.StageJob, Method: http.MethodGet, Path: jobPath(jobID),
			MaxResponseBytes: limit,
		})
		if checkpointErr := client.checkpoint(checkpoint, jobID, state); checkpointErr != nil {
			return document.RenditionResult{}, checkpointErr
		}
		if err != nil {
			return document.RenditionResult{}, provider.KnownJobError(err)
		}
		if !response.Success() {
			return document.RenditionResult{}, provider.KnownJobError(provider.StatusError(
				providerutil.StageJob, response.Status, response.RetryAfter, nil))
		}
		var job jobResponse
		if err := requireMembers(response.Body, "status"); err != nil {
			return document.RenditionResult{}, provider.Malformed("Reducto job schema changed", err)
		}
		if err := strictJSON(response.Body, &job); err != nil || !validProgress(job.Progress) {
			return document.RenditionResult{}, provider.Malformed("Reducto job schema changed", err)
		}
		switch job.Status {
		case "Pending", "Idle":
			if !isNullJSON(job.Result) {
				return document.RenditionResult{}, provider.Malformed(
					"Reducto pending job schema changed", nil)
			}
			if attempt+1 == client.profile.MaxPolls {
				break
			}
			state.usage.Retries++
			if err := state.operation.Wait(client.profile.PollInterval); err != nil {
				return document.RenditionResult{}, provider.KnownJobError(err)
			}
			state.pollDelay += client.profile.PollInterval
			if checkpointErr := client.checkpoint(checkpoint, jobID, state); checkpointErr != nil {
				return document.RenditionResult{}, checkpointErr
			}
		case "Completed":
			return client.completed(job.Result, jobID, authorization, state)
		case "Failed":
			return document.RenditionResult{}, provider.Malformed(
				"Reducto could not parse the input", nil)
		default:
			return document.RenditionResult{}, provider.AmbiguousJob(
				provider.Malformed("Reducto job status schema changed", nil))
		}
	}
	return document.RenditionResult{}, provider.AmbiguousJob(provider.Classified(
		document.RenditionErrorCapacity, "Reducto polling limit was reached", nil))
}

func (client *Client) completed(
	raw []byte, jobID string, authorization document.RenditionAuthorization, state *operationState,
) (document.RenditionResult, error) {
	if isNullJSON(raw) {
		return document.RenditionResult{}, provider.Malformed("Reducto completed without a result", nil)
	}
	if err := requireMembers(raw, "result", "usage", "duration", "job_id"); err != nil {
		return document.RenditionResult{}, provider.Malformed("Reducto completed result schema changed", err)
	}
	var completed completedResult
	if err := strictJSON(raw, &completed); err != nil {
		return document.RenditionResult{}, provider.Malformed("Reducto completed result schema changed", err)
	}
	if completed.JobID != jobID {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"Reducto completed job identity changed", nil)
	}
	if math.IsNaN(completed.Duration) || math.IsInf(completed.Duration, 0) || completed.Duration < 0 {
		return document.RenditionResult{}, provider.Malformed("Reducto result duration is invalid", nil)
	}
	if completed.Usage.NumPages == nil || *completed.Usage.NumPages < 0 {
		return document.RenditionResult{}, provider.Malformed("Reducto page usage is invalid", nil)
	}
	if err := requireMembers(completed.Result, "type"); err != nil {
		return document.RenditionResult{}, provider.Malformed("Reducto parse result schema changed", err)
	}
	var parsed parseResult
	if err := strictJSON(completed.Result, &parsed); err != nil {
		return document.RenditionResult{}, provider.Malformed("Reducto parse result schema changed", err)
	}
	if parsed.Type == "url" {
		if err := requireMembers(completed.Result, "result_id", "url"); err != nil ||
			parsed.ResultID == "" || parsed.URL == "" {
			return document.RenditionResult{}, provider.Malformed("Reducto URL result schema changed", err)
		}
		return document.RenditionResult{}, provider.Malformed("Reducto returned a provider URL instead of inline output", nil)
	}
	if parsed.Type != "full" || len(parsed.Chunks) == 0 {
		return document.RenditionResult{}, provider.Malformed("Reducto parse result type changed", nil)
	}
	if parsed.Custom != nil || parsed.OCR != nil {
		return document.RenditionResult{}, provider.Malformed("Reducto returned unrequested nested output", nil)
	}
	var rawChunks []jsontext.Value
	if err := strictJSON(parsed.Chunks, &rawChunks); err != nil || len(rawChunks) == 0 {
		return document.RenditionResult{}, provider.Malformed("Reducto chunk schema changed", err)
	}
	chunks := make([]resultChunk, len(rawChunks))
	for index, rawChunk := range rawChunks {
		if err := requireMembers(rawChunk, "blocks", "content", "embed", "enriched"); err != nil {
			return document.RenditionResult{}, provider.Malformed("Reducto chunk schema changed", err)
		}
		if err := strictJSON(rawChunk, &chunks[index]); err != nil {
			return document.RenditionResult{}, provider.Malformed("Reducto chunk schema changed", err)
		}
	}
	evidence, markdown, err := naturalEvidence(chunks, authorization.MediaFamily, *completed.Usage.NumPages)
	if err != nil {
		return document.RenditionResult{}, provider.Malformed("Reducto output is partial or malformed", err)
	}
	if providerutil.InjectsDocbankFrontmatter(markdown) {
		return document.RenditionResult{}, provider.Malformed(
			"Reducto provider Markdown attempts Docbank frontmatter injection", nil)
	}
	if authorization.MaxProviderMarkdownBytes == 0 {
		markdown = nil
	} else if len(markdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, provider.Malformed("Reducto Markdown exceeds authorization", nil)
	}
	result := document.RenditionResult{Evidence: evidence, ProviderMarkdown: markdown}
	if client.profile.RetainStructured && providerutil.AllowsStructured(authorization) {
		maximum := min(client.profile.MaxArtifactBytes, int64(authorization.MaxArtifactBytes))
		if int64(len(raw)) > maximum {
			return document.RenditionResult{}, provider.Malformed("Reducto structured result exceeds authorization", nil)
		}
		payload := bytes.Clone(raw)
		checksum := providerutil.SHA256Hex(payload)
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
		return document.RenditionResult{}, provider.Malformed("Reducto evidence could not be bounded", err)
	}
	total := len(encodedEvidence) + len(result.ProviderMarkdown)
	for _, artifact := range result.Artifacts {
		total += len(artifact.Payload)
	}
	if total > authorization.MaxTotalResultBytes {
		return document.RenditionResult{}, provider.Malformed("Reducto total result exceeds authorization", nil)
	}
	if err := document.ValidateSourceEvidenceV1(result.Evidence); err != nil {
		return document.RenditionResult{}, provider.Malformed("Reducto evidence is unrepresentable", err)
	}
	if err := state.operation.Check(); err != nil {
		return document.RenditionResult{}, provider.KnownJobError(err)
	}
	return result, nil
}

func naturalEvidence(
	chunks []resultChunk, family string, reportedUnits int64,
) (document.SourceEvidenceV1, []byte, error) {
	if family == "spreadsheet" {
		return document.SourceEvidenceV1{}, nil, errors.New("provider did not report a stable sheet name")
	}
	unitKind, locatorKind, ok := providerutil.NaturalUnit(family)
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
			if !utf8.ValidString(block.Content) {
				return document.SourceEvidenceV1{}, nil, errors.New("block content is invalid")
			}
			if chunk.Content == "" && block.Content != "" {
				return document.SourceEvidenceV1{}, nil, errors.New("contentless chunk contains unassigned block text")
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

type operationState struct {
	operation                *providerutil.Operation
	usage                    providerutil.Usage
	authorizationFingerprint string
	startedAt                time.Time
	submittedAt              time.Time
	inputBytes               int64
	pollDelay                time.Duration
	warnings                 []string
	completedAt              time.Time
}

type resumePayload struct {
	JobID                    string `json:"j"`
	AuthorizationFingerprint string `json:"f"`
	StartedAt                string `json:"s"`
	SubmittedAt              string `json:"a"`
	Requests                 int64  `json:"q"`
	Retries                  int64  `json:"r"`
	InputBytes               int64  `json:"i"`
	OutputBytes              int64  `json:"o"`
	RetryDelayMillis         int64  `json:"d"`
}

func (client *Client) checkpoint(
	checkpoint document.RenditionResumeCheckpoint, jobID string, state *operationState,
) error {
	if checkpoint == nil {
		return nil
	}
	resumeValue, err := encodeResumeHandle(resumePayload{
		JobID:                    jobID,
		AuthorizationFingerprint: state.authorizationFingerprint,
		StartedAt:                state.startedAt.Format(timeForm), SubmittedAt: state.submittedAt.Format(timeForm),
		Requests: state.usage.Requests, Retries: state.usage.Retries, InputBytes: state.inputBytes,
		OutputBytes: state.usage.OutputBytes, RetryDelayMillis: state.pollDelay.Milliseconds(),
	})
	if err != nil {
		return provider.AmbiguousJob(provider.Malformed(
			"Reducto job resume state could not be encoded", err))
	}
	if err := checkpoint(document.RenditionResumeHandle{Value: resumeValue}); err != nil {
		return provider.AmbiguousJob(err)
	}
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

func jobPath(jobID string) string { return "/job/" + jobID }

func defaultFilename(mediaType string) string {
	if mediaType == "application/vnd.openxmlformats-officedocument.presentationml.presentation" {
		return "document.pptx"
	}
	return "document.pdf"
}

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
	if err := provider.ValidateIdentifier(profile.SecretBinding, "secret binding"); err != nil {
		return err
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
	if err != nil || !bytes.Equal(raw, canonical) || validateJobToken(payload.JobID) != nil ||
		len(payload.AuthorizationFingerprint) != 64 ||
		validateCanonicalToken(payload.AuthorizationFingerprint, 64) != nil {
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
