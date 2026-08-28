package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	EmbeddingInputGenerationVersionV1 = 1
	EmbeddingInputGenerationVersion   = 2

	maxAttachmentTitleBytes       = 512
	maxAttachmentContextBytes     = 4096
	maxGeneratedInputs            = 100_000
	maxInputFormatterBytes        = 128
	maxTruncationSearchRunes      = 4096
	maxNonMonotonicFitChecks      = 4096
	maxGenerationTotalTokens      = int64(10_000_000)
	maxGenerationTotalBytes       = int64(1 << 30)
	maxGenerationEncodedBytes     = int64(16 << 30)
	maxGenerationWorkTokens       = int64(100_000_000)
	maxGenerationWorkBytes        = int64(16 << 30)
	maxGenerationJSONIntegerBytes = 20
)

// TruncationPolicy declares what happens when one tokenizer token cannot fit
// the provider's complete rendered request boundary.
type TruncationPolicy string

const (
	TruncationPolicyReject              TruncationPolicy = "reject_indivisible"
	TruncationPolicyTruncateIndivisible TruncationPolicy = "truncate_indivisible"
	// TruncateIndivisibleAtom is retained as the provisional E2 spelling.
	TruncateIndivisibleAtom = TruncationPolicyTruncateIndivisible
)

// AttachmentContextSnapshotConfig is explicitly supplied human-authored
// context. Paths, collection names, and other mutable navigation metadata do
// not belong here.
type AttachmentContextSnapshotConfig struct {
	Title   string
	Context string
}

// AttachmentContextSnapshot is immutable after construction: its bounded
// strings are private and only value accessors are exposed.
type AttachmentContextSnapshot struct {
	title   string
	context string
}

// NewAttachmentContextSnapshot validates and seals human-authored attachment
// context. An all-empty snapshot is not a declared attachment context.
func NewAttachmentContextSnapshot(config AttachmentContextSnapshotConfig) (AttachmentContextSnapshot, error) {
	if config.Title == "" && config.Context == "" {
		return AttachmentContextSnapshot{}, errors.New("attachment context snapshot must contain a title or context")
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{{"title", config.Title, maxAttachmentTitleBytes}, {"context", config.Context, maxAttachmentContextBytes}} {
		if !utf8.ValidString(field.value) || len(field.value) > field.limit || strings.ContainsAny(field.value, "\x00\r") || !norm.NFC.IsNormalString(field.value) {
			return AttachmentContextSnapshot{}, fmt.Errorf("attachment context %s must be bounded valid UTF-8 NFC text", field.name)
		}
	}
	return AttachmentContextSnapshot{title: config.Title, context: config.Context}, nil
}

func (snapshot AttachmentContextSnapshot) Title() string   { return snapshot.title }
func (snapshot AttachmentContextSnapshot) Context() string { return snapshot.context }
func (snapshot AttachmentContextSnapshot) declared() bool {
	return snapshot.title != "" || snapshot.context != ""
}

func (snapshot AttachmentContextSnapshot) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Title   string `json:"title,omitempty"`
		Context string `json:"context,omitempty"`
	}{snapshot.title, snapshot.context})
}

func (snapshot *AttachmentContextSnapshot) UnmarshalJSON(data []byte) error {
	var encoded struct {
		Title   string `json:"title,omitempty"`
		Context string `json:"context,omitempty"`
	}
	if err := decodeStrictJSON(data, &encoded); err != nil {
		return fmt.Errorf("decode attachment context: %w", err)
	}
	canonical, err := NewAttachmentContextSnapshot(AttachmentContextSnapshotConfig{Title: encoded.Title, Context: encoded.Context})
	if err != nil {
		return err
	}
	*snapshot = canonical
	return nil
}

// InputPolicy seals one deterministic, document-role input generation.
type InputPolicy struct {
	Tokenizer                  Tokenizer
	ContentTokenBudget         int
	OverlapTokens              int
	MaxProviderTokens          int
	MaxProviderBytes           int64
	MaxGeneratedInputs         int
	MaxTotalContentTokens      int64
	MaxTotalRenderedTokens     int64
	MaxTotalContentBytes       int64
	MaxTotalRenderedBytes      int64
	MaxFittingWorkTokens       int64
	MaxFittingWorkBytes        int64
	ModelInput                 ModelInputContract
	Formatter                  string
	LexicalEvidenceFingerprint string
	ContextFingerprint         string
	AttachmentContext          *AttachmentContextSnapshot
	TruncationPolicy           TruncationPolicy
}

// GeneratedEmbeddingInput is one exact provider-ready document input.
type GeneratedEmbeddingInput struct {
	Key            string      `json:"key"`
	Content        string      `json:"content"`
	Rendered       string      `json:"rendered"`
	ContentTokens  int         `json:"content_tokens"`
	RenderedTokens int         `json:"rendered_tokens"`
	Checksum       string      `json:"checksum"`
	HeadingPaths   [][]string  `json:"heading_paths,omitempty"`
	SourceSpans    []ChunkSpan `json:"source_spans"`
	Truncated      bool        `json:"truncated"`
}

type generatedEmbeddingInputJSON struct {
	Key            string                    `json:"key"`
	Content        string                    `json:"content"`
	Rendered       string                    `json:"rendered"`
	ContentTokens  int                       `json:"content_tokens"`
	RenderedTokens int                       `json:"rendered_tokens"`
	Checksum       string                    `json:"checksum"`
	HeadingPaths   [][]string                `json:"heading_paths,omitempty"`
	SourceSpans    []generatedSourceSpanJSON `json:"source_spans"`
	Truncated      bool                      `json:"truncated"`
}

type generatedSourceSpanJSON struct {
	UnitIndex int `json:"unit_index"`
	CharStart int `json:"char_start"`
	CharEnd   int `json:"char_end"`
}

// MarshalJSON gives generated inputs a stable wire contract without changing
// the deliberately unserialized legacy ChunkSpan contract.
func (input GeneratedEmbeddingInput) MarshalJSON() ([]byte, error) {
	encoded := generatedEmbeddingInputJSON{
		Key: input.Key, Content: input.Content, Rendered: input.Rendered,
		ContentTokens: input.ContentTokens, RenderedTokens: input.RenderedTokens,
		Checksum: input.Checksum, HeadingPaths: input.HeadingPaths, Truncated: input.Truncated,
		SourceSpans: make([]generatedSourceSpanJSON, len(input.SourceSpans)),
	}
	for index, span := range input.SourceSpans {
		encoded.SourceSpans[index] = generatedSourceSpanJSON(span)
	}
	return json.Marshal(encoded)
}

func (input *GeneratedEmbeddingInput) UnmarshalJSON(data []byte) error {
	var encoded generatedEmbeddingInputJSON
	if err := decodeStrictJSON(data, &encoded); err != nil {
		return fmt.Errorf("decode generated embedding input: %w", err)
	}
	input.Key = encoded.Key
	input.Content = encoded.Content
	input.Rendered = encoded.Rendered
	input.ContentTokens = encoded.ContentTokens
	input.RenderedTokens = encoded.RenderedTokens
	input.Checksum = encoded.Checksum
	input.HeadingPaths = cloneHeadingPaths(encoded.HeadingPaths)
	input.Truncated = encoded.Truncated
	input.SourceSpans = make([]ChunkSpan, len(encoded.SourceSpans))
	for index, span := range encoded.SourceSpans {
		input.SourceSpans[index] = ChunkSpan(span)
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
			Text: contextualized, HeadingPath: slices.Clone(input.HeadingPaths[0]), SourceSpans: slices.Clone(input.SourceSpans),
		}
	}
	return result, nil
}

