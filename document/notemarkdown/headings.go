package notemarkdown

import (
	"context"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func extractHeadings(ctx context.Context, original []byte, blocks []sourceBlock, links []Link, b *budget) ([]Heading, error) {
	var result []Heading
	linkAt := map[int]Link{}
	for _, link := range links {
		if err := b.step(ctx, 1); err != nil {
			return nil, err
		}
		linkAt[link.Span.Start] = link
	}
	used := stringSet{}
	nextSuffix := stringCounters{}
	for _, block := range blocks {
		if block.level == 0 {
			continue
		}
		if err := limitCount("headings", len(result)+1, maxHeadings); err != nil {
			return nil, err
		}
		raw := original[block.content.Start:block.content.End]
		if err := b.reserveStrings(len(raw)); err != nil {
			return nil, err
		}
		markdown := string(raw)
		text, err := headingText(ctx, original, block.content, linkAt, b)
		if err != nil {
			return nil, err
		}
		base, err := headingKey(ctx, text, b)
		if err != nil {
			return nil, err
		}
		if base == "" {
			base = "section"
		}
		anchor := base
		counter, err := nextSuffix.counter(ctx, base, b)
		if err != nil {
			return nil, err
		}
		suffix := max(1, *counter)
		for {
			duplicate, err := used.add(ctx, anchor, b)
			if err != nil {
				return nil, err
			}
			if !duplicate {
				break
			}
			if err := b.step(ctx, 1); err != nil {
				return nil, err
			}
			// Reserve each attempted candidate before materializing it.
			if err := b.reserveStrings(len(base) + 1 + len(strconv.Itoa(suffix))); err != nil {
				return nil, err
			}
			anchor = base + "-" + strconv.Itoa(suffix)
			suffix++
		}
		*counter = suffix
		result = append(result, Heading{Level: block.level, Span: block.span, Section: Span{block.span.Start, len(original)}, Markdown: markdown, Text: text, Anchor: anchor})
	}
	var stack []int
	for i := range result {
		for len(stack) > 0 && result[stack[len(stack)-1]].Level >= result[i].Level {
			j := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			result[j].Section.End = result[i].Span.Start
		}
		stack = append(stack, i)
	}
	return result, nil
}

func headingKey(ctx context.Context, text string, b *budget) (string, error) {
	// NFC can expand a code point; reserve a conservative UTF-8 bound first.
	if err := b.reserveStrings(3 * len(text)); err != nil {
		return "", err
	}
	var normalized norm.Iter
	normalized.InitString(norm.NFC, text)
	var out strings.Builder
	for !normalized.Done() {
		segment := normalized.Next()
		for len(segment) > 0 {
			r, width := utf8.DecodeRune(segment)
			segment = segment[width:]
			if err := b.step(ctx, 1); err != nil {
				return "", err
			}
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
				out.WriteRune(unicode.ToLower(r))
			} else if unicode.IsSpace(r) {
				out.WriteByte('-')
			}
		}
	}
	return out.String(), nil
}

func asciiPunctuation(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}
