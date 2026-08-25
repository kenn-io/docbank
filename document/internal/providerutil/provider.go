// Package providerutil contains shared implementation details for rendition
// provider adapters.
package providerutil

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
)

const (
	// TimestampForm is the canonical RFC 3339 nanosecond form used by every
	// authorization and receipt timestamp.
	TimestampForm = "2006-01-02T15:04:05.000000000Z"

	maxConsecutiveEmptyReads = 100
)

// ValidOptionalFilename accepts an undisclosed filename or one safe basename
// whose case-insensitive extension is in allowedExtensions.
func ValidOptionalFilename(filename string, allowedExtensions ...string) bool {
	if filename == "" {
		return true
	}
	if filename != strings.TrimSpace(filename) || strings.ContainsAny(filename, "/\\:\x00") {
		return false
	}
	return slices.Contains(allowedExtensions, strings.ToLower(filepath.Ext(filename)))
}

// Provider is one adapter's display name. It prefixes every classified
// error cause the adapter produces and, lowercased, every constructor error.
type Provider string

func (provider Provider) prefix() string { return strings.ToLower(string(provider)) }

// Classified constructs a provider error with a stable public class.
func (provider Provider) Classified(code document.RenditionErrorCode, message string, cause error) error {
	return ClassifiedError(string(provider), code, message, 0, cause)
}

// Malformed classifies a response Docbank cannot trust as evidence.
func (provider Provider) Malformed(message string, cause error) error {
	return provider.Classified(document.RenditionErrorMalformedEvidence, message, cause)
}

// Canceled classifies a rendering interrupted by the caller.
func (provider Provider) Canceled(cause error) error {
	return provider.Classified(document.RenditionErrorCanceled, string(provider)+" rendering canceled", cause)
}

// Expired classifies an authorization whose expiry passed during rendering.
func (provider Provider) Expired() error {
	return provider.Classified(document.RenditionErrorPolicyRejected, string(provider)+" authorization expired", nil)
}

// AmbiguousSubmission wraps a failure after which the provider may or may not
// have accepted the job.
func (provider Provider) AmbiguousSubmission(cause error) error {
	return provider.Classified(document.RenditionErrorAmbiguousSubmission,
		string(provider)+" submission outcome is unknown", cause)
}

// AmbiguousJob wraps a failure after which a known job may still complete.
func (provider Provider) AmbiguousJob(cause error) error {
	return provider.Classified(document.RenditionErrorAmbiguousSubmission,
		string(provider)+" job outcome is unknown", cause)
}

// KnownJobError converts a retryable failure into an ambiguous outcome once a
// provider job exists: resubmitting would duplicate work that may still finish.
func (provider Provider) KnownJobError(err error) error {
	if document.IsRenditionProviderErrorRetryable(err) {
		return provider.AmbiguousJob(err)
	}
	return err
}

// Bounded replaces a zero value with fallback and reports whether the result
// lies within (0, maximum].
func Bounded[T cmp.Ordered](value *T, fallback, maximum T) bool {
	var zero T
	if *value == zero {
		*value = fallback
	}
	return *value > zero && *value <= maximum
}

// IsNil reports whether value is nil, including a typed nil behind an interface.
func IsNil(value any) bool {
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

// CloneDescriptor returns a descriptor whose slices do not alias the input.
func CloneDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = slices.Clone(value.SupportedFormats)
	value.ArtifactRoles = slices.Clone(value.ArtifactRoles)
	return value
}

// CanonicalDescriptor validates that a profile descriptor is exactly its
// canonical form and returns an unaliased copy.
func (provider Provider) CanonicalDescriptor(
	descriptor document.RenditionDescriptor,
) (document.RenditionDescriptor, error) {
	canonical, err := document.NewRenditionDescriptor(descriptor)
	if err != nil || !reflect.DeepEqual(canonical, descriptor) {
		if err == nil {
			err = errors.New("descriptor is not canonical")
		}
		return document.RenditionDescriptor{}, fmt.Errorf("%s: invalid descriptor: %w", provider.prefix(), err)
	}
	return CloneDescriptor(canonical), nil
}

// AllowsArtifact reports whether an authorization can retain at least one artifact with role.
func AllowsArtifact(
	authorization document.RenditionAuthorization, role document.EvidenceArtifactRole,
) bool {
	return slices.Contains(authorization.AllowedArtifactRoles, role) &&
		authorization.MaxArtifacts > 0 && authorization.MaxArtifactBytes > 0
}

// AllowsStructured reports whether an authorization can retain one structured artifact.
func AllowsStructured(authorization document.RenditionAuthorization) bool {
	return AllowsArtifact(authorization, document.EvidenceArtifactStructured)
}

// NaturalUnit maps a media family to the evidence unit its provider output
// proves natively: pages for paged families and slides for presentations.
func NaturalUnit(family string) (document.EvidenceUnitKind, document.EvidenceLocatorKind, bool) {
	switch family {
	case "pdf", "image", "word":
		return document.EvidenceUnitPage, document.EvidenceLocatorPage, true
	case "presentation":
		return document.EvidenceUnitSlide, document.EvidenceLocatorSlide, true
	default:
		return "", "", false
	}
}

