package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// RenditionProviderContractVersion identifies the provider boundary defined here.
	RenditionProviderContractVersion = 1

	maxRenditionFormats          = 64
	maxRenditionArtifactRoles    = 16
	maxRenditionArtifacts        = 64
	maxRenditionMediaTypeBytes   = 255
	maxRenditionSourceBytes      = int64(1 << 40)
	maxRenditionMarkdownBytes    = 64 << 20
	maxRenditionArtifactBytes    = 256 << 20
	maxRenditionTotalResultBytes = 512 << 20
	maxRenditionWarnings         = 64
	maxRenditionUsageValue       = int64(1 << 50)
	renditionTimestampForm       = "2006-01-02T15:04:05.000000000Z"
)

// RenditionProvider renders one authorized upload into provider-neutral evidence.
type RenditionProvider interface {
	Descriptor() RenditionDescriptor
	Render(ctx context.Context, upload AuthorizedUpload, authorization RenditionAuthorization) (RenditionResult, error)
}

// AuthorizedUpload is a read-once upload with immutable, authorization-bound metadata.
type AuthorizedUpload interface {
	io.ReadCloser
	Metadata() AuthorizedUploadMetadata
}

// RenditionTrustBoundary identifies where provider-controlled processing occurs.
type RenditionTrustBoundary string

const (
	// RenditionTrustLocalProcess keeps processing within a local child process.
	RenditionTrustLocalProcess RenditionTrustBoundary = "local_process"
	// RenditionTrustOperatorNetwork sends input to operator-controlled infrastructure.
	RenditionTrustOperatorNetwork RenditionTrustBoundary = "operator_network"
	// RenditionTrustHostedProvider sends input to a third-party provider.
	RenditionTrustHostedProvider RenditionTrustBoundary = "hosted_provider"
)

// RenditionInputKind identifies the exact representation accepted by a provider.
type RenditionInputKind string

const (
	// RenditionInputOriginalFile sends the exact source file.
	RenditionInputOriginalFile RenditionInputKind = "original_file"
	// RenditionInputDerivedUpload sends a separately authorized derived representation.
	RenditionInputDerivedUpload RenditionInputKind = "derived_upload"
)

// RenditionFormatCapability is one exact supported media-family, media-type, and input-kind tuple.
type RenditionFormatCapability struct {
	MediaFamily string             `json:"media_family"`
	MediaType   string             `json:"media_type"`
	InputKind   RenditionInputKind `json:"input_kind"`
}

// RenditionDescriptor is the canonical immutable identity of a provider contract.
type RenditionDescriptor struct {
	ID                string                      `json:"id"`
	ContractVersion   int                         `json:"contract_version"`
	PolicyFingerprint string                      `json:"policy_fingerprint"`
	TrustBoundary     RenditionTrustBoundary      `json:"trust_boundary"`
	SupportedFormats  []RenditionFormatCapability `json:"supported_formats"`
	ReturnsMarkdown   bool                        `json:"returns_markdown"`
	ReturnsStructured bool                        `json:"returns_structured"`
	ArtifactRoles     []EvidenceArtifactRole      `json:"artifact_roles"`
	Fingerprint       string                      `json:"fingerprint"`
}

type renditionDescriptorIdentity struct {
	ID                string                      `json:"id"`
	ContractVersion   int                         `json:"contract_version"`
	PolicyFingerprint string                      `json:"policy_fingerprint"`
	TrustBoundary     RenditionTrustBoundary      `json:"trust_boundary"`
	SupportedFormats  []RenditionFormatCapability `json:"supported_formats"`
	ReturnsMarkdown   bool                        `json:"returns_markdown"`
	ReturnsStructured bool                        `json:"returns_structured"`
	ArtifactRoles     []EvidenceArtifactRole      `json:"artifact_roles"`
}

// NewRenditionDescriptor validates, canonicalizes, and fingerprints a descriptor.
func NewRenditionDescriptor(value RenditionDescriptor) (RenditionDescriptor, error) {
	value.Fingerprint = ""
	value.SupportedFormats = slices.Clone(value.SupportedFormats)
	value.ArtifactRoles = slices.Clone(value.ArtifactRoles)
	slices.SortFunc(value.SupportedFormats, compareRenditionFormats)
	slices.Sort(value.ArtifactRoles)
	if err := validateRenditionDescriptorFields(value); err != nil {
		return RenditionDescriptor{}, err
	}
	encoded, err := canonicalJSON(descriptorIdentity(value))
	if err != nil {
		return RenditionDescriptor{}, fmt.Errorf("encode rendition descriptor: %w", err)
	}
	value.Fingerprint = sha256Hex(encoded)
	return value, nil
}

// AuthorizedUploadMetadata describes the exact bytes presented to a provider.
// Filename is empty when the authorization withholds filename disclosure.
type AuthorizedUploadMetadata struct {
	Filename                 string             `json:"filename"`
	MediaFamily              string             `json:"media_family"`
	MediaType                string             `json:"media_type"`
	ByteLength               int64              `json:"byte_length"`
	SHA256                   string             `json:"sha256"`
	CapabilityRecordChecksum string             `json:"capability_record_checksum"`
	ProviderMetadataChecksum string             `json:"provider_metadata_checksum"`
	InputKind                RenditionInputKind `json:"input_kind"`
}

