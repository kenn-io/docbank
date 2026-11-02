package lexical

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Token is one term from a caller-supplied Chinese segmenter.
// Prefix keeps a trailing prefix operator outside quotes. Prefix text must
// not already contain query syntax.
type Token struct {
	Text   string
	Prefix bool
}

// Prepared is a backend match expression and the analyzer that produced it.
type Prepared struct {
	Match       string
	Identity    Identity
	SnippetTerm string
}

// Analyzer prepares queries for one index identity.
// Segment is required for Chinese segmentation and is not called for
// character-phrase analysis, for phrases, or when the query contains
// Hiragana, Katakana, or Hangul.
type Analyzer struct {
	Identity Identity
	Segment  func(string) ([]Token, error)
}

// Literal returns the quoting analyzer. Punctuation stays inside quoted
// terms so the full-text engine does not parse it as query syntax.
func Literal() Analyzer {
	return Analyzer{Identity: literalIdentity()}
}

// CharacterPhrase returns adjacency analysis for Han, Hiragana, Katakana,
// and Hangul. Index documents with IndexText so a unicode61-style tokenizer
// emits one token per character. This is not Chinese word segmentation.
func CharacterPhrase() Analyzer {
	return Analyzer{Identity: characterPhraseIdentity()}
}

// Chinese returns segmentation that calls segment for Han text. runtime is
// the fingerprint from FingerprintRuntime and must be non-empty. Queries that
// contain Hiragana, Katakana, or Hangul are quoted phrases and do not call
// segment; those scripts are not segmented as morphology.
func Chinese(runtime string, segment func(string) ([]Token, error)) (Analyzer, error) {
	runtime = strings.TrimSpace(runtime)
	if runtime == "" {
		return Analyzer{}, errors.New("lexical: chinese analyzer requires a runtime fingerprint")
	}
	if segment == nil {
		return Analyzer{}, errors.New("lexical: chinese analyzer requires a segmenter")
	}
	return Analyzer{
		Identity: Identity{Kind: KindChinese, Version: ChineseQueryVersion, Runtime: runtime},
		Segment:  segment,
	}, nil
}

// PrepareLiteral splits the query on whitespace and combines terms with AND.
// Empty input is rejected so a blank query cannot match the whole index.
func (a Analyzer) PrepareLiteral(text string) (Prepared, error) {
	if err := a.valid(); err != nil {
		return Prepared{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Prepared{}, errors.New("lexical: query text is empty")
	}
	if a.Identity.Kind == KindChinese && !containsKanaOrHangul(text) && containsHan(text) {
		return a.segmented(text)
	}
	fields := strings.Fields(text)
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, a.renderTerm(field))
	}
	return Prepared{
		Match:       strings.Join(parts, " "),
		Identity:    a.Identity,
		SnippetTerm: fields[0],
	}, nil
}

// PreparePhrase keeps the whole query as one ordered phrase. Whitespace does
// not become AND. Chinese segmentation is not used, so a quoted Han phrase
// keeps character order instead of dictionary tokens.
func (a Analyzer) PreparePhrase(text string) (Prepared, error) {
	if err := a.valid(); err != nil {
		return Prepared{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Prepared{}, errors.New("lexical: phrase text is empty")
	}
	flat := strings.Join(strings.Fields(text), " ")
	match := quote(flat)
	if a.Identity.Kind == KindCharacterPhrase {
		match = a.renderTerm(flat)
	}
	return Prepared{Match: match, Identity: a.Identity, SnippetTerm: flat}, nil
}

// PrepareAdvanced returns expression unchanged, aside from trimming space.
// The caller is asserting that expression is intentional query syntax for
// the same analyzer identity. This function does not quote, segment, or
// rewrite it.
func (a Analyzer) PrepareAdvanced(expression string) (Prepared, error) {
	if err := a.valid(); err != nil {
		return Prepared{}, err
	}
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return Prepared{}, errors.New("lexical: advanced query is empty")
	}
	return Prepared{Match: expression, Identity: a.Identity, SnippetTerm: expression}, nil
}

