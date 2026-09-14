package document

import "slices"

const (
	// FormatCoverageContractV1 identifies the first machine-readable format
	// capability inventory.
	FormatCoverageContractV1 = "format-coverage/v1"

	MaxFormatCoverageRows    = 512
	MaxFormatVariantRows     = 256
	MaxCoverageEvidenceBytes = 512
	MaxCoverageNoteBytes     = 512
)

// CapabilityKey identifies one independently qualified format behavior.
type CapabilityKey string

const (
	CapabilityDetect     CapabilityKey = "detect"
	CapabilityRetain     CapabilityKey = "retain"
	CapabilityMetadata   CapabilityKey = "metadata"
	CapabilityExpand     CapabilityKey = "expand"
	CapabilityText       CapabilityKey = "text"
	CapabilityPages      CapabilityKey = "pages"
	CapabilityTranscript CapabilityKey = "transcript"
)

var allCapabilityKeys = []CapabilityKey{
	CapabilityDetect,
	CapabilityRetain,
	CapabilityMetadata,
	CapabilityExpand,
	CapabilityText,
	CapabilityPages,
	CapabilityTranscript,
}

// AllCapabilityKeys returns the closed capability vocabulary in display order.
func AllCapabilityKeys() []CapabilityKey { return slices.Clone(allCapabilityKeys) }

// ValidCapabilityKey reports whether key belongs to the closed vocabulary.
func ValidCapabilityKey(key CapabilityKey) bool { return slices.Contains(allCapabilityKeys, key) }

// CapabilityState distinguishes fixture qualification from runtime availability.
type CapabilityState string

const (
	CapabilityQualified     CapabilityState = "qualified"
	CapabilityUnqualified   CapabilityState = "unqualified"
	CapabilityUnsupported   CapabilityState = "unsupported"
	CapabilityNotApplicable CapabilityState = "not_applicable"
)

var allCapabilityStates = []CapabilityState{
	CapabilityQualified,
	CapabilityUnqualified,
	CapabilityUnsupported,
	CapabilityNotApplicable,
}

// AllCapabilityStates returns the closed state vocabulary in display order.
func AllCapabilityStates() []CapabilityState { return slices.Clone(allCapabilityStates) }

// ValidCapabilityState reports whether state belongs to the closed vocabulary.
func ValidCapabilityState(state CapabilityState) bool {
	return slices.Contains(allCapabilityStates, state)
}

// FormatLookupMatch identifies which inventory surface matched a query.
type FormatLookupMatch string

const (
	FormatLookupFormat  FormatLookupMatch = "format"
	FormatLookupPending FormatLookupMatch = "pending"
	FormatLookupUnknown FormatLookupMatch = "unknown_format"
)

type CapabilityStateV1 struct {
	Evidence            string          `json:"evidence"`
	Note                string          `json:"note"`
	Provider            string          `json:"provider"`
	ProviderFingerprint string          `json:"provider_fingerprint"`
	State               CapabilityState `json:"state"`
}

type FormatVariantCapabilityV1 struct {
	Capabilities map[CapabilityKey]CapabilityStateV1 `json:"capabilities"`
}

type FormatCapabilityV1 struct {
	Capabilities map[CapabilityKey]CapabilityStateV1 `json:"capabilities"`
	Extensions   []string                            `json:"extensions"`
	ID           string                              `json:"id"`
	MediaType    string                              `json:"media_type"`
	QueryFamily  string                              `json:"query_family"`
	UnitKind     string                              `json:"unit_kind"`
	Variants     []FormatVariantCapabilityV1         `json:"variants"`
}

type PendingFormatV1 struct {
	Extensions []string `json:"extensions"`
	Label      string   `json:"label"`
	Note       string   `json:"note"`
	OwnerSlice string   `json:"owner_slice"`
}

type CoverageSourcesV1 struct {
	BoundProviders []string `json:"bound_providers"`
	CatalogRows    int      `json:"catalog_rows"`
	ExtractorID    string   `json:"extractor_id"`
}

type FormatCoverageV1 struct {
	ContractVersion string               `json:"contract_version"`
	Formats         []FormatCapabilityV1 `json:"formats"`
	GeneratedBy     CoverageSourcesV1    `json:"generated_by"`
	Pending         []PendingFormatV1    `json:"pending"`
}

type FormatLookupV1 struct {
	Format  *FormatCapabilityV1 `json:"format,omitzero"`
	Match   FormatLookupMatch   `json:"match"`
	Pending *PendingFormatV1    `json:"pending,omitzero"`
	Query   string              `json:"query"`
}
