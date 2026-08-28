package llamaparse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	"go.kenn.io/docbank/document/media"
)

const (
	apiHost                = "api.cloud.llamaindex.ai"
	apiOrigin              = "https://" + apiHost
	uploadPath             = "/api/v1/parsing/upload"
	providerID             = "llamaparse.parse-v1"
	timeForm               = "2006-01-02T15:04:05.000000000Z"
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

var (
	_ document.RenditionProvider          = (*Client)(nil)
	_ document.ResumableRenditionProvider = (*Client)(nil)
)

// SecretResolver resolves the one credential named by a frozen profile.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, name string) (string, error)
}

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
	secrets    SecretResolver
	transport  http.RoundTripper
}

// NewProvider constructs a provider around an injected hardened transport.
// It never resolves credentials or performs network access during setup.
func NewProvider(profile Profile, secrets SecretResolver, transport http.RoundTripper) (*Client, error) {
	profile = defaultProfile(profile)
	if err := validateProfile(profile); err != nil {
		return nil, err
	}
	if nilValue(secrets) {
		return nil, errors.New("LlamaParse named credential resolver is required")
	}
	if nilValue(transport) {
		return nil, errors.New("LlamaParse hardened transport is required")
	}
	policyDigest := sha256.Sum256([]byte(fmt.Sprintf(
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
		PolicyFingerprint: hex.EncodeToString(policyDigest[:]),
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
		descriptor: cloneDescriptor(descriptor), profile: profile,
		secrets: secrets, transport: &fixedOriginTransport{base: transport},
	}, nil
}

// Descriptor returns a defensive copy of the immutable provider identity.
func (client *Client) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return cloneDescriptor(client.descriptor)
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
	now := time.Now().UTC()
	resumeState, expiresAt, enforceExpiry, err := client.validateInvocation(now, upload, authorization, resume)
	if err != nil {
		return document.RenditionResult{}, err
	}
	operationCtx, cancel := boundedOperationContext(ctx, expiresAt, client.profile.MaxWallTime, enforceExpiry)
	defer cancel()
	state := operationState{startedAt: resumeState.submittedAt, completedAt: resumeState.checkpointedAt,
		enforceExpiry: enforceExpiry}
	var jobID string
	if resume == nil {
		jobID, err = client.submit(operationCtx, ctx, expiresAt, upload, &state)
		if err != nil {
			return document.RenditionResult{}, err
		}
		checkpointedAt := time.Now().UTC()
		if err := client.lifecycleErrorAt(
			ctx, operationCtx, expiresAt, enforceExpiry, checkpointedAt, nil,
		); err != nil {
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
	if err := client.poll(operationCtx, ctx, expiresAt, jobID, &state); err != nil {
		return document.RenditionResult{}, err
	}
	result, err := client.result(operationCtx, ctx, expiresAt, jobID, authorization, &state)
	if err != nil {
		return document.RenditionResult{}, err
	}
	acceptedAt := time.Now().UTC()
	if resume == nil {
		state.completedAt = acceptedAt
	}
	if err := client.lifecycleErrorAt(
		ctx, operationCtx, expiresAt, enforceExpiry, acceptedAt, nil,
	); err != nil {
		return document.RenditionResult{}, err
	}
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, fmt.Errorf("LlamaParse: fingerprint authorization: %w", err)
	}
	result.Receipt = document.RenditionReceipt{
		ProviderID: client.descriptor.ID, DescriptorFingerprint: client.descriptor.Fingerprint,
		PolicyFingerprint:           authorization.PolicyFingerprint,
		RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
		AuthorizationFingerprint:    authorizationFingerprint, SourceSHA256: authorization.SourceSHA256,
		OperationID: "llamaparse-" + jobID[:8],
		StartedAt:   state.startedAt.Format(timeForm), CompletedAt: state.completedAt.Format(timeForm),
		Warnings: state.warnings,
		Usage: document.RenditionUsage{
			Requests: state.requests, Retries: state.retries, InputBytes: authorization.SourceBytes,
			OutputBytes: state.outputBytes, Units: int64(len(result.Evidence.Units)),
		},
		RetryDelayMillis: state.pollDelay.Milliseconds(),
	}
	return result, nil
}

func (client *Client) validateInvocation(
	now time.Time, upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
	resume *document.RenditionResumeHandle,
) (resumeStateV1, time.Time, bool, error) {
	expiresAt, err := time.Parse(timeForm, authorization.ExpiresAt)
	if err != nil {
		return resumeStateV1{}, time.Time{}, false, renditionError(document.RenditionErrorPolicyRejected,
			"LlamaParse authorization expiry is invalid", err)
	}
	if resume == nil {
		if nilValue(upload) {
			return resumeStateV1{}, time.Time{}, false, errors.New("LlamaParse authorized upload is required for submission")
		}
		if _, err := document.ValidateRenditionProviderRequestAt(now, client, upload, authorization); err != nil {
			return resumeStateV1{}, time.Time{}, false, err
		}
	} else {
		if !nilValue(upload) {
			return resumeStateV1{}, time.Time{}, false, errors.New("LlamaParse resume must not receive source bytes")
		}
		parsed, parseErr := parseResumeHandle(resume.Value)
		if parseErr != nil {
			return resumeStateV1{}, time.Time{}, false, renditionError(document.RenditionErrorUnknownJob,
				"LlamaParse resume handle is invalid", parseErr)
		}
		if authorization.ProviderID != client.descriptor.ID ||
			authorization.DescriptorFingerprint != client.descriptor.Fingerprint ||
			authorization.PolicyFingerprint != client.descriptor.PolicyFingerprint {
			return resumeStateV1{}, time.Time{}, false, renditionError(document.RenditionErrorPolicyRejected,
				"LlamaParse resume authority changed", nil)
		}
		authorizedAt, authErr := time.Parse(timeForm, authorization.AuthorizedAt)
		if authErr != nil || parsed.submittedAt.Before(authorizedAt) ||
			parsed.checkpointedAt.Before(parsed.submittedAt) || parsed.checkpointedAt.After(expiresAt) {
			return resumeStateV1{}, time.Time{}, false, renditionError(document.RenditionErrorPolicyRejected,
				"LlamaParse resume receipt facts are outside the sealed authorization", authErr)
		}
		return parsed, expiresAt, false, nil
	}
	return resumeStateV1{}, expiresAt, true, nil
}

func (client *Client) submit(
	operationCtx, callerCtx context.Context, expiresAt time.Time, upload document.AuthorizedUpload,
	state *operationState,
) (string, error) {
	metadata := upload.Metadata()
	if metadata.ByteLength > client.profile.MaxUploadBytes {
		return "", renditionError(document.RenditionErrorPolicyRejected,
			"LlamaParse upload exceeds profile limit", nil)
	}
	source, err := readExactUpload(operationCtx, upload, metadata, client.profile.MaxUploadBytes)
	if err != nil {
		if operationCtx.Err() != nil {
			return "", client.contextOrTransient(callerCtx, operationCtx, expiresAt, state.enforceExpiry,
				"LlamaParse upload read timed out", err)
		}
		return "", err
	}
	defer clear(source)
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	for _, field := range [][2]string{
		{"model", client.profile.Model}, {"preset", client.profile.Preset},
		{"page_error_tolerance", "0"}, {"save_images", strconv.FormatBool(client.profile.RetainImages)},
		{"disable_image_extraction", strconv.FormatBool(!client.profile.RetainImages)},
		{"take_screenshot", "false"},
	} {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return "", renditionError(document.RenditionErrorTransient,
				"LlamaParse request could not be built", err)
		}
	}
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
			"LlamaParse request could not be built", err)
	}
	if int64(body.Len()) > client.profile.MaxRequestBytes {
		return "", renditionError(document.RenditionErrorPolicyRejected,
			"LlamaParse request exceeds profile limit", nil)
	}
	state.startedAt = time.Now().UTC()
	raw, status, err := client.do(operationCtx, http.MethodPost, uploadPath,
		writer.FormDataContentType(), body.Bytes(), client.profile.MaxControlBytes, state)
	if err != nil {
		if providerError, ok := errors.AsType[*document.RenditionProviderError](err); ok &&
			providerError.Code() == document.RenditionErrorAuthentication {
			return "", err
		}
		if callerCtx.Err() != nil {
			return "", renditionError(document.RenditionErrorAmbiguousSubmission,
				"LlamaParse submission outcome is ambiguous", callerCtx.Err())
		}
		return "", renditionError(document.RenditionErrorAmbiguousSubmission,
			"LlamaParse submission outcome is ambiguous", err)
	}
	if status < 200 || status >= 300 {
		return "", client.httpError(status, "submission", raw)
	}
	var job jobResponse
	if err := strictJSON(raw, &job); err != nil || validateJobID(job.ID) != nil {
		return "", renditionError(document.RenditionErrorPolicyRejected,
			"LlamaParse submission schema changed", err)
	}
	if err := validateInitialStatus(job.Status); err != nil {
		return "", err
	}
	return job.ID, nil
}

