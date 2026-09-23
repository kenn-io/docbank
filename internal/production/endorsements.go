package production

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	endorsementFontSizeMilliPoints = int64(8_000)
	endorsementStripHeight         = int64(5_000)
	endorsementMaxPageTextBytes    = 1 << 20
)

// EndorsedPage is one deterministic page layout and its public text. SHA256
// binds the layout and ordered endorsements without admitting private reasons.
type EndorsedPage struct {
	Layout       redaction.PageLayout    `json:"layout"`
	Endorsements []redaction.Endorsement `json:"endorsements"`
	SHA256       string                  `json:"sha256"`
}

type plannedEndorsement struct {
	kind      string
	text      string
	insideBox *redaction.Box
}

type endorsementMetrics struct {
	face font.Face
	dpi  int64
}

type endorsementPixelRect struct {
	x0, y0, x1, y1 int64
}

// PlanEndorsementPages lays out one immutable production occurrence. Pages
// remain in sealed order. Public region labels remain in canonical region
// order on each page, and the reserved page number follows those labels.
func PlanEndorsementPages(memberID string, pages []redaction.Page, resolved redaction.Resolved, numbers []documentproduction.AssignedNumber, recipe redaction.Recipe) ([]EndorsedPage, error) {
	qualified, err := pdfproduction.QualifiedRecipeForDPI(recipe.DPI)
	if err != nil || recipe != qualified || !slices.Equal(pages, resolved.Pages) {
		return nil, endorsementPlanningConflict()
	}
	if _, err := redaction.Text(resolved); err != nil {
		return nil, endorsementPlanningConflict()
	}
	recipeBytes, err := canonical.Marshal(recipe)
	if err != nil || sha256HexBytes(recipeBytes) != resolved.RecipeSHA256 {
		return nil, endorsementPlanningConflict()
	}
	metrics, closeMetrics, err := newEndorsementMetrics(recipe)
	if err != nil {
		return nil, endorsementPlanningConflict()
	}
	defer closeMetrics()

	pageNumbers, err := assignedNumbersForMember(memberID, pages, numbers, metrics)
	if err != nil {
		return nil, err
	}
	plannedByPage, stripByPage, err := planTextByPage(pages, resolved.Regions, resolved.RedactBoxes, resolved.Runs, pageNumbers, metrics)
	if err != nil {
		return nil, err
	}
	result := make([]EndorsedPage, 0, len(pages))
	for _, page := range pages {
		layout, err := endorsementPageLayout(page, stripByPage[page.Number], recipe)
		if err != nil {
			return nil, err
		}
		endorsements, err := placePageEndorsements(layout, plannedByPage[page.Number], metrics, recipe.FontSHA256)
		if err != nil {
			return nil, err
		}
		value := EndorsedPage{Layout: layout, Endorsements: endorsements}
		value.SHA256, err = endorsedPageSHA256(value)
		if err != nil {
			return nil, endorsementPlanningConflict()
		}
		result = append(result, value)
	}
	return result, nil
}

func newEndorsementMetrics(recipe redaction.Recipe) (endorsementMetrics, func(), error) {
	fontBytes := pdfproduction.BundledUnicodeFont()
	if sha256HexBytes(fontBytes) != recipe.FontSHA256 {
		return endorsementMetrics{}, func() {}, endorsementPlanningConflict()
	}
	parsed, err := opentype.Parse(fontBytes)
	if err != nil {
		return endorsementMetrics{}, func() {}, fmt.Errorf("parse qualified endorsement font: %w", err)
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
		Size: float64(endorsementFontSizeMilliPoints) / 1_000, DPI: float64(recipe.DPI), Hinting: font.HintingNone,
	})
	if err != nil {
		return endorsementMetrics{}, func() {}, fmt.Errorf("open qualified endorsement font: %w", err)
	}
	return endorsementMetrics{face: face, dpi: int64(recipe.DPI)}, func() { _ = face.Close() }, nil
}

