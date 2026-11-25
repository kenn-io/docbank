package processing

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/coverage"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/ingest"
)

// ComputeFormatCoverage joins local implementation identities and already-constructed
// rendition descriptors. It performs no provider construction or I/O.
func ComputeFormatCoverage(bound []document.RenditionDescriptor, extractorID string) (document.FormatCoverageV1, error) {
	sources := coverage.DefaultSources()
	sources.BoundProviders = bound
	sources.ExtractorID = extractorID
	for _, qualification := range canonical.AllFormatQualifications() {
		switch qualification.Capability {
		case canonical.FormatCapabilityMetadata:
			sources.Metadata = append(sources.Metadata, coverage.MetadataCapability{
				CatalogID: qualification.CatalogID, Evidence: qualification.Evidence,
				ImplementationID: extractorID,
			})
		case canonical.FormatCapabilityRetain:
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
