package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/util"
	"go.kenn.io/docbank/document"
)

type textSectionIndex struct {
	sections []document.TextOutlineEntry
	chars    int
}

func newTextSectionParser() parser.Parser {
	// Inline parsing adds potentially quadratic work; titles use the original block lines.
	// The block parsers and priorities are those of parser.CommonMark.
	return parser.New(
		parser.WithDefaultParsers(false),
		parser.WithBlockParsers(
			util.Prioritized(parser.NewSetextHeadingParser(), 100),
			util.Prioritized(parser.NewThematicBreakParser(), 200),
			util.Prioritized(parser.NewListParser(), 300),
			util.Prioritized(parser.NewListItemParser(), 400),
			util.Prioritized(parser.NewCodeBlockParser(), 500),
			util.Prioritized(parser.NewATXHeadingParser(), 600),
			util.Prioritized(parser.NewFencedCodeBlockParser(), 700),
			util.Prioritized(parser.NewBlockquoteParser(), 800),
			util.Prioritized(parser.NewHTMLBlockParser(), 900),
			util.Prioritized(parser.NewParagraphParser(), 1000),
		),
		parser.WithParagraphTransformers(util.Prioritized(parser.LinkReferenceParagraphTransformer, 100)),
		parser.WithExtensions(extension.TableParser),
	)
}

func buildTextSectionIndex(
	ctx context.Context, data []byte, markdown parser.Parser,
) (textSectionIndex, error) {
	if err := ctx.Err(); err != nil {
		return textSectionIndex{}, err
	}
	if bytes.Count(data, []byte{'\n'}) > document.MaxTextNavigationLineBreaks {
		return textSectionIndex{}, document.ErrTextNavigationLimit
	}
	root := markdown.Parse(data)
	if err := ctx.Err(); err != nil {
		return textSectionIndex{}, err
	}
	index := textSectionIndex{sections: []document.TextOutlineEntry{{}}}
	// Only advance through the source; never recount each heading's entire prefix.
	byteOffset, chars := 0, 0
	for node := root.FirstChild(); node != nil; node = node.NextSibling() {
		if err := ctx.Err(); err != nil {
			return textSectionIndex{}, err
		}
		heading, ok := node.(*ast.Heading)
		if !ok {
			continue
		}
		if len(index.sections) > document.MaxTextNavigationHeadings {
			return textSectionIndex{}, document.ErrTextNavigationLimit
		}
		pos := heading.Pos()
		for pos > 0 && data[pos-1] != '\n' {
			pos--
		}
		added, err := countSectionChars(ctx, data[byteOffset:pos])
		if err != nil {
			return textSectionIndex{}, err
		}
		chars += added
		byteOffset = pos
		title, truncated := sectionTitle(heading, data)
		index.sections = append(index.sections, document.TextOutlineEntry{
			Start: chars, Level: heading.Level, TitleMarkdown: title, TitleTruncated: truncated})
	}
	remaining, err := countSectionChars(ctx, data[byteOffset:])
	if err != nil {
		return textSectionIndex{}, err
	}
	index.chars = chars + remaining
	index.sections[0].End = index.chars
	// The stack contains heading indexes whose next equal-or-lesser heading is unknown.
	var stack []int
	for i := 1; i < len(index.sections); i++ {
		for len(stack) > 0 && index.sections[stack[len(stack)-1]].Level >= index.sections[i].Level {
			index.sections[stack[len(stack)-1]].End = index.sections[i].Start
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, i)
	}
	for _, i := range stack {
		index.sections[i].End = index.chars
	}
	return index, nil
}

func countSectionChars(ctx context.Context, data []byte) (int, error) {
	chars, checked := 0, 0
	for offset := range string(data) {
		if offset >= checked {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			checked = offset + 4096
		}
		chars++
	}
	return chars, ctx.Err()
}

func sectionTitle(heading *ast.Heading, data []byte) (string, bool) {
	var title strings.Builder
	for i, line := range heading.Source() {
		if i > 0 {
			title.WriteByte(' ')
		}
		title.Write(bytes.TrimSpace(line.Bytes(data)))
	}
	value := strings.TrimSpace(title.String())
	chars := 0
	for offset := range value {
		if chars == document.MaxTextOutlineTitleChars {
			return strings.Clone(value[:offset]), true
		}
		chars++
	}
	return strings.Clone(value), false
}

func (index textSectionIndex) outline(
	request document.TextOutlineRequest,
) (document.TextOutlinePage, error) {
	if request.Citation.End > index.chars {
		return document.TextOutlinePage{}, document.ErrInvalidCitationRange
	}
	total := len(index.sections)
	if request.Offset < 0 || request.Offset > total {
		return document.TextOutlinePage{}, document.ErrInvalidTextNavigation
	}
	enclosing := document.TextSectionRange{Start: 0, End: index.chars}
	for _, section := range index.sections[1:] {
		if section.Start <= request.Citation.Start && section.End >= request.Citation.End {
			enclosing = document.TextSectionRange{Start: section.Start, End: section.End}
		}
	}
	end := min(request.Offset+document.TextOutlinePageSize, total)
	result := document.TextOutlinePage{Citation: request.Citation, DocumentChars: index.chars,
		EnclosingSection: enclosing, Offset: request.Offset, Total: total,
		Sections: append([]document.TextOutlineEntry{}, index.sections[request.Offset:end]...)}
	if end < total {
		result.NextOffset = new(end)
	}
	return result, nil
}

func (index textSectionIndex) page(
	ctx context.Context, data []byte, request document.TextSectionRequest,
) (document.TextSectionPage, error) {
	if request.Citation.End > index.chars {
		return document.TextSectionPage{}, document.ErrInvalidCitationRange
	}
	section := request.Section
	found := slices.ContainsFunc(index.sections, func(entry document.TextOutlineEntry) bool {
		return entry.Start == section.Start && entry.End == section.End
	})
	if !found || request.Offset < 0 || request.Offset > section.End-section.Start ||
		request.MaxChars < 1 || request.MaxChars > document.MaxTextSectionChars {
		return document.TextSectionPage{}, document.ErrInvalidTextNavigation
	}
	start := section.Start + request.Offset
	end := start + min(request.MaxChars, section.End-start)
	quote, err := sectionText(ctx, data, start, end)
	if err != nil {
		return document.TextSectionPage{}, err
	}
	digest := sha256.Sum256([]byte(quote))
	return document.TextSectionPage{Citation: request.Citation,
		Section: section, Offset: request.Offset,
		ActualStart: start, ActualEnd: end, NextOffset: end - section.Start, Complete: end == section.End,
		Text: quote, TextBytes: len(quote), TextSHA256: hex.EncodeToString(digest[:])}, nil
}

func sectionText(ctx context.Context, data []byte, start, end int) (string, error) {
	chars, offset, begin := 0, 0, 0
	for chars < end {
		if chars%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		if chars == start {
			begin = offset
		}
		_, width := utf8.DecodeRune(data[offset:])
		offset += width
		chars++
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if start == end {
		return "", nil
	}
	return string(data[begin:offset]), nil
}
