// Package bridgeprofile contains the shared mechanics for fixed bridge
// compatibility profiles. Provider packages retain their own public contracts
// and policy validation while this package owns canonical encoding, identity
// validation, and the common broad-format intersection.
package bridgeprofile

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	canonicaljson "go.kenn.io/docbank/internal/canonical"
)

// Codec canonicalizes one provider's profile without coupling its public
// schema or validation rules to another provider.
type Codec[T any] struct {
	Prefix      string
	Clone       func(T) T
	Normalize   func(*T)
	Fingerprint func(*T) *string
	Validate    func(T) error
}

// Canonical validates and encodes a profile with its content fingerprint.
func (codec Codec[T]) Canonical(profile T) ([]byte, string, error) {
	canonical := codec.Clone(profile)
	codec.Normalize(&canonical)
	claimed := *codec.Fingerprint(&canonical)
	*codec.Fingerprint(&canonical) = ""
	if err := codec.Validate(canonical); err != nil {
		return nil, "", fmt.Errorf("%s: invalid profile: %w", codec.Prefix, err)
	}
	identityJSON, err := canonicaljson.Marshal(canonical)
	if err != nil {
		return nil, "", fmt.Errorf("%s: encode profile identity: %w", codec.Prefix, err)
	}
	fingerprint := providerutil.SHA256Hex(identityJSON)
	if claimed != "" && claimed != fingerprint {
		return nil, "", fmt.Errorf("%s: policy fingerprint does not match canonical profile", codec.Prefix)
	}
	*codec.Fingerprint(&canonical) = fingerprint
	encoded, err := canonicaljson.Marshal(canonical)
	if err != nil {
		return nil, "", fmt.Errorf("%s: encode canonical profile: %w", codec.Prefix, err)
	}
	return encoded, fingerprint, nil
}

// Parse accepts only the exact canonical representation produced by Canonical.
func (codec Codec[T]) Parse(raw []byte) (T, error) {
	profile, err := canonicaljson.DecodeWith(raw, func(value T) ([]byte, error) {
		encoded, _, encodeErr := codec.Canonical(value)
		return encoded, encodeErr
	})
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%s: decode profile: %w", codec.Prefix, err)
	}
	return codec.Clone(profile), nil
}

// ValidateIdentity accepts a bounded ASCII identifier. Runtime identities may
// opt into ':' so they can use forms such as sha256:<digest>.
func ValidateIdentity(value, subject string, allowColon bool, maximum int) error {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is invalid", subject)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._-", character) ||
			allowColon && character == ':' {
			continue
		}
		return fmt.Errorf("%s is invalid", subject)
	}
	return nil
}

// BroadOriginalFormats returns the locally inspectable original-file format
// intersection shared by broad-format bridge profiles.
func BroadOriginalFormats() []document.RenditionFormatCapability {
	formats := []document.RenditionFormatCapability{
		{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "application/vnd.oasis.opendocument.spreadsheet", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "text/csv", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "ebook", MediaType: "application/epub+zip", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "mail", MediaType: "message/rfc822", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "structured", MediaType: "application/xml", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/markdown", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile},
	}
	slices.SortFunc(formats, CompareFormats)
	return formats
}

// CompareFormats orders format capabilities by their canonical tuple.
func CompareFormats(left, right document.RenditionFormatCapability) int {
	if comparison := strings.Compare(left.MediaFamily, right.MediaFamily); comparison != 0 {
		return comparison
	}
	if comparison := strings.Compare(left.MediaType, right.MediaType); comparison != 0 {
		return comparison
	}
	return strings.Compare(string(left.InputKind), string(right.InputKind))
}
