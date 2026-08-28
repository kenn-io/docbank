package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	renditionProviderID = "mistral.ocr-v1"
	renditionTimeForm   = "2006-01-02T15:04:05.000000000Z"
)

var _ document.RenditionProvider = (*RenditionClient)(nil)

// SecretResolver resolves only the profile-bound Mistral OCR credential.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, name string) (string, error)
}

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
	descriptor, err := document.NewRenditionDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		if err == nil {
			err = errors.New("descriptor is not canonical")
		}
		return nil, fmt.Errorf("mistral rendition descriptor: %w", err)
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
	if profile.SecretBinding == "" || nilValue(secrets) {
		return nil, errors.New("mistral rendition requires a named secret binding and resolver")
	}
	if err := validateRenditionToken(profile.SecretBinding, "secret binding"); err != nil {
		return nil, err
	}
	if httpClient == nil {
		return nil, errors.New("mistral rendition HTTP client is required")
	}
	if profile.Timeout == 0 {
		profile.Timeout = DefaultTimeout
	}
	if profile.MaxRetries == 0 {
		profile.MaxRetries = DefaultMaxRetries
	}
	if profile.MaxRetryDelay == 0 {
		profile.MaxRetryDelay = DefaultMaxRetryDelay
	}
	if profile.Timeout < 0 || profile.Timeout > MaxTimeout ||
		profile.MaxRetries < 0 || profile.MaxRetries > MaxRetries ||
		profile.MaxRetryDelay < 0 || profile.MaxRetryDelay > MaxRetryDelay {
		return nil, errors.New("mistral rendition execution bounds are invalid")
	}
	isolate := *httpClient
	isolate.Jar = nil
	isolate.CheckRedirect = providerhttp.RefuseRedirects
	if isolate.Timeout <= 0 || isolate.Timeout > profile.Timeout {
		isolate.Timeout = profile.Timeout
	}
	ocr, err := newClientWithCredential(profile.Policy, ClientConfig{
		Timeout: profile.Timeout, MaxRetries: profile.MaxRetries,
		MaxRetryDelay: profile.MaxRetryDelay, HTTPClient: &isolate,
	}, func(ctx context.Context) (string, error) {
		return secrets.ResolveSecret(ctx, profile.SecretBinding)
	})
	if err != nil {
		return nil, fmt.Errorf("mistral rendition client: %w", err)
	}
	return &RenditionClient{
		descriptor: cloneRenditionDescriptor(descriptor), policy: profile.Policy, manifest: manifest,
		ocr: ocr,
	}, nil
}

