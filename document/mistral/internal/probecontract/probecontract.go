// Package probecontract owns the private serialized request identity shared by
// production probing and consumer test support.
package probecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
)

const (
	FixtureVersion            = 2
	RequestFingerprintVersion = 2
	ReasonBoundUnitsMismatch  = "bound_units_mismatch"
)

// Candidate is the fingerprinted subset of a detected document format.
type Candidate struct {
	ID        string `json:"id"`
	Family    string `json:"family"`
	MediaType string `json:"media_type"`
	UnitKind  string `json:"unit_kind"`
}

// RequestOptions is the fingerprinted provider request shape.
type RequestOptions struct {
	Pages         string `json:"pages"`
	ExtractHeader bool   `json:"extract_header"`
	ExtractFooter bool   `json:"extract_footer"`
}

type fingerprintPayload struct {
	Version   int            `json:"version"`
	Endpoint  string         `json:"endpoint"`
	Model     string         `json:"model"`
	Candidate Candidate      `json:"candidate"`
	Options   RequestOptions `json:"options"`
}

// Fingerprint returns the lowercase SHA-256 identity of one probe request.
func Fingerprint(endpoint, model string, candidate Candidate, options RequestOptions) string {
	payload, err := json.Marshal(fingerprintPayload{
		Version: RequestFingerprintVersion, Endpoint: endpoint, Model: model,
		Candidate: candidate, Options: options,
	})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
