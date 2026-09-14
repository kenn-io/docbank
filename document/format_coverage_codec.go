package document

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/formatqualification"
)

const (
	maxFormatCoverageEncodedBytes = 16 << 20
	maxCoverageLabelBytes         = 512
)

// MarshalFormatCoverageV1 validates and canonically encodes one coverage
// record. The returned fingerprint is the lowercase SHA-256 of those bytes.
func MarshalFormatCoverageV1(value FormatCoverageV1) ([]byte, string, error) {
	record, err := canonicalFormatCoverageV1(value)
	if err != nil {
		return nil, "", err
	}
	encoded, err := canonical.Marshal(record)
	if err != nil {
		return nil, "", fmt.Errorf("encoding format coverage: %w", err)
	}
	if len(encoded) > maxFormatCoverageEncodedBytes {
		return nil, "", errors.New("format coverage record is too large")
	}
	digest := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(digest[:]), nil
}

// DecodeFormatCoverageV1 accepts strict ordinary JSON and returns its
// canonical value. Member order and insignificant whitespace do not define
// transport identity; callers use MarshalFormatCoverageV1 when identity is
// required.
func DecodeFormatCoverageV1(raw []byte) (FormatCoverageV1, error) {
	if len(raw) > maxFormatCoverageEncodedBytes {
		return FormatCoverageV1{}, errors.New("format coverage record is too large")
	}
	var value FormatCoverageV1
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return FormatCoverageV1{}, fmt.Errorf("decoding format coverage: %w", err)
	}
	return canonicalFormatCoverageV1(value)
}

// ValidateFormatCoverageV1 verifies the bounded, sorted contract and every
// independently qualified evidence claim without mutating value.
func ValidateFormatCoverageV1(value FormatCoverageV1) error {
	_, err := canonicalFormatCoverageV1(value)
	return err
}

// CloneFormatCoverageV1 returns a defensive copy of every mutable nested
// collection in value without canonicalizing or validating it.
func CloneFormatCoverageV1(value FormatCoverageV1) FormatCoverageV1 {
	return cloneFormatCoverageV1(value)
}

func canonicalFormatCoverageV1(value FormatCoverageV1) (FormatCoverageV1, error) {
	value = cloneFormatCoverageV1(value)
	canonicalizeCoverageSlices(&value)
	if err := validateFormatCoverageV1(value); err != nil {
		return FormatCoverageV1{}, err
	}
	return value, nil
}

func cloneFormatCoverageV1(value FormatCoverageV1) FormatCoverageV1 {
	value.GeneratedBy.BoundProviders = slices.Clone(value.GeneratedBy.BoundProviders)
	value.Formats = slices.Clone(value.Formats)
	for formatIndex := range value.Formats {
		format := &value.Formats[formatIndex]
		format.Extensions = slices.Clone(format.Extensions)
		format.Capabilities = maps.Clone(format.Capabilities)
		format.Variants = slices.Clone(format.Variants)
		for variantIndex := range format.Variants {
			format.Variants[variantIndex].Capabilities = maps.Clone(format.Variants[variantIndex].Capabilities)
		}
	}
	value.Pending = slices.Clone(value.Pending)
	for index := range value.Pending {
		value.Pending[index].Extensions = slices.Clone(value.Pending[index].Extensions)
	}
	return value
}

func canonicalizeCoverageSlices(value *FormatCoverageV1) {
	if value.GeneratedBy.BoundProviders == nil {
		value.GeneratedBy.BoundProviders = []string{}
	}
	if value.Formats == nil {
		value.Formats = []FormatCapabilityV1{}
	}
	for formatIndex := range value.Formats {
		format := &value.Formats[formatIndex]
		if format.Extensions == nil {
			format.Extensions = []string{}
		}
		if format.Variants == nil {
			format.Variants = []FormatVariantCapabilityV1{}
		}
	}
	if value.Pending == nil {
		value.Pending = []PendingFormatV1{}
	}
	for index := range value.Pending {
		if value.Pending[index].Extensions == nil {
			value.Pending[index].Extensions = []string{}
		}
	}
}