// RenditionAuthorization binds one provider invocation to exact input and output limits.
type RenditionAuthorization struct {
	ProviderID                  string                 `json:"provider_id"`
	DescriptorFingerprint       string                 `json:"descriptor_fingerprint"`
	PolicyFingerprint           string                 `json:"policy_fingerprint"`
	RenditionRequestFingerprint string                 `json:"rendition_request_fingerprint"`
	SourceSHA256                string                 `json:"source_sha256"`
	SourceBytes                 int64                  `json:"source_bytes"`
	CapabilityRecordChecksum    string                 `json:"capability_record_checksum"`
	ProviderMetadataChecksum    string                 `json:"provider_metadata_checksum"`
	MediaFamily                 string                 `json:"media_family"`
	MediaType                   string                 `json:"media_type"`
	InputKind                   RenditionInputKind     `json:"input_kind"`
	DiscloseFilename            bool                   `json:"disclose_filename"`
	AllowedArtifactRoles        []EvidenceArtifactRole `json:"allowed_artifact_roles"`
	MaxProviderMarkdownBytes    int                    `json:"max_provider_markdown_bytes"`
	MaxArtifactBytes            int                    `json:"max_artifact_bytes"`
	MaxArtifacts                int                    `json:"max_artifacts"`
	MaxTotalResultBytes         int                    `json:"max_total_result_bytes"`
	AuthorizedAt                string                 `json:"authorized_at"`
	ExpiresAt                   string                 `json:"expires_at"`
}

// RenditionArtifact is one bounded, checksum-addressed provider output.
type RenditionArtifact struct {
	Role      EvidenceArtifactRole `json:"role"`
	MediaType string               `json:"media_type"`
	Payload   []byte               `json:"payload"`
	SHA256    string               `json:"sha256"`
}

// RenditionUsage contains bounded numeric provider accounting only.
type RenditionUsage struct {
	Requests    int64 `json:"requests"`
	Retries     int64 `json:"retries"`
	InputBytes  int64 `json:"input_bytes"`
	OutputBytes int64 `json:"output_bytes"`
	Units       int64 `json:"units"`
}

// RenditionReceipt is a sanitized, bounded execution record without provider bodies or secrets.
type RenditionReceipt struct {
	ProviderID                  string         `json:"provider_id"`
	DescriptorFingerprint       string         `json:"descriptor_fingerprint"`
	PolicyFingerprint           string         `json:"policy_fingerprint"`
	RenditionRequestFingerprint string         `json:"rendition_request_fingerprint"`
	SourceSHA256                string         `json:"source_sha256"`
	OperationID                 string         `json:"operation_id"`
	StartedAt                   string         `json:"started_at"`
	CompletedAt                 string         `json:"completed_at"`
	Warnings                    []string       `json:"warnings,omitempty"`
	Usage                       RenditionUsage `json:"usage"`
	RetryDelayMillis            int64          `json:"retry_delay_millis,omitempty"`
}

// RenditionResult contains bounded provider output and its sanitized receipt.
type RenditionResult struct {
	Evidence         SourceEvidenceV1    `json:"evidence"`
	ProviderMarkdown []byte              `json:"provider_markdown,omitempty"`
	Artifacts        []RenditionArtifact `json:"artifacts,omitempty"`
	Receipt          RenditionReceipt    `json:"receipt"`
}

// ValidateRenditionProviderRequest validates the provider snapshot and exact upload authorization.
func ValidateRenditionProviderRequest(
	provider RenditionProvider, upload AuthorizedUpload, authorization RenditionAuthorization,
) (RenditionDescriptor, error) {
	return ValidateRenditionProviderRequestAt(time.Now().UTC(), provider, upload, authorization)
}

// ValidateRenditionProviderRequestAt validates a request against an explicit
// trusted clock. Callers with a transaction clock can avoid time-of-check
// drift while retaining the same expiry boundary.
func ValidateRenditionProviderRequestAt(
	now time.Time, provider RenditionProvider, upload AuthorizedUpload,
	authorization RenditionAuthorization,
) (RenditionDescriptor, error) {
	descriptor, _, err := validateRenditionProviderRequestAt(now, provider, upload, authorization)
	return descriptor, err
}

func validateRenditionProviderRequestAt(
	now time.Time, provider RenditionProvider, upload AuthorizedUpload,
	authorization RenditionAuthorization,
) (RenditionDescriptor, AuthorizedUploadMetadata, error) {
	if nilInterface(provider) {
		return RenditionDescriptor{}, AuthorizedUploadMetadata{}, errors.New("rendition provider is required")
	}
	if nilInterface(upload) {
		return RenditionDescriptor{}, AuthorizedUploadMetadata{}, errors.New("authorized upload is required")
	}
	authorization = cloneRenditionAuthorization(authorization)
	descriptor := cloneRenditionDescriptor(provider.Descriptor())
	if err := validateRenditionDescriptor(descriptor); err != nil {
		return RenditionDescriptor{}, AuthorizedUploadMetadata{}, err
	}
	if second := cloneRenditionDescriptor(provider.Descriptor()); !equalRenditionDescriptors(descriptor, second) {
		return RenditionDescriptor{}, AuthorizedUploadMetadata{}, errors.New("rendition descriptor changed during validation")
	}
	metadata := upload.Metadata()
	if err := validateAuthorizedUploadMetadata(metadata); err != nil {
		return RenditionDescriptor{}, AuthorizedUploadMetadata{}, err
	}
	if second := upload.Metadata(); second != metadata {
		return RenditionDescriptor{}, AuthorizedUploadMetadata{}, errors.New("authorized upload metadata changed during validation")
	}
	if err := validateRenditionAuthorization(descriptor, metadata, authorization); err != nil {
		return RenditionDescriptor{}, AuthorizedUploadMetadata{}, err
	}
	if err := validateAuthorizationCurrentAt(authorization, now); err != nil {
		return RenditionDescriptor{}, AuthorizedUploadMetadata{}, err
	}
	return cloneRenditionDescriptor(descriptor), metadata, nil
}

