// Package tika defines the fixed compatibility profile used when an operator
// deploys an Apache Tika adapter behind docbank-rendition/v1.
package tika

import (
	"errors"
	"fmt"
	"slices"

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
type LimitsV1 bridgeprofile.Limits

// DisclosurePolicyV1 permits only exact supplied bytes and records the
// bridge's safe-basename disclosure. Authorization binds every byte and tuple.
type DisclosurePolicyV1 bridgeprofile.DisclosurePolicy

// ReferencePolicyV1 refuses both embedded and external reference fetching.
// The generic bridge cannot inspect parser internals, so compatibility requires
// an operator-pinned adapter runtime audited to enforce both refusals.
type ReferencePolicyV1 struct {
	EmbeddedReferenceFetch string `json:"embedded_reference_fetch"`
	EnforcementBoundary    string `json:"enforcement_boundary"`
	ExternalReferenceFetch string `json:"external_reference_fetch"`
}

// EvidencePolicyV1 fixes bounded provider-neutral evidence and Markdown.
type EvidencePolicyV1 bridgeprofile.EvidencePolicy

// ArtifactPolicyV1 permits only one bounded structured-evidence artifact.
type ArtifactPolicyV1 bridgeprofile.ArtifactPolicy

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
		ArtifactPolicy: ArtifactPolicyV1(bridgeprofile.StandardArtifactPolicy()),
		BridgeContract: bridge.ContractVersion, ContractVersion: ProfileContractV1,
		CredentialBinding: config.CredentialBinding, DeploymentID: config.DeploymentID,
		Disclosure:      DisclosurePolicyV1(bridgeprofile.StandardDisclosurePolicy()),
		EvidencePolicy:  EvidencePolicyV1(bridgeprofile.StandardEvidencePolicy()),
		InputKind:       document.RenditionInputOriginalFile,
		Limits:          LimitsV1(bridgeprofile.StandardLimits()),
		ReferencePolicy: standardReferencePolicy(),
		RuntimeID:       config.RuntimeID, SupportedFormats: bridgeprofile.BroadOriginalFormats(),
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
	projected, err := bridgeprofile.BroadOriginalBridgeProfile(descriptorID, fingerprint, profile.CredentialBinding, origin)
	if err != nil {
		return bridge.Profile{}, fmt.Errorf("tika: construct bridge descriptor: %w", err)
	}
	return projected, nil
}

func validateProfile(profile ProfileV1) error {
	if profile.ContractVersion != ProfileContractV1 || profile.BridgeContract != bridge.ContractVersion {
		return errors.New("contract version is invalid")
	}
	if err := bridgeprofile.ValidateOperatorIdentity(provider, profile.DeploymentID, profile.RuntimeID, profile.CredentialBinding); err != nil {
		return err
	}
	if profile.TrustBoundary != document.RenditionTrustOperatorNetwork ||
		profile.InputKind != document.RenditionInputOriginalFile ||
		profile.Disclosure != DisclosurePolicyV1(bridgeprofile.StandardDisclosurePolicy()) {
		return errors.New("disclosure or execution boundary is invalid")
	}
	if profile.ReferencePolicy != standardReferencePolicy() {
		return errors.New("embedded and external reference fetching must be refused")
	}
	return bridgeprofile.ValidateBroadOriginalOutput(profile.SupportedFormats,
		bridgeprofile.Limits(profile.Limits), bridgeprofile.EvidencePolicy(profile.EvidencePolicy), bridgeprofile.ArtifactPolicy(profile.ArtifactPolicy))
}

func standardReferencePolicy() ReferencePolicyV1 {
	return ReferencePolicyV1{
		EmbeddedReferenceFetch: "refuse", EnforcementBoundary: "pinned_audited_adapter_runtime",
		ExternalReferenceFetch: "refuse",
	}
}

func cloneProfile(profile ProfileV1) ProfileV1 {
	profile.SupportedFormats = slices.Clone(profile.SupportedFormats)
	profile.ArtifactPolicy.AllowedRoles = slices.Clone(profile.ArtifactPolicy.AllowedRoles)
	return profile
}
