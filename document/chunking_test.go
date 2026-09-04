package document

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syntheticTokenizer is a deterministic test tokenizer with a fixed identity
// and a pluggable segmentation.
type syntheticTokenizer struct {
	name      string
	revision  string
	monotonic bool
	tokenize  func(text string, limit int) ([]TokenBoundary, error)
}

func (tokenizer *syntheticTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: tokenizer.name, Revision: tokenizer.revision}
}
func (tokenizer *syntheticTokenizer) PrefixTokenCountsMonotonic() bool { return tokenizer.monotonic }
func (tokenizer *syntheticTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	return tokenizer.tokenize(text, limit)
}

func pairTokenizer() *syntheticTokenizer {
	return &syntheticTokenizer{name: "synthetic-pairs", revision: "v1", monotonic: true, tokenize: pairBoundaries}
}

func pairBoundaries(text string, limit int) ([]TokenBoundary, error) {
	runes := []rune(text)
	result := make([]TokenBoundary, 0, min((len(runes)+1)/2, limit))
	for start := 0; start < len(runes); start += 2 {
		if len(result) == limit {
			return nil, ErrTokenizerLimit
		}
		result = append(result, TokenBoundary{Start: start, End: min(start+2, len(runes))})
	}
	return result, nil
}

func runeBoundaries(text string, limit int) ([]TokenBoundary, error) {
	runes := []rune(text)
	if len(runes) > limit {
		return nil, ErrTokenizerLimit
	}
	result := make([]TokenBoundary, len(runes))
	for index := range runes {
		result[index] = TokenBoundary{Start: index, End: index + 1}
	}
	return result, nil
}

func runeTokenizer(name string, monotonic bool) *syntheticTokenizer {
	return &syntheticTokenizer{name: name, revision: "v1", monotonic: monotonic, tokenize: runeBoundaries}
}

// fixedBoundaries returns a tokenizer that emits fixed boundaries for listed
// texts and rune boundaries otherwise.
func fixedBoundaries(name string, fixed map[string][]TokenBoundary) *syntheticTokenizer {
	return &syntheticTokenizer{name: name, revision: "v1", tokenize: func(text string, limit int) ([]TokenBoundary, error) {
		result, ok := fixed[text]
		if !ok {
			return runeBoundaries(text, limit)
		}
		if len(result) > limit {
			return nil, ErrTokenizerLimit
		}
		return result, nil
	}}
}

type identityDriftTokenizer struct{ calls int }

func (tokenizer *identityDriftTokenizer) Identity() TokenizerIdentity {
	tokenizer.calls++
	if tokenizer.calls == 1 {
		return TokenizerIdentity{Name: "synthetic-pairs", Revision: "v1"}
	}
	return TokenizerIdentity{Name: "synthetic-pairs", Revision: fmt.Sprintf("v%d", tokenizer.calls)}
}
func (*identityDriftTokenizer) PrefixTokenCountsMonotonic() bool { return true }
func (*identityDriftTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	return pairBoundaries(text, limit)
}

type nilTokenizer struct{}

func (*nilTokenizer) Identity() TokenizerIdentity { panic("typed nil tokenizer must not be called") }
func (*nilTokenizer) PrefixTokenCountsMonotonic() bool {
	panic("typed nil tokenizer must not be called")
}
func (*nilTokenizer) Tokenize(string, int) ([]TokenBoundary, error) {
	panic("typed nil tokenizer must not be called")
}

