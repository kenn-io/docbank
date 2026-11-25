package canonical

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// CompatBundleSHA256 is the expected digest of the raw compatibility bundle.
const CompatBundleSHA256 = "2f968cce51b67ff5969bab6e5502d5792f815d7cd6c416802ee16ba0f8e36b89"

//go:embed testdata/document-compat-v1.json
var bundleJSON []byte

// CompatBundle describes the immutable bundle metadata and its independently owned sections.
type CompatBundle struct {
	BundleSchema   int                      `json:"bundle_schema"`
	FixtureID      string                   `json:"fixture_id"`
	SourcePR       string                   `json:"source_pr"`
	BaselineCommit string                   `json:"baseline_commit"`
	GeneratedBy    string                   `json:"generated_by"`
	Sections       map[string]CompatSection `json:"sections"`
}

// CompatSection leaves its cases encoded so each package decodes only the contract it owns.
type CompatSection struct {
	Owner string         `json:"owner"`
	Cases jsontext.Value `json:"cases"`
}

// LoadCompatBundle verifies the raw bundle before decoding it.
func LoadCompatBundle() (CompatBundle, []byte, error) {
	digest := sha256.Sum256(bundleJSON)
	actual := hex.EncodeToString(digest[:])
	if actual != CompatBundleSHA256 {
		return CompatBundle{}, nil, fmt.Errorf("document compatibility bundle digest is %s, want %s", actual, CompatBundleSHA256)
	}

	var bundle CompatBundle
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		return CompatBundle{}, nil, fmt.Errorf("decode document compatibility bundle: %w", err)
	}
	return bundle, bytes.Clone(bundleJSON), nil
}
