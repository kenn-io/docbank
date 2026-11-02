package lexical

// Index identifies which caller-owned full-text index answers a query.
type Index int

const (
	// IndexOrdinary is the stock full-text index.
	IndexOrdinary Index = iota
	// IndexCJK is the additional index for Han, Hangul, Hiragana, and Katakana.
	IndexCJK
)

// String returns "cjk" or "ordinary".
func (i Index) String() string {
	if i == IndexCJK {
		return "cjk"
	}
	return "ordinary"
}

// CJK is the optional Chinese, Japanese, and Korean search index.
// The zero value is off, so every query uses IndexOrdinary.
// Turning it on does not create storage and does not fetch dictionaries.
// The caller creates the second index and passes its name to the backend
// query builder. SQLite, Postgres, and ClickHouse each keep their own index.
type CJK struct {
	enabled  bool
	analyzer Analyzer
}

// EnableCharacterPhrase turns on a CJK index that stores Han, kana, and
// Hangul as adjacent characters. A Latin run stays one word, so a mixed
// query such as "run 搜索" is answered from this index.
func EnableCharacterPhrase() CJK {
	return CJK{enabled: true, analyzer: CharacterPhrase()}
}

// EnableChinese turns on a CJK index whose Han text is cut by segment.
// runtime is the dictionary fingerprint from FingerprintRuntime. Kana and
// Hangul queries stay unsegmented. The caller owns index-time tokenization
// and the dictionary files.
func EnableChinese(runtime string, segment func(string) ([]Token, error)) (CJK, error) {
	analyzer, err := Chinese(runtime, segment)
	if err != nil {
		return CJK{}, err
	}
	return CJK{enabled: true, analyzer: analyzer}, nil
}

// Enabled reports whether the additional CJK index is in use.
func (c CJK) Enabled() bool { return c.enabled }

// IndexFor chooses the index for text.
// With CJK off, the result is IndexOrdinary.
// With CJK on, Han, Hangul, Hiragana, or Katakana select IndexCJK.
// Digits and punctuation alone stay on the ordinary index.
func (c CJK) IndexFor(text string) Index {
	if c.enabled && containsCJK(text) {
		return IndexCJK
	}
	return IndexOrdinary
}

// Analyzer returns the analyzer for a query IndexFor sent to IndexCJK.
// The boolean is false when the query stays on the ordinary index.
// The returned analyzer's identity must match the index the caller built.
func (c CJK) Analyzer(text string) (Analyzer, bool) {
	if c.IndexFor(text) != IndexCJK {
		return Analyzer{}, false
	}
	return c.analyzer, true
}
