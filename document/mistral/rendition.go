package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	renditionProviderID = "mistral.ocr-v1"
	renditionProvider   = providerutil.Provider("Mistral")
)

var _ document.RenditionProvider = (*RenditionClient)(nil)

// SecretResolver resolves only the profile-bound Mistral OCR credential.
type SecretResolver = providerutil.SecretResolver

// Profile binds one rendition adapter to the existing Mistral OCR policy and
// its complete capability evidence.
type Profile struct {
	Policy             Policy
	CapabilityManifest CapabilityManifest
	Descriptor         document.RenditionDescriptor
	SecretBinding      string
	Timeout            time.Duration
	MaxRetries         int
	MaxRetryDelay      time.Duration
}

// RenditionClient adapts the bounded Mistral OCR operation to Docbank source
// evidence. OCR remains separate from Mistral embedding operations.
type RenditionClient struct {
	descriptor document.RenditionDescriptor
	policy     Policy
	manifest   CapabilityManifest
	ocr        *Client
}

// NewRenditionProvider validates a fixed hosted OCR profile without resolving
// its named credential or performing network access.
func NewRenditionProvider(
	profile Profile, secrets SecretResolver, httpClient *http.Client,
) (*RenditionClient, error) {
	if profile.Policy.digest == "" {
		return nil, errors.New("mistral rendition policy is invalid; use NewPolicy")
	}
	manifest := cloneCapabilityManifest(profile.CapabilityManifest)
	if err := manifest.ValidateComplete(); err != nil {
		return nil, fmt.Errorf("mistral rendition capability manifest: %w", err)
	}
	descriptor, err := renditionProvider.CanonicalDescriptor(profile.Descriptor)
	if err != nil {
		return nil, err
	}
	if descriptor.ID != renditionProviderID {
		return nil, fmt.Errorf("mistral rendition descriptor ID must be %s", renditionProviderID)
	}
	if descriptor.TrustBoundary != document.RenditionTrustHostedProvider ||
		!descriptor.ReturnsMarkdown || !descriptor.ReturnsStructured || len(descriptor.ArtifactRoles) != 0 {
		return nil, errors.New("mistral rendition descriptor result or trust contract is invalid")
	}
	fingerprint, err := profile.Policy.Fingerprint(manifest)
	if err != nil {
		return nil, fmt.Errorf("mistral rendition policy fingerprint: %w", err)
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("mistral rendition descriptor policy fingerprint does not match capability evidence")
	}
	for _, format := range descriptor.SupportedFormats {
		candidate, ok := renditionCandidate(format)
		if !ok || format.InputKind != document.RenditionInputOriginalFile {
			return nil, errors.New("mistral rendition descriptor format is not locally detectable")
		}
		authority, authorizeErr := profile.Policy.Authorize(manifest, candidate.ID)
		if authorizeErr != nil || authority.method == UnitBoundNone {
			return nil, fmt.Errorf("mistral rendition format %q has no enforceable upload authority", candidate.ID)
		}
	}
	if profile.SecretBinding == "" || providerutil.IsNil(secrets) {
		return nil, errors.New("mistral rendition requires a named secret binding and resolver")
	}
	if err := renditionProvider.ValidateIdentifier(profile.SecretBinding, "secret binding"); err != nil {
		return nil, err
	}
	if httpClient == nil {
		return nil, errors.New("mistral rendition HTTP client is required")
	}
	if !providerutil.Bounded(&profile.Timeout, DefaultTimeout, MaxTimeout) ||
		!providerutil.Bounded(&profile.MaxRetries, DefaultMaxRetries, MaxRetries) ||
		!providerutil.Bounded(&profile.MaxRetryDelay, DefaultMaxRetryDelay, MaxRetryDelay) {
		return nil, errors.New("mistral rendition execution bounds are invalid")
	}
	isolate := providerhttp.IsolateClient(httpClient)
	if isolate.Timeout <= 0 || isolate.Timeout > profile.Timeout {
		isolate.Timeout = profile.Timeout
	}
	ocr, err := newClientWithCredential(profile.Policy, ClientConfig{
		Timeout: profile.Timeout, MaxRetries: profile.MaxRetries,
		MaxRetryDelay: profile.MaxRetryDelay, HTTPClient: isolate,
	}, func(ctx context.Context) (string, error) {
		return secrets.ResolveSecret(ctx, profile.SecretBinding)
	})
	if err != nil {
		return nil, fmt.Errorf("mistral rendition client: %w", err)
	}
	return &RenditionClient{descriptor: descriptor, policy: profile.Policy, manifest: manifest, ocr: ocr}, nil
}

// Descriptor returns an immutable copy of the configured provider identity.
func (client *RenditionClient) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(client.descriptor)
}

