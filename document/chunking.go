package document

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	maxTruncationSearchRunes = 4096
	maxNonMonotonicFitChecks = 4096
	maxGenerationWorkTokens  = int64(100_000_000)
	maxGenerationWorkBytes   = int64(16 << 30)
)

// InputPolicy is the resolved runtime form of one rendition-chunk binding: the
// operator's identity-bearing chunk policy, the tokenizer implementation that
// proves it, the sealed model-input contract, the lexical evidence identity,
// and the provider's hard rendered-input limits. Everything here enters the
// generation's policy fingerprint.
type InputPolicy struct {
	Chunk                      EmbeddingChunkPolicyV1
	Tokenizer                  Tokenizer
	ModelInput                 ModelInputContract
	LexicalEvidenceFingerprint string
	MaxInputTokens             int
	MaxInputBytes              int64
	AttachmentContext          *AttachmentContextSnapshot
}

// NewInputPolicy resolves one rendition-chunk binding into its runtime form.
// The tokenizer must report exactly the identity the binding declares.
func NewInputPolicy(binding EmbeddingBindingV1, tokenizer Tokenizer, lexicalEvidenceFingerprint string, attachment *AttachmentContextSnapshot) (InputPolicy, error) {
	if binding.InputKind != EmbeddingInputRenditionChunk || binding.Chunk == nil {
		return InputPolicy{}, errors.New("embedding input policy requires a rendition_chunk binding with a chunk policy")
	}
	policy := InputPolicy{
		Chunk: *binding.Chunk, Tokenizer: tokenizer, ModelInput: binding.ModelInput,
		LexicalEvidenceFingerprint: lexicalEvidenceFingerprint,
		MaxInputTokens:             binding.MaxInputTokens, MaxInputBytes: binding.MaxInputBytes,
		AttachmentContext: attachment,
	}
	if err := validateInputPolicy(policy); err != nil {
		return InputPolicy{}, err
	}
	return policy, nil
}

// GenerationLimits bound one BuildEmbeddingInputs call. They can only turn a
// generation into an error and never change a successful output, so they stay
// out of every fingerprint.
type GenerationLimits struct {
	MaxInputs              int
	MaxTotalContentTokens  int64
	MaxTotalRenderedTokens int64
	MaxTotalContentBytes   int64
	MaxTotalRenderedBytes  int64
	MaxFittingWorkTokens   int64
	MaxFittingWorkBytes    int64
}

func (limits GenerationLimits) totals() generationTotals {
	return generationTotals{limits.MaxTotalContentTokens, limits.MaxTotalRenderedTokens, limits.MaxTotalContentBytes, limits.MaxTotalRenderedBytes}
}

func validateInputPolicy(policy InputPolicy) error {
	if nilInterface(policy.Tokenizer) {
		return errors.New("embedding input policy requires a tokenizer")
	}
	if err := validateChunkPolicy(policy.Chunk); err != nil {
		return err
	}
	declared := TokenizerIdentity{Name: policy.Chunk.Tokenizer, Revision: policy.Chunk.TokenizerRevision}
	if identity := policy.Tokenizer.Identity(); identity != declared {
		return fmt.Errorf("tokenizer identity %s/%s does not match declared chunk policy %s/%s", identity.Name, identity.Revision, declared.Name, declared.Revision)
	}
	if err := validateProviderInputLimits(policy.MaxInputTokens, policy.MaxInputBytes); err != nil {
		return err
	}
	if err := validateModelInputContract(policy.ModelInput); err != nil || policy.ModelInput.Profile == "" {
		return errors.New("embedding input policy has invalid model-input contract")
	}
	if err := validateFingerprint(policy.LexicalEvidenceFingerprint, "lexical evidence fingerprint"); err != nil {
		return err
	}
	if policy.AttachmentContext != nil {
		return validateAttachmentContextSnapshot(*policy.AttachmentContext)
	}
	return nil
}

