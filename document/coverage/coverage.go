// Package coverage computes format-coverage/v1 snapshots from immutable
// catalog, qualification, implementation, and runtime descriptor inputs.
package coverage

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/formatqualification"
)

// Compute joins the document catalog to explicit, fixture-backed source
// capabilities. It performs no I/O and does not construct providers.
func Compute(sources Sources) (document.FormatCoverageV1, error) {
	if sources.ExtractorFingerprint == "" {
		return document.FormatCoverageV1{}, errors.New("format coverage extractor fingerprint is required")
	}
	if err := validateMetadataCapabilities(sources.Metadata); err != nil {
		return document.FormatCoverageV1{}, err
	}

	catalog := document.FormatMetadataCatalog()
	slices.SortFunc(catalog, func(left, right document.FormatMetadata) int {
		return strings.Compare(left.ID, right.ID)
	})
	bound, err := providerSet(sources.BoundProviders)
	if err != nil {
		return document.FormatCoverageV1{}, err
	}
	known, err := canonicalProviders(sources.KnownProviders)
	if err != nil {
		return document.FormatCoverageV1{}, err
	}
	knownFingerprints := make(map[string]bool, len(known))
	for _, descriptor := range known {
		knownFingerprints[descriptor.Fingerprint] = true
	}
	for fingerprint := range bound {
		if !knownFingerprints[fingerprint] {
			return document.FormatCoverageV1{}, fmt.Errorf("bound provider %s is absent from the known provider set", fingerprint)
		}
	}

	record := document.FormatCoverageV1{
		ContractVersion: document.FormatCoverageContractV1,
		Formats:         make([]document.FormatCapabilityV1, 0, len(catalog)),
		GeneratedBy: document.CoverageSourcesV1{
			BoundProviders:       sortedKeys(bound),
			CatalogRows:          len(catalog),
			DecoderFormats:       compiledDecoderFormats(sources.Dispatch),
			ExtractorFingerprint: sources.ExtractorFingerprint,
		},
		Pending: clonePending(sources.Pending),
	}
	for _, metadata := range catalog {
		record.Formats = append(record.Formats, computeFormat(metadata, sources, known, bound))
	}
	slices.SortFunc(record.Pending, func(left, right document.PendingFormatV1) int {
		return strings.Compare(left.Label, right.Label)
	})
	if err := document.ValidateFormatCoverageV1(record); err != nil {
		return document.FormatCoverageV1{}, fmt.Errorf("validating computed format coverage: %w", err)
	}
	return record, nil
}

func computeFormat(
	metadata document.FormatMetadata, sources Sources, known []document.RenditionDescriptor,
	bound map[string]bool,
) document.FormatCapabilityV1 {
	localCapabilities := baselineCapabilities(metadata)
	applyLocalCapabilities(metadata.ID, sources, localCapabilities)
	capabilities := maps.Clone(localCapabilities)

	providerVariants := make([]document.FormatVariantCapabilityV1, 0, len(known))
	for _, descriptor := range known {
		variant, eligible := providerVariant(
			metadata, sources, descriptor, bound[descriptor.Fingerprint], localCapabilities,
		)
		if !eligible {
			continue
		}
		providerVariants = append(providerVariants, variant)
		for _, capability := range []document.CapabilityKey{
			document.CapabilityText, document.CapabilityPages, document.CapabilityTranscript,
		} {
			candidate := variant.Capabilities[capability]
			if betterCapability(candidate, capabilities[capability]) {
				capabilities[capability] = candidate
			}
		}
	}
	if len(providerVariants) < 2 {
		providerVariants = nil
	} else {
		slices.SortFunc(providerVariants, compareVariants)
	}

	return document.FormatCapabilityV1{
		Capabilities: capabilities,
		Extensions:   slices.Clone(metadata.Extensions),
		ID:           metadata.ID,
		MediaType:    metadata.MediaType,
		QueryFamily:  metadata.QueryFamily,
		UnitKind:     metadata.UnitKind,
		Variants:     providerVariants,
	}
}

func baselineCapabilities(metadata document.FormatMetadata) map[document.CapabilityKey]document.CapabilityStateV1 {
	result := make(map[document.CapabilityKey]document.CapabilityStateV1, len(document.AllCapabilityKeys()))
	for _, capability := range document.AllCapabilityKeys() {
		result[capability] = document.CapabilityStateV1{State: document.CapabilityUnsupported}
	}
	result[document.CapabilityDetect] = document.CapabilityStateV1{
		State: document.CapabilityUnqualified,
		Note:  "Catalog MIME and extension hints do not prove byte detection.",
	}
	result[document.CapabilityRetain] = document.CapabilityStateV1{
		State: document.CapabilityUnqualified,
		Note:  "Original-byte retention requires the qualified ingest implementation.",
	}
	if metadata.Family != "audio_video" {
		result[document.CapabilityTranscript] = document.CapabilityStateV1{State: document.CapabilityNotApplicable}
	}
	return result
}