// Render verifies the one-shot upload again, re-detects its exact media type,
// and maps bounded OCR output into provider-neutral source evidence.
func (client *RenditionClient) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("mistral rendition client is required")
	}
	metadata := upload.Metadata()
	if metadata.ByteLength > client.policy.values.MaxDocumentBytes {
		return document.RenditionResult{}, renditionProvider.Classified(document.RenditionErrorPolicyRejected,
			"Mistral input exceeds the policy byte limit", nil)
	}
	operation, err := providerutil.NewOperation(ctx, renditionProvider, authorization.ExpiresAt, 0)
	if err != nil {
		return document.RenditionResult{}, err
	}
	defer operation.Cancel()
	startedAt := time.Now().UTC()
	source, err := operation.ReadUpload(upload)
	if err != nil {
		return document.RenditionResult{}, err
	}
	defer clear(source)
	candidate, localUnits, err := client.verifySource(source, metadata)
	if err != nil {
		return document.RenditionResult{}, err
	}
	providerResult, err := client.process(operation, source, metadata, candidate, localUnits,
		min(client.policy.values.MaxResponseBytes, int64(authorization.MaxTotalResultBytes)))
	if err != nil {
		return document.RenditionResult{}, err
	}
	if (candidate.ID == formatIDPDF || localUnits > 0) && int64(providerResult.UnitsProcessed) != localUnits {
		message := "Mistral OCR unit count changed"
		if candidate.ID == formatIDPDF {
			message = "Mistral OCR page count changed"
		}
		return document.RenditionResult{}, renditionProvider.Classified(document.RenditionErrorPolicyRejected,
			message, ErrCapabilityContract)
	}
	completedAt := time.Now().UTC()
	if err := operation.Check(); err != nil {
		return document.RenditionResult{}, err
	}
	includeMarkdown := authorization.MaxProviderMarkdownBytes > 0
	evidence, markdown, err := mistralEvidence(
		providerResult.Document, includeMarkdown, int64(authorization.MaxTotalResultBytes),
	)
	if err != nil {
		return document.RenditionResult{}, renditionProvider.Malformed("Mistral OCR output is malformed", err)
	}
	if providerutil.InjectsDocbankFrontmatter(markdown) {
		return document.RenditionResult{}, renditionProvider.Malformed(
			"Mistral OCR provider Markdown attempts Docbank frontmatter injection", nil)
	}
	if len(markdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, renditionProvider.Malformed("Mistral OCR Markdown exceeds authorization", nil)
	}
	receipt, err := providerutil.NewReceipt(renditionProvider, providerutil.Receipt{
		Descriptor: client.descriptor, Authorization: authorization, SourceSHA256: metadata.SHA256,
		OperationID: "mistral-" + authorization.RenditionRequestFingerprint,
		StartedAt:   startedAt, CompletedAt: completedAt,
		Usage: document.RenditionUsage{
			Requests: int64(providerResult.Metrics.Requests), Retries: int64(providerResult.Metrics.Retries),
			InputBytes: metadata.ByteLength, OutputBytes: providerResult.ResponseBytes,
			Units: int64(providerResult.UnitsProcessed),
		},
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return document.RenditionResult{Evidence: evidence, ProviderMarkdown: markdown, Receipt: receipt}, nil
}

// verifySource re-detects the exact format and proves any registered local
// unit count before any byte leaves the process.
func (client *RenditionClient) verifySource(
	source []byte, metadata document.AuthorizedUploadMetadata,
) (CandidateFormat, int64, error) {
	candidate, err := formatdetect.DetectFormat(bytes.NewReader(source), int64(len(source)), metadata.MediaType)
	if err != nil {
		return CandidateFormat{}, 0, renditionProvider.Classified(document.RenditionErrorUnsupportedInput,
			"Mistral input format could not be verified", err)
	}
	if candidate.Family != metadata.MediaFamily || candidate.MediaType != metadata.MediaType {
		return CandidateFormat{}, 0, renditionProvider.Classified(document.RenditionErrorPolicyRejected,
			"Mistral input identity does not match authorization", nil)
	}
	if candidate.ID == formatIDPDF {
		localUnits, err := formatdetect.CountPDFPages(source)
		if err != nil {
			return CandidateFormat{}, 0, renditionProvider.Classified(document.RenditionErrorUnsupportedInput,
				"Mistral PDF page count could not be verified", err)
		}
		if localUnits <= 0 || localUnits > int64(client.policy.values.MaxUnits) {
			return CandidateFormat{}, 0, renditionProvider.Classified(document.RenditionErrorPolicyRejected,
				"Mistral PDF exceeds the complete unit limit", nil)
		}
		return candidate, localUnits, nil
	}
	if expectedUnitBound(candidate.ID) != UnitBoundLocalExact {
		return candidate, 0, nil
	}
	localUnits, err := countLocalUnits(candidate, bytes.NewReader(source), int64(len(source)))
	if err != nil {
		return CandidateFormat{}, 0, renditionProvider.Classified(document.RenditionErrorUnsupportedInput,
			"Mistral OCR local unit count could not be verified", err)
	}
	if localUnits <= 0 || int64(localUnits) > int64(client.policy.values.MaxUnits) {
		return CandidateFormat{}, 0, renditionProvider.Classified(document.RenditionErrorPolicyRejected,
			"Mistral OCR exceeds the complete unit limit", nil)
	}
	return candidate, int64(localUnits), nil
}

func (client *RenditionClient) process(
	operation *providerutil.Operation, source []byte, metadata document.AuthorizedUploadMetadata,
	candidate CandidateFormat, localUnits int64, maxResponseBytes int64,
) (Result, error) {
	formatAuthorization, err := client.policy.Authorize(client.manifest, candidate.ID)
	if err != nil {
		return Result{}, renditionProvider.Classified(document.RenditionErrorUnsupportedInput,
			"Mistral input has no enforceable upload authority", err)
	}
	snapshot := preparedSnapshot{
		size: int64(len(source)), sha256: metadata.SHA256, format: candidate,
		mediaType: metadata.MediaType, localUnits: int(localUnits),
	}
	snapshotForAttempt := func() (preparedSnapshot, error) {
		digest := sha256.Sum256(source)
		if int64(len(source)) != metadata.ByteLength || hex.EncodeToString(digest[:]) != metadata.SHA256 {
			return preparedSnapshot{}, errors.New("mistral rendition source identity changed")
		}
		return snapshot, nil
	}
	readDocument := func(attemptCtx context.Context, attempt preparedSnapshot) ([]byte, error) {
		if err := attemptCtx.Err(); err != nil {
			return nil, err
		}
		if attempt.size != int64(len(source)) || attempt.sha256 != metadata.SHA256 {
			return nil, errors.New("mistral rendition source identity changed")
		}
		return bytes.Clone(source), nil
	}
	options := probeRequestOptions(candidate, client.policy.values.MaxUnits,
		client.policy.values.ExtractHeader, client.policy.values.ExtractFooter)
	providerResult, err := client.ocr.processWith(
		operation.Context(), snapshotForAttempt, readDocument, operation.Check, options,
		formatAuthorization.method, client.policy.values.MaxUnits, maxResponseBytes,
	)
	if err != nil {
		if operationErr := operation.Check(); operationErr != nil {
			return Result{}, operationErr
		}
		return Result{}, classifyRenditionError(err)
	}
	return providerResult, nil
}

func mistralEvidence(
	source document.SourceDocument, includeMarkdown bool, maxResultBytes int64,
) (document.SourceEvidenceV1, []byte, error) {
	unitKind, locatorKind, indexed, ok := renditionUnitKinds(source.UnitKind)
	if !ok || len(source.Units) == 0 || maxResultBytes <= 0 {
		return document.SourceEvidenceV1{}, nil, errors.New("unsupported or empty Mistral OCR unit sequence")
	}
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: source.Family, UnitKind: unitKind,
		Units: make([]document.SourceEvidenceUnitV1, 0, len(source.Units)),
	}
	markdownPages := make([]string, 0, len(source.Units))
	remainingBytes := maxResultBytes
	for order, sourceUnit := range source.Units {
		if sourceUnit.Index != order || !utf8.ValidString(sourceUnit.Markdown) ||
			!utf8.ValidString(sourceUnit.Header) || !utf8.ValidString(sourceUnit.Footer) {
			return document.SourceEvidenceV1{}, nil, errors.New("invalid Mistral OCR unit")
		}
		evidenceBytes := mistralUnitBytes(sourceUnit)
		markdownBytes := int64(0)
		if includeMarkdown {
			markdownBytes = int64(len(sourceUnit.Markdown))
			if order > 0 {
				markdownBytes += int64(len("\n\n---\n\n"))
			}
		}
		if evidenceBytes > remainingBytes || markdownBytes > remainingBytes-evidenceBytes {
			return document.SourceEvidenceV1{}, nil, errors.New("mistral OCR output exceeds authorization")
		}
		remainingBytes -= evidenceBytes + markdownBytes
		text := joinMistralUnit(sourceUnit)
		locator := document.SourceEvidenceLocatorV1{
			Kind: locatorKind, IndexOrigin: document.EvidenceIndexOriginNone,
		}
		if indexed {
			locator.IndexOrigin = document.EvidenceIndexOriginZero
			locator.Start, locator.End = int64(order), int64(order)
			if locatorKind == document.EvidenceLocatorSheet {
				locator.Name = fmt.Sprintf("mistral-unit-%d", order)
			}
		} else {
			locator.Name = fmt.Sprintf("mistral-unit-%d", order)
		}
		evidence.Units = append(evidence.Units, document.SourceEvidenceUnitV1{
			Order: order, ProviderID: fmt.Sprintf("mistral-unit-%d", order), Text: text, Locator: locator,
		})
		if includeMarkdown {
			markdownPages = append(markdownPages, sourceUnit.Markdown)
		}
	}
	var markdown []byte
	if includeMarkdown {
		markdown = []byte(strings.Join(markdownPages, "\n\n---\n\n"))
		if len(markdown) == 0 {
			return document.SourceEvidenceV1{}, nil, errors.New("mistral OCR returned empty Markdown")
		}
	}
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		return document.SourceEvidenceV1{}, nil, err
	}
	return evidence, markdown, nil
}

