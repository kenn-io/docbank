package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	// RenditionExecutionIdentityContractV1 is the stable shared-build input
	// contract. It excludes only the transient authorization interval.
	RenditionExecutionIdentityContractV1 = "rendition-execution-identity/v1"
	// RenditionExecutionSnapshotContractV1 is the sealed durable provider
	// execution authority retained for a known resume handle.
	RenditionExecutionSnapshotContractV1 = "rendition-execution-snapshot/v1"
)

// EvidencePolicyIdentity is every effective provider-evidence normalization
// bound. Keeping the fixed v1 values explicit makes future policy changes
// split shared builds instead of silently reusing different output semantics.
type EvidencePolicyIdentity struct {
	MaxArtifacts      int `json:"max_artifacts"`
	MaxCellsPerTable  int `json:"max_cells_per_table"`
	MaxDocumentChars  int `json:"max_document_chars"`
	MaxOmissions      int `json:"max_omissions"`
	MaxRegionsPerUnit int `json:"max_regions_per_unit"`
	MaxTablesPerUnit  int `json:"max_tables_per_unit"`
	MaxUnits          int `json:"max_units"`
}

// Identity returns every value that can affect normalized evidence.
func (policy EvidencePolicy) Identity() EvidencePolicyIdentity {
	return EvidencePolicyIdentity{
		MaxArtifacts: policy.maxArtifacts, MaxCellsPerTable: policy.maxCellsPerTable,
		MaxDocumentChars: policy.maxDocumentChars, MaxOmissions: policy.maxOmissions,
		MaxRegionsPerUnit: policy.maxRegionsPerUnit,
		MaxTablesPerUnit:  policy.maxTablesPerUnit, MaxUnits: policy.maxUnits,
	}
}

// RenditionPolicyIdentity is every value that can affect normalized Markdown,
// units, and lexical segments.
type RenditionPolicyIdentity struct {
	MaxDocumentChars   int `json:"max_document_chars"`
	MaxLinkChars       int `json:"max_link_chars"`
	MaxSegmentRunes    int `json:"max_segment_runes"`
	MaxSourceUnitBytes int `json:"max_source_unit_bytes"`
	MaxUnitRunes       int `json:"max_unit_runes"`
}

// Identity returns every effective rendition-construction policy value.
func (policy RenditionPolicy) Identity() RenditionPolicyIdentity {
	return RenditionPolicyIdentity{
		MaxDocumentChars: policy.maxDocumentChars, MaxLinkChars: policy.maxLinkChars,
		MaxSegmentRunes: policy.maxSegmentRunes, MaxSourceUnitBytes: policy.maxSourceUnitBytes,
		MaxUnitRunes: policy.maxUnitRunes,
	}
}

// RenditionAuthorizationIdentityV1 projects the stable provider-visible input
// and output bounds from a sealed authorization, deliberately omitting only
// AuthorizedAt and ExpiresAt.
type RenditionAuthorizationIdentityV1 struct {
	ProviderID                  string                 `json:"provider_id"`
	DescriptorFingerprint       string                 `json:"descriptor_fingerprint"`
	PolicyFingerprint           string                 `json:"policy_fingerprint"`
	RenditionRequestFingerprint string                 `json:"rendition_request_fingerprint"`
	SourceSHA256                string                 `json:"source_sha256"`
	SourceBytes                 int64                  `json:"source_bytes"`
	CapabilityRecordChecksum    string                 `json:"capability_record_checksum"`
	ProviderMetadataChecksum    string                 `json:"provider_metadata_checksum"`
	MediaFamily                 string                 `json:"media_family"`
	MediaType                   string                 `json:"media_type"`
	InputKind                   RenditionInputKind     `json:"input_kind"`
	DiscloseFilename            bool                   `json:"disclose_filename"`
	AllowedArtifactRoles        []EvidenceArtifactRole `json:"allowed_artifact_roles"`
	MaxProviderMarkdownBytes    int                    `json:"max_provider_markdown_bytes"`
	MaxArtifactBytes            int                    `json:"max_artifact_bytes"`
	MaxArtifacts                int                    `json:"max_artifacts"`
	MaxTotalResultBytes         int                    `json:"max_total_result_bytes"`
}

// RenditionExecutionIdentityV1 is the complete stable identity of a shared
// provider execution and its deterministic local normalization.
type RenditionExecutionIdentityV1 struct {
	ContractVersion string                           `json:"contract_version"`
	Upload          AuthorizedUploadMetadata         `json:"upload"`
	Authorization   RenditionAuthorizationIdentityV1 `json:"authorization"`
	EvidencePolicy  EvidencePolicyIdentity           `json:"evidence_policy"`
	RenditionPolicy RenditionPolicyIdentity          `json:"rendition_policy"`
}