func (client *Client) poll(
	operationCtx, callerCtx context.Context, expiresAt time.Time, jobID string,
	state *operationState,
) error {
	for attempt := range client.profile.MaxPolls {
		raw, status, err := client.do(operationCtx, http.MethodGet, statusPath(jobID), "", nil,
			client.profile.MaxControlBytes, state)
		if err != nil {
			return client.contextOrTransient(callerCtx, operationCtx, expiresAt, state.enforceExpiry,
				"LlamaParse status request failed", err)
		}
		if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, nil); err != nil {
			return err
		}
		if status < 200 || status >= 300 {
			return client.httpError(status, "status", raw)
		}
		var job jobResponse
		if err := strictJSON(raw, &job); err != nil || job.ID != jobID {
			return renditionError(document.RenditionErrorPolicyRejected,
				"LlamaParse status schema changed", err)
		}
		switch job.Status {
		case "SUCCESS":
			return nil
		case "PENDING":
			if attempt+1 == client.profile.MaxPolls {
				break
			}
			state.retries++
			if err := waitContext(operationCtx, client.profile.PollInterval); err != nil {
				return client.contextOrTransient(callerCtx, operationCtx, expiresAt, state.enforceExpiry,
					"LlamaParse polling timed out", err)
			}
			state.pollDelay += client.profile.PollInterval
		case "ERROR":
			return renditionError(document.RenditionErrorUnsupportedInput,
				"LlamaParse could not parse the input", nil)
		case "PARTIAL_SUCCESS":
			return renditionError(document.RenditionErrorMalformedEvidence,
				"LlamaParse returned partial output", nil)
		case "CANCELLED":
			return renditionError(document.RenditionErrorCanceled,
				"LlamaParse job was canceled", nil)
		default:
			return renditionError(document.RenditionErrorPolicyRejected,
				"LlamaParse status schema changed", nil)
		}
	}
	return renditionError(document.RenditionErrorTransient,
		"LlamaParse polling limit was reached", nil)
}

