package redaction

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

// Text serializes only hash-bound retained source runs and generated markers.
func Text(plan Resolved) ([]byte, error) {
	if plan.Contract != "redaction-plan/v1" || !isSHA256(plan.MapSHA256) || !isSHA256(plan.RecipeSHA256) ||
		!isSHA256(plan.SHA256) || plan.PageCount < 1 || plan.PageCount > maxResolvePages || len(plan.Runs) > maxResolveFragments {
		return nil, errors.New("invalid resolved redaction plan")
	}
	if err := validateResolvedStructure(plan); err != nil {
		return nil, err
	}
	_, digest, err := CanonicalResolved(plan)
	if err != nil || digest != plan.SHA256 {
		return nil, errors.New("resolved redaction plan digest mismatch")
	}
	totalBytes := int64(plan.PageCount)
	for _, run := range plan.Runs {
		if int64(len(run.Text)) > 256<<20-totalBytes {
			return nil, &Problem{Code: problemRenderLimit}
		}
		totalBytes += int64(len(run.Text))
	}
	var output strings.Builder
	output.Grow(int(totalBytes))
	previousPage := 1
	pageRuns := 0
	for _, run := range plan.Runs {
		if run.Page < previousPage || run.Page < 1 || run.Page > plan.PageCount || !utf8.ValidString(run.Text) || run.Text == "" ||
			run.FontSizeMilliPoints <= 0 || len(run.Boxes) > maxResolveFragments {
			return nil, errors.New("invalid sanitized text run")
		}
		for previousPage < run.Page {
			output.WriteByte('\f')
			previousPage++
			pageRuns = 0
		}
		pageRuns++
		if pageRuns > maxResolvePageRuns {
			return nil, &Problem{Code: problemRenderLimit}
		}
		for _, box := range run.Boxes {
			if validateStandaloneBox(box) != nil || box.Page != run.Page {
				return nil, errors.New("invalid sanitized text run box")
			}
		}
		switch run.Kind {
		case "text":
			span := run.SourceSpan
			if span == nil || span.Start < 0 || span.End <= span.Start || span.End-span.Start != int64(len(run.Text)) ||
				!utf8Boundary(run.Text, 0) || !utf8Boundary(run.Text, int64(len(run.Text))) {
				return nil, errors.New("invalid retained source run")
			}
		case "redaction":
			if run.Text != redactionMarker || run.SourceSpan != nil || len(run.Boxes) == 0 {
				return nil, errors.New("invalid redaction marker run")
			}
		default:
			return nil, errors.New("unknown sanitized text run kind")
		}
		output.WriteString(run.Text)
	}
	for page := previousPage; page <= plan.PageCount; page++ {
		output.WriteByte('\f')
	}
	return []byte(output.String()), nil
}