func testModelInput(t *testing.T) ModelInputContract {
	t.Helper()
	contract, err := NewModelInputContract(ModelInputContractConfig{
		Profile: ModelInputProfileCustom, CompatibilityID: "synthetic-space",
		Document: ModelInputEncoder{Mode: ModelInputModeText, Template: "document: {{content}}"},
		Query:    ModelInputEncoder{Mode: ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	return contract
}

func testInputPolicy(t *testing.T, budget, overlap int) InputPolicy {
	t.Helper()
	policy := InputPolicy{
		Chunk: EmbeddingChunkPolicyV1{
			ContextFingerprint: strings.Repeat("b", 64), Formatter: "evidence-text/v1",
			MaxTokens: budget, OverlapTokens: overlap, TruncationPolicy: TruncationPolicyReject,
		},
		ModelInput: testModelInput(t), LexicalEvidenceFingerprint: strings.Repeat("a", 64),
		MaxInputTokens: 256, MaxInputBytes: 4096,
	}
	useTokenizer(&policy, pairTokenizer())
	return policy
}

// useTokenizer installs a tokenizer and declares its identity in the chunk
// policy, the way a resolved binding would.
func useTokenizer(policy *InputPolicy, tokenizer Tokenizer) {
	policy.Tokenizer = tokenizer
	identity := tokenizer.Identity()
	policy.Chunk.Tokenizer, policy.Chunk.TokenizerRevision = identity.Name, identity.Revision
}

func testGenerationLimits() GenerationLimits {
	return GenerationLimits{
		MaxInputs: 128, MaxTotalContentTokens: 4096, MaxTotalRenderedTokens: 8192,
		MaxTotalContentBytes: 1 << 20, MaxTotalRenderedBytes: 2 << 20,
		MaxFittingWorkTokens: 1 << 20, MaxFittingWorkBytes: 8 << 20,
	}
}

func testGenerationDecodeBounds() EmbeddingInputGenerationDecodeBounds {
	return EmbeddingInputGenerationDecodeBounds{MaxEncodedBytes: 1 << 20, MaxInputs: 128}
}

func build(t *testing.T, evidence NormalizedEvidenceV1, policy InputPolicy) EmbeddingInputGeneration {
	t.Helper()
	generation, err := BuildEmbeddingInputs(evidence, policy, testGenerationLimits())
	require.NoError(t, err)
	return generation
}

func contents(generation EmbeddingInputGeneration) []string {
	result := make([]string, len(generation.Inputs))
	for index, input := range generation.Inputs {
		result[index] = input.Content
	}
	return result
}

func TestBuildEmbeddingInputsUsesNaturalBoundariesAndExactOverlap(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{
		{text: "AABBCCDDEEFF", heading: []string{"First"}, regions: []SourceEvidenceRegionV1{{ProviderID: "p1", Kind: EvidenceRegionParagraph, Order: 0, TextRange: EvidenceTextRangeV1{Start: 0, End: 12}}}},
		{text: "GGHH", heading: []string{"Second"}, regions: []SourceEvidenceRegionV1{{ProviderID: "p2", Kind: EvidenceRegionParagraph, Order: 0, TextRange: EvidenceTextRangeV1{Start: 0, End: 4}}}},
	})
	generation := build(t, evidence, testInputPolicy(t, 4, 2))
	require.Len(t, generation.Inputs, 3)

	assert.Equal(t, []string{"AABBCCDD", "CCDDEEFF", "GGHH"}, contents(generation))
	assert.Equal(t, []int{4, 4, 2}, []int{
		generation.Inputs[0].ContentTokens, generation.Inputs[1].ContentTokens, generation.Inputs[2].ContentTokens,
	})
	assert.Equal(t, []string{"First"}, generation.Inputs[0].HeadingPath)
	assert.Equal(t, []string{"First"}, generation.Inputs[1].HeadingPath)
	assert.Equal(t, ChunkSpan{UnitIndex: 0, CharStart: 4, CharEnd: 12}, generation.Inputs[1].SourceSpan)
	assert.Equal(t, generation.Inputs[0].Content[len(generation.Inputs[0].Content)-4:], generation.Inputs[1].Content[:4])
	assertGenerationSpans(t, evidence, generation)
}

func TestBuildEmbeddingInputsNeverCombinesNaturalUnitsBeforeExactTokenization(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "A"}, {text: "B"}})
	policy := testInputPolicy(t, 1, 0)
	useTokenizer(&policy, fixedBoundaries("concat-adversarial", map[string][]TokenBoundary{"AB": {{Start: 0, End: 2}}}))
	generation := build(t, evidence, policy)
	require.Len(t, generation.Inputs, 2)
	assert.Equal(t, []string{"A", "B"}, contents(generation))
	assert.Equal(t, 1, generation.Inputs[0].ContentTokens)
	assert.Equal(t, 1, generation.Inputs[1].ContentTokens)
	assert.Equal(t, ChunkSpan{UnitIndex: 0, CharStart: 0, CharEnd: 1}, generation.Inputs[0].SourceSpan)
	assert.Equal(t, ChunkSpan{UnitIndex: 1, CharStart: 0, CharEnd: 1}, generation.Inputs[1].SourceSpan)
}

func TestBuildEmbeddingInputsDerivesOverlapFromExactEmittedTokenization(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcdef"}})
	policy := testInputPolicy(t, 2, 1)
	useTokenizer(&policy, fixedBoundaries("overlap-adversarial", map[string][]TokenBoundary{
		"abcdef": {{Start: 0, End: 2}, {Start: 2, End: 4}, {Start: 4, End: 6}},
		"abcd":   {{Start: 0, End: 1}, {Start: 1, End: 4}},
		"bcd":    {{Start: 0, End: 1}, {Start: 1, End: 3}},
		"cdef":   {{Start: 0, End: 2}, {Start: 2, End: 4}},
	}))
	generation := build(t, evidence, policy)
	require.Len(t, generation.Inputs, 3)
	assert.Equal(t, []string{"abcd", "bcd", "cdef"}, contents(generation))
	for _, input := range generation.Inputs {
		assert.Equal(t, 2, input.ContentTokens)
	}
	assert.Equal(t, "bcd", generation.Inputs[1].Content[:3], "the second input starts with the exact final token of the first")
	assert.Equal(t, "cd", generation.Inputs[2].Content[:2], "the third input starts with the exact final token of the second")
	assertGenerationSpans(t, evidence, generation)
}

func TestBuildEmbeddingInputsPrefersRegionAndTableAtomsBeforeTokenSplits(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{
		text: "AAAABBBBCCCC", heading: []string{"Structured"},
		regions: []SourceEvidenceRegionV1{
			{ProviderID: "heading", Kind: EvidenceRegionHeading, Order: 0, TextRange: EvidenceTextRangeV1{Start: 0, End: 4}},
			{ProviderID: "table", Kind: EvidenceRegionTable, Order: 1, TextRange: EvidenceTextRangeV1{Start: 4, End: 8}},
			{ProviderID: "paragraph", Kind: EvidenceRegionParagraph, Order: 2, TextRange: EvidenceTextRangeV1{Start: 8, End: 12}},
		},
		tables: []SourceEvidenceTableV1{{
			ProviderID: "t1", RegionProviderID: "table", Order: 0, Rows: 1, Columns: 1,
			Cells: []SourceEvidenceTableCellV1{{Order: 0, Row: 0, Column: 0, RowSpan: 1, ColumnSpan: 1, TextRange: EvidenceTextRangeV1{Start: 4, End: 8}}},
		}},
	}})
	generation := build(t, evidence, testInputPolicy(t, 3, 0))
	assert.Equal(t, []string{"AAAA", "BBBB", "CCCC"}, contents(generation))
	assertGenerationSpans(t, evidence, generation)
}

func TestBuildEmbeddingInputsKeepsContentBudgetSeparateFromAttachmentAndEnvelope(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABBCC", heading: []string{"Evidence"}}})
	attachment, err := NewAttachmentContextSnapshot("Human title", "Human context")
	require.NoError(t, err)
	policy := testInputPolicy(t, 3, 0)
	policy.AttachmentContext = &attachment
	generation := build(t, evidence, policy)
	require.Len(t, generation.Inputs, 1)

	input := generation.Inputs[0]
	assert.Equal(t, "AABBCC", input.Content)
	assert.Equal(t, 3, input.ContentTokens)
	assert.Greater(t, input.RenderedTokens, input.ContentTokens)
	assert.Equal(t, "document: Title: Human title\nContext: Human context\n\nAABBCC", input.Rendered)
	require.NotNil(t, generation.AttachmentContext)
	assert.Equal(t, attachment, *generation.AttachmentContext)
}