func mistralUnitBytes(unit document.SourceUnit) int64 {
	parts := int64(0)
	total := int64(0)
	for _, part := range []string{unit.Header, unit.Markdown, unit.Footer} {
		if part == "" {
			continue
		}
		if parts > 0 {
			total += int64(len("\n\n"))
		}
		total += int64(len(part))
		parts++
	}
	return total
}

func joinMistralUnit(unit document.SourceUnit) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{unit.Header, unit.Markdown, unit.Footer} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n")
}

func renditionUnitKinds(
	unitKind string,
) (document.EvidenceUnitKind, document.EvidenceLocatorKind, bool, bool) {
	switch unitKind {
	case "page":
		return document.EvidenceUnitPage, document.EvidenceLocatorPage, true, true
	case "slide":
		return document.EvidenceUnitSlide, document.EvidenceLocatorSlide, true, true
	case "sheet":
		return document.EvidenceUnitSheet, document.EvidenceLocatorSheet, true, true
	case "record":
		return document.EvidenceUnitRecord, document.EvidenceLocatorRecord, true, true
	case "spine":
		return document.EvidenceUnitSpine, document.EvidenceLocatorSpine, true, true
	case "line":
		return document.EvidenceUnitLine, document.EvidenceLocatorLine, true, true
	case "message":
		return document.EvidenceUnitMessage, document.EvidenceLocatorMessage, false, true
	case "section":
		return document.EvidenceUnitSection, document.EvidenceLocatorSection, false, true
	default:
		return "", "", false, false
	}
}

