package llamaparse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	apiHost                = "api.cloud.llamaindex.ai"
	apiOrigin              = "https://" + apiHost
	uploadPath             = "/api/v1/parsing/upload"
	providerID             = "llamaparse.parse-v1"
	timeForm               = providerutil.TimestampForm
	markdownPageSeparator  = "\n\n---\n\n"
	maxConfiguredBytes     = 512 << 20
	maxConfiguredPolls     = 100_000
	maxConfiguredDuration  = 24 * time.Hour
	defaultUploadBytes     = 50 << 20
	defaultRequestOverhead = 1 << 20
	defaultControlBytes    = 64 << 10
	defaultResultBytes     = 64 << 20
	defaultArtifactBytes   = 16 << 20
)

var errExecutionIdentityChanged = errors.New("reported execution identity changed")

var provider = providerutil.Provider("LlamaParse")

var (
	_ document.RenditionProvider          = (*Client)(nil)
	_ document.ResumableRenditionProvider = (*Client)(nil)
)

// SecretResolver resolves the one credential named by a frozen profile.
type SecretResolver = providerutil.SecretResolver

// Profile freezes the hosted model, parse preset, and every network/output
// bound. API origin and routes are deliberately not configurable.
type Profile struct {
	Model            string
	Preset           string
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
	RetainImages     bool
}

// Client is a fixed-origin hosted LlamaParse rendition provider.
type Client struct {
	descriptor document.RenditionDescriptor
	profile    Profile
	executor   providerutil.Executor
}

// NewProvider constructs a provider around an injected hardened transport.
// It never resolves credentials or performs network access during setup.
func NewProvider(profile Profile, secrets SecretResolver, transport http.RoundTripper) (*Client, error) {
	profile = defaultProfile(profile)
	if err := validateProfile(profile); err != nil {
		return nil, err
	}
	if providerutil.IsNil(secrets) {
		return nil, errors.New("LlamaParse named credential resolver is required")
	}
	if providerutil.IsNil(transport) {
		return nil, errors.New("LlamaParse hardened transport is required")
	}
	credential := providerutil.BearerCredential(profile.SecretBinding, secrets)
	if err := credential.Validate(provider); err != nil {
		return nil, err
	}
	policyFingerprint := providerutil.SHA256Hex([]byte(fmt.Sprintf(
		"llamaparse-profile/v1\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%t",
		profile.Model, profile.Preset, profile.SecretBinding, profile.MaxUploadBytes,
		profile.MaxRequestBytes, profile.MaxControlBytes, profile.MaxPolls,
		profile.PollInterval, profile.RequestTimeout, profile.MaxResultBytes,
		profile.MaxArtifactBytes, profile.MaxWallTime, profile.RetainImages,
	)))
	roles := []document.EvidenceArtifactRole(nil)
	if profile.RetainImages {
		roles = []document.EvidenceArtifactRole{document.EvidenceArtifactImage}
	}
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: providerID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: policyFingerprint,
		TrustBoundary:     document.RenditionTrustHostedProvider,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "pdf", MediaType: "application/pdf",
			InputKind: document.RenditionInputOriginalFile,
		}},
		ReturnsMarkdown: true, ReturnsStructured: true, ArtifactRoles: roles,
	})
	if err != nil {
		return nil, fmt.Errorf("LlamaParse descriptor: %w", err)
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

// Render starts and completes one hosted parse operation.
func (client *Client) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	return client.RenderResumable(ctx, upload, authorization, nil, nil)
}