func TestBuildEmbeddingInputsTruncatesOnlyAnIndivisibleTokenWhenDeclared(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcdefghij"}})
	policy := testInputPolicy(t, 1, 0)
	policy.MaxInputBytes = 15
	policy.Chunk.TruncationPolicy = TruncationPolicyTruncateIndivisible
	generation := build(t, evidence, policy)
	require.Len(t, generation.Inputs, 5)
	assert.Equal(t, "ab", generation.Inputs[0].Content)
	assert.False(t, generation.Inputs[0].Truncated)

	policy.MaxInputBytes = 11
	generation = build(t, evidence, policy)
	assert.Equal(t, "a", generation.Inputs[0].Content)
	assert.Equal(t, ChunkSpan{UnitIndex: 0, CharStart: 0, CharEnd: 1}, generation.Inputs[0].SourceSpan)
	assert.True(t, generation.Inputs[0].Truncated)

	policy.Chunk.TruncationPolicy = TruncationPolicyReject
	_, err := BuildEmbeddingInputs(evidence, policy, testGenerationLimits())
	require.ErrorContains(t, err, "indivisible token")
}

func TestBuildEmbeddingInputsRetokenizesExactTruncatedContent(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcd"}})
	policy := testInputPolicy(t, 1, 0)
	useTokenizer(&policy, fixedBoundaries("truncation-adversarial", map[string][]TokenBoundary{"abcd": {{Start: 0, End: 4}}}))
	policy.MaxInputBytes = 13
	policy.Chunk.TruncationPolicy = TruncationPolicyTruncateIndivisible
	generation := build(t, evidence, policy)
	require.Len(t, generation.Inputs, 1)
	assert.Equal(t, "a", generation.Inputs[0].Content)
	assert.Equal(t, 1, generation.Inputs[0].ContentTokens)
	assert.Equal(t, ChunkSpan{UnitIndex: 0, CharStart: 0, CharEnd: 1}, generation.Inputs[0].SourceSpan)
	assert.True(t, generation.Inputs[0].Truncated)
}

func TestBuildEmbeddingInputsSkipsEmptyNaturalUnits(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AA"}, {text: ""}, {text: "BB"}})
	generation := build(t, evidence, testInputPolicy(t, 2, 0))
	require.Len(t, generation.Inputs, 2)
	assert.Equal(t, ChunkSpan{UnitIndex: 0, CharStart: 0, CharEnd: 2}, generation.Inputs[0].SourceSpan)
	assert.Equal(t, ChunkSpan{UnitIndex: 2, CharStart: 0, CharEnd: 2}, generation.Inputs[1].SourceSpan)

	allEmpty := testChunkEvidence(t, []sourceChunkUnit{{text: ""}, {text: ""}})
	emptyGeneration := build(t, allEmpty, testInputPolicy(t, 2, 0))
	assert.Empty(t, emptyGeneration.Inputs)
	assert.NotEmpty(t, emptyGeneration.Checksum)
}

func TestBuildEmbeddingInputsSealsIdentityInputsButNotGenerationLimits(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB", heading: []string{"Heading"}}})
	basePolicy := testInputPolicy(t, 2, 0)
	base := build(t, evidence, basePolicy)
	assert.Equal(t, base, build(t, evidence, basePolicy))

	attachment, err := NewAttachmentContextSnapshot("Title", "")
	require.NoError(t, err)
	mutations := map[string]func(*InputPolicy){
		"formatter": func(policy *InputPolicy) { policy.Chunk.Formatter = "evidence-text/v2" },
		"tokenizer name": func(policy *InputPolicy) {
			useTokenizer(policy, &syntheticTokenizer{name: "other", revision: "v1", monotonic: true, tokenize: pairBoundaries})
		},
		"tokenizer revision": func(policy *InputPolicy) {
			useTokenizer(policy, &syntheticTokenizer{name: "synthetic-pairs", revision: "v2", monotonic: true, tokenize: pairBoundaries})
		},
		"content budget":     func(policy *InputPolicy) { policy.Chunk.MaxTokens++ },
		"overlap":            func(policy *InputPolicy) { policy.Chunk.OverlapTokens = 1 },
		"truncation policy":  func(policy *InputPolicy) { policy.Chunk.TruncationPolicy = TruncationPolicyTruncateIndivisible },
		"context rules":      func(policy *InputPolicy) { policy.Chunk.ContextFingerprint = strings.Repeat("c", 64) },
		"lexical evidence":   func(policy *InputPolicy) { policy.LexicalEvidenceFingerprint = strings.Repeat("d", 64) },
		"provider tokens":    func(policy *InputPolicy) { policy.MaxInputTokens++ },
		"provider bytes":     func(policy *InputPolicy) { policy.MaxInputBytes++ },
		"attachment context": func(policy *InputPolicy) { policy.AttachmentContext = &attachment },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			policy := basePolicy
			mutate(&policy)
			changed := build(t, evidence, policy)
			assert.NotEqual(t, base.PolicyFingerprint, changed.PolicyFingerprint)
			assert.NotEqual(t, base.Checksum, changed.Checksum)
		})
	}

	// A tokenizer's search strategy is not identity: an honest tokenizer
	// produces the same inputs under either fitting search.
	nonMonotonic := basePolicy
	useTokenizer(&nonMonotonic, &syntheticTokenizer{name: "synthetic-pairs", revision: "v1", tokenize: pairBoundaries})
	assert.Equal(t, base, build(t, evidence, nonMonotonic))

	limitMutations := map[string]func(*GenerationLimits){
		"max inputs":      func(limits *GenerationLimits) { limits.MaxInputs++ },
		"content tokens":  func(limits *GenerationLimits) { limits.MaxTotalContentTokens++ },
		"rendered tokens": func(limits *GenerationLimits) { limits.MaxTotalRenderedTokens++ },
		"content bytes":   func(limits *GenerationLimits) { limits.MaxTotalContentBytes++ },
		"rendered bytes":  func(limits *GenerationLimits) { limits.MaxTotalRenderedBytes++ },
		"work tokens":     func(limits *GenerationLimits) { limits.MaxFittingWorkTokens++ },
		"work bytes":      func(limits *GenerationLimits) { limits.MaxFittingWorkBytes++ },
	}
	for name, mutate := range limitMutations {
		t.Run("limit "+name, func(t *testing.T) {
			limits := testGenerationLimits()
			mutate(&limits)
			changed, err := BuildEmbeddingInputs(evidence, basePolicy, limits)
			require.NoError(t, err)
			assert.Equal(t, base, changed, "generation limits must not enter the generation identity")
		})
	}

	changedHeading := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB", heading: []string{"Other heading"}}})
	changed := build(t, changedHeading, basePolicy)
	assert.Equal(t, base.PolicyFingerprint, changed.PolicyFingerprint)
	assert.NotEqual(t, base.Checksum, changed.Checksum)
}

