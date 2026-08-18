package voyage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document/media"
)

const (
	defaultProvider        = "voyage"
	canonicalPolicyVersion = 1
	inputTypeDocument      = "document"
	inputTypeQuery         = "query"

	// DefaultEndpoint is the package-pinned Voyage API root.
	DefaultEndpoint = "https://api.voyageai.com/v1"
	// DefaultModel is the package-pinned multimodal embedding model.
	DefaultModel = "voyage-multimodal-3.5"
	// DefaultDimension is the pinned output dimension for DefaultModel.
	DefaultDimension = 1024
	// MaxBatchItems is the largest document batch accepted by Policy.
	MaxBatchItems = 64
	// MaxRequestBytes is the largest encoded request accepted by Policy.
	MaxRequestBytes = int64(64 << 20)
	// MaxResponseBytes is the largest provider response accepted by Policy.
	MaxResponseBytes = int64(8 << 20)
)

// PolicyConfig contains reusable processing policy.
type PolicyConfig struct {
	// Model must be a package-pinned model; empty selects DefaultModel.
	Model string
	// Dimension must be the pinned dimension for Model; zero selects it.
	Dimension int
	// Media bounds document and query media.
	Media media.Policy
	// MaxBatchItems bounds one document request; zero selects MaxBatchItems.
	MaxBatchItems int
	// MaxRequestBytes bounds one encoded request; zero selects MaxRequestBytes.
	MaxRequestBytes int64
	// MaxResponseBytes bounds one provider response; zero selects MaxResponseBytes.
	MaxResponseBytes int64
}

// PolicyValues is a read-only copy of every effective policy value.
type PolicyValues struct {
	Provider         string       `json:"provider"`
	Endpoint         string       `json:"endpoint"`
	Model            string       `json:"model"`
	Dimension        int          `json:"dimension"`
	Media            media.Policy `json:"media"`
	MaxBatchItems    int          `json:"max_batch_items"`
	MaxRequestBytes  int64        `json:"max_request_bytes"`
	MaxResponseBytes int64        `json:"max_response_bytes"`
}

// Policy is an opaque reusable Voyage processing policy.
type Policy struct {
	values PolicyValues
	digest string
}

// NewPolicy validates and constructs an immutable policy.
func NewPolicy(config PolicyConfig) (Policy, error) {
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.Dimension == 0 {
		config.Dimension = DefaultDimension
	}
	endpoint, ok := pinnedEndpoint(config.Model, config.Dimension)
	if !ok {
		return Policy{}, fmt.Errorf("voyage model %q with dimension %d is not pinned", config.Model, config.Dimension)
	}
	if config.MaxBatchItems == 0 {
		config.MaxBatchItems = MaxBatchItems
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = MaxRequestBytes
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = MaxResponseBytes
	}
	if config.MaxBatchItems < 1 || config.MaxBatchItems > MaxBatchItems ||
		config.MaxRequestBytes < 1 || config.MaxRequestBytes > MaxRequestBytes ||
		config.MaxResponseBytes < 1 || config.MaxResponseBytes > MaxResponseBytes {
		return Policy{}, errors.New("voyage policy request bounds are invalid")
	}
	mediaPolicy := config.Media.Normalized()
	if err := mediaPolicy.Validate(); err != nil {
		return Policy{}, fmt.Errorf("voyage policy media bounds: %w", err)
	}
	values := PolicyValues{
		Provider: defaultProvider, Endpoint: endpoint, Model: config.Model, Dimension: config.Dimension,
		Media: mediaPolicy, MaxBatchItems: config.MaxBatchItems,
		MaxRequestBytes: config.MaxRequestBytes, MaxResponseBytes: config.MaxResponseBytes,
	}
	digest, err := policyValuesDigest(values)
	if err != nil {
		return Policy{}, err
	}
	return Policy{values: values, digest: digest}, nil
}

// pinnedEndpoint is the package-pinned model allowlist. Live availability must
// also be demonstrated by an authenticated capability probe.
func pinnedEndpoint(model string, dimension int) (string, bool) {
	if model == DefaultModel && dimension == DefaultDimension {
		return DefaultEndpoint, true
	}
	return "", false
}

// Values returns a copy of every effective policy value.
func (p Policy) Values() PolicyValues { return p.values }

// MediaPolicy returns the media bounds covered by this policy's identity.
func (p Policy) MediaPolicy() media.Policy { return p.values.Media }

func (p Policy) valid() bool { return p.digest != "" }

type capabilityIdentity struct {
	CapabilityID       string `json:"capability_id"`
	RequestFingerprint string `json:"request_fingerprint"`
	FixtureDigest      string `json:"fixture_digest"`
}

type canonicalPolicy struct {
	Version               int                  `json:"version"`
	Provider              string               `json:"provider"`
	Endpoint              string               `json:"endpoint"`
	Model                 string               `json:"model"`
	Dimension             int                  `json:"dimension"`
	Media                 media.Policy         `json:"media"`
	MaxBatchItems         int                  `json:"max_batch_items"`
	MaxRequestBytes       int64                `json:"max_request_bytes"`
	MaxResponseBytes      int64                `json:"max_response_bytes"`
	CapabilityAuthorities []capabilityIdentity `json:"capability_authorities"`
}

