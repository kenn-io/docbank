package voyage

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"go.kenn.io/docbank/document/internal/manifestjson"
)

const (
	// CapabilitySchemaVersion is the manifest schema this package reads and
	// writes.
	CapabilitySchemaVersion = 1
	// ProbeFixtureContract identifies the deterministic fixture set a probe
	// must have used.
	ProbeFixtureContract = 1

	maxManifestBytes = int64(1 << 20)
	fixtureDigestLen = 16
)

// ProbeStatus describes the result of one authenticated capability probe.
type ProbeStatus string

// Probe statuses.
const (
	ProbeStatusPassed   ProbeStatus = "passed"
	ProbeStatusRejected ProbeStatus = "provider_rejected"
	ProbeStatusFailed   ProbeStatus = "probe_failed"
)

// Sanitized reason codes for non-passing probe results.
const (
	ReasonProviderRejected    = "provider_rejected"
	ReasonProviderLimit       = "provider_limit"
	ReasonMalformedResponse   = "malformed_response"
	ReasonTransientExhausted  = "transient_exhausted"
	ReasonMotionNotObserved   = "motion_not_observed"
	ReasonRankingNotObserved  = "ranking_not_observed"
	ReasonOrderNotObserved    = "order_not_observed"
	ReasonInvalidOrLocalError = "invalid_or_local_failure"
)

var failureReasonCodes = []string{
	ReasonProviderRejected, ReasonProviderLimit, ReasonMalformedResponse, ReasonTransientExhausted,
	ReasonMotionNotObserved, ReasonRankingNotObserved, ReasonOrderNotObserved, ReasonInvalidOrLocalError,
}

// CapabilityManifest contains sanitized, operator-produced evidence from an
// authenticated capability probe. It contains no media, vectors, or secrets.
type CapabilityManifest struct {
	SchemaVersion        int                `json:"schema_version"`
	ProbeFixtureContract int                `json:"probe_fixture_contract"`
	ObservedOn           string             `json:"observed_on"`
	Endpoint             string             `json:"endpoint"`
	Model                string             `json:"model"`
	Dimension            int                `json:"dimension"`
	MaxBatchItems        int                `json:"max_batch_items"`
	Results              []CapabilityResult `json:"results"`
}

// CapabilityResult contains sanitized observations for one capability.
type CapabilityResult struct {
	CapabilityID       string      `json:"capability_id"`
	Status             ProbeStatus `json:"status"`
	ReasonCode         string      `json:"reason_code,omitempty"`
	FixtureDigest      string      `json:"fixture_digest"`
	RequestFingerprint string      `json:"request_fingerprint"`
	TotalTokens        *int64      `json:"total_tokens,omitempty"`
}

// ValidateComplete validates a complete manifest without performing network
// access.
func (m CapabilityManifest) ValidateComplete() error {
	if m.SchemaVersion != CapabilitySchemaVersion {
		return fmt.Errorf("voyage capability manifest schema must be %d", CapabilitySchemaVersion)
	}
	if m.ProbeFixtureContract != ProbeFixtureContract {
		return fmt.Errorf("voyage capability manifest fixture contract must be %d", ProbeFixtureContract)
	}
	observed, err := time.Parse(time.DateOnly, m.ObservedOn)
	if err != nil || observed.After(time.Now().UTC().Add(24*time.Hour)) {
		return errors.New("voyage capability manifest has invalid observation date")
	}
	if _, ok := pinnedEndpoint(m.Model, m.Dimension); !ok || m.Endpoint != DefaultEndpoint {
		return errors.New("voyage capability manifest endpoint, model, or dimension is not pinned")
	}
	if m.MaxBatchItems < 1 || m.MaxBatchItems > MaxBatchItems {
		return errors.New("voyage capability manifest has invalid batch limit")
	}
	if len(m.Results) != len(capabilities) {
		return fmt.Errorf("voyage capability manifest has %d results, want %d", len(m.Results), len(capabilities))
	}
	for index, result := range m.Results {
		if err := validateCapabilityResult(capabilities[index], result); err != nil {
			return err
		}
	}
	return nil
}

func validateCapabilityResult(capability Capability, result CapabilityResult) error {
	if result.CapabilityID != capability.ID {
		return fmt.Errorf("voyage capability manifest result %d must be %q", indexOf(capability), capability.ID)
	}
	if !slices.Contains([]ProbeStatus{ProbeStatusPassed, ProbeStatusRejected, ProbeStatusFailed}, result.Status) {
		return fmt.Errorf("voyage capability manifest result %q has invalid status", capability.ID)
	}
	if len(result.FixtureDigest) != fixtureDigestLen || !manifestjson.LowerHex(result.FixtureDigest) {
		return fmt.Errorf("voyage capability manifest result %q has invalid fixture digest", capability.ID)
	}
	if len(result.RequestFingerprint) != sha256.Size*2 || !manifestjson.LowerHex(result.RequestFingerprint) {
		return fmt.Errorf("voyage capability manifest result %q has invalid request fingerprint", capability.ID)
	}
	if result.Status != ProbeStatusPassed {
		if !slices.Contains(failureReasonCodes, result.ReasonCode) || result.TotalTokens != nil {
			return fmt.Errorf("voyage capability manifest non-passing result %q is not scrubbed", capability.ID)
		}
		return nil
	}
	if result.ReasonCode != "" || (result.TotalTokens != nil && *result.TotalTokens < 0) {
		return fmt.Errorf("voyage capability manifest passing result %q is inconsistent", capability.ID)
	}
	return nil
}

func indexOf(capability Capability) int {
	for index, candidate := range capabilities {
		if candidate.ID == capability.ID {
			return index
		}
	}
	return -1
}

// EncodeCapabilityManifest writes an indented, validated manifest.
func EncodeCapabilityManifest(writer io.Writer, manifest CapabilityManifest) error {
	if err := manifest.ValidateComplete(); err != nil {
		return err
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(manifest); err != nil {
		return fmt.Errorf("encode Voyage capability manifest: %w", err)
	}
	return nil
}

// DecodeCapabilityManifest strictly decodes a bounded manifest.
func DecodeCapabilityManifest(reader io.Reader) (CapabilityManifest, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxManifestBytes+1))
	if err != nil {
		return CapabilityManifest{}, fmt.Errorf("read Voyage capability manifest: %w", err)
	}
	if int64(len(data)) > maxManifestBytes {
		return CapabilityManifest{}, errors.New("voyage capability manifest is too large")
	}
	if err := manifestjson.RejectDuplicateKeys(data, "voyage capability manifest"); err != nil {
		return CapabilityManifest{}, fmt.Errorf("decode Voyage capability manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest CapabilityManifest
	if err := decoder.Decode(&manifest); err != nil {
		return CapabilityManifest{}, fmt.Errorf("decode Voyage capability manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CapabilityManifest{}, errors.New("voyage capability manifest has trailing JSON")
	}
	if err := manifest.ValidateComplete(); err != nil {
		return CapabilityManifest{}, err
	}
	return manifest, nil
}
