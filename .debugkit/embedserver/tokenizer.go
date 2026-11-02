package embedserver

import (
	json "encoding/json/v2"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/unicode/rangetable"
)

// b11845's Unicode 15.1 additions need no transforms beyond isCJK.
// Preserve later assignments instead of applying Go's newer Unicode tables.
var wordPieceAssigned = rangetable.Assigned("15.0.0")

// Match llama.cpp b11845 WPM, including per-rune first-code-point NFD
// and unrestricted word length. Normalization and splitting follow:
// https://github.com/ggml-org/llama.cpp/blob/b11845/src/llama-vocab.cpp
type tokenizer struct {
	vocab    map[string]int
	specials map[string]int
	maxPiece int
}

func loadTokenizer(path string) (*tokenizer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Model struct {
			Vocab map[string]int `json:"vocab"`
		} `json:"model"`
		Added []struct {
			Content string `json:"content"`
			ID      int    `json:"id"`
			Special bool   `json:"special"`
		} `json:"added_tokens"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	t := &tokenizer{vocab: make(map[string]int), specials: make(map[string]int)}
	for _, special := range file.Added {
		if special.Special {
			t.specials[special.Content] = special.ID
		}
	}
	for piece, id := range file.Model.Vocab {
		if _, control := t.specials[piece]; !control {
			if rest, continuation := strings.CutPrefix(piece, "##"); continuation {
				piece = rest
			} else {
				piece = "▁" + piece
			}
		}
		t.vocab[piece] = id
		t.maxPiece = max(t.maxPiece, len(piece))
	}
	return t, nil
}

func (t *tokenizer) wordPieces(word string) []int {
	if id, special := t.specials[word]; special {
		return []int{id}
	}
	var pieces []int
	word = "▁" + word
	for len(word) > 0 {
		matched := 0
		for end := min(len(word), t.maxPiece); end > 0; end-- {
			if id, ok := t.vocab[word[:end]]; ok {
				// Finish the word even after the cap: a later unmatched suffix must
				// replace the entire word with UNK, rather than retaining its prefix.
				if len(pieces) < 2046 {
					pieces = append(pieces, id)
				}
				matched = end
				break
			}
		}
		if matched == 0 {
			return []int{t.vocab["[UNK]"]}
		}
		word = word[matched:]
	}
	return pieces
}

func (t *tokenizer) encode(text string) []int {
	ids := []int{t.vocab["[CLS]"]}
	add := func(word string) {
		pieces := t.wordPieces(word)
		ids = append(ids, pieces[:min(len(pieces), 2047-len(ids))]...)
	}
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			add(word.String())
			word.Reset()
		}
	}
next:
	for len(text) > 0 && len(ids) < 2047 {
		for token := range t.specials {
			if rest, ok := strings.CutPrefix(text, token); ok {
				flush()
				add(token)
				text = rest
				continue next
			}
		}
		r, size := utf8.DecodeRuneInString(text)
		text = text[size:]
		if !unicode.Is(wordPieceAssigned, r) && !isCJK(r) {
			word.WriteRune(r)
			continue
		}
		if unicode.Is(wordPieceAssigned, r) {
			r, _ = utf8.DecodeRuneInString(norm.NFD.String(string(r)))
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		if r == 0 || r == utf8.RuneError || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs, unicode.M) {
			continue
		}
		if unicode.IsPunct(r) || (r < 0x7f && unicode.IsSymbol(r)) || isCJK(r) {
			flush()
			add(string(unicode.ToLower(r)))
		} else {
			word.WriteRune(unicode.ToLower(r))
		}
	}
	flush()
	return append(ids, t.vocab["[SEP]"])
}

func isCJK(r rune) bool {
	// b11845 intentionally follows BERT's 0x2B920 boundary, rather than 0x2B820.
	return r >= 0x4e00 && r <= 0x9fff || r >= 0x3400 && r <= 0x4dbf ||
		r >= 0x20000 && r <= 0x2a6df || r >= 0x2a700 && r <= 0x2b73f ||
		r >= 0x2b740 && r <= 0x2b81f || r >= 0x2b920 && r <= 0x2ceaf ||
		r >= 0xf900 && r <= 0xfaff || r >= 0x2f800 && r <= 0x2fa1f
}