func TestNewInputPolicyDerivesFromRenditionChunkBinding(t *testing.T) {
	contract, err := NewModelInputContract(ModelInputContractConfig{Profile: ModelInputProfileE5})
	require.NoError(t, err)
	binding := EmbeddingBindingV1{
		InputKind: EmbeddingInputRenditionChunk, ModelInput: contract, MaxInputTokens: 512, MaxInputBytes: 8192,
		Chunk: &EmbeddingChunkPolicyV1{
			ContextFingerprint: strings.Repeat("b", 64), Formatter: "evidence-text/v1", MaxTokens: 4, OverlapTokens: 1,
			Tokenizer: "synthetic-pairs", TokenizerRevision: "v1", TruncationPolicy: TruncationPolicyReject,
		},
	}
	policy, err := NewInputPolicy(binding, pairTokenizer(), strings.Repeat("a", 64), nil)
	require.NoError(t, err)
	assert.Equal(t, *binding.Chunk, policy.Chunk)
	assert.Equal(t, contract, policy.ModelInput)
	assert.Equal(t, 512, policy.MaxInputTokens)
	assert.Equal(t, int64(8192), policy.MaxInputBytes)

	generation := build(t, testChunkEvidence(t, []sourceChunkUnit{{text: "AABBCC"}}), policy)
	assert.Equal(t, *binding.Chunk, generation.Chunk)
	assert.Equal(t, "passage: AABBCC", generation.Inputs[0].Rendered)

	_, err = NewInputPolicy(binding, runeTokenizer("synthetic-pairs", true), strings.Repeat("a", 64), nil)
	require.NoError(t, err, "identity, not segmentation, is what the binding can check")
	mismatched := binding
	chunk := *binding.Chunk
	chunk.TokenizerRevision = "v2"
	mismatched.Chunk = &chunk
	_, err = NewInputPolicy(mismatched, pairTokenizer(), strings.Repeat("a", 64), nil)
	require.ErrorContains(t, err, "does not match declared chunk policy")

	direct := binding
	direct.InputKind = EmbeddingInputOriginalFile
	direct.Chunk = nil
	_, err = NewInputPolicy(direct, pairTokenizer(), strings.Repeat("a", 64), nil)
	require.ErrorContains(t, err, "rendition_chunk")

	invalid := binding
	invalid.Chunk = &EmbeddingChunkPolicyV1{}
	_, err = NewInputPolicy(invalid, pairTokenizer(), strings.Repeat("a", 64), nil)
	require.ErrorContains(t, err, "chunk max tokens")
}

func TestBuildEmbeddingInputsDoesNotLeakFrontmatterOrProvenanceMetadata(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "Actual evidence", heading: []string{"Visible heading"}}})
	policy := testInputPolicy(t, 100, 0)
	policy.LexicalEvidenceFingerprint = strings.Repeat("d", 64)
	generation := build(t, evidence, policy)
	require.Len(t, generation.Inputs, 1)

	rendered := generation.Inputs[0].Rendered
	assert.Equal(t, "document: Actual evidence", rendered)
	for _, forbidden := range []string{"---", "checksum", evidence.Checksum, evidence.Units[0].ID, policy.LexicalEvidenceFingerprint, policy.Chunk.ContextFingerprint, "Visible heading", "page:", "line:", "byte:"} {
		assert.NotContains(t, rendered, forbidden)
	}
}

func TestBuildEmbeddingInputsRejectsUnboundedOrNoncanonicalTokenizerOutput(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}})
	broken := func(tokens []TokenBoundary, err error) *syntheticTokenizer {
		return &syntheticTokenizer{name: "broken", revision: "v1", tokenize: func(string, int) ([]TokenBoundary, error) { return tokens, err }}
	}
	for _, testCase := range []struct {
		name      string
		tokenizer Tokenizer
		want      string
	}{
		{"gap", broken([]TokenBoundary{{Start: 0, End: 1}, {Start: 2, End: 4}}, nil), "contiguous"},
		{"outside", broken([]TokenBoundary{{Start: 0, End: 5}}, nil), "bounds"},
		{"empty", broken(nil, nil), "at least one"},
		{"too many", broken(nil, ErrTokenizerLimit), "token limit"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			policy := testInputPolicy(t, 2, 0)
			useTokenizer(&policy, testCase.tokenizer)
			_, err := BuildEmbeddingInputs(evidence, policy, testGenerationLimits())
			require.ErrorContains(t, err, testCase.want)
		})
	}
	limits := testGenerationLimits()
	limits.MaxInputs = 1
	_, err := BuildEmbeddingInputs(testChunkEvidence(t, []sourceChunkUnit{{text: "AABBCCDD"}}), testInputPolicy(t, 2, 0), limits)
	require.ErrorContains(t, err, "generated input limit")
}