// RenderResumable submits exact authorized bytes or resumes one known UUID
// handle. A new handle is checkpointed before the first status request.
func (client *Client) RenderResumable(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization, resume *document.RenditionResumeHandle,
	checkpoint document.RenditionResumeCheckpoint,
) (document.RenditionResult, error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("LlamaParse client is required")
	}
	resumeState, err := client.validateInvocation(upload, authorization, resume)
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
	state := operationState{startedAt: resumeState.submittedAt, completedAt: resumeState.checkpointedAt,
		operation: operation}
	var jobID string
	if resume == nil {
		jobID, err = client.submit(upload, &state)
		if err != nil {
			return document.RenditionResult{}, err
		}
		checkpointedAt := time.Now().UTC()
		if err := operation.Check(); err != nil {
			return document.RenditionResult{}, err
		}
		handle := encodeResumeHandle(resumeStateV1{
			jobID: jobID, submittedAt: state.startedAt, checkpointedAt: checkpointedAt,
		})
		if checkpoint != nil {
			if err := checkpoint(document.RenditionResumeHandle{Value: handle}); err != nil {
				return document.RenditionResult{}, err
			}
		}
	} else {
		jobID = resumeState.jobID
	}
	if err := client.poll(jobID, &state); err != nil {
		return document.RenditionResult{}, err
	}
	result, err := client.result(jobID, authorization, &state)
	if err != nil {
		return document.RenditionResult{}, err
	}
	acceptedAt := time.Now().UTC()
	if resume == nil {
		state.completedAt = acceptedAt
	}
	if err := operation.Check(); err != nil {
		return document.RenditionResult{}, err
	}
	result.Receipt, err = providerutil.NewReceipt(provider, providerutil.Receipt{
		Descriptor: client.descriptor, Authorization: authorization, SourceSHA256: authorization.SourceSHA256,
		OperationID: "llamaparse-" + jobID[:8], StartedAt: state.startedAt, CompletedAt: state.completedAt,
		Warnings:   state.warnings,
		Usage:      state.usage.Rendition(authorization.SourceBytes, int64(len(result.Evidence.Units))),
		RetryDelay: state.pollDelay,
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return result, nil
}

func (client *Client) validateInvocation(
	upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
	resume *document.RenditionResumeHandle,
) (resumeStateV1, error) {
	expiresAt, err := time.Parse(timeForm, authorization.ExpiresAt)
	if err != nil {
		return resumeStateV1{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"LlamaParse authorization expiry is invalid", err)
	}
	if resume == nil {
		if providerutil.IsNil(upload) {
			return resumeStateV1{}, errors.New("LlamaParse authorized upload is required for submission")
		}
	} else {
		if !providerutil.IsNil(upload) {
			return resumeStateV1{}, errors.New("LlamaParse resume must not receive source bytes")
		}
		parsed, parseErr := parseResumeHandle(resume.Value)
		if parseErr != nil {
			return resumeStateV1{}, provider.Classified(document.RenditionErrorUnknownJob,
				"LlamaParse resume handle is invalid", parseErr)
		}
		if authorization.ProviderID != client.descriptor.ID ||
			authorization.DescriptorFingerprint != client.descriptor.Fingerprint ||
			authorization.PolicyFingerprint != client.descriptor.PolicyFingerprint {
			return resumeStateV1{}, provider.Classified(document.RenditionErrorPolicyRejected,
				"LlamaParse resume authority changed", nil)
		}
		authorizedAt, authErr := time.Parse(timeForm, authorization.AuthorizedAt)
		if authErr != nil || parsed.submittedAt.Before(authorizedAt) ||
			parsed.checkpointedAt.Before(parsed.submittedAt) || parsed.checkpointedAt.After(expiresAt) {
			return resumeStateV1{}, provider.Classified(document.RenditionErrorPolicyRejected,
				"LlamaParse resume receipt facts are outside the sealed authorization", authErr)
		}
		return parsed, nil
	}
	return resumeStateV1{}, nil
}

func (client *Client) submit(
	upload document.AuthorizedUpload, state *operationState,
) (string, error) {
	metadata := upload.Metadata()
	if metadata.ByteLength > client.profile.MaxUploadBytes {
		return "", provider.Classified(document.RenditionErrorPolicyRejected,
			"LlamaParse upload exceeds profile limit", nil)
	}
	source, err := state.operation.ReadUpload(upload)
	if err != nil {
		return "", err
	}
	defer clear(source)
	body := &providerutil.MultipartUpload{
		FieldName: "file", Filename: metadata.Filename, MediaType: metadata.MediaType,
		Source: bytes.NewReader(source), Length: int64(len(source)), Fields: [][2]string{
			{"model", client.profile.Model}, {"preset", client.profile.Preset},
			{"page_error_tolerance", "0"}, {"save_images", strconv.FormatBool(client.profile.RetainImages)},
			{"disable_image_extraction", strconv.FormatBool(!client.profile.RetainImages)},
			{"take_screenshot", "false"},
		},
	}
	requestBytes, err := body.EncodedLength()
	if err != nil {
		return "", provider.Classified(document.RenditionErrorTransient,
			"LlamaParse request could not be built", err)
	}
	if requestBytes > client.profile.MaxRequestBytes {
		return "", provider.Classified(document.RenditionErrorPolicyRejected,
			"LlamaParse request exceeds profile limit", nil)
	}
	state.startedAt = time.Now().UTC()
	response, err := client.executor.Do(state.operation, &state.usage, providerutil.Request{
		Stage: providerutil.StageSubmission, Method: http.MethodPost, Path: uploadPath,
		Upload: body, MaxResponseBytes: client.profile.MaxControlBytes,
	})
	if err != nil {
		return "", err
	}
	if !response.Success() {
		return "", provider.StatusError(providerutil.StageSubmission, response.Status, response.RetryAfter, nil)
	}
	var job jobResponse
	if err := strictJSON(response.Body, &job); err != nil || validateJobID(job.ID) != nil {
		return "", provider.AmbiguousSubmission(provider.Malformed("LlamaParse submission schema changed", err))
	}
	if err := validateInitialStatus(job.Status); err != nil {
		return "", err
	}
	return job.ID, nil
}

func (client *Client) poll(
	jobID string, state *operationState,
) error {
	for attempt := range client.profile.MaxPolls {
		response, err := client.executor.Do(state.operation, &state.usage, providerutil.Request{
			Stage: providerutil.StageJob, Method: http.MethodGet, Path: statusPath(jobID),
			MaxResponseBytes: client.profile.MaxControlBytes,
		})
		if err != nil {
			if operationErr := state.operation.Check(); operationErr != nil {
				return provider.KnownJobError(operationErr)
			}
			return provider.KnownJobError(err)
		}
		if !response.Success() {
			return provider.KnownJobError(
				provider.StatusError(providerutil.StageJob, response.Status, response.RetryAfter, nil))
		}
		var job jobResponse
		if err := strictJSON(response.Body, &job); err != nil || job.ID != jobID {
			return provider.Malformed(
				"LlamaParse status schema changed", err)
		}
		switch job.Status {
		case "SUCCESS":
			return nil
		case "PENDING":
			if attempt+1 == client.profile.MaxPolls {
				break
			}
			state.usage.Retries++
			if err := state.operation.Wait(client.profile.PollInterval); err != nil {
				return provider.KnownJobError(err)
			}
			state.pollDelay += client.profile.PollInterval
		case "ERROR":
			return provider.Classified(document.RenditionErrorUnsupportedInput,
				"LlamaParse could not parse the input", nil)
		case "PARTIAL_SUCCESS":
			return provider.Malformed(
				"LlamaParse returned partial output", nil)
		case "CANCELLED":
			return provider.Classified(document.RenditionErrorCanceled,
				"LlamaParse job was canceled", nil)
		default:
			return provider.Classified(document.RenditionErrorPolicyRejected,
				"LlamaParse status schema changed", nil)
		}
	}
	return provider.AmbiguousJob(provider.Classified(document.RenditionErrorCapacity,
		"LlamaParse polling limit was reached", nil))
}

func (client *Client) result(
	jobID string, authorization document.RenditionAuthorization, state *operationState,
) (document.RenditionResult, error) {
	budget := min(client.profile.MaxResultBytes, int64(authorization.MaxTotalResultBytes))
	response, err := client.executor.Do(state.operation, &state.usage, providerutil.Request{
		Stage: providerutil.StageJob, Method: http.MethodGet, Path: jsonResultPath(jobID),
		MaxResponseBytes: budget,
	})
	if err != nil {
		return document.RenditionResult{}, provider.KnownJobError(err)
	}
	if !response.Success() {
		return document.RenditionResult{}, provider.KnownJobError(provider.StatusError(
			providerutil.StageJob, response.Status, response.RetryAfter, nil))
	}
	budget -= int64(len(response.Body))
	var envelope jsonResult
	parseErr := strictJSON(response.Body, &envelope)
	if err := state.operation.Check(); err != nil {
		return document.RenditionResult{}, err
	}
	if parseErr != nil || len(envelope.Pages) == 0 || !jsonObject(envelope.JobMetadata) {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse result schema changed", parseErr)
	}
	jobPages, err := client.validateJobMetadata(envelope.JobMetadata)
	if lifecycleErr := state.operation.Check(); lifecycleErr != nil {
		return document.RenditionResult{}, lifecycleErr
	}
	if err != nil {
		if errors.Is(err, errExecutionIdentityChanged) {
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
				"LlamaParse model or preset changed", err)
		}
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse result metadata is malformed", err)
	}
	var pages []resultPage
	pageParseErr := strictJSON(envelope.Pages, &pages)
	if err := state.operation.Check(); err != nil {
		return document.RenditionResult{}, err
	}
	if pageParseErr != nil {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse page schema changed", pageParseErr)
	}
	if len(pages) == 0 {
		if jobPages != 0 {
			return document.RenditionResult{}, provider.Malformed(
				"LlamaParse page output is incomplete", nil)
		}
		return client.markdownFallback(jobID, authorization, jobPages, budget, state)
	}
	if jobPages <= 0 || len(pages) != jobPages {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse page output is incomplete", nil)
	}
	evidence, markdown, imageName, err := client.pageEvidence(pages, authorization)
	if lifecycleErr := state.operation.Check(); lifecycleErr != nil {
		return document.RenditionResult{}, lifecycleErr
	}
	if err != nil {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse page output is incomplete", err)
	}
	if providerutil.InjectsDocbankFrontmatter(markdown) {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse provider Markdown attempts Docbank frontmatter injection", nil)
	}
	if authorization.MaxProviderMarkdownBytes == 0 {
		markdown = nil
	} else if len(markdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse Markdown exceeds authorization", nil)
	}
	result := document.RenditionResult{Evidence: evidence, ProviderMarkdown: markdown}
	if imageName != "" {
		artifact, sourceArtifact, artifactErr := client.fetchImage(
			jobID, imageName, authorization, budget, state)
		if artifactErr != nil {
			return document.RenditionResult{}, artifactErr
		}
		result.Artifacts = []document.RenditionArtifact{artifact}
		result.Evidence.Artifacts = []document.SourceEvidenceArtifactV1{sourceArtifact}
	}
	if err := state.operation.Check(); err != nil {
		return document.RenditionResult{}, err
	}
	return result, nil
}

