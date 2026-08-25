package document

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// BuildRenditionV1 derives sanitized Markdown, normalized units, and lexical
// spans from exactly one validated normalized-evidence/v1 authority.
func BuildRenditionV1(evidence NormalizedEvidenceV1, policy RenditionPolicy) (RenditionV1, error) {
	if err := validateRenditionPolicy(policy); err != nil {
		return RenditionV1{}, err
	}
	_, evidenceChecksum, err := MarshalNormalizedEvidenceV1(evidence)
	if err != nil {
		return RenditionV1{}, fmt.Errorf("validate rendition evidence: %w", err)
	}

	result := RenditionV1{
		Completeness:     evidence.Completeness,
		ContractVersion:  RenditionContractV1,
		EvidenceChecksum: evidenceChecksum,
		Units:            make([]NormalizedUnitV1, 0, len(evidence.Units)),
	}
	if evidence.Completeness == EvidenceDegradedProvenance {
		result.Warnings = []RenditionWarningV1{{Code: "degraded_provenance"}}
	}

	markdownUnits := make([]string, 0, len(evidence.Units))
	remaining := policy.normalization.maxDocumentChars
	truncated := false
	for _, evidenceUnit := range evidence.Units {
		separatorBudget := 0
		if len(markdownUnits) > 0 {
			separatorBudget = utf8.RuneCountInString("\n\n---\n\n")
		}
		unitBudget := min(policy.normalization.maxUnitChars, max(remaining-separatorBudget, 0))
		text, sourceTruncated, renditionTruncated, err := sanitizeRenditionMarkdown(
			evidenceUnit.Text,
			policy.normalization.maxLinkChars,
			policy.normalization.maxSourceUnitBytes,
			unitBudget,
		)
		if err != nil {
			return RenditionV1{}, fmt.Errorf("render evidence unit %d: %w", evidenceUnit.Order, err)
		}
		truncated = truncated || sourceTruncated || renditionTruncated
		unit := normalizedRenditionUnit(evidenceUnit, text)
		result.Units = append(result.Units, unit)
		if text != "" {
			remaining = max(0, remaining-separatorBudget)
			remaining = max(0, remaining-utf8.RuneCountInString(text))
			markdownUnits = append(markdownUnits, text)
			result.LexicalSegments = append(result.LexicalSegments, lexicalSegment(unit, len(result.LexicalSegments)))
		}
	}
	if truncated {
		result.Warnings = append(result.Warnings, RenditionWarningV1{Code: "truncated"})
	}

	if len(markdownUnits) == 0 {
		return RenditionV1{}, invalidRenditionError("evidence produced no readable text")
	}
	result.Markdown = []byte(strings.Join(markdownUnits, "\n\n---\n\n") + "\n")
	result.MarkdownChecksum = checksumBytes(result.Markdown)
	result.Checksum = renditionChecksum(result)
	return result, nil
}

func normalizedRenditionUnit(evidenceUnit NormalizedEvidenceUnitV1, text string) NormalizedUnitV1 {
	unit := NormalizedUnitV1{
		EvidenceUnitID: evidenceUnit.ID,
		HeadingPath:    append([]string(nil), evidenceUnit.HeadingPath...),
		Locator:        evidenceUnit.Locator,
		Order:          evidenceUnit.Order,
		Text:           text,
	}
	unit.Checksum = checksumStrings(
		RenditionContractV1, unit.EvidenceUnitID, strconv.Itoa(unit.Order), unit.Text,
		strings.Join(unit.HeadingPath, "\x00"),
		string(unit.Locator.Kind), string(unit.Locator.IndexOrigin),
		strconv.FormatInt(unit.Locator.Start, 10), strconv.FormatInt(unit.Locator.End, 10), unit.Locator.Name,
	)
	unit.ID = "rendition_unit_" + unit.Checksum
	return unit
}

func lexicalSegment(unit NormalizedUnitV1, order int) LexicalSegmentV1 {
	segment := LexicalSegmentV1{
		CharEnd: utf8.RuneCountInString(unit.Text),
		Order:   order,
		Text:    unit.Text,
		UnitID:  unit.ID,
	}
	segment.Checksum = checksumStrings(
		RenditionContractV1, unit.ID, strconv.Itoa(segment.Order),
		strconv.Itoa(segment.CharStart), strconv.Itoa(segment.CharEnd), segment.Text,
	)
	segment.ID = "lexical_segment_" + segment.Checksum
	return segment
}

func renditionChecksum(rendition RenditionV1) string {
	parts := []string{
		RenditionContractV1, rendition.EvidenceChecksum, string(rendition.Completeness), rendition.MarkdownChecksum,
	}
	for _, unit := range rendition.Units {
		parts = append(parts, unit.Checksum)
	}
	for _, segment := range rendition.LexicalSegments {
		parts = append(parts, segment.Checksum)
	}
	for _, warning := range rendition.Warnings {
		parts = append(parts, warning.Code)
	}
	return checksumStrings(parts...)
}

func checksumBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
