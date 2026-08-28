package document

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pairTokenizer struct{}

func (pairTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "synthetic-pairs", Revision: "v1", PrefixTokenCountsMonotonic: true}
}

func (pairTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
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

func TestBuildEmbeddingInputsUsesNaturalBoundariesAndExactOverlap(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{
		{text: "AABBCCDDEEFF", heading: []string{"First"}, regions: []SourceEvidenceRegionV1{{ProviderID: "p1", Kind: EvidenceRegionParagraph, Order: 0, TextRange: EvidenceTextRangeV1{Start: 0, End: 12}}}},
		{text: "GGHH", heading: []string{"Second"}, regions: []SourceEvidenceRegionV1{{ProviderID: "p2", Kind: EvidenceRegionParagraph, Order: 0, TextRange: EvidenceTextRangeV1{Start: 0, End: 4}}}},
	})
	generation, err := BuildEmbeddingInputs(evidence, testInputPolicy(t, 4, 2))
	require.NoError(t, err)
	require.Len(t, generation.Inputs, 3)

	assert.Equal(t, []string{"AABBCCDD", "CCDDEEFF", "GGHH"}, []string{
		generation.Inputs[0].Content, generation.Inputs[1].Content, generation.Inputs[2].Content,
	})
	assert.Equal(t, []int{4, 4, 2}, []int{
		generation.Inputs[0].ContentTokens, generation.Inputs[1].ContentTokens, generation.Inputs[2].ContentTokens,
	})
	assert.Equal(t, [][]string{{"First"}}, generation.Inputs[0].HeadingPaths)
	assert.Equal(t, [][]string{{"First"}}, generation.Inputs[1].HeadingPaths)
	assert.Equal(t, []ChunkSpan{{UnitIndex: 0, CharStart: 4, CharEnd: 12}}, generation.Inputs[1].SourceSpans)
	assert.Equal(t, generation.Inputs[0].Content[len(generation.Inputs[0].Content)-4:], generation.Inputs[1].Content[:4])
	assertGenerationSpans(t, evidence, generation)
}

func TestBuildEmbeddingInputsNeverCombinesNaturalUnitsBeforeExactTokenization(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "A"}, {text: "B"}})
	policy := testInputPolicy(t, 1, 0)
	policy.Tokenizer = concatAdversarialTokenizer{}
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	require.Len(t, generation.Inputs, 2)
	assert.Equal(t, "A", generation.Inputs[0].Content)
	assert.Equal(t, "B", generation.Inputs[1].Content)
	assert.Equal(t, 1, generation.Inputs[0].ContentTokens)
	assert.Equal(t, 1, generation.Inputs[1].ContentTokens)
	assert.Equal(t, []ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 1}}, generation.Inputs[0].SourceSpans)
	assert.Equal(t, []ChunkSpan{{UnitIndex: 1, CharStart: 0, CharEnd: 1}}, generation.Inputs[1].SourceSpans)
}

func TestBuildEmbeddingInputsDerivesOverlapFromExactEmittedTokenization(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcdef"}})
	policy := testInputPolicy(t, 2, 1)
	policy.Tokenizer = overlapAdversarialTokenizer{}
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	require.Len(t, generation.Inputs, 3)
	assert.Equal(t, []string{"abcd", "bcd", "cdef"}, []string{
		generation.Inputs[0].Content, generation.Inputs[1].Content, generation.Inputs[2].Content,
	})
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
	policy := testInputPolicy(t, 3, 0)
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	assert.Equal(t, []string{"AAAA", "BBBB", "CCCC"}, []string{generation.Inputs[0].Content, generation.Inputs[1].Content, generation.Inputs[2].Content})
	assertGenerationSpans(t, evidence, generation)
}

func TestBuildEmbeddingInputsKeepsContentBudgetSeparateFromAttachmentAndEnvelope(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABBCC", heading: []string{"Evidence"}}})
	attachment, err := NewAttachmentContextSnapshot(AttachmentContextSnapshotConfig{Title: "Human title", Context: "Human context"})
	require.NoError(t, err)
	policy := testInputPolicy(t, 3, 0)
	policy.AttachmentContext = &attachment
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	require.Len(t, generation.Inputs, 1)

	input := generation.Inputs[0]
	assert.Equal(t, "AABBCC", input.Content)
	assert.Equal(t, 3, input.ContentTokens)
	assert.Greater(t, input.RenderedTokens, input.ContentTokens)
	assert.Contains(t, input.Rendered, "Human title")
	assert.Contains(t, input.Rendered, "Human context")
	assert.True(t, strings.HasPrefix(input.Rendered, "document: Title: Human title"))
	assert.True(t, strings.HasSuffix(input.Rendered, "AABBCC"))
	require.NotNil(t, generation.AttachmentContext)
	assert.Equal(t, "Human title", generation.AttachmentContext.Title())
	assert.Equal(t, "Human context", generation.AttachmentContext.Context())
}

