// Package probecontract pins the request identity shared by the Voyage
// capability probe and synthetic test manifests.
package probecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
)

// FingerprintVersion changes whenever the request identity fields change.
const FingerprintVersion = 1

type requestIdentity struct {
	Version      int    `json:"version"`
	Endpoint     string `json:"endpoint"`
	Model        string `json:"model"`
	Dimension    int    `json:"dimension"`
	CapabilityID string `json:"capability_id"`
	InputType    string `json:"input_type"`
	Truncation   bool   `json:"truncation"`
}

// Fingerprint identifies the exact provider request policy a capability was
// probed with. Media caps do not change the request and are not part of it.
func Fingerprint(endpoint, model string, dimension int, capabilityID, inputType string) string {
	encoded, err := json.Marshal(requestIdentity{
		Version: FingerprintVersion, Endpoint: endpoint, Model: model, Dimension: dimension,
		CapabilityID: capabilityID, InputType: inputType, Truncation: false,
	})
	if err != nil {
		panic(err) // Static struct with string and integer fields cannot fail.
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