func (client *Client) result(
	operationCtx, callerCtx context.Context, expiresAt time.Time, jobID string,
	authorization document.RenditionAuthorization, state *operationState,
) (document.RenditionResult, error) {
	budget := min64(client.profile.MaxResultBytes, int64(authorization.MaxTotalResultBytes))
	raw, status, err := client.do(operationCtx, http.MethodGet, jsonResultPath(jobID), "", nil,
		budget, state)
	if err != nil {
		return document.RenditionResult{}, client.contextOrMalformed(
			callerCtx, operationCtx, expiresAt, state.enforceExpiry, "LlamaParse result request failed", err)
	}
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, nil); err != nil {
		return document.RenditionResult{}, err
	}
	if status < 200 || status >= 300 {
		return document.RenditionResult{}, client.httpError(status, "result", raw)
	}
	budget -= int64(len(raw))
	var envelope jsonResult
	parseErr := strictJSON(raw, &envelope)
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, parseErr); err != nil {
		return document.RenditionResult{}, err
	}
	if parseErr != nil || len(envelope.Pages) == 0 || !jsonObject(envelope.JobMetadata) {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse result schema changed", parseErr)
	}
	jobPages, err := client.validateJobMetadata(envelope.JobMetadata)
	if lifecycleErr := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, err); lifecycleErr != nil {
		return document.RenditionResult{}, lifecycleErr
	}
	if err != nil {
		if errors.Is(err, errExecutionIdentityChanged) {
			return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
				"LlamaParse model or preset changed", err)
		}
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse result metadata is malformed", err)
	}
	var pages []resultPage
	pageParseErr := strictJSON(envelope.Pages, &pages)
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, pageParseErr); err != nil {
		return document.RenditionResult{}, err
	}
	if pageParseErr != nil {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse page schema changed", pageParseErr)
	}
	if len(pages) == 0 {
		if jobPages != 0 {
			return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
				"LlamaParse page output is incomplete", nil)
		}
		return client.markdownFallback(
			operationCtx, callerCtx, expiresAt, jobID, authorization, jobPages, budget, state)
	}
	if jobPages <= 0 || len(pages) != jobPages {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse page output is incomplete", nil)
	}
	evidence, markdown, imageName, err := client.pageEvidence(pages, authorization)
	if lifecycleErr := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, err); lifecycleErr != nil {
		return document.RenditionResult{}, lifecycleErr
	}
	if err != nil {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse page output is incomplete", err)
	}
	if len(markdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse Markdown exceeds authorization", nil)
	}
	result := document.RenditionResult{Evidence: evidence, ProviderMarkdown: markdown}
	if imageName != "" {
		artifact, sourceArtifact, artifactErr := client.fetchImage(
			operationCtx, callerCtx, expiresAt, jobID, imageName, authorization, budget, state)
		if artifactErr != nil {
			return document.RenditionResult{}, artifactErr
		}
		result.Artifacts = []document.RenditionArtifact{artifact}
		result.Evidence.Artifacts = []document.SourceEvidenceArtifactV1{sourceArtifact}
	}
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, nil); err != nil {
		return document.RenditionResult{}, err
	}
	return result, nil
}

