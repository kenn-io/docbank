package coverage

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/formatqualification"
)

type DispatchEntry struct {
	CatalogID      string
	Compiled       bool
	DispatchFormat string
	Evidence       string
}

type DetectCapability struct {
	CatalogID                 string
	Evidence                  string
	ImplementationFingerprint string
}

type RetainCapability struct {
	CatalogID                 string
	Evidence                  string
	ImplementationFingerprint string
}

type MetadataCapability struct {
	CatalogID                 string
	Evidence                  string
	ImplementationFingerprint string
	NotApplicable             bool
}

type Qualification struct {
	DescriptorFingerprint string
	CatalogID             string
	Capability            document.CapabilityKey
	InputKind             document.RenditionInputKind
	Evidence              string
}

type Sources struct {
	BoundProviders       []document.RenditionDescriptor
	KnownProviders       []document.RenditionDescriptor
	Qualifications       []Qualification
	Detect               []DetectCapability
	Retain               []RetainCapability
	Dispatch             []DispatchEntry
	ExtractorFingerprint string
	Metadata             []MetadataCapability
	PageFramesAvailable  bool
	Pending              []document.PendingFormatV1
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
		matchesMedia := qualification.ImplementationFingerprint == media.DetectionImplementationFingerprint &&
			mediaFormats[qualification.CatalogID]
		matchesDocument := qualification.ImplementationFingerprint == formatdetect.DetectionImplementationFingerprint &&
			documentFormats[qualification.CatalogID]
		if matchesMedia || matchesDocument {
			detect = append(detect, DetectCapability{
				CatalogID: qualification.CatalogID, Evidence: qualification.Evidence,
				ImplementationFingerprint: qualification.ImplementationFingerprint,
			})
		}
	}
	return Sources{Detect: detect, Pending: document.PendingFormats()}
}
