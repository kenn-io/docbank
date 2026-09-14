// Package formatcoverage composes the dependency-neutral compiled format
// inventory used by daemon and embedded constructors.
package formatcoverage

import (
	"errors"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/coverage"
	"go.kenn.io/docbank/internal/formatqualification"
)

// Compute joins compiled local implementations and already-constructed
// rendition descriptors. It performs no provider construction or I/O.
func Compute(bound []document.RenditionDescriptor) (document.FormatCoverageV1, error) {
	sources := coverage.DefaultSources()
	sources.BoundProviders = slices.Clone(bound)
	sources.KnownProviders = slices.Clone(bound)
	sources.Qualifications = providerQualifications(bound)
	metadata, extractorFingerprint, err := metadataCapabilities()
	if err != nil {
		return document.FormatCoverageV1{}, err
	}
	sources.ExtractorFingerprint = extractorFingerprint
	sources.Metadata = metadata
	sources.Retain = retentionCapabilities()
	return coverage.Compute(sources)
}

func metadataCapabilities() ([]coverage.MetadataCapability, string, error) {
	result := []coverage.MetadataCapability{}
	extractorFingerprint := ""
	for _, qualification := range formatqualification.All() {
		if qualification.Capability != formatqualification.CapabilityMetadata {
			continue
		}
		if extractorFingerprint == "" {
			extractorFingerprint = qualification.ImplementationFingerprint
		} else if qualification.ImplementationFingerprint != extractorFingerprint {
			return nil, "", errors.New("metadata qualifications name conflicting extractor implementations")
		}
		result = append(result, coverage.MetadataCapability{
			CatalogID: qualification.CatalogID, Evidence: qualification.Evidence,
			ImplementationFingerprint: qualification.ImplementationFingerprint,
		})
	}
	for _, catalogID := range []string{
		"7z", "csv", "go", "gzip", "javascript", "json", "jsonl", "jsonl-alias", "latex",
		"markdown", "python", "rar", "rst", "tar", "txt", "xml", "yaml", "zip",
	} {
		result = append(result, coverage.MetadataCapability{CatalogID: catalogID, NotApplicable: true})
	}
	slices.SortFunc(result, func(left, right coverage.MetadataCapability) int {
		return strings.Compare(left.CatalogID, right.CatalogID)
	})
	if extractorFingerprint == "" {
		return nil, "", errors.New("metadata qualifications do not name an extractor implementation")
	}
	return result, extractorFingerprint, nil
}

func retentionCapabilities() []coverage.RetainCapability {
	result := []coverage.RetainCapability{}
	for _, qualification := range formatqualification.All() {
		if qualification.Capability != formatqualification.CapabilityRetain {
			continue
		}
		result = append(result, coverage.RetainCapability{
			CatalogID: qualification.CatalogID, Evidence: qualification.Evidence,
			ImplementationFingerprint: qualification.ImplementationFingerprint,
		})
	}
	return result
}

func providerQualifications(descriptors []document.RenditionDescriptor) []coverage.Qualification {
	known := make(map[string]bool, len(descriptors))
	for _, descriptor := range descriptors {
		known[descriptor.Fingerprint] = true
	}
	result := []coverage.Qualification{}
	for _, qualification := range formatqualification.All() {
		if qualification.DescriptorFingerprint == "" || !known[qualification.DescriptorFingerprint] {
			continue
		}
		result = append(result, coverage.Qualification{
			DescriptorFingerprint: qualification.DescriptorFingerprint,
			CatalogID:             qualification.CatalogID,
			Capability:            document.CapabilityKey(qualification.Capability),
			InputKind:             document.RenditionInputKind(qualification.InputKind),
			Evidence:              qualification.Evidence,
		})
	}
	return result
}
