package query

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	maxHighlightTerms     = 64
	maxHighlightTermRunes = 256
)

var highlightColorPattern = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// HighlightSet is an ordered set of literal visual highlight terms.
type HighlightSet struct {
	V     int             `json:"v"`
	Terms []HighlightTerm `json:"terms"`
}

// HighlightTerm is literal text and its display color. It has no regex or
// executable query behavior.
type HighlightTerm struct {
	Text  string `json:"text"`
	Color string `json:"color"`
}

type highlightSetInput struct {
	V     *int                  `json:"v"`
	Terms *[]highlightTermInput `json:"terms"`
}

type highlightTermInput struct {
	Text  *string `json:"text"`
	Color *string `json:"color"`
}

// ParseHighlightSet validates and normalizes one bounded HighlightSetV1 JSON value.
func ParseHighlightSet(raw []byte) (HighlightSet, error) {
	if err := preflightJSON(raw, false); err != nil {
		return HighlightSet{}, fmt.Errorf("parse highlight set: %w", err)
	}
	var input highlightSetInput
	if err := json.Unmarshal(raw, &input, json.RejectUnknownMembers(true), json.MatchCaseInsensitiveNames(false)); err != nil {
		return HighlightSet{}, fmt.Errorf("parse highlight set: %w", err)
	}
	value := HighlightSet{V: 1}
	if input.V != nil {
		value.V = *input.V
	}
	if input.Terms == nil {
		return HighlightSet{}, errors.New("highlight terms are required")
	}
	value.Terms = make([]HighlightTerm, len(*input.Terms))
	for index, term := range *input.Terms {
		if term.Text == nil || term.Color == nil {
			return HighlightSet{}, errors.New("highlight term text and color are required")
		}
		value.Terms[index] = HighlightTerm{Text: *term.Text, Color: *term.Color}
	}
	normalized, err := normalizeHighlightSet(value)
	if err != nil {
		return HighlightSet{}, err
	}
	if _, err := CanonicalHighlightSet(normalized); err != nil {
		return HighlightSet{}, err
	}
	return normalized, nil
}

// CanonicalHighlightSet returns the one canonical HighlightSetV1 encoding.
func CanonicalHighlightSet(value HighlightSet) ([]byte, error) {
	normalized, err := normalizeHighlightSet(value)
	if err != nil {
		return nil, err
	}
	encoded, err := canonical.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("encode highlight set: %w", err)
	}
	if len(encoded) > maxCanonicalBytes {
		return nil, errors.New("canonical highlight set exceeds 64 KiB")
	}
	return encoded, nil
}

// HighlightSetFingerprint returns the SHA-256 identity of canonical highlight bytes.
func HighlightSetFingerprint(value HighlightSet) (string, error) {
	encoded, err := CanonicalHighlightSet(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func normalizeHighlightSet(value HighlightSet) (HighlightSet, error) {
	if value.V != 1 {
		return HighlightSet{}, errors.New("highlight set version must be 1")
	}
	if len(value.Terms) < 1 || len(value.Terms) > maxHighlightTerms {
		return HighlightSet{}, errors.New("highlight set must contain 1 to 64 terms")
	}
	value.Terms = slices.Clone(value.Terms)
	seen := make(map[string]struct{}, len(value.Terms))
	for _, term := range value.Terms {
		if !utf8.ValidString(term.Text) {
			return HighlightSet{}, errors.New("highlight text is not valid Unicode")
		}
		count := utf8.RuneCountInString(term.Text)
		if count < 1 || count > maxHighlightTermRunes {
			return HighlightSet{}, errors.New("highlight text is outside its length bounds")
		}
		if !highlightColorPattern.MatchString(term.Color) {
			return HighlightSet{}, errors.New("highlight color must be lowercase #rrggbb")
		}
		if _, exists := seen[term.Text]; exists {
			return HighlightSet{}, errors.New("highlight text is duplicated")
		}
		seen[term.Text] = struct{}{}
	}
	return value, nil
}
