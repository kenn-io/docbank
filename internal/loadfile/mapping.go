package loadfile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	MappingContractV1 = "loadfile-mapping/v1"
	MaxMappingBytes   = 256 << 10
)

var (
	ErrInvalidMapping   = errors.New("invalid_package_mapping: mapping document is malformed or out of catalog")
	ErrMappingAmbiguous = errors.New("package_mapping_ambiguous: mapping requires an explicit choice")
)

type MappingColumn struct {
	Source            string  `json:"source"`
	SourceOrdinal     *int    `json:"source_ordinal,omitempty"`
	Sensitive         bool    `json:"sensitive"`
	PairedDateOrdinal *int    `json:"paired_date_ordinal,omitempty"`
	Canonical         *string `json:"canonical"`
	DateFormat        string  `json:"date_format,omitzero"`
	Timezone          string  `json:"timezone,omitzero"`
	MultiValue        bool    `json:"multi_value,omitzero"`
}

type Mapping struct {
	Contract         string            `json:"contract"`
	Columns          []MappingColumn   `json:"columns"`
	CustodianColumn  string            `json:"custodian_column,omitzero"`
	DefaultCustodian string            `json:"default_custodian,omitzero"`
	VolumeRoots      map[string]string `json:"volume_roots,omitzero"`
}

func DecodeMapping(raw []byte, columns []string) (Mapping, string, error) {
	if len(raw) > MaxMappingBytes {
		return Mapping{}, "", invalidMapping("document is %d bytes; allowed %d", len(raw), MaxMappingBytes)
	}

	var mapping Mapping
	if err := json.Unmarshal(raw, &mapping, json.RejectUnknownMembers(true)); err != nil {
		return Mapping{}, "", invalidMapping("decoding JSON: %v", err)
	}
	if mapping.Contract != MappingContractV1 {
		return Mapping{}, "", invalidMapping("unsupported contract %q", mapping.Contract)
	}

	ordinals := make(map[string][]int, len(columns))
	for ordinal, column := range columns {
		ordinals[column] = append(ordinals[column], ordinal)
	}
	claimed := make([]bool, len(columns))
	targets := make([]string, len(columns))
	singleValueTargets := make(map[string]bool)
	for index := range mapping.Columns {
		column := &mapping.Columns[index]
		matches := ordinals[column.Source]
		if len(matches) == 0 {
			return Mapping{}, "", invalidMapping("column %d names absent source %q", index, column.Source)
		}

		var ordinal int
		switch {
		case column.SourceOrdinal == nil && len(matches) != 1:
			return Mapping{}, "", invalidMapping("column %d source %q is ambiguous without source_ordinal", index, column.Source)
		case column.SourceOrdinal == nil:
			ordinal = matches[0]
			column.SourceOrdinal = new(ordinal)
		case *column.SourceOrdinal < 0 || *column.SourceOrdinal >= len(columns):
			return Mapping{}, "", invalidMapping("column %d source_ordinal %d is out of range", index, *column.SourceOrdinal)
		case columns[*column.SourceOrdinal] != column.Source:
			return Mapping{}, "", invalidMapping("column %d source_ordinal %d names %q, not %q", index, *column.SourceOrdinal, columns[*column.SourceOrdinal], column.Source)
		default:
			ordinal = *column.SourceOrdinal
		}
		if claimed[ordinal] {
			return Mapping{}, "", invalidMapping("source_ordinal %d is claimed more than once", ordinal)
		}
		claimed[ordinal] = true

		if column.Canonical != nil && !FieldCatalogKeyAllowed(*column.Canonical) {
			return Mapping{}, "", invalidMapping("column %d has unknown canonical target %q", index, *column.Canonical)
		}
		if column.Canonical != nil {
			if err := claimSingleValueTarget(*column.Canonical, singleValueTargets); err != nil {
				return Mapping{}, "", err
			}
			targets[ordinal] = *column.Canonical
		}
		if column.DateFormat != "" && declaredDateLayout(column.DateFormat) == "" {
			return Mapping{}, "", invalidMapping("column %d has unsupported date_format %q", index, column.DateFormat)
		}
		if _, offset := parseOffset(column.Timezone); column.Timezone != "" && column.Timezone != "Z" && !offset && !validIANAName(column.Timezone) {
			return Mapping{}, "", invalidMapping("column %d has invalid timezone %q", index, column.Timezone)
		}
		if column.PairedDateOrdinal != nil && (*column.PairedDateOrdinal < 0 || *column.PairedDateOrdinal >= len(columns)) {
			return Mapping{}, "", invalidMapping("column %d paired_date_ordinal %d is out of range", index, *column.PairedDateOrdinal)
		}
	}

	for index, column := range mapping.Columns {
		if column.PairedDateOrdinal == nil {
			continue
		}
		paired := *column.PairedDateOrdinal
		if paired == *column.SourceOrdinal || !strings.HasPrefix(targets[*column.SourceOrdinal], "loadfile.time.") || !strings.HasPrefix(targets[paired], "loadfile.date.") {
			return Mapping{}, "", invalidMapping("column %d must pair a time target with a separate mapped date column", index)
		}
	}

	if mapping.CustodianColumn != "" && len(ordinals[mapping.CustodianColumn]) != 1 {
		return Mapping{}, "", invalidMapping("custodian_column %q must name one unambiguous source column", mapping.CustodianColumn)
	}
	for volume, root := range mapping.VolumeRoots {
		if volume == "" || !portableRelativeRoot(root) {
			return Mapping{}, "", invalidMapping("volume root %q=%q is not a portable relative path", volume, root)
		}
	}

	encoded, err := canonical.Marshal(mapping)
	if err != nil {
		return Mapping{}, "", invalidMapping("encoding canonical mapping: %v", err)
	}
	digest := sha256.Sum256(encoded)
	return mapping, hex.EncodeToString(digest[:]), nil
}

func claimSingleValueTarget(target string, claimed map[string]bool) error {
	switch target {
	case "loadfile.document.id", "loadfile.family.parent", "loadfile.family.id":
		if claimed[target] {
			return fmt.Errorf("%w: canonical target %q is claimed more than once", ErrMappingAmbiguous, target)
		}
		claimed[target] = true
	}
	return nil
}

func portableRelativeRoot(root string) bool {
	if root == "" || strings.IndexByte(root, 0) >= 0 {
		return false
	}
	normalized := strings.ReplaceAll(root, `\`, "/")
	if strings.HasPrefix(normalized, "/") || strings.Contains(normalized, ":") {
		return false
	}
	for element := range strings.SplitSeq(normalized, "/") {
		if element == "" || element == "." || element == ".." ||
			strings.HasSuffix(element, ".") || strings.HasSuffix(element, " ") {
			return false
		}
	}
	return true
}

func invalidMapping(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMapping, fmt.Sprintf(format, args...))
}
