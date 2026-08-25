package document

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"golang.org/x/text/unicode/norm"
)

const (
	// EmbeddingProviderContractVersion identifies the embedding execution boundary.
	EmbeddingProviderContractVersion = 1
)

// EmbeddingInputQueryText is query-time text. It is not a persistent source
// representation and is therefore intentionally absent from profile bindings.
const EmbeddingInputQueryText EmbeddingInputKind = "query_text"

type EmbeddingRole string

const (
	EmbeddingRoleDocument EmbeddingRole = "document"
	EmbeddingRoleQuery    EmbeddingRole = "query"
)

type EmbeddingTrustBoundary string

const (
	EmbeddingTrustLocalProcess    EmbeddingTrustBoundary = "local_process"
	EmbeddingTrustOperatorNetwork EmbeddingTrustBoundary = "operator_network"
	EmbeddingTrustHostedProvider  EmbeddingTrustBoundary = "hosted_provider"
)

const (
	VectorMetricCosine     = "cosine"
	VectorMetricDotProduct = "dot_product"
	VectorMetricL2         = "l2"
)

// EmbeddingProvider embeds one validated, storage-neutral input batch.
type EmbeddingProvider interface {
	Descriptor() EmbeddingDescriptor
	Embed(ctx context.Context, inputs []EmbeddingInput, authorization EmbeddingAuthorization) (EmbeddingResult, error)
}

// EmbeddingDescriptor fixes one immutable provider vector-space contract.
type EmbeddingDescriptor struct {
	ID                    string                 `json:"id"`
	ContractVersion       int                    `json:"contract_version"`
	PolicyFingerprint     string                 `json:"policy_fingerprint"`
	TrustBoundary         EmbeddingTrustBoundary `json:"trust_boundary"`
	Model                 string                 `json:"model"`
	ModelRevision         string                 `json:"model_revision"`
	Dimension             int                    `json:"dimension"`
	Metric                string                 `json:"metric"`
	Normalization         string                 `json:"normalization"`
	ScalarEncoding        string                 `json:"scalar_encoding"`
	DocumentFormatter     string                 `json:"document_formatter"`
	QueryFormatter        string                 `json:"query_formatter"`
	InputKinds            []EmbeddingInputKind   `json:"input_kinds"`
	CompatibilityID       string                 `json:"compatibility_id"`
	SupportsTextQuery     bool                   `json:"supports_text_query"`
	ModelInput            ModelInputContract     `json:"model_input"`
	SupportedRequestModes []ModelInputMode       `json:"supported_request_modes"`
	Fingerprint           string                 `json:"fingerprint"`
}

type embeddingDescriptorFields struct {
	ID                    string                 `json:"id"`
	ContractVersion       int                    `json:"contract_version"`
	PolicyFingerprint     string                 `json:"policy_fingerprint"`
	TrustBoundary         EmbeddingTrustBoundary `json:"trust_boundary"`
	Model                 string                 `json:"model"`
	ModelRevision         string                 `json:"model_revision"`
	Dimension             int                    `json:"dimension"`
	Metric                string                 `json:"metric"`
	Normalization         string                 `json:"normalization"`
	ScalarEncoding        string                 `json:"scalar_encoding"`
	DocumentFormatter     string                 `json:"document_formatter"`
	QueryFormatter        string                 `json:"query_formatter"`
	InputKinds            []EmbeddingInputKind   `json:"input_kinds"`
	CompatibilityID       string                 `json:"compatibility_id"`
	SupportsTextQuery     bool                   `json:"supports_text_query"`
	ModelInput            ModelInputContract     `json:"model_input"`
	SupportedRequestModes []ModelInputMode       `json:"supported_request_modes"`
}

// NewEmbeddingDescriptor validates, canonicalizes, and fingerprints a provider descriptor.
func NewEmbeddingDescriptor(value EmbeddingDescriptor) (EmbeddingDescriptor, error) {
	value.Fingerprint = ""
	value.InputKinds = slices.Clone(value.InputKinds)
	value.SupportedRequestModes = slices.Clone(value.SupportedRequestModes)
	slices.Sort(value.InputKinds)
	slices.Sort(value.SupportedRequestModes)
	if err := validateEmbeddingDescriptorFields(value); err != nil {
		return EmbeddingDescriptor{}, err
	}
	encoded, err := canonical.Marshal(embeddingDescriptorIdentity(value))
	if err != nil {
		return EmbeddingDescriptor{}, fmt.Errorf("encode embedding descriptor: %w", err)
	}
	value.Fingerprint = sha256Hex(encoded)
	return value, nil
}

