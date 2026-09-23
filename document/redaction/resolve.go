package redaction

import (
	"cmp"
	"math"
	"slices"
	"strings"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	maxResolvePages       = 1_000
	maxResolveAtoms       = 2_000_000
	maxResolveInputs      = 4_000_000
	maxResolveMapBytes    = int64(128 << 20)
	maxResolveFragments   = 1_000_000
	maxResolvePageRuns    = 16_384
	maxResolveWork        = 50_000_000
	redactionMarker       = "[REDACTED]"
	defaultRedactionLabel = "REDACTED"
	problemInvalidMode    = "invalid_mode"
	problemRenderLimit    = "render_limit"
)

type pixelRect struct {
	page           int
	frame          string
	x0, y0, x1, y1 int64
}

type resolvedDecision struct {
	id, action, label    string
	selector             Selector
	spans, acceptedSpans []Span
	boxes, acceptedBoxes []pixelRect
	addedAtoms           []int
}

type resolveWorkBudget struct {
	used int
}

func (budget *resolveWorkBudget) spend(count int) error {
	if count < 0 || count > maxResolveWork-budget.used {
		return &Problem{Code: problemRenderLimit}
	}
	budget.used += count
	return nil
}

func (budget *resolveWorkBudget) compare() error { return budget.spend(1) }

// Resolve expands immutable selectors into one deterministic raster and text
// removal plan. It performs no lookup and therefore cannot drift to a newer
// document head.
func Resolve(m TextMap, mode string, decisions []Decision, recipe Recipe) (Resolved, error) {
	if err := preflightResolve(m, decisions); err != nil {
		return Resolved{}, err
	}
	if err := ValidateMap(m); err != nil {
		return Resolved{}, err
	}
	if !validMode(mode) {
		return Resolved{}, &Problem{Code: problemInvalidMode}
	}
	if !qualifiedRecipe(recipe) {
		return Resolved{}, &Problem{Code: problemRenderLimit}
	}
	for _, page := range m.Pages {
		if _, _, err := pagePixels(page, recipe); err != nil {
			return Resolved{}, err
		}
	}
	recipeBytes, err := canonical.Marshal(recipe)
	if err != nil {
		return Resolved{}, err
	}
	recipeSHA := sha256Hex(recipeBytes)
	work := &resolveWorkBudget{}
	ordered, err := canonicalDecisionOrder(decisions, work)
	if err != nil {
		return Resolved{}, err
	}

	resolved := make([]resolvedDecision, 0, len(ordered))
	uncertain := make([]string, 0)
	for _, decision := range ordered {
		if decision.Selector.MapSHA256 != m.SHA256 {
			return Resolved{}, &Problem{Code: "source_stale", DecisionIDs: []string{decision.ID}}
		}
		selected, err := resolveOne(m, decision, recipe, work)
		if err != nil {
			return Resolved{}, err
		}
		resolved = append(resolved, selected)
		if decision.Uncertain {
			uncertain = append(uncertain, decision.ID)
		}
	}

	keeps := filterDecisions(resolved, "keep")
	redacts := filterDecisions(resolved, "redact")
	if mode == "keep_selected" && len(keeps) == 0 {
		wholeDocument, err := hasWholeDocumentRedaction(m, ordered, work)
		if err != nil {
			return Resolved{}, err
		}
		if !wholeDocument {
			return Resolved{}, &Problem{Code: "decision_conflict"}
		}
	}
	if err := checkDecisionConflicts(keeps, redacts, work); err != nil {
		return Resolved{}, err
	}
	if err := checkLabelConflicts(redacts, work); err != nil {
		return Resolved{}, err
	}
	if err := checkRequiredExpansions(m, mode, resolved, recipe, work); err != nil {
		return Resolved{}, err
	}

	redactByPage := make(map[int][]pixelRect, len(m.Pages))
	keepByPage := make(map[int][]pixelRect, len(m.Pages))
	for _, decision := range redacts {
		for _, box := range decision.boxes {
			redactByPage[box.page] = append(redactByPage[box.page], box)
		}
	}
	for _, decision := range keeps {
		for _, box := range decision.boxes {
			keepByPage[box.page] = append(keepByPage[box.page], box)
		}
	}

	finalPixels := make(map[int][]pixelRect, len(m.Pages))
	for _, page := range m.Pages {
		boxes := redactByPage[page.Number]
		if mode == "keep_selected" {
			complement, err := complementPage(page, keepByPage[page.Number], recipe, work)
			if err != nil {
				return Resolved{}, err
			}
			boxes = append(boxes, complement...)
		}
		boxes, err = unionPixelRects(boxes, work)
		if err != nil {
			return Resolved{}, err
		}
		finalPixels[page.Number] = boxes
	}
	if err := requireGapsMasked(m, finalPixels, recipe, work); err != nil {
		return Resolved{}, err
	}

	removed, err := removedText(m, mode, keeps, redacts, work)
	if err != nil {
		return Resolved{}, err
	}
	for _, page := range m.Pages {
		masked, err := pageFullyMasked(page, finalPixels[page.Number], recipe, work)
		if err != nil {
			return Resolved{}, err
		}
		if masked && page.Span.End > page.Span.Start {
			removed = append(removed, page.Span)
		}
	}
	removed, err = unionPageSpans(m, removed, work)
	if err != nil {
		return Resolved{}, err
	}
	runs, err := resolvedRuns(m, removed, finalPixels, recipe, work)
	if err != nil {
		return Resolved{}, err
	}
	redactBoxes := make([]Box, 0)
	for _, page := range m.Pages {
		for _, rectangle := range finalPixels[page.Number] {
			box, err := pixelToBox(page, rectangle, recipe.DPI)
			if err != nil {
				return Resolved{}, err
			}
			redactBoxes = append(redactBoxes, box)
		}
	}
	regions, err := resolvedRegions(m, mode, redacts, keepByPage, recipe, work)
	if err != nil {
		return Resolved{}, err
	}
	redactBoxes, err = compactBoxesBounded(redactBoxes, work)
	if err != nil {
		return Resolved{}, err
	}
	plan := Resolved{
		Contract: "redaction-plan/v1", MapSHA256: m.SHA256, RecipeSHA256: recipeSHA,
		PageCount: len(m.Pages), Pages: slices.Clone(m.Pages), Gaps: slices.Clone(m.Gaps),
		RedactBoxes: nonNilBoxes(redactBoxes), Removed: nonNilSpans(removed),
		Runs: nonNilRuns(runs), UncertainDecisionIDs: nonNilStrings(uncertain), Regions: nonNilRegions(regions),
	}
	_, plan.SHA256, err = CanonicalResolved(plan)
	if err != nil {
		return Resolved{}, err
	}
	return plan, nil
}

