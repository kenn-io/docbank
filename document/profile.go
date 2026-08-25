package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"golang.org/x/text/unicode/norm"
)

const (
	// ProcessingProfileContractV1 identifies immutable processing policy.
	ProcessingProfileContractV1 = "processing-profile/v1"
	// MaxRetrievalCandidateLimit bounds each processing-profile retrieval lane.
	MaxRetrievalCandidateLimit = 1_000

	maxProfileStringBytes     = 1 << 10
	maxProcessingEmbeddings   = 64
	maxRequestedArtifacts     = 4
	maxRenditionDocumentBytes = int64(1 << 40)
	maxRenditionResponseBytes = int64(1 << 30)
	maxRenditionUnits         = 1_000_000
	maxEvidenceUnitRunes      = 256 << 20
	maxEvidenceSegmentRunes   = 1 << 20
	maxEmbeddingDimensions    = 1_000_000
	maxEmbeddingBatchItems    = 10_000
	maxEmbeddingInputBytes    = int64(1 << 30)
	maxEmbeddingResponseBytes = int64(1 << 30)
	maxEmbeddingChunkTokens   = 1_000_000
)

// EmbeddingInputKind identifies the evidence presented to an embedding
// provider. Original files and rendition chunks are distinct semantic inputs.
type EmbeddingInputKind string

const (
	EmbeddingInputOriginalFile   EmbeddingInputKind = "original_file"
	EmbeddingInputRenditionChunk EmbeddingInputKind = "rendition_chunk"
)

// EmbeddingActivation identifies whether failure of one binding fails the
// processing profile or leaves that vector space unavailable.
type EmbeddingActivation string

const (
	EmbeddingOptional EmbeddingActivation = "optional"
	EmbeddingRequired EmbeddingActivation = "required"
)

// ProviderDescriptorV1 pins a stable public provider descriptor snapshot.
type ProviderDescriptorV1 struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

// RenditionBindingV1 binds rendition request, disclosure, and bounded
// transport policy. CredentialBinding is a credential:<name> reference only.
type RenditionBindingV1 struct {
	AdapterContract          string                 `json:"adapter_contract"`
	AuthorizationFingerprint string                 `json:"authorization_fingerprint"`
	CredentialBinding        string                 `json:"credential_binding"`
	DeploymentFingerprint    string                 `json:"deployment_fingerprint"`
	Descriptor               ProviderDescriptorV1   `json:"descriptor"`
	DiscloseFilename         bool                   `json:"disclose_filename"`
	DisclosureFingerprint    string                 `json:"disclosure_fingerprint"`
	MaxDocumentBytes         int64                  `json:"max_document_bytes"`
	MaxResponseBytes         int64                  `json:"max_response_bytes"`
	MaxUnits                 int                    `json:"max_units"`
	Name                     string                 `json:"name"`
	RequestedArtifacts       []EvidenceArtifactRole `json:"requested_artifacts"`
	TrustBoundary            string                 `json:"trust_boundary"`
	UploadOptionsFingerprint string                 `json:"upload_options_fingerprint"`
}

// EvidenceLexicalPolicyV1 pins source evidence, normalization, completeness,
// sanitization, and lexical segmentation independently from retention.
type EvidenceLexicalPolicyV1 struct {
	CompletenessFingerprint     string `json:"completeness_fingerprint"`
	LexicalSegmenterFingerprint string `json:"lexical_segmenter_fingerprint"`
	MaxDocumentChars            int    `json:"max_document_chars"`
	MaxSegmentRunes             int    `json:"max_segment_runes"`
	MaxUnitRunes                int    `json:"max_unit_runes"`
	NormalizedEvidenceContract  string `json:"normalized_evidence_contract"`
	NormalizerFingerprint       string `json:"normalizer_fingerprint"`
	RenditionContract           string `json:"rendition_contract"`
	SanitizerFingerprint        string `json:"sanitizer_fingerprint"`
	SourceEvidenceContract      string `json:"source_evidence_contract"`
}