func validateGenerationLimits(limits GenerationLimits) error {
	if limits.MaxInputs < 1 || limits.MaxInputs > maxGeneratedInputs {
		return errors.New("embedding generated input limit is invalid")
	}
	for _, total := range []struct {
		name  string
		value int64
		limit int64
	}{
		{"content token", limits.MaxTotalContentTokens, maxGenerationTotalTokens},
		{"rendered token", limits.MaxTotalRenderedTokens, maxGenerationTotalTokens},
		{"content byte", limits.MaxTotalContentBytes, maxGenerationTotalBytes},
		{"rendered byte", limits.MaxTotalRenderedBytes, maxGenerationTotalBytes},
		{"fitting work token", limits.MaxFittingWorkTokens, maxGenerationWorkTokens},
		{"fitting work byte", limits.MaxFittingWorkBytes, maxGenerationWorkBytes},
	} {
		if total.value < 1 || total.value > total.limit {
			return fmt.Errorf("embedding aggregate %s limit is invalid", total.name)
		}
	}
	return nil
}

// BuildEmbeddingInputs derives provider inputs from canonical evidence only.
// It never reads a retained Markdown or YAML rendition.
func BuildEmbeddingInputs(evidence NormalizedEvidenceV1, policy InputPolicy, limits GenerationLimits) (EmbeddingInputGeneration, error) {
	if err := validateInputPolicy(policy); err != nil {
		return EmbeddingInputGeneration{}, err
	}
	if err := validateGenerationLimits(limits); err != nil {
		return EmbeddingInputGeneration{}, err
	}
	if _, checksum, err := MarshalNormalizedEvidenceV1(evidence); err != nil || checksum != evidence.Checksum {
		if err != nil {
			return EmbeddingInputGeneration{}, fmt.Errorf("validate normalized evidence: %w", err)
		}
		return EmbeddingInputGeneration{}, errors.New("normalized evidence checksum is invalid")
	}
	tokenizerIdentity := policy.Tokenizer.Identity()
	tokens, naturalEnds, err := tokenizeEvidence(evidence, policy.Tokenizer)
	if err != nil {
		return EmbeddingInputGeneration{}, err
	}
	result := EmbeddingInputGeneration{
		Version: EmbeddingInputGenerationVersion, EvidenceChecksum: evidence.Checksum, Chunk: policy.Chunk,
		ModelInputFingerprint: policy.ModelInput.Fingerprint, LexicalEvidenceFingerprint: policy.LexicalEvidenceFingerprint,
		MaxInputTokens: policy.MaxInputTokens, MaxInputBytes: policy.MaxInputBytes,
		Inputs: make([]GeneratedEmbeddingInput, 0, min(limits.MaxInputs, len(tokens))),
	}
	if policy.AttachmentContext != nil {
		attachment := *policy.AttachmentContext
		result.AttachmentContext = &attachment
	}
	result.PolicyFingerprint, err = inputPolicyFingerprint(result.policyIdentity())
	if err != nil {
		return EmbeddingInputGeneration{}, err
	}
	fitter := newInputFitter(evidence, tokens, naturalEnds, policy, limits)
	var totals generationTotals
	for start := 0; start < len(tokens); {
		if len(result.Inputs) == limits.MaxInputs {
			return EmbeddingInputGeneration{}, errors.New("embedding generated input limit exceeded")
		}
		chunk, err := remainingChunkLimits(totals, policy, limits)
		if err != nil {
			return EmbeddingInputGeneration{}, err
		}
		end := chooseChunkEnd(start, tokens[start].unitEnd, policy.Chunk.MaxTokens, policy.Chunk.OverlapTokens, naturalEnds)
		fitted, err := fitter.fit(start, end, chunk, len(result.Inputs))
		if err != nil {
			return EmbeddingInputGeneration{}, err
		}
		if !totals.add(fitted.input, limits.totals()) {
			return EmbeddingInputGeneration{}, errors.New("embedding generation exceeds aggregate limits")
		}
		result.Inputs = append(result.Inputs, fitted.input)
		start, err = fitter.advance(start, fitted, chunk.contentTokens)
		if err != nil {
			return EmbeddingInputGeneration{}, err
		}
	}
	if policy.Tokenizer.Identity() != tokenizerIdentity {
		return EmbeddingInputGeneration{}, errors.New("embedding tokenizer identity changed during generation")
	}
	result.TotalContentTokens, result.TotalRenderedTokens = totals.contentTokens, totals.renderedTokens
	result.TotalContentBytes, result.TotalRenderedBytes = totals.contentBytes, totals.renderedBytes
	result.Checksum, err = generationChecksum(result)
	if err != nil {
		return EmbeddingInputGeneration{}, err
	}
	return result, nil
}

