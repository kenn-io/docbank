package embedding

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

// RepresentationKind identifies the source representation of an input.
type RepresentationKind string

const (
	RepresentationKindRaw       RepresentationKind = "raw"
	RepresentationKindDistilled RepresentationKind = "distilled"
)

// EmbeddingInput is one deterministic, bounded provider input.
type EmbeddingInput struct {
	Key        string             `json:"key"`
	Ordinal    int                `json:"ordinal"`
	Kind       RepresentationKind `json:"kind"`
	Text       string             `json:"text"`
	SourceRefs []SourceRef        `json:"source_refs"`
	Checksum   string             `json:"checksum"`
	Truncated  bool               `json:"truncated"`
}

type embeddingInputIdentity struct {
	Ordinal   int                `json:"ordinal"`
	Kind      RepresentationKind `json:"kind"`
	Checksum  string             `json:"checksum"`
	Refs      []SourceRef        `json:"refs"`
	Truncated bool               `json:"truncated"`
}

// EmbeddingPlan is an immutable set of inputs for one complete normalized
// document. Applications can claim these plan inputs independently without
// changing their text or identity.
type EmbeddingPlan struct {
	RecipeFingerprint     string           `json:"recipe_fingerprint"`
	SourceChecksum        string           `json:"source_checksum"`
	ContextFingerprint    string           `json:"context_fingerprint"`
	DistillateFingerprint string           `json:"distillate_fingerprint,omitempty"`
	Inputs                []EmbeddingInput `json:"inputs"`
	Fingerprint           string           `json:"fingerprint"`
}

// BuildEmbeddingPlan constructs all inputs for a complete normalized document.
// Distilled and combined recipes require the exact validated distillate
// prepared from the same document, recipe, and context.
func BuildEmbeddingPlan(normalized document.NormalizedDocument, context DocumentContext, recipe Recipe, distillate *Distillate) (EmbeddingPlan, error) {
	if err := validateNormalizedDocument(normalized); err != nil {
		return EmbeddingPlan{}, err
	}
	if !recipe.valid() {
		return EmbeddingPlan{}, errors.New("embedding recipe is invalid; use NewRecipe")
	}
	context, err := normalizeContext(context, recipe.values)
	if err != nil {
		return EmbeddingPlan{}, err
	}
	contextFingerprint, err := digestJSON(context)
	if err != nil {
		return EmbeddingPlan{}, err
	}
	if recipe.values.Mode == RepresentationRaw && distillate != nil {
		return EmbeddingPlan{}, errors.New("raw embedding recipe does not accept a distillate")
	}
	if recipe.values.Mode != RepresentationRaw {
		if distillate == nil {
			return EmbeddingPlan{}, errors.New("distilled and combined embedding recipes require a distillate")
		}
		if err := validateDistillateForPlan(*distillate, normalized.Checksum, contextFingerprint, recipe); err != nil {
			return EmbeddingPlan{}, err
		}
	}

	plan := EmbeddingPlan{
		RecipeFingerprint: recipe.digest, SourceChecksum: normalized.Checksum,
		ContextFingerprint: contextFingerprint,
	}
	if recipe.values.Mode == RepresentationRaw || recipe.values.Mode == RepresentationCombined {
		for _, chunk := range normalized.Chunks {
			input, err := rawInput(
				len(plan.Inputs), normalized, chunk, context,
				recipe.values.MaxInputRunes, recipe.values.MaxHeadingRunes,
			)
			if err != nil {
				return EmbeddingPlan{}, err
			}
			plan.Inputs = append(plan.Inputs, input)
		}
	}
	if recipe.values.Mode == RepresentationDistilled || recipe.values.Mode == RepresentationCombined {
		plan.DistillateFingerprint = distillate.Fingerprint
		for _, section := range distillate.Sections {
			input, err := distilledInput(len(plan.Inputs), section, context, recipe.values.MaxInputRunes)
			if err != nil {
				return EmbeddingPlan{}, err
			}
			plan.Inputs = append(plan.Inputs, input)
		}
	}
	if len(plan.Inputs) == 0 {
		return EmbeddingPlan{}, errors.New("embedding plan has no eligible inputs")
	}
	fingerprint, err := embeddingPlanFingerprint(plan)
	if err != nil {
		return EmbeddingPlan{}, err
	}
	plan.Fingerprint = fingerprint
	return plan, nil
}