func validateFormatCoverageV1(value FormatCoverageV1) error {
	if value.ContractVersion != FormatCoverageContractV1 {
		return fmt.Errorf("format coverage contract version must be %q", FormatCoverageContractV1)
	}
	if value.GeneratedBy.CatalogRows < len(value.Formats) || value.GeneratedBy.CatalogRows > MaxFormatCoverageRows {
		return fmt.Errorf("format coverage catalog rows must be between the returned format count and %d", MaxFormatCoverageRows)
	}
	if err := validateCoverageText(value.GeneratedBy.ExtractorID, "extractor identity", false); err != nil {
		return err
	}
	if err := validateSortedCoverageStrings(value.GeneratedBy.BoundProviders, "bound provider", true); err != nil {
		return err
	}
	if len(value.Formats) > MaxFormatCoverageRows {
		return errors.New("format coverage has too many format rows")
	}
	variantRows := 0
	previousID := ""
	for index, format := range value.Formats {
		if err := validateCoverageText(format.ID, "format id", false); err != nil {
			return fmt.Errorf("format row %d: %w", index, err)
		}
		if index > 0 && format.ID <= previousID {
			if format.ID == previousID {
				return fmt.Errorf("format coverage has duplicate format id %q", format.ID)
			}
			return errors.New("format coverage rows must be sorted by id")
		}
		previousID = format.ID
		for subject, field := range map[string]string{
			"media type": format.MediaType, "query family": format.QueryFamily, "unit kind": format.UnitKind,
		} {
			if err := validateCoverageText(field, subject, subject == "unit kind"); err != nil {
				return fmt.Errorf("format %q: %w", format.ID, err)
			}
		}
		if err := validateCoverageStringList(format.Extensions, "extension"); err != nil {
			return fmt.Errorf("format %q: %w", format.ID, err)
		}
		if err := validateCapabilityMap(value, format.ID, format.Capabilities); err != nil {
			return fmt.Errorf("format %q: %w", format.ID, err)
		}
		variantRows += len(format.Variants)
		if variantRows > MaxFormatVariantRows {
			return errors.New("format coverage has too many variant rows")
		}
		if err := validateVariants(value, format.ID, format.Variants); err != nil {
			return fmt.Errorf("format %q: %w", format.ID, err)
		}
	}
	if len(value.Pending) > MaxFormatCoverageRows {
		return errors.New("format coverage has too many pending rows")
	}
	previousLabel := ""
	for index, pending := range value.Pending {
		if err := validateCoverageText(pending.Label, "pending label", false); err != nil {
			return fmt.Errorf("pending row %d: %w", index, err)
		}
		if index > 0 && pending.Label <= previousLabel {
			if pending.Label == previousLabel {
				return fmt.Errorf("format coverage has duplicate pending label %q", pending.Label)
			}
			return errors.New("format coverage pending rows must be sorted by label")
		}
		previousLabel = pending.Label
		if err := validateCoverageStringList(pending.Extensions, "pending extension"); err != nil {
			return fmt.Errorf("pending %q: %w", pending.Label, err)
		}
		if err := validateCoverageText(pending.OwnerSlice, "owner slice", false); err != nil {
			return fmt.Errorf("pending %q: %w", pending.Label, err)
		}
		if err := validateBoundedCoverageText(pending.Note, MaxCoverageNoteBytes, "note"); err != nil {
			return fmt.Errorf("pending %q: %w", pending.Label, err)
		}
	}
	return nil
}

func validateCapabilityMap(value FormatCoverageV1, catalogID string, capabilities map[CapabilityKey]CapabilityStateV1) error {
	if len(capabilities) != len(allCapabilityKeys) {
		return fmt.Errorf("capability map must contain exactly %d entries", len(allCapabilityKeys))
	}
	for key := range capabilities {
		if !ValidCapabilityKey(key) {
			return fmt.Errorf("capability map contains unknown capability %q", key)
		}
	}
	for _, key := range allCapabilityKeys {
		state, found := capabilities[key]
		if !found {
			return fmt.Errorf("capability map is missing capability %q", key)
		}
		if !ValidCapabilityState(state.State) {
			return fmt.Errorf("capability %q has unknown state %q", key, state.State)
		}
		if err := validateBoundedCoverageText(state.Evidence, MaxCoverageEvidenceBytes, "evidence"); err != nil {
			return fmt.Errorf("capability %q: %w", key, err)
		}
		if err := validateBoundedCoverageText(state.Note, MaxCoverageNoteBytes, "note"); err != nil {
			return fmt.Errorf("capability %q: %w", key, err)
		}
		if err := validateCoverageText(state.Provider, "provider", true); err != nil {
			return fmt.Errorf("capability %q: %w", key, err)
		}
		if state.ProviderFingerprint != "" && !canonical.IsSHA256Hex(state.ProviderFingerprint) {
			return fmt.Errorf("capability %q provider fingerprint must be a lowercase SHA-256 value", key)
		}
		if (state.Provider == "") != (state.ProviderFingerprint == "") {
			return fmt.Errorf("capability %q requires both provider and provider fingerprint", key)
		}
		if state.Provider != "" && !slices.Contains(value.GeneratedBy.BoundProviders, state.ProviderFingerprint) {
			return fmt.Errorf("capability %q must name a bound provider", key)
		}
		if state.State != CapabilityQualified {
			continue
		}
		if state.Evidence == "" {
			return fmt.Errorf("qualified capability %q requires evidence", key)
		}
		if !registeredCoverageClaim(value, catalogID, key, state) {
			return fmt.Errorf("qualified capability %q evidence is not registered for format %q", key, catalogID)
		}
	}
	return nil
}