func TestBuildEmbeddingInputsTruncatesOnlyAnIndivisibleTokenWhenDeclared(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcdefghij"}})
	policy := testInputPolicy(t, 1, 0)
	policy.MaxProviderBytes = 15
	policy.TruncationPolicy = TruncationPolicyTruncateIndivisible
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	require.Len(t, generation.Inputs, 5)
	assert.Equal(t, "ab", generation.Inputs[0].Content)
	assert.False(t, generation.Inputs[0].Truncated)

	policy.MaxProviderBytes = 11
	generation, err = BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	assert.Equal(t, "a", generation.Inputs[0].Content)
	assert.Equal(t, []ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 1}}, generation.Inputs[0].SourceSpans)
	assert.True(t, generation.Inputs[0].Truncated)

	policy.TruncationPolicy = TruncationPolicyReject
	_, err = BuildEmbeddingInputs(evidence, policy)
	require.ErrorContains(t, err, "indivisible token")
}

func TestBuildEmbeddingInputsRetokenizesExactTruncatedContent(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcd"}})
	policy := testInputPolicy(t, 1, 0)
	policy.Tokenizer = truncationAdversarialTokenizer{}
	policy.MaxProviderBytes = 13
	policy.TruncationPolicy = TruncationPolicyTruncateIndivisible
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	require.Len(t, generation.Inputs, 1)
	assert.Equal(t, "a", generation.Inputs[0].Content)
	assert.Equal(t, 1, generation.Inputs[0].ContentTokens)
	assert.Equal(t, []ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 1}}, generation.Inputs[0].SourceSpans)
	assert.True(t, generation.Inputs[0].Truncated)
}

func TestBuildEmbeddingInputsSkipsEmptyNaturalUnits(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AA"}, {text: ""}, {text: "BB"}})
	generation, err := BuildEmbeddingInputs(evidence, testInputPolicy(t, 2, 0))
	require.NoError(t, err)
	require.Len(t, generation.Inputs, 2)
	assert.Equal(t, []ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 2}}, generation.Inputs[0].SourceSpans)
	assert.Equal(t, []ChunkSpan{{UnitIndex: 2, CharStart: 0, CharEnd: 2}}, generation.Inputs[1].SourceSpans)

	allEmpty := testChunkEvidence(t, []sourceChunkUnit{{text: ""}, {text: ""}})
	emptyGeneration, err := BuildEmbeddingInputs(allEmpty, testInputPolicy(t, 2, 0))
	require.NoError(t, err)
	assert.Empty(t, emptyGeneration.Inputs)
	assert.NotEmpty(t, emptyGeneration.Checksum)
}

func TestBuildEmbeddingInputsSealsEveryIdentityInput(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB", heading: []string{"Heading"}}})
	basePolicy := testInputPolicy(t, 2, 0)
	base, err := BuildEmbeddingInputs(evidence, basePolicy)
	require.NoError(t, err)
	repeat, err := BuildEmbeddingInputs(evidence, basePolicy)
	require.NoError(t, err)
	assert.Equal(t, base, repeat)

	mutations := []func(*InputPolicy){
		func(policy *InputPolicy) { policy.Formatter = "evidence-text/v2" },
		func(policy *InputPolicy) { policy.Tokenizer = namedPairTokenizer{"other", "v1"} },
		func(policy *InputPolicy) { policy.Tokenizer = nonMonotonicNamedPairTokenizer{} },
		func(policy *InputPolicy) { policy.ContentTokenBudget++ },
		func(policy *InputPolicy) { policy.OverlapTokens = 1 },
		func(policy *InputPolicy) { policy.MaxProviderTokens++ },
		func(policy *InputPolicy) { policy.MaxProviderBytes++ },
		func(policy *InputPolicy) { policy.MaxGeneratedInputs++ },
		func(policy *InputPolicy) { policy.MaxTotalContentTokens++ },
		func(policy *InputPolicy) { policy.MaxTotalRenderedTokens++ },
		func(policy *InputPolicy) { policy.MaxTotalContentBytes++ },
		func(policy *InputPolicy) { policy.MaxTotalRenderedBytes++ },
		func(policy *InputPolicy) { policy.MaxFittingWorkTokens++ },
		func(policy *InputPolicy) { policy.MaxFittingWorkBytes++ },
	}
	for index, mutate := range mutations {
		policy := basePolicy
		mutate(&policy)
		changed, err := BuildEmbeddingInputs(evidence, policy)
		require.NoError(t, err, index)
		assert.NotEqual(t, base.Checksum, changed.Checksum, index)
	}

	changedHeading := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB", heading: []string{"Other heading"}}})
	changed, err := BuildEmbeddingInputs(changedHeading, basePolicy)
	require.NoError(t, err)
	assert.NotEqual(t, base.Checksum, changed.Checksum)
}

