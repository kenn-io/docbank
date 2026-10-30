package grounding

import (
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// citationText prepares only list-marker candidates, before punctuation escapes
// are decoded by the existing scanners. It never changes the citation digest.
func citationText(c *Citation) string {
	raw := []byte(c.Text)
	candidates := sourceMarkerCandidates(raw)
	if len(candidates) == 0 {
		return c.Text
	}
	if c.AtDocumentStart {
		proven := sourceListMarkers(raw)
		for _, pos := range candidates {
			if proven[pos] {
				raw[pos] = ' '
			}
		}
	} else {
		decoded, starts, ends := sourceDecodedOffsets(raw)
		for _, pos := range candidates {
			idx, ok := starts[pos]
			if !ok {
				continue
			}
			end := sourceAmbiguousEnd(decoded[idx:])
			if end > 0 {
				stop := ends[idx+end-1]
				for j := pos; j < stop; j++ {
					raw[j] = ' '
				}
			}
		}
	}
	c.Text = string(raw)
	return c.Text
}

// Bare numeric evidence also feeds amount diagnostics, so suppress it when
// the preceding marker may be either a list marker or a sign.
func sourceAmbiguousEnd(rs []rune) int {
	end, values := amountAt(rs)
	if len(values) > 0 && sourceTokenEnd(rs, end) {
		return end
	}
	start := skipHorizontalSpace(rs, 1)
	end, values = amountAt(rs[start:])
	if len(values) > 0 && sourceTokenEnd(rs[start:], end) {
		return start + end
	}
	end = start
	for end < len(rs) && !split(rs[end]) {
		end++
	}
	if len(bareNumberValues(trim(rs[start:end]))) > 0 {
		return end
	}
	return 0
}

// The source scanner treats delimiters as token boundaries, just as extract does.
func sourceTokenEnd(rs []rune, end int) bool {
	for end < len(rs) && !split(rs[end]) {
		if !edge(rs[end]) {
			return false
		}
		end++
	}
	return true
}

// Candidate markers occur in a line's Markdown container prefix, before text.
func sourceMarkerCandidates(raw []byte) []int {
	var result []int
	for line := 0; line < len(raw); {
		end := line
		for end < len(raw) && raw[end] != '\n' {
			end++
		}
		for i := line; i < end; {
			for i < end && (raw[i] == ' ' || raw[i] == '\t') {
				i++
			}
			if i < end && raw[i] == '>' {
				i++
				continue
			}
			if i+1 < end && (raw[i] == '-' || raw[i] == '+' || raw[i] == '*') && (raw[i+1] == ' ' || raw[i+1] == '\t') {
				if raw[i] != '*' {
					result = append(result, i)
				}
				i++
				continue
			}
			start := i
			for i < end && raw[i] >= '0' && raw[i] <= '9' {
				i++
			}
			if i > start && i+1 < end && (raw[i] == '.' || raw[i] == ')') && (raw[i+1] == ' ' || raw[i+1] == '\t') {
				i++
				continue
			}
			break
		}
		line = end + 1
	}
	return result
}

// Goldmark marks list content lines after their syntactic container prefixes.
func sourceListMarkers(raw []byte) map[int]bool {
	result := map[int]bool{}
	p := parser.NewParser(parser.WithBlockParsers(parser.DefaultBlockParsers()...))
	root := p.Parse(text.NewReader(raw))
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || node.Kind() != ast.KindParagraph && node.Kind() != ast.KindTextBlock && node.Kind() != ast.KindCodeBlock && node.Kind() != ast.KindFencedCodeBlock {
			return ast.WalkContinue, nil
		}
		inList := false
		for parent := node.Parent(); parent != nil; parent = parent.Parent() {
			if parent.Kind() == ast.KindListItem {
				inList = true
				break
			}
		}
		if !inList {
			return ast.WalkContinue, nil
		}
		for j := range node.Lines().Len() {
			start := node.Lines().At(j).Start
			line := start
			for line > 0 && raw[line-1] != '\n' {
				line--
			}
			for _, marker := range sourceMarkerCandidates(raw[line:start]) {
				result[line+marker] = true
			}
		}
		return ast.WalkContinue, nil
	})
	return result
}

// Map each decoded rune to the raw byte span that produced it.
func sourceDecodedOffsets(raw []byte) ([]rune, map[int]int, []int) {
	decoded := []rune{}
	starts := map[int]int{}
	ends := []int{}
	for i := 0; i < len(raw); {
		start := i
		if raw[i] == '\\' && i+1 < len(raw) && util.IsPunct(raw[i+1]) {
			i++
		}
		r, size := utf8.DecodeRune(raw[i:])
		i += size
		starts[start] = len(decoded)
		decoded = append(decoded, r)
		ends = append(ends, i)
	}
	return decoded, starts, ends
}
