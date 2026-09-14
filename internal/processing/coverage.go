package processing

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/coverage"
)

// FormatCoverage returns the immutable coverage snapshot captured after all
// configured rendition providers were constructed and validated.
func (service *Service) FormatCoverage() document.FormatCoverageV1 {
	return document.CloneFormatCoverageV1(service.formatCoverage)
}

// LookupFormat resolves query against the same constructor-frozen snapshot.
func (service *Service) LookupFormat(query string) document.FormatLookupV1 {
	return coverage.Lookup(service.formatCoverage, query)
}