// IndexText rewrites document text into the token stream this analyzer
// expects. Literal text is unchanged. Character-phrase text gains a space
// around every Han, Hiragana, Katakana, and Hangul character. Chinese
// indexing belongs to the external tokenizer, so IndexText rejects it.
func (a Analyzer) IndexText(text string) (string, error) {
	if err := a.valid(); err != nil {
		return "", err
	}
	switch a.Identity.Kind {
	case KindLiteral:
		return text, nil
	case KindCharacterPhrase:
		return separateCharacters(text), nil
	case KindChinese:
		return "", errors.New("lexical: chinese indexing is owned by the external tokenizer")
	default:
		return "", fmt.Errorf("lexical: unknown analyzer kind %q", a.Identity.Kind)
	}
}

func (a Analyzer) segmented(text string) (Prepared, error) {
	tokens, err := a.Segment(text)
	if err != nil {
		return Prepared{}, fmt.Errorf("lexical: segment query: %w", err)
	}
	if len(tokens) == 0 {
		return Prepared{}, errors.New("lexical: query is empty after segmentation")
	}
	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		part, err := formatToken(token)
		if err != nil {
			return Prepared{}, err
		}
		parts = append(parts, part)
	}
	return Prepared{
		Match:       strings.Join(parts, " AND "),
		Identity:    a.Identity,
		SnippetTerm: tokens[0].Text,
	}, nil
}

func (a Analyzer) renderTerm(term string) string {
	if a.Identity.Kind == KindCharacterPhrase && containsCJK(term) {
		return quote(characterPhrase(term))
	}
	return quote(term)
}

func (a Analyzer) valid() error {
	switch a.Identity.Kind {
	case KindLiteral:
		if a.Identity.Version != literalVersion || a.Identity.Runtime != "" {
			return errors.New("lexical: literal analyzer identity was modified")
		}
	case KindCharacterPhrase:
		if a.Identity.Version != characterPhraseVersion || a.Identity.Runtime != "" {
			return errors.New("lexical: character-phrase analyzer identity was modified")
		}
	case KindChinese:
		if a.Identity.Version != ChineseQueryVersion || strings.TrimSpace(a.Identity.Runtime) == "" || a.Segment == nil {
			return errors.New("lexical: chinese analyzer identity was modified")
		}
	default:
		return fmt.Errorf("lexical: unknown analyzer kind %q", a.Identity.Kind)
	}
	return nil
}

func formatToken(token Token) (string, error) {
	text := token.Text
	if text == "" || strings.TrimSpace(text) != text {
		return "", errors.New("lexical: segment token is empty")
	}
	if strings.Contains(text, `"`) {
		return "", fmt.Errorf("lexical: segment token %q contains a quote", text)
	}
	if token.Prefix {
		if !safePrefix(text) {
			return "", fmt.Errorf("lexical: prefix token %q contains query syntax", text)
		}
		return text + "*", nil
	}
	return quote(text), nil
}

func safePrefix(text string) bool {
	for _, r := range text {
		if r <= unicode.MaxASCII && !isASCIIToken(r) {
			return false
		}
	}
	switch strings.ToUpper(text) {
	case "AND", "OR", "NOT", "NEAR":
		return false
	}
	return utf8.ValidString(text)
}

func isASCIIToken(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}

func quote(term string) string {
	return `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
}

func characterPhrase(term string) string {
	var parts []string
	var latin strings.Builder
	flush := func() {
		if latin.Len() > 0 {
			parts = append(parts, latin.String())
			latin.Reset()
		}
	}
	for _, r := range term {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isCJK(r):
			flush()
			parts = append(parts, string(r))
		default:
			latin.WriteRune(r)
		}
	}
	flush()
	return strings.Join(parts, " ")
}

func separateCharacters(text string) string {
	var b strings.Builder
	var prev rune
	var hasPrev bool
	for _, r := range text {
		if hasPrev && !unicode.IsSpace(prev) && !unicode.IsSpace(r) && (isCJK(prev) || isCJK(r)) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		prev = r
		hasPrev = true
	}
	return b.String()
}

func containsCJK(text string) bool {
	return containsScript(text, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana)
}

func containsHan(text string) bool {
	return containsScript(text, unicode.Han)
}

func containsKanaOrHangul(text string) bool {
	return containsScript(text, unicode.Hangul, unicode.Hiragana, unicode.Katakana)
}

func containsScript(text string, tables ...*unicode.RangeTable) bool {
	for _, r := range text {
		if unicode.In(r, tables...) {
			return true
		}
	}
	return false
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana)
}