// RenditionExecutionSnapshotV1 retains the original sealed authorization
// interval alongside its stable identity. It is provider-neutral and contains
// no source bytes, credentials, paths, provider bodies, or HTTP state.
type RenditionExecutionSnapshotV1 struct {
	ContractVersion string                       `json:"contract_version"`
	Identity        RenditionExecutionIdentityV1 `json:"identity"`
	Authorization   RenditionAuthorization       `json:"authorization"`
}

// NewRenditionExecutionIdentityV1 constructs the canonical stable execution
// identity from a planned sealed request and local output policies.
func NewRenditionExecutionIdentityV1(
	metadata AuthorizedUploadMetadata, authorization RenditionAuthorization,
	evidence EvidencePolicy, rendition RenditionPolicy,
) (RenditionExecutionIdentityV1, error) {
	authorization = canonicalExecutionAuthorization(authorization)
	identity := RenditionExecutionIdentityV1{
		ContractVersion: RenditionExecutionIdentityContractV1,
		Upload:          metadata,
		Authorization:   renditionAuthorizationIdentity(authorization),
		EvidencePolicy:  evidence.Identity(),
		RenditionPolicy: rendition.Identity(),
	}
	identity = canonicalExecutionIdentity(identity)
	if _, _, err := CanonicalRenditionExecutionIdentityV1(identity); err != nil {
		return RenditionExecutionIdentityV1{}, err
	}
	return cloneRenditionExecutionIdentity(identity), nil
}

// CanonicalRenditionExecutionIdentityV1 validates and deterministically
// encodes a stable identity and returns its SHA-256 fingerprint.
func CanonicalRenditionExecutionIdentityV1(
	identity RenditionExecutionIdentityV1,
) ([]byte, string, error) {
	identity = canonicalExecutionIdentity(identity)
	if err := validateRenditionExecutionIdentity(identity); err != nil {
		return nil, "", err
	}
	encoded, err := canonical.Marshal(identity)
	if err != nil {
		return nil, "", fmt.Errorf("encoding rendition execution identity: %w", err)
	}
	return encoded, sha256Hex(encoded), nil
}

// ParseRenditionExecutionIdentityV1 decodes only the exact canonical form.
func ParseRenditionExecutionIdentityV1(raw []byte) (RenditionExecutionIdentityV1, error) {
	identity, err := canonical.Decode[RenditionExecutionIdentityV1](raw)
	if err != nil {
		return RenditionExecutionIdentityV1{}, fmt.Errorf("decoding rendition execution identity: %w", err)
	}
	encoded, _, err := CanonicalRenditionExecutionIdentityV1(identity)
	if err != nil {
		return RenditionExecutionIdentityV1{}, err
	}
	if !bytes.Equal(raw, encoded) {
		return RenditionExecutionIdentityV1{}, errors.New("rendition execution identity is not canonical")
	}
	return cloneRenditionExecutionIdentity(identity), nil
}

// ValidateRenditionExecutionProfileV1 binds a durable execution identity to
// the executable provider and output policy selected by an immutable profile.
// Callers use this same provider-neutral check at enqueue, restore, and resume
// boundaries so provider adapters cannot drift on disclosure behavior.
func ValidateRenditionExecutionProfileV1(
	identity RenditionExecutionIdentityV1, profile ProcessingProfileV1,
) error {
	identity = canonicalExecutionIdentity(identity)
	if err := validateRenditionExecutionIdentity(identity); err != nil {
		return err
	}
	canonicalProfile, err := CanonicalizeProfile(profile)
	if err != nil {
		return err
	}
	_, fingerprints, err := CanonicalProfile(canonicalProfile)
	if err != nil {
		return err
	}
	expectedEvidence, expectedRendition, err := renditionExecutionPoliciesForCanonicalProfileV1(canonicalProfile)
	if err != nil {
		return err
	}
	if canonicalProfile.Rendition == nil ||
		identity.Authorization.RenditionRequestFingerprint != fingerprints.RenditionRequest ||
		identity.Authorization.ProviderID != canonicalProfile.Rendition.Descriptor.ID ||
		identity.Authorization.DescriptorFingerprint != canonicalProfile.Rendition.Descriptor.Fingerprint ||
		identity.Authorization.DiscloseFilename != canonicalProfile.Rendition.DiscloseFilename ||
		identity.EvidencePolicy != expectedEvidence.Identity() ||
		identity.RenditionPolicy != expectedRendition.Identity() ||
		identity.Upload.InputKind != RenditionInputOriginalFile ||
		!renditionExecutionAuthorizationMatchesProfileV1(identity, *canonicalProfile.Rendition) {
		return errors.New("rendition execution identity does not match executable profile")
	}
	return nil
}