// RenderRendition takes ownership of upload, validates immutable boundary
// snapshots, gives the provider a separate authorization copy, and validates
// its result against the sealed copy. Provider mutation therefore cannot
// broaden its own output authority. The upload is closed exactly once before
// return, including when validation or the provider fails.
func RenderRendition(
	ctx context.Context, provider RenditionProvider, upload AuthorizedUpload,
	authorization RenditionAuthorization,
) (result RenditionResult, err error) {
	if nilInterface(upload) {
		return RenditionResult{}, errors.New("authorized upload is required")
	}
	ownedUpload := &ownedAuthorizedUpload{upload: upload}
	defer func() {
		closeErr := ownedUpload.Close()
		if closeErr != nil && !errors.Is(err, closeErr) {
			err = errors.Join(err, fmt.Errorf("close authorized upload: %w", closeErr))
		}
	}()
	stopClose := context.AfterFunc(ctx, func() { _ = ownedUpload.Close() })
	defer stopClose()
	if err := ctx.Err(); err != nil {
		return RenditionResult{}, err
	}

	sealed := cloneRenditionAuthorization(authorization)
	descriptor, metadata, err := validateRenditionProviderRequestAt(time.Now().UTC(), provider, ownedUpload, sealed)
	if err != nil {
		return RenditionResult{}, err
	}
	expiresAt, err := parseRenditionTimestamp(sealed.ExpiresAt)
	if err != nil {
		return RenditionResult{}, errors.New("authorization expiry must be canonical")
	}
	executionCtx, cancelExecution := context.WithDeadline(ctx, expiresAt)
	defer cancelExecution()
	stopExecutionClose := context.AfterFunc(executionCtx, func() { _ = ownedUpload.Close() })
	defer stopExecutionClose()
	if !sealed.DiscloseFilename {
		metadata.Filename = ""
	}
	hasher := sha256.New()
	limited := &io.LimitedReader{R: ownedUpload, N: sealed.SourceBytes}
	providerUpload := &sealedAuthorizedUpload{
		ctx: executionCtx, source: ownedUpload, metadata: metadata, limited: limited,
		reader: io.TeeReader(limited, hasher), hasher: hasher, expectedSHA256: sealed.SourceSHA256,
	}
	if err := executionCtx.Err(); err != nil {
		return RenditionResult{}, err
	}
	result, err = provider.Render(executionCtx, providerUpload, cloneRenditionAuthorization(sealed))
	_ = providerUpload.Close()
	if contextErr := ctx.Err(); contextErr != nil {
		return RenditionResult{}, contextErr
	}
	if err := validateAuthorizationCurrentAt(sealed, time.Now().UTC()); err != nil {
		return RenditionResult{}, err
	}
	if contextErr := executionCtx.Err(); contextErr != nil {
		return RenditionResult{}, contextErr
	}
	if err != nil {
		if classified := ValidateRenditionProviderError(err); classified != nil {
			return RenditionResult{}, classified
		}
		return RenditionResult{}, err
	}
	if err := ValidateRenditionResult(descriptor, sealed, result); err != nil {
		return RenditionResult{}, err
	}
	result = cloneRenditionResult(result)
	if err := ValidateRenditionResult(descriptor, sealed, result); err != nil {
		return RenditionResult{}, err
	}
	if err := providerUpload.verify(executionCtx); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RenditionResult{}, contextErr
		}
		if currentErr := validateAuthorizationCurrentAt(sealed, time.Now().UTC()); currentErr != nil {
			return RenditionResult{}, currentErr
		}
		if contextErr := executionCtx.Err(); contextErr != nil {
			return RenditionResult{}, contextErr
		}
		return RenditionResult{}, err
	}
	if err := validateAuthorizationCurrentAt(sealed, time.Now().UTC()); err != nil {
		return RenditionResult{}, err
	}
	return result, nil
}

// ValidateRenditionResult rejects provider output outside the authorized contract.
func ValidateRenditionResult(
	descriptor RenditionDescriptor, authorization RenditionAuthorization, result RenditionResult,
) error {
	if err := validateRenditionDescriptor(descriptor); err != nil {
		return err
	}
	if err := validateAuthorizationWithoutUpload(descriptor, authorization); err != nil {
		return err
	}
	if err := ValidateSourceEvidenceV1(result.Evidence); err != nil {
		return fmt.Errorf("provider evidence: %w", err)
	}
	if result.Evidence.Family != authorization.MediaFamily {
		return errors.New("provider evidence family does not match authorization")
	}
	if !descriptor.ReturnsMarkdown && len(result.ProviderMarkdown) != 0 {
		return errors.New("provider Markdown is not declared by descriptor")
	}
	if len(result.ProviderMarkdown) > authorization.MaxProviderMarkdownBytes {
		return errors.New("provider Markdown exceeds authorized byte limit")
	}
	if err := validateRenditionArtifacts(descriptor, authorization, result.Artifacts); err != nil {
		return err
	}
	if err := validateEvidenceArtifactAuthorization(
		authorization, result.Evidence.Artifacts, result.Artifacts); err != nil {
		return err
	}
	if err := validateRenditionReceipt(descriptor, authorization, result.Receipt); err != nil {
		return err
	}
	encodedEvidence, err := canonicalJSON(result.Evidence)
	if err != nil {
		return fmt.Errorf("encode provider evidence: %w", err)
	}
	encodedReceipt, err := canonicalJSON(result.Receipt)
	if err != nil {
		return fmt.Errorf("encode provider receipt: %w", err)
	}
	total := int64(len(encodedEvidence)) + int64(len(encodedReceipt)) + int64(len(result.ProviderMarkdown))
	for _, artifact := range result.Artifacts {
		total += int64(len(artifact.Role) + len(artifact.MediaType) + len(artifact.SHA256) + len(artifact.Payload))
		if total > int64(authorization.MaxTotalResultBytes) {
			return errors.New("provider total result bytes exceed authorization")
		}
	}
	if total > int64(authorization.MaxTotalResultBytes) {
		return errors.New("provider total result bytes exceed authorization")
	}
	return nil
}

