package production

import (
	"errors"
	"sort"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
)

func semanticUnits(m redaction.TextMap, frames []document.PageFrameV1, evidence document.NormalizedEvidenceV1, evidenceSHA string) ([]redaction.Unit, error) {
	kind := identityKind(evidence)
	units := make([]redaction.Unit, 0, len(evidence.Units))
	for _, evidenceUnit := range evidence.Units {
		var regions []*document.NormalizedEvidenceRegionV1
		if kind == "paragraph" && evidence.UnitKind == document.EvidenceUnitPage {
			regions = make([]*document.NormalizedEvidenceRegionV1, 0, len(evidenceUnit.Regions))
			for index := range evidenceUnit.Regions {
				if evidenceUnit.Regions[index].Kind == document.EvidenceRegionParagraph && evidenceUnit.Regions[index].Geometry != nil {
					regions = append(regions, &evidenceUnit.Regions[index])
				}
			}
			if len(evidenceUnit.Regions) != 0 && len(regions) == 0 {
				return nil, mappingError("page evidence has no positioned paragraph regions")
			}
		}
		if len(regions) == 0 {
			regions = append(regions, nil)
		}
		for _, region := range regions {
			expected := evidenceUnit.Text
			if region != nil {
				boundaries := runeByteBoundaries(evidenceUnit.Text)
				if region.TextRange.Start < 0 || region.TextRange.End <= region.TextRange.Start || region.TextRange.End >= len(boundaries) {
					return nil, mappingError("semantic evidence region text range is invalid")
				}
				expected = evidenceUnit.Text[boundaries[region.TextRange.Start]:boundaries[region.TextRange.End]]
			}
			spans, err := semanticEvidenceSpans(m, frames, evidence, evidenceUnit, region)
			if err != nil {
				return nil, err
			}
			var visible strings.Builder
			for _, span := range spans {
				visible.WriteString(m.Text[span.Start:span.End])
			}
			if normalizeVisibleText(expected) == "" || normalizeVisibleText(strings.TrimSpace(visible.String())) != normalizeVisibleText(expected) {
				return nil, mappingError("position-bound semantic evidence differs from mapped visible text")
			}
			identityInput := struct {
				EvidenceSHA256, Family, Kind string
				Locator                      document.EvidenceLocatorV1
				Order                        int
				RegionID                     string
				RegionOrder                  int
				TextRange                    document.EvidenceTextRangeV1
			}{EvidenceSHA256: evidenceSHA, Family: evidence.Family, Kind: kind, Locator: evidenceUnit.Locator, Order: evidenceUnit.Order}
			if region != nil {
				identityInput.RegionID, identityInput.RegionOrder, identityInput.TextRange = region.ID, region.Order, region.TextRange
			}
			identity, err := canonicalDigest(identityInput)
			if err != nil {
				return nil, err
			}
			unit := redaction.Unit{ID: kind + ":" + identity[:24], Kind: kind, Spans: spans}
			unit.Boxes = boxesForSpans(m, unit.Spans)
			if len(unit.Boxes) == 0 {
				return nil, mappingError("semantic evidence has no visible ink boxes")
			}
			units = append(units, unit)
		}
	}
	return units, nil
}