// EmbeddingInputGeneration is one ordered projection of normalized evidence.
// Callers persist its exact JSON or clone it before exposing mutable slices.
type EmbeddingInputGeneration struct {
	Version                    int                        `json:"version"`
	Checksum                   string                     `json:"checksum"`
	PolicyFingerprint          string                     `json:"policy_fingerprint"`
	EvidenceChecksum           string                     `json:"evidence_checksum"`
	TokenizerIdentity          TokenizerIdentity          `json:"tokenizer_identity"`
	LexicalEvidenceFingerprint string                     `json:"lexical_evidence_fingerprint"`
	Formatter                  string                     `json:"formatter"`
	ModelInputFingerprint      string                     `json:"model_input_fingerprint"`
	ContentTokenBudget         int                        `json:"content_token_budget"`
	OverlapTokens              int                        `json:"overlap_tokens"`
	TruncationPolicy           TruncationPolicy           `json:"truncation_policy"`
	ContextFingerprint         string                     `json:"context_fingerprint"`
	AttachmentContext          *AttachmentContextSnapshot `json:"attachment_context,omitempty"`
	TotalContentTokens         int64                      `json:"total_content_tokens"`
	TotalRenderedTokens        int64                      `json:"total_rendered_tokens"`
	TotalContentBytes          int64                      `json:"total_content_bytes"`
	TotalRenderedBytes         int64                      `json:"total_rendered_bytes"`
	Inputs                     []GeneratedEmbeddingInput  `json:"inputs"`
}

// embeddingInputGenerationV1 is the exact published E2 wire contract. It is
// retained as a decode and checksum boundary; the policy-complete fields added
// for catalog authority belong only to v2.
type embeddingInputGenerationV1 struct {
	Version                    int                        `json:"version"`
	Checksum                   string                     `json:"checksum"`
	PolicyFingerprint          string                     `json:"policy_fingerprint"`
	EvidenceChecksum           string                     `json:"evidence_checksum"`
	TokenizerIdentity          TokenizerIdentity          `json:"tokenizer_identity"`
	LexicalEvidenceFingerprint string                     `json:"lexical_evidence_fingerprint"`
	Formatter                  string                     `json:"formatter"`
	ModelInputFingerprint      string                     `json:"model_input_fingerprint"`
	AttachmentContext          *AttachmentContextSnapshot `json:"attachment_context,omitempty"`
	TotalContentTokens         int64                      `json:"total_content_tokens"`
	TotalRenderedTokens        int64                      `json:"total_rendered_tokens"`
	TotalContentBytes          int64                      `json:"total_content_bytes"`
	TotalRenderedBytes         int64                      `json:"total_rendered_bytes"`
	Inputs                     []GeneratedEmbeddingInput  `json:"inputs"`
}

type embeddingInputGenerationV2 EmbeddingInputGeneration

func (generation EmbeddingInputGeneration) MarshalJSON() ([]byte, error) {
	if generation.Version == EmbeddingInputGenerationVersionV1 {
		return json.Marshal(embeddingInputGenerationV1{
			Version: generation.Version, Checksum: generation.Checksum,
			PolicyFingerprint: generation.PolicyFingerprint, EvidenceChecksum: generation.EvidenceChecksum,
			TokenizerIdentity: generation.TokenizerIdentity, LexicalEvidenceFingerprint: generation.LexicalEvidenceFingerprint,
			Formatter: generation.Formatter, ModelInputFingerprint: generation.ModelInputFingerprint,
			AttachmentContext: generation.AttachmentContext, TotalContentTokens: generation.TotalContentTokens,
			TotalRenderedTokens: generation.TotalRenderedTokens, TotalContentBytes: generation.TotalContentBytes,
			TotalRenderedBytes: generation.TotalRenderedBytes, Inputs: generation.Inputs,
		})
	}
	return json.Marshal(embeddingInputGenerationV2(generation))
}

func (generation *EmbeddingInputGeneration) UnmarshalJSON(data []byte) error {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	switch header.Version {
	case EmbeddingInputGenerationVersionV1:
		var encoded embeddingInputGenerationV1
		if err := decodeStrictJSON(data, &encoded); err != nil {
			return err
		}
		*generation = EmbeddingInputGeneration{
			Version: encoded.Version, Checksum: encoded.Checksum,
			PolicyFingerprint: encoded.PolicyFingerprint, EvidenceChecksum: encoded.EvidenceChecksum,
			TokenizerIdentity: encoded.TokenizerIdentity, LexicalEvidenceFingerprint: encoded.LexicalEvidenceFingerprint,
			Formatter: encoded.Formatter, ModelInputFingerprint: encoded.ModelInputFingerprint,
			AttachmentContext: encoded.AttachmentContext, TotalContentTokens: encoded.TotalContentTokens,
			TotalRenderedTokens: encoded.TotalRenderedTokens, TotalContentBytes: encoded.TotalContentBytes,
			TotalRenderedBytes: encoded.TotalRenderedBytes, Inputs: encoded.Inputs,
		}
		return nil
	case EmbeddingInputGenerationVersion:
		var encoded embeddingInputGenerationV2
		if err := decodeStrictJSON(data, &encoded); err != nil {
			return err
		}
		*generation = EmbeddingInputGeneration(encoded)
		return nil
	default:
		return fmt.Errorf("embedding input generation version must be %d or %d", EmbeddingInputGenerationVersionV1, EmbeddingInputGenerationVersion)
	}
}

// EmbeddingInputGenerationDecodeBounds authorizes the encoded artifact and
// its allocations before canonical decoding begins.
type EmbeddingInputGenerationDecodeBounds struct {
	MaxEncodedBytes     int64
	MaxInputs           int
	MaxObjectFields     int
	MaxStringBytes      int64
	MaxTotalStringBytes int64
}

// DecodeEmbeddingInputGeneration strictly decodes and validates the complete
// sealed generation. Unknown fields, lossy spans, forged totals, and stale
// checksums are rejected.
func DecodeEmbeddingInputGeneration(data []byte, bounds EmbeddingInputGenerationDecodeBounds) (EmbeddingInputGeneration, error) {
	if err := preflightEmbeddingInputGenerationJSON(data, bounds); err != nil {
		return EmbeddingInputGeneration{}, fmt.Errorf("preflight embedding input generation: %w", err)
	}
	var generation EmbeddingInputGeneration
	if err := decodeStrictJSON(data, &generation); err != nil {
		return EmbeddingInputGeneration{}, fmt.Errorf("decode embedding input generation: %w", err)
	}
	if err := validateEmbeddingInputGeneration(generation); err != nil {
		return EmbeddingInputGeneration{}, err
	}
	return generation, nil
}

func preflightEmbeddingInputGenerationJSON(data []byte, bounds EmbeddingInputGenerationDecodeBounds) error {
	if bounds.MaxEncodedBytes < 1 || bounds.MaxEncodedBytes > maxGenerationEncodedBytes || int64(len(data)) > bounds.MaxEncodedBytes {
		return errors.New("embedding generation encoded bytes exceed bounds")
	}
	if bounds.MaxInputs < 1 || bounds.MaxInputs > maxGeneratedInputs {
		return errors.New("embedding generation input decode bound is invalid")
	}
	if bounds.MaxObjectFields < 1 || bounds.MaxObjectFields > 64 {
		return errors.New("embedding generation object field decode bound is invalid")
	}
	if bounds.MaxStringBytes < 1 || bounds.MaxStringBytes > bounds.MaxEncodedBytes || bounds.MaxTotalStringBytes < 1 || bounds.MaxTotalStringBytes > bounds.MaxEncodedBytes {
		return errors.New("embedding generation string decode bounds are invalid")
	}
	scanner := generationJSONPreflight{data: data, bounds: bounds}
	if err := scanner.value(generationJSONPathRoot, 0); err != nil {
		return err
	}
	scanner.skipSpace()
	if scanner.position != len(data) {
		return errors.New("JSON contains trailing value")
	}
	return nil
}

type generationJSONPreflight struct {
	data           []byte
	bounds         EmbeddingInputGenerationDecodeBounds
	position       int
	rawStringBytes int64
}

type generationJSONPath uint8

const (
	generationJSONPathOther generationJSONPath = iota
	generationJSONPathRoot
	generationJSONPathInputs
	generationJSONPathInput
	generationJSONPathHeadingPaths
	generationJSONPathHeadingPath
	generationJSONPathSourceSpans
)