// Descriptor returns an immutable copy of the configured provider identity.
func (client *RenditionClient) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return cloneRenditionDescriptor(client.descriptor)
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
	if _, err := document.ValidateRenditionProviderRequest(client, upload, authorization); err != nil {
		return document.RenditionResult{}, err
	}
	metadata := upload.Metadata()
	expiresAt, err := time.Parse(renditionTimeForm, authorization.ExpiresAt)
	if err != nil {
		return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
			"Mistral authorization expiry is invalid", err)
	}
	startedAt := time.Now().UTC()
	if !startedAt.Before(expiresAt) {
		return document.RenditionResult{}, expiredRenditionError()
	}
	operationCtx, cancel := context.WithDeadline(ctx, expiresAt)
	defer cancel()
	source, err := readRenditionUpload(operationCtx, upload, metadata, client.policy.values.MaxDocumentBytes)
	if err != nil {
		if authorizationDeadlineExpired(ctx, operationCtx) {
			return document.RenditionResult{}, expiredRenditionError()
		}
		return document.RenditionResult{}, err
	}
	defer clear(source)
	candidate, err := formatdetect.DetectFormat(bytes.NewReader(source), int64(len(source)), metadata.MediaType)
	if err != nil {
		return document.RenditionResult{}, renditionError(document.RenditionErrorUnsupportedInput,
			"Mistral input format could not be verified", err)
	}
	if candidate.Family != metadata.MediaFamily || candidate.MediaType != metadata.MediaType {
		return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
			"Mistral input identity does not match authorization", nil)
	}
	localUnits := int64(0)
	if candidate.ID == formatIDPDF {
		localUnits, err = formatdetect.CountPDFPages(source)
		if err != nil {
			return document.RenditionResult{}, renditionError(document.RenditionErrorUnsupportedInput,
				"Mistral PDF page count could not be verified", err)
		}
		if localUnits <= 0 || localUnits > int64(client.policy.values.MaxUnits) {
			return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
				"Mistral PDF exceeds the complete unit limit", nil)
		}
	}
	formatAuthorization, err := client.policy.Authorize(client.manifest, candidate.ID)
	if err != nil {
		return document.RenditionResult{}, renditionError(document.RenditionErrorUnsupportedInput,
			"Mistral input has no enforceable upload authority", err)
	}
	snapshot := preparedSnapshot{
		size: int64(len(source)), sha256: metadata.SHA256, format: candidate,
		mediaType: metadata.MediaType,
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
	checkExpiry := func() error {
		if !time.Now().UTC().Before(expiresAt) {
			return expiredRenditionError()
		}
		return nil
	}
	options := probeRequestOptions(candidate, client.policy.values.MaxUnits,
		client.policy.values.ExtractHeader, client.policy.values.ExtractFooter)
	providerResult, err := client.ocr.processWith(
		operationCtx, snapshotForAttempt, readDocument, checkExpiry, options,
		formatAuthorization.method, client.policy.values.MaxUnits,
	)
	if err != nil {
		if authorizationDeadlineExpired(ctx, operationCtx) {
			return document.RenditionResult{}, expiredRenditionError()
		}
		return document.RenditionResult{}, classifyRenditionError(ctx, err)
	}
	if candidate.ID == formatIDPDF && int64(providerResult.UnitsProcessed) != localUnits {
		return document.RenditionResult{}, renditionError(document.RenditionErrorPolicyRejected,
			"Mistral OCR page count changed", ErrCapabilityContract)
	}
	completedAt := time.Now().UTC()
	if !completedAt.Before(expiresAt) {
		return document.RenditionResult{}, expiredRenditionError()
	}
	evidence, markdown, err := mistralEvidence(providerResult.Document)
	if err != nil {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"Mistral OCR output is malformed", err)
	}
	if len(markdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, renditionError(document.RenditionErrorMalformedEvidence,
			"Mistral OCR Markdown exceeds authorization", nil)
	}
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, fmt.Errorf("mistral: fingerprint authorization: %w", err)
	}
	return document.RenditionResult{
		Evidence: evidence, ProviderMarkdown: markdown,
		Receipt: document.RenditionReceipt{
			ProviderID: client.descriptor.ID, DescriptorFingerprint: client.descriptor.Fingerprint,
			PolicyFingerprint:           authorization.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint, SourceSHA256: metadata.SHA256,
			OperationID: "mistral-" + authorization.RenditionRequestFingerprint[:24],
			StartedAt:   startedAt.Format(renditionTimeForm), CompletedAt: completedAt.Format(renditionTimeForm),
			Usage: document.RenditionUsage{
				Requests: int64(providerResult.Metrics.Requests), Retries: int64(providerResult.Metrics.Retries),
				InputBytes: metadata.ByteLength, OutputBytes: providerResult.ResponseBytes,
				Units: int64(providerResult.UnitsProcessed),
			},
		},
	}, nil
}

func readRenditionUpload(
	ctx context.Context, upload io.Reader, metadata document.AuthorizedUploadMetadata, maximum int64,
) ([]byte, error) {
	if metadata.ByteLength > maximum {
		return nil, renditionError(document.RenditionErrorPolicyRejected,
			"Mistral input exceeds the policy byte limit", nil)
	}
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: upload}, maximum+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, renditionError(document.RenditionErrorCanceled, "Mistral rendering canceled", ctxErr)
		}
		return nil, renditionError(document.RenditionErrorTransient,
			"Mistral input could not be read", err)
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != metadata.ByteLength || int64(len(data)) > maximum ||
		hex.EncodeToString(digest[:]) != metadata.SHA256 {
		clear(data)
		return nil, renditionError(document.RenditionErrorPolicyRejected,
			"Mistral input identity does not match authorization", nil)
	}
	return data, nil
}

