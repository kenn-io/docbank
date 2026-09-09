// Package canonical is the one JSON encoding used for every durable document
// contract and fingerprint. Every codec in the document tree and its
// consumers encodes through Marshal and accepts stored bytes through Decode,
// so a value has exactly one byte form and one SHA-256.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// Marshal encodes value deterministically and then applies RFC 8785 JSON
// canonicalization: sorted object members, minimal strings, ES6 number
// formatting for floats, and integers preserved exactly as written.
func Marshal(value any) ([]byte, error) {
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	canonical := jsontext.Value(encoded)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, fmt.Errorf("canonicalizing JSON: %w", err)
	}
	return []byte(canonical), nil
}

// Decode accepts only bytes that are the exact Marshal encoding of a T.
// Unknown members are rejected, and the decoded value must re-encode to the
// same bytes, so alternative spellings of one value never reach a caller.
func Decode[T any](raw []byte) (T, error) {
	return DecodeWith(raw, func(value T) ([]byte, error) { return Marshal(value) })
}

// DecodeWith is Decode for a contract whose canonical encoder is not Marshal:
// raw must equal marshal(value) byte for byte.
func DecodeWith[T any](raw []byte, marshal func(T) ([]byte, error)) (T, error) {
	var zero T
	var value T
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return zero, err
	}
	encoded, err := marshal(value)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(raw, encoded) {
		return zero, errors.New("bytes are not canonical JSON")
	}
	return value, nil
}

// IsSHA256Hex reports whether value is a lowercase hex SHA-256 digest.
func IsSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for index := range len(value) {
		char := value[index]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