func applyLocalCapabilities(
	catalogID string, sources Sources, capabilities map[document.CapabilityKey]document.CapabilityStateV1,
) {
	for _, candidate := range sources.Detect {
		if candidate.CatalogID == catalogID && localQualification(catalogID, document.CapabilityDetect,
			candidate.Evidence, candidate.ImplementationFingerprint) {
			capabilities[document.CapabilityDetect] = qualifiedLocal(candidate.Evidence)
		}
	}
	for _, candidate := range sources.Retain {
		if candidate.CatalogID == catalogID && localQualification(catalogID, document.CapabilityRetain,
			candidate.Evidence, candidate.ImplementationFingerprint) {
			capabilities[document.CapabilityRetain] = qualifiedLocal(candidate.Evidence)
		}
	}
	for _, candidate := range sources.Metadata {
		if candidate.CatalogID != catalogID {
			continue
		}
		if candidate.NotApplicable {
			capabilities[document.CapabilityMetadata] = document.CapabilityStateV1{State: document.CapabilityNotApplicable}
			continue
		}
		if localQualification(catalogID, document.CapabilityMetadata,
			candidate.Evidence, candidate.ImplementationFingerprint) {
			capabilities[document.CapabilityMetadata] = qualifiedLocal(candidate.Evidence)
		} else {
			capabilities[document.CapabilityMetadata] = document.CapabilityStateV1{State: document.CapabilityUnqualified}
		}
	}
	for _, candidate := range sources.Dispatch {
		if candidate.CatalogID == catalogID && candidate.Compiled {
			capabilities[document.CapabilityExpand] = document.CapabilityStateV1{
				State: document.CapabilityUnqualified,
				Note:  "Compiled dispatch declaration lacks independent fixture qualification.",
			}
		}
	}
}

func validateMetadataCapabilities(values []MetadataCapability) error {
	byCatalogID := make(map[string]MetadataCapability, len(values))
	for _, value := range values {
		previous, found := byCatalogID[value.CatalogID]
		if found && previous != value {
			return fmt.Errorf("conflicting metadata declarations for %s", value.CatalogID)
		}
		byCatalogID[value.CatalogID] = value
	}
	return nil
}

func localQualification(catalogID string, capability document.CapabilityKey, evidence, fingerprint string) bool {
	_, found := formatqualification.Lookup(formatqualification.Query{
		CatalogID: catalogID, Capability: formatqualification.Capability(capability), Evidence: evidence,
		ImplementationFingerprint: fingerprint, InputKind: formatqualification.InputOriginalFile,
	})
	return found
}

func qualifiedLocal(evidence string) document.CapabilityStateV1 {
	return document.CapabilityStateV1{State: document.CapabilityQualified, Evidence: evidence}
}

func providerVariant(
	metadata document.FormatMetadata, sources Sources, descriptor document.RenditionDescriptor,
	bound bool, baseline map[document.CapabilityKey]document.CapabilityStateV1,
) (document.FormatVariantCapabilityV1, bool) {
	capabilities := maps.Clone(baseline)
	eligible := false
	for _, capability := range []document.CapabilityKey{
		document.CapabilityText, document.CapabilityPages, document.CapabilityTranscript,
	} {
		if !descriptorSupports(descriptor, metadata, capability) {
			continue
		}
		eligible = true
		state := document.CapabilityStateV1{
			State: document.CapabilityUnqualified, Provider: descriptor.ID,
			ProviderFingerprint: descriptor.Fingerprint,
		}
		if capability == document.CapabilityPages && !sources.PageFramesAvailable {
			state.Note = "Verified page-frame machinery is unavailable."
			capabilities[capability] = state
			continue
		}
		if qualification, found := providerQualification(sources.Qualifications, descriptor, metadata.ID, capability); found {
			state.Evidence = qualification.Evidence
			if bound {
				state.State = document.CapabilityQualified
			} else {
				state.State = document.CapabilityProviderRequired
			}
		}
		capabilities[capability] = state
	}
	return document.FormatVariantCapabilityV1{Capabilities: capabilities}, eligible
}

func descriptorSupports(
	descriptor document.RenditionDescriptor, metadata document.FormatMetadata, capability document.CapabilityKey,
) bool {
	if capability == document.CapabilityTranscript && metadata.Family != "audio_video" {
		return false
	}
	formatMatch := false
	for _, format := range descriptor.SupportedFormats {
		if format.MediaFamily == metadata.Family && format.MediaType == metadata.MediaType &&
			format.InputKind == document.RenditionInputOriginalFile {
			formatMatch = true
			break
		}
	}
	if !formatMatch {
		return false
	}
	var role document.EvidenceArtifactRole
	switch capability {
	case document.CapabilityText:
		if !descriptor.ReturnsMarkdown {
			return false
		}
		role = document.EvidenceArtifactMarkdown
	case document.CapabilityPages:
		role = document.EvidenceArtifactImage
	case document.CapabilityTranscript:
		role = document.EvidenceArtifactTranscript
	default:
		return false
	}
	return slices.Contains(descriptor.ArtifactRoles, role)
}