func renditionExecutionAuthorizationMatchesProfileV1(
	identity RenditionExecutionIdentityV1, binding RenditionBindingV1,
) bool {
	authorization := identity.Authorization
	if identity.Upload.ByteLength > binding.MaxDocumentBytes ||
		authorization.SourceBytes > binding.MaxDocumentBytes ||
		int64(authorization.MaxProviderMarkdownBytes) > binding.MaxResponseBytes ||
		int64(authorization.MaxArtifactBytes) > binding.MaxResponseBytes ||
		int64(authorization.MaxTotalResultBytes) > binding.MaxResponseBytes {
		return false
	}
	wantsMarkdown := false
	wantedArtifactRoles := make([]EvidenceArtifactRole, 0, len(binding.RequestedArtifacts))
	for _, role := range binding.RequestedArtifacts {
		if role == EvidenceArtifactMarkdown {
			wantsMarkdown = true
			continue
		}
		wantedArtifactRoles = append(wantedArtifactRoles, role)
	}
	if !slices.Equal(authorization.AllowedArtifactRoles, wantedArtifactRoles) ||
		wantsMarkdown != (authorization.MaxProviderMarkdownBytes > 0) {
		return false
	}
	if len(wantedArtifactRoles) == 0 {
		return authorization.MaxArtifacts == 0 && authorization.MaxArtifactBytes == 0
	}
	return authorization.MaxArtifacts > 0 && authorization.MaxArtifactBytes > 0
}

// RenditionExecutionPoliciesForProfileV1 constructs the complete executable
// evidence and rendition policies pinned by one immutable profile. Provider
// runtimes use this shared mapping instead of recreating policy defaults.
func RenditionExecutionPoliciesForProfileV1(
	profile ProcessingProfileV1,
) (EvidencePolicy, RenditionPolicy, error) {
	canonicalProfile, err := CanonicalizeProfile(profile)
	if err != nil {
		return EvidencePolicy{}, RenditionPolicy{}, err
	}
	return renditionExecutionPoliciesForCanonicalProfileV1(canonicalProfile)
}

func renditionExecutionPoliciesForCanonicalProfileV1(
	profile ProcessingProfileV1,
) (EvidencePolicy, RenditionPolicy, error) {
	if profile.Rendition == nil {
		return EvidencePolicy{}, RenditionPolicy{}, errors.New(
			"rendition execution profile has no rendition binding")
	}
	evidence, err := NewEvidencePolicy(profile.EvidenceLexical.MaxDocumentChars)
	if err != nil {
		return EvidencePolicy{}, RenditionPolicy{}, err
	}
	rendition, err := NewRenditionPolicy(RenditionLimits{
		MaxDocumentChars: profile.EvidenceLexical.MaxDocumentChars,
		MaxUnitRunes:     profile.EvidenceLexical.MaxUnitRunes,
		MaxSegmentRunes:  profile.EvidenceLexical.MaxSegmentRunes,
	})
	if err != nil {
		return EvidencePolicy{}, RenditionPolicy{}, err
	}
	return evidence, rendition, nil
}

// SealRenditionExecutionAt validates the exact provider/upload request at the
// egress clock and captures the immutable authority needed for safe resume.
func SealRenditionExecutionAt(
	now time.Time, provider RenditionProvider, upload AuthorizedUpload,
	authorization RenditionAuthorization, evidence EvidencePolicy, rendition RenditionPolicy,
) (RenditionExecutionSnapshotV1, error) {
	authorization = canonicalExecutionAuthorization(authorization)
	if _, _, err := validateRenditionProviderRequestAt(now, provider, upload, authorization); err != nil {
		return RenditionExecutionSnapshotV1{}, err
	}
	identity, err := NewRenditionExecutionIdentityV1(
		upload.Metadata(), authorization, evidence, rendition)
	if err != nil {
		return RenditionExecutionSnapshotV1{}, err
	}
	return NewRenditionExecutionSnapshotV1(identity, authorization)
}