func validateResolvedStructure(plan Resolved) error {
	if plan.Pages == nil || plan.Gaps == nil || plan.RedactBoxes == nil || plan.Removed == nil || plan.Runs == nil || plan.UncertainDecisionIDs == nil || plan.Regions == nil ||
		len(plan.Pages) != plan.PageCount ||
		len(plan.Gaps) > maxResolveFragments || len(plan.RedactBoxes) > maxResolveFragments || len(plan.Removed) > maxResolveFragments || len(plan.Regions) > maxResolveFragments {
		if len(plan.Gaps) > maxResolveFragments || len(plan.RedactBoxes) > maxResolveFragments || len(plan.Removed) > maxResolveFragments || len(plan.Regions) > maxResolveFragments {
			return &Problem{Code: problemRenderLimit}
		}
		return errors.New("resolved redaction plan is not canonical")
	}
	var pageEnd int64
	for index, page := range plan.Pages {
		if page.Number != index+1 || !isSHA256(page.FrameSHA256) || page.Width <= 0 || page.Height <= 0 ||
			page.Span.Start != pageEnd || page.Span.End < page.Span.Start {
			return errors.New("invalid resolved page authority")
		}
		pageEnd = page.Span.End
	}
	if !strictCanonicalBoxes(plan.RedactBoxes) {
		return errors.New("resolved mask boxes are not canonical")
	}
	for _, box := range plan.RedactBoxes {
		if !resolvedBoxValid(box, plan.Pages) {
			return errors.New("invalid resolved mask box")
		}
	}
	priorPage, priorEnd, spanPageIndex := 0, int64(-1), 0
	for _, span := range plan.Removed {
		for spanPageIndex < len(plan.Pages) && span.Start >= plan.Pages[spanPageIndex].Span.End {
			spanPageIndex++
		}
		if validateStandaloneSpan(span) != nil || spanPageIndex == len(plan.Pages) || span.Start < plan.Pages[spanPageIndex].Span.Start || span.End > plan.Pages[spanPageIndex].Span.End {
			return errors.New("invalid resolved removed spans")
		}
		page := plan.Pages[spanPageIndex].Number
		if page < priorPage || page == priorPage && span.Start <= priorEnd {
			return errors.New("invalid resolved removed spans")
		}
		priorPage, priorEnd = page, span.End
	}
	if !strictCanonicalGaps(plan.Gaps) {
		return errors.New("resolved gaps are not canonical")
	}
	for _, gap := range plan.Gaps {
		if !resolvedBoxValid(gap.Box, plan.Pages) {
			return errors.New("invalid resolved gap")
		}
		page := plan.Pages[gap.Box.Page-1]
		if gap.Anchor < page.Span.Start || gap.Anchor > page.Span.End || gap.Unordered && gap.Anchor != page.Span.Start {
			return errors.New("invalid resolved gap anchor")
		}
	}
	priorID := ""
	for _, id := range plan.UncertainDecisionIDs {
		if !canonicalUUIDv4(id) || id <= priorID {
			return errors.New("invalid resolved uncertainty identity")
		}
		priorID = id
	}
	totalRegionBoxes := 0
	for index, region := range plan.Regions {
		if totalRegionBoxes > maxResolveFragments-len(region.Boxes) {
			return &Problem{Code: problemRenderLimit}
		}
		if !boundedUTF8(region.Label, MaxDecisionLabelBytes) || region.Label == "" || region.Boxes == nil || region.DecisionIDs == nil || len(region.Boxes) == 0 {
			return errors.New("invalid resolved public region")
		}
		if index > 0 && compareRegion(plan.Regions[index-1], region) >= 0 || !strictCanonicalBoxes(region.Boxes) {
			return errors.New("resolved public regions are not canonical")
		}
		totalRegionBoxes += len(region.Boxes)
		for _, box := range region.Boxes {
			if !resolvedBoxValid(box, plan.Pages) {
				return errors.New("invalid resolved public region box")
			}
		}
		priorID = ""
		for _, id := range region.DecisionIDs {
			if !canonicalUUIDv4(id) || id <= priorID {
				return errors.New("invalid resolved public region decision identity")
			}
			priorID = id
		}
	}
	for _, run := range plan.Runs {
		if !strictCanonicalBoxes(run.Boxes) {
			return errors.New("sanitized text run boxes are not canonical")
		}
	}
	pageRunCounts := make([]int, plan.PageCount)
	for _, run := range plan.Runs {
		if run.Page >= 1 && run.Page <= plan.PageCount {
			pageRunCounts[run.Page-1]++
			if pageRunCounts[run.Page-1] > maxResolvePageRuns {
				return &Problem{Code: problemRenderLimit}
			}
		}
	}
	expected, err := expectedRunStructure(plan)
	if err != nil {
		return err
	}
	if len(expected) != len(plan.Runs) {
		return errors.New("redaction markers do not exactly account for removed content")
	}
	for index, want := range expected {
		got := plan.Runs[index]
		if got.Page != want.page || got.Kind != want.kind || got.Anchor != want.anchor {
			return errors.New("sanitized text runs do not cover pages canonically")
		}
		if want.span == nil {
			if got.SourceSpan != nil {
				return errors.New("redaction marker carries a source span")
			}
		} else if got.SourceSpan == nil || *got.SourceSpan != *want.span {
			return errors.New("retained source run intersects or omits removed content")
		}
	}
	return nil
}

type expectedRun struct {
	page   int
	kind   string
	anchor int64
	span   *Span
}