// chunkLimits are the limits one input may consume: the provider hard limits
// narrowed by whatever aggregate room the generation still has.
type chunkLimits struct {
	contentTokens  int
	renderedTokens int
	contentBytes   int64
	renderedBytes  int64
}

func remainingChunkLimits(totals generationTotals, policy InputPolicy, limits GenerationLimits) (chunkLimits, error) {
	remaining := chunkLimits{
		contentTokens:  int(limits.MaxTotalContentTokens - totals.contentTokens),
		renderedTokens: int(limits.MaxTotalRenderedTokens - totals.renderedTokens),
		contentBytes:   limits.MaxTotalContentBytes - totals.contentBytes,
		renderedBytes:  limits.MaxTotalRenderedBytes - totals.renderedBytes,
	}
	if remaining.contentTokens < 1 || remaining.renderedTokens < 1 || remaining.contentBytes < 1 || remaining.renderedBytes < 1 {
		return chunkLimits{}, errors.New("embedding generation exceeds aggregate limits")
	}
	return chunkLimits{
		contentTokens:  min(remaining.contentTokens, policy.Chunk.MaxTokens),
		renderedTokens: min(remaining.renderedTokens, policy.MaxInputTokens),
		contentBytes:   min(remaining.contentBytes, policy.MaxInputBytes),
		renderedBytes:  min(remaining.renderedBytes, policy.MaxInputBytes),
	}, nil
}

type embeddingToken struct {
	unitIndex int
	start     int
	end       int
	unitEnd   int
}

// tokenizeEvidence pre-tokenizes every unit separately so candidate cuts never
// combine natural units. naturalEnds[i] marks token index i as a preferred cut.
func tokenizeEvidence(evidence NormalizedEvidenceV1, tokenizer Tokenizer) ([]embeddingToken, []bool, error) {
	tokens := make([]embeddingToken, 0, min(maxEmbeddingTokensPerGeneration, len(evidence.Units)))
	var naturalEnds []bool
	for unitIndex, unit := range evidence.Units {
		runeCount := utf8.RuneCountInString(unit.Text)
		if runeCount == 0 {
			continue
		}
		remaining := maxEmbeddingTokensPerGeneration - len(tokens)
		if remaining < 1 {
			return nil, nil, ErrTokenizerLimit
		}
		unitTokens, err := tokenizer.Tokenize(unit.Text, remaining)
		if err != nil {
			if errors.Is(err, ErrTokenizerLimit) {
				return nil, nil, ErrTokenizerLimit
			}
			return nil, nil, fmt.Errorf("tokenize evidence unit %d: %w", unitIndex, err)
		}
		if err := validateTokenBoundaries(unitTokens, runeCount, remaining); err != nil {
			return nil, nil, fmt.Errorf("tokenize evidence unit %d: %w", unitIndex, err)
		}
		base := len(tokens)
		unitEnd := base + len(unitTokens)
		cuts := naturalRuneEnds(unit, runeCount)
		naturalEnds = append(naturalEnds, make([]bool, unitEnd+1-len(naturalEnds))...)
		for localIndex, token := range unitTokens {
			tokens = append(tokens, embeddingToken{unitIndex: unitIndex, start: token.Start, end: token.End, unitEnd: unitEnd})
			_, natural := cuts[token.End]
			naturalEnds[base+localIndex+1] = natural
		}
		naturalEnds[unitEnd] = true
	}
	return tokens, naturalEnds, nil
}