// NewRenditionExecutionSnapshotV1 combines a validated stable identity with
// its exact sealed authorization interval. Provider validation is performed by
// SealRenditionExecutionAt for new work and again by ResumeRendition before a
// known handle is consumed.
func NewRenditionExecutionSnapshotV1(
	identity RenditionExecutionIdentityV1, authorization RenditionAuthorization,
) (RenditionExecutionSnapshotV1, error) {
	snapshot := RenditionExecutionSnapshotV1{
		ContractVersion: RenditionExecutionSnapshotContractV1,
		Identity:        cloneRenditionExecutionIdentity(identity),
		Authorization:   canonicalExecutionAuthorization(authorization),
	}
	snapshot.Identity = canonicalExecutionIdentity(snapshot.Identity)
	if _, err := CanonicalRenditionExecutionSnapshotV1(snapshot); err != nil {
		return RenditionExecutionSnapshotV1{}, err
	}
	return cloneRenditionExecutionSnapshot(snapshot), nil
}

// CanonicalRenditionExecutionSnapshotV1 validates and deterministically
// encodes the provider-neutral durable resume authority.
func CanonicalRenditionExecutionSnapshotV1(snapshot RenditionExecutionSnapshotV1) ([]byte, error) {
	snapshot = cloneRenditionExecutionSnapshot(snapshot)
	snapshot.Identity = canonicalExecutionIdentity(snapshot.Identity)
	if err := validateRenditionExecutionSnapshot(snapshot); err != nil {
		return nil, err
	}
	encoded, err := canonical.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encoding rendition execution snapshot: %w", err)
	}
	return encoded, nil
}

// ParseRenditionExecutionSnapshotV1 decodes only exact canonical snapshots.
func ParseRenditionExecutionSnapshotV1(raw []byte) (RenditionExecutionSnapshotV1, error) {
	snapshot, err := canonical.Decode[RenditionExecutionSnapshotV1](raw)
	if err != nil {
		return RenditionExecutionSnapshotV1{}, fmt.Errorf("decoding rendition execution snapshot: %w", err)
	}
	encoded, err := CanonicalRenditionExecutionSnapshotV1(snapshot)
	if err != nil {
		return RenditionExecutionSnapshotV1{}, err
	}
	if !bytes.Equal(raw, encoded) {
		return RenditionExecutionSnapshotV1{}, errors.New("rendition execution snapshot is not canonical")
	}
	return cloneRenditionExecutionSnapshot(snapshot), nil
}

// Policies reconstructs the executable local policies from their complete
// frozen identities.
func (snapshot RenditionExecutionSnapshotV1) Policies() (
	EvidencePolicy, RenditionPolicy, error,
) {
	if err := validateRenditionExecutionSnapshot(snapshot); err != nil {
		return EvidencePolicy{}, RenditionPolicy{}, err
	}
	evidence, err := evidencePolicyFromIdentity(snapshot.Identity.EvidencePolicy)
	if err != nil {
		return EvidencePolicy{}, RenditionPolicy{}, err
	}
	rendition, err := renditionPolicyFromIdentity(snapshot.Identity.RenditionPolicy)
	if err != nil {
		return EvidencePolicy{}, RenditionPolicy{}, err
	}
	return evidence, rendition, nil
}

// ResumeRendition continues exactly one provider-issued durable operation.
// It deliberately does not require the original upload and does not reopen a
// transient authorization window; result receipts remain bound to the sealed
// original interval.
func ResumeRendition(
	ctx context.Context, provider RenditionProvider, snapshot RenditionExecutionSnapshotV1,
	handle RenditionResumeHandle, checkpoint RenditionResumeCheckpoint,
) (RenditionResult, error) {
	if nilInterface(provider) {
		return RenditionResult{}, errors.New("rendition provider is required")
	}
	if err := validateRenditionResumeHandle(handle); err != nil {
		return RenditionResult{}, err
	}
	snapshot = cloneRenditionExecutionSnapshot(snapshot)
	if err := validateRenditionExecutionSnapshot(snapshot); err != nil {
		return RenditionResult{}, err
	}
	descriptor := cloneRenditionDescriptor(provider.Descriptor())
	if err := validateRenditionDescriptor(descriptor); err != nil {
		return RenditionResult{}, err
	}
	if second := cloneRenditionDescriptor(provider.Descriptor()); !equalRenditionDescriptors(descriptor, second) {
		return RenditionResult{}, errors.New("rendition descriptor changed during resume validation")
	}
	if err := validateRenditionAuthorization(
		descriptor, snapshot.Identity.Upload, snapshot.Authorization); err != nil {
		return RenditionResult{}, err
	}
	resumable, ok := provider.(ResumableRenditionProvider)
	if !ok {
		return RenditionResult{}, errors.New("rendition provider does not support durable resume")
	}
	checkedCheckpoint := newRenditionCheckpoint(checkpoint)
	result, err := resumable.RenderResumable(
		ctx, nil, cloneRenditionAuthorization(snapshot.Authorization), &handle, checkedCheckpoint.Record)
	if checkpointErr := checkedCheckpoint.Err(); checkpointErr != nil {
		return RenditionResult{}, checkpointErr
	}
	if err != nil {
		if classified := ValidateRenditionProviderError(err); classified != nil {
			return RenditionResult{}, classified
		}
		return RenditionResult{}, err
	}
	return validateAndOwnRenditionResult(descriptor, snapshot.Authorization, result)
}

