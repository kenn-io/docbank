// Package manifestjson provides strict JSON checks shared by provider
// capability manifests and provider responses.
package manifestjson

import (
	"bytes"
	"encoding/hex"
	"encoding/json/jsontext"
	"fmt"
	"strings"
)

const maxDepth = 64

// RejectDuplicateKeys fails when data contains a duplicate, non-lowercase, or
// non-ASCII object key or is nested deeper than 64 levels.
func RejectDuplicateKeys(data []byte, subject string) error {
	decoder := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true))
	return ScanValue(decoder, 0, subject)
}

// ScanValue consumes one JSON value from decoder, applying the same key rules
// as RejectDuplicateKeys. Decoder errors are returned unwrapped.
func ScanValue(decoder *jsontext.Decoder, depth int, subject string) error {
	if depth > maxDepth {
		return fmt.Errorf("%s JSON is too deeply nested", subject)
	}
	token, err := decoder.ReadToken()
	if err != nil {
		return err
	}
	if token.Kind() != jsontext.KindBeginObject && token.Kind() != jsontext.KindBeginArray {
		return nil
	}
	switch token.Kind() {
	case jsontext.KindBeginObject:
		keys := make(map[string]struct{})
		for decoder.PeekKind() != jsontext.KindEndObject {
			keyToken, err := decoder.ReadToken()
			if err != nil {
				return err
			}
			if keyToken.Kind() != jsontext.KindString {
				return fmt.Errorf("%s has a non-string JSON object key", subject)
			}
			key := keyToken.String()
			if !CanonicalKey(key) {
				return fmt.Errorf("%s JSON object key %q must use lowercase ASCII", subject, key)
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("%s has duplicate JSON object key %q", subject, key)
			}
			keys[key] = struct{}{}
			if err := ScanValue(decoder, depth+1, subject); err != nil {
				return err
			}
		}
		_, err = decoder.ReadToken()
		return err
	case jsontext.KindBeginArray:
		for decoder.PeekKind() != jsontext.KindEndArray {
			if err := ScanValue(decoder, depth+1, subject); err != nil {
				return err
			}
		}
		_, err = decoder.ReadToken()
		return err
	default:
		return fmt.Errorf("%s has an unexpected JSON delimiter", subject)
	}
}

// CanonicalKey reports whether key is non-empty lowercase ASCII.
func CanonicalKey(key string) bool {
	if key == "" {
		return false
	}
	for index := range len(key) {
		char := key[index]
		if char >= 'A' && char <= 'Z' || char >= 0x80 {
			return false
		}
	}
	return true
}

// LowerHex reports whether value is lowercase hexadecimal.
func LowerHex(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