func (client *Client) markdownFallback(
	jobID string, authorization document.RenditionAuthorization,
	expectedJobPages int, budget int64, state *operationState,
) (document.RenditionResult, error) {
	response, err := client.executor.Do(state.operation, &state.usage, providerutil.Request{
		Stage: providerutil.StageJob, Method: http.MethodGet, Path: markdownResultPath(jobID),
		MaxResponseBytes: budget,
	})
	if err != nil {
		return document.RenditionResult{}, provider.KnownJobError(err)
	}
	if !response.Success() {
		return document.RenditionResult{}, provider.KnownJobError(provider.StatusError(
			providerutil.StageJob, response.Status, response.RetryAfter, nil))
	}
	var envelope markdownResult
	parseErr := strictJSON(response.Body, &envelope)
	if err := state.operation.Check(); err != nil {
		return document.RenditionResult{}, err
	}
	if parseErr != nil || !jsonObject(envelope.JobMetadata) ||
		envelope.Markdown == "" || !utf8.ValidString(envelope.Markdown) {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse Markdown result is malformed", parseErr)
	}
	jobPages, metadataErr := client.validateJobMetadata(envelope.JobMetadata)
	if err := state.operation.Check(); err != nil {
		return document.RenditionResult{}, err
	}
	if metadataErr != nil {
		if errors.Is(metadataErr, errExecutionIdentityChanged) {
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
				"LlamaParse model or preset changed", metadataErr)
		}
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse result metadata is malformed", metadataErr)
	}
	if jobPages != expectedJobPages {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse result page counts disagree", nil)
	}
	markdown := []byte(envelope.Markdown)
	if providerutil.InjectsDocbankFrontmatter(markdown) {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse provider Markdown attempts Docbank frontmatter injection", nil)
	}
	if authorization.MaxProviderMarkdownBytes == 0 {
		markdown = nil
	} else if len(markdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse Markdown exceeds authorization", nil)
	}
	evidence := providerutil.DegradedEvidence(authorization.MediaFamily, envelope.Markdown,
		"LlamaParse returned Markdown without page provenance")
	evidence.Units[0].ProviderID = "llamaparse-markdown"
	evidence.Units[0].Locator.Name = "llamaparse-markdown"
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		return document.RenditionResult{}, provider.Malformed(
			"LlamaParse Markdown cannot be represented", err)
	}
	if err := state.operation.Check(); err != nil {
		return document.RenditionResult{}, err
	}
	state.warnings = []string{"degraded_provenance"}
	return document.RenditionResult{Evidence: evidence, ProviderMarkdown: markdown}, nil
}

