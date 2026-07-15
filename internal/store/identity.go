package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// newUUIDv4 returns a canonical lowercase random UUID. Version identities are
// intentionally independent of SQLite allocators, content hashes, and clocks.
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
	if len(value) != 36 || value[8] != '-' || value[13] != '-' ||
		value[18] != '-' || value[23] != '-' {
		return errors.New("must be a canonical UUIDv4")
	}
	compact := value[0:8] + value[9:13] + value[14:18] + value[19:23] + value[24:36]
	var raw [16]byte
	decoded, err := hex.Decode(raw[:], []byte(compact))
	if err != nil || decoded != len(raw) || hex.EncodeToString(raw[:]) != compact {
		return errors.New("must be a canonical lowercase UUIDv4")
	}
	if raw[6]>>4 != 4 || raw[8]>>6 != 2 {
		return errors.New("must be a version-4 UUID with an RFC 4122 variant")
	}
	return nil
}