func preflightResolve(m TextMap, decisions []Decision) error {
	if len(m.Text) > int(maxResolveMapBytes) || len(m.Pages) == 0 || len(m.Pages) > maxResolvePages ||
		len(m.Atoms) > maxResolveAtoms || len(m.Units) > maxResolveAtoms || len(m.Gaps) > maxResolveAtoms ||
		len(decisions) > maxResolveAtoms {
		return &Problem{Code: problemRenderLimit}
	}
	total := 0
	add := func(count int) bool {
		if count < 0 || total > maxResolveInputs-count {
			return false
		}
		total += count
		return true
	}
	for _, atom := range m.Atoms {
		if !add(len(atom.Boxes)) {
			return &Problem{Code: problemRenderLimit}
		}
	}
	unitSpans := 0
	for _, unit := range m.Units {
		if !add(len(unit.Spans)) || !add(len(unit.Boxes)) {
			return &Problem{Code: problemRenderLimit}
		}
		if len(unit.Spans) > maxResolveInputs-unitSpans {
			return &Problem{Code: problemRenderLimit}
		}
		unitSpans += len(unit.Spans)
	}
	if !add(len(m.Gaps)) {
		return &Problem{Code: problemRenderLimit}
	}
	for _, decision := range decisions {
		if !add(len(decision.Selector.Boxes)) || !add(len(decision.Selector.Pages)) {
			return &Problem{Code: problemRenderLimit}
		}
	}
	if len(decisions) != 0 && len(m.Atoms) > maxResolveWork/len(decisions) {
		return &Problem{Code: problemRenderLimit}
	}
	if len(decisions) != 0 && len(m.Units) > maxResolveWork/len(decisions) {
		return &Problem{Code: problemRenderLimit}
	}
	if len(decisions) != 0 && len(m.Pages) > maxResolveWork/len(decisions) {
		return &Problem{Code: problemRenderLimit}
	}
	if len(m.Atoms) != 0 {
		if len(m.Units) > maxResolveWork/len(m.Atoms) {
			return &Problem{Code: problemRenderLimit}
		}
		remaining := maxResolveWork - len(m.Atoms)*len(m.Units)
		if unitSpans > remaining/len(m.Atoms) {
			return &Problem{Code: problemRenderLimit}
		}
	}
	if len(m.Gaps) > maxResolveFragments {
		return &Problem{Code: problemRenderLimit}
	}
	if _, exceeded, err := canonical.BoundedSize(m, maxResolveMapBytes); err != nil {
		return err
	} else if exceeded {
		return &Problem{Code: problemRenderLimit}
	}
	if _, exceeded, err := canonical.BoundedSize(decisions, maxResolveMapBytes); err != nil {
		return err
	} else if exceeded {
		return &Problem{Code: problemRenderLimit}
	}
	return nil
}

func canonicalDecisionOrder(decisions []Decision, work *resolveWorkBudget) ([]Decision, error) {
	if err := work.chargeSort(len(decisions)); err != nil {
		return nil, err
	}
	// Only validation is needed here. CanonicalDecisions also sorts and hashes
	// a separate copy, which would perform an extra, uncharged library sort.
	seen := make(map[string]struct{}, len(decisions))
	for _, decision := range decisions {
		if err := ValidateDecision(decision); err != nil {
			return nil, err
		}
		if _, duplicate := seen[decision.ID]; duplicate {
			return nil, decisionProblem(decision.ID, "duplicate decision ID")
		}
		seen[decision.ID] = struct{}{}
	}
	ordered := slices.Clone(decisions)
	heapSort(ordered, func(a, b Decision) int { return compareString(a.ID, b.ID) })
	return ordered, nil
}

func resolveOne(m TextMap, decision Decision, recipe Recipe, work *resolveWorkBudget) (resolvedDecision, error) {
	result := resolvedDecision{id: decision.ID, action: decision.Action, label: decision.Label, selector: decision.Selector}
	if result.label == "" {
		result.label = defaultRedactionLabel
	}
	selector := decision.Selector
	var seedBoxes []Box
	switch selector.Kind {
	case "text":
		if err := validateTextSpan(*selector.Span, m.Text, false); err != nil {
			return result, decisionProblem(decision.ID, err.Error())
		}
		result.spans = []Span{*selector.Span}
	case "rectangle":
		for _, box := range selector.Boxes {
			page, ok := pageByNumber(m, box.Page)
			if !ok || validateFramedBox(box, page) != nil {
				return result, &Problem{Code: "source_stale", DecisionIDs: []string{decision.ID}}
			}
		}
		seedBoxes = slices.Clone(selector.Boxes)
	case "paragraph", "email_message", "transcript_turn":
		found := false
		for _, unit := range m.Units {
			if err := work.compare(); err != nil {
				return result, err
			}
			if unit.ID == selector.UnitID && unit.Kind == selector.Kind {
				result.spans = slices.Clone(unit.Spans)
				seedBoxes = slices.Clone(unit.Boxes)
				found = true
				break
			}
		}
		if !found {
			return result, &Problem{Code: "mapping_incomplete", DecisionIDs: []string{decision.ID}}
		}
	case "page":
		for _, number := range selector.Pages {
			page, ok := pageByNumber(m, number)
			if !ok {
				return result, &Problem{Code: "source_stale", DecisionIDs: []string{decision.ID}}
			}
			result.spans = append(result.spans, page.Span)
			seedBoxes = append(seedBoxes, Box{Page: number, FrameSHA256: page.FrameSHA256, X1: page.Width, Y1: page.Height})
		}
	default:
		return result, decisionProblem(decision.ID, "unknown selector kind")
	}

	acceptedSpans, err := unionSpansBounded(result.spans, work)
	if err != nil {
		return result, err
	}
	result.acceptedSpans = acceptedSpans
	if selector.Kind == "text" {
		seedBoxes, err = atomBoxesForSpans(m.Atoms, result.spans, work)
		if err != nil {
			return result, err
		}
	}
	pixels := make([]pixelRect, 0, len(seedBoxes))
	for _, box := range seedBoxes {
		page, _ := pageByNumber(m, box.Page)
		pixel, err := boxToPixel(page, box, recipe.DPI)
		if err != nil {
			return result, err
		}
		if selector.Kind == "rectangle" || selector.Kind == "page" {
			result.acceptedBoxes = append(result.acceptedBoxes, pixel)
		}
		if decision.Action == "redact" {
			pixel = padPixel(pixel, page, recipe)
		}
		pixels = append(pixels, pixel)
	}
	result.boxes = pixels
	closedSpans, closedBoxes, err := atomClosure(m, result.spans, result.boxes, recipe, decision.Action == "redact", work)
	if err != nil {
		return result, err
	}
	result.spans, result.boxes = closedSpans, closedBoxes
	result.acceptedBoxes, err = unionPixelRects(result.acceptedBoxes, work)
	if err != nil {
		return result, err
	}
	for index, atom := range m.Atoms {
		intersects, err := spanIntersectsUnion(atom.Span, result.spans, work)
		if err != nil {
			return result, err
		}
		if !intersects {
			continue
		}
		accepted, err := decisionAcceptsAtom(m, result, atom, recipe.DPI, work)
		if err != nil {
			return result, err
		}
		if !accepted {
			result.addedAtoms = append(result.addedAtoms, index)
		}
	}
	if len(result.spans)+len(result.boxes) > maxResolveFragments {
		return result, &Problem{Code: problemRenderLimit}
	}
	return result, nil
}