func rawInput(ordinal int, normalized document.NormalizedDocument, chunk document.Chunk, context DocumentContext, maxRunes, maxHeadingRunes int) (EmbeddingInput, error) {
	sourcePrefix := "Source: " + formatLocator(normalized, chunk.Spans) + "\nContent:\n"
	contextPrefix, contextTruncated := boundedContext(
		context, maxRunes-utf8.RuneCountInString(sourcePrefix)-1,
	)
	heading, headingTruncated := boundedHeadingContext(
		chunk.HeadingPath, maxHeadingRunes,
		maxRunes-utf8.RuneCountInString(contextPrefix)-utf8.RuneCountInString(sourcePrefix)-1,
	)
	prefix := contextPrefix + heading + sourcePrefix
	text, truncated, err := boundedInput(prefix, chunk.Text, maxRunes)
	if err != nil {
		return EmbeddingInput{}, fmt.Errorf("format normalized chunk %q: %w", chunk.Key, err)
	}
	return makeInput(
		ordinal, RepresentationKindRaw, text, []SourceRef{sourceRef(chunk)},
		chunk.Truncated || contextTruncated || headingTruncated || truncated,
	)
}

func distilledInput(ordinal int, section DerivedSection, context DocumentContext, maxRunes int) (EmbeddingInput, error) {
	sourcePrefix := "Source: " + formatSourceRefs(section.SourceRefs) + "\nDerived summary (verify against source):\n"
	contextPrefix, contextTruncated := boundedContext(
		context, maxRunes-utf8.RuneCountInString(sourcePrefix)-1,
	)
	prefix := contextPrefix + sourcePrefix
	text, truncated, err := boundedInput(prefix, section.Text, maxRunes)
	if err != nil {
		return EmbeddingInput{}, fmt.Errorf("format distilled section %q: %w", section.Key, err)
	}
	return makeInput(
		ordinal, RepresentationKindDistilled, text, section.SourceRefs,
		section.Truncated || contextTruncated || truncated,
	)
}

func makeInput(ordinal int, kind RepresentationKind, text string, refs []SourceRef, truncated bool) (EmbeddingInput, error) {
	input := EmbeddingInput{
		Ordinal: ordinal, Kind: kind, Text: text, SourceRefs: cloneSourceRefs(refs),
		Checksum: fingerprint([]byte(text)), Truncated: truncated,
	}
	keyBytes, err := json.Marshal(embeddingInputIdentity{
		Ordinal: input.Ordinal, Kind: input.Kind, Checksum: input.Checksum,
		Refs: input.SourceRefs, Truncated: input.Truncated,
	})
	if err != nil {
		return EmbeddingInput{}, fmt.Errorf("encode embedding input identity: %w", err)
	}
	input.Key = fingerprint(keyBytes)
	return input, nil
}