// DegradedEvidence builds one generic Markdown evidence unit with an explicit
// natural-provenance omission.
func DegradedEvidence(family, markdown, reason string) document.SourceEvidenceV1 {
	return document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceDegradedProvenance,
		Family:          family,
		UnitKind:        document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: reason,
		}},
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, Text: markdown,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
			},
		}},
	}
}

// InjectsDocbankFrontmatter reports whether provider Markdown claims Docbank's
// reserved sanitized-Markdown marker.
func InjectsDocbankFrontmatter(markdown []byte) bool {
	frontmatter := bytes.HasPrefix(markdown, []byte("---\n")) ||
		bytes.HasPrefix(markdown, []byte("---\r\n")) ||
		bytes.HasPrefix(markdown, []byte("---\r"))
	return frontmatter && bytes.Contains(markdown, []byte("docbank-sanitized-markdown/v1"))
}

// ReadAuthorizedUpload reads and verifies the exact authorized bytes. The
// caller owns interrupting upload when ctx ends so blocked reads can stop.
func ReadAuthorizedUpload(
	ctx context.Context, upload io.Reader, metadata document.AuthorizedUploadMetadata, providerName string,
) ([]byte, error) {
	provider := Provider(providerName)
	limited := &io.LimitedReader{R: upload, N: metadata.ByteLength + 1}
	data := make([]byte, 0, min(metadata.ByteLength, 32<<10))
	buffer := make([]byte, 32<<10)
	emptyReads := 0
	for limited.N > 0 {
		if err := ctx.Err(); err != nil {
			return nil, provider.Canceled(err)
		}
		count, err := limited.Read(buffer)
		if count > 0 {
			data = append(data, buffer[:count]...)
			emptyReads = 0
		}
		switch {
		case errors.Is(err, io.EOF):
			limited.N = 0
		case err != nil:
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, provider.Canceled(contextErr)
			}
			return nil, provider.Classified(document.RenditionErrorTransient,
				"could not read the authorized upload", err)
		case count == 0:
			emptyReads++
			if emptyReads >= maxConsecutiveEmptyReads {
				return nil, provider.Classified(document.RenditionErrorTransient,
					"authorized upload stopped making progress", io.ErrNoProgress)
			}
		}
	}
	if int64(len(data)) != metadata.ByteLength || SHA256Hex(data) != metadata.SHA256 {
		return nil, provider.Classified(document.RenditionErrorPolicyRejected,
			"authorized upload identity mismatch", nil)
	}
	return data, nil
}

// Wait blocks for delay or returns the context error.
func Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SHA256Hex returns the lowercase hexadecimal SHA-256 digest of value.
func SHA256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// ClassifiedError constructs a provider error with a stable public class, an
// optional provider retry hint, and a provider-specific private cause.
func ClassifiedError(
	providerName string, code document.RenditionErrorCode, message string, retryAfter time.Duration, cause error,
) error {
	if cause == nil {
		cause = errors.New(message)
	} else {
		cause = fmt.Errorf("%s: %w", message, cause)
	}
	providerError, err := document.NewRenditionProviderError(code, retryAfter, cause)
	if err == nil {
		return providerError
	}
	fallback, fallbackErr := document.NewRenditionProviderError(document.RenditionErrorMalformedEvidence,
		0, fmt.Errorf("%s returned an invalid error: %w", providerName, err))
	if fallbackErr == nil {
		return fallback
	}
	return errors.Join(err, fallbackErr)
}

// Usage accumulates request accounting for one rendering.
type Usage struct {
	Requests    int64
	Retries     int64
	OutputBytes int64
}

// Rendition returns the receipt usage for the accumulated requests.
func (usage Usage) Rendition(inputBytes, units int64) document.RenditionUsage {
	return document.RenditionUsage{
		Requests: usage.Requests, Retries: usage.Retries, InputBytes: inputBytes,
		OutputBytes: usage.OutputBytes, Units: units,
	}
}

// Receipt collects the inputs of one rendition receipt.
type Receipt struct {
	Descriptor    document.RenditionDescriptor
	Authorization document.RenditionAuthorization
	SourceSHA256  string
	OperationID   string
	StartedAt     time.Time
	CompletedAt   time.Time
	Warnings      []string
	Usage         document.RenditionUsage
	RetryDelay    time.Duration
}

// NewReceipt binds a completed rendering to its descriptor and authorization.
func NewReceipt(provider Provider, input Receipt) (document.RenditionReceipt, error) {
	authorizationFingerprint, err := input.Authorization.Fingerprint()
	if err != nil {
		return document.RenditionReceipt{}, provider.Classified(document.RenditionErrorPolicyRejected,
			string(provider)+" authorization fingerprint is invalid", err)
	}
	return document.RenditionReceipt{
		ProviderID: input.Descriptor.ID, DescriptorFingerprint: input.Descriptor.Fingerprint,
		PolicyFingerprint:           input.Authorization.PolicyFingerprint,
		RenditionRequestFingerprint: input.Authorization.RenditionRequestFingerprint,
		AuthorizationFingerprint:    authorizationFingerprint,
		SourceSHA256:                input.SourceSHA256,
		OperationID:                 input.OperationID,
		StartedAt:                   input.StartedAt.UTC().Format(TimestampForm),
		CompletedAt:                 input.CompletedAt.UTC().Format(TimestampForm),
		Warnings:                    input.Warnings,
		Usage:                       input.Usage,
		RetryDelayMillis:            input.RetryDelay.Milliseconds(),
	}, nil
}