func TestBuildEmbeddingInputsDoesNotLeakFrontmatterOrProvenanceMetadata(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "Actual evidence", heading: []string{"Visible heading"}}})
	policy := testInputPolicy(t, 100, 0)
	policy.LexicalEvidenceFingerprint = strings.Repeat("d", 64)
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	require.Len(t, generation.Inputs, 1)

	rendered := generation.Inputs[0].Rendered
	assert.Equal(t, "document: Actual evidence", rendered)
	for _, forbidden := range []string{"---", "checksum", evidence.Checksum, evidence.Units[0].ID, policy.LexicalEvidenceFingerprint, "Visible heading", "page:", "line:", "byte:"} {
		assert.NotContains(t, rendered, forbidden)
	}
}

func TestBuildEmbeddingInputsRejectsUnboundedOrNoncanonicalTokenizerOutput(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}})
	for _, testCase := range []struct {
		name      string
		tokenizer Tokenizer
		want      string
	}{
		{"gap", brokenTokenizer{tokens: []TokenBoundary{{Start: 0, End: 1}, {Start: 2, End: 4}}}, "contiguous"},
		{"outside", brokenTokenizer{tokens: []TokenBoundary{{Start: 0, End: 5}}}, "bounds"},
		{"empty", brokenTokenizer{}, "at least one"},
		{"too many", brokenTokenizer{err: ErrTokenizerLimit}, "token limit"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			policy := testInputPolicy(t, 2, 0)
			policy.Tokenizer = testCase.tokenizer
			_, err := BuildEmbeddingInputs(evidence, policy)
			require.ErrorContains(t, err, testCase.want)
		})
	}
	policy := testInputPolicy(t, 2, 0)
	policy.MaxGeneratedInputs = 1
	_, err := BuildEmbeddingInputs(testChunkEvidence(t, []sourceChunkUnit{{text: "AABBCCDD"}}), policy)
	require.ErrorContains(t, err, "generated input limit")
}

func TestBuildEmbeddingInputsBoundsAggregateAmplificationBeforeAppend(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABBCCDDEEFF"}})
	policy := testInputPolicy(t, 4, 3)
	policy.MaxTotalContentBytes = 12
	policy.MaxTotalRenderedBytes = 44
	policy.MaxTotalContentTokens = 6
	policy.MaxTotalRenderedTokens = 40
	_, err := BuildEmbeddingInputs(evidence, policy)
	require.ErrorContains(t, err, "aggregate")

	policy.MaxTotalContentTokens = 100
	policy.MaxTotalRenderedTokens = 100
	policy.MaxTotalContentBytes = 100
	policy.MaxTotalRenderedBytes = 200
	generation, err := BuildEmbeddingInputs(evidence, policy)
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

	assert.False(t, addWithinAggregate(10, 1, 10))
	assert.False(t, addWithinAggregate(int64(^uint64(0)>>1), 1, int64(^uint64(0)>>1)))
}

func TestBuildEmbeddingInputsAppliesRemainingAggregateBytesBeforeConstruction(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AA"}, {text: strings.Repeat("b", 1024)}})
	tokenizer := &recordingRuneTokenizer{contentLimit: 10_000}
	policy := testInputPolicy(t, tokenizer.contentLimit, 0)
	policy.Tokenizer = tokenizer
	policy.MaxProviderBytes = 20_000
	policy.MaxProviderTokens = 20_000
	policy.MaxGeneratedInputs = 16
	policy.MaxTotalContentBytes = 6
	policy.MaxTotalRenderedBytes = 26
	policy.MaxTotalContentTokens = 20_000
	policy.MaxTotalRenderedTokens = 40_000
	_, err := BuildEmbeddingInputs(evidence, policy)
	require.ErrorContains(t, err, "aggregate")
	assert.LessOrEqual(t, tokenizer.maxExactContentRunes, 4, "remaining aggregate bytes must shrink a candidate before content construction")
}

