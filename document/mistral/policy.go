package mistral

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document"
)

const (
	defaultProvider        = "mistral"
	defaultEndpoint        = "https://api.eu.mistral.ai/v1/ocr"
	canonicalPolicyVersion = 1

	// RegionEU is the supported Mistral processing region.
	RegionEU = "eu"
	// DefaultModel is the package-pinned Mistral OCR model.
	DefaultModel = "mistral-ocr-4-0"
	// RetentionStandard identifies Mistral's standard retention posture.
	RetentionStandard = "standard"
	// RetentionZDR identifies Mistral's zero-data-retention posture.
	RetentionZDR = "zdr"
	// TrainingDefaultOptOut identifies Mistral's default training opt-out posture.
	TrainingDefaultOptOut = "default-opt-out"
	// TrainingOptedOut identifies an explicit training opt-out posture.
	TrainingOptedOut = "opted-out"
	// MaxDocumentBytes is the largest document accepted by Policy.
	MaxDocumentBytes = int64(500 << 20)
	// MaxResponseBytes is the largest provider response accepted by Policy.
	MaxResponseBytes = int64(512 << 20)
	// MinUnits is the smallest per-document unit limit accepted by Policy.
	MinUnits = 3
	// MaxUnits is the largest per-document unit limit accepted by Policy.
	MaxUnits = 5_000

	defaultRegion = RegionEU
	defaultModel  = DefaultModel
)

// PolicyConfig contains reusable processing and privacy policy.
type PolicyConfig struct {
	Region           string
	Model            string
	Retention        string
	Training         string
	MaxDocumentBytes int64
	MaxResponseBytes int64
	MaxUnits         int
	ExtractHeader    bool
	ExtractFooter    bool
	NormalizePolicy  document.NormalizePolicy
}

// PolicyValues is a read-only copy of every effective policy value.
type PolicyValues struct {
	Provider         string                           `json:"provider"`
	Endpoint         string                           `json:"endpoint"`
	Region           string                           `json:"region"`
	Model            string                           `json:"model"`
	Retention        string                           `json:"retention"`
	Training         string                           `json:"training"`
	MaxDocumentBytes int64                            `json:"max_document_bytes"`
	MaxResponseBytes int64                            `json:"max_response_bytes"`
	MaxUnits         int                              `json:"max_units"`
	ExtractHeader    bool                             `json:"extract_header"`
	ExtractFooter    bool                             `json:"extract_footer"`
	Normalization    document.NormalizePolicyIdentity `json:"normalization"`
}

// Policy is an opaque reusable Mistral processing policy.
type Policy struct {
	values          PolicyValues
	normalizePolicy document.NormalizePolicy
	digest          string
}

// NewPolicy validates and constructs an immutable policy.
func NewPolicy(config PolicyConfig) (Policy, error) {
	endpoint, available := regionalOCREndpoint(config.Region, config.Model)
	if !available {
		return Policy{}, fmt.Errorf("mistral OCR model %q is unavailable in region %q", config.Model, config.Region)
	}
	if !slices.Contains([]string{RetentionStandard, RetentionZDR}, config.Retention) {
		return Policy{}, errors.New("mistral policy retention posture must be known")
	}
	if !slices.Contains([]string{TrainingDefaultOptOut, TrainingOptedOut}, config.Training) {
		return Policy{}, errors.New("mistral policy training posture must be known")
	}
	if config.MaxDocumentBytes <= 0 || config.MaxDocumentBytes > MaxDocumentBytes ||
		config.MaxResponseBytes <= 0 || config.MaxResponseBytes > MaxResponseBytes ||
		config.MaxUnits < MinUnits || config.MaxUnits > MaxUnits {
		return Policy{}, errors.New("mistral policy processing bounds are invalid")
	}
	normalization := config.NormalizePolicy.Identity()
	if normalization.Version <= 0 || normalization.MaxDocumentChars <= 0 {
		return Policy{}, errors.New("mistral policy normalization is invalid; use document.NewNormalizePolicy")
	}
	values := PolicyValues{
		Provider: defaultProvider, Endpoint: endpoint, Region: config.Region, Model: config.Model,
		Retention: config.Retention, Training: config.Training,
		MaxDocumentBytes: config.MaxDocumentBytes, MaxResponseBytes: config.MaxResponseBytes,
		MaxUnits: config.MaxUnits, ExtractHeader: config.ExtractHeader, ExtractFooter: config.ExtractFooter,
		Normalization: normalization,
	}
	digest, err := policyValuesDigest(values)
	if err != nil {
		return Policy{}, err
	}
	return Policy{values: values, normalizePolicy: config.NormalizePolicy, digest: digest}, nil
}