func checkRequiredExpansions(m TextMap, mode string, decisions []resolvedDecision, recipe Recipe, work *resolveWorkBudget) error {
	action := "redact"
	if mode == "keep_selected" {
		action = "keep"
	}
	combined := resolvedDecision{action: action}
	for _, decision := range decisions {
		if decision.action == action {
			combined.acceptedSpans = append(combined.acceptedSpans, decision.acceptedSpans...)
			combined.acceptedBoxes = append(combined.acceptedBoxes, decision.acceptedBoxes...)
		}
	}
	var err error
	combined.acceptedSpans, err = unionSpansBounded(combined.acceptedSpans, work)
	if err != nil {
		return err
	}
	combined.acceptedBoxes, err = unionPixelRects(combined.acceptedBoxes, work)
	if err != nil {
		return err
	}
	for _, decision := range decisions {
		if decision.action != action || len(decision.addedAtoms) == 0 {
			continue
		}
		required := false
		for _, atomIndex := range decision.addedAtoms {
			covered, err := decisionAcceptsAtom(m, combined, m.Atoms[atomIndex], recipe.DPI, work)
			if err != nil {
				return err
			}
			if !covered {
				required = true
				break
			}
		}
		if required {
			original := Decision{ID: decision.id, Selector: decision.selector}
			// The exact selector shape is diagnostic convenience only; spans and
			// boxes below are the authoritative complete closure.
			problem, err := expansionProblem(m, original, decision.spans, recipe, work)
			if err != nil {
				return err
			}
			return problem
		}
	}
	return nil
}

func decisionAcceptsAtom(m TextMap, decision resolvedDecision, atom Atom, dpi int, work *resolveWorkBudget) (bool, error) {
	for _, span := range decision.acceptedSpans {
		if err := work.compare(); err != nil {
			return false, err
		}
		if span.Start <= atom.Span.Start && span.End >= atom.Span.End {
			return true, nil
		}
	}
	if len(decision.acceptedBoxes) == 0 {
		return false, nil
	}
	for _, box := range atom.Boxes {
		page, _ := pageByNumber(m, box.Page)
		pixel, err := boxToPixel(page, box, dpi)
		if err != nil {
			return false, err
		}
		covered, err := pixelRectCovered(pixel, decision.acceptedBoxes, work)
		if err != nil || !covered {
			return false, err
		}
	}
	return true, nil
}

func spanIntersectsUnion(span Span, spans []Span, work *resolveWorkBudget) (bool, error) {
	steps := 1
	for length := len(spans); length > 1; length = (length + 1) / 2 {
		steps++
	}
	if err := work.spend(steps); err != nil {
		return false, err
	}
	index, _ := slices.BinarySearchFunc(spans, span.Start, func(candidate Span, start int64) int {
		if candidate.End <= start {
			return -1
		}
		return 0
	})
	return index < len(spans) && spansOverlap(span, spans[index]), nil
}

func expansionProblem(m TextMap, decision Decision, spans []Span, recipe Recipe, work *resolveWorkBudget) (*Problem, error) {
	selectionBoxes := make([]Box, 0)
	if decision.Selector.Kind == "rectangle" {
		selectionBoxes = append(selectionBoxes, decision.Selector.Boxes...)
	}
	atomBoxes, err := atomBoxesForSpans(m.Atoms, spans, work)
	if err != nil {
		return nil, err
	}
	selectionBoxes = append(selectionBoxes, atomBoxes...)
	pixels := make([]pixelRect, 0, len(selectionBoxes))
	for _, box := range selectionBoxes {
		page, _ := pageByNumber(m, box.Page)
		pixel, err := boxToPixel(page, box, recipe.DPI)
		if err != nil {
			return nil, err
		}
		pixels = append(pixels, pixel)
	}
	pixels, err = unionPixelRects(pixels, work)
	if err != nil {
		return nil, err
	}
	boxes := make([]Box, 0, len(pixels))
	for _, rectangle := range pixels {
		page, _ := pageByNumber(m, rectangle.page)
		box, err := pixelToBox(page, rectangle, recipe.DPI)
		if err != nil {
			return nil, err
		}
		boxes = append(boxes, box)
	}
	boxes, err = compactBoxesBounded(boxes, work)
	if err != nil {
		return nil, err
	}
	expandedSpans, err := unionSpansBounded(spans, work)
	if err != nil {
		return nil, err
	}
	problem := &Problem{Code: "selection_expansion_required", DecisionIDs: []string{decision.ID}, ExpandedMapSHA256: m.SHA256,
		ExpandedSpans: nonNilSpans(expandedSpans), ExpandedBoxes: nonNilBoxes(boxes)}
	switch decision.Selector.Kind {
	case "text":
		if len(problem.ExpandedSpans) == 1 {
			span := problem.ExpandedSpans[0]
			problem.Expanded = &Selector{Kind: "text", MapSHA256: m.SHA256, Span: &span}
		}
	case "rectangle":
		problem.Expanded = &Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: boxes}
	}
	return problem, nil
}