func TestBuildEmbeddingInputsPassesRemainingAggregateTokenLimitToTokenizer(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AA"}, {text: "BBBB"}})
	tokenizer := &recordingRuneTokenizer{contentLimit: 4}
	policy := testInputPolicy(t, tokenizer.contentLimit, 0)
	policy.Tokenizer = tokenizer
	policy.MaxTotalContentTokens = 3
	_, err := BuildEmbeddingInputs(evidence, policy)
	require.ErrorContains(t, err, "aggregate")
	assert.True(t, tokenizer.sawOneTokenContentLimit, "the exact tokenizer must receive the remaining aggregate content-token limit")
}

func TestBuildEmbeddingInputsRejectsTypedNilAndTokenizerIdentityDrift(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AA"}})
	policy := testInputPolicy(t, 2, 0)
	var typedNil *nilTokenizer
	policy.Tokenizer = typedNil
	_, err := BuildEmbeddingInputs(evidence, policy)
	require.ErrorContains(t, err, "requires a tokenizer")

	policy = testInputPolicy(t, 2, 0)
	policy.Tokenizer = &identityDriftTokenizer{}
	_, err = BuildEmbeddingInputs(evidence, policy)
	require.ErrorContains(t, err, "identity changed")
}

func TestBuildEmbeddingInputsUsesBoundedFittingSearch(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: strings.Repeat("a", 1024)}})
	tokenizer := &countingRuneTokenizer{}
	policy := testInputPolicy(t, 1024, 0)
	policy.Tokenizer = tokenizer
	policy.MaxProviderBytes = 20
	policy.MaxProviderTokens = 2048
	policy.MaxGeneratedInputs = 128
	policy.MaxTotalContentBytes = 4096
	policy.MaxTotalRenderedBytes = 8192
	policy.MaxTotalContentTokens = 2048
	policy.MaxTotalRenderedTokens = 4096
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	require.NotEmpty(t, generation.Inputs)
	assert.LessOrEqual(t, tokenizer.calls, 5000, "fitting must not retry one token at a time")
}

func TestBuildEmbeddingInputsCapsCumulativeNonMonotonicFittingWork(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: strings.Repeat("a", 2048)}})
	tokenizer := &fittingWorkAdversarialTokenizer{}
	policy := testInputPolicy(t, 2048, 0)
	policy.Tokenizer = tokenizer
	policy.MaxProviderBytes = 8192
	policy.MaxProviderTokens = 4096
	policy.MaxFittingWorkBytes = 20_000
	policy.MaxFittingWorkTokens = 100_000
	_, err := BuildEmbeddingInputs(evidence, policy)
	require.ErrorContains(t, err, "fitting work")
	assert.LessOrEqual(t, tokenizer.exactCalls, 3, "the work budget must be consumed before reconstruction and tokenization")
}

func TestBuildEmbeddingInputsDoesNotAssumePrefixTokenCountsAreMonotonic(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "abcd"}})
	policy := testInputPolicy(t, 4, 0)
	policy.Tokenizer = nonMonotonicPrefixTokenizer{}
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
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
	policy.Tokenizer = suffixTruncationAdversarialTokenizer{}
	policy.ModelInput = contract
	policy.MaxProviderTokens = 1
	policy.TruncationPolicy = TruncationPolicyTruncateIndivisible
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
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
	policy.MaxProviderBytes = int64(len("document: ") + 7)
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	require.NotEmpty(t, generation.Inputs)
	assert.Equal(t, "AAAA", generation.Inputs[0].Content)
}

