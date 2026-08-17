package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

const (
	CapabilitySchemaVersion   = 3
	probeFixtureContract      = 2
	requestFingerprintVersion = 2
	maxManifestBytes          = int64(1 << 20)
)

// UnitBoundMethod identifies how a format's per-document unit limit is
// enforced before upload.
type UnitBoundMethod string

const (
	UnitBoundNone            UnitBoundMethod = "none"
	UnitBoundProviderRequest UnitBoundMethod = "provider_request"
	UnitBoundLocalExact      UnitBoundMethod = "local_exact"
)

// ProbeStatus describes the result of one authenticated format probe.
type ProbeStatus string

const (
	ProbeStatusPassed   ProbeStatus = "passed"
	ProbeStatusRejected ProbeStatus = "provider_rejected"
	ProbeStatusFailed   ProbeStatus = "probe_failed"
)

const reasonBoundUnverified = "bound_unverified"

var failureReasonCodes = []string{
	"empty_output",
	"invalid_or_local_failure",
	"provider_4xx",
	"response_too_large",
	"sentinel_missing",
	"transient_exhausted",
}

var expectedUnitBounds = map[string]UnitBoundMethod{
	"pdf": UnitBoundProviderRequest,
}

func expectedUnitBound(formatID string) UnitBoundMethod {
	if method, ok := expectedUnitBounds[formatID]; ok {
		return method
	}
	return UnitBoundNone
}

// CapabilityManifest contains sanitized, operator-produced evidence from an
// authenticated capability probe. It contains no document content or secrets.
type CapabilityManifest struct {
	SchemaVersion        int                `json:"schema_version"`
	ProbeFixtureContract int                `json:"probe_fixture_contract"`
	ObservedOn           string             `json:"observed_on"`
	Endpoint             string             `json:"endpoint"`
	Region               string             `json:"region"`
	RequestedModel       string             `json:"requested_model"`
	MaxUnits             int                `json:"max_units"`
	Results              []CapabilityResult `json:"results"`
}

// CapabilityResult contains sanitized observations for one candidate format.
type CapabilityResult struct {
	FormatID            string          `json:"format_id"`
	Family              string          `json:"family"`
	MediaType           string          `json:"media_type"`
	UnitKind            string          `json:"unit_kind"`
	Status              ProbeStatus     `json:"status"`
	ReasonCode          string          `json:"reason_code,omitempty"`
	FixtureDigest       string          `json:"fixture_digest"`
	RequestFingerprint  string          `json:"request_fingerprint"`
	ReturnedModel       string          `json:"returned_model,omitempty"`
	UnitCount           int             `json:"unit_count,omitempty"`
	UnitsProcessed      int             `json:"units_processed,omitempty"`
	ProviderBytes       *int64          `json:"provider_bytes,omitempty"`
	UnitBoundMethod     UnitBoundMethod `json:"unit_bound_method"`
	FixtureUnits        int             `json:"fixture_units,omitempty"`
	BoundRequestedUnits int             `json:"bound_requested_units,omitempty"`
	BoundUnitsProcessed int             `json:"bound_units_processed,omitempty"`
	LocalUnits          int             `json:"local_units,omitempty"`
}

// ValidateComplete validates a complete manifest without performing network
// access.
func (m CapabilityManifest) ValidateComplete() error {
	if m.SchemaVersion != CapabilitySchemaVersion {
		return fmt.Errorf("mistral capability manifest schema must be %d", CapabilitySchemaVersion)
	}
	if m.ProbeFixtureContract != probeFixtureContract {
		return fmt.Errorf("mistral capability manifest fixture contract must be %d", probeFixtureContract)
	}
	observed, err := time.Parse(time.DateOnly, m.ObservedOn)
	if err != nil || observed.After(time.Now().UTC().Add(24*time.Hour)) {
		return errors.New("mistral capability manifest has invalid observation date")
	}
	if m.Endpoint != defaultEndpoint || m.Region != defaultRegion || m.RequestedModel != defaultModel {
		return errors.New("mistral capability manifest endpoint, region, or model is not pinned")
	}
	if m.MaxUnits <= 1 || m.MaxUnits > hardMaxUnits {
		return errors.New("mistral capability manifest has invalid unit limit")
	}
	if len(m.Results) != len(candidateFormats) {
		return fmt.Errorf("mistral capability manifest has %d results, want %d", len(m.Results), len(candidateFormats))
	}
	for index, result := range m.Results {
		candidate := candidateFormats[index]
		if err := validateCapabilityResult(m, candidate, result); err != nil {
			return err
		}
	}
	return nil
}