func registeredCoverageClaim(value FormatCoverageV1, catalogID string, key CapabilityKey, state CapabilityStateV1) bool {
	matches := formatqualification.LookupEvidence(formatqualification.EvidenceQuery{
		CatalogID: catalogID, Capability: formatqualification.Capability(key), Evidence: state.Evidence,
	})
	for _, qualification := range matches {
		switch {
		case state.Provider != "":
			if qualification.DescriptorFingerprint == state.ProviderFingerprint &&
				qualification.InputKind == formatqualification.InputOriginalFile {
				return true
			}
		default:
			if qualification.ImplementationID == "" || qualification.DescriptorFingerprint != "" ||
				qualification.InputKind != formatqualification.InputOriginalFile {
				continue
			}
			if key != CapabilityMetadata ||
				qualification.ImplementationID == value.GeneratedBy.ExtractorID {
				return true
			}
		}
	}
	return false
}

func validateVariants(value FormatCoverageV1, catalogID string, variants []FormatVariantCapabilityV1) error {
	for index, variant := range variants {
		if err := validateCapabilityMap(value, catalogID, variant.Capabilities); err != nil {
			return fmt.Errorf("variant %d: %w", index, err)
		}
		if index == 0 {
			continue
		}
		order, err := compareFormatVariants(variants[index-1], variant)
		if err != nil {
			return err
		}
		switch {
		case order == 0:
			return errors.New("variant rows contain an exact duplicate")
		case order > 0:
			return errors.New("variant rows must be sorted by capabilities")
		}
	}
	return nil
}

func compareFormatVariants(left, right FormatVariantCapabilityV1) (int, error) {
	leftCapabilities, err := canonical.Marshal(left.Capabilities)
	if err != nil {
		return 0, fmt.Errorf("encoding left variant capabilities: %w", err)
	}
	rightCapabilities, err := canonical.Marshal(right.Capabilities)
	if err != nil {
		return 0, fmt.Errorf("encoding right variant capabilities: %w", err)
	}
	return slices.Compare(leftCapabilities, rightCapabilities), nil
}

func validateSortedCoverageStrings(values []string, subject string, fingerprints bool) error {
	previous := ""
	for index, value := range values {
		if fingerprints {
			if !canonical.IsSHA256Hex(value) {
				return fmt.Errorf("format coverage %s must be a lowercase SHA-256 value", subject)
			}
		} else if err := validateCoverageText(value, subject, false); err != nil {
			return err
		}
		if index > 0 && value <= previous {
			if value == previous {
				return fmt.Errorf("format coverage has duplicate %s %q", subject, value)
			}
			return fmt.Errorf("format coverage %ss must be sorted", subject)
		}
		previous = value
	}
	return nil
}

func validateCoverageStringList(values []string, subject string) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if err := validateCoverageText(value, subject, false); err != nil {
			return err
		}
		if seen[value] {
			return fmt.Errorf("duplicate %s %q", subject, value)
		}
		seen[value] = true
	}
	return nil
}

func validateCoverageText(value, subject string, emptyAllowed bool) error {
	if !utf8.ValidString(value) || len(value) > maxCoverageLabelBytes || (!emptyAllowed && value == "") {
		return fmt.Errorf("%s must be bounded UTF-8", subject)
	}
	return nil
}

func validateBoundedCoverageText(value string, maximum int, subject string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be UTF-8", subject)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", subject, maximum)
	}
	return nil
}