// EmbeddingChunkPolicyV1 pins exact model input generation from a rendition.
type EmbeddingChunkPolicyV1 struct {
	ContextFingerprint string `json:"context_fingerprint"`
	Formatter          string `json:"formatter"`
	MaxTokens          int    `json:"max_tokens"`
	OverlapTokens      int    `json:"overlap_tokens"`
	Tokenizer          string `json:"tokenizer"`
	TruncationPolicy   string `json:"truncation_policy"`
}

// EmbeddingBindingV1 binds one named input plan, vector compatibility space,
// operational limits, and disclosure boundary.
type EmbeddingBindingV1 struct {
	Activation               EmbeddingActivation     `json:"activation"`
	AuthorizationFingerprint string                  `json:"authorization_fingerprint"`
	Chunk                    *EmbeddingChunkPolicyV1 `json:"chunk"`
	CompatibilityID          string                  `json:"compatibility_id"`
	CredentialBinding        string                  `json:"credential_binding"`
	Descriptor               ProviderDescriptorV1    `json:"descriptor"`
	Dimensions               int                     `json:"dimensions"`
	DisclosureFingerprint    string                  `json:"disclosure_fingerprint"`
	DocumentFormatter        string                  `json:"document_formatter"`
	InputKind                EmbeddingInputKind      `json:"input_kind"`
	MaxBatchItems            int                     `json:"max_batch_items"`
	MaxInputBytes            int64                   `json:"max_input_bytes"`
	MaxResponseBytes         int64                   `json:"max_response_bytes"`
	Metric                   string                  `json:"metric"`
	ModelInput               ModelInputContract      `json:"model_input"`
	Model                    string                  `json:"model"`
	Name                     string                  `json:"name"`
	Normalization            string                  `json:"normalization"`
	QueryFormatter           string                  `json:"query_formatter"`
	ScalarEncoding           string                  `json:"scalar_encoding"`
	TrustBoundary            string                  `json:"trust_boundary"`
}

// RetentionDisclosurePolicyV1 pins durable derivative classes and attachment
// consent independently from provider execution policy.
type RetentionDisclosurePolicyV1 struct {
	AttachmentPolicyFingerprint string `json:"attachment_policy_fingerprint"`
	ConsentFingerprint          string `json:"consent_fingerprint"`
	RetainProviderMarkdown      bool   `json:"retain_provider_markdown"`
	RetainSanitizedMarkdown     bool   `json:"retain_sanitized_markdown"`
	RetainTypedArtifacts        bool   `json:"retain_typed_artifacts"`
	TrustBoundary               string `json:"trust_boundary"`
}

// RetrievalPolicyV1 pins the bounded lexical and vector candidate sets used
// to assemble document search results.
type RetrievalPolicyV1 struct {
	LexicalLimit int `json:"lexical_limit"`
	VectorLimit  int `json:"vector_limit"`
}

// ProcessingProfileV1 is immutable non-secret policy for derivatives.
// Originals and verified source versions remain authoritative.
type ProcessingProfileV1 struct {
	ContractVersion     string                      `json:"contract_version"`
	Embeddings          []EmbeddingBindingV1        `json:"embeddings"`
	EvidenceLexical     EvidenceLexicalPolicyV1     `json:"evidence_lexical"`
	Rendition           *RenditionBindingV1         `json:"rendition"`
	RetentionDisclosure RetentionDisclosurePolicyV1 `json:"retention_disclosure"`
	Retrieval           RetrievalPolicyV1           `json:"retrieval"`
}

// FingerprintSet contains the assembled identity and independent derivative
// layer identities. Maps are keyed by embedding binding name.
type FingerprintSet struct {
	Profile             string
	RenditionRequest    string
	EvidenceLexical     string
	EmbeddingInput      map[string]string
	VectorSpace         map[string]string
	RetentionDisclosure string
}

type fingerprintEnvelope[T any] struct {
	Kind    string `json:"kind"`
	Value   T      `json:"value"`
	Version int    `json:"version"`
}

