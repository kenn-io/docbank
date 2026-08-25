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
	normalization NormalizePolicy
}

// NewRenditionPolicy returns a policy whose bounds match the supplied document
// normalization policy.
func NewRenditionPolicy(normalization NormalizePolicy) (RenditionPolicy, error) {
	if err := normalization.validate(); err != nil {
		return RenditionPolicy{}, fmt.Errorf("rendition normalization policy: %w", err)
	}
	return RenditionPolicy{normalization: normalization}, nil
}

func (p RenditionPolicy) validate() error {
	if err := p.normalization.validate(); err != nil {
		return fmt.Errorf("rendition policy: %w", err)
	}
	return nil
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

func validateRenditionPolicy(policy RenditionPolicy) error {
	if err := policy.validate(); err != nil {
		return err
	}
	return nil
}

func invalidRenditionError(reason string) error {
	return errors.New("invalid rendition: " + reason)
}
