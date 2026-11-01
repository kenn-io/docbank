package main

import "go.kenn.io/docbank/document"

const unicodeRuneSpec = "unicode-runes@v1"

func configuredEmbeddingTokenizer(identity string) document.Tokenizer {
	if identity == unicodeRuneSpec {
		return document.UnicodeRuneTokenizer{}
	}
	return nil
}
