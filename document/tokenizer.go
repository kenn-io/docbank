package document

import (
	"errors"
	"fmt"
)

const (
	maxEmbeddingTokensPerGeneration = 1_000_000
	maxTokenizerIdentityBytes       = 128
)

// ErrTokenizerLimit lets a tokenizer reject an input before allocating more
// than the caller-authorized number of token boundaries.
var ErrTokenizerLimit = errors.New("tokenizer token limit exceeded")

// TokenizerIdentity pins the exact tokenizer vocabulary and segmentation
// revision. PrefixTokenCountsMonotonic is an explicit contract that a longer
// prefix can never have fewer tokens; fitting may use binary search only when
// it is true. Model names are deliberately not tokenizer identities.
type TokenizerIdentity struct {
	Name                       string `json:"name"`
	Revision                   string `json:"revision"`
	PrefixTokenCountsMonotonic bool   `json:"prefix_token_counts_monotonic"`
}

// TokenBoundary is one half-open rune range. A canonical tokenization is a
// non-empty contiguous partition of the complete input string.
type TokenBoundary struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Tokenizer returns exact rune boundaries and must honor limit before growing
// its result beyond that many entries.
type Tokenizer interface {
	Identity() TokenizerIdentity
	Tokenize(text string, limit int) ([]TokenBoundary, error)
}

func validateTokenizerIdentity(identity TokenizerIdentity) error {
	if err := validateStableToken(identity.Name, "tokenizer name", maxTokenizerIdentityBytes); err != nil {
		return err
	}
	if err := validateStableToken(identity.Revision, "tokenizer revision", maxTokenizerIdentityBytes); err != nil {
		return err
	}
	return nil
}

func validateTokenBoundaries(tokens []TokenBoundary, runeCount, limit int) error {
	if len(tokens) == 0 {
		return errors.New("tokenizer must return at least one token for non-empty evidence")
	}
	if len(tokens) > limit {
		return ErrTokenizerLimit
	}
	expectedStart := 0
	for index, token := range tokens {
		if token.Start != expectedStart {
			return fmt.Errorf("tokenizer boundaries must be contiguous at token %d", index)
		}
		if token.End <= token.Start || token.End > runeCount {
			return fmt.Errorf("tokenizer boundary %d leaves input bounds", index)
		}
		expectedStart = token.End
	}
	if expectedStart != runeCount {
		return errors.New("tokenizer boundaries must cover the complete input contiguously")
	}
	return nil
}
