// Package query implements the bounded, canonical saved-query contracts shared
// by storage, HTTP, and the browser client. It does not execute queries.
package query

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	// MaxInputBytes is the largest raw QueryV1 or HighlightSetV1 JSON input.
	MaxInputBytes     = 128 << 10
	maxCanonicalBytes = 64 << 10
	maxJSONDepth      = 16
	maxTextRunes      = 8192
	maxIDValues       = 64
	maxExtensions     = 32
	maxSafeInteger    = int64(9007199254740991)
)

var (
	uuidV4Pattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	extensionPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,31})$`)
	rfc3339NanoPattern = regexp.MustCompile(
		`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})$`,
	)
)

// Query is a normalized QueryV1 value. Text is retained exactly as entered.
type Query struct {
	V       int     `json:"v"`
	Text    string  `json:"text"`
	Syntax  string  `json:"syntax"`
	Mode    string  `json:"mode"`
	Filters Filters `json:"filters"`
	Sort    Sort    `json:"sort"`
}

// Sort identifies the requested result ordering. Execution support is decided
// by the later query executor.
type Sort struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

// Filters contains the complete set of typed QueryV1 facets. Slice fields are
// set-valued after normalization.
type Filters struct {
	Paths                []string `json:"paths,omitempty"`
	ExcludePaths         []string `json:"exclude_paths,omitempty"`
	CollectionIDs        []string `json:"collection_ids,omitempty"`
	ExcludeCollectionIDs []string `json:"exclude_collection_ids,omitempty"`
	TagIDs               []string `json:"tag_ids,omitempty"`
	ExcludeTagIDs        []string `json:"exclude_tag_ids,omitempty"`
	NoTags               bool     `json:"no_tags,omitzero"`
	MediaFamilies        []string `json:"media_families,omitempty"`
	MIMETypes            []string `json:"mime_types,omitempty"`
	Extensions           []string `json:"extensions,omitempty"`
	ModifiedAfter        string   `json:"modified_after,omitzero"`
	ModifiedBefore       string   `json:"modified_before,omitzero"`
	SizeMin              int64    `json:"size_min,omitzero"`
	SizeMax              int64    `json:"size_max,omitzero"`
	TextCoverage         []string `json:"text_coverage,omitempty"`
	HasDuplicates        bool     `json:"has_duplicates,omitzero"`
	CollapseDuplicates   bool     `json:"collapse_duplicates,omitzero"`
}

type queryInput struct {
	V       *int          `json:"v"`
	Text    *string       `json:"text"`
	Syntax  *string       `json:"syntax"`
	Mode    *string       `json:"mode"`
	Filters *filtersInput `json:"filters"`
	Sort    *sortInput    `json:"sort"`
}

type sortInput struct {
	Field     *string `json:"field"`
	Direction *string `json:"direction"`
}

type filtersInput struct {
	Paths                *[]string `json:"paths"`
	ExcludePaths         *[]string `json:"exclude_paths"`
	CollectionIDs        *[]string `json:"collection_ids"`
	ExcludeCollectionIDs *[]string `json:"exclude_collection_ids"`
	TagIDs               *[]string `json:"tag_ids"`
	ExcludeTagIDs        *[]string `json:"exclude_tag_ids"`
	NoTags               *bool     `json:"no_tags"`
	MediaFamilies        *[]string `json:"media_families"`
	MIMETypes            *[]string `json:"mime_types"`
	Extensions           *[]string `json:"extensions"`
	ModifiedAfter        *string   `json:"modified_after"`
	ModifiedBefore       *string   `json:"modified_before"`
	SizeMin              *int64    `json:"size_min"`
	SizeMax              *int64    `json:"size_max"`
	TextCoverage         *[]string `json:"text_coverage"`
	HasDuplicates        *bool     `json:"has_duplicates"`
	CollapseDuplicates   *bool     `json:"collapse_duplicates"`
}

var optionalFilterFields = map[string]struct{}{
	"paths": {}, "exclude_paths": {}, "collection_ids": {}, "exclude_collection_ids": {},
	"tag_ids": {}, "exclude_tag_ids": {}, "no_tags": {}, "media_families": {},
	"mime_types": {}, "extensions": {}, "modified_after": {}, "modified_before": {},
	"size_min": {}, "size_max": {}, "text_coverage": {}, "has_duplicates": {},
	"collapse_duplicates": {},
}

// Parse validates and normalizes one bounded QueryV1 JSON value.
func Parse(raw []byte) (Query, error) {
	if err := preflightJSON(raw, true); err != nil {
		return Query{}, fmt.Errorf("parse query: %w", err)
	}
	var input queryInput
	if err := json.Unmarshal(raw, &input, json.RejectUnknownMembers(true), json.MatchCaseInsensitiveNames(false)); err != nil {
		return Query{}, fmt.Errorf("parse query: %w", err)
	}
	value := Query{V: 1, Syntax: "simple", Mode: "lexical", Sort: Sort{Field: "name", Direction: "asc"}}
	if input.V != nil {
		value.V = *input.V
	}
	if input.Text != nil {
		value.Text = *input.Text
	}
	if input.Syntax != nil {
		value.Syntax = *input.Syntax
	}
	if input.Mode != nil {
		value.Mode = *input.Mode
	}
	if input.Filters != nil {
		if input.Filters.ModifiedAfter != nil && *input.Filters.ModifiedAfter == "" ||
			input.Filters.ModifiedBefore != nil && *input.Filters.ModifiedBefore == "" {
			return Query{}, errors.New("query timestamps must be nonempty RFC3339 values")
		}
		value.Filters = input.Filters.value()
	}
	if input.Sort != nil {
		if input.Sort.Field != nil {
			value.Sort.Field = *input.Sort.Field
		}
		if input.Sort.Direction != nil {
			value.Sort.Direction = *input.Sort.Direction
		}
	}
	normalized, err := normalizeQuery(value)
	if err != nil {
		return Query{}, err
	}
	if _, err := Canonical(normalized); err != nil {
		return Query{}, err
	}
	return normalized, nil
}

// Canonical validates value and returns its one canonical QueryV1 encoding.
func Canonical(value Query) ([]byte, error) {
	normalized, err := normalizeQuery(value)
	if err != nil {
		return nil, err
	}
	encoded, err := canonical.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("encode query: %w", err)
	}
	if len(encoded) > maxCanonicalBytes {
		return nil, errors.New("canonical query exceeds 64 KiB")
	}
	return encoded, nil
}

// Fingerprint returns the domain-neutral SHA-256 identity of canonical query bytes.
func Fingerprint(value Query) (string, error) {
	encoded, err := Canonical(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (input filtersInput) value() Filters {
	value := Filters{}
	if input.Paths != nil {
		value.Paths = *input.Paths
	}
	if input.ExcludePaths != nil {
		value.ExcludePaths = *input.ExcludePaths
	}
	if input.CollectionIDs != nil {
		value.CollectionIDs = *input.CollectionIDs
	}
	if input.ExcludeCollectionIDs != nil {
		value.ExcludeCollectionIDs = *input.ExcludeCollectionIDs
	}
	if input.TagIDs != nil {
		value.TagIDs = *input.TagIDs
	}
	if input.ExcludeTagIDs != nil {
		value.ExcludeTagIDs = *input.ExcludeTagIDs
	}
	if input.NoTags != nil {
		value.NoTags = *input.NoTags
	}
	if input.MediaFamilies != nil {
		value.MediaFamilies = *input.MediaFamilies
	}
	if input.MIMETypes != nil {
		value.MIMETypes = *input.MIMETypes
	}
	if input.Extensions != nil {
		value.Extensions = *input.Extensions
	}
	if input.ModifiedAfter != nil {
		value.ModifiedAfter = *input.ModifiedAfter
	}
	if input.ModifiedBefore != nil {
		value.ModifiedBefore = *input.ModifiedBefore
	}
	if input.SizeMin != nil {
		value.SizeMin = *input.SizeMin
	}
	if input.SizeMax != nil {
		value.SizeMax = *input.SizeMax
	}
	if input.TextCoverage != nil {
		value.TextCoverage = *input.TextCoverage
	}
	if input.HasDuplicates != nil {
		value.HasDuplicates = *input.HasDuplicates
	}
	if input.CollapseDuplicates != nil {
		value.CollapseDuplicates = *input.CollapseDuplicates
	}
	return value
}

func normalizeQuery(value Query) (Query, error) {
	if value.V != 1 {
		return Query{}, errors.New("query version must be 1")
	}
	if !utf8.ValidString(value.Text) || utf8.RuneCountInString(value.Text) > maxTextRunes {
		return Query{}, errors.New("query text is not valid bounded Unicode")
	}
	if !oneOf(value.Syntax, "simple", "advanced") {
		return Query{}, errors.New("query syntax is unknown")
	}
	if !oneOf(value.Mode, "lexical", "semantic", "hybrid") {
		return Query{}, errors.New("query mode is unknown")
	}
	if !oneOf(value.Sort.Field, "name", "path", "modified_at", "size", "media_type", "relevance") ||
		!oneOf(value.Sort.Direction, "asc", "desc") {
		return Query{}, errors.New("query sort is invalid")
	}
	filters, err := normalizeFilters(value.Filters)
	if err != nil {
		return Query{}, err
	}
	value.Filters = filters
	return value, nil
}

func normalizeFilters(value Filters) (Filters, error) {
	var err error
	if value.Paths, err = normalizeSet(value.Paths, maxIDValues, validVirtualPath, "paths"); err != nil {
		return Filters{}, err
	}
	if value.ExcludePaths, err = normalizeSet(value.ExcludePaths, maxIDValues, validVirtualPath, "exclude_paths"); err != nil {
		return Filters{}, err
	}
	if value.CollectionIDs, err = normalizeSet(value.CollectionIDs, maxIDValues, validUUIDv4, "collection_ids"); err != nil {
		return Filters{}, err
	}
	if value.ExcludeCollectionIDs, err = normalizeSet(value.ExcludeCollectionIDs, maxIDValues, validUUIDv4, "exclude_collection_ids"); err != nil {
		return Filters{}, err
	}
	if value.TagIDs, err = normalizeSet(value.TagIDs, maxIDValues, validUUIDv4, "tag_ids"); err != nil {
		return Filters{}, err
	}
	if value.ExcludeTagIDs, err = normalizeSet(value.ExcludeTagIDs, maxIDValues, validUUIDv4, "exclude_tag_ids"); err != nil {
		return Filters{}, err
	}
	if value.MediaFamilies, err = normalizeSet(value.MediaFamilies, 13, validMediaFamily, "media_families"); err != nil {
		return Filters{}, err
	}
	if value.MIMETypes, err = normalizeSet(value.MIMETypes, maxIDValues, validConcreteMIME, "mime_types"); err != nil {
		return Filters{}, err
	}
	if value.Extensions, err = normalizeSet(value.Extensions, maxExtensions, extensionPattern.MatchString, "extensions"); err != nil {
		return Filters{}, err
	}
	if value.TextCoverage, err = normalizeSet(value.TextCoverage, 6, validTextCoverage, "text_coverage"); err != nil {
		return Filters{}, err
	}
	if value.NoTags && len(value.TagIDs) != 0 {
		return Filters{}, errors.New("no_tags conflicts with tag_ids")
	}
	if value.ModifiedAfter, err = normalizeTimestamp(value.ModifiedAfter, "modified_after"); err != nil {
		return Filters{}, err
	}
	if value.ModifiedBefore, err = normalizeTimestamp(value.ModifiedBefore, "modified_before"); err != nil {
		return Filters{}, err
	}
	if value.ModifiedAfter != "" && value.ModifiedBefore != "" {
		after, _ := time.Parse(time.RFC3339Nano, value.ModifiedAfter)
		before, _ := time.Parse(time.RFC3339Nano, value.ModifiedBefore)
		if !after.Before(before) {
			return Filters{}, errors.New("modified_after must precede modified_before")
		}
	}
	if value.SizeMin < 0 || value.SizeMin > maxSafeInteger || value.SizeMax < 0 || value.SizeMax > maxSafeInteger {
		return Filters{}, errors.New("query size bounds must be safe nonnegative integers")
	}
	if value.SizeMin != 0 && value.SizeMax != 0 && value.SizeMin > value.SizeMax {
		return Filters{}, errors.New("size_min exceeds size_max")
	}
	return value, nil
}

func normalizeSet(values []string, limit int, valid func(string) bool, field string) ([]string, error) {
	if len(values) > limit {
		return nil, fmt.Errorf("%s has more than %d supplied entries", field, limit)
	}
	result := slices.Clone(values)
	for _, value := range result {
		if !utf8.ValidString(value) || !valid(value) {
			return nil, fmt.Errorf("%s contains an invalid value", field)
		}
	}
	slices.Sort(result)
	result = slices.Compact(result)
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func validVirtualPath(value string) bool {
	if value == "/" {
		return true
	}
	if value == "" || value[0] != '/' || strings.HasSuffix(value, "/") || strings.ContainsRune(value, 0) {
		return false
	}
	for segment := range strings.SplitSeq(value[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validUUIDv4(value string) bool { return uuidV4Pattern.MatchString(value) }

func validConcreteMIME(value string) bool {
	if strings.ToLower(value) != value {
		return false
	}
	typeEnd := strings.IndexByte(value, '/')
	return typeEnd > 0 && typeEnd == strings.LastIndexByte(value, '/') &&
		validMIMEToken(value[:typeEnd], false) && validMIMEToken(value[typeEnd+1:], false)
}

// validMIMEToken implements the RFC 2045 token grammar. Concrete MIME
// essences exclude "*"; parameter names and unquoted values may contain it.
func validMIMEToken(value string, allowWildcard bool) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character <= ' ' || character >= 0x7f || strings.ContainsRune("()<>@,;:\\\"/[]?=", rune(character)) ||
			(!allowWildcard && character == '*') {
			return false
		}
	}
	return true
}

func validMediaFamily(value string) bool {
	return oneOf(value, "email", "document", "spreadsheet", "presentation", "image", "audio_video",
		"text", "source_code", "web", "calendar", "archive", "cad", "unknown")
}

func validTextCoverage(value string) bool {
	return oneOf(value, "complete", "partial", "failed", "unprocessed", "none", "unavailable")
}

func normalizeTimestamp(value, field string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !rfc3339NanoPattern.MatchString(value) {
		return "", fmt.Errorf("%s is not strict RFC3339Nano", field)
	}
	if value[len(value)-1] != 'Z' {
		zone := value[len(value)-6:]
		zoneHour := int(zone[1]-'0')*10 + int(zone[2]-'0')
		zoneMinute := int(zone[4]-'0')*10 + int(zone[5]-'0')
		if zoneHour > 23 || zoneMinute > 59 {
			return "", fmt.Errorf("%s has an invalid RFC3339 offset", field)
		}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("%s is not RFC3339: %w", field, err)
	}
	utc := parsed.UTC()
	if utc.Year() < 0 || utc.Year() > 9999 {
		return "", fmt.Errorf("%s normalizes outside RFC3339", field)
	}
	return utc.Format(time.RFC3339Nano), nil
}

func oneOf(value string, allowed ...string) bool { return slices.Contains(allowed, value) }

func preflightJSON(raw []byte, allowFilterNull bool) error {
	if len(raw) > MaxInputBytes {
		return errors.New("input exceeds 128 KiB")
	}
	if err := validateEscapedSurrogates(raw); err != nil {
		return err
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowInvalidUTF8(false), jsontext.AllowDuplicateNames(false))
	if err := scanJSONValue(decoder, nil, 0, allowFilterNull); err != nil {
		return err
	}
	if _, err := decoder.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("input contains trailing JSON")
		}
		return err
	}
	return nil
}

func validateEscapedSurrogates(raw []byte) error {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		for index++; index < len(raw) && raw[index] != '"'; index++ {
			if raw[index] != '\\' {
				continue
			}
			index++
			if index >= len(raw) || raw[index] != 'u' || index+4 >= len(raw) {
				continue
			}
			first, ok := escapedHex16(raw[index+1 : index+5])
			if !ok {
				continue
			}
			index += 4
			if first >= 0xdc00 && first <= 0xdfff {
				return errors.New("JSON string contains a lone surrogate")
			}
			if first < 0xd800 || first > 0xdbff {
				continue
			}
			if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
				return errors.New("JSON string contains a lone surrogate")
			}
			second, ok := escapedHex16(raw[index+3 : index+7])
			if !ok || second < 0xdc00 || second > 0xdfff {
				return errors.New("JSON string contains a lone surrogate")
			}
			index += 6
		}
	}
	return nil
}

func escapedHex16(raw []byte) (uint16, bool) {
	if len(raw) != 4 {
		return 0, false
	}
	var value uint16
	for _, char := range raw {
		value <<= 4
		switch {
		case char >= '0' && char <= '9':
			value |= uint16(char - '0')
		case char >= 'a' && char <= 'f':
			value |= uint16(char-'a') + 10
		case char >= 'A' && char <= 'F':
			value |= uint16(char-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func scanJSONValue(decoder *jsontext.Decoder, path []string, depth int, allowFilterNull bool) error {
	kind := decoder.PeekKind()
	switch kind {
	case '{':
		if depth+1 > maxJSONDepth {
			return errors.New("JSON nesting exceeds 16 levels")
		}
		if _, err := decoder.ReadToken(); err != nil {
			return err
		}
		for decoder.PeekKind() != '}' {
			name, err := decoder.ReadToken()
			if err != nil {
				return err
			}
			if name.Kind() != '"' {
				return errors.New("object name is not a string")
			}
			if err := scanJSONValue(decoder, append(path, name.String()), depth+1, allowFilterNull); err != nil {
				return err
			}
		}
		_, err := decoder.ReadToken()
		return err
	case '[':
		if depth+1 > maxJSONDepth {
			return errors.New("JSON nesting exceeds 16 levels")
		}
		if _, err := decoder.ReadToken(); err != nil {
			return err
		}
		for decoder.PeekKind() != ']' {
			if err := scanJSONValue(decoder, append(path, "[]"), depth+1, allowFilterNull); err != nil {
				return err
			}
		}
		_, err := decoder.ReadToken()
		return err
	default:
		token, err := decoder.ReadToken()
		if err != nil {
			return err
		}
		if token.Kind() == 'n' && (!allowFilterNull || len(path) != 2 || path[0] != "filters" || !isOptionalFilter(path[1])) {
			return errors.New("null is allowed only for optional query filters")
		}
		if token.Kind() == '0' {
			lexeme := token.String()
			if strings.ContainsAny(lexeme, ".eE") {
				return errors.New("JSON numbers must use whole-number lexemes")
			}
			integer, err := token.Int()
			if err != nil || integer < -maxSafeInteger || integer > maxSafeInteger {
				return errors.New("JSON integer exceeds the safe range")
			}
		}
		return nil
	}
}

func isOptionalFilter(name string) bool {
	_, ok := optionalFilterFields[name]
	return ok
}
