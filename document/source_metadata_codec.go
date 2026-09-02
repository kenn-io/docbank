package document

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"golang.org/x/text/unicode/norm"
)

var sourceMetadataKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
var sourceMetadataOffsetPattern = regexp.MustCompile(`^[+-](0[0-9]|1[0-4]):[0-5][0-9]$`)

var sourceMetadataCommonKeys = map[string]bool{
	"attachment_count": true, "calendar.end": true, "calendar.start": true,
	"created": true, "creators": true, "description": true,
	"email.bcc": true, "email.cc": true, "email.from": true,
	"email.received": true, "email.sent": true, "email.subject": true,
	"email.to": true, "keywords": true, "language": true, "modified": true,
	"page_count": true, "subject": true, "title": true,
}

var sourceMetadataNamespaces = []string{
	"calendar.", "email.", "image.exif.", "image.iptc.", "image.xmp.",
	"media.container.", "media.id3.", "office.core.", "office.custom.",
	"pdf.info.", "xmp.",
}

var sourceMetadataSourceNamespaces = map[string]bool{
	"calendar": true, "email": true, "image.exif": true, "image.iptc": true,
	"image.xmp": true, "media.container": true, "media.id3": true,
	"office.core": true, "office.custom": true, "pdf.info": true, "xmp": true,
}

var forbiddenSourceMetadataSegments = map[string]bool{
	"bates": true, "bates_start": true, "bates_end": true, "collection_processing_timezone": true,
	"custodian": true, "duplicate_path": true, "extension": true, "family_date": true,
	"filename": true, "filesystem_mtime": true, "ingest_time": true,
	"produced_document_link": true, "produced_text_link": true, "production_volume": true,
	"redaction_state": true, "source_path": true,
}