// EmbeddingInput is one requested provider vector.
type EmbeddingInput struct {
	Key         string             `json:"key"`
	Role        EmbeddingRole      `json:"role"`
	Kind        EmbeddingInputKind `json:"kind"`
	Text        string             `json:"text,omitempty"`
	Source      AuthorizedUpload   `json:"-"`
	HeadingPath []string           `json:"heading_path,omitempty"`
	SourceSpans []ChunkSpan        `json:"source_spans,omitempty"`
}

// EmbeddingAuthorization bounds one embedding invocation and pins its provider identity.
// MaxResponseBytes is the exact sum of every result key's UTF-8 bytes plus four
// bytes for every float32 scalar; adapters must independently bound raw transport bytes.
type EmbeddingAuthorization struct {
	ProviderID            string `json:"provider_id"`
	DescriptorFingerprint string `json:"descriptor_fingerprint"`
	PolicyFingerprint     string `json:"policy_fingerprint"`
	MaxBatchItems         int    `json:"max_batch_items"`
	MaxInputBytes         int64  `json:"max_input_bytes"`
	MaxResponseBytes      int64  `json:"max_response_bytes"`
}

// EmbeddingVector is one provider result. Index is required only when results
// are returned out of request order; zero values preserve ordered APIs.
type EmbeddingVector struct {
	Key    string    `json:"key"`
	Index  *int      `json:"index,omitempty"`
	Values []float32 `json:"values"`
}

type EmbeddingResult struct {
	Vectors []EmbeddingVector `json:"vectors"`
}

// ValidateEmbeddingProviderRequest checks the adapter snapshot and all input
// limits before any bytes reach a provider.
func ValidateEmbeddingProviderRequest(provider EmbeddingProvider, inputs []EmbeddingInput, authorization EmbeddingAuthorization) error {
	_, _, err := validateEmbeddingProviderRequest(provider, inputs, authorization)
	return err
}

func validateEmbeddingProviderRequest(
	provider EmbeddingProvider, inputs []EmbeddingInput, authorization EmbeddingAuthorization,
) (EmbeddingDescriptor, []AuthorizedUploadMetadata, error) {
	if nilInterface(provider) {
		return EmbeddingDescriptor{}, nil, errors.New("embedding provider is required")
	}
	descriptor := cloneEmbeddingDescriptor(provider.Descriptor())
	if err := validateEmbeddingDescriptor(descriptor); err != nil {
		return EmbeddingDescriptor{}, nil, err
	}
	if next := cloneEmbeddingDescriptor(provider.Descriptor()); !equalEmbeddingDescriptors(descriptor, next) {
		return EmbeddingDescriptor{}, nil, errors.New("embedding descriptor changed during validation")
	}
	if err := validateEmbeddingAuthorization(descriptor, authorization); err != nil {
		return EmbeddingDescriptor{}, nil, err
	}
	metadata := make([]AuthorizedUploadMetadata, len(inputs))
	if err := validateEmbeddingInputs(descriptor, inputs, authorization, metadata); err != nil {
		return EmbeddingDescriptor{}, nil, err
	}
	if minimumResultFootprint(descriptor.Dimension, inputs) > authorization.MaxResponseBytes {
		return EmbeddingDescriptor{}, nil, errors.New("embedding authorization response byte limit cannot hold requested vectors")
	}
	return descriptor, metadata, nil
}