func (client *Client) markdownFallback(
	operationCtx, callerCtx context.Context, expiresAt time.Time, jobID string,
	authorization document.RenditionAuthorization, expectedJobPages int, budget int64, state *operationState,
) (document.RenditionResult, error) {
	raw, status, err := client.do(operationCtx, http.MethodGet, markdownResultPath(jobID), "", nil,
		budget, state)
	if err != nil {
		return document.RenditionResult{}, client.contextOrMalformed(
			callerCtx, operationCtx, expiresAt, state.enforceExpiry, "LlamaParse Markdown request failed", err)
	}
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, nil); err != nil {
		return document.RenditionResult{}, err
	}
	if status < 200 || status >= 300 {
		return document.RenditionResult{}, client.httpError(status, "Markdown result", raw)
	}
	var envelope markdownResult
	parseErr := strictJSON(raw, &envelope)
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, parseErr); err != nil {
		return document.RenditionResult{}, err
	}
	if parseErr != nil || !jsonObject(envelope.JobMetadata) ||
		envelope.Markdown == "" || !utf8.ValidString(envelope.Markdown) {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse Markdown result is malformed", parseErr)
	}
	jobPages, metadataErr := client.validateJobMetadata(envelope.JobMetadata)
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, metadataErr); err != nil {
		return document.RenditionResult{}, err
	}
	if metadataErr != nil {
		if errors.Is(metadataErr, errExecutionIdentityChanged) {
			return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
				"LlamaParse model or preset changed", metadataErr)
		}
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse result metadata is malformed", metadataErr)
	}
	if jobPages != expectedJobPages {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse result page counts disagree", nil)
	}
	markdown := []byte(envelope.Markdown)
	if len(markdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse Markdown exceeds authorization", nil)
	}
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceDegradedProvenance,
		Family:          authorization.MediaFamily, UnitKind: document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "natural_provenance",
			Reason: "LlamaParse returned Markdown without page provenance",
		}},
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, ProviderID: "llamaparse-markdown", Text: envelope.Markdown,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
				Name: "llamaparse-markdown",
			},
		}},
	}
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"LlamaParse Markdown cannot be represented", err)
	}
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, nil); err != nil {
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
		if page.Page == nil || *page.Page != order {
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
			Order: order, ProviderID: fmt.Sprintf("llamaparse-page-%d", order), Text: text,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginZero,
				Start: int64(order), End: int64(order),
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
	operationCtx, callerCtx context.Context, expiresAt time.Time, jobID, name string,
	authorization document.RenditionAuthorization, budget int64, state *operationState,
) (document.RenditionArtifact, document.SourceEvidenceArtifactV1, error) {
	limit := min64(client.profile.MaxArtifactBytes, int64(authorization.MaxArtifactBytes))
	limit = min64(limit, budget)
	raw, status, mediaType, err := client.doWithMedia(operationCtx, http.MethodGet,
		imageResultPath(jobID, name), "", nil, limit, state)
	if err != nil {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{},
			client.contextOrMalformed(callerCtx, operationCtx, expiresAt, state.enforceExpiry,
				"LlamaParse image request failed", err)
	}
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, nil); err != nil {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{}, err
	}
	if status < 200 || status >= 300 {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{},
			client.httpError(status, "image result", raw)
	}
	canonical, _, err := mime.ParseMediaType(mediaType)
	if err != nil || (canonical != "image/png" && canonical != "image/jpeg" && canonical != "image/webp") {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{}, renditionError(
			document.RenditionErrorMalformedEvidence, "LlamaParse image media type changed", err)
	}
	detected, err := media.DetectBytes(raw, canonical)
	if err != nil || detected.Kind != media.KindImage || detected.MediaType != canonical ||
		detected.FrameCount != 1 || detected.Animated {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{}, renditionError(
			document.RenditionErrorMalformedEvidence, "LlamaParse image result is malformed", err)
	}
	if err := client.lifecycleError(callerCtx, operationCtx, expiresAt, state.enforceExpiry, nil); err != nil {
		return document.RenditionArtifact{}, document.SourceEvidenceArtifactV1{}, err
	}
	digest := sha256.Sum256(raw)
	checksum := hex.EncodeToString(digest[:])
	pointer := "images/" + name
	return document.RenditionArtifact{
		Role: document.EvidenceArtifactImage, MediaType: canonical,
		Payload: raw, SHA256: checksum,
	}, document.SourceEvidenceArtifactV1{
		Pointer: pointer, ProviderID: "llamaparse-image-0",
		Role: document.EvidenceArtifactImage, SHA256: checksum,
	}, nil
}

