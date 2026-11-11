package document

import "strings"

// ImageUnderstandingProviderID selects an ordinary image interpretation rendition.
const ImageUnderstandingProviderID = "compatible-vision.understand-v1"

// MinImageUnderstandingUnits is the fewest units that fit all four sections.
const MinImageUnderstandingUnits = 4

// ImageUnderstandingHeading is the caveat that leads every interpreted unit's
// heading path, so each search excerpt and embedding input carries it.
const ImageUnderstandingHeading = "Machine-generated image understanding; unverified; sender intent unknown"

const imageUnderstandingTitle = "Machine-generated image understanding"
const imageUnderstandingNotice = "This is a model interpretation of an image, not verified source testimony. Possible meaning is uncertain without conversation context and does not establish anyone's beliefs, endorsement, or intent."

func isImageUnderstandingUnit(headings []string) bool {
	return len(headings) == 2 && headings[0] == ImageUnderstandingHeading
}

// ImageUnderstandingMarkdown renders one interpreted unit for readers: its
// escaped section heading and text, and before the first unit the notice and
// the model. Evidence keeps the plain text, so none of this is searched.
func ImageUnderstandingMarkdown(headings []string, model, text string, first bool) string {
	var out strings.Builder
	if first {
		out.WriteString("# " + EscapeRenditionText(imageUnderstandingTitle) + "\n\n" + EscapeRenditionText(imageUnderstandingNotice) + "\n\n" + EscapeRenditionText("Configured model: "+model) + "\n\n")
	}
	out.WriteString("## " + EscapeRenditionText(headings[1]) + "\n\n" + EscapeRenditionText(text))
	return out.String()
}

// UnescapeRenditionText undoes EscapeRenditionText.
func UnescapeRenditionText(value string) string {
	runes := []rune(value)
	var out strings.Builder
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\\' && i+1 < len(runes) && isMarkdownASCIIPunctuation(runes[i+1]) {
			i++
		}
		out.WriteRune(runes[i])
	}
	return out.String()
}

// renditionSourceText is the Markdown the sanitizer reads for a unit: plain
// interpretation text is escaped so it survives as written.
func renditionSourceText(unit NormalizedEvidenceUnitV1) string {
	if isImageUnderstandingUnit(unit.HeadingPath) {
		return EscapeRenditionText(unit.Text)
	}
	return unit.Text
}

// renditionPresentation prefixes a readable unit with its reader-facing labels.
func renditionPresentation(unit NormalizedEvidenceUnitV1, first bool) string {
	if !isImageUnderstandingUnit(unit.HeadingPath) {
		return ""
	}
	return ImageUnderstandingMarkdown(unit.HeadingPath, unit.Locator.Name, "", first)
}
