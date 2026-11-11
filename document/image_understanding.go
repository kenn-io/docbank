package document

import (
	"strings"
	"unicode/utf8"
)

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

// sanitizeRenditionUnit sanitizes a unit's text. Interpretation text is plain,
// so each line is escaped and sanitized alone and the line breaks Markdown
// would fold into spaces survive.
func sanitizeRenditionUnit(unit NormalizedEvidenceUnitV1, maxLinkChars, maxSourceBytes, maxRunes int) (string, bool, bool, error) {
	if !isImageUnderstandingUnit(unit.HeadingPath) {
		return sanitizeRenditionMarkdown(unit.Text, maxLinkChars, maxSourceBytes, maxRunes)
	}
	var lines []string
	var sourceTruncated, truncated bool
	for _, line := range strings.Split(unit.Text, "\n") {
		text, source, rendition, err := sanitizeRenditionMarkdown(EscapeRenditionText(line), maxLinkChars, maxSourceBytes, maxRunes)
		if err != nil {
			return "", false, false, err
		}
		sourceTruncated, truncated = sourceTruncated || source, truncated || rendition
		lines = append(lines, text)
		if maxRunes -= utf8.RuneCountInString(text) + 1; maxRunes <= 0 {
			break
		}
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n"), sourceTruncated, truncated || maxRunes <= 0, nil
}

// renditionPresentation renders a readable unit's labels for readers: its
// escaped section heading, and before the first unit the notice and the
// model. Evidence keeps the plain text, so none of this is searched.
func renditionPresentation(unit NormalizedEvidenceUnitV1, first bool) string {
	if !isImageUnderstandingUnit(unit.HeadingPath) {
		return ""
	}
	labels := "## " + EscapeRenditionText(unit.HeadingPath[1]) + "\n\n"
	if first {
		labels = "# " + EscapeRenditionText(imageUnderstandingTitle) + "\n\n" + EscapeRenditionText(imageUnderstandingNotice) + "\n\n" + EscapeRenditionText("Configured model: "+unit.Locator.Name) + "\n\n" + labels
	}
	return labels
}