func (client *Client) do(
	ctx context.Context, method, path, contentType string, body []byte, limit int64,
	state *operationState,
) ([]byte, int, error) {
	raw, status, _, err := client.doWithMedia(ctx, method, path, contentType, body, limit, state)
	return raw, status, err
}

func (client *Client) doWithMedia(
	ctx context.Context, method, path, contentType string, body []byte, limit int64,
	state *operationState,
) ([]byte, int, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, "", err
	}
	credential, err := client.secrets.ResolveSecret(ctx, client.profile.SecretBinding)
	if err != nil || !validCredential(credential) {
		return nil, 0, "", renditionError(document.RenditionErrorAuthentication,
			"LlamaParse credential is unavailable", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, apiOrigin+path, reader)
	if err != nil {
		return nil, 0, "", err
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	state.requests++
	response, err := client.transport.RoundTrip(request)
	if err != nil {
		return nil, 0, "", err
	}
	if response == nil || response.Body == nil {
		return nil, 0, "", errors.New("provider returned an empty HTTP response")
	}
	raw, readErr := readBounded(response.Body, limit)
	closeErr := response.Body.Close()
	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, response.StatusCode, response.Header.Get("Content-Type"), ctxErr
		}
		return nil, response.StatusCode, response.Header.Get("Content-Type"), readErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, response.StatusCode, response.Header.Get("Content-Type"), ctxErr
	}
	if closeErr != nil {
		return nil, response.StatusCode, response.Header.Get("Content-Type"),
			fmt.Errorf("close provider response: %w", closeErr)
	}
	state.outputBytes += int64(len(raw))
	return raw, response.StatusCode, response.Header.Get("Content-Type"), nil
}

