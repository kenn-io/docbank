package canonical

import (
	"encoding/hex"
	"errors"
)

// ValidateUUIDv4 checks canonical lowercase version-4 identities.
func ValidateUUIDv4(value string) error {
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