func validateCapabilityResult(manifest CapabilityManifest, candidate CandidateFormat, result CapabilityResult) error {
	if result.FormatID != candidate.ID || result.Family != candidate.Family ||
		result.MediaType != candidate.MediaType || result.UnitKind != candidate.UnitKind {
		return fmt.Errorf("mistral capability manifest result for %q does not match its candidate", candidate.ID)
	}
	if !slices.Contains([]ProbeStatus{ProbeStatusPassed, ProbeStatusRejected, ProbeStatusFailed}, result.Status) {
		return fmt.Errorf("mistral capability manifest result %q has invalid status", candidate.ID)
	}
	if len(result.FixtureDigest) != 16 || !lowerHex(result.FixtureDigest) {
		return fmt.Errorf("mistral capability manifest result %q has invalid fixture digest", candidate.ID)
	}
	if len(result.RequestFingerprint) != sha256.Size*2 || !lowerHex(result.RequestFingerprint) {
		return fmt.Errorf("mistral capability manifest result %q has invalid request fingerprint", candidate.ID)
	}
	if result.Status != ProbeStatusPassed {
		if !slices.Contains(failureReasonCodes, result.ReasonCode) || result.ReturnedModel != "" || result.UnitCount != 0 ||
			result.UnitsProcessed != 0 || result.ProviderBytes != nil || result.UnitBoundMethod != UnitBoundNone ||
			result.FixtureUnits != 0 || result.BoundRequestedUnits != 0 ||
			result.BoundUnitsProcessed != 0 || result.LocalUnits != 0 {
			return fmt.Errorf("mistral capability manifest non-passing result %q is not scrubbed", candidate.ID)
		}
		return nil
	}
	if result.ReturnedModel != manifest.RequestedModel || result.UnitCount <= 0 ||
		result.UnitsProcessed != result.UnitCount ||
		(result.ProviderBytes != nil && *result.ProviderBytes < 0) {
		return fmt.Errorf("mistral capability manifest passing result %q is incomplete", candidate.ID)
	}

	expectedMethod := expectedUnitBound(candidate.ID)
	switch result.UnitBoundMethod {
	case UnitBoundProviderRequest:
		if expectedMethod != UnitBoundProviderRequest || result.ReasonCode != "" ||
			result.FixtureUnits != result.UnitCount || result.FixtureUnits != result.UnitsProcessed ||
			result.BoundRequestedUnits <= 0 || result.BoundRequestedUnits >= result.FixtureUnits ||
			result.FixtureUnits >= manifest.MaxUnits ||
			result.BoundUnitsProcessed != result.BoundRequestedUnits || result.LocalUnits != 0 {
			return fmt.Errorf("mistral capability manifest result %q has invalid provider-request bound evidence", candidate.ID)
		}
	case UnitBoundLocalExact:
		if expectedMethod != UnitBoundLocalExact || result.ReasonCode != "" || result.LocalUnits <= 0 ||
			result.LocalUnits != result.UnitsProcessed || result.FixtureUnits != 0 ||
			result.BoundRequestedUnits != 0 || result.BoundUnitsProcessed != 0 {
			return fmt.Errorf("mistral capability manifest result %q has invalid local-exact bound evidence", candidate.ID)
		}
	case UnitBoundNone:
		if result.FixtureUnits != 0 || result.BoundRequestedUnits != 0 ||
			result.BoundUnitsProcessed != 0 || result.LocalUnits != 0 {
			return fmt.Errorf("mistral capability manifest result %q has observations without a unit bound", candidate.ID)
		}
		if expectedMethod == UnitBoundNone && result.ReasonCode != "" {
			return fmt.Errorf("mistral capability manifest result %q has an unexpected reason", candidate.ID)
		}
		if expectedMethod != UnitBoundNone && result.ReasonCode != reasonBoundUnverified {
			return fmt.Errorf("mistral capability manifest result %q does not explain its unverified bound", candidate.ID)
		}
	default:
		return fmt.Errorf("mistral capability manifest result %q has invalid unit-bound method", candidate.ID)
	}
	return nil
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
		return fmt.Errorf("encode Mistral capability manifest: %w", err)
	}
	return nil
}