func TestBuildEmbeddingInputsBoundsAggregateAmplificationBeforeAppend(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABBCCDDEEFF"}})
	policy := testInputPolicy(t, 4, 3)
	limits := testGenerationLimits()
	limits.MaxTotalContentBytes = 12
	limits.MaxTotalRenderedBytes = 44
	limits.MaxTotalContentTokens = 6
	limits.MaxTotalRenderedTokens = 40
	_, err := BuildEmbeddingInputs(evidence, policy, limits)
	require.ErrorContains(t, err, "aggregate")

	limits.MaxTotalContentTokens = 100
	limits.MaxTotalRenderedTokens = 100
	limits.MaxTotalContentBytes = 100
	limits.MaxTotalRenderedBytes = 200
	generation, err := BuildEmbeddingInputs(evidence, policy, limits)
	require.NoError(t, err)
	var contentTokens, renderedTokens, contentBytes, renderedBytes int64
	for _, input := range generation.Inputs {
		contentTokens += int64(input.ContentTokens)
		renderedTokens += int64(input.RenderedTokens)
		contentBytes += int64(len(input.Content))
		renderedBytes += int64(len(input.Rendered))
	}
	assert.Equal(t, contentTokens, generation.TotalContentTokens)
	assert.Equal(t, renderedTokens, generation.TotalRenderedTokens)
	assert.Equal(t, contentBytes, generation.TotalContentBytes)
	assert.Equal(t, renderedBytes, generation.TotalRenderedBytes)
}

func TestBuildEmbeddingInputsRejectsTypedNilAndTokenizerIdentityDrift(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AA"}})
	policy := testInputPolicy(t, 2, 0)
	var typedNil *nilTokenizer
	policy.Tokenizer = typedNil
	_, err := BuildEmbeddingInputs(evidence, policy, testGenerationLimits())
	require.ErrorContains(t, err, "requires a tokenizer")

	policy = testInputPolicy(t, 2, 0)
	policy.Tokenizer = &identityDriftTokenizer{}
	_, err = BuildEmbeddingInputs(evidence, policy, testGenerationLimits())
	require.ErrorContains(t, err, "identity changed")

	policy = testInputPolicy(t, 2, 0)
	policy.Chunk.TokenizerRevision = "v2"
	_, err = BuildEmbeddingInputs(evidence, policy, testGenerationLimits())
	require.ErrorContains(t, err, "does not match declared chunk policy")
}

func TestBuildEmbeddingInputsUsesBoundedFittingSearch(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: strings.Repeat("a", 1024)}})
	calls := 0
	tokenizer := &syntheticTokenizer{name: "counting-runes", revision: "v1", monotonic: true, tokenize: func(text string, limit int) ([]TokenBoundary, error) {
		calls++
		return runeBoundaries(text, limit)
	}}
	policy := testInputPolicy(t, 1024, 0)
	useTokenizer(&policy, tokenizer)
	policy.MaxInputBytes = 20
	policy.MaxInputTokens = 2048
	limits := testGenerationLimits()
	limits.MaxTotalContentBytes = 4096
	limits.MaxTotalRenderedBytes = 8192
	limits.MaxTotalContentTokens = 2048
	limits.MaxTotalRenderedTokens = 4096
	generation, err := BuildEmbeddingInputs(evidence, policy, limits)
	require.NoError(t, err)
	require.NotEmpty(t, generation.Inputs)
	assert.LessOrEqual(t, calls, 5000, "fitting must not retry one token at a time")
}

func TestBuildEmbeddingInputsCapsCumulativeNonMonotonicFittingWork(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: strings.Repeat("a", 2048)}})
	exactCalls := 0
	tokenizer := &syntheticTokenizer{name: "fitting-work-adversarial", revision: "v1", tokenize: func(text string, limit int) ([]TokenBoundary, error) {
		if limit == maxEmbeddingTokensPerGeneration {
			return runeBoundaries(text, limit)
		}
		exactCalls++
		return nil, ErrTokenizerLimit
	}}
	policy := testInputPolicy(t, 2048, 0)
	useTokenizer(&policy, tokenizer)
	policy.MaxInputBytes = 8192
	policy.MaxInputTokens = 4096
	limits := testGenerationLimits()
	limits.MaxFittingWorkBytes = 20_000
	limits.MaxFittingWorkTokens = 100_000
	_, err := BuildEmbeddingInputs(evidence, policy, limits)
	require.ErrorContains(t, err, "fitting work")
	assert.LessOrEqual(t, exactCalls, 3, "the work budget must be consumed before reconstruction and tokenization")
}

func TestBuildEmbeddingInputsChargesByteFittingProbes(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcd"}})
	policy := testInputPolicy(t, 1, 0)
	useTokenizer(&policy, fixedBoundaries("truncation-adversarial", map[string][]TokenBoundary{"abcd": {{Start: 0, End: 4}}}))
	policy.MaxInputBytes = 11
	policy.Chunk.TruncationPolicy = TruncationPolicyTruncateIndivisible
	limits := testGenerationLimits()
	limits.MaxFittingWorkBytes = 13
	limits.MaxFittingWorkTokens = 13
	_, err := BuildEmbeddingInputs(evidence, policy, limits)
	require.ErrorContains(t, err, "fitting work")
}

func TestBuildEmbeddingInputsDoesNotAssumePrefixTokenCountsAreMonotonic(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcd"}})
	policy := testInputPolicy(t, 4, 0)
	useTokenizer(&policy, &syntheticTokenizer{name: "nonmonotonic-prefix", revision: "v1", tokenize: func(text string, limit int) ([]TokenBoundary, error) {
		if limit <= 4 && !strings.HasPrefix(text, "document: ") && (text == "abcd" || text == "ab") {
			return nil, ErrTokenizerLimit
		}
		return runeBoundaries(text, limit)
	}})
	generation := build(t, evidence, policy)
	require.NotEmpty(t, generation.Inputs)
	assert.Equal(t, "abc", generation.Inputs[0].Content, "the longer fitting prefix must not be discarded after the middle prefix exceeds the token limit")
}

