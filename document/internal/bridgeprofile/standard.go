package bridgeprofile

import (
	"errors"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bridge"
	"go.kenn.io/docbank/document/internal/providerutil"
)

// Limits fixes every finite input, output, polling, and wall-clock bound.
type Limits struct {
	MaxDocumentBytes     int64 `json:"max_document_bytes"`
	MaxPollAttempts      int   `json:"max_poll_attempts"`
	MaxResponseBytes     int64 `json:"max_response_bytes"`
	PollIntervalMillis   int64 `json:"poll_interval_millis"`
	RequestTimeoutMillis int64 `json:"request_timeout_millis"`
	TotalTimeoutMillis   int64 `json:"total_timeout_millis"`
}

// DisclosurePolicy permits only the exact supplied bytes and records the
// bridge's safe-basename disclosure. Authorization binds every byte and tuple.
type DisclosurePolicy struct {
	DiscloseFilename bool   `json:"disclose_filename"`
	Source           string `json:"source"`
}

// EvidencePolicy fixes bounded provider-neutral evidence and Markdown.
type EvidencePolicy struct {
	MaxProviderMarkdownBytes int    `json:"max_provider_markdown_bytes"`
	MaxTotalResultBytes      int    `json:"max_total_result_bytes"`
	MaxUnits                 int    `json:"max_units"`
	SourceEvidenceContract   string `json:"source_evidence_contract"`
}

// ArtifactPolicy permits only one bounded structured-evidence artifact.
type ArtifactPolicy struct {
	AllowedRoles     []document.EvidenceArtifactRole `json:"allowed_roles"`
	MaxArtifactBytes int64                           `json:"max_artifact_bytes"`
	MaxArtifacts     int                             `json:"max_artifacts"`
}

func StandardLimits() Limits {
	return Limits{
		MaxDocumentBytes: 100 << 20, MaxPollAttempts: 300, MaxResponseBytes: 128 << 20,
		PollIntervalMillis: 1_000, RequestTimeoutMillis: 30_000, TotalTimeoutMillis: 600_000,
	}
}

func StandardDisclosurePolicy() DisclosurePolicy {
	return DisclosurePolicy{DiscloseFilename: true, Source: "exact_supplied_bytes"}
}

func StandardEvidencePolicy() EvidencePolicy {
	return EvidencePolicy{
		MaxProviderMarkdownBytes: 32 << 20, MaxTotalResultBytes: 128 << 20,
		MaxUnits: 100_000, SourceEvidenceContract: document.SourceEvidenceContractV1,
	}
}

func StandardArtifactPolicy() ArtifactPolicy {
	return ArtifactPolicy{
		AllowedRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
		MaxArtifactBytes: 64 << 20, MaxArtifacts: 1,
	}
}

// ValidateOperatorIdentity checks the pinned deployment, runtime and optional
// credential binding used by fixed bridge profiles.
func ValidateOperatorIdentity(provider providerutil.Provider, deploymentID, runtimeID, credentialBinding string) error {
	if err := ValidateIdentity(deploymentID, "deployment ID", false, 256); err != nil {
		return err
	}
	if err := ValidateIdentity(runtimeID, "runtime ID", true, 256); err != nil {
		return err
	}
	if credentialBinding != "" {
		return provider.ValidateIdentifier(credentialBinding, "credential binding")
	}
	return nil
}

// ValidateBroadOriginalOutput requires the formats and finite bounds shared by
// the fixed Tika and Unstructured profiles.
func ValidateBroadOriginalOutput(formats []document.RenditionFormatCapability, limits Limits, evidence EvidencePolicy, artifacts ArtifactPolicy) error {
	if !slices.Equal(formats, BroadOriginalFormats()) {
		return errors.New("supported formats differ from the standard profile")
	}
	if !slices.Equal(artifacts.AllowedRoles, []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}) {
		return errors.New("artifact roles differ from the standard profile")
	}
	standardArtifacts := StandardArtifactPolicy()
	if limits != StandardLimits() || evidence != StandardEvidencePolicy() ||
		artifacts.MaxArtifactBytes != standardArtifacts.MaxArtifactBytes || artifacts.MaxArtifacts != standardArtifacts.MaxArtifacts {
		return errors.New("limits differ from the finite standard profile")
	}
	return nil
}

// BroadOriginalBridgeProfile projects a validated fixed broad-format profile
// into the shared bridge runtime.
func BroadOriginalBridgeProfile(descriptorID, fingerprint, binding, origin string) (bridge.Profile, error) {
	artifacts := StandardArtifactPolicy()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: descriptorID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: fingerprint, TrustBoundary: document.RenditionTrustOperatorNetwork,
		SupportedFormats: BroadOriginalFormats(), ReturnsMarkdown: true,
		ReturnsStructured: true, ArtifactRoles: artifacts.AllowedRoles,
	})
	if err != nil {
		return bridge.Profile{}, err
	}
	limits, evidence := StandardLimits(), StandardEvidencePolicy()
	return bridge.Profile{
		Origin: origin, Descriptor: descriptor, SecretBinding: binding,
		RequestTimeout:  time.Duration(limits.RequestTimeoutMillis) * time.Millisecond,
		TotalTimeout:    time.Duration(limits.TotalTimeoutMillis) * time.Millisecond,
		PollInterval:    time.Duration(limits.PollIntervalMillis) * time.Millisecond,
		MaxPollAttempts: limits.MaxPollAttempts, MaxResponseBytes: limits.MaxResponseBytes,
		MaxDocumentBytes:         limits.MaxDocumentBytes,
		MaxProviderMarkdownBytes: evidence.MaxProviderMarkdownBytes,
		MaxArtifactBytes:         int(artifacts.MaxArtifactBytes), MaxArtifacts: artifacts.MaxArtifacts,
		MaxTotalResultBytes: evidence.MaxTotalResultBytes,
	}, nil
}
