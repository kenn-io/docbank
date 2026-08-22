package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

// DocumentContext is bounded, non-authoritative context attached to document
// inputs. Applications should populate it only from trusted metadata fields.
type DocumentContext struct {
	Filename string `json:"filename,omitempty"`
	Title    string `json:"title,omitempty"`
}

// SourceRef identifies the normalized source evidence represented by an input.
type SourceRef struct {
	ChunkKey      string               `json:"chunk_key"`
	ChunkChecksum string               `json:"chunk_checksum"`
	UnitSpans     []document.ChunkSpan `json:"unit_spans"`
}

type sourceRefJSON struct {
	ChunkKey      string          `json:"chunk_key"`
	ChunkChecksum string          `json:"chunk_checksum"`
	UnitSpans     []chunkSpanJSON `json:"unit_spans"`
}

type chunkSpanJSON struct {
	UnitIndex int `json:"unit_index"`
	CharStart int `json:"char_start"`
	CharEnd   int `json:"char_end"`
}

// MarshalJSON gives SourceRef a stable encoding without changing the JSON
// contract of document.ChunkSpan itself.
func (ref SourceRef) MarshalJSON() ([]byte, error) {
	encoded := sourceRefJSON{ChunkKey: ref.ChunkKey, ChunkChecksum: ref.ChunkChecksum}
	for _, span := range ref.UnitSpans {
		encoded.UnitSpans = append(encoded.UnitSpans, chunkSpanJSON{
			UnitIndex: span.UnitIndex, CharStart: span.CharStart, CharEnd: span.CharEnd,
		})
	}
	return json.Marshal(encoded)
}

// UnmarshalJSON restores SourceRef from its stable encoding.
func (ref *SourceRef) UnmarshalJSON(data []byte) error {
	var encoded sourceRefJSON
	if err := json.Unmarshal(data, &encoded); err != nil {
		return fmt.Errorf("decode embedding source reference: %w", err)
	}
	ref.ChunkKey = encoded.ChunkKey
	ref.ChunkChecksum = encoded.ChunkChecksum
	ref.UnitSpans = make([]document.ChunkSpan, 0, len(encoded.UnitSpans))
	for _, span := range encoded.UnitSpans {
		ref.UnitSpans = append(ref.UnitSpans, document.ChunkSpan{
			UnitIndex: span.UnitIndex, CharStart: span.CharStart, CharEnd: span.CharEnd,
		})
	}
	return nil
}

// SourcePartition is a deterministic, whole-document distillation partition.
// Partitions contain complete normalized chunks and never depend on worker
// batch size or claim timing.
type SourcePartition struct {
	Key        string      `json:"key"`
	Ordinal    int         `json:"ordinal"`
	Text       string      `json:"text"`
	SourceRefs []SourceRef `json:"source_refs"`
	Checksum   string      `json:"checksum"`
}

type partitionIdentity struct {
	SourceChecksum string      `json:"source_checksum"`
	Ordinal        int         `json:"ordinal"`
	Refs           []SourceRef `json:"refs"`
	Checksum       string      `json:"checksum"`
}

type derivedSectionIdentity struct {
	RequestFingerprint string      `json:"request_fingerprint"`
	Ordinal            int         `json:"ordinal"`
	Checksum           string      `json:"checksum"`
	Refs               []SourceRef `json:"refs"`
}

// DistillationRequest is the immutable input to a Distiller.
type DistillationRequest struct {
	RecipeFingerprint     string            `json:"recipe_fingerprint"`
	SourceChecksum        string            `json:"source_checksum"`
	Context               DocumentContext   `json:"context"`
	ContextFingerprint    string            `json:"context_fingerprint"`
	Provider              string            `json:"provider"`
	Model                 string            `json:"model"`
	ModelRevision         string            `json:"model_revision"`
	PromptTemplateVersion int               `json:"prompt_template_version"`
	MaxSections           int               `json:"max_sections"`
	MaxSectionRunes       int               `json:"max_section_runes"`
	Partitions            []SourcePartition `json:"partitions"`
	Fingerprint           string            `json:"fingerprint"`
}

// Distiller transforms deterministic source partitions into derived sections.
// Implementations own provider transport, authentication, retry, redirect,
// and per-request consent enforcement.
type Distiller interface {
	Distill(ctx context.Context, request DistillationRequest) (DistillationResult, error)
}