type renditionRequestIdentity struct {
	AdapterContract          string                 `json:"adapter_contract"`
	DeploymentFingerprint    string                 `json:"deployment_fingerprint"`
	Descriptor               ProviderDescriptorV1   `json:"descriptor"`
	DiscloseFilename         bool                   `json:"disclose_filename"`
	DisclosureFingerprint    string                 `json:"disclosure_fingerprint"`
	MaxDocumentBytes         int64                  `json:"max_document_bytes"`
	MaxResponseBytes         int64                  `json:"max_response_bytes"`
	MaxUnits                 int                    `json:"max_units"`
	RequestedArtifacts       []EvidenceArtifactRole `json:"requested_artifacts"`
	UploadOptionsFingerprint string                 `json:"upload_options_fingerprint"`
}

type embeddingInputIdentity struct {
	Chunk                 *EmbeddingChunkPolicyV1 `json:"chunk"`
	EvidenceLexical       string                  `json:"evidence_lexical"`
	InputKind             EmbeddingInputKind      `json:"input_kind"`
	ModelInputFingerprint string                  `json:"model_input_fingerprint"`
}

type vectorSpaceIdentity struct {
	CompatibilityID       string               `json:"compatibility_id"`
	Descriptor            ProviderDescriptorV1 `json:"descriptor"`
	Dimensions            int                  `json:"dimensions"`
	DocumentFormatter     string               `json:"document_formatter"`
	Metric                string               `json:"metric"`
	ModelInputFingerprint string               `json:"model_input_fingerprint"`
	Model                 string               `json:"model"`
	Normalization         string               `json:"normalization"`
	QueryFormatter        string               `json:"query_formatter"`
	ScalarEncoding        string               `json:"scalar_encoding"`
}

type providerDisclosureIdentity struct {
	DeploymentFingerprint string               `json:"deployment_fingerprint"`
	Descriptor            ProviderDescriptorV1 `json:"descriptor"`
	DiscloseFilename      bool                 `json:"disclose_filename"`
	DisclosureFingerprint string               `json:"disclosure_fingerprint"`
	InputKind             EmbeddingInputKind   `json:"input_kind,omitempty"`
	Kind                  string               `json:"kind"`
	TrustBoundary         string               `json:"trust_boundary"`
}

type retentionDisclosureIdentity struct {
	AttachmentPolicyFingerprint string                       `json:"attachment_policy_fingerprint"`
	ConsentFingerprint          string                       `json:"consent_fingerprint"`
	Providers                   []providerDisclosureIdentity `json:"providers"`
	RetainProviderMarkdown      bool                         `json:"retain_provider_markdown"`
	RetainSanitizedMarkdown     bool                         `json:"retain_sanitized_markdown"`
	RetainTypedArtifacts        bool                         `json:"retain_typed_artifacts"`
	TrustBoundary               string                       `json:"trust_boundary"`
}