func naturalRuneEnds(unit NormalizedEvidenceUnitV1, runeCount int) map[int]struct{} {
	result := map[int]struct{}{runeCount: {}}
	for _, region := range unit.Regions {
		result[region.TextRange.Start] = struct{}{}
		result[region.TextRange.End] = struct{}{}
	}
	for _, table := range unit.Tables {
		if len(table.Cells) == 0 {
			continue
		}
		start, end := table.Cells[0].TextRange.Start, table.Cells[0].TextRange.End
		for _, cell := range table.Cells[1:] {
			start = min(start, cell.TextRange.Start)
			end = max(end, cell.TextRange.End)
		}
		result[start] = struct{}{}
		result[end] = struct{}{}
	}
	return result
}

func chooseChunkEnd(start, total, budget, overlap int, naturalEnds []bool) int {
	hardEnd := min(total, start+budget)
	for candidate := hardEnd; candidate > start; candidate-- {
		if naturalEnds[candidate] && (candidate == total || candidate-start > overlap) {
			return candidate
		}
	}
	return hardEnd
}

func preferNaturalFitEnd(start, fittedEnd, overlap int, naturalEnds []bool) int {
	for candidate := fittedEnd; candidate > start; candidate-- {
		if naturalEnds[candidate] && candidate-start > overlap {
			return candidate
		}
	}
	return fittedEnd
}

// inputFitter turns pre-tokenized candidate ranges into exact provider inputs
// under one policy, charging every probe against the fitting work budget.
type inputFitter struct {
	evidence    NormalizedEvidenceV1
	tokens      []embeddingToken
	naturalEnds []bool
	policy      InputPolicy
	attachment  AttachmentContextSnapshot
	monotonic   bool
	work        fittingWorkBudget
}

type fittingWorkBudget struct {
	remainingTokens int64
	remainingBytes  int64
}

type fittedInput struct {
	input             GeneratedEmbeddingInput
	contentBoundaries []TokenBoundary
	end               int
	truncated         bool
}

var errProviderInputLimit = errors.New("provider input limit")
var errContentTokenLimit = errors.New("content token limit")

func fittingLimitError(err error) bool {
	return errors.Is(err, errProviderInputLimit) || errors.Is(err, errContentTokenLimit)
}

func newInputFitter(evidence NormalizedEvidenceV1, tokens []embeddingToken, naturalEnds []bool, policy InputPolicy, limits GenerationLimits) *inputFitter {
	fitter := &inputFitter{
		evidence: evidence, tokens: tokens, naturalEnds: naturalEnds, policy: policy,
		monotonic: policy.Tokenizer.PrefixTokenCountsMonotonic() && strings.HasSuffix(policy.ModelInput.Document.Template, modelInputContentSlot),
		work:      fittingWorkBudget{remainingTokens: limits.MaxFittingWorkTokens, remainingBytes: limits.MaxFittingWorkBytes},
	}
	if policy.AttachmentContext != nil {
		fitter.attachment = *policy.AttachmentContext
	}
	return fitter
}

func (fitter *inputFitter) fit(start, end int, limits chunkLimits, ordinal int) (fittedInput, error) {
	byteEnd, err := fitter.maximalByteFit(start, end, limits)
	if err != nil {
		return fittedInput{}, err
	}
	if byteEnd == 0 {
		return fitter.truncateOrReject(start, limits, ordinal)
	}
	candidates := []int{preferNaturalFitEnd(start, byteEnd, fitter.policy.Chunk.OverlapTokens, fitter.naturalEnds)}
	if candidates[0] != byteEnd {
		candidates = append(candidates, byteEnd)
	}
	for _, candidateEnd := range candidates {
		input, contentBoundaries, err := fitter.make(fitter.tokens[start:candidateEnd], limits, ordinal)
		if err == nil {
			return fittedInput{input: input, contentBoundaries: contentBoundaries, end: candidateEnd}, nil
		}
		if !fittingLimitError(err) {
			return fittedInput{}, err
		}
	}
	bestEnd, err := fitter.tokenFit(start, byteEnd, limits, ordinal)
	if err != nil {
		return fittedInput{}, err
	}
	if bestEnd == 0 {
		return fitter.truncateOrReject(start, limits, ordinal)
	}
	input, contentBoundaries, err := fitter.make(fitter.tokens[start:bestEnd], limits, ordinal)
	if err != nil {
		return fittedInput{}, fmt.Errorf("validate fitted embedding input: %w", err)
	}
	return fittedInput{input: input, contentBoundaries: contentBoundaries, end: bestEnd}, nil
}