func TestEmbeddingInputGenerationRoundTripsAndRejectsMalformedJSON(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB", heading: []string{"Evidence"}}})
	attachment, err := NewAttachmentContextSnapshot(AttachmentContextSnapshotConfig{Title: "Human title", Context: "Human context"})
	require.NoError(t, err)
	policy := testInputPolicy(t, 2, 0)
	policy.AttachmentContext = &attachment
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	encoded, err := json.Marshal(generation)
	require.NoError(t, err)
	decoded, err := DecodeEmbeddingInputGeneration(encoded, testGenerationDecodeBounds())
	require.NoError(t, err)
	assert.Equal(t, generation, decoded)
	reencoded, err := json.Marshal(decoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(encoded), string(reencoded))

	for _, testCase := range []struct{ name, value, want string }{
		{"unknown generation field", strings.Replace(string(encoded), fmt.Sprintf(`{"version":%d`, EmbeddingInputGenerationVersion), fmt.Sprintf(`{"unknown":1,"version":%d`, EmbeddingInputGenerationVersion), 1), "unknown field"},
		{"forged checksum", strings.Replace(string(encoded), generation.Checksum, strings.Repeat("0", 64), 1), "checksum"},
		{"empty attachment", strings.Replace(string(encoded), `"title":"Human title","context":"Human context"`, `"title":"","context":""`, 1), "attachment"},
		{"negative span", strings.Replace(string(encoded), `"unit_index":0`, `"unit_index":-1`, 1), "source span"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := DecodeEmbeddingInputGeneration([]byte(testCase.value), testGenerationDecodeBounds())
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestEmbeddingInputGenerationDecodesFrozenPublishedV1WithoutReinterpretation(t *testing.T) {
	encoded, err := os.ReadFile("testdata/embedding-input-generation-v1.golden.json")
	require.NoError(t, err)
	generation, err := DecodeEmbeddingInputGeneration(encoded, testGenerationDecodeBounds())
	require.NoError(t, err)
	assert.Equal(t, 1, generation.Version)
	assert.Zero(t, generation.ContentTokenBudget)
	assert.Zero(t, generation.OverlapTokens)
	assert.Empty(t, generation.TruncationPolicy)
	assert.Empty(t, generation.ContextFingerprint)
	reencoded, err := json.MarshalIndent(generation, "", "  ")
	require.NoError(t, err)
	assert.JSONEq(t, string(encoded), string(reencoded))
	assert.NotContains(t, string(reencoded), "content_token_budget")
}

func TestEmbeddingInputGenerationBuildsPolicyCompleteCurrentVersion(t *testing.T) {
	generation, err := BuildEmbeddingInputs(testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}}), testInputPolicy(t, 2, 0))
	require.NoError(t, err)
	assert.Equal(t, 2, generation.Version)
	assert.Positive(t, generation.ContentTokenBudget)
	assert.NotEmpty(t, generation.TruncationPolicy)
	assert.NotEmpty(t, generation.ContextFingerprint)
}

func TestEmbeddingInputGenerationChecksumFramesEmptyHeadingCardinality(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}})
	generation, err := BuildEmbeddingInputs(evidence, testInputPolicy(t, 2, 0))
	require.NoError(t, err)
	encoded, err := json.Marshal(generation)
	require.NoError(t, err)
	tampered := strings.Replace(string(encoded), `"heading_paths":[[]]`, `"heading_paths":[]`, 1)
	require.NotEqual(t, string(encoded), tampered)
	_, err = DecodeEmbeddingInputGeneration([]byte(tampered), testGenerationDecodeBounds())
	require.ErrorContains(t, err, "canonical cardinality")
}

func TestEmbeddingInputGenerationDecodePreflightsCallerBounds(t *testing.T) {
	generation, err := BuildEmbeddingInputs(testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}}), testInputPolicy(t, 2, 0))
	require.NoError(t, err)
	encoded, err := json.Marshal(generation)
	require.NoError(t, err)

	tooManyInputs := []byte(`{"inputs":[` + strings.TrimSuffix(strings.Repeat(`{},`, 64), ",") + `]}`)
	bounds := testGenerationDecodeBounds()
	bounds.MaxInputs = 2
	_, err = DecodeEmbeddingInputGeneration(tooManyInputs, bounds)
	require.ErrorContains(t, err, "inputs")

	hugeString := []byte(`{"formatter":"` + strings.Repeat("x", 4096) + `"}`)
	bounds = testGenerationDecodeBounds()
	bounds.MaxStringBytes = 32
	_, err = DecodeEmbeddingInputGeneration(hugeString, bounds)
	require.ErrorContains(t, err, "raw string")

	escapeHeavyString := []byte(`{"formatter":"` + strings.Repeat(`\u0061`, 64) + `"}`)
	_, err = DecodeEmbeddingInputGeneration(escapeHeavyString, bounds)
	require.ErrorContains(t, err, "raw string")

	invalidEscapeAfterBound := []byte(`{"formatter":"` + strings.Repeat("x", 64) + `\q"}`)
	_, err = DecodeEmbeddingInputGeneration(invalidEscapeAfterBound, bounds)
	require.ErrorContains(t, err, "raw string")
	assert.NotContains(t, err.Error(), "escape", "the lexical bound must reject before JSON unmarshal reaches the invalid escape")

	bounds = testGenerationDecodeBounds()
	bounds.MaxObjectFields = 2
	_, err = DecodeEmbeddingInputGeneration([]byte(`{"a":null,"b":null,"c":null}`), bounds)
	require.ErrorContains(t, err, "object fields")

	bounds = testGenerationDecodeBounds()
	bounds.MaxEncodedBytes = int64(len(encoded) - 1)
	_, err = DecodeEmbeddingInputGeneration(encoded, bounds)
	require.ErrorContains(t, err, "encoded")
}

