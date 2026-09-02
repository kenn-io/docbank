package document

import (
	"errors"
	"fmt"
)

// RenditionContractV1 identifies the durable, sanitized rendition contract.
const RenditionContractV1 = "rendition/v1"

// Sanitizer bounds fixed for rendition/v1. They are part of the policy
// identity so a future change splits shared builds instead of reusing them.
const (
	renditionMaxSourceUnitBytes = 4_000_000
	renditionMaxLinkChars       = 2_048
)

// RenditionLimits are the profile-owned bounds applied while building a
// rendition: the whole-document budget, the per-unit truncation point, and
// the lexical segment size.
type RenditionLimits struct {
	MaxDocumentChars int
	MaxUnitRunes     int
	MaxSegmentRunes  int
}

// RenditionPolicy binds rendition construction to one immutable set of
// limits. Renditions never chunk, so it carries no chunk bounds.
type RenditionPolicy struct {
	maxDocumentChars   int
	maxUnitRunes       int
	maxSegmentRunes    int
	maxSourceUnitBytes int
	maxLinkChars       int
}

// NewRenditionPolicy returns a policy that truncates every unit at the
// supplied profile limits and segments text at the lexical segment size.
func NewRenditionPolicy(limits RenditionLimits) (RenditionPolicy, error) {
	policy := RenditionPolicy{
		maxDocumentChars:   limits.MaxDocumentChars,
		maxUnitRunes:       limits.MaxUnitRunes,
		maxSegmentRunes:    limits.MaxSegmentRunes,
		maxSourceUnitBytes: renditionMaxSourceUnitBytes,
		maxLinkChars:       renditionMaxLinkChars,
	}
	if err := policy.validate(); err != nil {
		return RenditionPolicy{}, err
	}
	return policy, nil
}

func (p RenditionPolicy) validate() error {
	if p.maxDocumentChars <= 0 {
		return errors.New("rendition policy max document chars must be positive")
	}
	if p.maxUnitRunes <= 0 || p.maxUnitRunes > maxEvidenceUnitRunes {
		return fmt.Errorf("rendition policy max unit runes must be between 1 and %d", maxEvidenceUnitRunes)
	}
	if p.maxSegmentRunes <= 0 || p.maxSegmentRunes > maxEvidenceSegmentRunes {
		return fmt.Errorf("rendition policy max segment runes must be between 1 and %d", maxEvidenceSegmentRunes)
	}
	if p.maxSourceUnitBytes <= 0 || p.maxLinkChars <= 0 {
		return errors.New("rendition policy sanitizer bounds must be positive")
	}
	return nil
}

// Limits returns the profile-owned bounds carried by the policy.
func (p RenditionPolicy) Limits() RenditionLimits {
	return RenditionLimits{
		MaxDocumentChars: p.maxDocumentChars, MaxUnitRunes: p.maxUnitRunes, MaxSegmentRunes: p.maxSegmentRunes,
	}
}

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

func invalidRenditionError(reason string) error {
	return errors.New("invalid rendition: " + reason)
}
