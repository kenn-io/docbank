package vector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Generation identifies an embedding model configuration. Two pieces of
// content embedded under generations with the same Fingerprint share a
// vector space; a different fingerprint means the caller should treat the
// vectors as a new generation and re-embed.
//
// Every field that affects the vector space must be exported and JSON
// encodable so Fingerprint accounts for it automatically. A field that
// must not affect identity has to be tagged json:"-".
type Generation struct {
	// Model names the embedding model, e.g. "text-embedding-3-small".
	Model string `json:"model,omitempty"`
	// Dimensions is the length of the vectors the model emits.
	Dimensions int `json:"dimensions,omitempty"`
	// Params holds any additional knobs that change the vector space,
	// such as a pooling mode or prompt template.
	Params map[string]string `json:"params,omitempty"`
}

// Fingerprint returns a stable identifier derived from every field that
// affects the vector space. Callers persist it alongside stored vectors
// and compare it to decide whether a new generation is required.
//
// It is built to be stable across future changes to this type:
//
//   - It encodes the struct itself, so a field added later participates
//     automatically rather than being silently excluded — the failure
//     mode that would let two distinct vector spaces share a fingerprint.
//   - It then re-encodes through a generic value, and encoding/json sorts
//     object keys at every level, so neither struct field order nor map
//     insertion order affects the hash.
//   - Decoding with UseNumber preserves numeric tokens exactly, so no
//     field loses precision through float64.
//   - omitempty drops zero-valued fields, so adding an unused field never
//     shifts an existing generation's fingerprint.
//
// All values are JSON encodable, so the marshal and decode errors are
// unreachable.
func (g Generation) Fingerprint() string {
	raw, _ := json.Marshal(g) //nolint:errchkjson // Generation holds only JSON-encodable fields

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	_ = dec.Decode(&generic)

	canonical, _ := json.Marshal(generic) //nolint:errchkjson // generic was produced by decoding valid JSON
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:8])
}