func expectedRunStructure(plan Resolved) ([]expectedRun, error) {
	recipe, ok := qualifiedRecipeBySHA256(plan.RecipeSHA256)
	if !ok {
		return nil, errors.New("resolved redaction plan has an unqualified recipe identity")
	}
	result := make([]expectedRun, 0, len(plan.Runs))
	removedIndex, gapIndex, boxIndex := 0, 0, 0
	work := &resolveWorkBudget{}
	appendMarker := func(page int, anchor int64) {
		if len(result) == 0 || result[len(result)-1].page != page || result[len(result)-1].kind != "redaction" {
			result = append(result, expectedRun{page: page, kind: "redaction", anchor: anchor})
		}
	}
	for _, page := range plan.Pages {
		removedStart := removedIndex
		for removedIndex < len(plan.Removed) && plan.Removed[removedIndex].Start >= page.Span.Start && plan.Removed[removedIndex].End <= page.Span.End {
			removedIndex++
		}
		removed := plan.Removed[removedStart:removedIndex]
		boxStart := boxIndex
		for boxIndex < len(plan.RedactBoxes) && plan.RedactBoxes[boxIndex].Page == page.Number {
			boxIndex++
		}
		fullMask, err := resolvedPageFullyMasked(page, plan.RedactBoxes[boxStart:boxIndex], recipe, work)
		if err != nil {
			return nil, err
		}
		var anchors []int64
		for gapIndex < len(plan.Gaps) && plan.Gaps[gapIndex].Box.Page == page.Number {
			gap := plan.Gaps[gapIndex]
			gapIndex++
			if gap.Unordered {
				if !fullMask {
					return nil, errors.New("unordered resolved gap is not protected by a full-page mask")
				}
				continue
			}
			anchors = append(anchors, gap.Anchor)
		}
		slices.Sort(anchors)
		anchors = slices.Compact(anchors)
		if fullMask && page.Span.Start == page.Span.End {
			appendMarker(page.Number, page.Span.Start)
			continue
		}
		if fullMask && (len(removed) != 1 || removed[0] != page.Span) {
			return nil, errors.New("full-page mask retains source text")
		}
		cursor := page.Span.Start
		pageRemovedIndex, anchorIndex := 0, 0
		for cursor < page.Span.End || pageRemovedIndex < len(removed) || anchorIndex < len(anchors) {
			for pageRemovedIndex < len(removed) && removed[pageRemovedIndex].End <= cursor {
				pageRemovedIndex++
			}
			for anchorIndex < len(anchors) && anchors[anchorIndex] < cursor {
				anchorIndex++
			}
			next := page.Span.End
			if pageRemovedIndex < len(removed) {
				next = min(next, removed[pageRemovedIndex].Start)
			}
			if anchorIndex < len(anchors) {
				next = min(next, anchors[anchorIndex])
			}
			if cursor < next {
				span := Span{Start: cursor, End: next}
				result = append(result, expectedRun{page: page.Number, kind: "text", anchor: span.Start, span: &span})
				cursor = next
			}
			if pageRemovedIndex < len(removed) && removed[pageRemovedIndex].Start <= cursor {
				interval := removed[pageRemovedIndex]
				appendMarker(page.Number, interval.Start)
				cursor = max(cursor, interval.End)
				pageRemovedIndex++
				for anchorIndex < len(anchors) && anchors[anchorIndex] <= interval.End {
					anchorIndex++
				}
				continue
			}
			if anchorIndex < len(anchors) && anchors[anchorIndex] == cursor {
				appendMarker(page.Number, anchors[anchorIndex])
				anchorIndex++
				continue
			}
			if cursor == page.Span.End {
				break
			}
		}
	}
	return result, nil
}

func strictCanonicalBoxes(boxes []Box) bool {
	for index := 1; index < len(boxes); index++ {
		if compareBox(boxes[index-1], boxes[index]) >= 0 {
			return false
		}
	}
	return true
}

func strictCanonicalGaps(gaps []Gap) bool {
	for index := 1; index < len(gaps); index++ {
		left, right := gaps[index-1], gaps[index]
		if left.Box.Page > right.Box.Page || left.Box.Page == right.Box.Page && (left.Anchor > right.Anchor ||
			left.Anchor == right.Anchor && (left.Unordered && !right.Unordered || left.Unordered == right.Unordered && compareBox(left.Box, right.Box) >= 0)) {
			return false
		}
	}
	return true
}

func resolvedBoxValid(box Box, pages []Page) bool {
	return box.Page >= 1 && box.Page <= len(pages) && validateFramedBox(box, pages[box.Page-1]) == nil
}

func resolvedPageFullyMasked(page Page, boxes []Box, recipe Recipe, work *resolveWorkBudget) (bool, error) {
	masks := make([]pixelRect, 0, len(boxes))
	for _, box := range boxes {
		if err := work.compare(); err != nil {
			return false, err
		}
		if box.Page == page.Number && box.FrameSHA256 == page.FrameSHA256 {
			mask, err := boxToPixel(page, box, recipe.DPI)
			if err != nil {
				return false, err
			}
			masks = append(masks, mask)
		}
	}
	width, height, err := pagePixels(page, recipe)
	if err != nil {
		return false, err
	}
	remaining, err := complementRects(pixelRect{page: page.Number, frame: page.FrameSHA256, x1: width, y1: height}, masks, work)
	return len(remaining) == 0, err
}

// ReviewBinding hashes the complete explicit review authority and no mutable
// declaration fields.
func ReviewBinding(input ReviewInput) (string, error) {
	if !canonicalUUIDv4(input.SetID) || !canonicalUUIDv4(input.MemberID) || !canonicalUUIDv4(input.VaultID) ||
		!canonicalUUIDv4(input.SourceVersionID) || input.Revision < 1 || input.Ordinal < 1 || input.NodeID < 1 ||
		input.SourceSize < 0 || input.PDFSize < 1 || !validMode(input.Mode) {
		return "", errors.New("invalid review input")
	}
	for _, digest := range []string{input.SourceSHA256, input.PDFSHA256, input.PageInventorySHA256, input.MapSHA256,
		input.MemberHash, input.InstructionsSHA256, input.RecipeSHA256, input.DecisionsSHA256, input.ResolvedSHA256} {
		if !isSHA256(digest) {
			return "", errors.New("invalid review input digest")
		}
	}
	encoded, err := canonical.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("canonicalize review input: %w", err)
	}
	return sha256Hex(encoded), nil
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}