func atomClosure(m TextMap, spans []Span, boxes []pixelRect, recipe Recipe, redact bool, work *resolveWorkBudget) ([]Span, []pixelRect, error) {
	included := make([]bool, len(m.Atoms))
	for changed := true; changed; {
		changed = false
		for index, atom := range m.Atoms {
			if included[index] {
				continue
			}
			intersects := false
			for _, span := range spans {
				if err := work.compare(); err != nil {
					return nil, nil, err
				}
				if spansOverlap(atom.Span, span) {
					intersects = true
					break
				}
			}
			if !intersects {
				for _, box := range atom.Boxes {
					page, _ := pageByNumber(m, box.Page)
					pixel, err := boxToPixel(page, box, recipe.DPI)
					if err != nil {
						return nil, nil, err
					}
					for _, selectedBox := range boxes {
						if err := work.compare(); err != nil {
							return nil, nil, err
						}
						if pixelOverlap(pixel, selectedBox) {
							intersects = true
							break
						}
					}
					if intersects {
						break
					}
				}
			}
			if !intersects {
				continue
			}
			included[index], changed = true, true
			spans = append(spans, atom.Span)
			for _, box := range atom.Boxes {
				page, _ := pageByNumber(m, box.Page)
				pixel, err := boxToPixel(page, box, recipe.DPI)
				if err != nil {
					return nil, nil, err
				}
				if redact {
					pixel = padPixel(pixel, page, recipe)
				}
				boxes = append(boxes, pixel)
			}
		}
	}
	spans, err := unionSpansBounded(spans, work)
	if err != nil {
		return nil, nil, err
	}
	boxes, err = compactPixelRectsBounded(boxes, work)
	if err != nil {
		return nil, nil, err
	}
	return spans, boxes, nil
}

func filterDecisions(input []resolvedDecision, action string) []resolvedDecision {
	result := make([]resolvedDecision, 0)
	for _, decision := range input {
		if decision.action == action {
			result = append(result, decision)
		}
	}
	return result
}

func checkDecisionConflicts(keeps, redacts []resolvedDecision, work *resolveWorkBudget) error {
	if len(keeps) != 0 && len(redacts) > maxResolveWork/len(keeps) {
		return &Problem{Code: problemRenderLimit}
	}
	for _, keep := range keeps {
		for _, redact := range redacts {
			conflict, err := boundedDecisionIntersection(keep, redact, work)
			if err != nil {
				return err
			}
			if conflict {
				return &Problem{Code: "decision_conflict", DecisionIDs: sortedIDs(keep.id, redact.id)}
			}
		}
	}
	return nil
}

func checkLabelConflicts(redacts []resolvedDecision, work *resolveWorkBudget) error {
	if len(redacts) > 1 && len(redacts) > maxResolveWork/(len(redacts)-1) {
		return &Problem{Code: problemRenderLimit}
	}
	for left := range redacts {
		for right := left + 1; right < len(redacts); right++ {
			if redacts[left].label == redacts[right].label {
				continue
			}
			intersects, err := boundedPixelIntersection(redacts[left].boxes, redacts[right].boxes, work)
			if err != nil {
				return err
			}
			if intersects {
				return &Problem{Code: "decision_conflict", DecisionIDs: sortedIDs(redacts[left].id, redacts[right].id)}
			}
		}
	}
	return nil
}

func hasWholeDocumentRedaction(m TextMap, decisions []Decision, work *resolveWorkBudget) (bool, error) {
	for _, decision := range decisions {
		if err := work.compare(); err != nil {
			return false, err
		}
		if decision.Action != "redact" || decision.Selector.Kind != "page" || len(decision.Selector.Pages) != len(m.Pages) {
			continue
		}
		match := true
		for index, page := range decision.Selector.Pages {
			if err := work.compare(); err != nil {
				return false, err
			}
			match = match && page == index+1
		}
		if match {
			return true, nil
		}
	}
	return false, nil
}

func complementPage(page Page, protected []pixelRect, recipe Recipe, work *resolveWorkBudget) ([]pixelRect, error) {
	width, height, err := pagePixels(page, recipe)
	if err != nil {
		return nil, err
	}
	protected, err = unionPixelRects(protected, work)
	if err != nil {
		return nil, err
	}
	return complementRects(pixelRect{page: page.Number, frame: page.FrameSHA256, x1: width, y1: height}, protected, work)
}

func complementRects(canvas pixelRect, protected []pixelRect, work *resolveWorkBudget) ([]pixelRect, error) {
	if len(protected) != 0 && len(protected) > maxResolveWork/max(2, len(protected)*2) {
		return nil, &Problem{Code: problemRenderLimit}
	}
	xs := []int64{canvas.x0, canvas.x1}
	for _, rectangle := range protected {
		if err := work.compare(); err != nil {
			return nil, err
		}
		if pixelOverlap(rectangle, canvas) {
			xs = append(xs, max(canvas.x0, rectangle.x0), min(canvas.x1, rectangle.x1))
		}
	}
	if err := work.chargeSort(len(xs)); err != nil {
		return nil, err
	}
	heapSort(xs, cmp.Compare[int64])
	xs = slices.Compact(xs)
	result := make([]pixelRect, 0)
	for xIndex := 0; xIndex+1 < len(xs); xIndex++ {
		x0, x1 := xs[xIndex], xs[xIndex+1]
		if x1 <= x0 {
			continue
		}
		yIntervals := make([]Span, 0)
		for _, rectangle := range protected {
			if err := work.compare(); err != nil {
				return nil, err
			}
			if pixelOverlap(rectangle, canvas) && rectangle.x0 < x1 && rectangle.x1 > x0 {
				yIntervals = append(yIntervals, Span{max(canvas.y0, rectangle.y0), min(canvas.y1, rectangle.y1)})
			}
		}
		if err := work.chargeSort(len(yIntervals)); err != nil {
			return nil, err
		}
		yIntervals = unionSpans(yIntervals)
		y := canvas.y0
		for _, interval := range yIntervals {
			if interval.Start > y {
				var err error
				result, err = appendMergeX(result, pixelRect{canvas.page, canvas.frame, x0, y, x1, interval.Start}, work)
				if err != nil {
					return nil, err
				}
			}
			y = max(y, interval.End)
		}
		if y < canvas.y1 {
			var err error
			result, err = appendMergeX(result, pixelRect{canvas.page, canvas.frame, x0, y, x1, canvas.y1}, work)
			if err != nil {
				return nil, err
			}
		}
		if len(result) > maxResolveFragments {
			return nil, &Problem{Code: problemRenderLimit}
		}
	}
	return result, nil
}