// RenditionErrorCode is a stable, bounded provider failure class.
type RenditionErrorCode string

const (
	RenditionErrorUnsupportedInput    RenditionErrorCode = "unsupported_input"
	RenditionErrorPolicyRejected      RenditionErrorCode = "policy_rejected"
	RenditionErrorAuthentication      RenditionErrorCode = "authentication"
	RenditionErrorCapacity            RenditionErrorCode = "capacity"
	RenditionErrorRateLimited         RenditionErrorCode = "rate_limited"
	RenditionErrorTransient           RenditionErrorCode = "transient"
	RenditionErrorMalformedEvidence   RenditionErrorCode = "malformed_evidence"
	RenditionErrorUnknownJob          RenditionErrorCode = "unknown_job"
	RenditionErrorCanceled            RenditionErrorCode = "canceled"
	RenditionErrorAmbiguousSubmission RenditionErrorCode = "ambiguous_submission"
)

// RenditionProviderError preserves a private cause behind a fixed error class.
type RenditionProviderError struct {
	code       RenditionErrorCode
	retryAfter time.Duration
	cause      error
}

// NewRenditionProviderError constructs a classified provider failure.
func NewRenditionProviderError(
	code RenditionErrorCode, retryAfter time.Duration, cause error,
) (*RenditionProviderError, error) {
	providerError := &RenditionProviderError{code: code, retryAfter: retryAfter, cause: cause}
	if err := validateClassifiedProviderError(providerError); err != nil {
		return nil, err
	}
	return providerError, nil
}

// Error returns only the fixed message for the failure class.
func (providerError *RenditionProviderError) Error() string {
	if providerError == nil {
		return "rendition provider error"
	}
	switch providerError.code {
	case RenditionErrorUnsupportedInput:
		return "rendition provider does not support the input"
	case RenditionErrorPolicyRejected:
		return "rendition provider rejected the request policy"
	case RenditionErrorAuthentication:
		return "rendition provider authentication failed"
	case RenditionErrorCapacity:
		return "rendition provider has insufficient capacity"
	case RenditionErrorRateLimited:
		return "rendition provider rate limit reached"
	case RenditionErrorTransient:
		return "rendition provider failed temporarily"
	case RenditionErrorMalformedEvidence:
		return "rendition provider returned malformed evidence"
	case RenditionErrorUnknownJob:
		return "rendition provider job is unknown"
	case RenditionErrorCanceled:
		return "rendition provider request was canceled"
	case RenditionErrorAmbiguousSubmission:
		return "rendition provider submission status is ambiguous"
	default:
		return "rendition provider error"
	}
}

// Unwrap exposes the private cause for programmatic matching without rendering it.
func (providerError *RenditionProviderError) Unwrap() error {
	if providerError == nil {
		return nil
	}
	return providerError.cause
}

// Code returns the stable provider failure class.
func (providerError *RenditionProviderError) Code() RenditionErrorCode {
	if providerError == nil {
		return ""
	}
	return providerError.code
}

// RetryAfter returns the bounded provider-supplied delay.
func (providerError *RenditionProviderError) RetryAfter() time.Duration {
	if providerError == nil {
		return 0
	}
	return providerError.retryAfter
}

// ValidateRenditionProviderError rejects raw, unclassified provider failures.
func ValidateRenditionProviderError(err error) error {
	providerError, ok := err.(*RenditionProviderError) //nolint:errorlint // wrappers may expose unsanitized text
	if !ok {
		return errors.New("unclassified rendition provider error")
	}
	return validateClassifiedProviderError(providerError)
}

// IsRenditionProviderErrorRetryable reports whether an explicitly classified failure is retryable.
func IsRenditionProviderErrorRetryable(err error) bool {
	providerError, ok := err.(*RenditionProviderError) //nolint:errorlint // only the top-level safe error is retryable
	if !ok || validateClassifiedProviderError(providerError) != nil {
		return false
	}
	switch providerError.code {
	case RenditionErrorCapacity, RenditionErrorRateLimited, RenditionErrorTransient:
		return true
	default:
		return false
	}
}

func validateRenditionDescriptor(descriptor RenditionDescriptor) error {
	if err := validateRenditionDescriptorFields(descriptor); err != nil {
		return err
	}
	if err := validateFingerprint(descriptor.Fingerprint, "descriptor fingerprint"); err != nil {
		return err
	}
	canonical, err := NewRenditionDescriptor(descriptor)
	if err != nil {
		return err
	}
	if canonical.Fingerprint != descriptor.Fingerprint || !equalRenditionDescriptors(canonical, descriptor) {
		return errors.New("descriptor fingerprint or canonical ordering is invalid")
	}
	return nil
}

func cloneRenditionAuthorization(value RenditionAuthorization) RenditionAuthorization {
	value.AllowedArtifactRoles = slices.Clone(value.AllowedArtifactRoles)
	return value
}

func cloneRenditionResult(value RenditionResult) RenditionResult {
	value.Evidence = cloneSourceEvidenceV1(value.Evidence)
	value.ProviderMarkdown = slices.Clone(value.ProviderMarkdown)
	value.Artifacts = slices.Clone(value.Artifacts)
	for index := range value.Artifacts {
		value.Artifacts[index].Payload = slices.Clone(value.Artifacts[index].Payload)
	}
	value.Receipt.Warnings = slices.Clone(value.Receipt.Warnings)
	return value
}