func TestBuildEmbeddingInputsDoesNotBinarySearchTruncationWithTemplateSuffix(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcdef"}})
	contract, err := NewModelInputContract(ModelInputContractConfig{
		Profile: ModelInputProfileCustom, CompatibilityID: "suffix-space",
		Document: ModelInputEncoder{Mode: ModelInputModeText, Template: "{{content}} suffix"},
		Query:    ModelInputEncoder{Mode: ModelInputModeText, Template: "{{content}} suffix"},
	})
	require.NoError(t, err)
	policy := testInputPolicy(t, 1, 0)
	useTokenizer(&policy, &syntheticTokenizer{name: "suffix-truncation-adversarial", revision: "v1", monotonic: true, tokenize: func(text string, limit int) ([]TokenBoundary, error) {
		whole := []TokenBoundary{{Start: 0, End: utf8.RuneCountInString(text)}}
		if limit > 1 || !strings.HasSuffix(text, " suffix") || text == "a suffix" || text == "abcd suffix" {
			return whole, nil
		}
		return nil, ErrTokenizerLimit
	}})
	policy.ModelInput = contract
	policy.MaxInputTokens = 1
	policy.Chunk.TruncationPolicy = TruncationPolicyTruncateIndivisible
	generation := build(t, evidence, policy)
	require.Len(t, generation.Inputs, 1)
	assert.Equal(t, "abcd", generation.Inputs[0].Content, "a longer fitting truncation must survive a failing middle prefix")
	assert.True(t, generation.Inputs[0].Truncated)
}

func TestBuildEmbeddingInputsPreservesNaturalCutWhenProviderLimitShrinksChunk(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{
		{text: "AAAABBBBCCCC", regions: []SourceEvidenceRegionV1{
			{ProviderID: "first", Kind: EvidenceRegionParagraph, Order: 0, TextRange: EvidenceTextRangeV1{Start: 0, End: 4}},
			{ProviderID: "second", Kind: EvidenceRegionParagraph, Order: 1, TextRange: EvidenceTextRangeV1{Start: 4, End: 8}},
			{ProviderID: "third", Kind: EvidenceRegionParagraph, Order: 2, TextRange: EvidenceTextRangeV1{Start: 8, End: 12}},
		}},
	})
	policy := testInputPolicy(t, 6, 0)
	policy.MaxInputBytes = int64(len("document: ") + 7)
	generation := build(t, evidence, policy)
	require.NotEmpty(t, generation.Inputs)
	assert.Equal(t, "AAAA", generation.Inputs[0].Content)
}

func TestBuildEmbeddingInputsFailsClosedWhenOverlapCannotBePreserved(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABBCCDDEEFF"}})
	policy := testInputPolicy(t, 4, 3)
	policy.MaxInputBytes = int64(len("document: ") + 6)
	_, err := BuildEmbeddingInputs(evidence, policy, testGenerationLimits())
	require.ErrorContains(t, err, "provider limits cannot preserve configured token overlap")
}

func TestBuildEmbeddingInputsAggregateLimitsRejectButNeverShapeInputs(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABBCCDDEE"}})
	policy := testInputPolicy(t, 2, 0)
	policy.Chunk.TruncationPolicy = TruncationPolicyTruncateIndivisible
	unlimited := build(t, evidence, policy)
	require.Len(t, unlimited.Inputs, 3)
	assert.Equal(t, "EE", unlimited.Inputs[2].Content)

	exact := testGenerationLimits()
	exact.MaxInputs = len(unlimited.Inputs)
	exact.MaxTotalContentTokens = unlimited.TotalContentTokens
	exact.MaxTotalRenderedTokens = unlimited.TotalRenderedTokens
	exact.MaxTotalContentBytes = unlimited.TotalContentBytes
	exact.MaxTotalRenderedBytes = unlimited.TotalRenderedBytes
	generation, err := BuildEmbeddingInputs(evidence, policy, exact)
	require.NoError(t, err)
	assert.Equal(t, unlimited, generation, "limits that the generation fits inside must not change it")

	for name, mutate := range map[string]func(*GenerationLimits){
		"content bytes":   func(limits *GenerationLimits) { limits.MaxTotalContentBytes-- },
		"content tokens":  func(limits *GenerationLimits) { limits.MaxTotalContentTokens-- },
		"rendered bytes":  func(limits *GenerationLimits) { limits.MaxTotalRenderedBytes-- },
		"rendered tokens": func(limits *GenerationLimits) { limits.MaxTotalRenderedTokens-- },
	} {
		t.Run(name, func(t *testing.T) {
			limits := exact
			mutate(&limits)
			_, err := BuildEmbeddingInputs(evidence, policy, limits)
			require.ErrorContains(t, err, "aggregate limits", "the last input must be rejected, never truncated to the remaining budget")
		})
	}
}

