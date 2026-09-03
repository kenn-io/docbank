// Package tika defines the fixed compatibility profile used when an operator
// deploys an Apache Tika adapter behind docbank-rendition/v1.
package tika

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bridge"
	"go.kenn.io/docbank/document/internal/bridgeprofile"
	"go.kenn.io/docbank/document/internal/providerutil"
)

const (
	// ProfileContractV1 identifies the canonical Apache Tika bridge profile.
	ProfileContractV1 = "tika-bridge-profile/v1"
	descriptorID      = "tika.bridge.v1"
)

var provider = providerutil.Provider("Tika")

var profileCodec = bridgeprofile.Codec[ProfileV1]{
	Prefix: "tika",
	Clone:  cloneProfile,
	Normalize: func(profile *ProfileV1) {
		slices.SortFunc(profile.SupportedFormats, bridgeprofile.CompareFormats)
		slices.Sort(profile.ArtifactPolicy.AllowedRoles)
	},
	Fingerprint: func(profile *ProfileV1) *string { return &profile.PolicyFingerprint },
	Validate:    validateProfile,
}

// Config supplies the only operator-specific profile values. It deliberately
// has no routes, URLs, headers, parser options, or fetch controls.
type Config struct {
	DeploymentID      string
	RuntimeID         string
	CredentialBinding string
}

// LimitsV1 fixes every finite input, output, polling, and wall-clock bound.
type LimitsV1 struct {
	MaxDocumentBytes     int64 `json:"max_document_bytes"`
	MaxPollAttempts      int   `json:"max_poll_attempts"`
	MaxResponseBytes     int64 `json:"max_response_bytes"`
	PollIntervalMillis   int64 `json:"poll_interval_millis"`
	RequestTimeoutMillis int64 `json:"request_timeout_millis"`
	TotalTimeoutMillis   int64 `json:"total_timeout_millis"`
}

// DisclosurePolicyV1 permits only exact supplied bytes and records the
// bridge's safe-basename disclosure. Authorization binds every byte and tuple.
type DisclosurePolicyV1 struct {
	DiscloseFilename bool   `json:"disclose_filename"`
	Source           string `json:"source"`
}

// ReferencePolicyV1 refuses both embedded and external reference fetching.
// The generic bridge cannot inspect parser internals, so compatibility requires
// an operator-pinned adapter runtime audited to enforce both refusals.
type ReferencePolicyV1 struct {
	EmbeddedReferenceFetch string `json:"embedded_reference_fetch"`
	EnforcementBoundary    string `json:"enforcement_boundary"`
	ExternalReferenceFetch string `json:"external_reference_fetch"`
}

// EvidencePolicyV1 fixes bounded provider-neutral evidence and Markdown.
type EvidencePolicyV1 struct {
	MaxProviderMarkdownBytes int    `json:"max_provider_markdown_bytes"`
	MaxTotalResultBytes      int    `json:"max_total_result_bytes"`
	MaxUnits                 int    `json:"max_units"`
	SourceEvidenceContract   string `json:"source_evidence_contract"`
}

// ArtifactPolicyV1 permits only one bounded structured-evidence artifact.
type ArtifactPolicyV1 struct {
	AllowedRoles     []document.EvidenceArtifactRole `json:"allowed_roles"`
	MaxArtifactBytes int64                           `json:"max_artifact_bytes"`
	MaxArtifacts     int                             `json:"max_artifacts"`
}

// ProfileV1 is the immutable compatibility identity expected from an
// operator-network Apache Tika bridge deployment.
type ProfileV1 struct {
	ArtifactPolicy    ArtifactPolicyV1                     `json:"artifact_policy"`
	BridgeContract    string                               `json:"bridge_contract"`
	ContractVersion   string                               `json:"contract_version"`
	CredentialBinding string                               `json:"credential_binding"`
	DeploymentID      string                               `json:"deployment_id"`
	Disclosure        DisclosurePolicyV1                   `json:"disclosure"`
	EvidencePolicy    EvidencePolicyV1                     `json:"evidence_policy"`
	InputKind         document.RenditionInputKind          `json:"input_kind"`
	Limits            LimitsV1                             `json:"limits"`
	PolicyFingerprint string                               `json:"policy_fingerprint"`
	ReferencePolicy   ReferencePolicyV1                    `json:"reference_policy"`
	RuntimeID         string                               `json:"runtime_id"`
	SupportedFormats  []document.RenditionFormatCapability `json:"supported_formats"`
	TrustBoundary     document.RenditionTrustBoundary      `json:"trust_boundary"`
}

// NewProfile returns the standard profile with operator-pinned deployment and
// runtime identity and an optional named credential binding.
func NewProfile(config Config) (ProfileV1, error) {
	profile := ProfileV1{
		ArtifactPolicy: standardArtifactPolicy(),
		BridgeContract: bridge.ContractVersion, ContractVersion: ProfileContractV1,
		CredentialBinding: config.CredentialBinding, DeploymentID: config.DeploymentID,
		Disclosure:      standardDisclosurePolicy(),
		EvidencePolicy:  standardEvidencePolicy(),
		InputKind:       document.RenditionInputOriginalFile,
		Limits:          standardLimits(),
		ReferencePolicy: standardReferencePolicy(),
		RuntimeID:       config.RuntimeID, SupportedFormats: standardFormats(),
		TrustBoundary: document.RenditionTrustOperatorNetwork,
	}
	_, fingerprint, err := CanonicalProfile(profile)
	if err != nil {
		return ProfileV1{}, err
	}
	profile.PolicyFingerprint = fingerprint
	return profile, nil
}

// CanonicalProfile validates, sorts, and deterministically encodes a profile.
// A populated fingerprint must match the canonical identity.
func CanonicalProfile(profile ProfileV1) ([]byte, string, error) {
	return profileCodec.Canonical(profile)
}