func (client *Client) pageEvidence(
	pages []resultPage, authorization document.RenditionAuthorization,
) (document.SourceEvidenceV1, []byte, string, error) {
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: authorization.MediaFamily, UnitKind: document.EvidenceUnitPage,
		Units: make([]document.SourceEvidenceUnitV1, 0, len(pages)),
	}
	markdown := make([]string, 0, len(pages))
	imageName := ""
	for order, page := range pages {
		pageNumber := order + 1
		if page.Page == nil || *page.Page != pageNumber {
			return document.SourceEvidenceV1{}, nil, "", errors.New("page sequence is not complete")
		}
		if page.Status != nil && *page.Status != "" && *page.Status != "SUCCESS" {
			return document.SourceEvidenceV1{}, nil, "", errors.New("page has a non-success status")
		}
		text := ""
		if page.Markdown != nil {
			text = *page.Markdown
		} else if page.Text != nil {
			text = *page.Text
		}
		if !utf8.ValidString(text) || text == "" && (page.NoTextContent == nil || !*page.NoTextContent) {
			return document.SourceEvidenceV1{}, nil, "", errors.New("page has no representable text")
		}
		if page.Confidence != nil && (math.IsNaN(*page.Confidence) || math.IsInf(*page.Confidence, 0) ||
			*page.Confidence < 0 || *page.Confidence > 1) {
			return document.SourceEvidenceV1{}, nil, "", errors.New("page confidence is invalid")
		}
		unit := document.SourceEvidenceUnitV1{
			Order: order, ProviderID: fmt.Sprintf("llamaparse-page-%d", pageNumber), Text: text,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: int64(pageNumber), End: int64(pageNumber),
			},
		}
		if page.Confidence != nil {
			unit.Confidence = &document.SourceEvidenceConfidenceV1{
				Interpretation: document.EvidenceConfidenceProbability,
				Minimum:        0, Maximum: 1, Value: *page.Confidence,
			}
		}
		evidence.Units = append(evidence.Units, unit)
		markdown = append(markdown, text)
		for _, image := range page.Images {
			if !client.profile.RetainImages || imageName != "" || validateArtifactName(image.Name) != nil {
				return document.SourceEvidenceV1{}, nil, "", errors.New("provider images are unrepresentable")
			}
			imageName = image.Name
		}
	}
	if imageName != "" && (client.profile.MaxArtifacts != 1 || authorization.MaxArtifacts < 1 ||
		!slices.Contains(authorization.AllowedArtifactRoles, document.EvidenceArtifactImage)) {
		return document.SourceEvidenceV1{}, nil, "", errors.New("provider image is not authorized")
	}
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		return document.SourceEvidenceV1{}, nil, "", err
	}
	return evidence, []byte(strings.Join(markdown, markdownPageSeparator)), imageName, nil
}