func validateRenditionExecutionIdentity(identity RenditionExecutionIdentityV1) error {
	if identity.ContractVersion != RenditionExecutionIdentityContractV1 {
		return errors.New("rendition execution identity contract is invalid")
	}
	if err := validateAuthorizedUploadMetadata(identity.Upload); err != nil {
		return err
	}
	authorization := identity.Authorization
	if err := validateStableToken(authorization.ProviderID, "authorization provider ID", 128); err != nil {
		return err
	}
	for subject, value := range map[string]string{
		"authorization descriptor fingerprint":        authorization.DescriptorFingerprint,
		"authorization policy fingerprint":            authorization.PolicyFingerprint,
		"authorization rendition request fingerprint": authorization.RenditionRequestFingerprint,
		"authorization source SHA-256":                authorization.SourceSHA256,
		"authorization capability record checksum":    authorization.CapabilityRecordChecksum,
		"authorization provider metadata checksum":    authorization.ProviderMetadataChecksum,
	} {
		if err := validateFingerprint(value, subject); err != nil {
			return err
		}
	}
	if authorization.SourceSHA256 != identity.Upload.SHA256 ||
		authorization.SourceBytes != identity.Upload.ByteLength ||
		authorization.CapabilityRecordChecksum != identity.Upload.CapabilityRecordChecksum ||
		authorization.ProviderMetadataChecksum != identity.Upload.ProviderMetadataChecksum ||
		authorization.MediaFamily != identity.Upload.MediaFamily ||
		authorization.MediaType != identity.Upload.MediaType ||
		authorization.InputKind != identity.Upload.InputKind {
		return errors.New("rendition execution authorization does not match upload metadata")
	}
	if !authorization.DiscloseFilename && identity.Upload.Filename != "" {
		return errors.New("rendition execution identity discloses a withheld filename")
	}
	if authorization.DiscloseFilename && identity.Upload.Filename == "" {
		return errors.New("rendition execution identity omits the disclosed filename")
	}
	if !slices.IsSorted(authorization.AllowedArtifactRoles) {
		return errors.New("rendition execution artifact roles are not canonical")
	}
	seen := make(map[EvidenceArtifactRole]struct{}, len(authorization.AllowedArtifactRoles))
	for _, role := range authorization.AllowedArtifactRoles {
		if !validProfileArtifactRole(role) {
			return fmt.Errorf("rendition execution artifact role %q is invalid", role)
		}
		if _, ok := seen[role]; ok {
			return fmt.Errorf("rendition execution artifact role %q is duplicated", role)
		}
		seen[role] = struct{}{}
	}
	if authorization.SourceBytes <= 0 || authorization.SourceBytes > maxRenditionSourceBytes ||
		authorization.MaxProviderMarkdownBytes < 0 || authorization.MaxProviderMarkdownBytes > maxRenditionMarkdownBytes ||
		authorization.MaxArtifactBytes < 0 || authorization.MaxArtifactBytes > maxRenditionArtifactBytes ||
		authorization.MaxArtifacts < 0 || authorization.MaxArtifacts > maxRenditionArtifacts ||
		authorization.MaxArtifacts > len(authorization.AllowedArtifactRoles) ||
		authorization.MaxTotalResultBytes <= 0 || authorization.MaxTotalResultBytes > maxRenditionTotalResultBytes {
		return errors.New("rendition execution authorization bounds are invalid")
	}
	if _, err := evidencePolicyFromIdentity(identity.EvidencePolicy); err != nil {
		return err
	}
	_, err := renditionPolicyFromIdentity(identity.RenditionPolicy)
	return err
}