// ValidateEmbeddingProviderResult checks exact key membership, order, and
// finite vector shape for a completed provider operation.
func ValidateEmbeddingProviderResult(descriptor EmbeddingDescriptor, inputs []EmbeddingInput, authorization EmbeddingAuthorization, result EmbeddingResult) error {
	if err := validateEmbeddingDescriptor(descriptor); err != nil {
		return err
	}
	if err := validateEmbeddingAuthorization(descriptor, authorization); err != nil {
		return err
	}
	if err := validateEmbeddingInputs(descriptor, inputs, authorization, nil); err != nil {
		return err
	}
	if minimumResultFootprint(descriptor.Dimension, inputs) > authorization.MaxResponseBytes {
		return errors.New("embedding authorization response byte limit cannot hold requested vectors")
	}
	if len(result.Vectors) != len(inputs) {
		return errors.New("provider result has a missing vector")
	}
	indexed := result.Vectors[0].Index != nil
	seen := make(map[string]struct{}, len(result.Vectors))
	seenIndices := make(map[int]struct{}, len(result.Vectors))
	expectedKeys := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		expectedKeys[input.Key] = struct{}{}
	}
	for position, vector := range result.Vectors {
		if (vector.Index != nil) != indexed {
			return errors.New("provider result has partial indexes")
		}
		if _, expected := expectedKeys[vector.Key]; !expected {
			return errors.New("provider result has an unexpected vector key")
		}
		if _, exists := seen[vector.Key]; exists {
			return errors.New("provider result has a duplicate vector key")
		}
		seen[vector.Key] = struct{}{}
		expected := position
		if indexed {
			expected = *vector.Index
			if expected < 0 || expected >= len(inputs) {
				return errors.New("provider result vector index is outside request bounds")
			}
			if _, exists := seenIndices[expected]; exists {
				return errors.New("provider result has a duplicate vector index")
			}
			seenIndices[expected] = struct{}{}
		}
		if vector.Key != inputs[expected].Key {
			if indexed {
				return errors.New("provider result vector key does not match index")
			}
			return errors.New("provider result vector order does not match request")
		}
		if len(vector.Values) != descriptor.Dimension {
			return errors.New("provider result vector dimension does not match descriptor")
		}
		for _, value := range vector.Values {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return errors.New("provider result vector contains non-finite value")
			}
		}
	}
	if resultFootprint(result) > authorization.MaxResponseBytes {
		return errors.New("provider result exceeds authorized response bytes")
	}
	return nil
}