func (client *Client) httpError(status int, stage string, raw []byte) error {
	_ = raw // Provider bodies stay private and are never interpolated.
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return renditionError(document.RenditionErrorAuthentication,
			"LlamaParse authentication was rejected", nil)
	case http.StatusTooManyRequests:
		return renditionError(document.RenditionErrorRateLimited,
			"LlamaParse rate limit was exhausted", nil)
	case http.StatusServiceUnavailable, http.StatusInsufficientStorage:
		return renditionError(document.RenditionErrorCapacity,
			"LlamaParse capacity is unavailable", nil)
	case http.StatusNotFound, http.StatusGone:
		if stage != "submission" {
			return renditionError(document.RenditionErrorUnknownJob,
				"LlamaParse job is unknown or expired", nil)
		}
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusUnsupportedMediaType,
		http.StatusRequestEntityTooLarge:
		return renditionError(document.RenditionErrorUnsupportedInput,
			"LlamaParse rejected the input", nil)
	}
	if status >= 300 && status < 400 {
		return renditionError(document.RenditionErrorPolicyRejected,
			"LlamaParse redirects are refused", nil)
	}
	if status >= 500 {
		return renditionError(document.RenditionErrorTransient,
			"LlamaParse service request failed", nil)
	}
	return renditionError(document.RenditionErrorPolicyRejected,
		"LlamaParse rejected the request", nil)
}

func (client *Client) contextOrTransient(
	callerCtx, operationCtx context.Context, expiresAt time.Time, enforceExpiry bool,
	message string, cause error,
) error {
	if callerCtx.Err() != nil {
		return renditionError(document.RenditionErrorCanceled, "LlamaParse rendering was canceled", callerCtx.Err())
	}
	if enforceExpiry && !time.Now().UTC().Before(expiresAt) {
		return expiredError(cause)
	}
	if operationCtx.Err() != nil {
		return renditionError(document.RenditionErrorTransient, "LlamaParse operation timed out", cause)
	}
	if hasRenditionErrorCode(cause, document.RenditionErrorAuthentication) {
		return cause
	}
	return renditionError(document.RenditionErrorTransient, message, cause)
}

func (client *Client) contextOrMalformed(
	callerCtx, operationCtx context.Context, expiresAt time.Time, enforceExpiry bool,
	message string, cause error,
) error {
	if callerCtx.Err() != nil || operationCtx.Err() != nil {
		return client.contextOrTransient(callerCtx, operationCtx, expiresAt, enforceExpiry, message, cause)
	}
	if hasRenditionErrorCode(cause, document.RenditionErrorAuthentication) {
		return cause
	}
	return renditionError(document.RenditionErrorMalformedEvidence, message, cause)
}

func (client *Client) lifecycleError(
	callerCtx, operationCtx context.Context, expiresAt time.Time, enforceExpiry bool, cause error,
) error {
	return client.lifecycleErrorAt(
		callerCtx, operationCtx, expiresAt, enforceExpiry, time.Now().UTC(), cause)
}