func assignedNumbersForMember(memberID string, pages []redaction.Page, numbers []documentproduction.AssignedNumber, metrics endorsementMetrics) (map[int]string, error) {
	pageSet := make(map[int]struct{}, len(pages))
	for _, page := range pages {
		pageSet[page.Number] = struct{}{}
	}
	result := make(map[int]string, len(pages))
	for _, number := range numbers {
		if number.MemberID != memberID {
			continue
		}
		if number.MemberOrdinal < 1 {
			return nil, endorsementPlanningConflict()
		}
		if _, exists := pageSet[number.Page]; !exists {
			return nil, endorsementPlanningConflict()
		}
		if _, duplicate := result[number.Page]; duplicate {
			return nil, endorsementPlanningConflict()
		}
		if _, _, err := metrics.measure(number.Text); err != nil {
			return nil, endorsementPlanningConflict()
		}
		result[number.Page] = number.Text
	}
	if (len(numbers) != 0 && len(result) == 0) || (len(result) != 0 && len(result) != len(pages)) {
		return nil, endorsementPlanningConflict()
	}
	return result, nil
}

func planTextByPage(pages []redaction.Page, regions []redaction.RedactionRegion, masks []redaction.Box, runs []redaction.Run, numbers map[int]string, metrics endorsementMetrics) (map[int][]plannedEndorsement, map[int]bool, error) {
	result := make(map[int][]plannedEndorsement, len(pages))
	needsStrip := make(map[int]bool, len(pages))
	pageSet := make(map[int]struct{}, len(pages))
	masksByPage := make(map[int][]redaction.Box, len(pages))
	occupiedByPage := make(map[int][]endorsementPixelRect, len(pages))
	textBytes := make(map[int]int, len(pages))
	for _, page := range pages {
		pageSet[page.Number] = struct{}{}
		result[page.Number] = []plannedEndorsement{}
	}
	for _, mask := range masks {
		masksByPage[mask.Page] = append(masksByPage[mask.Page], mask)
	}
	for _, run := range runs {
		if _, exists := pageSet[run.Page]; !exists || len(run.Text) > endorsementMaxPageTextBytes-textBytes[run.Page] {
			return nil, nil, endorsementPlanningConflict()
		}
		textBytes[run.Page] += len(run.Text)
	}
	for _, region := range regions {
		if _, _, err := metrics.measure(region.Label); err != nil {
			return nil, nil, endorsementPlanningConflict()
		}
		for start := 0; start < len(region.Boxes); {
			page := region.Boxes[start].Page
			if _, exists := pageSet[page]; !exists {
				return nil, nil, endorsementPlanningConflict()
			}
			end := start + 1
			for end < len(region.Boxes) && region.Boxes[end].Page == page {
				end++
			}
			var inside *redaction.Box
			for index := start; index < end; index++ {
				candidate := region.Boxes[index]
				pixelRect, ok := inwardPixelRectangle(candidate, metrics.dpi)
				if !metrics.fits(candidate, region.Label) || !boxContainedByMask(candidate, masksByPage[page]) || !ok || pixelRect.overlapsAny(occupiedByPage[page]) {
					continue
				}
				inside = &candidate
				occupiedByPage[page] = append(occupiedByPage[page], pixelRect)
				break
			}
			if inside == nil {
				if !spendPageText(textBytes, page, region.Label) {
					return nil, nil, endorsementPlanningConflict()
				}
				needsStrip[page] = true
				result[page] = append(result[page], plannedEndorsement{kind: "legend", text: region.Label})
			} else {
				if !spendPageText(textBytes, page, region.Label) {
					return nil, nil, endorsementPlanningConflict()
				}
				result[page] = append(result[page], plannedEndorsement{kind: "label", text: region.Label, insideBox: inside})
			}
			start = end
		}
	}
	for _, page := range pages {
		if number, ok := numbers[page.Number]; ok {
			if !spendPageText(textBytes, page.Number, number) {
				return nil, nil, endorsementPlanningConflict()
			}
			needsStrip[page.Number] = true
			result[page.Number] = append(result[page.Number], plannedEndorsement{kind: "number", text: number})
		}
		if len(result[page.Number]) > 4096 { // Matches the qualified writer's per-page evidence bound.
			return nil, nil, endorsementPlanningConflict()
		}
	}
	return result, needsStrip, nil
}

func spendPageText(used map[int]int, page int, text string) bool {
	if len(text) > endorsementMaxPageTextBytes-used[page] {
		return false
	}
	used[page] += len(text)
	return true
}

func boxContainedByMask(box redaction.Box, masks []redaction.Box) bool {
	for _, mask := range masks {
		if box.Page == mask.Page && box.FrameSHA256 == mask.FrameSHA256 && box.X0 >= mask.X0 && box.Y0 >= mask.Y0 && box.X1 <= mask.X1 && box.Y1 <= mask.Y1 {
			return true
		}
	}
	return false
}