// advance returns the next chunk start, moving the source rune cursor by the
// exact emitted tokenization so overlap is measured in real tokens.
func (fitter *inputFitter) advance(start int, fitted fittedInput, remainingContentTokens int) (int, error) {
	overlap := fitter.policy.Chunk.OverlapTokens
	if fitted.truncated || fitted.end == fitter.tokens[start].unitEnd || overlap == 0 {
		return fitted.end, nil
	}
	if len(fitted.contentBoundaries) <= overlap {
		if remainingContentTokens <= overlap {
			return 0, errors.New("embedding generation exceeds aggregate limits while preserving configured token overlap")
		}
		return 0, errors.New("provider limits cannot preserve configured token overlap")
	}
	desiredRuneStart := fitted.input.SourceSpan.CharStart + fitted.contentBoundaries[len(fitted.contentBoundaries)-overlap].Start
	// Exact tokenization can place the overlap boundary inside the current
	// pre-tokenized token. The token index may stay unchanged, but the source
	// rune cursor must always advance.
	if desiredRuneStart <= fitter.tokens[start].start {
		return 0, errors.New("exact token overlap does not advance within the source unit")
	}
	next := start
	for next < fitted.end && fitter.tokens[next].end <= desiredRuneStart {
		next++
	}
	if next == fitted.end {
		return 0, errors.New("exact token overlap does not advance within the source unit")
	}
	fitter.tokens[next].start = desiredRuneStart
	return next, nil
}

func (fitter *inputFitter) maximalByteFit(start, end int, limits chunkLimits) (int, error) {
	low, high := start+1, end
	bestEnd := 0
	for low <= high {
		candidate := low + (high-low)/2
		fits, err := fitter.fitsByteLimits(fitter.tokens[start:candidate], limits)
		if err != nil {
			return 0, err
		}
		if fits {
			bestEnd = candidate
			low = candidate + 1
		} else {
			high = candidate - 1
		}
	}
	return bestEnd, nil
}

func (fitter *inputFitter) fitsByteLimits(tokens []embeddingToken, limits chunkLimits) (bool, error) {
	span, _, err := fitter.tokenMetadata(tokens)
	if err != nil {
		return false, err
	}
	contentBytes, err := fitter.contentByteLength(span, math.MaxInt64)
	if err != nil {
		return false, err
	}
	contentRunes := int64(tokens[len(tokens)-1].end - tokens[0].start)
	if err := fitter.work.consume([]int64{contentBytes}, []int64{contentRunes}); err != nil {
		return false, err
	}
	if contentBytes > limits.contentBytes {
		return false, nil
	}
	return renderedDocumentByteLength(fitter.policy.ModelInput.Document, fitter.attachment, contentBytes) <= limits.renderedBytes, nil
}

// tokenFit finds the longest prefix of tokens[start:end) that fits every
// token limit; it returns 0 when no prefix longer than the first token fits.
func (fitter *inputFitter) tokenFit(start, end int, limits chunkLimits, ordinal int) (int, error) {
	if fitter.monotonic {
		low, high := start+1, end-1
		bestEnd := 0
		for low <= high {
			candidate := low + (high-low)/2
			_, _, err := fitter.make(fitter.tokens[start:candidate], limits, ordinal)
			if err == nil {
				bestEnd = candidate
				low = candidate + 1
				continue
			}
			if !fittingLimitError(err) {
				return 0, err
			}
			high = candidate - 1
		}
		return bestEnd, nil
	}
	lower := max(start+1, end-maxNonMonotonicFitChecks)
	for candidate := end - 1; candidate >= lower; candidate-- {
		_, _, err := fitter.make(fitter.tokens[start:candidate], limits, ordinal)
		if err == nil {
			return candidate, nil
		}
		if !fittingLimitError(err) {
			return 0, err
		}
	}
	if lower > start+1 {
		return 0, errors.New("non-monotonic tokenizer fit search exceeds bounded checks")
	}
	return 0, nil
}

