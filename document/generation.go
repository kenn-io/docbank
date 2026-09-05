package document

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"golang.org/x/text/unicode/norm"
)

const (
	EmbeddingInputGenerationVersion = 1

	maxAttachmentTitleBytes   = 512
	maxAttachmentContextBytes = 4096
	maxGeneratedInputs        = 100_000
	maxGenerationTotalTokens  = int64(10_000_000)
	maxGenerationTotalBytes   = int64(1 << 30)
	maxGenerationEncodedBytes = int64(16 << 30)
)

// AttachmentContextSnapshot seals the exact human-authored title and context
// that entered one generation. Paths, collection names, and other mutable
// navigation metadata do not belong here; the profile chunk policy's
// ContextFingerprint governs which context may be declared at all.
type AttachmentContextSnapshot struct {
	Title   string `json:"title,omitempty"`
	Context string `json:"context,omitempty"`
}

// NewAttachmentContextSnapshot validates human-authored attachment context.
// An all-empty snapshot is not a declared attachment context.
func NewAttachmentContextSnapshot(title, context string) (AttachmentContextSnapshot, error) {
	snapshot := AttachmentContextSnapshot{Title: title, Context: context}
	if err := validateAttachmentContextSnapshot(snapshot); err != nil {
		return AttachmentContextSnapshot{}, err
	}
	return snapshot, nil
}