func (client *Client) validateJobMetadata(raw json.RawMessage) (int, error) {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return 0, err
	}
	encodedPages, present := metadata["job_pages"]
	if !present {
		return 0, errors.New("reported page count is missing")
	}
	var pages int
	if err := strictJSON(encodedPages, &pages); err != nil || pages < 0 {
		return 0, errors.New("reported page count is invalid")
	}
	for field, expected := range map[string]string{
		"model": client.profile.Model, "preset": client.profile.Preset,
	} {
		encoded, present := metadata[field]
		if !present {
			continue
		}
		var actual string
		if err := strictJSON(encoded, &actual); err != nil || actual != expected {
			return 0, errExecutionIdentityChanged
		}
	}
	return pages, nil
}

func (client *Client) fetchImage(
	jobID, name string, authorization document.RenditionAuthorization,
	budget int64, state *operationState,
) (document.RenditionArtifact, document.SourceEvidenceArtifactV1, error) {
	limit := min(client.profile.MaxArtifactBytes, int64(authorization.MaxArtifactBytes))
	limit = min(limit, budget)
	response, err := client.executor.Do(state.operation, &state.usage, providerutil.Request{
		Stage: providerutil.StageJob, Method: http.MethodGet, Path: imageResultPath(jobID, name),
		MaxResponseBytes: limit, ResponseMediaType: "image/*",
	})
	if err != nil {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{}, provider.KnownJobError(err)
	}
	if !response.Success() {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{},
			provider.KnownJobError(
				provider.StatusError(providerutil.StageJob, response.Status, response.RetryAfter, nil))
	}
	canonical, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (canonical != "image/png" && canonical != "image/jpeg" && canonical != "image/webp") {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{}, provider.Malformed(
			"LlamaParse image media type changed", err)
	}
	detected, err := media.DetectBytes(response.Body, canonical)
	if err != nil || detected.Kind != media.KindImage || detected.MediaType != canonical ||
		detected.FrameCount != 1 || detected.Animated {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{}, provider.Malformed(
			"LlamaParse image result is malformed", err)
	}
	if err := state.operation.Check(); err != nil {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{}, err
	}
	checksum := providerutil.SHA256Hex(response.Body)
	pointer := "images/" + name
	return document.RenditionArtifact{
		Role: document.EvidenceArtifactImage, MediaType: canonical,
		Payload: response.Body, SHA256: checksum,
	}, document.SourceEvidenceArtifactV1{
		Pointer: pointer, ProviderID: "llamaparse-image-0",
		Role: document.EvidenceArtifactImage, SHA256: checksum,
	}, nil
}