func cloneSourceEvidenceV1(value SourceEvidenceV1) SourceEvidenceV1 {
	value.Artifacts = slices.Clone(value.Artifacts)
	value.Omissions = cloneSourceEvidenceOmissions(value.Omissions)
	value.Units = slices.Clone(value.Units)
	for unitIndex := range value.Units {
		unit := &value.Units[unitIndex]
		if unit.Confidence != nil {
			confidence := *unit.Confidence
			unit.Confidence = &confidence
		}
		unit.HeadingPath = slices.Clone(unit.HeadingPath)
		unit.Omissions = cloneSourceEvidenceOmissions(unit.Omissions)
		unit.Regions = slices.Clone(unit.Regions)
		for regionIndex := range unit.Regions {
			region := &unit.Regions[regionIndex]
			if region.Confidence != nil {
				confidence := *region.Confidence
				region.Confidence = &confidence
			}
			if region.Geometry != nil {
				geometry := *region.Geometry
				geometry.Boxes = slices.Clone(geometry.Boxes)
				geometry.Polygons = cloneEvidencePolygons(geometry.Polygons)
				region.Geometry = &geometry
			}
		}
		unit.Tables = slices.Clone(unit.Tables)
		for tableIndex := range unit.Tables {
			unit.Tables[tableIndex].Cells = slices.Clone(unit.Tables[tableIndex].Cells)
		}
	}
	return value
}

func cloneSourceEvidenceOmissions(source []SourceEvidenceOmissionV1) []SourceEvidenceOmissionV1 {
	result := slices.Clone(source)
	for index := range result {
		if result[index].Locator != nil {
			locator := *result[index].Locator
			result[index].Locator = &locator
		}
		if result[index].Range != nil {
			textRange := *result[index].Range
			result[index].Range = &textRange
		}
	}
	return result
}

type sealedAuthorizedUpload struct {
	mu sync.Mutex

	ctx            context.Context
	source         *ownedAuthorizedUpload
	metadata       AuthorizedUploadMetadata
	reader         io.Reader
	limited        *io.LimitedReader
	hasher         hash.Hash
	expectedSHA256 string
	providerClosed bool
}

func (upload *sealedAuthorizedUpload) Metadata() AuthorizedUploadMetadata {
	return upload.metadata
}

func (upload *sealedAuthorizedUpload) Read(buffer []byte) (int, error) {
	upload.mu.Lock()
	defer upload.mu.Unlock()
	if err := upload.ctx.Err(); err != nil {
		return 0, err
	}
	if upload.providerClosed {
		return 0, io.ErrClosedPipe
	}
	return upload.reader.Read(buffer)
}

// Close ends provider access. RenderRendition retains the source until it has
// verified and closed the exact authorized stream.
func (upload *sealedAuthorizedUpload) Close() error {
	upload.mu.Lock()
	defer upload.mu.Unlock()
	upload.providerClosed = true
	return nil
}

func (upload *sealedAuthorizedUpload) verify(ctx context.Context) error {
	upload.mu.Lock()
	defer upload.mu.Unlock()
	upload.providerClosed = true
	buffer := make([]byte, 128<<10)
	noProgress := 0
	for upload.limited.N != 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("verify authorized upload: %w", err)
		}
		count, err := upload.reader.Read(buffer)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read authorized upload: %w", err)
		}
		if errors.Is(err, io.EOF) && upload.limited.N != 0 {
			return errors.New("authorized upload is shorter than declared byte length")
		}
		if count == 0 && err == nil {
			noProgress++
			if noProgress >= 100 {
				return io.ErrNoProgress
			}
		} else {
			noProgress = 0
		}
	}
	var extra [1]byte
	noProgress = 0
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("verify authorized upload: %w", err)
		}
		count, err := upload.source.Read(extra[:])
		if count != 0 {
			return errors.New("authorized upload exceeds declared byte length")
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read authorized upload: %w", err)
		}
		noProgress++
		if noProgress >= 100 {
			return io.ErrNoProgress
		}
	}
	if hex.EncodeToString(upload.hasher.Sum(nil)) != upload.expectedSHA256 {
		return errors.New("authorized upload SHA-256 does not match authorization")
	}
	return nil
}

type ownedAuthorizedUpload struct {
	upload AuthorizedUpload

	closeErr  error
	closeOnce sync.Once
}

func (upload *ownedAuthorizedUpload) Read(buffer []byte) (int, error) {
	return upload.upload.Read(buffer)
}

func (upload *ownedAuthorizedUpload) Metadata() AuthorizedUploadMetadata {
	return upload.upload.Metadata()
}

func (upload *ownedAuthorizedUpload) Close() error {
	upload.closeOnce.Do(func() {
		upload.closeErr = upload.upload.Close()
	})
	return upload.closeErr
}