func formatContext(context DocumentContext) string {
	var builder strings.Builder
	if context.Filename != "" {
		builder.WriteString("Filename: ")
		builder.WriteString(context.Filename)
		builder.WriteByte('\n')
	}
	if context.Title != "" {
		builder.WriteString("Title: ")
		builder.WriteString(context.Title)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func boundedContext(context DocumentContext, availableRunes int) (string, bool) {
	formatted := formatContext(context)
	if utf8.RuneCountInString(formatted) <= availableRunes {
		return formatted, false
	}
	if availableRunes < 1 {
		return "", formatted != ""
	}

	var builder strings.Builder
	usedRunes := 0
	truncated := false
	for _, field := range [...]struct{ label, value string }{
		{label: "Filename: ", value: context.Filename},
		{label: "Title: ", value: context.Title},
	} {
		if field.value == "" {
			continue
		}
		remaining := availableRunes - usedRunes
		valueLimit := remaining - utf8.RuneCountInString(field.label) - 1
		if valueLimit < 1 {
			truncated = true
			continue
		}
		value := truncateRunes(field.value, valueLimit)
		builder.WriteString(field.label)
		builder.WriteString(value)
		builder.WriteByte('\n')
		usedRunes += utf8.RuneCountInString(field.label) + utf8.RuneCountInString(value) + 1
		truncated = truncated || value != field.value
	}
	return builder.String(), truncated
}

func boundedHeadingContext(path []string, maxHeadingRunes, availableRunes int) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	const headingPrefix = "Heading: "
	const headingSuffix = "\n"
	overhead := utf8.RuneCountInString(headingPrefix + headingSuffix)
	textLimit := min(maxHeadingRunes, availableRunes-overhead)
	if textLimit < 1 {
		return "", true
	}
	heading := strings.Join(path, " > ")
	bounded := truncateRunes(heading, textLimit)
	return headingPrefix + bounded + headingSuffix, bounded != heading
}

func formatLocator(normalized document.NormalizedDocument, spans []document.ChunkSpan) string {
	if len(spans) == 0 {
		return "document"
	}
	first := spans[0].UnitIndex
	last := spans[len(spans)-1].UnitIndex
	kind := normalized.UnitKind
	if kind == "" {
		kind = "unit"
	}
	if first == last {
		return fmt.Sprintf("%s %d", kind, first+1)
	}
	return fmt.Sprintf("%ss %d-%d", kind, first+1, last+1)
}

func formatSourceRefs(refs []SourceRef) string {
	if len(refs) == 0 || len(refs[0].UnitSpans) == 0 {
		return "document"
	}
	first := refs[0].UnitSpans[0].UnitIndex + 1
	lastRef := refs[len(refs)-1]
	last := lastRef.UnitSpans[len(lastRef.UnitSpans)-1].UnitIndex + 1
	if first == last {
		return fmt.Sprintf("unit %d", first)
	}
	return fmt.Sprintf("units %d-%d", first, last)
}

func boundedInput(prefix, body string, limit int) (string, bool, error) {
	prefixRunes := utf8.RuneCountInString(prefix)
	if prefixRunes >= limit {
		return "", false, errors.New("embedding input context consumes the configured rune limit")
	}
	bodyLimit := limit - prefixRunes
	truncated := utf8.RuneCountInString(body) > bodyLimit
	return prefix + truncateRunes(body, bodyLimit), truncated, nil
}

func validateDistillateForPlan(distillate Distillate, sourceChecksum, contextFingerprint string, recipe Recipe) error {
	if distillate.SourceChecksum != sourceChecksum || distillate.ContextFingerprint != contextFingerprint || distillate.RecipeFingerprint != recipe.digest {
		return errors.New("distillate identity differs from embedding plan inputs")
	}
	config := recipe.values.Distillation
	if config == nil || distillate.Provider != config.Provider || distillate.Model != config.Model || distillate.ModelRevision != config.ModelRevision {
		return errors.New("distillate target differs from embedding recipe")
	}
	if distillate.Fingerprint == "" || len(distillate.Sections) == 0 {
		return errors.New("distillate is incomplete")
	}
	for index, section := range distillate.Sections {
		if section.Ordinal != index || section.Key == "" || section.Text == "" || section.Checksum != fingerprint([]byte(section.Text)) {
			return fmt.Errorf("distillate section %d is invalid", index)
		}
		if err := validateSourceRefs(section.SourceRefs); err != nil {
			return fmt.Errorf("distillate section %d: %w", index, err)
		}
		keyBytes, err := json.Marshal(derivedSectionIdentity{
			RequestFingerprint: distillate.RequestFingerprint, Ordinal: section.Ordinal,
			Checksum: section.Checksum, Refs: section.SourceRefs, Truncated: section.Truncated,
		})
		if err != nil {
			return fmt.Errorf("encode derived section identity: %w", err)
		}
		if section.Key != fingerprint(keyBytes) {
			return fmt.Errorf("distillate section %d key is invalid", index)
		}
	}
	expected, err := distillateFingerprint(distillate)
	if err != nil {
		return err
	}
	if expected != distillate.Fingerprint {
		return errors.New("distillate fingerprint is invalid")
	}
	return nil
}

func embeddingPlanFingerprint(plan EmbeddingPlan) (string, error) {
	plan.Fingerprint = ""
	return digestJSON(plan)
}

func validateNormalizedDocument(normalized document.NormalizedDocument) error {
	if err := document.ValidateNormalizedDocument(normalized); err != nil {
		return fmt.Errorf("validate normalized document: %w", err)
	}
	if len(normalized.Chunks) == 0 {
		return errors.New("normalized document has no chunks")
	}
	return nil
}