func TestEmbeddingInputGenerationDecodePreflightsIntegerTokens(t *testing.T) {
	bounds := testGenerationDecodeBounds()
	for _, value := range []string{"0", "-0", "9223372036854775807", "-9223372036854775808"} {
		t.Run("valid_"+value, func(t *testing.T) {
			err := preflightEmbeddingInputGenerationJSON([]byte(`{"value":`+value+`}`), bounds)
			require.NoError(t, err)
		})
	}

	for _, testCase := range []struct {
		name  string
		value string
	}{
		{"overlong", "123456789012345678901"},
		{"decimal", "1.0"},
		{"exponent", "1e3"},
		{"leading_zero", "01"},
		{"negative_leading_zero", "-01"},
		{"sign_only", "-"},
		{"plus_sign", "+1"},
		{"double_sign", "--1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := preflightEmbeddingInputGenerationJSON([]byte(`{"value":`+testCase.value+`}`), bounds)
			require.ErrorContains(t, err, "integer")
		})
	}
}

func TestEmbeddingInputGenerationChecksumFramesTypedCollectionsWithoutCollisions(t *testing.T) {
	generation, err := BuildEmbeddingInputs(testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}}), testInputPolicy(t, 2, 0))
	require.NoError(t, err)
	span := generation.Inputs[0].SourceSpans[0]

	headingValues := generation
	headingValues.Inputs = slices.Clone(generation.Inputs)
	headingValues.Inputs[0].HeadingPaths = [][]string{{"span", "0", "1", "2"}}
	headingValues.Inputs[0].SourceSpans = []ChunkSpan{span}

	spanValues := generation
	spanValues.Inputs = slices.Clone(generation.Inputs)
	spanValues.Inputs[0].HeadingPaths = [][]string{{}}
	spanValues.Inputs[0].SourceSpans = []ChunkSpan{{UnitIndex: 0, CharStart: 1, CharEnd: 2}, span}

	assert.NotEqual(t, generationFingerprint(headingValues), generationFingerprint(spanValues), "heading text must not collide with span markers and integer frames")
}

func TestEmbeddingInputGenerationRequiresCanonicalCollectionCardinalities(t *testing.T) {
	generation, err := BuildEmbeddingInputs(testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}}), testInputPolicy(t, 2, 0))
	require.NoError(t, err)
	for _, testCase := range []struct {
		name   string
		mutate func(*GeneratedEmbeddingInput)
	}{
		{"heading paths", func(input *GeneratedEmbeddingInput) { input.HeadingPaths = nil }},
		{"source spans", func(input *GeneratedEmbeddingInput) {
			input.SourceSpans = append(input.SourceSpans, input.SourceSpans[0])
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tampered := generation
			tampered.Inputs = slices.Clone(generation.Inputs)
			testCase.mutate(&tampered.Inputs[0])
			tampered.Checksum = generationFingerprint(tampered)
			require.ErrorContains(t, validateEmbeddingInputGeneration(tampered), "canonical cardinality")
		})
	}
}

func TestEmbeddingInputGenerationMapsContextIntoE1ExactlyOnce(t *testing.T) {
	evidence := testChunkEvidence(t, []sourceChunkUnit{{text: "AABB", heading: []string{"Evidence"}}})
	attachment, err := NewAttachmentContextSnapshot(AttachmentContextSnapshotConfig{Title: "Human title", Context: "Human context"})
	require.NoError(t, err)
	policy := testInputPolicy(t, 2, 0)
	policy.AttachmentContext = &attachment
	generation, err := BuildEmbeddingInputs(evidence, policy)
	require.NoError(t, err)
	generated := generation.Inputs[0]
	inputs, err := generation.ToEmbeddingInputs(policy.ModelInput)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	input := inputs[0]
	assert.Contains(t, input.Text, "Human title")
	assert.Contains(t, input.Text, "Human context")
	assert.NotEqual(t, generated.Rendered, input.Text)
	assert.Equal(t, generated.Rendered, policy.ModelInput.EncodeDocument(input.Text))

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
	generation, err := BuildEmbeddingInputs(testChunkEvidence(t, []sourceChunkUnit{{text: "AABB"}}), policy)
	require.NoError(t, err)
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
			generation, err := BuildEmbeddingInputs(evidence, policy)
			require.NoError(t, err)
			encoded, err := json.MarshalIndent(generation, "", "  ")
			require.NoError(t, err)
			encoded = append(encoded, '\n')
			goldenPath := "testdata/chunks-" + strings.ReplaceAll(string(config.Profile), "/", "-") + ".golden.json"
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				require.NoError(t, os.WriteFile(goldenPath, encoded, 0o644))
			}
			golden, err := os.ReadFile(goldenPath)
			require.NoError(t, err)
			assert.JSONEq(t, string(golden), string(encoded))
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
		policy.MaxGeneratedInputs = 512
		policy.MaxTotalContentTokens = 512 * 32
		policy.MaxTotalRenderedTokens = 512 * int64(policy.MaxProviderTokens)
		policy.MaxTotalContentBytes = 512 * policy.MaxProviderBytes
		policy.MaxTotalRenderedBytes = 512 * policy.MaxProviderBytes
		generation, err := BuildEmbeddingInputs(evidence, policy)
		require.NoError(t, err)
		assertGenerationSpans(t, evidence, generation)
		for index, input := range generation.Inputs {
			assert.LessOrEqual(t, input.ContentTokens, budget)
			assert.LessOrEqual(t, input.RenderedTokens, policy.MaxProviderTokens)
			assert.LessOrEqual(t, int64(len(input.Rendered)), policy.MaxProviderBytes)
			exactTokens, err := policy.Tokenizer.Tokenize(input.Content, budget)
			require.NoError(t, err)
			require.NoError(t, validateTokenBoundaries(exactTokens, utf8.RuneCountInString(input.Content), budget))
			assert.Equal(t, len(exactTokens), input.ContentTokens)
			if index == 0 || overlap == 0 || generation.Inputs[index-1].Truncated || generation.Inputs[index-1].SourceSpans[0].UnitIndex != input.SourceSpans[0].UnitIndex {
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

type namedPairTokenizer struct{ name, revision string }

func (tokenizer namedPairTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: tokenizer.name, Revision: tokenizer.revision, PrefixTokenCountsMonotonic: true}
}

type nonMonotonicNamedPairTokenizer struct{}

func (nonMonotonicNamedPairTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "synthetic-pairs", Revision: "v1"}
}
func (nonMonotonicNamedPairTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	return pairTokenizer{}.Tokenize(text, limit)
}