func validateRenditionDescriptorFields(descriptor RenditionDescriptor) error {
	if err := validateStableToken(descriptor.ID, "descriptor ID", 128); err != nil {
		return err
	}
	if descriptor.ContractVersion != RenditionProviderContractVersion {
		return fmt.Errorf("descriptor contract version must be %d", RenditionProviderContractVersion)
	}
	if err := validateFingerprint(descriptor.PolicyFingerprint, "descriptor policy fingerprint"); err != nil {
		return err
	}
	switch descriptor.TrustBoundary {
	case RenditionTrustLocalProcess, RenditionTrustOperatorNetwork, RenditionTrustHostedProvider:
	default:
		return errors.New("descriptor trust boundary is invalid")
	}
	if len(descriptor.SupportedFormats) == 0 || len(descriptor.SupportedFormats) > maxRenditionFormats {
		return fmt.Errorf("descriptor supported formats must contain 1-%d entries", maxRenditionFormats)
	}
	seenFormats := make(map[RenditionFormatCapability]struct{}, len(descriptor.SupportedFormats))
	for _, format := range descriptor.SupportedFormats {
		if err := validateRenditionFormat(format); err != nil {
			return err
		}
		if _, exists := seenFormats[format]; exists {
			return errors.New("descriptor contains duplicate supported format")
		}
		seenFormats[format] = struct{}{}
	}
	if len(descriptor.ArtifactRoles) > maxRenditionArtifactRoles {
		return errors.New("descriptor has too many artifact roles")
	}
	seenRoles := make(map[EvidenceArtifactRole]struct{}, len(descriptor.ArtifactRoles))
	for _, role := range descriptor.ArtifactRoles {
		if !validProfileArtifactRole(role) {
			return fmt.Errorf("descriptor artifact role %q is invalid", role)
		}
		if _, exists := seenRoles[role]; exists {
			return fmt.Errorf("descriptor artifact role %q is duplicated", role)
		}
		seenRoles[role] = struct{}{}
	}
	if !descriptor.ReturnsStructured {
		return errors.New("descriptor must declare structured evidence")
	}
	return nil
}

func validateRenditionFormat(format RenditionFormatCapability) error {
	if err := validateStableToken(format.MediaFamily, "media family", 63); err != nil {
		return err
	}
	if err := validateCanonicalMediaType(format.MediaType); err != nil {
		return err
	}
	if !validRenditionInputKind(format.InputKind) {
		return errors.New("rendition input kind is invalid")
	}
	return nil
}

func validateAuthorizedUploadMetadata(metadata AuthorizedUploadMetadata) error {
	if len(metadata.Filename) > 255 || strings.ContainsAny(metadata.Filename, "/\\\x00") {
		return errors.New("upload filename must be a safe basename of at most 255 bytes")
	}
	if err := validateRenditionFormat(RenditionFormatCapability{
		MediaFamily: metadata.MediaFamily, MediaType: metadata.MediaType, InputKind: metadata.InputKind,
	}); err != nil {
		return fmt.Errorf("upload metadata: %w", err)
	}
	if metadata.ByteLength <= 0 || metadata.ByteLength > maxRenditionSourceBytes {
		return errors.New("upload byte length is outside the supported bound")
	}
	for subject, value := range map[string]string{
		"upload SHA-256": metadata.SHA256, "capability record checksum": metadata.CapabilityRecordChecksum,
		"provider metadata checksum": metadata.ProviderMetadataChecksum,
	} {
		if err := validateFingerprint(value, subject); err != nil {
			return err
		}
	}
	return nil
}

func validateRenditionAuthorization(
	descriptor RenditionDescriptor, metadata AuthorizedUploadMetadata, authorization RenditionAuthorization,
) error {
	if err := validateAuthorizationWithoutUpload(descriptor, authorization); err != nil {
		return err
	}
	if authorization.DiscloseFilename && metadata.Filename == "" {
		return errors.New("authorization requires filename disclosure but upload filename is empty")
	}
	if authorization.SourceSHA256 != metadata.SHA256 || authorization.SourceBytes != metadata.ByteLength {
		return errors.New("authorization does not match exact upload bytes")
	}
	if authorization.CapabilityRecordChecksum != metadata.CapabilityRecordChecksum {
		return errors.New("authorization capability record checksum does not match upload")
	}
	if authorization.ProviderMetadataChecksum != metadata.ProviderMetadataChecksum {
		return errors.New("authorization provider metadata checksum does not match upload")
	}
	if authorization.MediaFamily != metadata.MediaFamily || authorization.MediaType != metadata.MediaType ||
		authorization.InputKind != metadata.InputKind {
		return errors.New("authorization format does not match upload metadata")
	}
	return nil
}

func validateAuthorizationWithoutUpload(descriptor RenditionDescriptor, authorization RenditionAuthorization) error {
	if authorization.ProviderID != descriptor.ID {
		return errors.New("authorization provider ID does not match descriptor")
	}
	if authorization.DescriptorFingerprint != descriptor.Fingerprint {
		return errors.New("authorization descriptor fingerprint does not match descriptor")
	}
	if authorization.PolicyFingerprint != descriptor.PolicyFingerprint {
		return errors.New("authorization policy fingerprint does not match descriptor")
	}
	for subject, value := range map[string]string{
		"rendition request fingerprint": authorization.RenditionRequestFingerprint,
		"source SHA-256":                authorization.SourceSHA256, "capability record checksum": authorization.CapabilityRecordChecksum,
		"provider metadata checksum": authorization.ProviderMetadataChecksum,
	} {
		if err := validateFingerprint(value, subject); err != nil {
			return err
		}
	}
	format := RenditionFormatCapability{
		MediaFamily: authorization.MediaFamily, MediaType: authorization.MediaType, InputKind: authorization.InputKind,
	}
	if !slices.Contains(descriptor.SupportedFormats, format) {
		return errors.New("authorization requests an unsupported format")
	}
	if authorization.SourceBytes <= 0 || authorization.SourceBytes > maxRenditionSourceBytes {
		return errors.New("authorization source bytes are outside the supported bound")
	}
	if err := validateAuthorizedRoles(descriptor, authorization.AllowedArtifactRoles); err != nil {
		return err
	}
	if authorization.MaxProviderMarkdownBytes < 0 || authorization.MaxProviderMarkdownBytes > maxRenditionMarkdownBytes ||
		(!descriptor.ReturnsMarkdown && authorization.MaxProviderMarkdownBytes != 0) {
		return errors.New("authorization provider Markdown bytes are invalid")
	}
	if authorization.MaxArtifactBytes < 0 || authorization.MaxArtifactBytes > maxRenditionArtifactBytes {
		return errors.New("authorization artifact bytes are invalid")
	}
	if authorization.MaxArtifacts < 0 || authorization.MaxArtifacts > maxRenditionArtifacts ||
		authorization.MaxArtifacts > 0 && len(authorization.AllowedArtifactRoles) == 0 {
		return errors.New("authorization artifact count is invalid")
	}
	if authorization.MaxTotalResultBytes <= 0 || authorization.MaxTotalResultBytes > maxRenditionTotalResultBytes {
		return errors.New("authorization total result bytes are invalid")
	}
	authorizedAt, err := parseRenditionTimestamp(authorization.AuthorizedAt)
	if err != nil {
		return errors.New("authorization time must be canonical RFC3339Nano")
	}
	expiresAt, err := parseRenditionTimestamp(authorization.ExpiresAt)
	if err != nil || !expiresAt.After(authorizedAt) {
		return errors.New("authorization expiry must be canonical and after authorization time")
	}
	return nil
}