// ParseProfile accepts only the exact canonical v1 representation.
func ParseProfile(raw []byte) (ProfileV1, error) {
	return profileCodec.Parse(raw)
}

// BridgeProfile projects the compatibility identity into the generic hardened
// bridge. Origin remains generic bridge configuration, not profile schema.
func BridgeProfile(profile ProfileV1, origin string) (bridge.Profile, error) {
	_, fingerprint, err := CanonicalProfile(profile)
	if err != nil {
		return bridge.Profile{}, err
	}
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: descriptorID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: fingerprint, TrustBoundary: document.RenditionTrustOperatorNetwork,
		SupportedFormats: slices.Clone(profile.SupportedFormats), ReturnsMarkdown: true,
		ReturnsStructured: true,
		ArtifactRoles:     slices.Clone(profile.ArtifactPolicy.AllowedRoles),
	})
	if err != nil {
		return bridge.Profile{}, fmt.Errorf("tika: construct bridge descriptor: %w", err)
	}
	return bridge.Profile{
		Origin: origin, Descriptor: descriptor, SecretBinding: profile.CredentialBinding,
		RequestTimeout:  time.Duration(profile.Limits.RequestTimeoutMillis) * time.Millisecond,
		TotalTimeout:    time.Duration(profile.Limits.TotalTimeoutMillis) * time.Millisecond,
		PollInterval:    time.Duration(profile.Limits.PollIntervalMillis) * time.Millisecond,
		MaxPollAttempts: profile.Limits.MaxPollAttempts, MaxResponseBytes: profile.Limits.MaxResponseBytes,
		MaxDocumentBytes:         profile.Limits.MaxDocumentBytes,
		MaxProviderMarkdownBytes: profile.EvidencePolicy.MaxProviderMarkdownBytes,
		MaxArtifactBytes:         int(profile.ArtifactPolicy.MaxArtifactBytes),
		MaxArtifacts:             profile.ArtifactPolicy.MaxArtifacts,
		MaxTotalResultBytes:      profile.EvidencePolicy.MaxTotalResultBytes,
	}, nil
}

func validateProfile(profile ProfileV1) error {
	if profile.ContractVersion != ProfileContractV1 || profile.BridgeContract != bridge.ContractVersion {
		return errors.New("contract version is invalid")
	}
	if err := bridgeprofile.ValidateIdentity(profile.DeploymentID, "deployment ID", false, 256); err != nil {
		return err
	}
	if err := bridgeprofile.ValidateIdentity(profile.RuntimeID, "runtime ID", true, 256); err != nil {
		return err
	}
	if profile.CredentialBinding != "" {
		if err := provider.ValidateIdentifier(profile.CredentialBinding, "credential binding"); err != nil {
			return err
		}
	}
	if profile.TrustBoundary != document.RenditionTrustOperatorNetwork ||
		profile.InputKind != document.RenditionInputOriginalFile ||
		profile.Disclosure != standardDisclosurePolicy() {
		return errors.New("disclosure or execution boundary is invalid")
	}
	if profile.ReferencePolicy != standardReferencePolicy() {
		return errors.New("embedded and external reference fetching must be refused")
	}
	if !slices.Equal(profile.SupportedFormats, standardFormats()) {
		return errors.New("supported formats differ from the standard profile")
	}
	if !slices.Equal(profile.ArtifactPolicy.AllowedRoles,
		[]document.EvidenceArtifactRole{document.EvidenceArtifactStructured}) {
		return errors.New("artifact roles differ from the standard profile")
	}
	standardArtifacts := standardArtifactPolicy()
	if profile.Limits != standardLimits() || profile.EvidencePolicy != standardEvidencePolicy() ||
		profile.ArtifactPolicy.MaxArtifactBytes != standardArtifacts.MaxArtifactBytes ||
		profile.ArtifactPolicy.MaxArtifacts != standardArtifacts.MaxArtifacts {
		return errors.New("limits differ from the finite standard profile")
	}
	return nil
}

func standardLimits() LimitsV1 {
	return LimitsV1{
		MaxDocumentBytes: 100 << 20, MaxPollAttempts: 300, MaxResponseBytes: 128 << 20,
		PollIntervalMillis: 1_000, RequestTimeoutMillis: 30_000, TotalTimeoutMillis: 600_000,
	}
}

func standardDisclosurePolicy() DisclosurePolicyV1 {
	return DisclosurePolicyV1{DiscloseFilename: true, Source: "exact_supplied_bytes"}
}

func standardReferencePolicy() ReferencePolicyV1 {
	return ReferencePolicyV1{
		EmbeddedReferenceFetch: "refuse", EnforcementBoundary: "pinned_audited_adapter_runtime",
		ExternalReferenceFetch: "refuse",
	}
}

func standardEvidencePolicy() EvidencePolicyV1 {
	return EvidencePolicyV1{
		MaxProviderMarkdownBytes: 32 << 20, MaxTotalResultBytes: 128 << 20,
		MaxUnits: 100_000, SourceEvidenceContract: document.SourceEvidenceContractV1,
	}
}

func standardArtifactPolicy() ArtifactPolicyV1 {
	return ArtifactPolicyV1{
		AllowedRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
		MaxArtifactBytes: 64 << 20, MaxArtifacts: 1,
	}
}

func standardFormats() []document.RenditionFormatCapability {
	return bridgeprofile.BroadOriginalFormats()
}

func cloneProfile(profile ProfileV1) ProfileV1 {
	profile.SupportedFormats = slices.Clone(profile.SupportedFormats)
	profile.ArtifactPolicy.AllowedRoles = slices.Clone(profile.ArtifactPolicy.AllowedRoles)
	return profile
}