type concatAdversarialTokenizer struct{}

func (concatAdversarialTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "concat-adversarial", Revision: "v1"}
}

type overlapAdversarialTokenizer struct{}

func (overlapAdversarialTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "overlap-adversarial", Revision: "v1"}
}
func (overlapAdversarialTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	var result []TokenBoundary
	switch text {
	case "abcdef":
		result = []TokenBoundary{{Start: 0, End: 2}, {Start: 2, End: 4}, {Start: 4, End: 6}}
	case "abcd":
		result = []TokenBoundary{{Start: 0, End: 1}, {Start: 1, End: 4}}
	case "bcd":
		result = []TokenBoundary{{Start: 0, End: 1}, {Start: 1, End: 3}}
	case "cdef":
		result = []TokenBoundary{{Start: 0, End: 2}, {Start: 2, End: 4}}
	default:
		return runeBoundaries(text, limit)
	}
	if len(result) > limit {
		return nil, ErrTokenizerLimit
	}
	return result, nil
}
func (concatAdversarialTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	if text == "AB" {
		if limit < 1 {
			return nil, ErrTokenizerLimit
		}
		return []TokenBoundary{{Start: 0, End: 2}}, nil
	}
	return runeBoundaries(text, limit)
}

type truncationAdversarialTokenizer struct{}

func (truncationAdversarialTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "truncation-adversarial", Revision: "v1"}
}
func (truncationAdversarialTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	if text == "abcd" {
		if limit < 1 {
			return nil, ErrTokenizerLimit
		}
		return []TokenBoundary{{Start: 0, End: 4}}, nil
	}
	return runeBoundaries(text, limit)
}

type nonMonotonicPrefixTokenizer struct{}

func (nonMonotonicPrefixTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "nonmonotonic-prefix", Revision: "v1"}
}

type suffixTruncationAdversarialTokenizer struct{}

func (suffixTruncationAdversarialTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "suffix-truncation-adversarial", Revision: "v1", PrefixTokenCountsMonotonic: true}
}

type fittingWorkAdversarialTokenizer struct{ exactCalls int }