func validateAuthorizationCurrentAt(authorization RenditionAuthorization, now time.Time) error {
	authorizedAt, err := parseRenditionTimestamp(authorization.AuthorizedAt)
	if err != nil {
		return errors.New("authorization time must be canonical")
	}
	expiresAt, err := parseRenditionTimestamp(authorization.ExpiresAt)
	if err != nil {
		return errors.New("authorization expiry must be canonical")
	}
	if now.Before(authorizedAt) || !now.Before(expiresAt) {
		return errors.New("authorization is not current")
	}
	return nil
}

func validateAuthorizedRoles(descriptor RenditionDescriptor, roles []EvidenceArtifactRole) error {
	if len(roles) > maxRenditionArtifactRoles {
		return errors.New("authorization has too many artifact roles")
	}
	seen := make(map[EvidenceArtifactRole]struct{}, len(roles))
	for _, role := range roles {
		if !validProfileArtifactRole(role) || !slices.Contains(descriptor.ArtifactRoles, role) {
			return fmt.Errorf("authorization artifact role %q is not declared", role)
		}
		if _, exists := seen[role]; exists {
			return fmt.Errorf("authorization artifact role %q is duplicated", role)
		}
		seen[role] = struct{}{}
	}
	return nil
}

func validateRenditionArtifacts(
	descriptor RenditionDescriptor, authorization RenditionAuthorization, artifacts []RenditionArtifact,
) error {
	type identity struct {
		role   EvidenceArtifactRole
		sha256 string
	}
	seen := make(map[identity]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if !validProfileArtifactRole(artifact.Role) || !slices.Contains(descriptor.ArtifactRoles, artifact.Role) ||
			!slices.Contains(authorization.AllowedArtifactRoles, artifact.Role) {
			return fmt.Errorf("provider artifact role %q is not authorized", artifact.Role)
		}
		if err := validateCanonicalMediaType(artifact.MediaType); err != nil {
			return fmt.Errorf("provider artifact: %w", err)
		}
		if len(artifact.Payload) > authorization.MaxArtifactBytes {
			return errors.New("provider artifact exceeds authorized byte limit")
		}
		digest := sha256.Sum256(artifact.Payload)
		if artifact.SHA256 != hex.EncodeToString(digest[:]) {
			return errors.New("provider artifact checksum does not match payload")
		}
		key := identity{role: artifact.Role, sha256: artifact.SHA256}
		if _, exists := seen[key]; exists {
			return errors.New("provider artifact identity is duplicated")
		}
		seen[key] = struct{}{}
	}
	if len(artifacts) > authorization.MaxArtifacts {
		return errors.New("provider artifact count exceeds authorization")
	}
	return nil
}

func validateEvidenceArtifactAuthorization(
	authorization RenditionAuthorization, evidence []SourceEvidenceArtifactV1,
	retained []RenditionArtifact,
) error {
	type identity struct {
		role   EvidenceArtifactRole
		sha256 string
	}
	retainedSet := make(map[identity]struct{}, len(retained))
	for _, artifact := range retained {
		retainedSet[identity{role: artifact.Role, sha256: artifact.SHA256}] = struct{}{}
	}
	referenced := make(map[identity]struct{}, len(evidence))
	for _, artifact := range evidence {
		if !slices.Contains(authorization.AllowedArtifactRoles, artifact.Role) {
			return fmt.Errorf("source evidence artifact role %q is not authorized", artifact.Role)
		}
		key := identity{role: artifact.Role, sha256: artifact.SHA256}
		if _, ok := retainedSet[key]; !ok {
			return errors.New("source evidence artifact does not match a retained provider artifact")
		}
		referenced[key] = struct{}{}
	}
	for key := range retainedSet {
		if _, ok := referenced[key]; !ok {
			return errors.New("retained provider artifact is absent from source evidence")
		}
	}
	return nil
}