// CanonicalProfile validates and canonicalizes profile without mutating it,
// emits lexicographically canonical JSON, and derives layered SHA-256 values.
func CanonicalProfile(profile ProcessingProfileV1) ([]byte, FingerprintSet, error) {
	canonicalProfile, err := CanonicalizeProfile(profile)
	if err != nil {
		return nil, FingerprintSet{}, err
	}

	evidenceFingerprint, err := componentFingerprint("evidence_lexical", canonicalProfile.EvidenceLexical)
	if err != nil {
		return nil, FingerprintSet{}, err
	}
	result := FingerprintSet{
		EmbeddingInput: make(map[string]string, len(canonicalProfile.Embeddings)), VectorSpace: make(map[string]string, len(canonicalProfile.Embeddings)),
		EvidenceLexical: evidenceFingerprint,
	}
	var renditionIdentity *renditionRequestIdentity
	if canonicalProfile.Rendition != nil {
		renditionIdentity = &renditionRequestIdentity{
			AdapterContract:       canonicalProfile.Rendition.AdapterContract,
			DeploymentFingerprint: canonicalProfile.Rendition.DeploymentFingerprint, Descriptor: canonicalProfile.Rendition.Descriptor,
			DiscloseFilename: canonicalProfile.Rendition.DiscloseFilename, DisclosureFingerprint: canonicalProfile.Rendition.DisclosureFingerprint,
			MaxDocumentBytes: canonicalProfile.Rendition.MaxDocumentBytes,
			MaxResponseBytes: canonicalProfile.Rendition.MaxResponseBytes, MaxUnits: canonicalProfile.Rendition.MaxUnits,
			RequestedArtifacts: canonicalProfile.Rendition.RequestedArtifacts, UploadOptionsFingerprint: canonicalProfile.Rendition.UploadOptionsFingerprint,
		}
	}
	result.RenditionRequest, err = componentFingerprint("rendition_request", renditionIdentity)
	if err != nil {
		return nil, FingerprintSet{}, err
	}
	for _, binding := range canonicalProfile.Embeddings {
		input := embeddingInputIdentity{InputKind: binding.InputKind, Chunk: binding.Chunk, ModelInputFingerprint: binding.ModelInput.Fingerprint}
		if binding.InputKind == EmbeddingInputRenditionChunk {
			input.EvidenceLexical = evidenceFingerprint
		}
		result.EmbeddingInput[binding.Name], err = componentFingerprint("embedding_input", input)
		if err != nil {
			return nil, FingerprintSet{}, err
		}
		result.VectorSpace[binding.Name], err = componentFingerprint("vector_space", vectorSpaceIdentity{
			CompatibilityID: binding.CompatibilityID, Descriptor: binding.Descriptor, Dimensions: binding.Dimensions,
			DocumentFormatter: binding.DocumentFormatter, Metric: binding.Metric, Model: binding.Model,
			ModelInputFingerprint: binding.ModelInput.Fingerprint, Normalization: binding.Normalization,
			QueryFormatter: binding.QueryFormatter, ScalarEncoding: binding.ScalarEncoding,
		})
		if err != nil {
			return nil, FingerprintSet{}, err
		}
	}
	result.RetentionDisclosure, err = retentionFingerprint(canonicalProfile)
	if err != nil {
		return nil, FingerprintSet{}, err
	}
	encoded, err := canonical.Marshal(canonicalProfile)
	if err != nil {
		return nil, FingerprintSet{}, fmt.Errorf("encode canonical processing profile: %w", err)
	}
	result.Profile = sha256Hex(encoded)
	return encoded, result, nil
}

// CanonicalizeProfile validates profile and returns a detached canonical copy
// suitable for provider execution and durable policy identity.
func CanonicalizeProfile(profile ProcessingProfileV1) (ProcessingProfileV1, error) {
	canonicalProfile, err := canonicalProcessingProfile(profile)
	if err != nil {
		return ProcessingProfileV1{}, fmt.Errorf("invalid processing profile: %w", err)
	}
	if err := validateProcessingProfile(canonicalProfile); err != nil {
		return ProcessingProfileV1{}, fmt.Errorf("invalid processing profile: %w", err)
	}
	return canonicalProfile, nil
}

func canonicalProcessingProfile(profile ProcessingProfileV1) (ProcessingProfileV1, error) {
	canonicalProfile := profile
	canonicalProfile.Embeddings = slices.Clone(profile.Embeddings)
	if canonicalProfile.Embeddings == nil {
		canonicalProfile.Embeddings = make([]EmbeddingBindingV1, 0)
	}
	if profile.Rendition != nil {
		rendition := *profile.Rendition
		rendition.RequestedArtifacts = slices.Clone(profile.Rendition.RequestedArtifacts)
		canonicalProfile.Rendition = &rendition
	}
	for index := range canonicalProfile.Embeddings {
		if profile.Embeddings[index].Chunk != nil {
			chunk := *profile.Embeddings[index].Chunk
			canonicalProfile.Embeddings[index].Chunk = &chunk
		}
		if canonicalProfile.Embeddings[index].ModelInput == (ModelInputContract{}) {
			empty, err := NewModelInputContract(ModelInputContractConfig{})
			if err != nil {
				return ProcessingProfileV1{}, err
			}
			canonicalProfile.Embeddings[index].ModelInput = empty
		}
	}
	if err := normalizeProfileStrings(&canonicalProfile); err != nil {
		return ProcessingProfileV1{}, err
	}
	if canonicalProfile.Rendition != nil {
		slices.Sort(canonicalProfile.Rendition.RequestedArtifacts)
	}
	slices.SortFunc(canonicalProfile.Embeddings, func(left, right EmbeddingBindingV1) int { return strings.Compare(left.Name, right.Name) })
	return canonicalProfile, nil
}