// classifyRenditionError maps the OCR client's private failures onto the
// shared provider error classes. HTTP statuses use the shared status table
// as a synchronous result exchange; the OCR client already retried transient
// statuses, so exhaustion is reported as transient rather than ambiguous.
func classifyRenditionError(cause error) error {
	if providerError, ok := errors.AsType[*document.RenditionProviderError](cause); ok {
		return providerError
	}
	if _, ok := errors.AsType[*credentialError](cause); ok {
		return renditionProvider.Classified(document.RenditionErrorAuthentication,
			"Mistral credential is unavailable", cause)
	}
	if transient, ok := errors.AsType[*transientError](cause); ok && transient.status != 0 {
		code, message := renditionProvider.StatusClass(providerutil.StageResult, transient.status)
		return renditionProvider.Classified(code, message, cause)
	}
	if permanent, ok := errors.AsType[*permanentResponseError](cause); ok {
		code, message := renditionProvider.StatusClass(providerutil.StageResult, permanent.status)
		return renditionProvider.Classified(code, message, cause)
	}
	switch {
	case errors.Is(cause, ErrTransientResponse):
		return renditionProvider.Classified(document.RenditionErrorTransient,
			"Mistral request retries were exhausted", cause)
	case errors.Is(cause, ErrCapabilityContract):
		return renditionProvider.Classified(document.RenditionErrorPolicyRejected,
			"Mistral OCR capability changed", cause)
	case errors.Is(cause, ErrPermanentResponse):
		return renditionProvider.Classified(document.RenditionErrorPolicyRejected,
			"Mistral rejected the OCR request", cause)
	case errors.Is(cause, ErrResponseTooLarge):
		return renditionProvider.Malformed("Mistral OCR response exceeds policy", cause)
	default:
		return renditionProvider.Malformed("Mistral OCR response is malformed", cause)
	}
}

func renditionCandidate(format document.RenditionFormatCapability) (CandidateFormat, bool) {
	for _, candidate := range candidateFormats {
		if candidate.Family == format.MediaFamily && candidate.MediaType == format.MediaType {
			return candidate, true
		}
	}
	return CandidateFormat{}, false
}