func validateRenditionReceipt(
	descriptor RenditionDescriptor, authorization RenditionAuthorization, receipt RenditionReceipt,
) error {
	if receipt.ProviderID != descriptor.ID || receipt.DescriptorFingerprint != descriptor.Fingerprint ||
		receipt.PolicyFingerprint != authorization.PolicyFingerprint ||
		receipt.RenditionRequestFingerprint != authorization.RenditionRequestFingerprint ||
		receipt.SourceSHA256 != authorization.SourceSHA256 {
		return errors.New("provider receipt does not match authorization")
	}
	if err := validateStableToken(receipt.OperationID, "operation ID", 128); err != nil {
		return err
	}
	startedAt, err := parseRenditionTimestamp(receipt.StartedAt)
	if err != nil {
		return errors.New("receipt start time must be canonical RFC3339Nano")
	}
	completedAt, err := parseRenditionTimestamp(receipt.CompletedAt)
	if err != nil || completedAt.Before(startedAt) {
		return errors.New("receipt completion time must be canonical and not precede start")
	}
	authorizedAt, _ := parseRenditionTimestamp(authorization.AuthorizedAt)
	expiresAt, _ := parseRenditionTimestamp(authorization.ExpiresAt)
	if startedAt.Before(authorizedAt) || completedAt.After(expiresAt) {
		return errors.New("receipt execution is outside the authorization interval")
	}
	if len(receipt.Warnings) > maxRenditionWarnings {
		return errors.New("receipt has too many warning codes")
	}
	seenWarnings := make(map[string]struct{}, len(receipt.Warnings))
	for _, warning := range receipt.Warnings {
		if err := validateStableToken(warning, "receipt warning", 63); err != nil {
			return err
		}
		if _, exists := seenWarnings[warning]; exists {
			return errors.New("receipt warning codes must be unique")
		}
		seenWarnings[warning] = struct{}{}
	}
	if err := validateRenditionUsage(receipt.Usage); err != nil {
		return err
	}
	if receipt.Usage.Retries > receipt.Usage.Requests {
		return errors.New("receipt retries cannot exceed requests")
	}
	if receipt.RetryDelayMillis < 0 || receipt.RetryDelayMillis > int64((24*time.Hour)/time.Millisecond) {
		return errors.New("receipt retry delay is outside the supported bound")
	}
	return nil
}

func validateRenditionUsage(usage RenditionUsage) error {
	for subject, value := range map[string]int64{
		"requests": usage.Requests, "retries": usage.Retries, "input bytes": usage.InputBytes,
		"output bytes": usage.OutputBytes, "units": usage.Units,
	} {
		if value < 0 || value > maxRenditionUsageValue {
			return fmt.Errorf("receipt usage %s is outside the supported bound", subject)
		}
	}
	return nil
}

func validateClassifiedProviderError(providerError *RenditionProviderError) error {
	if providerError == nil {
		return errors.New("classified rendition provider error is nil")
	}
	switch providerError.code {
	case RenditionErrorUnsupportedInput, RenditionErrorPolicyRejected, RenditionErrorAuthentication,
		RenditionErrorCapacity, RenditionErrorRateLimited, RenditionErrorTransient,
		RenditionErrorMalformedEvidence, RenditionErrorUnknownJob, RenditionErrorCanceled,
		RenditionErrorAmbiguousSubmission:
	default:
		return errors.New("rendition provider error code is invalid")
	}
	if providerError.retryAfter < 0 || providerError.retryAfter > 24*time.Hour {
		return errors.New("rendition provider retry delay is outside the supported bound")
	}
	return nil
}

func validateStableToken(value, subject string, maxLength int) error {
	if value == "" || len(value) > maxLength {
		return fmt.Errorf("%s must contain 1-%d characters", subject, maxLength)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.' {
			continue
		}
		return fmt.Errorf("%s contains unsupported characters", subject)
	}
	return nil
}

func validateCanonicalMediaType(value string) error {
	if len(value) == 0 || len(value) > maxRenditionMediaTypeBytes {
		return fmt.Errorf("media type must contain 1-%d bytes", maxRenditionMediaTypeBytes)
	}
	parsed, parameters, err := mime.ParseMediaType(value)
	if err != nil || parsed != value || len(parameters) != 0 || !strings.Contains(value, "/") {
		return fmt.Errorf("media type %q must be canonical and contain no parameters", value)
	}
	return nil
}

func validRenditionInputKind(kind RenditionInputKind) bool {
	return kind == RenditionInputOriginalFile || kind == RenditionInputDerivedUpload
}

func parseRenditionTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(renditionTimestampForm, value)
	if err != nil || parsed.Format(renditionTimestampForm) != value {
		return time.Time{}, errors.New("timestamp is not canonical UTC RFC3339Nano")
	}
	return parsed, nil
}

func compareRenditionFormats(left, right RenditionFormatCapability) int {
	if comparison := strings.Compare(left.MediaFamily, right.MediaFamily); comparison != 0 {
		return comparison
	}
	if comparison := strings.Compare(left.MediaType, right.MediaType); comparison != 0 {
		return comparison
	}
	return strings.Compare(string(left.InputKind), string(right.InputKind))
}

func descriptorIdentity(value RenditionDescriptor) renditionDescriptorIdentity {
	return renditionDescriptorIdentity{
		ID: value.ID, ContractVersion: value.ContractVersion, PolicyFingerprint: value.PolicyFingerprint,
		TrustBoundary: value.TrustBoundary, SupportedFormats: value.SupportedFormats,
		ReturnsMarkdown: value.ReturnsMarkdown, ReturnsStructured: value.ReturnsStructured,
		ArtifactRoles: value.ArtifactRoles,
	}
}

func cloneRenditionDescriptor(value RenditionDescriptor) RenditionDescriptor {
	value.SupportedFormats = slices.Clone(value.SupportedFormats)
	value.ArtifactRoles = slices.Clone(value.ArtifactRoles)
	return value
}

func equalRenditionDescriptors(left, right RenditionDescriptor) bool {
	return left.ID == right.ID && left.ContractVersion == right.ContractVersion &&
		left.PolicyFingerprint == right.PolicyFingerprint && left.TrustBoundary == right.TrustBoundary &&
		left.ReturnsMarkdown == right.ReturnsMarkdown && left.ReturnsStructured == right.ReturnsStructured &&
		left.Fingerprint == right.Fingerprint && slices.Equal(left.SupportedFormats, right.SupportedFormats) &&
		slices.Equal(left.ArtifactRoles, right.ArtifactRoles)
}

func nilInterface(value any) bool {
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
