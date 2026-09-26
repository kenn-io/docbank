package processing

import (
	"bytes"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/store"
)

type contextByteSpan struct{ Start, End int }

// reconstructSegmentRanges derives only byte coordinates. Callers must still
// open and verify the retained artifact before returning any reconstructed
// text; the catalog's segment records alone are not content authority.
func reconstructSegmentRanges(build store.RenditionBuildRecord) ([]byte, map[string]contextByteSpan, error) {
	byUnit := make(map[string][]store.RenditionLexicalSegmentRecord, len(build.Units))
	for _, segment := range build.LexicalSegments {
		byUnit[segment.UnitID] = append(byUnit[segment.UnitID], segment)
	}
	var body bytes.Buffer
	spans := make(map[string]contextByteSpan, len(build.LexicalSegments))
	unitsWritten := 0
	segmentsWritten := 0
	for _, unit := range build.Units {
		segments := byUnit[unit.ID]
		if len(segments) == 0 {
			continue
		}
		if unitsWritten != 0 {
			_, _ = body.WriteString("\n\n---\n\n")
		}
		unitsWritten++
		runeCursor := 0
		for _, segment := range segments {
			if segment.ID == "" || segment.CharStart != runeCursor ||
				segment.CharEnd <= segment.CharStart || !utf8.ValidString(segment.Text) ||
				segment.CharEnd-segment.CharStart != utf8.RuneCountInString(segment.Text) {
				return nil, nil, ErrPassageCorrupt
			}
			if _, duplicate := spans[segment.ID]; duplicate {
				return nil, nil, ErrPassageCorrupt
			}
			start := body.Len()
			_, _ = body.WriteString(segment.Text)
			spans[segment.ID] = contextByteSpan{Start: start, End: body.Len()}
			runeCursor = segment.CharEnd
			segmentsWritten++
		}
	}
	if unitsWritten == 0 || segmentsWritten != len(build.LexicalSegments) {
		return nil, nil, ErrPassageCorrupt
	}
	_ = body.WriteByte('\n')
	return bytes.Clone(body.Bytes()), spans, nil
}