// SourceMetadataCanonicalKeyAllowed reports whether a key belongs to the
// content-scoped contract rather than attachment/provenance or legal workflow.
func SourceMetadataCanonicalKeyAllowed(key string) bool {
	if len(key) > MaxSourceMetadataLabelBytes || !sourceMetadataKeyPattern.MatchString(key) {
		return false
	}
	for segment := range strings.SplitSeq(key, ".") {
		if forbiddenSourceMetadataSegments[segment] {
			return false
		}
	}
	if sourceMetadataCommonKeys[key] {
		return true
	}
	for _, prefix := range sourceMetadataNamespaces {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// MarshalSourceMetadataV1 validates, canonicalizes, and hashes one record.
func MarshalSourceMetadataV1(value SourceMetadataV1) ([]byte, string, error) {
	record, err := canonicalSourceMetadataV1(value)
	if err != nil {
		return nil, "", err
	}
	encoded, err := canonical.Marshal(record)
	if err != nil {
		return nil, "", fmt.Errorf("encoding source metadata: %w", err)
	}
	if len(encoded) > MaxSourceMetadataEncodedBytes {
		return nil, "", errors.New("source metadata record is too large")
	}
	return encoded, sha256Hex(encoded), nil
}

// DecodeSourceMetadataV1 accepts the exact v1 typed schema and returns its
// canonical value and checksum. Unknown members and major contracts fail.
func DecodeSourceMetadataV1(encoded []byte) (SourceMetadataV1, string, error) {
	if len(encoded) > MaxSourceMetadataEncodedBytes {
		return SourceMetadataV1{}, "", errors.New("source metadata record is too large")
	}
	value, err := canonical.Decode[SourceMetadataV1](encoded)
	if err != nil {
		return SourceMetadataV1{}, "", fmt.Errorf("decoding source metadata: %w", err)
	}
	record, checksum, err := MarshalSourceMetadataV1(value)
	if err != nil {
		return SourceMetadataV1{}, "", err
	}
	if !bytes.Equal(encoded, record) {
		return SourceMetadataV1{}, "", errors.New("source metadata bytes are not canonical")
	}
	return value, checksum, nil
}

func canonicalSourceMetadataV1(value SourceMetadataV1) (SourceMetadataV1, error) {
	if value.ContractVersion != SourceMetadataContractV1 {
		return SourceMetadataV1{}, fmt.Errorf(
			"source metadata contract version must be %q", SourceMetadataContractV1)
	}
	if len(value.Fields) > MaxSourceMetadataFields {
		return SourceMetadataV1{}, errors.New("source metadata has too many fields")
	}
	if len(value.Warnings) > MaxSourceMetadataWarnings {
		return SourceMetadataV1{}, errors.New("source metadata has too many warnings")
	}
	value.Fields = append([]SourceMetadataFieldV1(nil), value.Fields...)
	seen := make(map[string]bool, len(value.Fields))
	for index := range value.Fields {
		field := &value.Fields[index]
		field.Key = norm.NFC.String(strings.TrimSpace(field.Key))
		field.Namespace = norm.NFC.String(strings.TrimSpace(field.Namespace))
		field.SourceField = norm.NFC.String(strings.TrimSpace(field.SourceField))
		if !SourceMetadataCanonicalKeyAllowed(field.Key) {
			return SourceMetadataV1{}, fmt.Errorf("source metadata field %d has forbidden canonical key %q", index, field.Key)
		}
		if seen[field.Key] {
			return SourceMetadataV1{}, fmt.Errorf("source metadata has duplicate canonical key %q", field.Key)
		}
		seen[field.Key] = true
		if err := validateSourceMetadataLabel(field.Namespace, "namespace"); err != nil {
			return SourceMetadataV1{}, fmt.Errorf("source metadata field %q: %w", field.Key, err)
		}
		if !sourceMetadataSourceNamespaces[field.Namespace] {
			return SourceMetadataV1{}, fmt.Errorf("source metadata field %q has unknown namespace %q", field.Key, field.Namespace)
		}
		if err := validateSourceMetadataLabel(field.SourceField, "source field"); err != nil {
			return SourceMetadataV1{}, fmt.Errorf("source metadata field %q: %w", field.Key, err)
		}
		if err := canonicalSourceMetadataValue(&field.Value); err != nil {
			return SourceMetadataV1{}, fmt.Errorf("source metadata field %q: %w", field.Key, err)
		}
	}
	slices.SortFunc(value.Fields, func(a, b SourceMetadataFieldV1) int {
		return strings.Compare(a.Key, b.Key)
	})
	value.Warnings = append([]SourceMetadataWarningV1(nil), value.Warnings...)
	for index := range value.Warnings {
		warning := &value.Warnings[index]
		warning.Code = norm.NFC.String(strings.TrimSpace(warning.Code))
		warning.Namespace = norm.NFC.String(strings.TrimSpace(warning.Namespace))
		warning.SourceField = norm.NFC.String(strings.TrimSpace(warning.SourceField))
		warning.Detail = norm.NFC.String(warning.Detail)
		for label, name := range map[string]string{
			warning.Code: "warning code", warning.Namespace: "warning namespace",
			warning.SourceField: "warning source field",
		} {
			if err := validateSourceMetadataLabel(label, name); err != nil {
				return SourceMetadataV1{}, fmt.Errorf("source metadata warning %d: %w", index, err)
			}
		}
		if err := validateSourceMetadataString(warning.Detail); err != nil {
			return SourceMetadataV1{}, fmt.Errorf("source metadata warning %d detail: %w", index, err)
		}
	}
	slices.SortFunc(value.Warnings, func(a, b SourceMetadataWarningV1) int {
		return strings.Compare(a.Namespace+"\x00"+a.SourceField+"\x00"+a.Code+"\x00"+a.Detail,
			b.Namespace+"\x00"+b.SourceField+"\x00"+b.Code+"\x00"+b.Detail)
	})
	if value.Fields == nil {
		value.Fields = []SourceMetadataFieldV1{}
	}
	if value.Warnings == nil {
		value.Warnings = []SourceMetadataWarningV1{}
	}
	return value, nil
}

func validateSourceMetadataLabel(value, name string) error {
	if value == "" || len(value) > MaxSourceMetadataLabelBytes || !utf8.ValidString(value) {
		return fmt.Errorf("%s must be bounded UTF-8", name)
	}
	return nil
}

func validateSourceMetadataString(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("value must be UTF-8")
	}
	if len(value) > MaxSourceMetadataValueBytes {
		return errors.New("value is too large")
	}
	return nil
}

func canonicalSourceMetadataValue(value *SourceMetadataValueV1) error {
	payloads := 0
	if value.String != nil {
		payloads++
	}
	if value.Strings != nil {
		payloads++
	}
	if value.Integer != nil {
		payloads++
	}
	if value.Number != nil {
		payloads++
	}
	if value.Boolean != nil {
		payloads++
	}
	if value.Timestamp != nil {
		payloads++
	}
	switch value.Kind {
	case SourceMetadataString:
		if payloads != 1 || value.String == nil {
			return errors.New("string value has conflicting payloads")
		}
		value.String = new(norm.NFC.String(*value.String))
		return validateSourceMetadataString(*value.String)
	case SourceMetadataStringList:
		if payloads != 1 || value.Strings == nil {
			return errors.New("string-list value has conflicting payloads")
		}
		if len(value.Strings) > MaxSourceMetadataListValues {
			return errors.New("string-list value has too many entries")
		}
		value.Strings = slices.Clone(value.Strings)
		for index := range value.Strings {
			value.Strings[index] = norm.NFC.String(value.Strings[index])
			if err := validateSourceMetadataString(value.Strings[index]); err != nil {
				return fmt.Errorf("string-list entry %d: %w", index, err)
			}
		}
		return nil
	case SourceMetadataInteger:
		if payloads != 1 || value.Integer == nil {
			return errors.New("integer value has conflicting payloads")
		}
		return nil
	case SourceMetadataNumber:
		if payloads != 1 || value.Number == nil {
			return errors.New("number value has conflicting payloads")
		}
		if math.IsNaN(*value.Number) || math.IsInf(*value.Number, 0) {
			return errors.New("number value must be finite")
		}
		return nil
	case SourceMetadataBoolean:
		if payloads != 1 || value.Boolean == nil {
			return errors.New("boolean value has conflicting payloads")
		}
		return nil
	case SourceMetadataTimestamp:
		if payloads != 1 || value.Timestamp == nil {
			return errors.New("timestamp value has conflicting payloads")
		}
		return canonicalSourceMetadataTimestamp(value.Timestamp)
	default:
		return fmt.Errorf("unknown source metadata value kind %q", value.Kind)
	}
}

func canonicalSourceMetadataTimestamp(value *SourceMetadataTimestampV1) error {
	value.Raw = norm.NFC.String(value.Raw)
	value.Normalized = norm.NFC.String(value.Normalized)
	if err := validateSourceMetadataString(value.Raw); err != nil || value.Raw == "" {
		if err == nil {
			err = errors.New("value is empty")
		}
		return fmt.Errorf("raw timestamp: %w", err)
	}
	if err := validateSourceMetadataString(value.Normalized); err != nil || value.Normalized == "" {
		if err == nil {
			err = errors.New("value is empty")
		}
		return fmt.Errorf("normalized timestamp: %w", err)
	}
	if _, ok := sourceMetadataPrecisionLayouts[value.Precision]; !ok {
		return errors.New("timestamp precision is invalid")
	}
	switch value.Timezone {
	case SourceMetadataTimezoneOmitted:
		if value.Offset != "" || strings.HasSuffix(value.Normalized, "Z") ||
			hasRFC3339Offset(value.Normalized) {
			return errors.New("timestamp timezone omission conflicts with normalized value")
		}
		return validateTimestampPrecision(value.Normalized, value.Precision, false)
	case SourceMetadataTimezoneUTC:
		if value.Offset != "" || !strings.HasSuffix(value.Normalized, "Z") {
			return errors.New("UTC timestamp timezone is inconsistent")
		}
	case SourceMetadataTimezoneOffset:
		if !sourceMetadataOffsetPattern.MatchString(value.Offset) ||
			!strings.HasSuffix(value.Normalized, value.Offset) {
			return errors.New("timestamp timezone offset is inconsistent")
		}
	default:
		return errors.New("timestamp timezone is invalid")
	}
	if value.Precision == SourceMetadataPrecisionDate || value.Precision == SourceMetadataPrecisionHour {
		return errors.New("timestamp timezone requires minute or finer precision")
	}
	return validateTimestampPrecision(value.Normalized, value.Precision, true)
}

func hasRFC3339Offset(value string) bool {
	return len(value) >= 6 && (value[len(value)-6] == '+' || value[len(value)-6] == '-') &&
		value[len(value)-3] == ':'
}

var sourceMetadataPrecisionLayouts = map[SourceMetadataTimestampPrecision]string{
	SourceMetadataPrecisionDate:     "2006-01-02",
	SourceMetadataPrecisionHour:     "2006-01-02T15",
	SourceMetadataPrecisionMinute:   "2006-01-02T15:04",
	SourceMetadataPrecisionSecond:   "2006-01-02T15:04:05",
	SourceMetadataPrecisionFraction: "2006-01-02T15:04:05.999999999",
}

// validateTimestampPrecision parses the normalized value with the exact
// layout for its precision. time.Parse tolerates a fractional second after any
// seconds field, so the fraction's presence is checked separately.
func validateTimestampPrecision(
	value string, precision SourceMetadataTimestampPrecision, zoned bool,
) error {
	layout := sourceMetadataPrecisionLayouts[precision]
	base := value
	if zoned {
		layout += "Z07:00"
		base = strings.TrimSuffix(value, "Z")
		if hasRFC3339Offset(value) {
			base = value[:len(value)-6]
		}
	}
	if _, err := time.Parse(layout, value); err != nil {
		return fmt.Errorf("timestamp does not match %s precision: %w", precision, err)
	}
	if (precision == SourceMetadataPrecisionFraction) != strings.Contains(base, ".") {
		return errors.New("timestamp fraction does not match precision")
	}
	return nil
}
