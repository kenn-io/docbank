package providerutil

import (
	json "encoding/json/v2"
	"errors"
)

// UnmarshalEmbeddingFloat32 requires a JSON number. In particular, null must
// not become a zero coordinate in an otherwise valid embedding vector.
func UnmarshalEmbeddingFloat32(data []byte, value *float32) error {
	if len(data) == 0 || (data[0] != '-' && (data[0] < '0' || data[0] > '9')) {
		return errors.New("embedding element must be a number")
	}
	return json.Unmarshal(data, value)
}
