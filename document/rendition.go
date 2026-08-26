package document

import (
	"errors"
	"fmt"
)

// RenditionContractV1 identifies the durable, sanitized rendition contract.
const RenditionContractV1 = "rendition/v1"

// RenditionPolicy binds rendition construction to the frozen document
// normalization limits.
type RenditionPolicy struct {
	normalization   NormalizePolicy
	maxSegmentRunes int
}

// NewRenditionPolicy returns a policy whose bounds match the supplied document
// normalization policy and, when supplied, the canonical evidence-lexical
// segment limit. The normalization chunk limit remains the compatibility
// default for callers that do not publish against a processing profile.
func NewRenditionPolicy(normalization NormalizePolicy, lexicalSegmentLimit ...int) (RenditionPolicy, error) {
	if err := normalization.validate(); err != nil {
		return RenditionPolicy{}, fmt.Errorf("rendition normalization policy: %w", err)
	}
	if len(lexicalSegmentLimit) > 1 {
		return RenditionPolicy{}, errors.New("rendition policy accepts at most one lexical segment limit")
	}
	maxSegmentRunes := normalization.maxChunkRunes
	if len(lexicalSegmentLimit) == 1 {
		maxSegmentRunes = lexicalSegmentLimit[0]
	}
	if maxSegmentRunes <= 0 || maxSegmentRunes > maxEvidenceSegmentRunes {
		return RenditionPolicy{}, fmt.Errorf(
			"rendition max segment runes must be between 1 and %d", maxEvidenceSegmentRunes)
	}
	return RenditionPolicy{
		normalization: normalization, maxSegmentRunes: maxSegmentRunes,
	}, nil
}

func (p RenditionPolicy) validate() error {
	if err := p.normalization.validate(); err != nil {
		return fmt.Errorf("rendition policy: %w", err)
	}
	if p.maxSegmentRunes <= 0 || p.maxSegmentRunes > maxEvidenceSegmentRunes {
		return fmt.Errorf("rendition policy max segment runes must be between 1 and %d", maxEvidenceSegmentRunes)
	}
	return nil
}

// MaxSegmentRunes returns the durable lexical segment bound carried by the
// policy for publication-profile validation.
func (p RenditionPolicy) MaxSegmentRunes() int { return p.maxSegmentRunes }

// RenditionWarningV1 records a non-fatal loss of source provenance while
// retaining sanitized readable evidence.
type RenditionWarningV1 struct {
	Code string
}

// NormalizedUnitV1 is one sanitized text unit with a stable link to its
// canonical evidence unit.
type NormalizedUnitV1 struct {
	Checksum       string
	EvidenceUnitID string
	HeadingPath    []string
	ID             string
	Locator        EvidenceLocatorV1
	Order          int
	Text           string
}

// LexicalSegmentV1 is one model-independent, half-open rune span in a
// normalized rendition unit.
type LexicalSegmentV1 struct {
	CharEnd   int
	CharStart int
	Checksum  string
	ID        string
	Order     int
	Text      string
	UnitID    string
}

// RenditionV1 contains every durable output derived from the same normalized
// evidence walk. Checksum covers the complete rendition manifest; Markdown and
// its checksum are retained independently for blob storage.
type RenditionV1 struct {
	Checksum         string
	Completeness     EvidenceCompleteness
	ContractVersion  string
	EvidenceChecksum string
	LexicalSegments  []LexicalSegmentV1
	Markdown         []byte
	MarkdownChecksum string
	Units            []NormalizedUnitV1
	Warnings         []RenditionWarningV1
}

func validateRenditionPolicy(policy RenditionPolicy) error {
	if err := policy.validate(); err != nil {
		return err
	}
	return nil
}

func invalidRenditionError(reason string) error {
	return errors.New("invalid rendition: " + reason)
}