type operationState struct {
	operation   *providerutil.Operation
	usage       providerutil.Usage
	startedAt   time.Time
	completedAt time.Time
	pollDelay   time.Duration
	warnings    []string
}

type resumeStateV1 struct {
	jobID          string
	submittedAt    time.Time
	checkpointedAt time.Time
}

type jobResponse struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	ErrorCode    *string `json:"error_code,omitempty"`
	ErrorMessage *string `json:"error_message,omitempty"`
}

type jsonResult struct {
	Pages       json.RawMessage `json:"pages"`
	JobMetadata json.RawMessage `json:"job_metadata"`
}

type markdownResult struct {
	Markdown    string          `json:"markdown"`
	JobMetadata json.RawMessage `json:"job_metadata"`
}

type resultPage struct {
	Page                *int            `json:"page"`
	Text                *string         `json:"text,omitempty"`
	Markdown            *string         `json:"md,omitempty"`
	Images              []resultImage   `json:"images"`
	Charts              json.RawMessage `json:"charts"`
	Tables              json.RawMessage `json:"tables"`
	Layout              json.RawMessage `json:"layout"`
	Items               json.RawMessage `json:"items"`
	Status              *string         `json:"status,omitempty"`
	Links               json.RawMessage `json:"links"`
	Width               *float64        `json:"width,omitempty"`
	Height              *float64        `json:"height,omitempty"`
	TriggeredAutoMode   *bool           `json:"triggeredAutoMode,omitempty"`
	ParsingMode         string          `json:"parsingMode"`
	StructuredData      json.RawMessage `json:"structuredData,omitempty"`
	NoStructuredContent bool            `json:"noStructuredContent"`
	NoTextContent       *bool           `json:"noTextContent"`
	IsAudioTranscript   bool            `json:"isAudioTranscript,omitempty"`
	DurationInSeconds   *float64        `json:"durationInSeconds,omitempty"`
	SlideSpeakerNotes   *string         `json:"slideSpeakerNotes,omitempty"`
	Confidence          *float64        `json:"confidence,omitempty"`
	PrintedPageNumber   *string         `json:"printedPageNumber,omitempty"`
	PageHeaderMarkdown  *string         `json:"pageHeaderMarkdown,omitempty"`
	PageFooterMarkdown  *string         `json:"pageFooterMarkdown,omitempty"`
}