func (rectangle endorsementPixelRect) overlapsAny(others []endorsementPixelRect) bool {
	for _, other := range others {
		if rectangle.x0 < other.x1 && other.x0 < rectangle.x1 && rectangle.y0 < other.y1 && other.y0 < rectangle.y1 {
			return true
		}
	}
	return false
}

func endorsementPageLayout(source redaction.Page, withStrip bool, recipe redaction.Recipe) (redaction.PageLayout, error) {
	if !pageWithinRecipe(source, recipe) {
		return redaction.PageLayout{}, endorsementPlanningConflict()
	}
	if !withStrip {
		return redaction.PageLayout{Source: source, Output: source}, nil
	}
	if source.Height > math.MaxInt64-endorsementStripHeight {
		return redaction.PageLayout{}, endorsementPlanningConflict()
	}
	output := source
	output.Height += endorsementStripHeight
	identity := struct {
		Contract    string         `json:"contract"`
		Source      redaction.Page `json:"source"`
		StripHeight int64          `json:"strip_height"`
	}{"production-page-layout/v1", source, endorsementStripHeight}
	encoded, err := canonical.Marshal(identity)
	if err != nil {
		return redaction.PageLayout{}, endorsementPlanningConflict()
	}
	output.FrameSHA256 = sha256HexBytes(encoded)
	if !pageWithinRecipe(output, recipe) {
		return redaction.PageLayout{}, endorsementPlanningConflict()
	}
	return redaction.PageLayout{Source: source, Output: output, StripHeight: endorsementStripHeight}, nil
}

func placePageEndorsements(layout redaction.PageLayout, planned []plannedEndorsement, metrics endorsementMetrics, fontSHA256 string) ([]redaction.Endorsement, error) {
	result := make([]redaction.Endorsement, 0, len(planned))
	cursorX, cursorY, rowBottom := int64(0), layout.Source.Height, layout.Source.Height
	for _, value := range planned {
		var box redaction.Box
		if value.insideBox != nil {
			box = *value.insideBox
			box.FrameSHA256 = layout.Output.FrameSHA256
		} else {
			var err error
			box, err = metrics.placeStripText(value.text, layout.Output, cursorX, cursorY)
			if err != nil && cursorX != 0 {
				cursorX, cursorY = 0, rowBottom
				box, err = metrics.placeStripText(value.text, layout.Output, cursorX, cursorY)
			}
			if err != nil {
				return nil, endorsementPlanningConflict()
			}
			cursorX = box.X1
			rowBottom = max(rowBottom, box.Y1)
		}
		color := "#000000"
		if value.kind == "label" {
			color = "#ffffff"
		}
		result = append(result, redaction.Endorsement{
			Kind: value.kind, Text: value.text, FontSHA256: fontSHA256, Color: color,
			Box: box, FontSizeMilliPoints: endorsementFontSizeMilliPoints,
		})
	}
	return result, nil
}

func (metrics endorsementMetrics) measure(text string) (int64, int64, error) {
	if text == "" || len(text) > redaction.MaxDecisionLabelBytes || !utf8.ValidString(text) || strings.TrimSpace(text) == "" || strings.ContainsAny(text, "\r\n\t\f") {
		return 0, 0, endorsementPlanningConflict()
	}
	var advance int64
	var previous rune
	first := true
	for _, character := range text {
		_, glyphAdvance, ok := metrics.face.GlyphBounds(character)
		if !ok {
			return 0, 0, endorsementPlanningConflict()
		}
		if !first {
			advance += int64(metrics.face.Kern(previous, character))
		}
		advance += int64(glyphAdvance)
		if advance < 0 || advance > math.MaxInt32 {
			return 0, 0, endorsementPlanningConflict()
		}
		previous, first = character, false
	}
	bounds, stringAdvance := font.BoundString(metrics.face, text)
	minX := min(bounds.Min.X, fixed.Int26_6(0))
	maxX := max(bounds.Max.X, stringAdvance)
	width := int64((maxX - minX).Ceil())
	height := int64((bounds.Max.Y - bounds.Min.Y).Ceil())
	if width < 1 || height < 1 {
		return 0, 0, endorsementPlanningConflict()
	}
	return width, height, nil
}