func unionPixelRects(input []pixelRect, work *resolveWorkBudget) ([]pixelRect, error) {
	if len(input) == 0 {
		return []pixelRect{}, nil
	}
	if len(input) > maxResolveWork/max(2, len(input)*2) {
		return nil, &Problem{Code: problemRenderLimit}
	}
	byPage := make(map[int][]pixelRect)
	pages := make([]int, 0)
	for _, rectangle := range input {
		if _, present := byPage[rectangle.page]; !present {
			pages = append(pages, rectangle.page)
		}
		byPage[rectangle.page] = append(byPage[rectangle.page], rectangle)
	}
	if err := work.chargeSort(len(pages)); err != nil {
		return nil, err
	}
	heapSort(pages, cmp.Compare[int])
	result := make([]pixelRect, 0)
	for _, page := range pages {
		rectangles := byPage[page]
		xs := make([]int64, 0, len(rectangles)*2)
		for _, rectangle := range rectangles {
			xs = append(xs, rectangle.x0, rectangle.x1)
		}
		if err := work.chargeSort(len(xs)); err != nil {
			return nil, err
		}
		heapSort(xs, cmp.Compare[int64])
		xs = slices.Compact(xs)
		for index := 0; index+1 < len(xs); index++ {
			x0, x1 := xs[index], xs[index+1]
			y := make([]Span, 0)
			for _, rectangle := range rectangles {
				if err := work.compare(); err != nil {
					return nil, err
				}
				if rectangle.x0 < x1 && rectangle.x1 > x0 {
					y = append(y, Span{rectangle.y0, rectangle.y1})
				}
			}
			if err := work.chargeSort(len(y)); err != nil {
				return nil, err
			}
			for _, interval := range unionSpans(y) {
				var err error
				result, err = appendMergeX(result, pixelRect{page, rectangles[0].frame, x0, interval.Start, x1, interval.End}, work)
				if err != nil {
					return nil, err
				}
				if len(result) > maxResolveFragments {
					return nil, &Problem{Code: problemRenderLimit}
				}
			}
		}
	}
	return result, nil
}

func appendMergeX(result []pixelRect, rectangle pixelRect, work *resolveWorkBudget) ([]pixelRect, error) {
	for index := len(result) - 1; index >= 0; index-- {
		if err := work.compare(); err != nil {
			return nil, err
		}
		prior := &result[index]
		if prior.page != rectangle.page || prior.x1 != rectangle.x0 {
			break
		}
		if prior.y0 == rectangle.y0 && prior.y1 == rectangle.y1 && prior.frame == rectangle.frame {
			prior.x1 = rectangle.x1
			return result, nil
		}
	}
	return append(result, rectangle), nil
}

func requireGapsMasked(m TextMap, masks map[int][]pixelRect, recipe Recipe, work *resolveWorkBudget) error {
	for _, gap := range m.Gaps {
		page, _ := pageByNumber(m, gap.Box.Page)
		if gap.Unordered {
			width, height, err := pagePixels(page, recipe)
			if err != nil {
				return err
			}
			remaining, err := complementRects(pixelRect{page: page.Number, frame: page.FrameSHA256, x1: width, y1: height}, masks[page.Number], work)
			if err != nil {
				return err
			}
			if len(remaining) != 0 {
				return &Problem{Code: "mapping_incomplete"}
			}
			continue
		}
		pixels, err := boxToPixel(page, gap.Box, recipe.DPI)
		if err != nil {
			return err
		}
		remaining, err := complementRects(pixels, masks[pixels.page], work)
		if err != nil {
			return err
		}
		if len(remaining) != 0 {
			return &Problem{Code: "mapping_incomplete"}
		}
	}
	return nil
}

func qualifiedRecipe(recipe Recipe) bool {
	return (recipe.DPI == 300 || recipe.DPI == 600) && recipe == qualifiedRecipeAtDPI(recipe.DPI)
}

func qualifiedRecipeAtDPI(dpi int) Recipe {
	if dpi != 300 && dpi != 600 {
		return Recipe{}
	}
	return Recipe{
		Contract: "raster-redaction/v1", RendererSHA256: "f651270c675cac90702b762f4b95d2b34e365cdb374065e40af016f4f0f304ea",
		WriterVersion: "docbank-fresh-objects/v2", FontSHA256: "c2f3b4d463500a2ddcd3849cded1fceeb9fd6d1c32e6cbecd568453ba50fc68f",
		DPI: dpi, PaddingPixels: 2, MaxPixels: 40_000_000, MaxAxis: 16_384,
		WASMMemoryBytes: 512 << 20, PageTimeoutSeconds: 60, QualifiedPeakRSSBytes: 1 << 30, MaxStagingBytes: 100 << 30,
	}
}

func qualifiedRecipeBySHA256(digest string) (Recipe, bool) {
	for _, dpi := range []int{300, 600} {
		recipe := qualifiedRecipeAtDPI(dpi)
		encoded, err := canonical.Marshal(recipe)
		if err == nil && sha256Hex(encoded) == digest {
			return recipe, true
		}
	}
	return Recipe{}, false
}

func removedText(m TextMap, mode string, keeps, redacts []resolvedDecision, work *resolveWorkBudget) ([]Span, error) {
	var removed []Span
	if mode == "keep_selected" {
		var retained []Span
		for _, keep := range keeps {
			retained = append(retained, keep.spans...)
		}
		var err error
		retained, err = unionSpansBounded(retained, work)
		if err != nil {
			return nil, err
		}
		for _, page := range m.Pages {
			cursor := page.Span.Start
			for _, span := range retained {
				if err := work.compare(); err != nil {
					return nil, err
				}
				start, end := max(span.Start, page.Span.Start), min(span.End, page.Span.End)
				if end <= start {
					continue
				}
				if start > cursor {
					removed = append(removed, Span{cursor, start})
				}
				cursor = max(cursor, end)
			}
			if cursor < page.Span.End {
				removed = append(removed, Span{cursor, page.Span.End})
			}
		}
	}
	for _, redact := range redacts {
		removed = append(removed, redact.spans...)
	}
	return unionPageSpans(m, removed, work)
}