// regionalOCREndpoint is the package-pinned region and model allowlist. Live
// availability must also be demonstrated by an authenticated capability probe.
func regionalOCREndpoint(region, model string) (string, bool) {
	if region == defaultRegion && model == defaultModel {
		return defaultEndpoint, true
	}
	return "", false
}

// Values returns a copy of every effective policy value.
func (p Policy) Values() PolicyValues { return p.values }

// NormalizePolicy returns the executable normalization policy covered by this
// policy's identity.
func (p Policy) NormalizePolicy() document.NormalizePolicy { return p.normalizePolicy }

type capabilityIdentity struct {
	FormatID           string          `json:"format_id"`
	UnitBoundMethod    UnitBoundMethod `json:"unit_bound_method"`
	RequestFingerprint string          `json:"request_fingerprint"`
	FixtureDigest      string          `json:"fixture_digest"`
}

type canonicalPolicy struct {
	Version           int                              `json:"version"`
	Provider          string                           `json:"provider"`
	Endpoint          string                           `json:"endpoint"`
	Region            string                           `json:"region"`
	Model             string                           `json:"model"`
	Retention         string                           `json:"retention"`
	Training          string                           `json:"training"`
	MaxDocumentBytes  int64                            `json:"max_document_bytes"`
	MaxResponseBytes  int64                            `json:"max_response_bytes"`
	MaxUnits          int                              `json:"max_units"`
	ExtractHeader     bool                             `json:"extract_header"`
	ExtractFooter     bool                             `json:"extract_footer"`
	Normalization     document.NormalizePolicyIdentity `json:"normalization"`
	FormatAuthorities []capabilityIdentity             `json:"format_authorities"`
}

// CanonicalJSON returns the canonical reusable policy identity.
func (p Policy) CanonicalJSON(manifest CapabilityManifest) ([]byte, error) {
	if p.digest == "" {
		return nil, errors.New("mistral policy is invalid; use NewPolicy")
	}
	if err := manifest.ValidateComplete(); err != nil {
		return nil, err
	}
	if manifest.Endpoint != p.values.Endpoint || manifest.Region != p.values.Region ||
		manifest.RequestedModel != p.values.Model {
		return nil, errors.New("mistral policy and capability target differ")
	}
	if p.values.MaxUnits > manifest.MaxUnits {
		return nil, fmt.Errorf(
			"mistral policy unit limit %d exceeds capability manifest authority %d",
			p.values.MaxUnits, manifest.MaxUnits,
		)
	}
	authorities := make([]capabilityIdentity, 0, len(manifest.Results))
	for index, result := range manifest.Results {
		if result.Status != ProbeStatusPassed || result.UnitBoundMethod == UnitBoundNone {
			continue
		}
		candidate := candidateFormats[index]
		expected := requestFingerprint(candidate, probeRequestOptions(
			candidate, manifest.MaxUnits, p.values.ExtractHeader, p.values.ExtractFooter,
		))
		if result.RequestFingerprint != expected {
			return nil, fmt.Errorf("mistral capability result %q was probed with a different request policy", candidate.ID)
		}
		authorities = append(authorities, capabilityIdentity{
			FormatID: candidate.ID, UnitBoundMethod: result.UnitBoundMethod,
			RequestFingerprint: result.RequestFingerprint, FixtureDigest: result.FixtureDigest,
		})
	}
	slices.SortFunc(authorities, func(left, right capabilityIdentity) int {
		if left.FormatID < right.FormatID {
			return -1
		}
		if left.FormatID > right.FormatID {
			return 1
		}
		return 0
	})
	return json.Marshal(canonicalPolicy{
		Version: canonicalPolicyVersion, Provider: p.values.Provider, Endpoint: p.values.Endpoint, Region: p.values.Region,
		Model: p.values.Model, Retention: p.values.Retention, Training: p.values.Training,
		MaxDocumentBytes: p.values.MaxDocumentBytes, MaxResponseBytes: p.values.MaxResponseBytes,
		MaxUnits: p.values.MaxUnits, ExtractHeader: p.values.ExtractHeader, ExtractFooter: p.values.ExtractFooter,
		Normalization: p.values.Normalization, FormatAuthorities: authorities,
	})
}