// ExecuteEmbedding is the validated execution entry point for core callers.
// It takes ownership of original-file uploads and closes them before returning.
func ExecuteEmbedding(
	ctx context.Context, provider EmbeddingProvider, inputs []EmbeddingInput, authorization EmbeddingAuthorization,
) (result EmbeddingResult, err error) {
	providerInputs := cloneEmbeddingInputs(inputs)
	ownedUploads := make([]*ownedAuthorizedUpload, 0, len(inputs))
	stopCloses := make([]func() bool, 0, len(inputs))
	for index := range providerInputs {
		if nilInterface(providerInputs[index].Source) {
			continue
		}
		owned := &ownedAuthorizedUpload{upload: providerInputs[index].Source}
		ownedUploads = append(ownedUploads, owned)
		providerInputs[index].Source = owned
		stopCloses = append(stopCloses, context.AfterFunc(ctx, func() { _ = owned.Close() }))
	}
	defer func() {
		for _, stopClose := range stopCloses {
			stopClose()
		}
		for _, upload := range ownedUploads {
			if closeErr := upload.Close(); closeErr != nil && !errors.Is(err, closeErr) {
				err = errors.Join(err, fmt.Errorf("close authorized upload: %w", closeErr))
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return EmbeddingResult{}, err
	}
	descriptor, metadata, err := validateEmbeddingProviderRequest(provider, providerInputs, authorization)
	if err != nil {
		return EmbeddingResult{}, err
	}
	sealedUploads := make([]*sealedAuthorizedUpload, 0, len(ownedUploads))
	for index := range providerInputs {
		owned, ok := providerInputs[index].Source.(*ownedAuthorizedUpload)
		if !ok {
			continue
		}
		sealed := newSealedAuthorizedUpload(ctx, owned, metadata[index])
		sealedUploads = append(sealedUploads, sealed)
		providerInputs[index].Source = sealed
	}
	result, err = provider.Embed(ctx, providerInputs, authorization)
	for _, upload := range sealedUploads {
		_ = upload.Close()
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return EmbeddingResult{}, contextErr
	}
	if err != nil {
		return EmbeddingResult{}, err
	}
	if err := ValidateEmbeddingProviderResult(descriptor, providerInputs, authorization, result); err != nil {
		return EmbeddingResult{}, err
	}
	result = cloneEmbeddingResult(result)
	for _, upload := range sealedUploads {
		if err := upload.verify(ctx); err != nil {
			return EmbeddingResult{}, err
		}
	}
	return result, nil
}

// ValidateEmbeddingQueryCompatibility proves two document/query descriptors
// occupy one safe text-retrieval vector space.
func ValidateEmbeddingQueryCompatibility(documentDescriptor, queryDescriptor EmbeddingDescriptor) error {
	if err := validateEmbeddingDescriptor(documentDescriptor); err != nil {
		return err
	}
	if err := validateEmbeddingDescriptor(queryDescriptor); err != nil {
		return err
	}
	if !queryDescriptor.SupportsTextQuery {
		return errors.New("query descriptor does not support text queries")
	}
	for _, field := range []struct{ name, left, right string }{
		{"compatibility ID", documentDescriptor.CompatibilityID, queryDescriptor.CompatibilityID},
		{"model", documentDescriptor.Model, queryDescriptor.Model}, {"model revision", documentDescriptor.ModelRevision, queryDescriptor.ModelRevision},
		{"metric", documentDescriptor.Metric, queryDescriptor.Metric},
		{"normalization", documentDescriptor.Normalization, queryDescriptor.Normalization},
		{"scalar encoding", documentDescriptor.ScalarEncoding, queryDescriptor.ScalarEncoding},
		{"document formatter", documentDescriptor.DocumentFormatter, queryDescriptor.DocumentFormatter},
		{"query formatter", documentDescriptor.QueryFormatter, queryDescriptor.QueryFormatter},
		{"model-input fingerprint", documentDescriptor.ModelInput.Fingerprint, queryDescriptor.ModelInput.Fingerprint},
	} {
		if field.left != field.right {
			return fmt.Errorf("embedding descriptors have incompatible %s", field.name)
		}
	}
	if documentDescriptor.Dimension != queryDescriptor.Dimension {
		return errors.New("embedding descriptors have incompatible dimension")
	}
	if !slices.Equal(documentDescriptor.InputKinds, queryDescriptor.InputKinds) {
		return errors.New("embedding descriptors have incompatible input representation")
	}
	return nil
}

func validateEmbeddingDescriptor(descriptor EmbeddingDescriptor) error {
	if err := validateEmbeddingDescriptorFields(descriptor); err != nil {
		return err
	}
	if err := validateFingerprint(descriptor.Fingerprint, "embedding descriptor fingerprint"); err != nil {
		return err
	}
	canonical, err := NewEmbeddingDescriptor(descriptor)
	if err != nil {
		return err
	}
	if !equalEmbeddingDescriptors(canonical, descriptor) {
		return errors.New("embedding descriptor fingerprint or canonical ordering is invalid")
	}
	return nil
}

func validateEmbeddingDescriptorFields(descriptor EmbeddingDescriptor) error {
	if err := validateStableToken(descriptor.ID, "embedding descriptor ID", 128); err != nil {
		return err
	}
	if descriptor.ContractVersion != EmbeddingProviderContractVersion {
		return fmt.Errorf("embedding descriptor contract version must be %d", EmbeddingProviderContractVersion)
	}
	if err := validateFingerprint(descriptor.PolicyFingerprint, "embedding descriptor policy fingerprint"); err != nil {
		return err
	}
	switch descriptor.TrustBoundary {
	case EmbeddingTrustLocalProcess, EmbeddingTrustOperatorNetwork, EmbeddingTrustHostedProvider:
	default:
		return errors.New("embedding descriptor trust boundary is invalid")
	}
	for _, field := range []struct{ name, value string }{{"embedding model", descriptor.Model}, {"embedding model revision", descriptor.ModelRevision}} {
		if err := validateStableToken(field.value, field.name, 128); err != nil {
			return err
		}
	}
	if err := validateCompatibilityID(descriptor.CompatibilityID); err != nil {
		return fmt.Errorf("embedding %w", err)
	}
	if descriptor.Dimension < 1 || descriptor.Dimension > maxEmbeddingDimensions {
		return fmt.Errorf("embedding descriptor dimension must be between 1 and %d", maxEmbeddingDimensions)
	}
	if !IsValidVectorMetric(descriptor.Metric) {
		return errors.New("embedding descriptor metric is invalid")
	}
	if !validVectorNormalization(descriptor.Normalization) {
		return errors.New("embedding descriptor normalization is invalid")
	}
	if err := validateStableToken(descriptor.ScalarEncoding, "embedding scalar encoding", 128); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{{"embedding document formatter", descriptor.DocumentFormatter}, {"embedding query formatter", descriptor.QueryFormatter}} {
		if err := validateCompatibilityID(field.value); err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
	}
	if len(descriptor.InputKinds) == 0 || len(descriptor.InputKinds) > 2 {
		return errors.New("embedding descriptor input kinds are invalid")
	}
	for index, kind := range descriptor.InputKinds {
		if !validEmbeddingDocumentInputKind(kind) {
			return fmt.Errorf("embedding descriptor input kind %q is invalid", kind)
		}
		if index > 0 && descriptor.InputKinds[index-1] == kind {
			return errors.New("embedding descriptor has duplicate input kind")
		}
	}
	if err := validateModelInputContract(descriptor.ModelInput); err != nil {
		return err
	}
	if descriptor.ModelInput.Profile == "" {
		return errors.New("embedding descriptor requires a model-input contract")
	}
	if descriptor.CompatibilityID != descriptor.ModelInput.CompatibilityID {
		return errors.New("embedding descriptor compatibility ID does not match model-input contract")
	}
	if len(descriptor.SupportedRequestModes) == 0 {
		return errors.New("embedding descriptor must declare supported request modes")
	}
	for index, mode := range descriptor.SupportedRequestModes {
		if !validModelInputMode(mode) {
			return errors.New("embedding descriptor request mode is invalid")
		}
		if index > 0 && descriptor.SupportedRequestModes[index-1] == mode {
			return errors.New("embedding descriptor has duplicate request mode")
		}
	}
	modes := []ModelInputMode{descriptor.ModelInput.Document.Mode}
	if descriptor.SupportsTextQuery {
		modes = append(modes, descriptor.ModelInput.Query.Mode)
	}
	for _, mode := range modes {
		if !slices.Contains(descriptor.SupportedRequestModes, mode) {
			return errors.New("embedding descriptor does not support a model-input request mode")
		}
	}
	if descriptor.SupportsTextQuery && !slices.Contains(descriptor.SupportedRequestModes, descriptor.ModelInput.Query.Mode) {
		return errors.New("embedding descriptor query request mode is unsupported")
	}
	return nil
}

func validateEmbeddingAuthorization(descriptor EmbeddingDescriptor, authorization EmbeddingAuthorization) error {
	if authorization.ProviderID != descriptor.ID || authorization.DescriptorFingerprint != descriptor.Fingerprint || authorization.PolicyFingerprint != descriptor.PolicyFingerprint {
		return errors.New("embedding authorization does not match descriptor")
	}
	if authorization.MaxBatchItems < 1 || authorization.MaxBatchItems > maxEmbeddingBatchItems {
		return errors.New("embedding authorization batch limit is invalid")
	}
	if authorization.MaxInputBytes < 1 || authorization.MaxInputBytes > maxEmbeddingInputBytes {
		return errors.New("embedding authorization input byte limit is invalid")
	}
	if authorization.MaxResponseBytes < 1 || authorization.MaxResponseBytes > maxEmbeddingResponseBytes {
		return errors.New("embedding authorization response byte limit is invalid")
	}
	if int64(descriptor.Dimension)*4 > authorization.MaxResponseBytes {
		return errors.New("embedding authorization response byte limit cannot hold one vector")
	}
	return nil
}

func validateEmbeddingInputs(
	descriptor EmbeddingDescriptor, inputs []EmbeddingInput, authorization EmbeddingAuthorization,
	metadataSnapshots []AuthorizedUploadMetadata,
) error {
	if len(inputs) == 0 || len(inputs) > authorization.MaxBatchItems {
		return errors.New("embedding request batch size is outside authorization")
	}
	seen := make(map[string]struct{}, len(inputs))
	var total int64
	for index, input := range inputs {
		if err := validateStableToken(input.Key, "embedding input key", 128); err != nil {
			return err
		}
		if _, exists := seen[input.Key]; exists {
			return errors.New("embedding request contains duplicate input key")
		}
		seen[input.Key] = struct{}{}
		if input.Role != EmbeddingRoleDocument && input.Role != EmbeddingRoleQuery {
			return errors.New("embedding request has unsupported role")
		}
		if input.Role == EmbeddingRoleDocument && !slices.Contains(descriptor.InputKinds, input.Kind) {
			return errors.New("embedding request has unsupported kind")
		}
		if input.Role == EmbeddingRoleQuery && (input.Kind != EmbeddingInputQueryText || !descriptor.SupportsTextQuery) {
			return errors.New("embedding request has unsupported role or kind")
		}
		if err := validateEmbeddingInputAuxiliaries(input); err != nil {
			return err
		}
		if input.Kind == EmbeddingInputOriginalFile {
			if nilInterface(input.Source) || input.Text != "" {
				return errors.New("original-file embedding input requires authorized upload")
			}
			metadata := input.Source.Metadata()
			if err := validateAuthorizedUploadMetadata(metadata); err != nil {
				return fmt.Errorf("embedding upload metadata: %w", err)
			}
			if metadata.InputKind != RenditionInputOriginalFile {
				return errors.New("original-file embedding input must use original file upload metadata")
			}
			if second := input.Source.Metadata(); second != metadata {
				return errors.New("embedding upload metadata changed during validation")
			}
			if metadataSnapshots != nil {
				metadataSnapshots[index] = metadata
			}
			if metadata.ByteLength > authorization.MaxInputBytes-total {
				return errors.New("embedding request exceeds authorized input bytes")
			}
			total += metadata.ByteLength
		} else {
			if input.Text == "" || input.Source != nil {
				return errors.New("text embedding input is required")
			}
			if !utf8.ValidString(input.Text) {
				return errors.New("embedding input text is not valid UTF-8")
			}
			encoder := descriptor.ModelInput.Query
			if input.Role == EmbeddingRoleDocument {
				encoder = descriptor.ModelInput.Document
			}
			renderedLength, err := modelInputRenderedLength(encoder, input.Text)
			if err != nil {
				return err
			}
			if renderedLength > authorization.MaxInputBytes-total {
				return errors.New("embedding request exceeds authorized input bytes")
			}
			total += renderedLength
		}
		if total > authorization.MaxInputBytes {
			return errors.New("embedding request exceeds authorized input bytes")
		}
	}
	return nil
}

func validateEmbeddingInputAuxiliaries(input EmbeddingInput) error {
	if input.Kind == EmbeddingInputQueryText || input.Kind == EmbeddingInputOriginalFile {
		if len(input.HeadingPath) != 0 || len(input.SourceSpans) != 0 {
			return errors.New("embedding input auxiliary fields are document-only")
		}
		return nil
	}
	if len(input.HeadingPath) > maxEvidenceHeadingDepth || len(input.SourceSpans) > 1024 {
		return errors.New("embedding input auxiliary slices exceed bounds")
	}
	remainingHeadingBytes := int64(maxEvidenceHeadingBytes)
	for _, heading := range input.HeadingPath {
		if heading == "" || !utf8.ValidString(heading) || strings.IndexFunc(heading, unicode.IsControl) >= 0 || !norm.NFC.IsNormalString(heading) {
			return errors.New("embedding heading is not canonical text")
		}
		if int64(len(heading)) > remainingHeadingBytes {
			return errors.New("embedding heading bytes exceed bounds")
		}
		remainingHeadingBytes -= int64(len(heading))
	}
	var spanBytes int64
	for _, span := range input.SourceSpans {
		if span.UnitIndex < 0 || span.UnitIndex >= maxEvidenceUnits || span.CharStart < 0 || span.CharEnd <= span.CharStart || span.CharEnd > maxEvidenceUnitRunes {
			return errors.New("embedding input has an invalid source span")
		}
		width := int64(span.CharEnd) - int64(span.CharStart)
		if width > maxEmbeddingInputBytes-spanBytes {
			return errors.New("embedding source spans exceed bounds")
		}
		spanBytes += width
	}
	return nil
}

func resultFootprint(result EmbeddingResult) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	var total int64
	for _, vector := range result.Vectors {
		if int64(len(vector.Key)) > maxInt64-total {
			return maxInt64
		}
		total += int64(len(vector.Key))
		if int64(len(vector.Values)) > (maxInt64-total)/4 {
			return maxInt64
		}
		total += int64(len(vector.Values)) * 4
	}
	return total
}

func minimumResultFootprint(dimension int, inputs []EmbeddingInput) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	var total int64
	for _, input := range inputs {
		if int64(len(input.Key)) > maxInt64-total {
			return maxInt64
		}
		total += int64(len(input.Key))
		if int64(dimension) > (maxInt64-total)/4 {
			return maxInt64
		}
		total += int64(dimension) * 4
	}
	return total
}

