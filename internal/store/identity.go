package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
)

// newUUIDv4 returns a canonical lowercase random UUID. Persistent identities
// are intentionally independent of SQLite allocators, content hashes, and clocks.
func newUUIDv4() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating UUIDv4: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded[:]), nil
}

func validateUUIDv4(value string) error {
	return canonical.ValidateUUIDv4(value)
}