func validateRenditionExecutionSnapshot(snapshot RenditionExecutionSnapshotV1) error {
	if snapshot.ContractVersion != RenditionExecutionSnapshotContractV1 {
		return errors.New("rendition execution snapshot contract is invalid")
	}
	if err := validateRenditionExecutionIdentity(snapshot.Identity); err != nil {
		return err
	}
	authorization := canonicalExecutionAuthorization(snapshot.Authorization)
	if !reflect.DeepEqual(authorization, snapshot.Authorization) {
		return errors.New("rendition execution snapshot authorization is not canonical")
	}
	if !reflect.DeepEqual(renditionAuthorizationIdentity(authorization), snapshot.Identity.Authorization) {
		return errors.New("rendition execution snapshot authorization identity drifted")
	}
	authorizedAt, err := parseRenditionTimestamp(authorization.AuthorizedAt)
	if err != nil {
		return errors.New("rendition execution authorization time is invalid")
	}
	expiresAt, err := parseRenditionTimestamp(authorization.ExpiresAt)
	if err != nil || !expiresAt.After(authorizedAt) {
		return errors.New("rendition execution authorization expiry is invalid")
	}
	return nil
}

func evidencePolicyFromIdentity(identity EvidencePolicyIdentity) (EvidencePolicy, error) {
	policy, err := NewEvidencePolicy(identity.MaxDocumentChars)
	if err != nil {
		return EvidencePolicy{}, err
	}
	if policy.Identity() != identity {
		return EvidencePolicy{}, errors.New("document evidence policy identity is unsupported")
	}
	return policy, nil
}

func renditionPolicyFromIdentity(identity RenditionPolicyIdentity) (RenditionPolicy, error) {
	policy, err := NewRenditionPolicy(RenditionLimits{
		MaxDocumentChars: identity.MaxDocumentChars, MaxUnitRunes: identity.MaxUnitRunes,
		MaxSegmentRunes: identity.MaxSegmentRunes,
	})
	if err != nil {
		return RenditionPolicy{}, err
	}
	if policy.Identity() != identity {
		return RenditionPolicy{}, errors.New("rendition policy identity is unsupported")
	}
	return policy, nil
}

func renditionAuthorizationIdentity(
	authorization RenditionAuthorization,
) RenditionAuthorizationIdentityV1 {
	return RenditionAuthorizationIdentityV1{
		ProviderID: authorization.ProviderID, DescriptorFingerprint: authorization.DescriptorFingerprint,
		PolicyFingerprint:           authorization.PolicyFingerprint,
		RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
		SourceSHA256:                authorization.SourceSHA256, SourceBytes: authorization.SourceBytes,
		CapabilityRecordChecksum: authorization.CapabilityRecordChecksum,
		ProviderMetadataChecksum: authorization.ProviderMetadataChecksum,
		MediaFamily:              authorization.MediaFamily, MediaType: authorization.MediaType,
		InputKind:                authorization.InputKind,
		DiscloseFilename:         authorization.DiscloseFilename,
		AllowedArtifactRoles:     slices.Clone(authorization.AllowedArtifactRoles),
		MaxProviderMarkdownBytes: authorization.MaxProviderMarkdownBytes,
		MaxArtifactBytes:         authorization.MaxArtifactBytes, MaxArtifacts: authorization.MaxArtifacts,
		MaxTotalResultBytes: authorization.MaxTotalResultBytes,
	}
}

func canonicalExecutionAuthorization(authorization RenditionAuthorization) RenditionAuthorization {
	authorization = cloneRenditionAuthorization(authorization)
	slices.Sort(authorization.AllowedArtifactRoles)
	return authorization
}

func canonicalExecutionIdentity(identity RenditionExecutionIdentityV1) RenditionExecutionIdentityV1 {
	identity = cloneRenditionExecutionIdentity(identity)
	if !identity.Authorization.DiscloseFilename {
		identity.Upload.Filename = ""
	}
	return identity
}

func cloneRenditionExecutionIdentity(identity RenditionExecutionIdentityV1) RenditionExecutionIdentityV1 {
	identity.Authorization.AllowedArtifactRoles = slices.Clone(identity.Authorization.AllowedArtifactRoles)
	return identity
}

func cloneRenditionExecutionSnapshot(snapshot RenditionExecutionSnapshotV1) RenditionExecutionSnapshotV1 {
	snapshot.Identity = cloneRenditionExecutionIdentity(snapshot.Identity)
	snapshot.Authorization = cloneRenditionAuthorization(snapshot.Authorization)
	return snapshot
}
