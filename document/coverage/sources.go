package coverage

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/formatqualification"
)

type DetectCapability struct {
	CatalogID        string
	Evidence         string
	ImplementationID string
}

type RetainCapability struct {
	CatalogID        string
	Evidence         string
	ImplementationID string
}

type MetadataCapability struct {
	CatalogID        string
	Evidence         string
	ImplementationID string
	NotApplicable    bool
}

type Sources struct {
	BoundProviders []document.RenditionDescriptor
	Detect         []DetectCapability
	Retain         []RetainCapability
	ExtractorID    string
	Metadata       []MetadataCapability
	Pending        []document.PendingFormatV1
}

// DefaultSources returns only dependency-free compiled inputs. Runtime owners
// add their exact implementation identities before calling Compute.
func DefaultSources() Sources {
	mediaFormats := make(map[string]bool)
	for _, format := range media.DetectableFormats() {
		mediaFormats[string(format)] = true
	}
	documentFormats := make(map[string]bool)
	for _, format := range formatdetect.CandidateFormats() {
		documentFormats[format.ID] = true
	}
	detect := []DetectCapability{}
	for _, qualification := range formatqualification.All() {
		if qualification.Capability != formatqualification.CapabilityDetect {
			continue
		}
		matchesMedia := qualification.ImplementationID == media.DetectionImplementationID &&
			mediaFormats[qualification.CatalogID]
		matchesDocument := qualification.ImplementationID == formatdetect.DetectionImplementationID &&
			documentFormats[qualification.CatalogID]
		if matchesMedia || matchesDocument {
			detect = append(detect, DetectCapability{
				CatalogID: qualification.CatalogID, Evidence: qualification.Evidence,
				ImplementationID: qualification.ImplementationID,
			})
		}
	}
	return Sources{Detect: detect, Pending: document.PendingFormats()}
}
