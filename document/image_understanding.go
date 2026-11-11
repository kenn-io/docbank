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

// sanitizeRenditionUnit sanitizes a unit's text, retaining literal image text.
func sanitizeRenditionUnit(unit NormalizedEvidenceUnitV1, maxLinkChars, maxSourceBytes, maxRunes int) (string, bool, bool, error) {
	if !isImageUnderstandingUnit(unit.HeadingPath) {
		return sanitizeRenditionMarkdown(unit.Text, maxLinkChars, maxSourceBytes, maxRunes)
	}
	source, sourceTruncated := truncateUTF8Bytes(unit.Text, maxSourceBytes)
	text, truncated := truncateRunes(source, maxRunes)
	return text, sourceTruncated, truncated, nil
}

func imageRenditionBlock(text string) (string, int) {
	block := serializeRenditionCodeBlock("", text+"\n")
	return block, strings.IndexByte(block, '\n') + 1
}

func imageRenditionPrefix(text string, budget int) string {
	end, count, run, longest := 0, 0, 0, 0
	for offset, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
		count++
		if count+2*max(3, longest+1)+2 > budget {
			break
		}
		end = offset + len(string(r))
	}
	return text[:end]
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