func (*fittingWorkAdversarialTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "fitting-work-adversarial", Revision: "v1"}
}
func (tokenizer *fittingWorkAdversarialTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	if limit == maxEmbeddingTokensPerGeneration {
		return runeBoundaries(text, limit)
	}
	tokenizer.exactCalls++
	return nil, ErrTokenizerLimit
}
func (suffixTruncationAdversarialTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	if limit > 1 || !strings.HasSuffix(text, " suffix") {
		return []TokenBoundary{{Start: 0, End: utf8.RuneCountInString(text)}}, nil
	}
	switch text {
	case "a suffix", "abcd suffix":
		return []TokenBoundary{{Start: 0, End: utf8.RuneCountInString(text)}}, nil
	default:
		return nil, ErrTokenizerLimit
	}
}
func (nonMonotonicPrefixTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	if limit > 4 || strings.HasPrefix(text, "document: ") {
		return runeBoundaries(text, limit)
	}
	switch text {
	case "abcd", "ab":
		return nil, ErrTokenizerLimit
	default:
		return runeBoundaries(text, limit)
	}
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
func (namedPairTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	return pairTokenizer{}.Tokenize(text, limit)
}

type brokenTokenizer struct {
	tokens []TokenBoundary
	err    error
}

type nilTokenizer struct{}

func (*nilTokenizer) Identity() TokenizerIdentity { panic("typed nil tokenizer must not be called") }
func (*nilTokenizer) Tokenize(string, int) ([]TokenBoundary, error) {
	panic("typed nil tokenizer must not be called")
}

type identityDriftTokenizer struct{ calls int }

func (tokenizer *identityDriftTokenizer) Identity() TokenizerIdentity {
	tokenizer.calls++
	return TokenizerIdentity{Name: "identity-drift", Revision: fmt.Sprintf("v%d", tokenizer.calls), PrefixTokenCountsMonotonic: true}
}
func (*identityDriftTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	return pairTokenizer{}.Tokenize(text, limit)
}

type countingRuneTokenizer struct{ calls int }

func (*countingRuneTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "counting-runes", Revision: "v1", PrefixTokenCountsMonotonic: true}
}
func (tokenizer *countingRuneTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	tokenizer.calls++
	return runeBoundaries(text, limit)
}

type recordingRuneTokenizer struct {
	contentLimit            int
	maxExactContentRunes    int
	sawOneTokenContentLimit bool
}

func (*recordingRuneTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "recording-runes", Revision: "v1", PrefixTokenCountsMonotonic: true}
}
func (tokenizer *recordingRuneTokenizer) Tokenize(text string, limit int) ([]TokenBoundary, error) {
	if limit == tokenizer.contentLimit {
		tokenizer.maxExactContentRunes = max(tokenizer.maxExactContentRunes, utf8.RuneCountInString(text))
	}
	if limit == 1 && !strings.HasPrefix(text, "document: ") {
		tokenizer.sawOneTokenContentLimit = true
	}
	return runeBoundaries(text, limit)
}

func (brokenTokenizer) Identity() TokenizerIdentity {
	return TokenizerIdentity{Name: "broken", Revision: "v1"}
}
func (tokenizer brokenTokenizer) Tokenize(string, int) ([]TokenBoundary, error) {
	return tokenizer.tokens, tokenizer.err
}

func testInputPolicy(t *testing.T, budget, overlap int) InputPolicy {
	t.Helper()
	contract, err := NewModelInputContract(ModelInputContractConfig{
		Profile: ModelInputProfileCustom, CompatibilityID: "synthetic-space",
		Document: ModelInputEncoder{Mode: ModelInputModeText, Template: "document: {{content}}"},
		Query:    ModelInputEncoder{Mode: ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	return InputPolicy{
		Tokenizer: pairTokenizer{}, ContentTokenBudget: budget, OverlapTokens: overlap,
		MaxProviderTokens: 256, MaxProviderBytes: 4096, MaxGeneratedInputs: 128,
		MaxTotalContentTokens: 4096, MaxTotalRenderedTokens: 8192,
		MaxTotalContentBytes: 1 << 20, MaxTotalRenderedBytes: 2 << 20,
		MaxFittingWorkBytes: 8 << 20, MaxFittingWorkTokens: 1 << 20,
		ModelInput: contract, Formatter: "evidence-text/v1", LexicalEvidenceFingerprint: strings.Repeat("a", 64),
		ContextFingerprint: strings.Repeat("b", 64),
		TruncationPolicy:   TruncationPolicyReject,
	}
}

func testGenerationDecodeBounds() EmbeddingInputGenerationDecodeBounds {
	return EmbeddingInputGenerationDecodeBounds{
		MaxEncodedBytes:     1 << 20,
		MaxInputs:           128,
		MaxObjectFields:     32,
		MaxStringBytes:      1 << 16,
		MaxTotalStringBytes: 1 << 20,
	}
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
		var reconstructed strings.Builder
		for _, span := range input.SourceSpans {
			require.GreaterOrEqual(t, span.UnitIndex, 0)
			require.Less(t, span.UnitIndex, len(evidence.Units))
			runes := []rune(evidence.Units[span.UnitIndex].Text)
			require.GreaterOrEqual(t, span.CharStart, 0)
			require.Greater(t, span.CharEnd, span.CharStart)
			require.LessOrEqual(t, span.CharEnd, len(runes))
			reconstructed.WriteString(string(runes[span.CharStart:span.CharEnd]))
		}
		assert.Equal(t, input.Content, reconstructed.String())
	}
}