// DistillationResult is provider output awaiting structural validation.
type DistillationResult struct {
	Provider      string                 `json:"provider"`
	Model         string                 `json:"model"`
	ModelRevision string                 `json:"model_revision"`
	Sections      []DerivedSectionResult `json:"sections"`
}

// DerivedSectionResult is one provider-produced section and the exact source
// partitions it summarizes.
type DerivedSectionResult struct {
	Text          string   `json:"text"`
	PartitionKeys []string `json:"partition_keys"`
}

// DerivedSection is validated, content-addressed derived evidence. Its text is
// untrusted and non-authoritative; SourceRefs point back to normalized evidence.
type DerivedSection struct {
	Key        string      `json:"key"`
	Ordinal    int         `json:"ordinal"`
	Text       string      `json:"text"`
	SourceRefs []SourceRef `json:"source_refs"`
	Checksum   string      `json:"checksum"`
}

// Distillate is a validated, content-addressed distillation artifact.
type Distillate struct {
	RequestFingerprint string           `json:"request_fingerprint"`
	RecipeFingerprint  string           `json:"recipe_fingerprint"`
	SourceChecksum     string           `json:"source_checksum"`
	ContextFingerprint string           `json:"context_fingerprint"`
	Provider           string           `json:"provider"`
	Model              string           `json:"model"`
	ModelRevision      string           `json:"model_revision"`
	Sections           []DerivedSection `json:"sections"`
	Fingerprint        string           `json:"fingerprint"`
}

// PrepareDistillation deterministically partitions a complete normalized
// document for the recipe's configured Distiller.
func PrepareDistillation(normalized document.NormalizedDocument, context DocumentContext, recipe Recipe) (DistillationRequest, error) {
	if err := validateNormalizedDocument(normalized); err != nil {
		return DistillationRequest{}, err
	}
	if !recipe.valid() || recipe.values.Distillation == nil {
		return DistillationRequest{}, errors.New("embedding recipe does not configure distillation")
	}
	context = normalizeContext(context, recipe.values)
	contextFingerprint, err := digestJSON(context)
	if err != nil {
		return DistillationRequest{}, err
	}
	partitions, err := partitionDocument(normalized, recipe.values.Distillation.MaxPartitionRunes)
	if err != nil {
		return DistillationRequest{}, err
	}
	config := recipe.values.Distillation
	request := DistillationRequest{
		RecipeFingerprint: recipe.digest, SourceChecksum: normalized.Checksum,
		Context: context, ContextFingerprint: contextFingerprint,
		Provider: config.Provider, Model: config.Model, ModelRevision: config.ModelRevision,
		PromptTemplateVersion: config.PromptTemplateVersion, MaxSections: config.MaxSections,
		MaxSectionRunes: config.MaxSectionRunes, Partitions: partitions,
	}
	fingerprint, err := distillationRequestFingerprint(request)
	if err != nil {
		return DistillationRequest{}, err
	}
	request.Fingerprint = fingerprint
	return request, nil
}

