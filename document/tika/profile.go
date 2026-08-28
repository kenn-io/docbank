// Package tika defines the fixed compatibility profile used when an operator
// deploys an Apache Tika adapter behind docbank-rendition/v1.
package tika

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bridge"
)

const (
	// ProfileContractV1 identifies the canonical Apache Tika bridge profile.
	ProfileContractV1         = "tika-bridge-profile/v1"
	descriptorID              = "tika.bridge.v1"
	maxCredentialBindingBytes = 128
)

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
	canonical := cloneProfile(profile)
	slices.SortFunc(canonical.SupportedFormats, compareFormats)
	slices.Sort(canonical.ArtifactPolicy.AllowedRoles)
	claimed := canonical.PolicyFingerprint
	canonical.PolicyFingerprint = ""
	if err := validateProfile(canonical); err != nil {
		return nil, "", fmt.Errorf("tika: invalid profile: %w", err)
	}
	identityJSON, err := json.Marshal(canonical, json.Deterministic(true))
	if err != nil {
		return nil, "", fmt.Errorf("tika: encode profile identity: %w", err)
	}
	digest := sha256.Sum256(identityJSON)
	fingerprint := hex.EncodeToString(digest[:])
	if claimed != "" && claimed != fingerprint {
		return nil, "", errors.New("tika: policy fingerprint does not match canonical profile")
	}
	canonical.PolicyFingerprint = fingerprint
	encoded, err := json.Marshal(canonical, json.Deterministic(true))
	if err != nil {
		return nil, "", fmt.Errorf("tika: encode canonical profile: %w", err)
	}
	return encoded, fingerprint, nil
}

// ParseProfile accepts only the exact canonical v1 representation.
func ParseProfile(raw []byte) (ProfileV1, error) {
	var profile ProfileV1
	if err := json.Unmarshal(raw, &profile, json.RejectUnknownMembers(true)); err != nil {
		return ProfileV1{}, fmt.Errorf("tika: decode profile: %w", err)
	}
	canonical, _, err := CanonicalProfile(profile)
	if err != nil {
		return ProfileV1{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return ProfileV1{}, errors.New("tika: profile is not canonical")
	}
	return cloneProfile(profile), nil
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
		MaxSourceBytes:           profile.Limits.MaxDocumentBytes,
		MaxProviderMarkdownBytes: profile.EvidencePolicy.MaxProviderMarkdownBytes,
		MaxArtifactBytes:         int(profile.ArtifactPolicy.MaxArtifactBytes),
		MaxArtifacts:             profile.ArtifactPolicy.MaxArtifacts,
		MaxTotalResultBytes:      profile.EvidencePolicy.MaxTotalResultBytes,
		MaxEvidenceUnits:         profile.EvidencePolicy.MaxUnits,
	}, nil
}

func validateProfile(profile ProfileV1) error {
	if profile.ContractVersion != ProfileContractV1 || profile.BridgeContract != bridge.ContractVersion {
		return errors.New("contract version is invalid")
	}
	if err := validateIdentity(profile.DeploymentID, "deployment ID", false); err != nil {
		return err
	}
	if err := validateIdentity(profile.RuntimeID, "runtime ID", true); err != nil {
		return err
	}
	if profile.CredentialBinding != "" {
		if len(profile.CredentialBinding) > maxCredentialBindingBytes {
			return errors.New("credential binding is invalid")
		}
		if err := validateIdentity(profile.CredentialBinding, "credential binding", false); err != nil {
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

func validateIdentity(value, subject string, allowColon bool) error {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is invalid", subject)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._-", character) ||
			allowColon && character == ':' {
			continue
		}
		return fmt.Errorf("%s is invalid", subject)
	}
	return nil
}

func standardFormats() []document.RenditionFormatCapability {
	formats := []document.RenditionFormatCapability{
		{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "application/vnd.oasis.opendocument.spreadsheet", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "text/csv", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "ebook", MediaType: "application/epub+zip", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "mail", MediaType: "message/rfc822", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "structured", MediaType: "application/xml", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/markdown", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile},
	}
	slices.SortFunc(formats, compareFormats)
	return formats
}

func compareFormats(left, right document.RenditionFormatCapability) int {
	if comparison := strings.Compare(left.MediaFamily, right.MediaFamily); comparison != 0 {
		return comparison
	}
	if comparison := strings.Compare(left.MediaType, right.MediaType); comparison != 0 {
		return comparison
	}
	return strings.Compare(string(left.InputKind), string(right.InputKind))
}

func cloneProfile(profile ProfileV1) ProfileV1 {
	profile.SupportedFormats = slices.Clone(profile.SupportedFormats)
	profile.ArtifactPolicy.AllowedRoles = slices.Clone(profile.ArtifactPolicy.AllowedRoles)
	return profile
}