func resolvedRuns(m TextMap, removed []Span, masks map[int][]pixelRect, recipe Recipe, work *resolveWorkBudget) ([]Run, error) {
	runs := make([]Run, 0)
	for _, page := range m.Pages {
		pageRunStart := len(runs)
		pageRemoved, err := clippedSpans(removed, page.Span, work)
		if err != nil {
			return nil, err
		}
		anchors := make([]int64, 0)
		gapBoxes := make(map[int64][]Box)
		for _, gap := range m.Gaps {
			if err := work.compare(); err != nil {
				return nil, err
			}
			if gap.Box.Page == page.Number && !gap.Unordered {
				anchors = append(anchors, gap.Anchor)
				gapBoxes[gap.Anchor] = append(gapBoxes[gap.Anchor], gap.Box)
			}
		}
		if err := work.chargeSort(len(anchors)); err != nil {
			return nil, err
		}
		heapSort(anchors, cmp.Compare[int64])
		anchors = slices.Compact(anchors)
		workUnits := 2*len(pageRemoved) + len(anchors) + 1
		if len(m.Atoms) != 0 && workUnits > maxResolveWork/len(m.Atoms) {
			return nil, &Problem{Code: problemRenderLimit}
		}
		if err := work.spend(len(m.Atoms) * workUnits); err != nil {
			return nil, err
		}
		cursor := page.Span.Start
		removedIndex, anchorIndex := 0, 0
		for cursor < page.Span.End || removedIndex < len(pageRemoved) || anchorIndex < len(anchors) {
			for removedIndex < len(pageRemoved) && pageRemoved[removedIndex].End <= cursor {
				removedIndex++
			}
			for anchorIndex < len(anchors) && anchors[anchorIndex] < cursor {
				anchorIndex++
			}
			next := page.Span.End
			if removedIndex < len(pageRemoved) {
				next = min(next, pageRemoved[removedIndex].Start)
			}
			if anchorIndex < len(anchors) {
				next = min(next, anchors[anchorIndex])
			}
			if cursor < next {
				run, err := sourceRun(m, page, Span{cursor, next}, work)
				if err != nil {
					return nil, err
				}
				runs = append(runs, run)
				cursor = next
			}
			if removedIndex < len(pageRemoved) && pageRemoved[removedIndex].Start <= cursor {
				interval := pageRemoved[removedIndex]
				boxes, err := boxesForSpan(m, interval, work)
				if err != nil {
					return nil, err
				}
				for anchorIndex < len(anchors) && anchors[anchorIndex] <= interval.End {
					boxes = append(boxes, gapBoxes[anchors[anchorIndex]]...)
					anchorIndex++
				}
				runs, err = appendMarker(runs, page, interval.Start, boxes, work)
				if err != nil {
					return nil, err
				}
				cursor = max(cursor, interval.End)
				removedIndex++
				continue
			}
			if anchorIndex < len(anchors) && anchors[anchorIndex] == cursor {
				var err error
				runs, err = appendMarker(runs, page, anchors[anchorIndex], gapBoxes[anchors[anchorIndex]], work)
				if err != nil {
					return nil, err
				}
				anchorIndex++
				continue
			}
			if cursor == page.Span.End {
				break
			}
		}
		if len(runs) == pageRunStart {
			masked, err := pageFullyMasked(page, masks[page.Number], recipe, work)
			if err != nil {
				return nil, err
			}
			if masked {
				runs, err = appendMarker(runs, page, page.Span.Start, []Box{{Page: page.Number, FrameSHA256: page.FrameSHA256, X1: page.Width, Y1: page.Height}}, work)
				if err != nil {
					return nil, err
				}
			}
		}
		if len(runsForPage(runs, page.Number)) > maxResolvePageRuns {
			return nil, &Problem{Code: problemRenderLimit}
		}
	}
	if len(runs) > maxResolveFragments {
		return nil, &Problem{Code: problemRenderLimit}
	}
	return runs, nil
}

func pageFullyMasked(page Page, masks []pixelRect, recipe Recipe, work *resolveWorkBudget) (bool, error) {
	width, height, err := pagePixels(page, recipe)
	if err != nil {
		return false, err
	}
	remaining, err := complementRects(pixelRect{page: page.Number, frame: page.FrameSHA256, x1: width, y1: height}, masks, work)
	return len(remaining) == 0, err
}

func sourceRun(m TextMap, page Page, span Span, work *resolveWorkBudget) (Run, error) {
	boxes, err := boxesForSpan(m, span, work)
	if err != nil {
		return Run{}, err
	}
	return Run{Kind: "text", Text: m.Text[span.Start:span.End], Page: page.Number, Anchor: span.Start, SourceSpan: &span,
		Boxes: boxes, FontSizeMilliPoints: 11_000}, nil
}

func appendMarker(runs []Run, page Page, anchor int64, boxes []Box, work *resolveWorkBudget) ([]Run, error) {
	if len(runs) != 0 && runs[len(runs)-1].Page == page.Number && runs[len(runs)-1].Kind == "redaction" {
		var err error
		runs[len(runs)-1].Boxes, err = compactBoxesBounded(append(runs[len(runs)-1].Boxes, boxes...), work)
		return runs, err
	}
	if len(boxes) == 0 {
		boxes = []Box{{Page: page.Number, FrameSHA256: page.FrameSHA256, X1: page.Width, Y1: page.Height}}
	}
	boxes, err := compactBoxesBounded(boxes, work)
	if err != nil {
		return nil, err
	}
	return append(runs, Run{Kind: "redaction", Text: redactionMarker, Page: page.Number, Anchor: anchor,
		Boxes: boxes, FontSizeMilliPoints: 11_000}), nil
}