func TestEmbeddingInputGenerationRoundTripsCanonicalBytesAndRejectsForgery(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB", heading: []string{"Evidence"}}})
	attachment, err := NewAttachmentContextSnapshot("Human title", "Human context")
	require.NoError(t, err)
	policy := testInputPolicy(t, 2, 0)
	policy.AttachmentContext = &attachment
	generation := build(t, evidence, policy)
	encoded, err := MarshalEmbeddingInputGeneration(generation)
	require.NoError(t, err)
	decoded, err := DecodeEmbeddingInputGeneration(encoded, testGenerationDecodeBounds())
	require.NoError(t, err)
	assert.Equal(t, generation, decoded)
	reencoded, err := MarshalEmbeddingInputGeneration(decoded)
	require.NoError(t, err)
	assert.Equal(t, encoded, reencoded)

	indented, err := json.MarshalIndent(generation, "", "  ")
	require.NoError(t, err)
	_, err = DecodeEmbeddingInputGeneration(indented, testGenerationDecodeBounds())
	require.ErrorContains(t, err, "canonical")

	for _, testCase := range []struct{ name, value, want string }{
		{"unknown generation field", strings.Replace(string(encoded), `{"attachment_context"`, `{"aaa":1,"attachment_context"`, 1), "aaa"},
		{"forged checksum", strings.Replace(string(encoded), generation.Checksum, strings.Repeat("0", 64), 1), "checksum"},
		{"forged policy fingerprint", strings.Replace(string(encoded), generation.PolicyFingerprint, strings.Repeat("0", 64), 1), "policy fingerprint"},
		{"forged total", strings.Replace(string(encoded), `"total_content_tokens":2`, `"total_content_tokens":3`, 1), "aggregate totals"},
		{"empty attachment", strings.Replace(string(encoded), `"attachment_context":{"context":"Human context","title":"Human title"}`, `"attachment_context":{}`, 1), "canonical"},
		{"negative span", strings.Replace(string(encoded), `"unit_index":0`, `"unit_index":-1`, 1), "source span"},
		{"tokens above sealed budget", strings.Replace(string(encoded), `"content_tokens":2`, `"content_tokens":3`, 1), "sealed limits"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			require.NotEqual(t, string(encoded), testCase.value, "the tamper must apply")
			_, err := DecodeEmbeddingInputGeneration([]byte(testCase.value), testGenerationDecodeBounds())
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestEmbeddingInputGenerationDecodeAppliesCallerBounds(t *testing.T) {
	generation := build(t, testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}}), testInputPolicy(t, 1, 0))
	encoded, err := MarshalEmbeddingInputGeneration(generation)
	require.NoError(t, err)
	require.Greater(t, len(generation.Inputs), 1)

	bounds := testGenerationDecodeBounds()
	bounds.MaxInputs = len(generation.Inputs) - 1
	_, err = DecodeEmbeddingInputGeneration(encoded, bounds)
	require.ErrorContains(t, err, "input count exceeds bounds")

	bounds = testGenerationDecodeBounds()
	bounds.MaxEncodedBytes = int64(len(encoded) - 1)
	_, err = DecodeEmbeddingInputGeneration(encoded, bounds)
	require.ErrorContains(t, err, "encoded bytes exceed bounds")

	_, err = DecodeEmbeddingInputGeneration(encoded, EmbeddingInputGenerationDecodeBounds{MaxEncodedBytes: 1 << 20})
	require.ErrorContains(t, err, "decode bound is invalid")
}

func TestEmbeddingInputGenerationMapsContextIntoE1ExactlyOnce(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB", heading: []string{"Evidence"}}})
	attachment, err := NewAttachmentContextSnapshot("Human title", "Human context")
	require.NoError(t, err)
	policy := testInputPolicy(t, 2, 0)
	policy.AttachmentContext = &attachment
	generation := build(t, evidence, policy)
	generated := generation.Inputs[0]
	inputs, err := generation.ToEmbeddingInputs(policy.ModelInput)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	input := inputs[0]
	assert.Contains(t, input.Text, "Human title")
	assert.Contains(t, input.Text, "Human context")
	assert.NotEqual(t, generated.Rendered, input.Text)
	assert.Equal(t, generated.Rendered, policy.ModelInput.EncodeDocument(input.Text))
	assert.Equal(t, []ChunkSpan{generated.SourceSpan}, input.SourceSpans)
	assert.Equal(t, generated.HeadingPath, input.HeadingPath)

	descriptor, err := NewEmbeddingDescriptor(EmbeddingDescriptor{
		ID: "synthetic-embedder", ContractVersion: EmbeddingProviderContractVersion,
		PolicyFingerprint: strings.Repeat("b", 64), TrustBoundary: EmbeddingTrustLocalProcess,
		Model: "synthetic-model", ModelRevision: "r1", Dimension: 2, Metric: VectorMetricCosine,
		InputKinds: []EmbeddingInputKind{EmbeddingInputRenditionChunk}, CompatibilityID: policy.ModelInput.CompatibilityID,
		ModelInput: policy.ModelInput, SupportedRequestModes: []ModelInputMode{ModelInputModeText},
		DocumentFormatter: "document/v1", QueryFormatter: "query/v1", Normalization: VectorNormalizationUnitLength,
		ScalarEncoding: "float32",
	})
	require.NoError(t, err)
	authorization := EmbeddingAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 1,
		MaxInputBytes: int64(len(generated.Rendered)), MaxResponseBytes: 1024,
	}
	require.NoError(t, ValidateEmbeddingProviderRequest(chunkingTestProvider{descriptor: descriptor}, []EmbeddingInput{input}, authorization))
	authorization.MaxInputBytes--
	require.ErrorContains(t, ValidateEmbeddingProviderRequest(chunkingTestProvider{descriptor: descriptor}, []EmbeddingInput{input}, authorization), "input bytes")
}

func TestEmbeddingInputGenerationRejectsEqualEnvelopeWithDifferentModelInputFingerprint(t *testing.T) {
	bge, err := NewModelInputContract(ModelInputContractConfig{Profile: ModelInputProfileBGEM3})
	require.NoError(t, err)
	gte, err := NewModelInputContract(ModelInputContractConfig{Profile: ModelInputProfileGTE})
	require.NoError(t, err)
	assert.Equal(t, bge.Document, gte.Document, "the regression requires equal document envelopes")
	assert.NotEqual(t, bge.Fingerprint, gte.Fingerprint)

	policy := testInputPolicy(t, 2, 0)
	policy.ModelInput = bge
	generation := build(t, testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}}), policy)
	_, err = generation.ToEmbeddingInputs(gte)
	require.ErrorContains(t, err, "fingerprint")
}