type resultImage struct {
	Name           string   `json:"name"`
	Height         *float64 `json:"height,omitempty"`
	Width          *float64 `json:"width,omitempty"`
	X              *float64 `json:"x,omitempty"`
	Y              *float64 `json:"y,omitempty"`
	OriginalWidth  *int     `json:"original_width,omitempty"`
	OriginalHeight *int     `json:"original_height,omitempty"`
	Type           *string  `json:"type,omitempty"`
}

func statusPath(jobID string) string            { return "/api/v1/parsing/job/" + jobID }
func jsonResultPath(jobID string) string        { return statusPath(jobID) + "/result/json" }
func markdownResultPath(jobID string) string    { return statusPath(jobID) + "/result/markdown" }
func imageResultPath(jobID, name string) string { return statusPath(jobID) + "/result/image/" + name }

func defaultProfile(profile Profile) Profile {
	if profile.MaxUploadBytes == 0 {
		profile.MaxUploadBytes = defaultUploadBytes
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = profile.MaxUploadBytes + defaultRequestOverhead
	}
	if profile.MaxControlBytes == 0 {
		profile.MaxControlBytes = defaultControlBytes
	}
	if profile.MaxPolls == 0 {
		profile.MaxPolls = 600
	}
	if profile.PollInterval == 0 {
		profile.PollInterval = time.Second
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = 30 * time.Second
	}
	if profile.MaxResultBytes == 0 {
		profile.MaxResultBytes = defaultResultBytes
	}
	if profile.MaxArtifactBytes == 0 {
		profile.MaxArtifactBytes = defaultArtifactBytes
	}
	if profile.MaxWallTime == 0 {
		profile.MaxWallTime = 30 * time.Minute
	}
	if profile.RetainImages && profile.MaxArtifacts == 0 {
		profile.MaxArtifacts = 1
	}
	return profile
}

