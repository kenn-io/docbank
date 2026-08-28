// Package plaintext implements the bounded in-process UTF-8 rendition provider.
package plaintext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const (
	providerID     = "plaintext.in-process-v1"
	profileVersion = "docbank-plaintext-profile/v1"
	timestampForm  = "2006-01-02T15:04:05.000000000Z"

	// MaxDocumentBytes preserves the released bounded plain-text extraction limit.
	MaxDocumentBytes = int64(16 << 20)
)

// Profile fixes the maximum exact upload size accepted by one provider instance.
type Profile struct {
	MaxDocumentBytes int64
}

// Provider renders verified UTF-8 bytes without network access.
type Provider struct {
	descriptor       document.RenditionDescriptor
	maxDocumentBytes int64
}

// New constructs one immutable local provider profile.
func New(profile Profile) (*Provider, error) {
	if profile.MaxDocumentBytes <= 0 || profile.MaxDocumentBytes > MaxDocumentBytes {
		return nil, fmt.Errorf("plaintext: max document bytes must be between 1 and %d", MaxDocumentBytes)
	}
	policyDigest := sha256.Sum256([]byte(profileVersion + "\x00" +
		strconv.FormatInt(profile.MaxDocumentBytes, 10)))
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID:                providerID,
		ContractVersion:   document.RenditionProviderContractVersion,
		PolicyFingerprint: hex.EncodeToString(policyDigest[:]),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats:  supportedFormats(),
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	if err != nil {
		return nil, fmt.Errorf("plaintext: construct descriptor: %w", err)
	}
	return &Provider{descriptor: cloneDescriptor(descriptor), maxDocumentBytes: profile.MaxDocumentBytes}, nil
}

// Descriptor returns the immutable provider identity fixed by the profile.
func (provider *Provider) Descriptor() document.RenditionDescriptor {
	if provider == nil {
		return document.RenditionDescriptor{}
	}
	return cloneDescriptor(provider.descriptor)
}

// Render reads and re-verifies one authorized upload, then emits one exact
// generic evidence unit. The provider never opens a path or performs I/O
// beyond the supplied read-once upload.
func (provider *Provider) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if provider == nil {
		return document.RenditionResult{}, errors.New("plaintext: provider is required")
	}
	if _, err := document.ValidateRenditionProviderRequest(provider, upload, authorization); err != nil {
		return document.RenditionResult{}, err
	}
	metadata := upload.Metadata()
	if metadata.ByteLength > provider.maxDocumentBytes {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected,
			"input exceeds the plain-text byte limit", nil)
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorCanceled,
			"plain-text rendering canceled", err)
	}

	startedAt := time.Now().UTC()
	data, err := readExact(ctx, upload, metadata.ByteLength, provider.maxDocumentBytes)
	if err != nil {
		return document.RenditionResult{}, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != metadata.SHA256 {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected,
			"authorized upload identity mismatch", nil)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorUnsupportedInput,
			"input is not UTF-8 plain text", nil)
	}
	completedAt := time.Now().UTC()
	return document.RenditionResult{
		Evidence: document.SourceEvidenceV1{
			ContractVersion: document.SourceEvidenceContractV1,
			Completeness:    document.EvidenceDegradedProvenance,
			Family:          authorization.MediaFamily,
			UnitKind:        document.EvidenceUnitGeneric,
			Omissions: []document.SourceEvidenceOmissionV1{{
				Kind: document.EvidenceOmissionField, Field: "natural_provenance",
				Reason: "plain-text provider emits one generic unit",
			}},
			Units: []document.SourceEvidenceUnitV1{{
				Order: 0, Text: string(data),
				Locator: document.SourceEvidenceLocatorV1{
					Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
				},
			}},
		},
		Receipt: document.RenditionReceipt{
			ProviderID: provider.descriptor.ID, DescriptorFingerprint: provider.descriptor.Fingerprint,
			PolicyFingerprint: authorization.PolicyFingerprint, SourceSHA256: metadata.SHA256,
			OperationID: "plaintext-" + authorization.RenditionRequestFingerprint[:24],
			StartedAt:   startedAt.Format(timestampForm), CompletedAt: completedAt.Format(timestampForm),
			Warnings: []string{"degraded_provenance"},
			Usage: document.RenditionUsage{
				Requests: 1, InputBytes: int64(len(data)), OutputBytes: int64(len(data)), Units: 1,
			},
		},
	}, nil
}

func readExact(
	ctx context.Context, reader io.Reader, expectedBytes, maxBytes int64,
) ([]byte, error) {
	data := make([]byte, 0, expectedBytes)
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, classifiedError(document.RenditionErrorCanceled,
				"plain-text rendering canceled", err)
		}
		read, err := reader.Read(buffer)
		if read > 0 {
			if int64(len(data))+int64(read) > maxBytes {
				return nil, classifiedError(document.RenditionErrorPolicyRejected,
					"input exceeds the plain-text byte limit", nil)
			}
			data = append(data, buffer[:read]...)
		}
		switch {
		case errors.Is(err, io.EOF):
			if int64(len(data)) != expectedBytes {
				return nil, classifiedError(document.RenditionErrorPolicyRejected,
					"authorized upload identity mismatch", nil)
			}
			return data, nil
		case err != nil:
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, classifiedError(document.RenditionErrorCanceled,
					"plain-text rendering canceled", contextErr)
			}
			return nil, classifiedError(document.RenditionErrorTransient,
				"could not read the authorized upload", err)
		case read == 0:
			return nil, classifiedError(document.RenditionErrorTransient,
				"authorized upload stopped making progress", io.ErrNoProgress)
		}
	}
}

func classifiedError(code document.RenditionErrorCode, message string, cause error) error {
	providerError, err := document.NewRenditionProviderError(code, message, 0, cause)
	if err != nil {
		return fmt.Errorf("plaintext: classify provider error: %w", err)
	}
	return providerError
}

func supportedFormats() []document.RenditionFormatCapability {
	formats := []struct {
		family    string
		mediaType string
	}{
		{family: "mail", mediaType: "message/rfc822"},
		{family: "source", mediaType: "text/javascript"},
		{family: "source", mediaType: "text/x-go"},
		{family: "source", mediaType: "text/x-python"},
		{family: "spreadsheet", mediaType: "text/csv"},
		{family: "structured", mediaType: "application/json"},
		{family: "structured", mediaType: "application/x-ndjson"},
		{family: "structured", mediaType: "application/xml"},
		{family: "structured", mediaType: "application/yaml"},
		{family: "text", mediaType: "application/x-tex"},
		{family: "text", mediaType: "text/markdown"},
		{family: "text", mediaType: "text/plain"},
		{family: "text", mediaType: "text/x-rst"},
	}
	result := make([]document.RenditionFormatCapability, 0, len(formats))
	for _, format := range formats {
		result = append(result, document.RenditionFormatCapability{
			MediaFamily: format.family, MediaType: format.mediaType,
			InputKind: document.RenditionInputOriginalFile,
		})
	}
	return result
}

func cloneDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = slices.Clone(value.SupportedFormats)
	value.ArtifactRoles = slices.Clone(value.ArtifactRoles)
	return value
}

var _ document.RenditionProvider = (*Provider)(nil)