func semanticEvidenceSpans(m redaction.TextMap, frames []document.PageFrameV1, evidence document.NormalizedEvidenceV1, unit document.NormalizedEvidenceUnitV1, only *document.NormalizedEvidenceRegionV1) ([]redaction.Span, error) {
	pageNumber := int(unit.Locator.Start)
	if unit.Locator.Kind != document.EvidenceLocatorPage || pageNumber < 1 || pageNumber > len(m.Pages) {
		return nil, mappingError("semantic evidence has no supported retained page locator")
	}
	page := m.Pages[pageNumber-1]
	var selected []redaction.Span
	regions := unit.Regions
	if only != nil {
		regions = []document.NormalizedEvidenceRegionV1{*only}
	}
	for _, region := range regions {
		if region.Geometry == nil {
			continue
		}
		if pageNumber > len(frames) {
			return nil, mappingError("semantic evidence has no retained page frame")
		}
		regionBoxes, err := evidenceBoxes(frames[pageNumber-1], page, *region.Geometry)
		if err != nil {
			return nil, err
		}
		for _, atom := range m.Atoms {
			if atom.Span.Start < page.Span.Start || atom.Span.End > page.Span.End {
				continue
			}
			if boxesIntersectAny(atom.Boxes, regionBoxes) {
				selected = append(selected, atom.Span)
			}
		}
	}
	if len(selected) == 0 {
		if len(regions) != 0 {
			return nil, mappingError("explicit semantic region selects no mapped text")
		}
		unitsOnPage := 0
		for _, candidate := range evidence.Units {
			if candidate.Locator.Kind == document.EvidenceLocatorPage && candidate.Locator.Start == int64(pageNumber) {
				unitsOnPage++
			}
		}
		if unitsOnPage != 1 || page.Span.Start == page.Span.End {
			return nil, mappingError("semantic evidence page occurrence is ambiguous")
		}
		return []redaction.Span{page.Span}, nil
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].Start != selected[j].Start {
			return selected[i].Start < selected[j].Start
		}
		return selected[i].End < selected[j].End
	})
	merged := selected[:0]
	for _, span := range selected {
		if len(merged) != 0 && (span.Start <= merged[len(merged)-1].End ||
			allInvisibleWhitespace(m.Text[merged[len(merged)-1].End:span.Start])) {
			merged[len(merged)-1].End = max(merged[len(merged)-1].End, span.End)
			continue
		}
		merged = append(merged, span)
	}
	return merged, nil
}

func boxesIntersectAny(left, right []redaction.Box) bool {
	for _, a := range left {
		for _, b := range right {
			if a.Page == b.Page && a.X0 < b.X1 && a.X1 > b.X0 && a.Y0 < b.Y1 && a.Y1 > b.Y0 {
				return true
			}
		}
	}
	return false
}

func bindProvidedUnits(m redaction.TextMap, input []redaction.Unit) ([]redaction.Unit, error) {
	m.Units = input
	if err := redaction.ValidateTextMapBounds(m); err != nil {
		return nil, err
	}
	units := make([]redaction.Unit, len(input))
	copy(units, input)
	for i := range units {
		if units[i].ID == "" || (units[i].Kind != "paragraph" && units[i].Kind != "email_message" && units[i].Kind != "transcript_turn") || len(units[i].Spans) == 0 || len(units[i].Boxes) != 0 {
			return nil, errors.New("invalid provided semantic unit")
		}
		var end int64
		for j, span := range units[i].Spans {
			if span.Start < 0 || span.End <= span.Start || span.End > int64(len(m.Text)) || j > 0 && span.Start < end {
				return nil, errors.New("invalid provided semantic span")
			}
			end = span.End
		}
		units[i].Spans = pageBoundedSpans(m.Pages, units[i].Spans)
		if len(units[i].Spans) == 0 {
			return nil, errors.New("provided semantic spans do not intersect retained pages")
		}
		units[i].Boxes = boxesForSpans(m, units[i].Spans)
		if len(units[i].Boxes) == 0 {
			return nil, mappingError("provided semantic unit has no visible boxes")
		}
	}
	return units, nil
}

func pageBoundedSpans(pages []redaction.Page, spans []redaction.Span) []redaction.Span {
	result := make([]redaction.Span, 0, len(spans))
	for _, span := range spans {
		for _, page := range pages {
			intersection := redaction.Span{Start: max(span.Start, page.Span.Start), End: min(span.End, page.Span.End)}
			if intersection.End > intersection.Start {
				result = append(result, intersection)
			}
		}
	}
	return result
}

func boxesForSpans(m redaction.TextMap, spans []redaction.Span) []redaction.Box {
	var result []redaction.Box
	for _, atom := range m.Atoms {
		for _, span := range spans {
			if atom.Span.Start < span.End && atom.Span.End > span.Start {
				result = append(result, atom.Boxes...)
				break
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Page != b.Page {
			return a.Page < b.Page
		}
		if a.Y0 != b.Y0 {
			return a.Y0 < b.Y0
		}
		return a.X0 < b.X0
	})
	return result
}