// CanonicalJSON returns the canonical reusable policy identity, including the
// capabilities the manifest authorizes under this policy.
func (p Policy) CanonicalJSON(manifest CapabilityManifest) ([]byte, error) {
	if !p.valid() {
		return nil, errors.New("voyage policy is invalid; use NewPolicy")
	}
	if err := p.checkManifestTarget(manifest); err != nil {
		return nil, err
	}
	authorities := make([]capabilityIdentity, 0, len(manifest.Results))
	for index, result := range manifest.Results {
		if result.Status != ProbeStatusPassed {
			continue
		}
		capability := capabilities[index]
		expected := requestFingerprint(p.values.Endpoint, p.values.Model, p.values.Dimension, capability)
		if result.RequestFingerprint != expected {
			return nil, fmt.Errorf("voyage capability result %q was probed with a different request policy", capability.ID)
		}
		authorities = append(authorities, capabilityIdentity{
			CapabilityID: capability.ID, RequestFingerprint: result.RequestFingerprint,
			FixtureDigest: result.FixtureDigest,
		})
	}
	slices.SortFunc(authorities, func(left, right capabilityIdentity) int {
		switch {
		case left.CapabilityID < right.CapabilityID:
			return -1
		case left.CapabilityID > right.CapabilityID:
			return 1
		default:
			return 0
		}
	})
	encoded, err := json.Marshal(canonicalPolicy{
		Version: canonicalPolicyVersion, Provider: p.values.Provider, Endpoint: p.values.Endpoint,
		Model: p.values.Model, Dimension: p.values.Dimension, Media: p.values.Media,
		MaxBatchItems: p.values.MaxBatchItems, MaxRequestBytes: p.values.MaxRequestBytes,
		MaxResponseBytes: p.values.MaxResponseBytes, CapabilityAuthorities: authorities,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Voyage policy identity: %w", err)
	}
	return encoded, nil
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

func (p Policy) checkManifestTarget(manifest CapabilityManifest) error {
	if err := manifest.ValidateComplete(); err != nil {
		return err
	}
	if manifest.Endpoint != p.values.Endpoint || manifest.Model != p.values.Model ||
		manifest.Dimension != p.values.Dimension {
		return errors.New("voyage policy target differs from capability manifest")
	}
	if p.values.MaxBatchItems > manifest.MaxBatchItems {
		return fmt.Errorf(
			"voyage policy batch limit %d exceeds capability manifest authority %d",
			p.values.MaxBatchItems, manifest.MaxBatchItems,
		)
	}
	return nil
}

// Authorization is opaque evidence that one capability passed an authenticated
// probe under a policy. It does not attest human consent.
type Authorization struct {
	capability        Capability
	policyFingerprint string
	policyDigest      string
}

// Capability returns the authorized capability.
func (a Authorization) Capability() Capability { return a.capability }

// PolicyFingerprint returns the public policy identity covered by the
// authorization.
func (a Authorization) PolicyFingerprint() string { return a.policyFingerprint }

// Authorize derives non-persistable capability authority from a complete
// manifest.
func (p Policy) Authorize(manifest CapabilityManifest, capabilityID string) (Authorization, error) {
	if !p.valid() {
		return Authorization{}, errors.New("voyage policy is invalid; use NewPolicy")
	}
	capability, ok := CapabilityByID(capabilityID)
	if !ok {
		return Authorization{}, fmt.Errorf("voyage capability %q is unknown", capabilityID)
	}
	if err := p.checkManifestTarget(manifest); err != nil {
		return Authorization{}, noUploadAuthority(err)
	}
	var result CapabilityResult
	for _, candidate := range manifest.Results {
		if candidate.CapabilityID == capability.ID {
			result = candidate
			break
		}
	}
	if result.Status != ProbeStatusPassed {
		return Authorization{}, noUploadAuthority(fmt.Errorf("capability %q did not pass the probe", capabilityID))
	}
	expected := requestFingerprint(p.values.Endpoint, p.values.Model, p.values.Dimension, capability)
	if result.RequestFingerprint != expected {
		return Authorization{}, noUploadAuthority(fmt.Errorf("capability %q was probed with a different request policy", capabilityID))
	}
	fingerprint, err := p.Fingerprint(manifest)
	if err != nil {
		return Authorization{}, noUploadAuthority(err)
	}
	return Authorization{capability: capability, policyFingerprint: fingerprint, policyDigest: p.digest}, nil
}

// AuthorizeAll returns an authorization for every capability the manifest
// passes under this policy. It never fails for unauthorized capabilities; it
// omits them.
func (p Policy) AuthorizeAll(manifest CapabilityManifest) ([]Authorization, error) {
	if !p.valid() {
		return nil, errors.New("voyage policy is invalid; use NewPolicy")
	}
	if err := p.checkManifestTarget(manifest); err != nil {
		return nil, noUploadAuthority(err)
	}
	authorizations := make([]Authorization, 0, len(capabilities))
	for _, capability := range capabilities {
		authorization, err := p.Authorize(manifest, capability.ID)
		if err != nil {
			continue
		}
		authorizations = append(authorizations, authorization)
	}
	return authorizations, nil
}

func noUploadAuthority(cause error) error {
	return fmt.Errorf("no capability has authorized upload authority; run the authenticated capability probe and supply its manifest: %w", cause)
}

func policyValuesDigest(values PolicyValues) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode Voyage policy values: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