func (fitter *inputFitter) truncateOrReject(start int, limits chunkLimits, ordinal int) (fittedInput, error) {
	if fitter.policy.Chunk.TruncationPolicy == TruncationPolicyReject {
		return fittedInput{}, errors.New("indivisible token exceeds content budget, provider limits, or aggregate limits")
	}
	input, contentBoundaries, err := fitter.truncate(fitter.tokens[start], limits, ordinal)
	if err != nil {
		return fittedInput{}, err
	}
	input.Truncated = true
	return fittedInput{input: input, contentBoundaries: contentBoundaries, end: start + 1, truncated: true}, nil
}

func (fitter *inputFitter) truncate(token embeddingToken, limits chunkLimits, ordinal int) (GeneratedEmbeddingInput, []TokenBoundary, error) {
	if token.end-token.start > maxTruncationSearchRunes {
		return GeneratedEmbeddingInput{}, nil, errors.New("indivisible token exceeds bounded truncation search")
	}
	byteEnd, err := fitter.maximalTruncatedByteFit(token, limits)
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, err
	}
	if byteEnd == 0 {
		return GeneratedEmbeddingInput{}, nil, errors.New("provider or aggregate limits cannot hold attachment context and one source rune")
	}
	prefix := func(end int) []embeddingToken {
		candidate := token
		candidate.end = end
		return []embeddingToken{candidate}
	}
	input, contentBoundaries, err := fitter.make(prefix(byteEnd), limits, ordinal)
	if err == nil {
		return input, contentBoundaries, nil
	}
	if !fittingLimitError(err) {
		return GeneratedEmbeddingInput{}, nil, err
	}
	bestEnd, err := fitter.truncatedTokenFit(token.start+1, byteEnd-1, prefix, limits, ordinal)
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, err
	}
	if bestEnd == 0 {
		return GeneratedEmbeddingInput{}, nil, errors.New("provider or aggregate limits cannot hold attachment context and one source rune")
	}
	validated, validatedBoundaries, err := fitter.make(prefix(bestEnd), limits, ordinal)
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, fmt.Errorf("validate truncated embedding input: %w", err)
	}
	return validated, validatedBoundaries, nil
}

func (fitter *inputFitter) truncatedTokenFit(low, high int, prefix func(int) []embeddingToken, limits chunkLimits, ordinal int) (int, error) {
	if fitter.monotonic {
		bestEnd := 0
		for low <= high {
			candidateEnd := low + (high-low)/2
			_, _, err := fitter.make(prefix(candidateEnd), limits, ordinal)
			if err == nil {
				bestEnd = candidateEnd
				low = candidateEnd + 1
				continue
			}
			if !fittingLimitError(err) {
				return 0, err
			}
			high = candidateEnd - 1
		}
		return bestEnd, nil
	}
	for candidateEnd := high; candidateEnd >= low; candidateEnd-- {
		_, _, err := fitter.make(prefix(candidateEnd), limits, ordinal)
		if err == nil {
			return candidateEnd, nil
		}
		if !fittingLimitError(err) {
			return 0, err
		}
	}
	return 0, nil
}

func (fitter *inputFitter) maximalTruncatedByteFit(token embeddingToken, limits chunkLimits) (int, error) {
	low, high := token.start+1, token.end-1
	bestEnd := 0
	for low <= high {
		candidateEnd := low + (high-low)/2
		candidate := token
		candidate.end = candidateEnd
		fits, err := fitter.fitsByteLimits([]embeddingToken{candidate}, limits)
		if err != nil {
			return 0, err
		}
		if fits {
			bestEnd = candidateEnd
			low = candidateEnd + 1
		} else {
			high = candidateEnd - 1
		}
	}
	return bestEnd, nil
}