type chunkingTestProvider struct{ descriptor EmbeddingDescriptor }

func (provider chunkingTestProvider) Descriptor() EmbeddingDescriptor { return provider.descriptor }
func (chunkingTestProvider) Embed(context.Context, []EmbeddingInput, EmbeddingAuthorization) (EmbeddingResult, error) {
	return EmbeddingResult{}, nil
}

func TestBuildEmbeddingInputsGoldenProfiles(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{
		{text: "Alpha beta.", heading: []string{"Intro"}},
		{text: "Gamma delta.", heading: []string{"Details"}},
	})
	profiles := []ModelInputContractConfig{
		{Profile: ModelInputProfileNomic},
		{Profile: ModelInputProfileE5},
		{Profile: ModelInputProfileBGEM3},
		{Profile: ModelInputProfileGTE},
		{Profile: ModelInputProfileQwen3, QueryInstruction: "Retrieve supporting passages"},
	}
	for _, config := range profiles {
		t.Run(string(config.Profile), func(t *testing.T) {
			contract, err := NewModelInputContract(config)
			require.NoError(t, err)
			policy := testInputPolicy(t, 6, 1)
			policy.ModelInput = contract
			generation := build(t, evidence, policy)
			encoded, err := MarshalEmbeddingInputGeneration(generation)
			require.NoError(t, err)
			encoded = append(encoded, '\n')
			goldenPath := "testdata/chunks-" + strings.ReplaceAll(string(config.Profile), "/", "-") + ".golden.json"
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				require.NoError(t, os.WriteFile(goldenPath, encoded, 0o644))
			}
			golden, err := os.ReadFile(goldenPath)
			require.NoError(t, err)
			assert.Equal(t, string(golden), string(encoded))
		})
	}
}

func FuzzEmbeddingSpans(f *testing.F) {
	f.Add("alpha beta", uint8(4), uint8(1))
	f.Add("éclair 世界", uint8(3), uint8(0))
	f.Fuzz(func(t *testing.T, text string, budgetByte, overlapByte uint8) {
		if text == "" || !utf8.ValidString(text) || strings.ContainsAny(text, "\x00\r") || len([]rune(text)) > 256 {
			t.Skip()
		}
		evidence := testChunkEvidence(t, []sourceChunkUnit{{text: text, heading: []string{"Synthetic"}}})
		budget := max(1, int(budgetByte%32))
		overlap := int(overlapByte) % budget
		policy := testInputPolicy(t, budget, overlap)
		limits := testGenerationLimits()
		limits.MaxInputs = 512
		limits.MaxTotalContentTokens = 512 * 32
		limits.MaxTotalRenderedTokens = 512 * int64(policy.MaxInputTokens)
		limits.MaxTotalContentBytes = 512 * policy.MaxInputBytes
		limits.MaxTotalRenderedBytes = 512 * policy.MaxInputBytes
		generation, err := BuildEmbeddingInputs(evidence, policy, limits)
		require.NoError(t, err)
		assertGenerationSpans(t, evidence, generation)
		for index, input := range generation.Inputs {
			assert.LessOrEqual(t, input.ContentTokens, budget)
			assert.LessOrEqual(t, input.RenderedTokens, policy.MaxInputTokens)
			assert.LessOrEqual(t, int64(len(input.Rendered)), policy.MaxInputBytes)
			exactTokens, err := policy.Tokenizer.Tokenize(input.Content, budget)
			require.NoError(t, err)
			require.NoError(t, validateTokenBoundaries(exactTokens, utf8.RuneCountInString(input.Content), budget))
			assert.Equal(t, len(exactTokens), input.ContentTokens)
			if index == 0 || overlap == 0 || generation.Inputs[index-1].Truncated || generation.Inputs[index-1].SourceSpan.UnitIndex != input.SourceSpan.UnitIndex {
				continue
			}
			previous := generation.Inputs[index-1]
			previousTokens, err := policy.Tokenizer.Tokenize(previous.Content, budget)
			require.NoError(t, err)
			overlapRunes := []rune(previous.Content)[previousTokens[len(previousTokens)-overlap].Start:]
			assert.True(t, strings.HasPrefix(input.Content, string(overlapRunes)))
		}
	})
}

type sourceChunkUnit struct {
	text    string
	heading []string
	regions []SourceEvidenceRegionV1
	tables  []SourceEvidenceTableV1
}

func testChunkEvidence(t *testing.T, units []sourceChunkUnit) NormalizedEvidenceV1 {
	t.Helper()
	sourceUnits := make([]SourceEvidenceUnitV1, len(units))
	for index, unit := range units {
		sourceUnits[index] = SourceEvidenceUnitV1{
			Order: index, HeadingPath: unit.heading, Text: unit.text, Regions: unit.regions, Tables: unit.tables,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne, Start: int64(index + 1), End: int64(index + 1)},
		}
	}
	source := SourceEvidenceV1{ContractVersion: SourceEvidenceContractV1, Completeness: EvidenceComplete, Family: "pdf", UnitKind: EvidenceUnitPage, Units: sourceUnits}
	policy, err := NewEvidencePolicy(4096)
	require.NoError(t, err)
	evidence, err := NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	return evidence
}

func assertGenerationSpans(t *testing.T, evidence NormalizedEvidenceV1, generation EmbeddingInputGeneration) {
	t.Helper()
	for _, input := range generation.Inputs {
		span := input.SourceSpan
		require.GreaterOrEqual(t, span.UnitIndex, 0)
		require.Less(t, span.UnitIndex, len(evidence.Units))
		runes := []rune(evidence.Units[span.UnitIndex].Text)
		require.GreaterOrEqual(t, span.CharStart, 0)
		require.Greater(t, span.CharEnd, span.CharStart)
		require.LessOrEqual(t, span.CharEnd, len(runes))
		assert.Equal(t, input.Content, string(runes[span.CharStart:span.CharEnd]))
	}
}