func providerQualification(
	qualifications []Qualification, descriptor document.RenditionDescriptor, catalogID string,
	capability document.CapabilityKey,
) (Qualification, bool) {
	for _, qualification := range qualifications {
		if qualification.DescriptorFingerprint != descriptor.Fingerprint || qualification.CatalogID != catalogID ||
			qualification.Capability != capability || qualification.InputKind != document.RenditionInputOriginalFile {
			continue
		}
		_, found := formatqualification.Lookup(formatqualification.Query{
			CatalogID: catalogID, Capability: formatqualification.Capability(capability),
			Evidence: qualification.Evidence, DescriptorFingerprint: descriptor.Fingerprint,
			InputKind: formatqualification.InputOriginalFile,
		})
		if found {
			return qualification, true
		}
	}
	return Qualification{}, false
}

func betterCapability(candidate, current document.CapabilityStateV1) bool {
	candidateRank, currentRank := capabilityRank(candidate.State), capabilityRank(current.State)
	if candidateRank != currentRank {
		return candidateRank > currentRank
	}
	if candidate.ProviderFingerprint == "" {
		return false
	}
	return current.ProviderFingerprint == "" || candidate.ProviderFingerprint < current.ProviderFingerprint
}

func capabilityRank(state document.CapabilityState) int {
	switch state {
	case document.CapabilityQualified:
		return 4
	case document.CapabilityProviderRequired:
		return 3
	case document.CapabilityUnqualified:
		return 2
	case document.CapabilityUnsupported:
		return 1
	default:
		return 0
	}
}

func canonicalProviders(values []document.RenditionDescriptor) ([]document.RenditionDescriptor, error) {
	byFingerprint := make(map[string]document.RenditionDescriptor, len(values))
	for _, descriptor := range values {
		canonical, err := document.NewRenditionDescriptor(descriptor)
		if err != nil || !reflect.DeepEqual(canonical, descriptor) {
			return nil, fmt.Errorf("provider %q descriptor identity is invalid", descriptor.ID)
		}
		byFingerprint[descriptor.Fingerprint] = descriptor
	}
	result := make([]document.RenditionDescriptor, 0, len(byFingerprint))
	for _, descriptor := range byFingerprint {
		result = append(result, descriptor)
	}
	slices.SortFunc(result, func(left, right document.RenditionDescriptor) int {
		return strings.Compare(left.Fingerprint, right.Fingerprint)
	})
	return result, nil
}

func providerSet(values []document.RenditionDescriptor) (map[string]bool, error) {
	result := make(map[string]bool, len(values))
	for _, descriptor := range values {
		canonical, err := document.NewRenditionDescriptor(descriptor)
		if err != nil || !reflect.DeepEqual(canonical, descriptor) {
			return nil, fmt.Errorf("bound provider %q descriptor identity is invalid", descriptor.ID)
		}
		result[descriptor.Fingerprint] = true
	}
	return result, nil
}

func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func compiledDecoderFormats(entries []DispatchEntry) []string {
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.Compiled {
			seen[entry.DispatchFormat] = true
		}
	}
	return sortedKeys(seen)
}

func compareVariants(left, right document.FormatVariantCapabilityV1) int {
	if order := strings.Compare(left.Container, right.Container); order != 0 {
		return order
	}
	if order := strings.Compare(left.Codec, right.Codec); order != 0 {
		return order
	}
	leftBytes, _ := canonical.Marshal(left.Capabilities)
	rightBytes, _ := canonical.Marshal(right.Capabilities)
	return bytes.Compare(leftBytes, rightBytes)
}

// Lookup resolves a catalog id or extension before the pending roster and
// preserves the caller's exact query in its response.
func Lookup(record document.FormatCoverageV1, query string) document.FormatLookupV1 {
	normalized := strings.ToLower(strings.TrimPrefix(query, "."))
	for _, format := range record.Formats {
		if normalized == format.ID || slices.Contains(format.Extensions, normalized) {
			foundFormat := cloneFormat(format)
			return document.FormatLookupV1{Format: &foundFormat, Match: document.FormatLookupFormat, Query: query}
		}
	}
	for _, pending := range record.Pending {
		if normalized == strings.ToLower(pending.Label) || slices.Contains(pending.Extensions, normalized) {
			foundPending := pending
			foundPending.Extensions = slices.Clone(pending.Extensions)
			return document.FormatLookupV1{Match: document.FormatLookupPending, Pending: &foundPending, Query: query}
		}
	}
	return document.FormatLookupV1{Match: document.FormatLookupUnknown, Query: query}
}

func cloneFormat(value document.FormatCapabilityV1) document.FormatCapabilityV1 {
	value.Extensions = slices.Clone(value.Extensions)
	value.Capabilities = maps.Clone(value.Capabilities)
	value.Variants = slices.Clone(value.Variants)
	for index := range value.Variants {
		value.Variants[index].Capabilities = maps.Clone(value.Variants[index].Capabilities)
	}
	return value
}

func clonePending(values []document.PendingFormatV1) []document.PendingFormatV1 {
	result := slices.Clone(values)
	for index := range result {
		result[index].Extensions = slices.Clone(result[index].Extensions)
	}
	return result
}