// make builds one exact input from a token range: it reconstructs the content
// from source runes, re-tokenizes it against the content budget, renders the
// document envelope, and re-counts the rendered input against provider limits.
func (fitter *inputFitter) make(tokens []embeddingToken, limits chunkLimits, ordinal int) (GeneratedEmbeddingInput, []TokenBoundary, error) {
	span, heading, err := fitter.tokenMetadata(tokens)
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, err
	}
	encoder := fitter.policy.ModelInput.Document
	contentBytes, err := fitter.contentByteLength(span, limits.contentBytes)
	if err != nil || renderedDocumentByteLength(encoder, fitter.attachment, contentBytes) > limits.renderedBytes {
		return GeneratedEmbeddingInput{}, nil, errProviderInputLimit
	}
	contentRunes := int64(tokens[len(tokens)-1].end - tokens[0].start)
	if err := fitter.work.consume(
		[]int64{contentBytes, contextualizedDocumentByteLength(fitter.attachment, contentBytes), renderedDocumentByteLength(encoder, fitter.attachment, contentBytes)},
		[]int64{contentRunes, contextualizedDocumentRuneLength(fitter.attachment, contentRunes), renderedDocumentRuneLength(encoder, fitter.attachment, contentRunes)},
	); err != nil {
		return GeneratedEmbeddingInput{}, nil, err
	}
	content, _ := runeRange(fitter.evidence.Units[span.UnitIndex].Text, span.CharStart, span.CharEnd)
	contentBoundaries, err := fitter.policy.Tokenizer.Tokenize(content, limits.contentTokens)
	if errors.Is(err, ErrTokenizerLimit) {
		return GeneratedEmbeddingInput{}, nil, errContentTokenLimit
	}
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, fmt.Errorf("tokenize exact embedding content: %w", err)
	}
	if err := validateTokenBoundaries(contentBoundaries, utf8.RuneCountInString(content), limits.contentTokens); err != nil {
		if errors.Is(err, ErrTokenizerLimit) {
			return GeneratedEmbeddingInput{}, nil, errContentTokenLimit
		}
		return GeneratedEmbeddingInput{}, nil, fmt.Errorf("validate exact embedding content tokens: %w", err)
	}
	rendered := fitter.policy.ModelInput.EncodeDocument(contextualizeDocumentInput(fitter.attachment, content))
	renderedTokens, err := countRenderedTokens(fitter.policy.Tokenizer, rendered, limits.renderedTokens)
	if errors.Is(err, ErrTokenizerLimit) || int64(len(rendered)) > limits.renderedBytes {
		return GeneratedEmbeddingInput{}, nil, errProviderInputLimit
	}
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, fmt.Errorf("tokenize rendered embedding input: %w", err)
	}
	checksum := sha256Hex([]byte(rendered))
	return GeneratedEmbeddingInput{
		Key: generatedInputKey(ordinal, checksum), Content: content, Rendered: rendered,
		ContentTokens: len(contentBoundaries), RenderedTokens: renderedTokens, Checksum: checksum,
		HeadingPath: heading, SourceSpan: span,
	}, contentBoundaries, nil
}

func (fitter *inputFitter) tokenMetadata(tokens []embeddingToken) (ChunkSpan, []string, error) {
	if len(tokens) == 0 {
		return ChunkSpan{}, nil, errors.New("embedding input requires at least one token")
	}
	first, last := tokens[0], tokens[len(tokens)-1]
	if first.unitIndex != last.unitIndex || first.unitIndex < 0 || first.unitIndex >= len(fitter.evidence.Units) || first.start < 0 || last.end <= first.start {
		return ChunkSpan{}, nil, errors.New("embedding input tokens cross natural units")
	}
	span := ChunkSpan{UnitIndex: first.unitIndex, CharStart: first.start, CharEnd: last.end}
	return span, slices.Clone(fitter.evidence.Units[first.unitIndex].HeadingPath), nil
}

