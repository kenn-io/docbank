package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"

	"go.kenn.io/docbank/document"
)

// SourceMetadataJSON returns immutable generation bytes for SQL, or nil for
// unsupported or damaged evidence. Publication and metadata import validate
// the typed canonical schema before storing these bytes.
func SourceMetadataJSON(encoded []byte, contract, checksum string) any {
	if contract != document.SourceMetadataContractV1 || len(encoded) > document.MaxSourceMetadataEncodedBytes || !jsontext.Value(encoded).IsValid() {
		return nil
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != checksum {
		return nil
	}
	return string(encoded)
}