func validateProfile(profile Profile) error {
	if err := provider.ValidateIdentifier(profile.Model, "model"); err != nil {
		return err
	}
	if err := provider.ValidateIdentifier(profile.Preset, "preset"); err != nil {
		return err
	}
	if profile.MaxUploadBytes <= 0 || profile.MaxUploadBytes > maxConfiguredBytes ||
		profile.MaxRequestBytes <= profile.MaxUploadBytes || profile.MaxRequestBytes > maxConfiguredBytes+defaultRequestOverhead ||
		profile.MaxControlBytes <= 0 || profile.MaxControlBytes > 1<<20 ||
		profile.MaxResultBytes <= 0 || profile.MaxResultBytes > maxConfiguredBytes ||
		profile.MaxArtifactBytes <= 0 || profile.MaxArtifactBytes > maxConfiguredBytes ||
		profile.MaxPolls <= 0 || profile.MaxPolls > maxConfiguredPolls ||
		profile.PollInterval <= 0 || profile.PollInterval > time.Hour ||
		profile.RequestTimeout <= 0 || profile.RequestTimeout > maxConfiguredDuration ||
		profile.MaxWallTime <= 0 || profile.MaxWallTime > maxConfiguredDuration {
		return errors.New("LlamaParse profile bounds are invalid")
	}
	if profile.RetainImages && profile.MaxArtifacts != 1 || !profile.RetainImages && profile.MaxArtifacts != 0 {
		return errors.New("LlamaParse image artifact count is unrepresentable")
	}
	return nil
}

func validateJobID(value string) error {
	if len(value) != 36 {
		return errors.New("job ID is not a canonical UUID")
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return errors.New("job ID is not a canonical UUID")
			}
			continue
		}
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return errors.New("job ID is not a canonical UUID")
		}
	}
	return nil
}

func encodeResumeHandle(state resumeStateV1) string {
	return fmt.Sprintf("lp1.%s.%d.%d", state.jobID,
		state.submittedAt.UnixNano(), state.checkpointedAt.UnixNano())
}

func parseResumeHandle(value string) (resumeStateV1, error) {
	if len(value) > 512 {
		return resumeStateV1{}, errors.New("resume handle exceeds its bound")
	}
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != "lp1" {
		return resumeStateV1{}, errors.New("resume handle version is invalid")
	}
	if err := validateJobID(parts[1]); err != nil {
		return resumeStateV1{}, err
	}
	parseTime := func(encoded string) (time.Time, error) {
		nanoseconds, err := strconv.ParseInt(encoded, 10, 64)
		if err != nil || nanoseconds <= 0 || strconv.FormatInt(nanoseconds, 10) != encoded {
			return time.Time{}, errors.New("resume handle timestamp is invalid")
		}
		return time.Unix(0, nanoseconds).UTC(), nil
	}
	submittedAt, err := parseTime(parts[2])
	if err != nil {
		return resumeStateV1{}, err
	}
	checkpointedAt, err := parseTime(parts[3])
	if err != nil || checkpointedAt.Before(submittedAt) {
		return resumeStateV1{}, errors.New("resume handle checkpoint time is invalid")
	}
	return resumeStateV1{
		jobID: parts[1], submittedAt: submittedAt, checkpointedAt: checkpointedAt,
	}, nil
}

func validateInitialStatus(status string) error {
	switch status {
	case "PENDING", "SUCCESS":
		return nil
	case "ERROR":
		return provider.Classified(document.RenditionErrorUnsupportedInput, "LlamaParse could not parse the input", nil)
	case "PARTIAL_SUCCESS":
		return provider.Malformed("LlamaParse returned partial output", nil)
	case "CANCELLED":
		return provider.Classified(document.RenditionErrorCanceled, "LlamaParse job was canceled", nil)
	default:
		return provider.Classified(document.RenditionErrorPolicyRejected, "LlamaParse submission schema changed", nil)
	}
}

func validateArtifactName(name string) error {
	if err := provider.ValidatePathIdentifier(name, "artifact name"); err != nil {
		return err
	}
	escaped := url.PathEscape(name)
	if escaped != name || strings.ContainsAny(name, "/\\:%") {
		return errors.New("artifact name is not an opaque path segment")
	}
	return nil
}

func strictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("JSON response has trailing values")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON response has trailing data")
	}
	return nil
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}