func mistralEvidence(source document.SourceDocument) (document.SourceEvidenceV1, []byte, error) {
	unitKind, locatorKind, indexed, ok := renditionUnitKinds(source.UnitKind)
	if !ok || len(source.Units) == 0 {
		return document.SourceEvidenceV1{}, nil, errors.New("unsupported or empty Mistral OCR unit sequence")
	}
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: source.Family, UnitKind: unitKind,
		Units: make([]document.SourceEvidenceUnitV1, 0, len(source.Units)),
	}
	markdownPages := make([]string, 0, len(source.Units))
	for order, sourceUnit := range source.Units {
		if sourceUnit.Index != order || !utf8.ValidString(sourceUnit.Markdown) ||
			!utf8.ValidString(sourceUnit.Header) || !utf8.ValidString(sourceUnit.Footer) {
			return document.SourceEvidenceV1{}, nil, errors.New("invalid Mistral OCR unit")
		}
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
		markdownPages = append(markdownPages, sourceUnit.Markdown)
	}
	markdown := []byte(strings.Join(markdownPages, "\n\n---\n\n"))
	if len(markdown) == 0 {
		return document.SourceEvidenceV1{}, nil, errors.New("mistral OCR returned empty Markdown")
	}
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		return document.SourceEvidenceV1{}, nil, err
	}
	return evidence, markdown, nil
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

func classifyRenditionError(ctx context.Context, cause error) error {
	if providerError, ok := errors.AsType[*document.RenditionProviderError](cause); ok {
		return providerError
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return renditionError(document.RenditionErrorCanceled, "Mistral rendering canceled", ctxErr)
	}
	if _, ok := errors.AsType[*credentialError](cause); ok {
		return renditionError(document.RenditionErrorAuthentication,
			"Mistral credential is unavailable", cause)
	}
	if transient, ok := errors.AsType[*transientError](cause); ok && transient.status == http.StatusTooManyRequests {
		return renditionError(document.RenditionErrorRateLimited, "Mistral rate limit was exhausted", cause)
	}
	if transient, ok := errors.AsType[*transientError](cause); ok &&
		(transient.status == http.StatusServiceUnavailable || transient.status == http.StatusInsufficientStorage) {
		return renditionError(document.RenditionErrorCapacity, "Mistral capacity is unavailable", cause)
	}
	if permanent, ok := errors.AsType[*permanentResponseError](cause); ok {
		switch permanent.status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return renditionError(document.RenditionErrorAuthentication, "Mistral authentication was rejected", cause)
		case http.StatusUnsupportedMediaType:
			return renditionError(document.RenditionErrorUnsupportedInput, "Mistral input format was rejected", cause)
		default:
			return renditionError(document.RenditionErrorPolicyRejected, "Mistral rejected the OCR request", cause)
		}
	}
	switch {
	case errors.Is(cause, ErrTransientResponse):
		return renditionError(document.RenditionErrorTransient, "Mistral request retries were exhausted", cause)
	case errors.Is(cause, ErrCapabilityContract):
		return renditionError(document.RenditionErrorPolicyRejected, "Mistral OCR capability changed", cause)
	case errors.Is(cause, ErrPermanentResponse):
		return renditionError(document.RenditionErrorPolicyRejected, "Mistral rejected the OCR request", cause)
	case errors.Is(cause, ErrResponseTooLarge):
		return renditionError(document.RenditionErrorMalformedEvidence, "Mistral OCR response exceeds policy", cause)
	default:
		return renditionError(document.RenditionErrorMalformedEvidence, "Mistral OCR response is malformed", cause)
	}
}

func expiredRenditionError() error {
	return renditionError(document.RenditionErrorPolicyRejected, "Mistral authorization expired", nil)
}

func authorizationDeadlineExpired(callerCtx, operationCtx context.Context) bool {
	return callerCtx.Err() == nil && errors.Is(operationCtx.Err(), context.DeadlineExceeded)
}

func renditionError(code document.RenditionErrorCode, message string, cause error) error {
	providerError, err := document.NewRenditionProviderError(code, message, 0, cause)
	if err != nil {
		return fmt.Errorf("mistral rendition error classification: %w", err)
	}
	return providerError
}

func renditionCandidate(format document.RenditionFormatCapability) (CandidateFormat, bool) {
	for _, candidate := range candidateFormats {
		if candidate.Family == format.MediaFamily && candidate.MediaType == format.MediaType {
			return candidate, true
		}
	}
	return CandidateFormat{}, false
}

func cloneRenditionDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = slices.Clone(value.SupportedFormats)
	value.ArtifactRoles = slices.Clone(value.ArtifactRoles)
	return value
}

func validateRenditionToken(value, subject string) error {
	if value == "" || len(value) > 128 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return fmt.Errorf("mistral rendition %s must contain 1-128 characters", subject)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("_.-", char) {
			continue
		}
		return fmt.Errorf("mistral rendition %s contains unsupported characters", subject)
	}
	return nil
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
