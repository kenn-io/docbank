// Package compattest loads the frozen cross-repository compatibility evidence.
package compattest

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// BundleSHA256 is the expected digest of the raw compatibility bundle.
const BundleSHA256 = "2f968cce51b67ff5969bab6e5502d5792f815d7cd6c416802ee16ba0f8e36b89"

//go:embed testdata/document-compat-v1.json
var bundleJSON []byte

// Bundle describes the immutable bundle metadata and its independently owned sections.
type Bundle struct {
	BundleSchema   int                `json:"bundle_schema"`
	FixtureID      string             `json:"fixture_id"`
	SourcePR       string             `json:"source_pr"`
	BaselineCommit string             `json:"baseline_commit"`
	GeneratedBy    string             `json:"generated_by"`
	Sections       map[string]Section `json:"sections"`
}

// Section leaves its cases encoded so each package decodes only the contract it owns.
type Section struct {
	Owner string         `json:"owner"`
	Cases jsontext.Value `json:"cases"`
}

// Load verifies the raw bundle before decoding it.
func Load() (Bundle, []byte, error) {
	digest := sha256.Sum256(bundleJSON)
	actual := hex.EncodeToString(digest[:])
	if actual != BundleSHA256 {
		return Bundle{}, nil, fmt.Errorf("document compatibility bundle digest is %s, want %s", actual, BundleSHA256)
	}

	var bundle Bundle
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		return Bundle{}, nil, fmt.Errorf("decode document compatibility bundle: %w", err)
	}
	return bundle, bytes.Clone(bundleJSON), nil
}
