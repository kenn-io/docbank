// Package lexical prepares literal, phrase, and analyzer-specific full-text
// queries and records the analyzer identity that must match the index.
//
// Three identities stay distinct. Literal quoting does not segment text.
// Character-phrase analysis preserves adjacency of Han, Hiragana, Katakana,
// and Hangul characters; it is not morphological analysis. Chinese
// segmentation is performed by a caller-supplied segmenter whose runtime and
// dictionary fingerprint is part of the identity. This package does not
// install that runtime.
//
// CJK search is off on the zero CJK value. The ordinary full-text index
// answers every query until the caller turns the second index on:
//
//	cjk := lexical.EnableCharacterPhrase()
//	switch cjk.IndexFor(query) {
//	case lexical.IndexOrdinary:
//	    // Query the stock full-text index.
//	case lexical.IndexCJK:
//	    analyzer, _ := cjk.Analyzer(query)
//	    prepared, _ := analyzer.PrepareLiteral(query)
//	    // Query the caller's CJK index with prepared.Match.
//	}
//
// IndexFor chooses the CJK index when the text contains Han, Hangul,
// Hiragana, or Katakana. A mixed query such as "run 搜索" uses that index
// too. Digits, punctuation, and ordinary words stay on the stock index.
// The caller creates the second index and names it. sqlitefts.New receives
// it through WithIndexTable. postgres.LexicalMapping.Vector names the stored
// search vector. clickhouse.TextRequest names Table and TextColumn. This
// package does not create those indexes.
//
// EnableCharacterPhrase stores Han, kana, and Hangul as adjacent characters
// and keeps a Latin run as one word. EnableChinese is the same switch with
// a caller-supplied dictionary cutter for Han text. Kana and Hangul stay
// unsegmented, and the caller keeps the dictionary files. Index and query
// preparation must use the same identity.
//
// Intentional query syntax uses PrepareAdvanced and is not inferred from
// punctuation.
package lexical