func (*Client) lifecycleErrorAt(
	callerCtx, operationCtx context.Context, expiresAt time.Time, enforceExpiry bool,
	observedAt time.Time, cause error,
) error {
	if callerCtx.Err() != nil {
		return renditionError(document.RenditionErrorCanceled,
			"LlamaParse rendering was canceled", callerCtx.Err())
	}
	if enforceExpiry && !observedAt.Before(expiresAt) {
		return expiredError(cause)
	}
	if operationCtx.Err() != nil {
		return renditionError(document.RenditionErrorTransient,
			"LlamaParse operation timed out", operationCtx.Err())
	}
	return nil
}

type operationState struct {
	startedAt     time.Time
	completedAt   time.Time
	enforceExpiry bool
	requests      int64
	retries       int64
	outputBytes   int64
	pollDelay     time.Duration
	warnings      []string
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

type fixedOriginTransport struct{ base http.RoundTripper }

func (transport *fixedOriginTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.URL.Scheme != "https" ||
		request.URL.Host != apiHost || request.URL.User != nil || request.URL.RawQuery != "" ||
		request.URL.Fragment != "" || request.Host != "" && request.Host != apiHost {
		return nil, errors.New("LlamaParse request destination is not fixed")
	}
	return transport.base.RoundTrip(request)
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
	for subject, value := range map[string]string{
		"model": profile.Model, "preset": profile.Preset, "credential binding": profile.SecretBinding,
	} {
		if err := validateToken(value); err != nil {
			return fmt.Errorf("LlamaParse %s: %w", subject, err)
		}
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

func validateToken(value string) error {
	if value == "" || len(value) > 128 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return errors.New("value must contain 1-128 canonical characters")
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("_.-", char) {
			continue
		}
		return errors.New("value contains unsupported characters")
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
		return renditionError(document.RenditionErrorUnsupportedInput, "LlamaParse could not parse the input", nil)
	case "PARTIAL_SUCCESS":
		return renditionError(document.RenditionErrorMalformedEvidence, "LlamaParse returned partial output", nil)
	case "CANCELLED":
		return renditionError(document.RenditionErrorCanceled, "LlamaParse job was canceled", nil)
	default:
		return renditionError(document.RenditionErrorPolicyRejected, "LlamaParse submission schema changed", nil)
	}
}

func validateArtifactName(name string) error {
	if err := validateToken(name); err != nil {
		return err
	}
	if name == "." || name == ".." {
		return errors.New("artifact name is not an opaque path segment")
	}
	escaped := url.PathEscape(name)
	if escaped != name || strings.ContainsAny(name, "/\\:%") {
		return errors.New("artifact name is not an opaque path segment")
	}
	return nil
}

func validCredential(value string) bool {
	if value == "" || len(value) > 4096 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char == 0x7f {
			return false
		}
	}
	return true
}

func readExactUpload(ctx context.Context, upload io.Reader, metadata document.AuthorizedUploadMetadata, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: upload}, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, renditionError(document.RenditionErrorTransient, "LlamaParse upload could not be read", err)
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != metadata.ByteLength || int64(len(data)) > limit || hex.EncodeToString(digest[:]) != metadata.SHA256 {
		clear(data)
		return nil, renditionError(document.RenditionErrorPolicyRejected, "LlamaParse upload identity changed", nil)
	}
	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("response byte budget is exhausted")
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("provider response exceeds byte limit")
	}
	return data, nil
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
	ctx context.Context, expiresAt time.Time, wall time.Duration, enforceExpiry bool,
) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(wall)
	if enforceExpiry && expiresAt.Before(deadline) {
		deadline = expiresAt
	}
	return context.WithDeadline(ctx, deadline)
}

func renditionError(code document.RenditionErrorCode, message string, cause error) error {
	providerError, err := document.NewRenditionProviderError(code, message, 0, cause)
	if err != nil {
		return fmt.Errorf("LlamaParse error classification failed: %w", err)
	}
	return providerError
}

func hasRenditionErrorCode(err error, code document.RenditionErrorCode) bool {
	providerError, ok := errors.AsType[*document.RenditionProviderError](err)
	return ok && providerError.Code() == code
}

func expiredError(cause error) error {
	return renditionError(document.RenditionErrorPolicyRejected, "LlamaParse authorization expired", cause)
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