// DecodeCapabilityManifest strictly decodes a bounded manifest.
func DecodeCapabilityManifest(reader io.Reader) (CapabilityManifest, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxManifestBytes+1))
	if err != nil {
		return CapabilityManifest{}, fmt.Errorf("read Mistral capability manifest: %w", err)
	}
	if int64(len(data)) > maxManifestBytes {
		return CapabilityManifest{}, errors.New("mistral capability manifest is too large")
	}
	if err := rejectDuplicateJSONKeys(data, "mistral capability manifest"); err != nil {
		return CapabilityManifest{}, fmt.Errorf("decode Mistral capability manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest CapabilityManifest
	if err := decoder.Decode(&manifest); err != nil {
		return CapabilityManifest{}, fmt.Errorf("decode Mistral capability manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CapabilityManifest{}, errors.New("mistral capability manifest has trailing JSON")
	}
	if err := manifest.ValidateComplete(); err != nil {
		return CapabilityManifest{}, err
	}
	return manifest, nil
}

func rejectDuplicateJSONKeys(data []byte, subject string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	return scanJSONValue(decoder, 0, subject)
}

func scanJSONValue(decoder *json.Decoder, depth int, subject string) error {
	if depth > 64 {
		return fmt.Errorf("%s JSON is too deeply nested", subject)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s has a non-string JSON object key", subject)
			}
			if !canonicalJSONKey(key) {
				return fmt.Errorf("%s JSON object key %q must use lowercase ASCII", subject, key)
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("%s has duplicate JSON object key %q", subject, key)
			}
			keys[key] = struct{}{}
			if err := scanJSONValue(decoder, depth+1, subject); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1, subject); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("%s has an unexpected JSON delimiter", subject)
	}
}

func canonicalJSONKey(key string) bool {
	if key == "" {
		return false
	}
	for index := range len(key) {
		char := key[index]
		if char >= 'A' && char <= 'Z' || char >= 0x80 {
			return false
		}
	}
	return true
}

type requestOptions struct {
	Pages         string `json:"pages"`
	ExtractHeader bool   `json:"extract_header"`
	ExtractFooter bool   `json:"extract_footer"`
}

type requestFingerprintPayload struct {
	Version   int             `json:"version"`
	Endpoint  string          `json:"endpoint"`
	Model     string          `json:"model"`
	Candidate CandidateFormat `json:"candidate"`
	Options   requestOptions  `json:"options"`
}

func requestFingerprint(candidate CandidateFormat, options requestOptions) string {
	return requestFingerprintForTarget(defaultEndpoint, defaultModel, candidate, options)
}

func requestFingerprintForTarget(
	endpoint string,
	model string,
	candidate CandidateFormat,
	options requestOptions,
) string {
	payload, err := json.Marshal(requestFingerprintPayload{
		Version: requestFingerprintVersion, Endpoint: endpoint, Model: model,
		Candidate: candidate, Options: options,
	})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func probeRequestOptions(candidate CandidateFormat, maxUnits int, extractHeader, extractFooter bool) requestOptions {
	options := requestOptions{ExtractHeader: extractHeader, ExtractFooter: extractFooter}
	if candidate.Family == "pdf" {
		options.Pages = fmt.Sprintf("0-%d", maxUnits-1)
	}
	return options
}

func shortFixtureDigest(fullSHA256 string) (string, error) {
	if len(fullSHA256) != sha256.Size*2 || !lowerHex(fullSHA256) {
		return "", errors.New("fixture SHA-256 must be lowercase hexadecimal")
	}
	return fullSHA256[:16], nil
}

func lowerHex(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