func resolvedRegions(m TextMap, mode string, redacts []resolvedDecision, keeps map[int][]pixelRect, recipe Recipe, work *resolveWorkBudget) ([]RedactionRegion, error) {
	type regionGroup struct {
		boxes []pixelRect
		ids   []string
	}
	groups := make(map[string]*regionGroup)
	explicitByPage := make(map[int][]pixelRect)
	for _, decision := range redacts {
		group := groups[decision.label]
		if group == nil {
			group = &regionGroup{}
			groups[decision.label] = group
		}
		group.boxes = append(group.boxes, decision.boxes...)
		group.ids = append(group.ids, decision.id)
		for _, rectangle := range decision.boxes {
			explicitByPage[rectangle.page] = append(explicitByPage[rectangle.page], rectangle)
		}
	}
	if mode == "keep_selected" {
		group := groups[defaultRedactionLabel]
		if group == nil {
			group = &regionGroup{}
			groups[defaultRedactionLabel] = group
		}
		for _, page := range m.Pages {
			complement, err := complementPage(page, keeps[page.Number], recipe, work)
			if err != nil {
				return nil, err
			}
			for _, rectangle := range complement {
				remainder, err := complementRects(rectangle, explicitByPage[page.Number], work)
				if err != nil {
					return nil, err
				}
				group.boxes = append(group.boxes, remainder...)
			}
		}
	}
	labels := make([]string, 0, len(groups))
	for label := range groups {
		labels = append(labels, label)
	}
	if err := work.chargeSort(len(labels)); err != nil {
		return nil, err
	}
	heapSort(labels, cmp.Compare[string])
	regions := make([]RedactionRegion, 0, len(labels))
	for _, label := range labels {
		group := groups[label]
		pixels, err := unionPixelRects(group.boxes, work)
		if err != nil {
			return nil, err
		}
		if len(pixels) == 0 {
			continue
		}
		boxes := make([]Box, 0, len(pixels))
		for _, rectangle := range pixels {
			page, _ := pageByNumber(m, rectangle.page)
			box, err := pixelToBox(page, rectangle, recipe.DPI)
			if err != nil {
				return nil, err
			}
			boxes = append(boxes, box)
		}
		if err := work.chargeSort(len(group.ids)); err != nil {
			return nil, err
		}
		heapSort(group.ids, cmp.Compare[string])
		group.ids = slices.Compact(group.ids)
		boxes, err = compactBoxesBounded(boxes, work)
		if err != nil {
			return nil, err
		}
		regions = append(regions, RedactionRegion{Boxes: boxes, Label: label, DecisionIDs: nonNilStrings(group.ids)})
	}
	if err := work.chargeSort(len(regions)); err != nil {
		return nil, err
	}
	heapSort(regions, compareRegion)
	return regions, nil
}

func compareRegion(a, b RedactionRegion) int {
	if len(a.Boxes) == 0 || len(b.Boxes) == 0 {
		return len(a.Boxes) - len(b.Boxes)
	}
	if order := compareBox(a.Boxes[0], b.Boxes[0]); order != 0 {
		return order
	}
	if a.Label != b.Label {
		return compareString(a.Label, b.Label)
	}
	return compareString(strings.Join(a.DecisionIDs, "\x00"), strings.Join(b.DecisionIDs, "\x00"))
}

func pagePixels(page Page, recipe Recipe) (int64, int64, error) {
	if page.Width > math.MaxInt64/int64(recipe.DPI) || page.Height > math.MaxInt64/int64(recipe.DPI) {
		return 0, 0, &Problem{Code: problemRenderLimit}
	}
	width := ceilDiv(page.Width*int64(recipe.DPI), 10_000)
	height := ceilDiv(page.Height*int64(recipe.DPI), 10_000)
	if width <= 0 || height <= 0 || recipe.MaxAxis > 0 && (width > recipe.MaxAxis || height > recipe.MaxAxis) ||
		recipe.MaxPixels > 0 && width > recipe.MaxPixels/height {
		return 0, 0, &Problem{Code: problemRenderLimit}
	}
	return width, height, nil
}

func boxToPixel(page Page, box Box, dpi int) (pixelRect, error) {
	if dpi <= 0 || validateFramedBox(box, page) != nil || page.Width > math.MaxInt64/int64(dpi) || page.Height > math.MaxInt64/int64(dpi) {
		return pixelRect{}, &Problem{Code: problemRenderLimit}
	}
	return pixelRect{page.Number, page.FrameSHA256,
		box.X0 * int64(dpi) / 10_000, box.Y0 * int64(dpi) / 10_000,
		ceilDiv(box.X1*int64(dpi), 10_000), ceilDiv(box.Y1*int64(dpi), 10_000)}, nil
}

func pixelToBox(page Page, rectangle pixelRect, dpi int) (Box, error) {
	if dpi <= 0 || rectangle.page != page.Number || rectangle.frame != page.FrameSHA256 || rectangle.x0 < 0 || rectangle.y0 < 0 || rectangle.x1 <= rectangle.x0 || rectangle.y1 <= rectangle.y0 ||
		rectangle.x0 > math.MaxInt64/10_000 || rectangle.y0 > math.MaxInt64/10_000 || rectangle.x1 > math.MaxInt64/10_000 || rectangle.y1 > math.MaxInt64/10_000 {
		return Box{}, &Problem{Code: problemRenderLimit}
	}
	box := Box{Page: page.Number, FrameSHA256: page.FrameSHA256,
		X0: ceilDiv(rectangle.x0*10_000, int64(dpi)), Y0: ceilDiv(rectangle.y0*10_000, int64(dpi)),
		X1: rectangle.x1 * 10_000 / int64(dpi), Y1: rectangle.y1 * 10_000 / int64(dpi)}
	if box.X1 > page.Width {
		box.X1 = page.Width
	}
	if box.Y1 > page.Height {
		box.Y1 = page.Height
	}
	if validateFramedBox(box, page) != nil {
		return Box{}, &Problem{Code: problemRenderLimit}
	}
	roundTrip, err := boxToPixel(page, box, dpi)
	if err != nil || roundTrip != rectangle {
		return Box{}, &Problem{Code: problemRenderLimit}
	}
	return box, nil
}

func padPixel(rectangle pixelRect, page Page, recipe Recipe) pixelRect {
	width, height, _ := pagePixels(page, recipe)
	padding := int64(recipe.PaddingPixels)
	rectangle.x0 = max(int64(0), rectangle.x0-padding)
	rectangle.y0 = max(int64(0), rectangle.y0-padding)
	rectangle.x1 = min(width, rectangle.x1+padding)
	rectangle.y1 = min(height, rectangle.y1+padding)
	return rectangle
}

func unionPageSpans(m TextMap, spans []Span, work *resolveWorkBudget) ([]Span, error) {
	result := make([]Span, 0)
	for _, page := range m.Pages {
		clipped, err := clippedSpans(spans, page.Span, work)
		if err != nil {
			return nil, err
		}
		result = append(result, clipped...)
	}
	return result, nil
}

func clippedSpans(spans []Span, bounds Span, work *resolveWorkBudget) ([]Span, error) {
	var result []Span
	for _, span := range spans {
		if err := work.compare(); err != nil {
			return nil, err
		}
		start, end := max(span.Start, bounds.Start), min(span.End, bounds.End)
		if end > start {
			result = append(result, Span{start, end})
		}
	}
	return unionSpansBounded(result, work)
}