func validModelInputMode(mode ModelInputMode) bool {
	return mode == ModelInputModeText || mode == ModelInputModeDocument || mode == ModelInputModeQuery
}
func validEmbeddingDocumentInputKind(kind EmbeddingInputKind) bool {
	return kind == EmbeddingInputOriginalFile || kind == EmbeddingInputRenditionChunk
}

// IsValidVectorMetric reports whether metric is part of the durable vector contract vocabulary.
func IsValidVectorMetric(metric string) bool {
	return metric == VectorMetricCosine || metric == VectorMetricDotProduct || metric == VectorMetricL2
}
func embeddingDescriptorIdentity(value EmbeddingDescriptor) embeddingDescriptorFields {
	return embeddingDescriptorFields{ID: value.ID, ContractVersion: value.ContractVersion, PolicyFingerprint: value.PolicyFingerprint, TrustBoundary: value.TrustBoundary, Model: value.Model, ModelRevision: value.ModelRevision, Dimension: value.Dimension, Metric: value.Metric, Normalization: value.Normalization, ScalarEncoding: value.ScalarEncoding, DocumentFormatter: value.DocumentFormatter, QueryFormatter: value.QueryFormatter, InputKinds: value.InputKinds, CompatibilityID: value.CompatibilityID, SupportsTextQuery: value.SupportsTextQuery, ModelInput: value.ModelInput, SupportedRequestModes: value.SupportedRequestModes}
}
func cloneEmbeddingDescriptor(value EmbeddingDescriptor) EmbeddingDescriptor {
	value.InputKinds = slices.Clone(value.InputKinds)
	value.SupportedRequestModes = slices.Clone(value.SupportedRequestModes)
	return value
}
func cloneEmbeddingInputs(inputs []EmbeddingInput) []EmbeddingInput {
	cloned := slices.Clone(inputs)
	for i := range cloned {
		cloned[i].HeadingPath = slices.Clone(inputs[i].HeadingPath)
		cloned[i].SourceSpans = slices.Clone(inputs[i].SourceSpans)
	}
	return cloned
}
func cloneEmbeddingResult(value EmbeddingResult) EmbeddingResult {
	value.Vectors = slices.Clone(value.Vectors)
	for index := range value.Vectors {
		value.Vectors[index].Values = slices.Clone(value.Vectors[index].Values)
		if value.Vectors[index].Index != nil {
			value.Vectors[index].Index = new(*value.Vectors[index].Index)
		}
	}
	return value
}
func equalEmbeddingDescriptors(left, right EmbeddingDescriptor) bool {
	return left.ID == right.ID && left.ContractVersion == right.ContractVersion && left.PolicyFingerprint == right.PolicyFingerprint && left.TrustBoundary == right.TrustBoundary && left.Model == right.Model && left.ModelRevision == right.ModelRevision && left.Dimension == right.Dimension && left.Metric == right.Metric && left.Normalization == right.Normalization && left.ScalarEncoding == right.ScalarEncoding && left.DocumentFormatter == right.DocumentFormatter && left.QueryFormatter == right.QueryFormatter && left.CompatibilityID == right.CompatibilityID && left.SupportsTextQuery == right.SupportsTextQuery && left.ModelInput == right.ModelInput && left.Fingerprint == right.Fingerprint && slices.Equal(left.InputKinds, right.InputKinds) && slices.Equal(left.SupportedRequestModes, right.SupportedRequestModes)
}