func (scanner *generationJSONPreflight) value(path generationJSONPath, depth int) error {
	if depth > 16 {
		return errors.New("embedding generation JSON nesting exceeds bounds")
	}
	scanner.skipSpace()
	if scanner.position >= len(scanner.data) {
		return io.ErrUnexpectedEOF
	}
	switch scanner.data[scanner.position] {
	case '{':
		return scanner.object(path, depth)
	case '[':
		return scanner.array(path, depth)
	case '"':
		_, _, _, err := scanner.stringValue()
		return err
	case 't':
		return scanner.keyword("true")
	case 'f':
		return scanner.keyword("false")
	case 'n':
		return scanner.keyword("null")
	default:
		return scanner.number()
	}
}

func (scanner *generationJSONPreflight) object(path generationJSONPath, depth int) error {
	scanner.position++
	scanner.skipSpace()
	if scanner.consume('}') {
		return nil
	}
	for fields := 1; ; fields++ {
		if fields > scanner.bounds.MaxObjectFields {
			return errors.New("embedding generation JSON object fields exceed bounds")
		}
		scanner.skipSpace()
		keyStart, keyEnd, escaped, err := scanner.stringValue()
		if err != nil {
			return err
		}
		if escaped || !asciiJSONField(scanner.data[keyStart:keyEnd]) {
			return errors.New("embedding generation JSON field names must be unescaped ASCII")
		}
		scanner.skipSpace()
		if !scanner.consume(':') {
			return errors.New("embedding generation JSON object field is missing a colon")
		}
		childPath := generationJSONChildPath(path, scanner.data[keyStart:keyEnd])
		if err := scanner.value(childPath, depth+1); err != nil {
			return err
		}
		scanner.skipSpace()
		if scanner.consume('}') {
			return nil
		}
		if !scanner.consume(',') {
			return errors.New("embedding generation JSON object is not closed")
		}
	}
}

func (scanner *generationJSONPreflight) array(path generationJSONPath, depth int) error {
	scanner.position++
	scanner.skipSpace()
	if scanner.consume(']') {
		return nil
	}
	limit := scanner.arrayLimit(path)
	for count := 1; ; count++ {
		if count > limit {
			return fmt.Errorf("embedding generation JSON %s collection exceeds bounds", generationJSONPathName(path))
		}
		if err := scanner.value(generationJSONArrayChildPath(path), depth+1); err != nil {
			return err
		}
		scanner.skipSpace()
		if scanner.consume(']') {
			return nil
		}
		if !scanner.consume(',') {
			return errors.New("embedding generation JSON array is not closed")
		}
	}
}

func (scanner *generationJSONPreflight) stringValue() (int, int, bool, error) {
	if !scanner.consume('"') {
		return 0, 0, false, errors.New("embedding generation JSON object key is invalid")
	}
	start := scanner.position
	escaped := false
	var rawLength int64
	for scanner.position < len(scanner.data) {
		value := scanner.data[scanner.position]
		if value == '"' {
			end := scanner.position
			scanner.position++
			return start, end, escaped, nil
		}
		if value < 0x20 {
			return 0, 0, false, errors.New("embedding generation JSON string contains a control byte")
		}
		if err := scanner.addRawStringByte(&rawLength); err != nil {
			return 0, 0, false, err
		}
		scanner.position++
		if value != '\\' {
			continue
		}
		escaped = true
		if scanner.position >= len(scanner.data) {
			return 0, 0, false, io.ErrUnexpectedEOF
		}
		escape := scanner.data[scanner.position]
		if err := scanner.addRawStringByte(&rawLength); err != nil {
			return 0, 0, false, err
		}
		scanner.position++
		if escape == 'u' {
			for range 4 {
				if scanner.position >= len(scanner.data) {
					return 0, 0, false, io.ErrUnexpectedEOF
				}
				if !isJSONHex(scanner.data[scanner.position]) {
					return 0, 0, false, errors.New("embedding generation JSON unicode escape is invalid")
				}
				if err := scanner.addRawStringByte(&rawLength); err != nil {
					return 0, 0, false, err
				}
				scanner.position++
			}
			continue
		}
		if !strings.ContainsRune(`"\\/bfnrt`, rune(escape)) {
			return 0, 0, false, errors.New("embedding generation JSON escape is invalid")
		}
	}
	return 0, 0, false, io.ErrUnexpectedEOF
}

func (scanner *generationJSONPreflight) addRawStringByte(rawLength *int64) error {
	if *rawLength >= scanner.bounds.MaxStringBytes || scanner.rawStringBytes >= scanner.bounds.MaxTotalStringBytes {
		return errors.New("embedding generation JSON raw string bytes exceed bounds")
	}
	*rawLength++
	scanner.rawStringBytes++
	return nil
}

func (scanner *generationJSONPreflight) keyword(value string) error {
	if len(scanner.data)-scanner.position < len(value) || string(scanner.data[scanner.position:scanner.position+len(value)]) != value {
		return errors.New("embedding generation JSON literal is invalid")
	}
	scanner.position += len(value)
	return nil
}

func (scanner *generationJSONPreflight) number() error {
	start := scanner.position
	if scanner.position < len(scanner.data) && scanner.data[scanner.position] == '-' {
		scanner.position++
	}
	if scanner.position >= len(scanner.data) || scanner.data[scanner.position] < '0' || scanner.data[scanner.position] > '9' {
		return errors.New("embedding generation JSON integer is invalid")
	}
	if scanner.data[scanner.position] == '0' {
		scanner.position++
		if scanner.position < len(scanner.data) && scanner.data[scanner.position] >= '0' && scanner.data[scanner.position] <= '9' {
			return errors.New("embedding generation JSON integer has a leading zero")
		}
	} else {
		for scanner.position < len(scanner.data) && scanner.data[scanner.position] >= '0' && scanner.data[scanner.position] <= '9' {
			scanner.position++
			if scanner.position-start > maxGenerationJSONIntegerBytes {
				return errors.New("embedding generation JSON integer token exceeds bounds")
			}
		}
	}
	if scanner.position-start > maxGenerationJSONIntegerBytes {
		return errors.New("embedding generation JSON integer token exceeds bounds")
	}
	if scanner.position < len(scanner.data) && !isJSONValueTerminator(scanner.data[scanner.position]) {
		return errors.New("embedding generation JSON integer grammar is invalid")
	}
	return nil
}

func (scanner *generationJSONPreflight) skipSpace() {
	for scanner.position < len(scanner.data) && strings.ContainsRune(" \t\r\n", rune(scanner.data[scanner.position])) {
		scanner.position++
	}
}

func (scanner *generationJSONPreflight) consume(value byte) bool {
	if scanner.position >= len(scanner.data) || scanner.data[scanner.position] != value {
		return false
	}
	scanner.position++
	return true
}

func (scanner *generationJSONPreflight) arrayLimit(path generationJSONPath) int {
	switch path {
	case generationJSONPathInputs:
		return scanner.bounds.MaxInputs
	case generationJSONPathHeadingPaths, generationJSONPathSourceSpans:
		return 1
	case generationJSONPathHeadingPath:
		return maxEvidenceHeadingDepth
	default:
		return max(scanner.bounds.MaxInputs, maxEvidenceHeadingDepth)
	}
}

func generationJSONChildPath(parent generationJSONPath, key []byte) generationJSONPath {
	if parent == generationJSONPathRoot && bytes.Equal(key, []byte("inputs")) {
		return generationJSONPathInputs
	}
	if parent == generationJSONPathInput {
		switch {
		case bytes.Equal(key, []byte("heading_paths")):
			return generationJSONPathHeadingPaths
		case bytes.Equal(key, []byte("source_spans")):
			return generationJSONPathSourceSpans
		}
	}
	return generationJSONPathOther
}

func generationJSONArrayChildPath(parent generationJSONPath) generationJSONPath {
	switch parent {
	case generationJSONPathInputs:
		return generationJSONPathInput
	case generationJSONPathHeadingPaths:
		return generationJSONPathHeadingPath
	default:
		return generationJSONPathOther
	}
}

func generationJSONPathName(path generationJSONPath) string {
	switch path {
	case generationJSONPathInputs:
		return "inputs"
	case generationJSONPathHeadingPaths:
		return "heading paths"
	case generationJSONPathHeadingPath:
		return "heading parts"
	case generationJSONPathSourceSpans:
		return "source spans"
	default:
		return "unknown"
	}
}

func asciiJSONField(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}

func isJSONHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func isJSONValueTerminator(value byte) bool {
	return value == ',' || value == ']' || value == '}' || strings.ContainsRune(" \t\r\n", rune(value))
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains trailing value")
		}
		return err
	}
	return nil
}

func validateEmbeddingInputGeneration(generation EmbeddingInputGeneration) error {
	if generation.Version != EmbeddingInputGenerationVersionV1 && generation.Version != EmbeddingInputGenerationVersion {
		return fmt.Errorf("embedding input generation version must be %d or %d", EmbeddingInputGenerationVersionV1, EmbeddingInputGenerationVersion)
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
	if err := validateTokenizerIdentity(generation.TokenizerIdentity); err != nil {
		return err
	}
	if err := validateCompatibilityID(generation.Formatter); err != nil {
		return fmt.Errorf("embedding generation formatter: %w", err)
	}
	if generation.AttachmentContext != nil && !generation.AttachmentContext.declared() {
		return errors.New("embedding generation attachment context is invalid")
	}
	if generation.Version == EmbeddingInputGenerationVersionV1 {
		if generation.ContentTokenBudget != 0 || generation.OverlapTokens != 0 || generation.TruncationPolicy != "" || generation.ContextFingerprint != "" {
			return errors.New("embedding generation v1 contains v2 policy authority")
		}
	} else {
		if err := validateFingerprint(generation.ContextFingerprint, "generation context fingerprint"); err != nil {
			return err
		}
		if generation.ContentTokenBudget < 1 || generation.ContentTokenBudget > maxEmbeddingChunkTokens ||
			generation.OverlapTokens < 0 || generation.OverlapTokens >= generation.ContentTokenBudget {
			return errors.New("embedding generation chunk policy is invalid")
		}
		switch generation.TruncationPolicy {
		case TruncationPolicyReject, TruncationPolicyTruncateIndivisible:
		default:
			return errors.New("embedding generation truncation policy is invalid")
		}
	}
	if len(generation.Inputs) > maxGeneratedInputs {
		return errors.New("embedding generation input count exceeds bounds")
	}
	var contentTokens, renderedTokens, contentBytes, renderedBytes int64
	for index, input := range generation.Inputs {
		if err := validateStableToken(input.Key, "generated embedding input key", 128); err != nil {
			return err
		}
		if input.Key != fmt.Sprintf("chunk-%06d-%s", index, input.Checksum[:min(12, len(input.Checksum))]) {
			return errors.New("generated embedding input key is not canonical")
		}
		if input.Content == "" || input.Rendered == "" || !utf8.ValidString(input.Content) || !utf8.ValidString(input.Rendered) {
			return errors.New("generated embedding input text is invalid")
		}
		if input.ContentTokens < 1 || input.ContentTokens > maxEmbeddingChunkTokens || input.RenderedTokens < 1 || input.RenderedTokens > maxEmbeddingChunkTokens {
			return errors.New("generated embedding input token counts are invalid")
		}
		if sha256Hex([]byte(input.Rendered)) != input.Checksum {
			return errors.New("generated embedding input checksum is invalid")
		}
		if err := validateEmbeddingInputAuxiliaries(EmbeddingInput{Role: EmbeddingRoleDocument, Kind: EmbeddingInputRenditionChunk, Text: input.Content, HeadingPath: firstHeadingPath(input.HeadingPaths), SourceSpans: input.SourceSpans}); err != nil {
			return fmt.Errorf("generated embedding input auxiliaries: %w", err)
		}
		if len(input.HeadingPaths) != 1 || len(input.SourceSpans) != 1 {
			return errors.New("generated embedding input collection canonical cardinality is invalid")
		}
		for _, span := range input.SourceSpans {
			if span.UnitIndex < 0 || span.CharStart < 0 || span.CharEnd <= span.CharStart {
				return errors.New("generated embedding input source span is invalid")
			}
		}
		if !addWithinAggregate(contentTokens, int64(input.ContentTokens), maxGenerationTotalTokens) ||
			!addWithinAggregate(renderedTokens, int64(input.RenderedTokens), maxGenerationTotalTokens) ||
			!addWithinAggregate(contentBytes, int64(len(input.Content)), maxGenerationTotalBytes) ||
			!addWithinAggregate(renderedBytes, int64(len(input.Rendered)), maxGenerationTotalBytes) {
			return errors.New("embedding generation aggregate values exceed bounds")
		}
		contentTokens += int64(input.ContentTokens)
		renderedTokens += int64(input.RenderedTokens)
		contentBytes += int64(len(input.Content))
		renderedBytes += int64(len(input.Rendered))
	}
	if generation.TotalContentTokens != contentTokens || generation.TotalRenderedTokens != renderedTokens || generation.TotalContentBytes != contentBytes || generation.TotalRenderedBytes != renderedBytes {
		return errors.New("embedding generation aggregate totals are invalid")
	}
	checksum := generation.Checksum
	generation.Checksum = ""
	if generationFingerprint(generation) != checksum {
		return errors.New("embedding input generation checksum is invalid")
	}
	return nil
}

func cloneHeadingPaths(source [][]string) [][]string {
	result := make([][]string, len(source))
	for index, heading := range source {
		result[index] = slices.Clone(heading)
	}
	return result
}

func firstHeadingPath(paths [][]string) []string {
	if len(paths) == 0 {
		return nil
	}
	return paths[0]
}

type embeddingToken struct {
	unitIndex int
	start     int
	end       int
	unitEnd   int
}

// BuildEmbeddingInputs derives provider inputs from canonical evidence only.
// It never reads a retained Markdown or YAML rendition.
func BuildEmbeddingInputs(evidence NormalizedEvidenceV1, policy InputPolicy) (EmbeddingInputGeneration, error) {
	if nilInterface(policy.Tokenizer) {
		return EmbeddingInputGeneration{}, errors.New("embedding input policy requires a tokenizer")
	}
	tokenizerIdentity := policy.Tokenizer.Identity()
	if _, checksum, err := MarshalNormalizedEvidenceV1(evidence); err != nil || checksum != evidence.Checksum {
		if err != nil {
			return EmbeddingInputGeneration{}, fmt.Errorf("validate normalized evidence: %w", err)
		}
		return EmbeddingInputGeneration{}, errors.New("normalized evidence checksum is invalid")
	}
	if err := validateInputPolicy(policy, tokenizerIdentity); err != nil {
		return EmbeddingInputGeneration{}, err
	}

	tokens, naturalEnds, err := tokenizeEvidence(evidence, policy.Tokenizer)
	if err != nil {
		return EmbeddingInputGeneration{}, err
	}
	attachment := AttachmentContextSnapshot{}
	if policy.AttachmentContext != nil {
		attachment = *policy.AttachmentContext
	}
	result := EmbeddingInputGeneration{
		Version: EmbeddingInputGenerationVersion, EvidenceChecksum: evidence.Checksum,
		TokenizerIdentity: tokenizerIdentity, LexicalEvidenceFingerprint: policy.LexicalEvidenceFingerprint,
		Formatter: policy.Formatter, ModelInputFingerprint: policy.ModelInput.Fingerprint,
		ContentTokenBudget: policy.ContentTokenBudget, OverlapTokens: policy.OverlapTokens,
		TruncationPolicy: policy.TruncationPolicy, ContextFingerprint: policy.ContextFingerprint,
		Inputs: make([]GeneratedEmbeddingInput, 0, generatedInputCapacity(policy, len(tokens))),
	}
	if attachment.declared() {
		result.AttachmentContext = &attachment
	}
	result.PolicyFingerprint = inputPolicyFingerprint(policy, attachment, tokenizerIdentity)
	work := fittingWorkBudget{remainingTokens: policy.MaxFittingWorkTokens, remainingBytes: policy.MaxFittingWorkBytes}

	for start := 0; start < len(tokens); {
		if len(result.Inputs) == policy.MaxGeneratedInputs {
			return EmbeddingInputGeneration{}, errors.New("embedding generated input limit exceeded")
		}
		limits, err := remainingInputLimits(result, policy, &work)
		if err != nil {
			return EmbeddingInputGeneration{}, err
		}
		unitEnd := tokens[start].unitEnd
		end := chooseChunkEnd(start, unitEnd, policy.ContentTokenBudget, policy.OverlapTokens, naturalEnds)
		input, contentBoundaries, fittedEnd, truncated, err := fitGeneratedInput(evidence, tokens, naturalEnds, start, end, policy, tokenizerIdentity, attachment, limits, len(result.Inputs))
		if err != nil {
			return EmbeddingInputGeneration{}, err
		}
		input.Truncated = truncated
		if err := addGeneratedInputTotals(&result, input, policy); err != nil {
			return EmbeddingInputGeneration{}, err
		}
		result.Inputs = append(result.Inputs, input)
		if truncated || fittedEnd == unitEnd {
			start = fittedEnd
			continue
		}
		if policy.OverlapTokens == 0 {
			start = fittedEnd
			continue
		}
		if len(contentBoundaries) <= policy.OverlapTokens || len(input.SourceSpans) != 1 {
			if limits.contentTokens <= policy.OverlapTokens {
				return EmbeddingInputGeneration{}, errors.New("embedding generation exceeds aggregate limits while preserving configured token overlap")
			}
			return EmbeddingInputGeneration{}, errors.New("provider limits cannot preserve configured token overlap")
		}
		desiredRuneStart := input.SourceSpans[0].CharStart + contentBoundaries[len(contentBoundaries)-policy.OverlapTokens].Start
		next := start
		for next < fittedEnd && tokens[next].end <= desiredRuneStart {
			next++
		}
		if next == fittedEnd {
			return EmbeddingInputGeneration{}, errors.New("exact token overlap does not advance within the source unit")
		}
		tokens[next].start = desiredRuneStart
		start = next
	}
	if policy.Tokenizer.Identity() != tokenizerIdentity {
		return EmbeddingInputGeneration{}, errors.New("embedding tokenizer identity changed during generation")
	}
	result.Checksum = generationFingerprint(result)
	return result, nil
}

type inputConstructionLimits struct {
	contentTokens  int
	renderedTokens int
	contentBytes   int64
	renderedBytes  int64
	work           *fittingWorkBudget
}

type fittingWorkBudget struct {
	remainingTokens int64
	remainingBytes  int64
}

func remainingInputLimits(generation EmbeddingInputGeneration, policy InputPolicy, work *fittingWorkBudget) (inputConstructionLimits, error) {
	limits := inputConstructionLimits{
		contentTokens:  int(policy.MaxTotalContentTokens - generation.TotalContentTokens),
		renderedTokens: int(policy.MaxTotalRenderedTokens - generation.TotalRenderedTokens),
		contentBytes:   policy.MaxTotalContentBytes - generation.TotalContentBytes,
		renderedBytes:  policy.MaxTotalRenderedBytes - generation.TotalRenderedBytes,
		work:           work,
	}
	if limits.contentTokens < 1 || limits.renderedTokens < 1 || limits.contentBytes < 1 || limits.renderedBytes < 1 {
		return inputConstructionLimits{}, errors.New("embedding generation exceeds aggregate limits")
	}
	limits.contentTokens = min(limits.contentTokens, policy.ContentTokenBudget)
	limits.renderedTokens = min(limits.renderedTokens, policy.MaxProviderTokens)
	limits.contentBytes = min(limits.contentBytes, policy.MaxProviderBytes)
	limits.renderedBytes = min(limits.renderedBytes, policy.MaxProviderBytes)
	return limits, nil
}

func generatedInputCapacity(policy InputPolicy, tokenCount int) int {
	return min(
		policy.MaxGeneratedInputs,
		tokenCount,
		int(policy.MaxTotalContentTokens),
		int(policy.MaxTotalRenderedTokens),
		int(policy.MaxTotalContentBytes),
		int(policy.MaxTotalRenderedBytes),
		int(policy.MaxFittingWorkTokens),
		int(policy.MaxFittingWorkBytes),
	)
}

func validateInputPolicy(policy InputPolicy, tokenizerIdentity TokenizerIdentity) error {
	if err := validateTokenizerIdentity(tokenizerIdentity); err != nil {
		return err
	}
	if policy.ContentTokenBudget < 1 || policy.ContentTokenBudget > maxEmbeddingChunkTokens {
		return errors.New("embedding content token budget is invalid")
	}
	if policy.OverlapTokens < 0 || policy.OverlapTokens >= policy.ContentTokenBudget {
		return errors.New("embedding overlap token count is invalid")
	}
	if policy.MaxProviderTokens < 1 || policy.MaxProviderTokens > maxEmbeddingChunkTokens || policy.MaxProviderBytes < 1 || policy.MaxProviderBytes > maxEmbeddingInputBytes {
		return errors.New("embedding provider limits are invalid")
	}
	if policy.MaxGeneratedInputs < 1 || policy.MaxGeneratedInputs > maxGeneratedInputs {
		return errors.New("embedding generated input limit is invalid")
	}
	for _, total := range []struct {
		name  string
		value int64
		limit int64
	}{
		{"content token", policy.MaxTotalContentTokens, maxGenerationTotalTokens},
		{"rendered token", policy.MaxTotalRenderedTokens, maxGenerationTotalTokens},
		{"content byte", policy.MaxTotalContentBytes, maxGenerationTotalBytes},
		{"rendered byte", policy.MaxTotalRenderedBytes, maxGenerationTotalBytes},
		{"fitting work token", policy.MaxFittingWorkTokens, maxGenerationWorkTokens},
		{"fitting work byte", policy.MaxFittingWorkBytes, maxGenerationWorkBytes},
	} {
		if total.value < 1 || total.value > total.limit {
			return fmt.Errorf("embedding aggregate %s limit is invalid", total.name)
		}
	}
	if err := validateModelInputContract(policy.ModelInput); err != nil || policy.ModelInput.Profile == "" {
		return errors.New("embedding input policy has invalid model-input contract")
	}
	if len(policy.Formatter) > maxInputFormatterBytes {
		return errors.New("embedding input formatter is too long")
	}
	if err := validateCompatibilityID(policy.Formatter); err != nil {
		return fmt.Errorf("embedding input formatter: %w", err)
	}
	if err := validateFingerprint(policy.LexicalEvidenceFingerprint, "lexical evidence fingerprint"); err != nil {
		return err
	}
	if err := validateFingerprint(policy.ContextFingerprint, "context fingerprint"); err != nil {
		return err
	}
	if policy.AttachmentContext != nil && !policy.AttachmentContext.declared() {
		return errors.New("attachment context snapshot is not canonical")
	}
	switch policy.TruncationPolicy {
	case TruncationPolicyReject, TruncationPolicyTruncateIndivisible:
	default:
		return errors.New("embedding truncation policy is invalid")
	}
	return nil
}

func addGeneratedInputTotals(generation *EmbeddingInputGeneration, input GeneratedEmbeddingInput, policy InputPolicy) error {
	contentBytes := int64(len(input.Content))
	renderedBytes := int64(len(input.Rendered))
	if !addWithinAggregate(generation.TotalContentTokens, int64(input.ContentTokens), policy.MaxTotalContentTokens) ||
		!addWithinAggregate(generation.TotalRenderedTokens, int64(input.RenderedTokens), policy.MaxTotalRenderedTokens) ||
		!addWithinAggregate(generation.TotalContentBytes, contentBytes, policy.MaxTotalContentBytes) ||
		!addWithinAggregate(generation.TotalRenderedBytes, renderedBytes, policy.MaxTotalRenderedBytes) {
		return errors.New("embedding generation exceeds aggregate limits")
	}
	generation.TotalContentTokens += int64(input.ContentTokens)
	generation.TotalRenderedTokens += int64(input.RenderedTokens)
	generation.TotalContentBytes += contentBytes
	generation.TotalRenderedBytes += renderedBytes
	return nil
}

func addWithinAggregate(current, addition, limit int64) bool {
	return current >= 0 && addition >= 0 && limit >= 0 && current <= limit && addition <= limit-current
}

func (budget *fittingWorkBudget) consume(byteParts, tokenParts []int64) error {
	if budget == nil {
		return errors.New("embedding fitting work budget is missing")
	}
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

func tokenizeEvidence(evidence NormalizedEvidenceV1, tokenizer Tokenizer) ([]embeddingToken, map[int]struct{}, error) {
	tokens := make([]embeddingToken, 0, min(maxEmbeddingTokensPerGeneration, len(evidence.Units)))
	naturalEnds := make(map[int]struct{}, min(maxEmbeddingTokensPerGeneration, len(evidence.Units)))
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
		for _, token := range unitTokens {
			tokens = append(tokens, embeddingToken{unitIndex: unitIndex, start: token.Start, end: token.End})
		}
		for tokenIndex := base; tokenIndex < len(tokens); tokenIndex++ {
			tokens[tokenIndex].unitEnd = len(tokens)
		}
		cuts := naturalRuneEnds(unit, runeCount)
		for localIndex, token := range unitTokens {
			if _, natural := cuts[token.End]; natural {
				naturalEnds[base+localIndex+1] = struct{}{}
			}
		}
		naturalEnds[len(tokens)] = struct{}{}
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

func chooseChunkEnd(start, total, budget, overlap int, naturalEnds map[int]struct{}) int {
	hardEnd := min(total, start+budget)
	for candidate := hardEnd; candidate > start; candidate-- {
		if _, natural := naturalEnds[candidate]; natural && (candidate == total || candidate-start > overlap) {
			return candidate
		}
	}
	return hardEnd
}

func fitGeneratedInput(evidence NormalizedEvidenceV1, tokens []embeddingToken, naturalEnds map[int]struct{}, start, end int, policy InputPolicy, tokenizerIdentity TokenizerIdentity, attachment AttachmentContextSnapshot, limits inputConstructionLimits, ordinal int) (GeneratedEmbeddingInput, []TokenBoundary, int, bool, error) {
	byteEnd, err := maximalByteFit(evidence, tokens, start, end, policy, attachment, limits)
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, 0, false, err
	}
	if byteEnd == 0 {
		return truncateOrRejectGeneratedInput(evidence, tokens[start], start+1, policy, tokenizerIdentity, attachment, limits, ordinal)
	}
	preferredEnd := preferNaturalFitEnd(start, byteEnd, policy.OverlapTokens, naturalEnds)
	input, contentBoundaries, err := makeGeneratedInput(evidence, tokens[start:preferredEnd], policy, attachment, limits, ordinal)
	if err == nil {
		return input, contentBoundaries, preferredEnd, false, nil
	}
	if !fittingLimitError(err) {
		return GeneratedEmbeddingInput{}, nil, 0, false, err
	}
	end = byteEnd
	if preferredEnd != byteEnd {
		input, contentBoundaries, err = makeGeneratedInput(evidence, tokens[start:byteEnd], policy, attachment, limits, ordinal)
		if err == nil {
			return input, contentBoundaries, byteEnd, false, nil
		}
		if !fittingLimitError(err) {
			return GeneratedEmbeddingInput{}, nil, 0, false, err
		}
	}

	var bestEnd int
	if tokenizerIdentity.PrefixTokenCountsMonotonic && strings.HasSuffix(policy.ModelInput.Document.Template, modelInputContentSlot) {
		bestEnd, err = monotonicTokenFit(evidence, tokens, start, end, policy, attachment, limits, ordinal)
	} else {
		bestEnd, err = boundedNonMonotonicTokenFit(evidence, tokens, start, end, policy, attachment, limits, ordinal)
	}
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, 0, false, err
	}
	if bestEnd != 0 {
		validated, validatedBoundaries, validateErr := makeGeneratedInput(evidence, tokens[start:bestEnd], policy, attachment, limits, ordinal)
		if validateErr != nil {
			return GeneratedEmbeddingInput{}, nil, 0, false, fmt.Errorf("validate fitted embedding input: %w", validateErr)
		}
		return validated, validatedBoundaries, bestEnd, false, nil
	}
	return truncateOrRejectGeneratedInput(evidence, tokens[start], start+1, policy, tokenizerIdentity, attachment, limits, ordinal)
}

func maximalByteFit(evidence NormalizedEvidenceV1, tokens []embeddingToken, start, end int, policy InputPolicy, attachment AttachmentContextSnapshot, limits inputConstructionLimits) (int, error) {
	low, high := start+1, end
	bestEnd := 0
	for low <= high {
		candidate := low + (high-low)/2
		fits, err := candidateFitsByteLimits(evidence, tokens[start:candidate], policy, attachment, limits)
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

func candidateFitsByteLimits(evidence NormalizedEvidenceV1, tokens []embeddingToken, policy InputPolicy, attachment AttachmentContextSnapshot, limits inputConstructionLimits) (bool, error) {
	spans, _, err := tokenMetadata(evidence, tokens)
	if err != nil {
		return false, err
	}
	contentBytes, err := contentByteLength(evidence, spans, limits.contentBytes)
	if errors.Is(err, errProviderInputLimit) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return renderedDocumentByteLength(policy.ModelInput.Document, attachment, contentBytes) <= limits.renderedBytes, nil
}

func monotonicTokenFit(evidence NormalizedEvidenceV1, tokens []embeddingToken, start, end int, policy InputPolicy, attachment AttachmentContextSnapshot, limits inputConstructionLimits, ordinal int) (int, error) {
	low, high := start+1, end-1
	bestEnd := 0
	for low <= high {
		candidate := low + (high-low)/2
		_, _, err := makeGeneratedInput(evidence, tokens[start:candidate], policy, attachment, limits, ordinal)
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

func boundedNonMonotonicTokenFit(evidence NormalizedEvidenceV1, tokens []embeddingToken, start, end int, policy InputPolicy, attachment AttachmentContextSnapshot, limits inputConstructionLimits, ordinal int) (int, error) {
	lower := max(start+1, end-maxNonMonotonicFitChecks)
	for candidate := end - 1; candidate >= lower; candidate-- {
		_, _, err := makeGeneratedInput(evidence, tokens[start:candidate], policy, attachment, limits, ordinal)
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

func truncateOrRejectGeneratedInput(evidence NormalizedEvidenceV1, token embeddingToken, fittedEnd int, policy InputPolicy, tokenizerIdentity TokenizerIdentity, attachment AttachmentContextSnapshot, limits inputConstructionLimits, ordinal int) (GeneratedEmbeddingInput, []TokenBoundary, int, bool, error) {
	if policy.TruncationPolicy == TruncationPolicyReject {
		return GeneratedEmbeddingInput{}, nil, 0, false, errors.New("indivisible token exceeds content budget, provider limits, or aggregate limits")
	}
	input, contentBoundaries, err := truncateGeneratedInput(evidence, token, policy, tokenizerIdentity, attachment, limits, ordinal)
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, 0, false, err
	}
	return input, contentBoundaries, fittedEnd, true, nil
}

func preferNaturalFitEnd(start, fittedEnd, overlap int, naturalEnds map[int]struct{}) int {
	for candidate := fittedEnd; candidate > start; candidate-- {
		if _, natural := naturalEnds[candidate]; natural && candidate-start > overlap {
			return candidate
		}
	}
	return fittedEnd
}

var errProviderInputLimit = errors.New("provider input limit")
var errContentTokenLimit = errors.New("content token limit")

func fittingLimitError(err error) bool {
	return errors.Is(err, errProviderInputLimit) || errors.Is(err, errContentTokenLimit)
}

func makeGeneratedInput(evidence NormalizedEvidenceV1, tokens []embeddingToken, policy InputPolicy, attachment AttachmentContextSnapshot, limits inputConstructionLimits, ordinal int) (GeneratedEmbeddingInput, []TokenBoundary, error) {
	spans, headings, err := tokenMetadata(evidence, tokens)
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, err
	}
	contentBytes, err := contentByteLength(evidence, spans, limits.contentBytes)
	if err != nil || renderedDocumentByteLength(policy.ModelInput.Document, attachment, contentBytes) > limits.renderedBytes {
		return GeneratedEmbeddingInput{}, nil, errProviderInputLimit
	}
	contentRunes := int64(tokens[len(tokens)-1].end - tokens[0].start)
	contextualizedBytes := contextualizedDocumentByteLength(attachment, contentBytes)
	renderedBytes := renderedDocumentByteLength(policy.ModelInput.Document, attachment, contentBytes)
	contextualizedRunes := contextualizedDocumentRuneLength(attachment, contentRunes)
	renderedRunes := renderedDocumentRuneLength(policy.ModelInput.Document, attachment, contentRunes)
	if err := limits.work.consume(
		[]int64{contentBytes, contextualizedBytes, renderedBytes},
		[]int64{contentRunes, contextualizedRunes, renderedRunes},
	); err != nil {
		return GeneratedEmbeddingInput{}, nil, err
	}
	content := contentForSpans(evidence, spans, contentBytes)
	contentBoundaries, err := policy.Tokenizer.Tokenize(content, limits.contentTokens)
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
	contextualized := contextualizeDocumentInput(attachment, content)
	rendered := policy.ModelInput.EncodeDocument(contextualized)
	renderedTokens, err := countRenderedTokens(policy.Tokenizer, rendered, limits.renderedTokens)
	if errors.Is(err, ErrTokenizerLimit) || int64(len(rendered)) > limits.renderedBytes {
		return GeneratedEmbeddingInput{}, nil, errProviderInputLimit
	}
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, fmt.Errorf("tokenize rendered embedding input: %w", err)
	}
	checksum := sha256Hex([]byte(rendered))
	return GeneratedEmbeddingInput{
		Key: fmt.Sprintf("chunk-%06d-%s", ordinal, checksum[:12]), Content: content,
		Rendered:      rendered,
		ContentTokens: len(contentBoundaries), RenderedTokens: renderedTokens, Checksum: checksum,
		HeadingPaths: headings, SourceSpans: spans,
	}, contentBoundaries, nil
}

func truncateGeneratedInput(evidence NormalizedEvidenceV1, token embeddingToken, policy InputPolicy, tokenizerIdentity TokenizerIdentity, attachment AttachmentContextSnapshot, limits inputConstructionLimits, ordinal int) (GeneratedEmbeddingInput, []TokenBoundary, error) {
	if token.end-token.start > maxTruncationSearchRunes {
		return GeneratedEmbeddingInput{}, nil, errors.New("indivisible token exceeds bounded truncation search")
	}
	byteEnd, err := maximalTruncatedByteFit(evidence, token, policy, attachment, limits)
	if err != nil {
		return GeneratedEmbeddingInput{}, nil, err
	}
	if byteEnd == 0 {
		return GeneratedEmbeddingInput{}, nil, errors.New("provider or aggregate limits cannot hold attachment context and one source rune")
	}
	candidate := token
	candidate.end = byteEnd
	input, contentBoundaries, err := makeGeneratedInput(evidence, []embeddingToken{candidate}, policy, attachment, limits, ordinal)
	if err == nil {
		return input, contentBoundaries, nil
	}
	if !fittingLimitError(err) {
		return GeneratedEmbeddingInput{}, nil, err
	}

	low, high := token.start+1, byteEnd-1
	bestEnd := 0
	if tokenizerIdentity.PrefixTokenCountsMonotonic && strings.HasSuffix(policy.ModelInput.Document.Template, modelInputContentSlot) {
		for low <= high {
			candidateEnd := low + (high-low)/2
			candidate := token
			candidate.end = candidateEnd
			_, _, err := makeGeneratedInput(evidence, []embeddingToken{candidate}, policy, attachment, limits, ordinal)
			if err == nil {
				bestEnd = candidateEnd
				low = candidateEnd + 1
				continue
			}
			if !fittingLimitError(err) {
				return GeneratedEmbeddingInput{}, nil, err
			}
			high = candidateEnd - 1
		}
	} else {
		for candidateEnd := high; candidateEnd >= low; candidateEnd-- {
			candidate := token
			candidate.end = candidateEnd
			_, _, err := makeGeneratedInput(evidence, []embeddingToken{candidate}, policy, attachment, limits, ordinal)
			if err == nil {
				bestEnd = candidateEnd
				break
			}
			if !fittingLimitError(err) {
				return GeneratedEmbeddingInput{}, nil, err
			}
		}
	}
	if bestEnd != 0 {
		validatedToken := token
		validatedToken.end = bestEnd
		validated, validatedBoundaries, err := makeGeneratedInput(evidence, []embeddingToken{validatedToken}, policy, attachment, limits, ordinal)
		if err != nil {
			return GeneratedEmbeddingInput{}, nil, fmt.Errorf("validate truncated embedding input: %w", err)
		}
		return validated, validatedBoundaries, nil
	}
	return GeneratedEmbeddingInput{}, nil, errors.New("provider or aggregate limits cannot hold attachment context and one source rune")
}

func maximalTruncatedByteFit(evidence NormalizedEvidenceV1, token embeddingToken, policy InputPolicy, attachment AttachmentContextSnapshot, limits inputConstructionLimits) (int, error) {
	low, high := token.start+1, token.end-1
	bestEnd := 0
	for low <= high {
		candidateEnd := low + (high-low)/2
		candidate := token
		candidate.end = candidateEnd
		fits, err := candidateFitsByteLimits(evidence, []embeddingToken{candidate}, policy, attachment, limits)
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

func tokenMetadata(evidence NormalizedEvidenceV1, tokens []embeddingToken) ([]ChunkSpan, [][]string, error) {
	if len(tokens) == 0 {
		return nil, nil, errors.New("embedding input requires at least one token")
	}
	first, last := tokens[0], tokens[len(tokens)-1]
	if first.unitIndex != last.unitIndex || first.unitIndex < 0 || first.unitIndex >= len(evidence.Units) || first.start < 0 || last.end <= first.start {
		return nil, nil, errors.New("embedding input tokens cross natural units")
	}
	span := ChunkSpan{UnitIndex: first.unitIndex, CharStart: first.start, CharEnd: last.end}
	heading := slices.Clone(evidence.Units[first.unitIndex].HeadingPath)
	return []ChunkSpan{span}, [][]string{heading}, nil
}

func contentByteLength(evidence NormalizedEvidenceV1, spans []ChunkSpan, limit int64) (int64, error) {
	var total int64
	for _, span := range spans {
		part, ok := runeRange(evidence.Units[span.UnitIndex].Text, span.CharStart, span.CharEnd)
		if !ok || int64(len(part)) > limit-total {
			return 0, errProviderInputLimit
		}
		total += int64(len(part))
	}
	return total, nil
}

func contentForSpans(evidence NormalizedEvidenceV1, spans []ChunkSpan, byteLength int64) string {
	var content strings.Builder
	content.Grow(int(byteLength))
	for _, span := range spans {
		part, _ := runeRange(evidence.Units[span.UnitIndex].Text, span.CharStart, span.CharEnd)
		content.WriteString(part)
	}
	return content.String()
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
	slot := strings.Index(encoder.Template, modelInputContentSlot)
	if slot < 0 {
		return int64(^uint64(0) >> 1)
	}
	return int64(len(encoder.Template)-len(modelInputContentSlot)) + contextualizedDocumentByteLength(attachment, contentBytes)
}

func contextualizedDocumentByteLength(attachment AttachmentContextSnapshot, contentBytes int64) int64 {
	length := contentBytes
	if attachment.title != "" {
		length += int64(len("Title: ") + len(attachment.title) + 1)
	}
	if attachment.context != "" {
		length += int64(len("Context: ") + len(attachment.context) + 1)
	}
	if attachment.declared() {
		length++
	}
	return length
}

func contextualizedDocumentRuneLength(attachment AttachmentContextSnapshot, contentRunes int64) int64 {
	length := contentRunes
	if attachment.title != "" {
		length += int64(utf8.RuneCountInString("Title: ") + utf8.RuneCountInString(attachment.title) + 1)
	}
	if attachment.context != "" {
		length += int64(utf8.RuneCountInString("Context: ") + utf8.RuneCountInString(attachment.context) + 1)
	}
	if attachment.declared() {
		length++
	}
	return length
}

func renderedDocumentRuneLength(encoder ModelInputEncoder, attachment AttachmentContextSnapshot, contentRunes int64) int64 {
	return int64(utf8.RuneCountInString(encoder.Template)-utf8.RuneCountInString(modelInputContentSlot)) + contextualizedDocumentRuneLength(attachment, contentRunes)
}

func contextualizeDocumentInput(attachment AttachmentContextSnapshot, content string) string {
	if !attachment.declared() {
		return content
	}
	var contextualized strings.Builder
	if attachment.title != "" {
		contextualized.WriteString("Title: ")
		contextualized.WriteString(attachment.title)
		contextualized.WriteByte('\n')
	}
	if attachment.context != "" {
		contextualized.WriteString("Context: ")
		contextualized.WriteString(attachment.context)
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

func inputPolicyFingerprint(policy InputPolicy, attachment AttachmentContextSnapshot, identity TokenizerIdentity) string {
	frames := newFingerprintFrames("docbank/embedding-input-policy", EmbeddingInputGenerationVersion)
	frames.text("tokenizer.name", identity.Name)
	frames.text("tokenizer.revision", identity.Revision)
	frames.boolean("tokenizer.prefix_token_counts_monotonic", identity.PrefixTokenCountsMonotonic)
	frames.integer("content_token_budget", policy.ContentTokenBudget)
	frames.integer("overlap_tokens", policy.OverlapTokens)
	frames.integer("max_provider_tokens", policy.MaxProviderTokens)
	frames.integer64("max_provider_bytes", policy.MaxProviderBytes)
	frames.integer("max_generated_inputs", policy.MaxGeneratedInputs)
	frames.integer64("max_total_content_tokens", policy.MaxTotalContentTokens)
	frames.integer64("max_total_rendered_tokens", policy.MaxTotalRenderedTokens)
	frames.integer64("max_total_content_bytes", policy.MaxTotalContentBytes)
	frames.integer64("max_total_rendered_bytes", policy.MaxTotalRenderedBytes)
	frames.integer64("max_fitting_work_tokens", policy.MaxFittingWorkTokens)
	frames.integer64("max_fitting_work_bytes", policy.MaxFittingWorkBytes)
	frames.text("model_input_fingerprint", policy.ModelInput.Fingerprint)
	frames.text("formatter", policy.Formatter)
	frames.text("lexical_evidence_fingerprint", policy.LexicalEvidenceFingerprint)
	frames.text("context_fingerprint", policy.ContextFingerprint)
	frames.boolean("attachment_context.declared", attachment.declared())
	frames.text("attachment_context.title", attachment.title)
	frames.text("attachment_context.context", attachment.context)
	frames.text("truncation_policy", string(policy.TruncationPolicy))
	return frames.sum()
}

func generationFingerprint(generation EmbeddingInputGeneration) string {
	frames := newFingerprintFrames("docbank/embedding-input-generation", generation.Version)
	frames.text("evidence_checksum", generation.EvidenceChecksum)
	frames.text("policy_fingerprint", generation.PolicyFingerprint)
	frames.text("tokenizer.name", generation.TokenizerIdentity.Name)
	frames.text("tokenizer.revision", generation.TokenizerIdentity.Revision)
	frames.boolean("tokenizer.prefix_token_counts_monotonic", generation.TokenizerIdentity.PrefixTokenCountsMonotonic)
	frames.text("lexical_evidence_fingerprint", generation.LexicalEvidenceFingerprint)
	frames.text("formatter", generation.Formatter)
	frames.text("model_input_fingerprint", generation.ModelInputFingerprint)
	if generation.Version >= EmbeddingInputGenerationVersion {
		frames.integer("content_token_budget", generation.ContentTokenBudget)
		frames.integer("overlap_tokens", generation.OverlapTokens)
		frames.text("truncation_policy", string(generation.TruncationPolicy))
		frames.text("context_fingerprint", generation.ContextFingerprint)
	}
	frames.boolean("attachment_context.declared", generation.AttachmentContext != nil)
	if generation.AttachmentContext != nil {
		frames.text("attachment_context.title", generation.AttachmentContext.title)
		frames.text("attachment_context.context", generation.AttachmentContext.context)
	}
	frames.integer64("total_content_tokens", generation.TotalContentTokens)
	frames.integer64("total_rendered_tokens", generation.TotalRenderedTokens)
	frames.integer64("total_content_bytes", generation.TotalContentBytes)
	frames.integer64("total_rendered_bytes", generation.TotalRenderedBytes)
	frames.count("inputs", len(generation.Inputs))
	for inputIndex, input := range generation.Inputs {
		frames.object("input")
		frames.integer("input.index", inputIndex)
		frames.text("input.key", input.Key)
		frames.text("input.content", input.Content)
		frames.text("input.rendered", input.Rendered)
		frames.integer("input.content_tokens", input.ContentTokens)
		frames.integer("input.rendered_tokens", input.RenderedTokens)
		frames.text("input.checksum", input.Checksum)
		frames.boolean("input.truncated", input.Truncated)
		frames.count("input.heading_paths", len(input.HeadingPaths))
		for headingIndex, heading := range input.HeadingPaths {
			frames.object("input.heading_path")
			frames.integer("input.heading_path.index", headingIndex)
			frames.count("input.heading_path.parts", len(heading))
			for partIndex, part := range heading {
				frames.integer("input.heading_path.part.index", partIndex)
				frames.text("input.heading_path.part", part)
			}
		}
		frames.count("input.source_spans", len(input.SourceSpans))
		for spanIndex, span := range input.SourceSpans {
			frames.object("input.source_span")
			frames.integer("input.source_span.index", spanIndex)
			frames.integer("input.source_span.unit_index", span.UnitIndex)
			frames.integer("input.source_span.char_start", span.CharStart)
			frames.integer("input.source_span.char_end", span.CharEnd)
		}
	}
	return frames.sum()
}

type fingerprintFrameType byte

const (
	fingerprintFrameDomain fingerprintFrameType = iota + 1
	fingerprintFrameText
	fingerprintFrameInteger
	fingerprintFrameBoolean
	fingerprintFrameObject
	fingerprintFrameCount
)

type fingerprintFrames struct {
	digest hash.Hash
}

func newFingerprintFrames(domain string, version int) *fingerprintFrames {
	frames := &fingerprintFrames{digest: sha256.New()}
	frames.writeStringFrame(fingerprintFrameDomain, "domain", domain)
	frames.integer("version", version)
	return frames
}
func (frames *fingerprintFrames) writeHeader(frameType fingerprintFrameType, field string, valueLength int) {
	_, _ = frames.digest.Write([]byte{byte(frameType)})
	frames.writeLength(len(field))
	_, _ = io.WriteString(frames.digest, field)
	frames.writeLength(valueLength)
}
func (frames *fingerprintFrames) writeLength(value int) {
	_, _ = io.WriteString(frames.digest, strconv.Itoa(value))
	_, _ = frames.digest.Write([]byte{':'})
}
func (frames *fingerprintFrames) writeStringFrame(frameType fingerprintFrameType, field, value string) {
	frames.writeHeader(frameType, field, len(value))
	_, _ = io.WriteString(frames.digest, value)
}
func (frames *fingerprintFrames) text(field, value string) {
	frames.writeStringFrame(fingerprintFrameText, field, value)
}
func (frames *fingerprintFrames) integer(field string, value int) {
	encoded := strconv.Itoa(value)
	frames.writeHeader(fingerprintFrameInteger, field, len(encoded))
	_, _ = io.WriteString(frames.digest, encoded)
}
func (frames *fingerprintFrames) integer64(field string, value int64) {
	encoded := strconv.FormatInt(value, 10)
	frames.writeHeader(fingerprintFrameInteger, field, len(encoded))
	_, _ = io.WriteString(frames.digest, encoded)
}
func (frames *fingerprintFrames) boolean(field string, value bool) {
	frames.writeHeader(fingerprintFrameBoolean, field, 1)
	encoded := byte(0)
	if value {
		encoded = 1
	}
	_, _ = frames.digest.Write([]byte{encoded})
}
func (frames *fingerprintFrames) object(field string) {
	frames.writeHeader(fingerprintFrameObject, field, 0)
}
func (frames *fingerprintFrames) count(field string, value int) {
	encoded := strconv.Itoa(value)
	frames.writeHeader(fingerprintFrameCount, field, len(encoded))
	_, _ = io.WriteString(frames.digest, encoded)
}
func (frames *fingerprintFrames) sum() string {
	return hex.EncodeToString(frames.digest.Sum(nil))
}