func (metrics endorsementMetrics) fits(box redaction.Box, text string) bool {
	width, height, err := metrics.measure(text)
	if err != nil {
		return false
	}
	boxWidth, boxHeight, ok := inwardPixelSize(box, metrics.dpi)
	return ok && box.Y1-box.Y0 >= minimumEndorsementHeight() && width <= boxWidth && height <= boxHeight
}

func (metrics endorsementMetrics) placeStripText(text string, output redaction.Page, x0, y0 int64) (redaction.Box, error) {
	width, height, err := metrics.measure(text)
	if err != nil {
		return redaction.Box{}, err
	}
	x1, ok := physicalEndForPixels(x0, width, metrics.dpi)
	if !ok {
		return redaction.Box{}, endorsementPlanningConflict()
	}
	y1, ok := physicalEndForPixels(y0, height, metrics.dpi)
	if !ok || y0 > math.MaxInt64-minimumEndorsementHeight() || x1 > output.Width {
		return redaction.Box{}, endorsementPlanningConflict()
	}
	y1 = max(y1, y0+minimumEndorsementHeight())
	if y1 > output.Height {
		return redaction.Box{}, endorsementPlanningConflict()
	}
	box := redaction.Box{Page: output.Number, FrameSHA256: output.FrameSHA256, X0: x0, Y0: y0, X1: x1, Y1: y1}
	if !metrics.fits(box, text) {
		return redaction.Box{}, endorsementPlanningConflict()
	}
	return box, nil
}

func minimumEndorsementHeight() int64 {
	return (endorsementFontSizeMilliPoints*10 + 71) / 72
}

func physicalEndForPixels(start, pixels, dpi int64) (int64, bool) {
	if start < 0 || pixels < 1 || dpi < 1 || start > (math.MaxInt64-9_999)/dpi {
		return 0, false
	}
	startPixel := (start*dpi + 9_999) / 10_000
	if startPixel > math.MaxInt64-pixels || startPixel+pixels > (math.MaxInt64-(dpi-1))/10_000 {
		return 0, false
	}
	end := ((startPixel+pixels)*10_000 + dpi - 1) / dpi
	return end, end > start
}

func inwardPixelSize(box redaction.Box, dpi int64) (int64, int64, bool) {
	rectangle, ok := inwardPixelRectangle(box, dpi)
	return rectangle.x1 - rectangle.x0, rectangle.y1 - rectangle.y0, ok
}

func inwardPixelRectangle(box redaction.Box, dpi int64) (endorsementPixelRect, bool) {
	if dpi < 1 || box.X0 < 0 || box.Y0 < 0 || box.X1 <= box.X0 || box.Y1 <= box.Y0 ||
		box.X1 > math.MaxInt64/dpi || box.Y1 > math.MaxInt64/dpi || box.X0 > (math.MaxInt64-9_999)/dpi || box.Y0 > (math.MaxInt64-9_999)/dpi {
		return endorsementPixelRect{}, false
	}
	x0 := (box.X0*dpi + 9_999) / 10_000
	y0 := (box.Y0*dpi + 9_999) / 10_000
	x1 := box.X1 * dpi / 10_000
	y1 := box.Y1 * dpi / 10_000
	return endorsementPixelRect{x0: x0, y0: y0, x1: x1, y1: y1}, x1 > x0 && y1 > y0
}

func pageWithinRecipe(page redaction.Page, recipe redaction.Recipe) bool {
	if page.Number < 1 || page.Width <= 0 || page.Height <= 0 || page.Width > (math.MaxInt64-9_999)/int64(recipe.DPI) || page.Height > (math.MaxInt64-9_999)/int64(recipe.DPI) {
		return false
	}
	width := (page.Width*int64(recipe.DPI) + 9_999) / 10_000
	height := (page.Height*int64(recipe.DPI) + 9_999) / 10_000
	return width > 0 && height > 0 && width <= recipe.MaxAxis && height <= recipe.MaxAxis && width <= recipe.MaxPixels/height
}

func endorsedPageSHA256(value EndorsedPage) (string, error) {
	identity := struct {
		Layout       redaction.PageLayout    `json:"layout"`
		Endorsements []redaction.Endorsement `json:"endorsements"`
	}{value.Layout, slices.Clone(value.Endorsements)}
	encoded, err := canonical.Marshal(identity)
	if err != nil {
		return "", err
	}
	return sha256HexBytes(encoded), nil
}

func endorsementPlanningConflict() error {
	return &redaction.Problem{Code: "endorsement_layout_conflict"}
}

func sha256HexBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
