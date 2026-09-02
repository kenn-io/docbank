// Package plaintext implements the bounded in-process UTF-8 rendition provider.
package plaintext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
)

const (
	providerID     = "plaintext.in-process-v1"
	profileVersion = "docbank-plaintext-profile/v1"
	provider       = providerutil.Provider("plain-text")

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
	})
	if err != nil {
		return nil, fmt.Errorf("plaintext: construct descriptor: %w", err)
	}
	return &Provider{descriptor: providerutil.CloneDescriptor(descriptor), maxDocumentBytes: profile.MaxDocumentBytes}, nil
}

// Descriptor returns the immutable provider identity fixed by the profile.
func (plaintext *Provider) Descriptor() document.RenditionDescriptor {
	if plaintext == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(plaintext.descriptor)
}

// Render reads and re-verifies one authorized upload, then emits one exact
// generic evidence unit. The provider never opens a path or performs I/O
// beyond the supplied read-once upload.
func (plaintext *Provider) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if plaintext == nil {
		return document.RenditionResult{}, errors.New("plaintext: provider is required")
	}
	metadata := upload.Metadata()
	if metadata.ByteLength > plaintext.maxDocumentBytes {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"input exceeds the plain-text byte limit", nil)
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, provider.Canceled(err)
	}
	startedAt := time.Now().UTC()
	data, err := providerutil.ReadAuthorizedUpload(ctx, upload, metadata, string(provider))
	if err != nil {
		return document.RenditionResult{}, err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorUnsupportedInput,
			"input is not UTF-8 plain text", nil)
	}
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{
		Descriptor: plaintext.descriptor, Authorization: authorization, SourceSHA256: metadata.SHA256,
		OperationID: "plaintext-" + authorization.RenditionRequestFingerprint,
		StartedAt:   startedAt, CompletedAt: time.Now().UTC(), Warnings: []string{"degraded_provenance"},
		Usage: document.RenditionUsage{
			Requests: 1, InputBytes: int64(len(data)), OutputBytes: int64(len(data)), Units: 1,
		},
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return document.RenditionResult{
		Evidence: providerutil.DegradedEvidence(authorization.MediaFamily, string(data),
			"plain-text provider emits one generic unit"),
		Receipt: receipt,
	}, nil
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

var _ document.RenditionProvider = (*Provider)(nil)
