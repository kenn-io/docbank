package notemarkdown

import (
	"context"
	"unicode/utf8"
)

// Parse interprets authored bytes without I/O or mutation. Any error returns a
// zero Note. Malformed frontmatter is diagnostic; the body remains readable.
func Parse(ctx context.Context, src []byte, limits Limits) (Note, error) {
	limits, err := normalizeLimits(limits)
	if err != nil {
		return Note{}, err
	}
	if err := ctx.Err(); err != nil {
		return Note{}, err
	}
	if err := limitCount("input_bytes", len(src), limits.MaxBytes); err != nil {
		return Note{}, err
	}
	b := &budget{limits: limits}
	lineStarts := []int{0}
	for i := 0; i < len(src); {
		r, width := utf8.DecodeRune(src[i:])
		if r == 0 || r == utf8.RuneError && width == 1 {
			return Note{}, ErrInvalidInput
		}
		if err := b.step(ctx, width); err != nil {
			return Note{}, err
		}
		if r == '\r' || r == '\n' && (i == 0 || src[i-1] != '\r') {
			next := i + width
			if r == '\r' && next < len(src) && src[next] == '\n' {
				next++
			}
			lineStarts = append(lineStarts, next)
		}
		i += width
	}
	n := Note{Lines: len(lineStarts)}
	n.Frontmatter, n.BodyStart, n.Diagnostics, err = parseFrontmatter(ctx, src, b)
	if err != nil {
		return Note{}, err
	}
	refs := map[string]string{}
	view, blocks, offsets, err := scanBlocks(ctx, src, n.BodyStart, refs, b)
	if err != nil {
		return Note{}, err
	}
	links, err := extractLinks(ctx, view, blocks, refs, b)
	if err != nil {
		return Note{}, err
	}
	n.Headings, err = extractHeadings(ctx, view, src, blocks, lineStarts, b)
	if err != nil {
		return Note{}, err
	}
	for i, link := range links {
		span := originalLinkSpan(link.Span, offsets)
		if err := b.reserveStrings(span.End - span.Start); err != nil {
			return Note{}, err
		}
		n.Links = append(n.Links, Link{Ordinal: i, Form: link.Form, Raw: string(src[span.Start:span.End]),
			Target: link.Target, Fragment: link.Fragment, Label: link.Label, Start: span.Start, End: span.End, Line: sourceLineNumber(lineStarts, span.Start)})
	}
	if n.Frontmatter != nil {
		for _, field := range n.Frontmatter.Fields {
			if field.Key == "title" && field.Value != "" {
				n.Title, n.TitleSource = field.Value, "frontmatter"
				break
			}
		}
	}
	if n.TitleSource == "" {
		for _, h := range n.Headings {
			if h.Level == 1 {
				n.Title, n.TitleSource = h.Text, "h1"
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Note{}, err
	}
	return n, nil
}
