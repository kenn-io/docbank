// Package embedfit fits formatted text into a token budget and records where
// each piece came from.
//
// The tokenizer counts the string the model will see, including a role prefix
// or suffix and any heading the caller already added. Splits prefer a
// paragraph boundary, then a sentence boundary, then a word boundary, in the
// last quarter of the window that fits, including a separator at the start
// of that quarter. A separator just past the window also counts, so a window
// that ends on "." followed by a space ends a sentence.
// Each span is then trimmed of blank text at both ends, using the
// embedmodel.BlankText rule, and its coordinates cover the trimmed text.
// A hard cut is truncation and happens only when the truncation policy
// allows it.
//
// Prepared is the chunk shape a fill loop can accept without assuming rune
// windows or a list of bare strings. It uses the prefix and suffix Fit
// counted. This package does not call vector.Fill.
// The fill entry point is tracked separately and should take Prepared values
// rather than defining a second chunk type.
package embedfit
