// Package epub implements the bounded local EPUB rendition provider.
package epub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/epubutil"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/providerutil"
)

const (
	providerID = "epub.in-process-v1"
	provider   = providerutil.Provider("epub")
	// Changes to admission, normalization, budgets, or counting require a new version.
	policyVersion = "epub/v3:single-reflowable-unencrypted;paths-v1;xhtml-xml-v1;complete-or-reject;input=min(profile,100MiB);work=500MiB;intermediate=min(100MiB,input+inline-and-structural-storage);runes=min(result-bytes,3888*remaining-units);evidence-bounds;LF-NFC;80-codepoints;48-lines;per-occurrence;repeats;linear-no;empty-zero;internal-blanks;terminal-newline-free;exact-limit;virtual-only"
	maxXHTMLBytes = int64(100 << 20)
)

// Profile fixes both input bytes and cumulative virtual units; zero is invalid.
type Profile struct {
	MaxDocumentBytes int64
	MaxUnits         int64
}

// Provider renders verified EPUB bytes without network access.
type Provider struct {
	descriptor document.RenditionDescriptor
	profile    Profile
}

// New snapshots validated limits into an immutable provider identity.
func New(profile Profile) (*Provider, error) {
	if profile.MaxDocumentBytes < 1 || profile.MaxDocumentBytes > formatdetect.MaxDocumentBytes {
		return nil, fmt.Errorf("epub: max document bytes must be between 1 and %d", formatdetect.MaxDocumentBytes)
	}
	if profile.MaxUnits < 1 || profile.MaxUnits > 1_000_000 {
		return nil, errors.New("epub: max units must be between 1 and 1000000")
	}
	identity := policyVersion + "\x00" + formatdetect.DetectionImplementationID + "\x00" + strconv.FormatInt(profile.MaxDocumentBytes, 10) + "\x00" + strconv.FormatInt(profile.MaxUnits, 10)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: providerID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: providerutil.SHA256Hex([]byte(identity)), TrustBoundary: document.RenditionTrustLocalProcess,
		SupportedFormats:  []document.RenditionFormatCapability{{MediaFamily: "ebook", MediaType: "application/epub+zip", InputKind: document.RenditionInputOriginalFile}},
		ReturnsStructured: true, ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	if err != nil {
		return nil, fmt.Errorf("epub: descriptor: %w", err)
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

// Render verifies authorized bytes and produces one unit for each spine occurrence.
func (p *Provider) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	if p == nil {
		return document.RenditionResult{}, errors.New("epub: provider is required")
	}
	metadata := upload.Metadata()
	if metadata.ByteLength <= 0 || metadata.ByteLength > p.profile.MaxDocumentBytes {
		return document.RenditionResult{}, rejected("input exceeds the EPUB byte limit")
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, provider.Canceled(err)
	}
	started := time.Now().UTC()
	data, err := providerutil.ReadAuthorizedUpload(ctx, upload, metadata, string(provider))
	if err != nil {
		return document.RenditionResult{}, err
	}
	if _, err := formatdetect.DetectFormatContext(ctx, bytes.NewReader(data), int64(len(data)), "application/epub+zip"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return document.RenditionResult{}, provider.Canceled(ctxErr)
		}
		return document.RenditionResult{}, unsupported()
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, provider.Canceled(err)
	}
	archive, err := epubutil.NewReaderContext(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return document.RenditionResult{}, provider.Canceled(ctxErr)
		}
		return document.RenditionResult{}, unsupported()
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, provider.Canceled(err)
	}
	records, err := epubutil.ReadPackagesContext(ctx, archive.File, min(p.profile.MaxDocumentBytes, maxXHTMLBytes))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return document.RenditionResult{}, provider.Canceled(ctxErr)
		}
		return document.RenditionResult{}, unsupported()
	}
	entries, err := admitSpine(archive.File, records)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return document.RenditionResult{}, provider.Canceled(ctxErr)
		}
		return document.RenditionResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, provider.Canceled(err)
	}
	evidence := document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Family: "ebook", UnitKind: document.EvidenceUnitSpine, Completeness: document.EvidenceComplete}
	var work, units, outputBytes int64
	for index, entry := range entries {
		if err := ctx.Err(); err != nil {
			return document.RenditionResult{}, provider.Canceled(err)
		}
		if entry.UncompressedSize64 > uint64(maxXHTMLBytes) {
			return document.RenditionResult{}, rejected("EPUB expanded work exceeds its limit")
		}
		entryBytes := int64(entry.UncompressedSize64)
		if entryBytes > p.profile.MaxDocumentBytes || entryBytes > formatdetect.MaxDocumentBytes-work {
			return document.RenditionResult{}, rejected("EPUB expanded work exceeds its limit")
		}
		work += entryBytes
		content, err := epubutil.ReadZIPEntryContext(ctx, entry, min(p.profile.MaxDocumentBytes, maxXHTMLBytes))
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return document.RenditionResult{}, provider.Canceled(ctxErr)
			}
			return document.RenditionResult{}, unsupported()
		}
		remainingBytes := int64(authorization.MaxTotalResultBytes) - outputBytes
		if remainingBytes < 0 {
			return document.RenditionResult{}, rejected("EPUB output exceeds its limit")
		}
		maxRunes := min(remainingBytes, 3888*(p.profile.MaxUnits-units))
		markdown, err := document.RenditionMarkdownFromXHTML(content, int(maxRunes))
		if ctxErr := ctx.Err(); ctxErr != nil {
			return document.RenditionResult{}, provider.Canceled(ctxErr)
		}
		if errors.Is(err, document.ErrRenditionXHTMLBudget) {
			return document.RenditionResult{}, rejected("EPUB normalization exceeds its limit")
		}
		if err != nil {
			return document.RenditionResult{}, unsupported()
		}
		used := virtualUnits(markdown)
		if used > p.profile.MaxUnits-units || int64(len(markdown)) > remainingBytes {
			return document.RenditionResult{}, rejected("EPUB output exceeds its limit")
		}
		units += used
		outputBytes += int64(len(markdown))
		evidence.Units = append(evidence.Units, document.SourceEvidenceUnitV1{Order: index, Text: markdown, Locator: document.SourceEvidenceLocatorV1{
			Kind: document.EvidenceLocatorSpine, IndexOrigin: document.EvidenceIndexOriginZero, Start: int64(index), End: int64(index), Name: entry.Name,
		}})
	}
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return document.RenditionResult{}, provider.Canceled(ctxErr)
		}
		return document.RenditionResult{}, rejected("EPUB evidence exceeds its limits")
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, provider.Canceled(err)
	}
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{
		Descriptor: p.descriptor, Authorization: authorization, SourceSHA256: metadata.SHA256,
		OperationID: "epub-" + authorization.RenditionRequestFingerprint, StartedAt: started, CompletedAt: time.Now().UTC(),
		Usage: document.RenditionUsage{Requests: 1, InputBytes: int64(len(data)), OutputBytes: outputBytes, Units: units},
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	result := document.RenditionResult{Evidence: evidence, Receipt: receipt}
	if err := document.ValidateRenditionResult(p.descriptor, authorization, result); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return document.RenditionResult{}, provider.Canceled(ctxErr)
		}
		return document.RenditionResult{}, rejected("EPUB result exceeds its authorization")
	}
	if err := ctx.Err(); err != nil {
		return document.RenditionResult{}, provider.Canceled(err)
	}
	return result, nil
}

func rejected(message string) error {
	return provider.Classified(document.RenditionErrorPolicyRejected, message, nil)
}
func unsupported() error {
	return provider.Classified(document.RenditionErrorUnsupportedInput, "EPUB package is malformed or unsupported", nil)
}

var _ document.RenditionProvider = (*Provider)(nil)