func validateAttachmentContextSnapshot(snapshot AttachmentContextSnapshot) error {
	if snapshot.Title == "" && snapshot.Context == "" {
		return errors.New("attachment context snapshot must contain a title or context")
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{{"title", snapshot.Title, maxAttachmentTitleBytes}, {"context", snapshot.Context, maxAttachmentContextBytes}} {
		if !utf8.ValidString(field.value) || len(field.value) > field.limit || strings.ContainsAny(field.value, "\x00\r") || !norm.NFC.IsNormalString(field.value) {
			return fmt.Errorf("attachment context %s must be bounded valid UTF-8 NFC text", field.name)
		}
	}
	return nil
}

// GeneratedEmbeddingInput is one exact provider-ready document input.
type GeneratedEmbeddingInput struct {
	Key            string
	Content        string
	Rendered       string
	ContentTokens  int
	RenderedTokens int
	Checksum       string
	HeadingPath    []string
	SourceSpan     ChunkSpan
	Truncated      bool
}

// generatedEmbeddingInputJSON gives generated inputs a snake_case wire form
// without changing the frozen legacy ChunkSpan encoding.
type generatedEmbeddingInputJSON struct {
	Key            string                  `json:"key"`
	Content        string                  `json:"content"`
	Rendered       string                  `json:"rendered"`
	ContentTokens  int                     `json:"content_tokens"`
	RenderedTokens int                     `json:"rendered_tokens"`
	Checksum       string                  `json:"checksum"`
	HeadingPath    []string                `json:"heading_path,omitempty"`
	SourceSpan     generatedSourceSpanJSON `json:"source_span"`
	Truncated      bool                    `json:"truncated"`
}

type generatedSourceSpanJSON struct {
	UnitIndex int `json:"unit_index"`
	CharStart int `json:"char_start"`
	CharEnd   int `json:"char_end"`
}

func (input GeneratedEmbeddingInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(generatedEmbeddingInputJSON{
		Key: input.Key, Content: input.Content, Rendered: input.Rendered,
		ContentTokens: input.ContentTokens, RenderedTokens: input.RenderedTokens,
		Checksum: input.Checksum, HeadingPath: input.HeadingPath,
		SourceSpan: generatedSourceSpanJSON(input.SourceSpan), Truncated: input.Truncated,
	}, json.Deterministic(true))
}

func (input *GeneratedEmbeddingInput) UnmarshalJSON(data []byte) error {
	var encoded generatedEmbeddingInputJSON
	if err := json.Unmarshal(data, &encoded, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("decode generated embedding input: %w", err)
	}
	*input = GeneratedEmbeddingInput{
		Key: encoded.Key, Content: encoded.Content, Rendered: encoded.Rendered,
		ContentTokens: encoded.ContentTokens, RenderedTokens: encoded.RenderedTokens,
		Checksum: encoded.Checksum, HeadingPath: encoded.HeadingPath,
		SourceSpan: ChunkSpan(encoded.SourceSpan), Truncated: encoded.Truncated,
	}
	return nil
}

// EmbeddingInputGeneration is one ordered projection of normalized evidence
// under one sealed input policy. Every identity input is self-described so a
// persisted generation can prove its own policy fingerprint. Callers persist
// the bytes from MarshalEmbeddingInputGeneration or clone the value before
// exposing its mutable slices.
type EmbeddingInputGeneration struct {
	Version                    int                        `json:"version"`
	Checksum                   string                     `json:"checksum"`
	PolicyFingerprint          string                     `json:"policy_fingerprint"`
	EvidenceChecksum           string                     `json:"evidence_checksum"`
	Chunk                      EmbeddingChunkPolicyV1     `json:"chunk"`
	ModelInputFingerprint      string                     `json:"model_input_fingerprint"`
	LexicalEvidenceFingerprint string                     `json:"lexical_evidence_fingerprint"`
	MaxInputTokens             int                        `json:"max_input_tokens"`
	MaxInputBytes              int64                      `json:"max_input_bytes"`
	AttachmentContext          *AttachmentContextSnapshot `json:"attachment_context,omitempty"`
	TotalContentTokens         int64                      `json:"total_content_tokens"`
	TotalRenderedTokens        int64                      `json:"total_rendered_tokens"`
	TotalContentBytes          int64                      `json:"total_content_bytes"`
	TotalRenderedBytes         int64                      `json:"total_rendered_bytes"`
	Inputs                     []GeneratedEmbeddingInput  `json:"inputs"`
}

// inputPolicyIdentity is every input that changes which chunks a policy can
// produce. Generation limits are deliberately absent: they can only turn a
// success into an error and never alter a successful output.
type inputPolicyIdentity struct {
	AttachmentContext          *AttachmentContextSnapshot `json:"attachment_context,omitempty"`
	Chunk                      EmbeddingChunkPolicyV1     `json:"chunk"`
	LexicalEvidenceFingerprint string                     `json:"lexical_evidence_fingerprint"`
	MaxInputBytes              int64                      `json:"max_input_bytes"`
	MaxInputTokens             int                        `json:"max_input_tokens"`
	ModelInputFingerprint      string                     `json:"model_input_fingerprint"`
}

func (generation EmbeddingInputGeneration) policyIdentity() inputPolicyIdentity {
	return inputPolicyIdentity{
		AttachmentContext: generation.AttachmentContext, Chunk: generation.Chunk,
		LexicalEvidenceFingerprint: generation.LexicalEvidenceFingerprint,
		MaxInputBytes:              generation.MaxInputBytes, MaxInputTokens: generation.MaxInputTokens,
		ModelInputFingerprint: generation.ModelInputFingerprint,
	}
}

func inputPolicyFingerprint(identity inputPolicyIdentity) (string, error) {
	return componentFingerprint("embedding_input_policy", identity)
}

func generationChecksum(generation EmbeddingInputGeneration) (string, error) {
	generation.Checksum = ""
	encoded, err := canonical.Marshal(generation)
	if err != nil {
		return "", fmt.Errorf("encode embedding input generation: %w", err)
	}
	return sha256Hex(encoded), nil
}

// MarshalEmbeddingInputGeneration returns the one canonical byte form of a
// validated generation.
func MarshalEmbeddingInputGeneration(generation EmbeddingInputGeneration) ([]byte, error) {
	if err := validateEmbeddingInputGeneration(generation); err != nil {
		return nil, err
	}
	encoded, err := canonical.Marshal(generation)
	if err != nil {
		return nil, fmt.Errorf("encode embedding input generation: %w", err)
	}
	return encoded, nil
}

// EmbeddingInputGenerationDecodeBounds authorizes the encoded artifact before
// canonical decoding begins.
type EmbeddingInputGenerationDecodeBounds struct {
	MaxEncodedBytes int64
	MaxInputs       int
}

// DecodeEmbeddingInputGeneration accepts only the exact canonical encoding of
// a sealed generation. Unknown fields, forged totals, forged policy
// fingerprints, and stale checksums are rejected.
func DecodeEmbeddingInputGeneration(data []byte, bounds EmbeddingInputGenerationDecodeBounds) (EmbeddingInputGeneration, error) {
	if bounds.MaxEncodedBytes < 1 || bounds.MaxEncodedBytes > maxGenerationEncodedBytes || int64(len(data)) > bounds.MaxEncodedBytes {
		return EmbeddingInputGeneration{}, errors.New("embedding generation encoded bytes exceed bounds")
	}
	if bounds.MaxInputs < 1 || bounds.MaxInputs > maxGeneratedInputs {
		return EmbeddingInputGeneration{}, errors.New("embedding generation input decode bound is invalid")
	}
	generation, err := canonical.Decode[EmbeddingInputGeneration](data)
	if err != nil {
		return EmbeddingInputGeneration{}, fmt.Errorf("decode embedding input generation: %w", err)
	}
	if len(generation.Inputs) > bounds.MaxInputs {
		return EmbeddingInputGeneration{}, errors.New("embedding generation input count exceeds bounds")
	}
	if err := validateEmbeddingInputGeneration(generation); err != nil {
		return EmbeddingInputGeneration{}, err
	}
	return generation, nil
}

// ValidateEvidence verifies that each input's text and headings come from its
// declared span in the canonical evidence. Checksums within a generation alone
// cannot establish this relationship.
func (generation EmbeddingInputGeneration) ValidateEvidence(evidence NormalizedEvidenceV1) error {
	if err := validateEmbeddingInputGeneration(generation); err != nil {
		return err
	}
	_, checksum, err := MarshalNormalizedEvidenceV1(evidence)
	if err != nil {
		return err
	}
	if checksum != generation.EvidenceChecksum {
		return errors.New("embedding generation evidence checksum mismatch")
	}
	unitRunes := make(map[int][]rune)
	for index, input := range generation.Inputs {
		span := input.SourceSpan
		if span.UnitIndex < 0 || span.UnitIndex >= len(evidence.Units) {
			return fmt.Errorf("embedding input %d source span names a missing evidence unit", index)
		}
		unit := evidence.Units[span.UnitIndex]
		runes, ok := unitRunes[span.UnitIndex]
		if !ok {
			runes = []rune(unit.Text)
			unitRunes[span.UnitIndex] = runes
		}
		if span.CharStart < 0 || span.CharEnd <= span.CharStart || span.CharEnd > len(runes) ||
			string(runes[span.CharStart:span.CharEnd]) != input.Content {
			return fmt.Errorf("embedding input %d content does not match evidence source span", index)
		}
		if !slices.Equal(input.HeadingPath, unit.HeadingPath) {
			return fmt.Errorf("embedding input %d headings do not match evidence source span", index)
		}
	}
	return nil
}

// ToEmbeddingInputs reconstructs the contextualized pre-envelope text for E1.
// The complete model-input fingerprint must match before any input is exposed,
// even when two contracts happen to share a document envelope.
func (generation EmbeddingInputGeneration) ToEmbeddingInputs(contract ModelInputContract) ([]EmbeddingInput, error) {
	if err := validateModelInputContract(contract); err != nil {
		return nil, err
	}
	if contract.Fingerprint != generation.ModelInputFingerprint {
		return nil, errors.New("embedding generation model-input fingerprint does not match contract")
	}
	if err := validateEmbeddingInputGeneration(generation); err != nil {
		return nil, err
	}
	attachment := AttachmentContextSnapshot{}
	if generation.AttachmentContext != nil {
		attachment = *generation.AttachmentContext
	}
	result := make([]EmbeddingInput, len(generation.Inputs))
	for index, input := range generation.Inputs {
		contextualized := contextualizeDocumentInput(attachment, input.Content)
		if contract.EncodeDocument(contextualized) != input.Rendered || sha256Hex([]byte(input.Rendered)) != input.Checksum {
			return nil, errors.New("generated embedding input does not match model-input contract")
		}
		result[index] = EmbeddingInput{
			Key: input.Key, Role: EmbeddingRoleDocument, Kind: EmbeddingInputRenditionChunk,
			Text: contextualized, HeadingPath: slices.Clone(input.HeadingPath), SourceSpans: []ChunkSpan{input.SourceSpan},
		}
	}
	return result, nil
}

func generatedInputKey(ordinal int, checksum string) string {
	return fmt.Sprintf("chunk-%06d-%s", ordinal, checksum[:min(12, len(checksum))])
}

func validateEmbeddingInputGeneration(generation EmbeddingInputGeneration) error {
	if generation.Version != EmbeddingInputGenerationVersion {
		return fmt.Errorf("embedding input generation version must be %d", EmbeddingInputGenerationVersion)
	}
	for _, fingerprint := range []struct{ value, name string }{
		{generation.Checksum, "generation checksum"},
		{generation.PolicyFingerprint, "generation policy fingerprint"},
		{generation.EvidenceChecksum, "generation evidence checksum"},
		{generation.LexicalEvidenceFingerprint, "generation lexical evidence fingerprint"},
		{generation.ModelInputFingerprint, "generation model-input fingerprint"},
	} {
		if err := validateFingerprint(fingerprint.value, fingerprint.name); err != nil {
			return err
		}
	}
	if err := validateChunkPolicy(generation.Chunk); err != nil {
		return fmt.Errorf("embedding generation %w", err)
	}
	if err := validateProviderInputLimits(generation.MaxInputTokens, generation.MaxInputBytes); err != nil {
		return err
	}
	if generation.AttachmentContext != nil {
		if err := validateAttachmentContextSnapshot(*generation.AttachmentContext); err != nil {
			return fmt.Errorf("embedding generation %w", err)
		}
	}
	if len(generation.Inputs) > maxGeneratedInputs {
		return errors.New("embedding generation input count exceeds bounds")
	}
	var totals generationTotals
	for index, input := range generation.Inputs {
		if err := validateGeneratedInput(generation, index, input); err != nil {
			return err
		}
		if !totals.add(input, generationTotals{maxGenerationTotalTokens, maxGenerationTotalTokens, maxGenerationTotalBytes, maxGenerationTotalBytes}) {
			return errors.New("embedding generation aggregate values exceed bounds")
		}
	}
	if totals != (generationTotals{generation.TotalContentTokens, generation.TotalRenderedTokens, generation.TotalContentBytes, generation.TotalRenderedBytes}) {
		return errors.New("embedding generation aggregate totals are invalid")
	}
	policyFingerprint, err := inputPolicyFingerprint(generation.policyIdentity())
	if err != nil || policyFingerprint != generation.PolicyFingerprint {
		return errors.New("embedding input generation policy fingerprint is invalid")
	}
	checksum, err := generationChecksum(generation)
	if err != nil || checksum != generation.Checksum {
		return errors.New("embedding input generation checksum is invalid")
	}
	return nil
}

func validateGeneratedInput(generation EmbeddingInputGeneration, index int, input GeneratedEmbeddingInput) error {
	if err := validateStableToken(input.Key, "generated embedding input key", 128); err != nil {
		return err
	}
	if input.Key != generatedInputKey(index, input.Checksum) {
		return errors.New("generated embedding input key is not canonical")
	}
	if input.Content == "" || input.Rendered == "" || !utf8.ValidString(input.Content) || !utf8.ValidString(input.Rendered) {
		return errors.New("generated embedding input text is invalid")
	}
	if input.ContentTokens < 1 || input.ContentTokens > generation.Chunk.MaxTokens || input.RenderedTokens < 1 || input.RenderedTokens > generation.MaxInputTokens {
		return errors.New("generated embedding input token counts exceed the sealed limits")
	}
	if int64(len(input.Rendered)) > generation.MaxInputBytes {
		return errors.New("generated embedding input rendered bytes exceed the sealed limit")
	}
	if sha256Hex([]byte(input.Rendered)) != input.Checksum {
		return errors.New("generated embedding input checksum is invalid")
	}
	auxiliaries := EmbeddingInput{Role: EmbeddingRoleDocument, Kind: EmbeddingInputRenditionChunk, Text: input.Content, HeadingPath: input.HeadingPath, SourceSpans: []ChunkSpan{input.SourceSpan}}
	if err := validateEmbeddingInputAuxiliaries(auxiliaries); err != nil {
		return fmt.Errorf("generated embedding input auxiliaries: %w", err)
	}
	return nil
}

func validateProviderInputLimits(maxInputTokens int, maxInputBytes int64) error {
	if maxInputTokens < 1 || maxInputTokens > maxEmbeddingChunkTokens || maxInputBytes < 1 || maxInputBytes > maxEmbeddingInputBytes {
		return errors.New("embedding provider input limits are invalid")
	}
	return nil
}

type generationTotals struct {
	contentTokens, renderedTokens, contentBytes, renderedBytes int64
}

func (totals *generationTotals) add(input GeneratedEmbeddingInput, limit generationTotals) bool {
	contentBytes := int64(len(input.Content))
	renderedBytes := int64(len(input.Rendered))
	if !addWithinAggregate(totals.contentTokens, int64(input.ContentTokens), limit.contentTokens) ||
		!addWithinAggregate(totals.renderedTokens, int64(input.RenderedTokens), limit.renderedTokens) ||
		!addWithinAggregate(totals.contentBytes, contentBytes, limit.contentBytes) ||
		!addWithinAggregate(totals.renderedBytes, renderedBytes, limit.renderedBytes) {
		return false
	}
	totals.contentTokens += int64(input.ContentTokens)
	totals.renderedTokens += int64(input.RenderedTokens)
	totals.contentBytes += contentBytes
	totals.renderedBytes += renderedBytes
	return true
}

func addWithinAggregate(current, addition, limit int64) bool {
	return current >= 0 && addition >= 0 && limit >= 0 && current <= limit && addition <= limit-current
}
