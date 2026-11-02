package vector

// Chunk is a window of text encoded as a single vector. Index is the
// chunk's position within the source content, starting at zero.
type Chunk struct {
	Index int
	Text  string
}

// SourceSpan is a half-open rune range in caller-owned source text.
// Start is inclusive and End is exclusive. The encoded text may include
// formatting that is not a substring of this range.
type SourceSpan struct {
	Start int
	End   int
}

// PreparedChunk is one encode input the caller built. Text is sent to the
// encoder unchanged. Index is stored with the vector. Span is nil when the
// caller has no coordinates. Truncated is the caller's statement that Text
// omits source content. Fill reports the chunk through progress and does
// not persist Span or Truncated.
type PreparedChunk struct {
	Index     int
	Text      string
	Span      *SourceSpan
	Truncated bool
}

// SplitOptions controls how Split windows content into chunks.
type SplitOptions struct {
	// MaxRunes bounds the number of runes in each chunk. Values <= 0
	// disable splitting and return the content as a single chunk, which
	// can exceed a model's per-input limit (often 512 to 8192 tokens).
	MaxRunes int
	// Overlap is the number of runes shared between consecutive chunks.
	// It is clamped to the range [0, MaxRunes-1].
	Overlap int
}

// Split windows content into overlapping chunks of at most MaxRunes runes.
// It splits on runes rather than bytes so multi-byte characters are never
// torn apart. Blank windows yield no chunks — empty, whitespace, or invisible
// formatting runes only — so callers never need to send semantically empty
// text to an embedding model.
//
// Split measures size in runes, not model tokens. Callers that budget by
// tokens should convert their token budget to an approximate rune count.
func Split(content string, o SplitOptions) []Chunk {
	if blank(content) {
		return nil
	}
	runes := []rune(content)
	if o.MaxRunes <= 0 || len(runes) <= o.MaxRunes {
		return []Chunk{{Index: 0, Text: content}}
	}

	overlap := min(max(o.Overlap, 0), o.MaxRunes-1)
	stride := o.MaxRunes - overlap

	var chunks []Chunk
	for start, idx := 0, 0; start < len(runes); start, idx = start+stride, idx+1 {
		end := start + o.MaxRunes
		if end >= len(runes) {
			text := string(runes[start:])
			if !blank(text) {
				chunks = append(chunks, Chunk{Index: idx, Text: text})
			}
			break
		}
		text := string(runes[start:end])
		if !blank(text) {
			chunks = append(chunks, Chunk{Index: idx, Text: text})
		}
	}
	return chunks
}
