package main

import (
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const unicodeRuneSpec = "unicode-runes@v1"

type unicodeRuneTokenizer struct{}

func (unicodeRuneTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{
		Name: "unicode-runes", Revision: "v1", PrefixTokenCountsMonotonic: true,
	}
}

func (unicodeRuneTokenizer) Tokenize(text string, limit int) ([]document.TokenBoundary, error) {
	runeCount := utf8.RuneCountInString(text)
	if runeCount > limit {
		return nil, document.ErrTokenizerLimit
	}
	boundaries := make([]document.TokenBoundary, runeCount)
	for index := range boundaries {
		boundaries[index] = document.TokenBoundary{Start: index, End: index + 1}
	}
	return boundaries, nil
}

func configuredEmbeddingTokenizer(identity string) document.Tokenizer {
	if identity == unicodeRuneSpec {
		return unicodeRuneTokenizer{}
	}
	return nil
}

var _ document.Tokenizer = unicodeRuneTokenizer{}
