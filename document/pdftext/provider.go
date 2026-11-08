// Package pdftext extracts a PDF's existing text layer with embedded PDFium.
package pdftext

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

// AdapterContract selects this provider in a daemon rendition profile.
const AdapterContract = "docbank-pdftext-rendition/v1"
const provider = providerutil.Provider("pdftext")

// Profile fixes the source byte and page limits in the provider identity.
type Profile struct {
	MaxDocumentBytes int64
	MaxPages         int64
}

// Provider extracts verified PDF text without a separate executable or network.
type Provider struct {
	descriptor document.RenditionDescriptor
	profile    Profile
}

// New constructs a provider with immutable admission and extraction limits.
func New(profile Profile) (*Provider, error) {
	if profile.MaxDocumentBytes < 1 || profile.MaxDocumentBytes > formatdetect.MaxDocumentBytes || profile.MaxPages < 1 || profile.MaxPages > 1_000_000 {
		return nil, errors.New("pdftext: invalid byte or page limit")
	}
	identity := pdfproduction.PDFTextPolicy + "\x00" + formatdetect.DetectionImplementationID + "\x00" + strconv.FormatInt(profile.MaxDocumentBytes, 10) + "\x00" + strconv.FormatInt(profile.MaxPages, 10)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "pdftext.in-process-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: providerutil.SHA256Hex([]byte(identity)), TrustBoundary: document.RenditionTrustLocalProcess,
		SupportedFormats:  []document.RenditionFormatCapability{{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile}},
		ReturnsStructured: true, ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	if err != nil {
		return nil, err
	}
	return &Provider{descriptor: descriptor, profile: profile}, nil
}

// Descriptor returns a defensive copy of the provider identity.
func (p *Provider) Descriptor() document.RenditionDescriptor {
	if p == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(p.descriptor)
}

// Render verifies one authorized upload and records present and omitted pages.
func (p *Provider) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	if p == nil {
		return document.RenditionResult{}, errors.New("pdftext: provider is required")
	}
	metadata := upload.Metadata()
	if metadata.ByteLength < 1 || metadata.ByteLength > p.profile.MaxDocumentBytes {
		return document.RenditionResult{}, rejected("PDF exceeds byte limit")
	}
	operation, err := providerutil.NewOperation(ctx, provider, authorization.ExpiresAt, 0)
	if err != nil {
		return document.RenditionResult{}, err
	}
	defer operation.Cancel()
	ctx = operation.Context()
	started := time.Now().UTC()
	data, err := operation.ReadUpload(upload)
	if err != nil {
		return document.RenditionResult{}, err
	}
	measurements, err := formatdetect.InspectPDF(data, formatdetect.PDFLimits{TextOnly: true, MaxExpandedBytes: p.profile.MaxDocumentBytes, MaxEntryBytes: p.profile.MaxDocumentBytes, MaxEntries: 100_000})
	if err != nil {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorUnsupportedInput, "PDF is malformed or unsupported", err)
	}
	if measurements.Pages > p.profile.MaxPages {
		return document.RenditionResult{}, rejected("PDF exceeds page limit")
	}
	evidence := document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Family: "pdf", UnitKind: document.EvidenceUnitPage, Completeness: document.EvidenceComplete}
	remaining := authorization.MaxTotalResultBytes
	resultBudget := int64(authorization.MaxTotalResultBytes)
	for index := range int(measurements.Pages) {
		page, err := pdfproduction.ReadPDFTextPage(ctx, data, index, p.profile.MaxPages, min(remaining, 4_000_000))
		if err != nil {
			if operationErr := operation.Check(); operationErr != nil {
				return document.RenditionResult{}, operationErr
			}
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "extract PDF text", err)
		}
		if int64(page.Pages) != measurements.Pages {
			return document.RenditionResult{}, rejected("PDF page counts disagree")
		}
		locator := document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: int64(index + 1), End: int64(index + 1)}
		var observation any
		reason := ""
		if page.Characters == 0 {
			reason = "no_text_layer"
		} else if strings.TrimFunc(page.Text, func(character rune) bool { return unicode.IsSpace(character) || unicode.IsControl(character) }) == "" {
			reason = "no_readable_text"
		}
		if reason != "" {
			evidence.Completeness = document.EvidencePartial
			evidence.Omissions = append(evidence.Omissions, document.SourceEvidenceOmissionV1{Kind: document.EvidenceOmissionUnit, Reason: reason, Locator: &locator})
			observation = evidence.Omissions[len(evidence.Omissions)-1]
		} else {
			text := document.EscapeRenditionText(page.Text)
			if len(text) > remaining {
				return document.RenditionResult{}, rejected("PDF text exceeds output limit")
			}
			remaining -= len(text)
			evidence.Units = append(evidence.Units, document.SourceEvidenceUnitV1{Order: len(evidence.Units), Text: text, Locator: locator})
			observation = evidence.Units[len(evidence.Units)-1]
		}
		size, exceeded, err := canonical.BoundedSize(observation, resultBudget)
		if err != nil || exceeded {
			return document.RenditionResult{}, rejected("PDF evidence exceeds output limit")
		}
		resultBudget -= size
	}
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{Descriptor: p.descriptor, Authorization: authorization, SourceSHA256: metadata.SHA256,
		OperationID: "pdftext-" + authorization.RenditionRequestFingerprint, StartedAt: started, CompletedAt: time.Now().UTC(),
		Usage: document.RenditionUsage{Requests: 1, InputBytes: int64(len(data)), OutputBytes: int64(authorization.MaxTotalResultBytes - remaining), Units: measurements.Pages},
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	result := document.RenditionResult{Evidence: evidence, Receipt: receipt}
	if err := operation.Check(); err != nil {
		return document.RenditionResult{}, err
	}
	return result, nil
}

func rejected(message string) error {
	return provider.Classified(document.RenditionErrorPolicyRejected, message, nil)
}

var _ document.RenditionProvider = (*Provider)(nil)