func unionSpansBounded(spans []Span, work *resolveWorkBudget) ([]Span, error) {
	if err := work.chargeSort(len(spans)); err != nil {
		return nil, err
	}
	return unionSpans(spans), nil
}

func unionSpans(spans []Span) []Span {
	spans = slices.Clone(spans)
	heapSort(spans, func(a, b Span) int {
		if a.Start != b.Start {
			return compareInt64(a.Start, b.Start)
		}
		return compareInt64(a.End, b.End)
	})
	result := make([]Span, 0, len(spans))
	for _, span := range spans {
		if span.End <= span.Start {
			continue
		}
		if len(result) == 0 || span.Start > result[len(result)-1].End {
			result = append(result, span)
		} else if span.End > result[len(result)-1].End {
			result[len(result)-1].End = span.End
		}
	}
	return result
}

func boxesForSpan(m TextMap, span Span, work *resolveWorkBudget) ([]Box, error) {
	var boxes []Box
	index, _ := slices.BinarySearchFunc(m.Atoms, span.Start, func(atom Atom, start int64) int {
		if atom.Span.End <= start {
			return -1
		}
		return 0
	})
	for _, atom := range m.Atoms[index:] {
		if atom.Span.Start >= span.End {
			break
		}
		if spansOverlap(atom.Span, span) {
			boxes = append(boxes, atom.Boxes...)
		}
	}
	return compactBoxesBounded(boxes, work)
}

func compactBoxes(boxes []Box) []Box {
	boxes = slices.Clone(boxes)
	heapSort(boxes, compareBox)
	return slices.Compact(boxes)
}

func compactBoxesBounded(boxes []Box, work *resolveWorkBudget) ([]Box, error) {
	if err := work.chargeSort(len(boxes)); err != nil {
		return nil, err
	}
	return compactBoxes(boxes), nil
}

func compactPixelRects(rectangles []pixelRect) []pixelRect {
	rectangles = slices.Clone(rectangles)
	heapSort(rectangles, comparePixel)
	return slices.Compact(rectangles)
}

func compactPixelRectsBounded(rectangles []pixelRect, work *resolveWorkBudget) ([]pixelRect, error) {
	if err := work.chargeSort(len(rectangles)); err != nil {
		return nil, err
	}
	return compactPixelRects(rectangles), nil
}

func pageByNumber(m TextMap, number int) (Page, bool) {
	if number < 1 || number > len(m.Pages) || m.Pages[number-1].Number != number {
		return Page{}, false
	}
	return m.Pages[number-1], true
}

func runsForPage(runs []Run, page int) []Run {
	start := len(runs)
	for start > 0 && runs[start-1].Page == page {
		start--
	}
	return runs[start:]
}

func pixelRectCovered(rectangle pixelRect, union []pixelRect, work *resolveWorkBudget) (bool, error) {
	want := (rectangle.x1 - rectangle.x0) * (rectangle.y1 - rectangle.y0)
	covered := int64(0)
	for _, candidate := range union {
		if err := work.compare(); err != nil {
			return false, err
		}
		if !pixelOverlap(rectangle, candidate) {
			continue
		}
		covered += (min(rectangle.x1, candidate.x1) - max(rectangle.x0, candidate.x0)) *
			(min(rectangle.y1, candidate.y1) - max(rectangle.y0, candidate.y0))
	}
	return covered == want, nil
}

func atomBoxesForSpans(atoms []Atom, spans []Span, work *resolveWorkBudget) ([]Box, error) {
	var err error
	spans, err = unionSpansBounded(spans, work)
	if err != nil {
		return nil, err
	}
	result := make([]Box, 0)
	spanIndex := 0
	for _, atom := range atoms {
		if err := work.compare(); err != nil {
			return nil, err
		}
		for spanIndex < len(spans) && spans[spanIndex].End <= atom.Span.Start {
			if err := work.compare(); err != nil {
				return nil, err
			}
			spanIndex++
		}
		if spanIndex == len(spans) {
			break
		}
		if spansOverlap(atom.Span, spans[spanIndex]) {
			result = append(result, atom.Boxes...)
		}
	}
	return result, nil
}

func boundedDecisionIntersection(left, right resolvedDecision, work *resolveWorkBudget) (bool, error) {
	for _, a := range left.spans {
		for _, b := range right.spans {
			if err := work.compare(); err != nil {
				return false, err
			}
			if spansOverlap(a, b) {
				return true, nil
			}
		}
	}
	return boundedPixelIntersection(left.boxes, right.boxes, work)
}

func boundedPixelIntersection(left, right []pixelRect, work *resolveWorkBudget) (bool, error) {
	for _, a := range left {
		for _, b := range right {
			if err := work.compare(); err != nil {
				return false, err
			}
			if pixelOverlap(a, b) {
				return true, nil
			}
		}
	}
	return false, nil
}

func pixelOverlap(a, b pixelRect) bool {
	return a.page == b.page && a.frame == b.frame && a.x0 < b.x1 && a.x1 > b.x0 && a.y0 < b.y1 && a.y1 > b.y0
}

func spansOverlap(a, b Span) bool { return a.Start < b.End && a.End > b.Start }

func comparePixel(a, b pixelRect) int {
	for _, pair := range [][2]int64{{int64(a.page), int64(b.page)}, {a.y0, b.y0}, {a.x0, b.x0}, {a.y1, b.y1}, {a.x1, b.x1}} {
		if pair[0] != pair[1] {
			return compareInt64(pair[0], pair[1])
		}
	}
	return compareString(a.frame, b.frame)
}

func compareBox(a, b Box) int {
	return comparePixel(pixelRect{a.Page, a.FrameSHA256, a.X0, a.Y0, a.X1, a.Y1}, pixelRect{b.Page, b.FrameSHA256, b.X0, b.Y0, b.X1, b.Y1})
}

func compareString(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareInt64(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func sortedIDs(ids ...string) []string   { slices.Sort(ids); return ids }
func ceilDiv(value, divisor int64) int64 { return value/divisor + btoi(value%divisor != 0) }
func btoi(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
func nonNilBoxes(v []Box) []Box {
	if v == nil {
		return []Box{}
	}
	return v
}
func nonNilSpans(v []Span) []Span {
	if v == nil {
		return []Span{}
	}
	return v
}
func nonNilRuns(v []Run) []Run {
	if v == nil {
		return []Run{}
	}
	return v
}
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
func nonNilRegions(v []RedactionRegion) []RedactionRegion {
	if v == nil {
		return []RedactionRegion{}
	}
	return v
}
