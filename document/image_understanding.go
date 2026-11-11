package document

import (
	"bytes"
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

// renditionTextOffset finds a unit's text past the labels written before it,
// so text that repeats a title, notice or model name still resolves to its
// own section. It returns -1 when the text is absent.
func renditionTextOffset(markdown []byte, cursor int, unit NormalizedUnitV1, first bool) int {
	if isImageUnderstandingUnit(unit.HeadingPath) {
		prefix := len(ImageUnderstandingMarkdown(unit.HeadingPath, unit.Locator.Name, "", first))
		if !first {
			prefix += len(renditionUnitSeparator)
		}
		cursor = min(cursor+prefix, len(markdown))
	}
	if relative := bytes.Index(markdown[cursor:], []byte(unit.Text)); relative >= 0 {
		return cursor + relative
	}
	return -1
}

// renditionPresentation prefixes a readable unit with its reader-facing labels.
func renditionPresentation(unit NormalizedEvidenceUnitV1, first bool) string {
	if !isImageUnderstandingUnit(unit.HeadingPath) {
		return ""
	}
	return ImageUnderstandingMarkdown(unit.HeadingPath, unit.Locator.Name, "", first)
}
