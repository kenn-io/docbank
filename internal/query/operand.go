package query

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValidateTextOperand applies the QueryV1 filter rules to an expression operand.
func ValidateTextOperand(field, value string) error {
	var valid bool
	switch field {
	case "xmp":
		valid = oneOf(strings.ToLower(value), "pending", "conflict")
	case "flag":
		valid = ValidPhotoFlag(value)
	case "label":
		valid = ValidPhotoColorLabel(value)
	case "kind":
		valid = oneOf(value, "photo", "video")
	case "camera", "lens", "location":
		valid = validPhotoLabel(value)
	case "asset", "set":
		valid = validUUIDv4(value)
	case "path":
		valid = validVirtualPath(value)
	case "mime":
		valid = validConcreteMIME(value)
	case "extension":
		valid = extensionPattern.MatchString(value)
	case "media_family":
		valid = validMediaFamily(value)
	default:
		return fmt.Errorf("unsupported text field %q", field)
	}
	if !utf8.ValidString(value) || !valid {
		return fmt.Errorf("invalid %s operand", field)
	}
	return nil
}

// ParseSizeOperand reads a decimal operand within the QueryV1 size bounds.
func ParseSizeOperand(value string) (int64, error) {
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, errors.New("size operand must be a decimal nonnegative safe integer")
	}
	bound, err := strconv.ParseInt(value, 10, 64)
	if err != nil || !validSizeBound(bound) {
		return 0, errors.New("size operand must be a decimal nonnegative safe integer")
	}
	return bound, nil
}

func validSizeBound(value int64) bool { return value >= 0 && value <= maxSafeInteger }

var pdfFieldKinds = map[string]string{
	"pdf_encrypted":                  "boolean",
	"pdf_last_signature_covers_file": "boolean",
	"pdf_has_javascript":             "boolean",
	"pdf_has_launch_actions":         "boolean",
	"pdf_permissions":                "string_list",
	"pdf_signature_field_count":      "integer",
	"pdf_signature_field_count_min":  "integer",
	"pdf_signature_field_count_max":  "integer",
	"pdf_embedded_file_count":        "integer",
	"pdf_embedded_file_count_min":    "integer",
	"pdf_embedded_file_count_max":    "integer",
}

// PDFFieldKind reports the recorded value kind for a PDF query field.
func PDFFieldKind(field string) string { return pdfFieldKinds[field] }

// PDFFieldComparison separates a fact field from its optional range bound.
func PDFFieldComparison(field string) (string, string) {
	if base, ok := strings.CutSuffix(field, "_min"); ok {
		return base, ">="
	}
	if base, ok := strings.CutSuffix(field, "_max"); ok {
		return base, "<="
	}
	return field, "="
}

func withPDFFields(fields map[string]struct{}) map[string]struct{} {
	for field := range pdfFieldKinds {
		fields[field] = struct{}{}
	}
	return fields
}

// ParsePDFOperand validates a typed PDF fact operand.
func ParsePDFOperand(field, value string) (any, error) {
	switch PDFFieldKind(field) {
	case "integer":
		return ParseSizeOperand(value)
	case "boolean":
		if value == "true" {
			return true, nil
		}
		if value == "false" {
			return false, nil
		}
	case "string_list":
		if utf8.ValidString(value) && value != "" && len(value) <= 256 {
			return value, nil
		}
	}
	return nil, fmt.Errorf("invalid %s operand", field)
}