func partitionDocument(normalized document.NormalizedDocument, maxRunes int) ([]SourcePartition, error) {
	partitions := make([]SourcePartition, 0, len(normalized.Chunks))
	type partitionChunk struct {
		chunk document.Chunk
		text  string
	}
	var chunks []partitionChunk
	runes := 0
	flush := func() error {
		if len(chunks) == 0 {
			return nil
		}
		partition := SourcePartition{Ordinal: len(partitions)}
		texts := make([]string, 0, len(chunks))
		for _, item := range chunks {
			texts = append(texts, item.text)
			partition.SourceRefs = append(partition.SourceRefs, sourceRef(item.chunk))
		}
		partition.Text = strings.Join(texts, "\n\n")
		partition.Checksum = fingerprint([]byte(partition.Text))
		keyBytes, err := json.Marshal(partitionIdentity{
			SourceChecksum: normalized.Checksum, Ordinal: partition.Ordinal,
			Refs: partition.SourceRefs, Checksum: partition.Checksum,
		})
		if err != nil {
			return fmt.Errorf("encode distillation partition identity: %w", err)
		}
		partition.Key = fingerprint(keyBytes)
		partitions = append(partitions, partition)
		chunks = nil
		runes = 0
		return nil
	}
	for _, chunk := range normalized.Chunks {
		chunkText := formatDistillationChunk(normalized, chunk)
		chunkRunes := utf8.RuneCountInString(chunkText)
		if chunkRunes > maxRunes {
			return nil, fmt.Errorf("normalized chunk %q exceeds distillation partition limit", chunk.Key)
		}
		separator := 0
		if len(chunks) > 0 {
			separator = 2
		}
		if runes+separator+chunkRunes > maxRunes {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		chunks = append(chunks, partitionChunk{chunk: chunk, text: chunkText})
		runes += separator + chunkRunes
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return partitions, nil
}

func formatDistillationChunk(normalized document.NormalizedDocument, chunk document.Chunk) string {
	var builder strings.Builder
	if len(chunk.HeadingPath) > 0 {
		builder.WriteString("Heading: ")
		builder.WriteString(strings.Join(chunk.HeadingPath, " > "))
		builder.WriteByte('\n')
	}
	builder.WriteString("Source: ")
	builder.WriteString(formatLocator(normalized, chunk.Spans))
	builder.WriteString("\nContent:\n")
	builder.WriteString(chunk.Text)
	return builder.String()
}

// ValidateDistillate validates provider output, attaches exact source
// provenance, and returns a content-addressed artifact.
func ValidateDistillate(request DistillationRequest, result DistillationResult) (Distillate, error) {
	if err := validateDistillationRequest(request); err != nil {
		return Distillate{}, err
	}
	if result.Provider != request.Provider || result.Model != request.Model || result.ModelRevision != request.ModelRevision {
		return Distillate{}, errors.New("distillation result target differs from request")
	}
	if len(result.Sections) == 0 || len(result.Sections) > request.MaxSections {
		return Distillate{}, errors.New("distillation result section count is outside request bounds")
	}
	partitionByKey := make(map[string]SourcePartition, len(request.Partitions))
	partitionOrder := make(map[string]int, len(request.Partitions))
	for index, partition := range request.Partitions {
		partitionByKey[partition.Key] = partition
		partitionOrder[partition.Key] = index
	}
	seen := make(map[string]bool, len(request.Partitions))
	nextPartition := 0
	sections := make([]DerivedSection, 0, len(result.Sections))
	for index, candidate := range result.Sections {
		if candidate.Text == "" || !utf8.ValidString(candidate.Text) || utf8.RuneCountInString(candidate.Text) > request.MaxSectionRunes {
			return Distillate{}, fmt.Errorf("distillation section %d text is invalid or exceeds its rune limit", index)
		}
		text := normalizeDerivedText(candidate.Text)
		if text == "" {
			return Distillate{}, fmt.Errorf("distillation section %d has no usable text", index)
		}
		if len(candidate.PartitionKeys) == 0 {
			return Distillate{}, fmt.Errorf("distillation section %d has no source partitions", index)
		}
		section := DerivedSection{Ordinal: index, Text: text}
		for _, key := range candidate.PartitionKeys {
			partition, ok := partitionByKey[key]
			if !ok {
				return Distillate{}, fmt.Errorf("distillation section %d references unknown partition %q", index, key)
			}
			if seen[key] || partitionOrder[key] != nextPartition {
				return Distillate{}, errors.New("distillation result must cover source partitions exactly once in source order")
			}
			seen[key] = true
			nextPartition++
			section.SourceRefs = append(section.SourceRefs, cloneSourceRefs(partition.SourceRefs)...)
		}
		section.Checksum = fingerprint([]byte(section.Text))
		keyBytes, err := json.Marshal(derivedSectionIdentity{
			RequestFingerprint: request.Fingerprint, Ordinal: section.Ordinal,
			Checksum: section.Checksum, Refs: section.SourceRefs,
		})
		if err != nil {
			return Distillate{}, fmt.Errorf("encode derived section identity: %w", err)
		}
		section.Key = fingerprint(keyBytes)
		sections = append(sections, section)
	}
	if nextPartition != len(request.Partitions) {
		return Distillate{}, errors.New("distillation result does not cover every source partition")
	}
	distillate := Distillate{
		RequestFingerprint: request.Fingerprint, RecipeFingerprint: request.RecipeFingerprint,
		SourceChecksum: request.SourceChecksum, ContextFingerprint: request.ContextFingerprint,
		Provider: result.Provider, Model: result.Model, ModelRevision: result.ModelRevision,
		Sections: sections,
	}
	fingerprint, err := distillateFingerprint(distillate)
	if err != nil {
		return Distillate{}, err
	}
	distillate.Fingerprint = fingerprint
	return distillate, nil
}

func validateDistillationRequest(request DistillationRequest) error {
	if request.Fingerprint == "" || request.RecipeFingerprint == "" || request.SourceChecksum == "" ||
		request.Provider == "" || request.Model == "" || request.ModelRevision == "" ||
		request.PromptTemplateVersion < 1 || request.MaxSections < 1 || request.MaxSectionRunes < 1 || len(request.Partitions) == 0 {
		return errors.New("distillation request is incomplete")
	}
	contextFingerprint, err := digestJSON(request.Context)
	if err != nil {
		return err
	}
	if contextFingerprint != request.ContextFingerprint {
		return errors.New("distillation request context fingerprint is invalid")
	}
	for index, partition := range request.Partitions {
		if partition.Ordinal != index || partition.Key == "" || partition.Text == "" || partition.Checksum != fingerprint([]byte(partition.Text)) || len(partition.SourceRefs) == 0 {
			return fmt.Errorf("distillation partition %d is invalid", index)
		}
		if err := validateSourceRefs(partition.SourceRefs); err != nil {
			return fmt.Errorf("distillation partition %d: %w", index, err)
		}
		keyBytes, err := json.Marshal(partitionIdentity{
			SourceChecksum: request.SourceChecksum, Ordinal: partition.Ordinal,
			Refs: partition.SourceRefs, Checksum: partition.Checksum,
		})
		if err != nil {
			return fmt.Errorf("encode distillation partition identity: %w", err)
		}
		if partition.Key != fingerprint(keyBytes) {
			return fmt.Errorf("distillation partition %d key is invalid", index)
		}
	}
	expected, err := distillationRequestFingerprint(request)
	if err != nil {
		return err
	}
	if expected != request.Fingerprint {
		return errors.New("distillation request fingerprint is invalid")
	}
	return nil
}

func validateSourceRefs(refs []SourceRef) error {
	for _, ref := range refs {
		if ref.ChunkKey == "" || ref.ChunkChecksum == "" || len(ref.UnitSpans) == 0 {
			return errors.New("source reference is incomplete")
		}
		for _, span := range ref.UnitSpans {
			if span.UnitIndex < 0 || span.CharStart < 0 || span.CharEnd <= span.CharStart {
				return errors.New("source reference span is invalid")
			}
		}
	}
	return nil
}

func distillationRequestFingerprint(request DistillationRequest) (string, error) {
	request.Fingerprint = ""
	return digestJSON(request)
}

func distillateFingerprint(distillate Distillate) (string, error) {
	distillate.Fingerprint = ""
	return digestJSON(distillate)
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode embedding identity: %w", err)
	}
	return fingerprint(encoded), nil
}

func normalizeContext(value DocumentContext, recipe RecipeValues) DocumentContext {
	return DocumentContext{
		Filename: truncateRunes(normalizeMetadata(value.Filename), recipe.MaxFilenameRunes),
		Title:    truncateRunes(normalizeMetadata(value.Title), recipe.MaxTitleRunes),
	}
}

func normalizeMetadata(value string) string {
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf)
	}), " ")
}

func normalizeDerivedText(value string) string {
	var builder strings.Builder
	for _, r := range strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n") {
		switch {
		case r == '\n':
			builder.WriteByte('\n')
		case unicode.IsSpace(r):
			builder.WriteByte(' ')
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf):
			continue
		default:
			builder.WriteRune(r)
		}
	}
	return strings.TrimSpace(builder.String())
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}

func sourceRef(chunk document.Chunk) SourceRef {
	return SourceRef{ChunkKey: chunk.Key, ChunkChecksum: chunk.Checksum, UnitSpans: slices.Clone(chunk.Spans)}
}

func cloneSourceRefs(refs []SourceRef) []SourceRef {
	result := make([]SourceRef, len(refs))
	for index, ref := range refs {
		result[index] = ref
		result[index].UnitSpans = slices.Clone(ref.UnitSpans)
	}
	return result
}
