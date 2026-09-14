package processing

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/coverage"
	internalformatcoverage "go.kenn.io/docbank/internal/formatcoverage"
)

// FormatCoverage composes the pure coverage computation with the exact local
// implementation identities and already-constructed provider descriptors.
func FormatCoverage(bound []document.RenditionDescriptor) (document.FormatCoverageV1, error) {
	return internalformatcoverage.Compute(bound)
}

// LookupFormat resolves one query against the same compiled snapshot inputs.
func LookupFormat(
	bound []document.RenditionDescriptor, query string,
) (document.FormatLookupV1, error) {
	record, err := FormatCoverage(bound)
	if err != nil {
		return document.FormatLookupV1{}, err
	}
	return coverage.Lookup(record, query), nil
}

// FormatCoverage returns the immutable coverage snapshot captured after all
// configured rendition providers were constructed and validated.
func (service *Service) FormatCoverage() document.FormatCoverageV1 {
	return document.CloneFormatCoverageV1(service.formatCoverage)
}

// LookupFormat resolves query against the same constructor-frozen snapshot.
func (service *Service) LookupFormat(query string) document.FormatLookupV1 {
	return coverage.Lookup(service.formatCoverage, query)
}