func (fitter *inputFitter) contentByteLength(span ChunkSpan, limit int64) (int64, error) {
	part, ok := runeRange(fitter.evidence.Units[span.UnitIndex].Text, span.CharStart, span.CharEnd)
	if !ok || int64(len(part)) > limit {
		return 0, errProviderInputLimit
	}
	return int64(len(part)), nil
}

func (budget *fittingWorkBudget) consume(byteParts, tokenParts []int64) error {
	bytes, ok := boundedWorkTotal(byteParts, budget.remainingBytes)
	if !ok {
		return errors.New("embedding fitting work byte budget exceeded")
	}
	tokens, ok := boundedWorkTotal(tokenParts, budget.remainingTokens)
	if !ok {
		return errors.New("embedding fitting work token budget exceeded")
	}
	budget.remainingBytes -= bytes
	budget.remainingTokens -= tokens
	return nil
}

func boundedWorkTotal(parts []int64, limit int64) (int64, bool) {
	var total int64
	for _, part := range parts {
		if !addWithinAggregate(total, part, limit) {
			return 0, false
		}
		total += part
	}
	return total, true
}

func runeRange(value string, runeStart, runeEnd int) (string, bool) {
	byteStart, byteEnd := -1, -1
	runeIndex := 0
	for byteIndex := range value {
		if runeIndex == runeStart {
			byteStart = byteIndex
		}
		if runeIndex == runeEnd {
			byteEnd = byteIndex
			break
		}
		runeIndex++
	}
	if runeStart == runeIndex && byteStart < 0 {
		byteStart = len(value)
	}
	if runeEnd == runeIndex && byteEnd < 0 {
		byteEnd = len(value)
	}
	if byteStart < 0 || byteEnd < byteStart {
		return "", false
	}
	return value[byteStart:byteEnd], true
}

func renderedDocumentByteLength(encoder ModelInputEncoder, attachment AttachmentContextSnapshot, contentBytes int64) int64 {
	if !strings.Contains(encoder.Template, modelInputContentSlot) {
		return math.MaxInt64
	}
	return int64(len(encoder.Template)-len(modelInputContentSlot)) + contextualizedDocumentByteLength(attachment, contentBytes)
}

func renderedDocumentRuneLength(encoder ModelInputEncoder, attachment AttachmentContextSnapshot, contentRunes int64) int64 {
	return int64(utf8.RuneCountInString(encoder.Template)-utf8.RuneCountInString(modelInputContentSlot)) + contextualizedDocumentRuneLength(attachment, contentRunes)
}

func contextualizedDocumentByteLength(attachment AttachmentContextSnapshot, contentBytes int64) int64 {
	return contentBytes + int64(len(contextualizeDocumentInput(attachment, "")))
}

func contextualizedDocumentRuneLength(attachment AttachmentContextSnapshot, contentRunes int64) int64 {
	return contentRunes + int64(utf8.RuneCountInString(contextualizeDocumentInput(attachment, "")))
}

// contextualizeDocumentInput prefixes declared human-authored context. The
// prefix is fixed by EmbeddingInputGenerationVersion.
func contextualizeDocumentInput(attachment AttachmentContextSnapshot, content string) string {
	if attachment == (AttachmentContextSnapshot{}) {
		return content
	}
	var contextualized strings.Builder
	if attachment.Title != "" {
		contextualized.WriteString("Title: ")
		contextualized.WriteString(attachment.Title)
		contextualized.WriteByte('\n')
	}
	if attachment.Context != "" {
		contextualized.WriteString("Context: ")
		contextualized.WriteString(attachment.Context)
		contextualized.WriteByte('\n')
	}
	contextualized.WriteByte('\n')
	contextualized.WriteString(content)
	return contextualized.String()
}

func countRenderedTokens(tokenizer Tokenizer, rendered string, limit int) (int, error) {
	tokens, err := tokenizer.Tokenize(rendered, limit)
	if err != nil {
		return 0, err
	}
	if err := validateTokenBoundaries(tokens, utf8.RuneCountInString(rendered), limit); err != nil {
		return 0, err
	}
	return len(tokens), nil
}
