// Package formatcoverage composes the compiled format inventory used by daemon
// and embedded constructors.
package formatcoverage

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/coverage"
	"go.kenn.io/docbank/internal/formatqualification"
	"go.kenn.io/docbank/internal/ingest"
)

// Compute joins local implementation identities and already-constructed
// rendition descriptors. It performs no provider construction or I/O.
func Compute(bound []document.RenditionDescriptor, extractorID string) (document.FormatCoverageV1, error) {
	sources := coverage.DefaultSources()
	sources.BoundProviders = bound
	sources.ExtractorID = extractorID
	for _, qualification := range formatqualification.All() {
		switch qualification.Capability {
		case formatqualification.CapabilityMetadata:
			sources.Metadata = append(sources.Metadata, coverage.MetadataCapability{
				CatalogID: qualification.CatalogID, Evidence: qualification.Evidence,
				ImplementationID: extractorID,
			})
		case formatqualification.CapabilityRetain:
			sources.Retain = append(sources.Retain, coverage.RetainCapability{
				CatalogID: qualification.CatalogID, Evidence: qualification.Evidence,
				ImplementationID: ingest.OriginalRetentionImplementationID,
			})
		default:
			continue
		}
	}
	for _, format := range document.FormatMetadataCatalog() {
		switch {
		case format.Family == "text", format.Family == "structured", format.Family == "source",
			format.Family == "archive", format.MediaType == "text/csv":
			sources.Metadata = append(sources.Metadata, coverage.MetadataCapability{
				CatalogID: format.ID, NotApplicable: true,
			})
		}
	}
	return coverage.Compute(sources)
}