func normalizeProfileStrings(profile *ProcessingProfileV1) error {
	if profile.Rendition != nil {
		values := []struct {
			value   *string
			subject string
		}{
			{&profile.Rendition.AdapterContract, "rendition adapter contract"}, {&profile.Rendition.Descriptor.ID, "rendition descriptor ID"},
			{&profile.Rendition.TrustBoundary, "rendition trust boundary"},
		}
		for _, item := range values {
			if err := normalizeProfileString(item.value, item.subject); err != nil {
				return err
			}
		}
	}
	evidenceStrings := []struct {
		value   *string
		subject string
	}{
		{&profile.EvidenceLexical.NormalizedEvidenceContract, "normalized evidence contract"}, {&profile.EvidenceLexical.RenditionContract, "rendition contract"},
		{&profile.EvidenceLexical.SourceEvidenceContract, "source evidence contract"}, {&profile.RetentionDisclosure.TrustBoundary, "retention trust boundary"},
	}
	for _, item := range evidenceStrings {
		if err := normalizeProfileString(item.value, item.subject); err != nil {
			return err
		}
	}
	for index := range profile.Embeddings {
		binding := &profile.Embeddings[index]
		values := []struct {
			value   *string
			subject string
		}{
			{&binding.CompatibilityID, "embedding compatibility ID"}, {&binding.Descriptor.ID, "embedding descriptor ID"},
			{&binding.DocumentFormatter, "embedding document formatter"}, {&binding.Metric, "embedding metric"},
			{&binding.Model, "embedding model"}, {&binding.Normalization, "embedding normalization"},
			{&binding.QueryFormatter, "embedding query formatter"}, {&binding.ScalarEncoding, "embedding scalar encoding"},
			{&binding.TrustBoundary, "embedding trust boundary"},
		}
		for _, item := range values {
			if err := normalizeProfileString(item.value, item.subject); err != nil {
				return err
			}
		}
		if binding.Chunk != nil {
			chunkValues := []struct {
				value   *string
				subject string
			}{
				{&binding.Chunk.Formatter, "chunk formatter"}, {&binding.Chunk.Tokenizer, "chunk tokenizer"},
				{&binding.Chunk.TruncationPolicy, "chunk truncation policy"},
			}
			for _, item := range chunkValues {
				if err := normalizeProfileString(item.value, item.subject); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func normalizeProfileString(value *string, subject string) error {
	if !utf8.ValidString(*value) {
		return fmt.Errorf("%s must be valid UTF-8", subject)
	}
	*value = strings.ReplaceAll(*value, "\r\n", "\n")
	*value = strings.ReplaceAll(*value, "\r", "\n")
	*value = norm.NFC.String(*value)
	if *value == "" || len(*value) > maxProfileStringBytes {
		return fmt.Errorf("%s must contain 1-%d canonical UTF-8 bytes", subject, maxProfileStringBytes)
	}
	return nil
}

func validateProcessingProfile(profile ProcessingProfileV1) error {
	if profile.ContractVersion != ProcessingProfileContractV1 {
		return fmt.Errorf("contract version must be %q", ProcessingProfileContractV1)
	}
	if len(profile.Embeddings) > maxProcessingEmbeddings {
		return fmt.Errorf("too many embedding bindings: maximum is %d", maxProcessingEmbeddings)
	}
	if profile.Rendition != nil {
		if err := validateRenditionBinding(*profile.Rendition); err != nil {
			return err
		}
	}
	if err := validateEvidenceLexicalPolicy(profile.EvidenceLexical); err != nil {
		return err
	}
	if err := validateRetentionDisclosure(profile.RetentionDisclosure); err != nil {
		return err
	}
	if err := validateRetrievalPolicy(profile.Retrieval); err != nil {
		return err
	}
	if profile.Rendition == nil && (profile.RetentionDisclosure.RetainSanitizedMarkdown || profile.RetentionDisclosure.RetainProviderMarkdown) {
		return errors.New("retained Markdown requires a rendition binding")
	}
	names := make(map[string]struct{}, len(profile.Embeddings))
	for index, binding := range profile.Embeddings {
		if _, exists := names[binding.Name]; exists {
			return fmt.Errorf("embedding binding name %q is duplicated", binding.Name)
		}
		names[binding.Name] = struct{}{}
		if err := validateEmbeddingBinding(binding, profile.Rendition != nil); err != nil {
			return fmt.Errorf("embedding binding %d: %w", index, err)
		}
	}
	return nil
}

func validateRenditionBinding(binding RenditionBindingV1) error {
	if err := validateBindingName(binding.Name, "rendition binding name"); err != nil {
		return err
	}
	if err := validateCredentialReference(binding.CredentialBinding, "rendition credential binding"); err != nil {
		return err
	}
	if err := validateProviderDescriptor(binding.Descriptor); err != nil {
		return fmt.Errorf("rendition descriptor: %w", err)
	}
	for subject, value := range map[string]string{
		"rendition authorization fingerprint": binding.AuthorizationFingerprint, "rendition deployment fingerprint": binding.DeploymentFingerprint,
		"rendition disclosure fingerprint": binding.DisclosureFingerprint, "rendition upload options fingerprint": binding.UploadOptionsFingerprint,
	} {
		if err := validateFingerprint(value, subject); err != nil {
			return err
		}
	}
	if binding.MaxDocumentBytes <= 0 || binding.MaxDocumentBytes > maxRenditionDocumentBytes {
		return fmt.Errorf("rendition max document bytes must be between 1 and %d", maxRenditionDocumentBytes)
	}
	if binding.MaxResponseBytes <= 0 || binding.MaxResponseBytes > maxRenditionResponseBytes {
		return fmt.Errorf("rendition max response bytes must be between 1 and %d", maxRenditionResponseBytes)
	}
	if binding.MaxUnits <= 0 || binding.MaxUnits > maxRenditionUnits {
		return fmt.Errorf("rendition max units must be between 1 and %d", maxRenditionUnits)
	}
	if len(binding.RequestedArtifacts) == 0 {
		return errors.New("rendition requested artifacts must not be empty")
	}
	if len(binding.RequestedArtifacts) > maxRequestedArtifacts {
		return fmt.Errorf("too many requested artifacts: maximum is %d", maxRequestedArtifacts)
	}
	seen := make(map[EvidenceArtifactRole]struct{}, len(binding.RequestedArtifacts))
	for _, role := range binding.RequestedArtifacts {
		if !validProfileArtifactRole(role) {
			return fmt.Errorf("rendition requested artifact role %q is unknown", role)
		}
		if _, exists := seen[role]; exists {
			return fmt.Errorf("rendition requested artifact role %q is duplicated", role)
		}
		seen[role] = struct{}{}
	}
	return nil
}

func validateEvidenceLexicalPolicy(policy EvidenceLexicalPolicyV1) error {
	if policy.SourceEvidenceContract != SourceEvidenceContractV1 {
		return fmt.Errorf("source evidence contract must be %q", SourceEvidenceContractV1)
	}
	if policy.NormalizedEvidenceContract != NormalizedEvidenceContractV1 {
		return fmt.Errorf("normalized evidence contract must be %q", NormalizedEvidenceContractV1)
	}
	if policy.RenditionContract != RenditionContractV1 {
		return fmt.Errorf("rendition contract must be %q", RenditionContractV1)
	}
	for subject, value := range map[string]string{
		"completeness fingerprint": policy.CompletenessFingerprint, "lexical segmenter fingerprint": policy.LexicalSegmenterFingerprint,
		"normalizer fingerprint": policy.NormalizerFingerprint, "sanitizer fingerprint": policy.SanitizerFingerprint,
	} {
		if err := validateFingerprint(value, subject); err != nil {
			return err
		}
	}
	if policy.MaxUnitRunes <= 0 || policy.MaxUnitRunes > maxEvidenceUnitRunes {
		return fmt.Errorf("evidence max unit runes must be between 1 and %d", maxEvidenceUnitRunes)
	}
	if policy.MaxDocumentChars <= 0 {
		return errors.New("evidence max document chars must be positive")
	}
	if policy.MaxSegmentRunes <= 0 || policy.MaxSegmentRunes > maxEvidenceSegmentRunes {
		return fmt.Errorf("evidence max segment runes must be between 1 and %d", maxEvidenceSegmentRunes)
	}
	if policy.MaxSegmentRunes > policy.MaxUnitRunes {
		return errors.New("evidence max segment runes must not exceed max unit runes")
	}
	return nil
}

func validateEmbeddingBinding(binding EmbeddingBindingV1, hasRendition bool) error {
	if err := validateBindingName(binding.Name, "name"); err != nil {
		return err
	}
	if err := validateCredentialReference(binding.CredentialBinding, "embedding credential binding"); err != nil {
		return err
	}
	if err := validateProviderDescriptor(binding.Descriptor); err != nil {
		return fmt.Errorf("descriptor: %w", err)
	}
	if err := validateModelInputContract(binding.ModelInput); err != nil {
		return fmt.Errorf("model input: %w", err)
	}
	if binding.ModelInput.Profile != "" && binding.ModelInput.CompatibilityID != binding.CompatibilityID {
		return errors.New("model input compatibility ID does not match embedding compatibility ID")
	}
	if !IsValidVectorMetric(binding.Metric) {
		return errors.New("embedding metric is invalid")
	}
	if !validVectorNormalization(binding.Normalization) {
		return errors.New("embedding normalization is invalid")
	}
	for subject, value := range map[string]string{"authorization fingerprint": binding.AuthorizationFingerprint, "disclosure fingerprint": binding.DisclosureFingerprint} {
		if err := validateFingerprint(value, subject); err != nil {
			return err
		}
	}
	switch binding.InputKind {
	case EmbeddingInputOriginalFile:
		if binding.Chunk != nil {
			return errors.New("original_file input must not define chunk policy")
		}
	case EmbeddingInputRenditionChunk:
		if !hasRendition {
			return errors.New("rendition_chunk input requires a rendition binding")
		}
		if binding.Chunk == nil {
			return errors.New("rendition_chunk input requires chunk policy")
		}
		if err := validateChunkPolicy(*binding.Chunk); err != nil {
			return err
		}
	default:
		return fmt.Errorf("input kind %q must be original_file or rendition_chunk", binding.InputKind)
	}
	if binding.Activation != EmbeddingOptional && binding.Activation != EmbeddingRequired {
		return fmt.Errorf("activation %q must be optional or required", binding.Activation)
	}
	if binding.Dimensions <= 0 || binding.Dimensions > maxEmbeddingDimensions {
		return fmt.Errorf("dimensions must be between 1 and %d", maxEmbeddingDimensions)
	}
	if binding.MaxBatchItems <= 0 || binding.MaxBatchItems > maxEmbeddingBatchItems {
		return fmt.Errorf("max batch items must be between 1 and %d", maxEmbeddingBatchItems)
	}
	if binding.MaxInputBytes <= 0 || binding.MaxInputBytes > maxEmbeddingInputBytes {
		return fmt.Errorf("max input bytes must be between 1 and %d", maxEmbeddingInputBytes)
	}
	if binding.MaxResponseBytes <= 0 || binding.MaxResponseBytes > maxEmbeddingResponseBytes {
		return fmt.Errorf("max response bytes must be between 1 and %d", maxEmbeddingResponseBytes)
	}
	return nil
}

func validateChunkPolicy(policy EmbeddingChunkPolicyV1) error {
	if policy.MaxTokens <= 0 || policy.MaxTokens > maxEmbeddingChunkTokens {
		return fmt.Errorf("chunk max tokens must be between 1 and %d", maxEmbeddingChunkTokens)
	}
	if policy.OverlapTokens < 0 || policy.OverlapTokens >= policy.MaxTokens {
		return errors.New("chunk overlap tokens must be non-negative and less than max tokens")
	}
	return validateFingerprint(policy.ContextFingerprint, "chunk context fingerprint")
}

func validateRetentionDisclosure(policy RetentionDisclosurePolicyV1) error {
	if err := validateFingerprint(policy.AttachmentPolicyFingerprint, "attachment policy fingerprint"); err != nil {
		return err
	}
	return validateFingerprint(policy.ConsentFingerprint, "retention consent fingerprint")
}

func validateRetrievalPolicy(policy RetrievalPolicyV1) error {
	if policy.LexicalLimit <= 0 || policy.LexicalLimit > MaxRetrievalCandidateLimit {
		return fmt.Errorf("retrieval lexical limit must be between 1 and %d", MaxRetrievalCandidateLimit)
	}
	if policy.VectorLimit <= 0 || policy.VectorLimit > MaxRetrievalCandidateLimit {
		return fmt.Errorf("retrieval vector limit must be between 1 and %d", MaxRetrievalCandidateLimit)
	}
	return nil
}

func validateProviderDescriptor(descriptor ProviderDescriptorV1) error {
	return validateFingerprint(descriptor.Fingerprint, "descriptor fingerprint")
}

func validateFingerprint(value, subject string) error {
	if !canonical.IsSHA256Hex(value) {
		return fmt.Errorf("%s must be a lowercase SHA-256 value", subject)
	}
	return nil
}

func validateCredentialReference(value, subject string) error {
	const prefix = "credential:"
	if !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("%s must use credential:<name>", subject)
	}
	return validateBindingName(strings.TrimPrefix(value, prefix), subject)
}

func validateBindingName(name, subject string) error {
	if name == "" || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("%s must start with a lowercase letter and contain 1-63 characters", subject)
	}
	for _, char := range name[1:] {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			continue
		}
		return fmt.Errorf("%s %q contains unsupported characters", subject, name)
	}
	return nil
}

func retentionFingerprint(profile ProcessingProfileV1) (string, error) {
	providers := make([]providerDisclosureIdentity, 0, len(profile.Embeddings)+1)
	if profile.Rendition != nil {
		providers = append(providers, providerDisclosureIdentity{DeploymentFingerprint: profile.Rendition.DeploymentFingerprint,
			Descriptor: profile.Rendition.Descriptor, DiscloseFilename: profile.Rendition.DiscloseFilename,
			DisclosureFingerprint: profile.Rendition.DisclosureFingerprint, Kind: "rendition", TrustBoundary: profile.Rendition.TrustBoundary})
	}
	for _, binding := range profile.Embeddings {
		providers = append(providers, providerDisclosureIdentity{Descriptor: binding.Descriptor,
			DisclosureFingerprint: binding.DisclosureFingerprint, InputKind: binding.InputKind,
			Kind: "embedding", TrustBoundary: binding.TrustBoundary})
	}
	slices.SortFunc(providers, func(left, right providerDisclosureIdentity) int {
		leftJSON, _ := canonical.Marshal(left)
		rightJSON, _ := canonical.Marshal(right)
		return bytes.Compare(leftJSON, rightJSON)
	})
	policy := profile.RetentionDisclosure
	return componentFingerprint("retention_disclosure", retentionDisclosureIdentity{
		AttachmentPolicyFingerprint: policy.AttachmentPolicyFingerprint, ConsentFingerprint: policy.ConsentFingerprint,
		Providers: providers, RetainProviderMarkdown: policy.RetainProviderMarkdown,
		RetainSanitizedMarkdown: policy.RetainSanitizedMarkdown, RetainTypedArtifacts: policy.RetainTypedArtifacts,
		TrustBoundary: policy.TrustBoundary,
	})
}

func validProfileArtifactRole(role EvidenceArtifactRole) bool {
	switch role {
	case EvidenceArtifactImage, EvidenceArtifactMarkdown, EvidenceArtifactStructured, EvidenceArtifactTranscript:
		return true
	default:
		return false
	}
}

func componentFingerprint[T any](kind string, value T) (string, error) {
	encoded, err := canonical.Marshal(fingerprintEnvelope[T]{Kind: kind, Value: value, Version: 1})
	if err != nil {
		return "", fmt.Errorf("encode %s fingerprint: %w", kind, err)
	}
	return sha256Hex(encoded), nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