// Fingerprint returns lowercase SHA-256 over CanonicalJSON.
func (p Policy) Fingerprint(manifest CapabilityManifest) (string, error) {
	encoded, err := p.CanonicalJSON(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// FormatAuthorization is opaque evidence that one format has a probe-tested,
// enforceable unit bound under a policy. It does not attest human consent.
type FormatAuthorization struct {
	format            CandidateFormat
	method            UnitBoundMethod
	policyFingerprint string
	policyDigest      string
}

// Format returns the authorized format.
func (a FormatAuthorization) Format() CandidateFormat { return a.format }

// PolicyFingerprint returns the public policy identity covered by the
// authorization.
func (a FormatAuthorization) PolicyFingerprint() string { return a.policyFingerprint }

// Authorize derives non-persistable format authority from a complete manifest.
func (p Policy) Authorize(manifest CapabilityManifest, formatID string) (FormatAuthorization, error) {
	if p.digest == "" {
		return FormatAuthorization{}, errors.New("mistral policy is invalid; use NewPolicy")
	}
	if err := manifest.ValidateComplete(); err != nil {
		return FormatAuthorization{}, noUploadAuthority(err)
	}
	candidate, ok := CandidateFormatByID(formatID)
	if !ok {
		return FormatAuthorization{}, fmt.Errorf("mistral format %q is unknown", formatID)
	}
	if manifest.Endpoint != p.values.Endpoint || manifest.Region != p.values.Region ||
		manifest.RequestedModel != p.values.Model {
		return FormatAuthorization{}, errors.New("mistral policy target differs from capability manifest")
	}
	if p.values.MaxUnits > manifest.MaxUnits {
		return FormatAuthorization{}, fmt.Errorf(
			"mistral policy unit limit %d exceeds capability manifest authority %d",
			p.values.MaxUnits, manifest.MaxUnits,
		)
	}
	var result CapabilityResult
	for _, candidateResult := range manifest.Results {
		if candidateResult.FormatID == candidate.ID {
			result = candidateResult
			break
		}
	}
	if result.Status != ProbeStatusPassed || result.UnitBoundMethod == UnitBoundNone {
		return FormatAuthorization{}, noUploadAuthority(fmt.Errorf("format %q has no enforceable unit bound", formatID))
	}
	expected := requestFingerprint(candidate, probeRequestOptions(
		candidate, manifest.MaxUnits, p.values.ExtractHeader, p.values.ExtractFooter,
	))
	if result.RequestFingerprint != expected {
		return FormatAuthorization{}, noUploadAuthority(fmt.Errorf("format %q was probed with a different request policy", formatID))
	}
	fingerprint, err := p.Fingerprint(manifest)
	if err != nil {
		return FormatAuthorization{}, noUploadAuthority(err)
	}
	return FormatAuthorization{
		format: candidate, method: result.UnitBoundMethod,
		policyFingerprint: fingerprint, policyDigest: p.digest,
	}, nil
}

func noUploadAuthority(cause error) error {
	return fmt.Errorf("no format has authorized upload authority; run the authenticated capability probe and supply its manifest: %w", cause)
}

func policyValuesDigest(values PolicyValues) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode Mistral policy values: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
