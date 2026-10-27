package grounding

import (
	"bufio"
	"bytes"
	stdhtml "html"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type markdownSource struct {
	original, structural, controls []rune
	byteRunes                      []int
	codeSpans                      []text.Segment
	references                     map[int]reference
	ordinals                       []text.Segment
	omitted, literal               []bool
	delimiters                     map[*parser.Delimiter]*[2]int
}

func analyzeMarkdown(s string) markdownSource {
	m := markdownSource{original: []rune(s), structural: []rune(s), controls: []rune(s), byteRunes: make([]int, len(s)+1), omitted: make([]bool, len([]rune(s))), literal: make([]bool, len([]rune(s))), delimiters: make(map[*parser.Delimiter]*[2]int), references: make(map[int]reference)}
	i := 0
	for offset := range s {
		m.byteRunes[offset] = i
		i++
	}
	m.byteRunes[len(s)] = i
	p := markdownParser(&m)
	data := []byte(s)
	// Goldmark reads LF lines; preserve byte positions while admitting standalone CR.
	for i, b := range data {
		if b == '\r' && (i+1 == len(data) || data[i+1] != '\n') {
			data[i] = '\n'
		}
	}
	root := p.Parse(text.NewReader(data))
	maskMarkdown(&m, data, root)
	return m
}

func markdownParser(m *markdownSource) parser.Parser {
	inline := parser.DefaultInlineParsers()
	for i := range inline {
		inline[i].Value = wrapInlineParser(inline[i].Value, m)
	}
	blocks := parser.DefaultBlockParsers()
	for i := range blocks {
		if blocks[i].Value == parser.NewListItemParser() {
			blocks[i].Value = &sourceListParser{BlockParser: parser.NewListItemParser(), source: m}
		}
	}
	return parser.NewParser(parser.WithBlockParsers(blocks...), parser.WithInlineParsers(inline...), parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...))
}

func wrapInlineParser(p any, m *markdownSource) any {
	switch p {
	case parser.NewEmphasisParser():
		return &sourceEmphasisParser{InlineParser: parser.NewEmphasisParser(), source: m}
	case parser.NewLinkParser():
		return &sourceLinkParser{InlineParser: parser.NewLinkParser(), source: m}
	case parser.NewCodeSpanParser():
		return &sourceCodeSpanParser{InlineParser: parser.NewCodeSpanParser(), source: m}
	default:
		return p
	}
}

type sourceCodeSpanParser struct {
	parser.InlineParser

	source *markdownSource
}

func (p *sourceCodeSpanParser) Parse(parent ast.Node, reader text.Reader, pc parser.Context) ast.Node {
	_, start := reader.Position()
	n := p.InlineParser.Parse(parent, reader, pc)
	if n != nil && n.Kind() == ast.KindCodeSpan {
		_, end := reader.Position()
		first, firstOK := n.FirstChild().(*ast.Text)
		last, lastOK := n.LastChild().(*ast.Text)
		if firstOK && lastOK {
			p.source.omit(start.Start, first.Segment.Start)
			p.source.omit(last.Segment.Stop, end.Start)
		}
	}
	return n
}

type sourceListParser struct {
	parser.BlockParser

	source *markdownSource
}

func (p *sourceListParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, before := reader.Position()
	n, state := p.BlockParser.Open(parent, reader, pc)
	if n != nil {
		_, after := reader.Position()
		end := after.Start
		if end == before.Start {
			end = before.Stop
		}
		if list, ok := parent.(*ast.List); ok && list.IsOrdered() {
			data := reader.Source()
			start := before.Start
			for start < end && (data[start] == ' ' || data[start] == '\t') {
				start++
			}
			stop := start
			for stop < end && data[stop] >= '0' && data[stop] <= '9' {
				stop++
			}
			if stop > start {
				p.source.ordinals = append(p.source.ordinals, text.NewSegment(start, stop))
			}
		}
		p.source.hide(before.Start, end)
	}
	return n, state
}

func maskMarkdown(m *markdownSource, data []byte, root ast.Node) {
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			maskMarkdownNode(m, data, n)
		}
		return ast.WalkContinue, nil
	})
}

func maskMarkdownNode(m *markdownSource, data []byte, node ast.Node) {
	switch n := node.(type) {
	case *ast.Text:
		if !n.IsRaw() && n.Parent().Kind() != ast.KindCodeSpan {
			m.collectReferences(data, n.Segment)
		}
	case *ast.CodeSpan:
		m.maskCodeSpan(n)
	case *ast.CodeBlock, *ast.FencedCodeBlock:
		if fence, ok := n.(*ast.FencedCodeBlock); ok && fence.Info != nil {
			m.hide(fence.Info.Segment.Start, fence.Info.Segment.Stop)
		}
		for i := range n.Lines().Len() {
			line := n.Lines().At(i)
			m.maskLiteral(line)
		}
	case *ast.LinkReferenceDefinition:
		for i := range n.Lines().Len() {
			line := n.Lines().At(i)
			m.hide(line.Start, line.Stop)
		}
	case *ast.RawHTML:
		for i := range n.Segments.Len() {
			line := n.Segments.At(i)
			m.mask(m.controls, line.Start, line.Stop)
			m.omit(line.Start, line.Stop)
		}
	case *ast.HTMLBlock:
		for i := range n.Lines().Len() {
			line := n.Lines().At(i)
			m.hide(line.Start, line.Stop)
		}
		if n.HasClosure() {
			m.hide(n.ClosureLine.Start, n.ClosureLine.Stop)
		}
	}
}

type reference struct {
	end     int
	visible []rune
}

func (m *markdownSource) collectReferences(data []byte, span text.Segment) {
	for i := span.Start; i < span.Stop; i++ {
		if data[i] != '&' || markdownEscaped(m.original, m.byteRunes[i]) {
			continue
		}
		j := i + 1
		for j < span.Stop && (util.IsAlphaNumeric(data[j]) || data[j] == '#') {
			j++
		}
		if j >= span.Stop || data[j] != ';' {
			i = j - 1
			continue
		}
		candidate := data[i : j+1]
		var output bytes.Buffer
		writer := bufio.NewWriter(&output)
		gmhtml.DefaultWriter.Write(writer, candidate)
		_ = writer.Flush()
		visible := stdhtml.UnescapeString(output.String())
		if visible != string(candidate) {
			start, end := m.byteRunes[i], m.byteRunes[j+1]
			m.references[start] = reference{end: end, visible: []rune(visible)}
		}
		i = j
	}
}

func (m *markdownSource) maskLiteral(span text.Segment) {
	m.mask(m.controls, span.Start, span.Stop)
	for i := m.byteRunes[span.Start]; i < m.byteRunes[span.Stop]; i++ {
		m.literal[i] = true
	}
}

func (m *markdownSource) maskCodeSpan(n *ast.CodeSpan) {
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		if content, ok := child.(*ast.Text); ok {
			m.maskLiteral(content.Segment)
			m.codeSpans = append(m.codeSpans, content.Segment)
		}
	}
}

func (m *markdownSource) claimText() string {
	rs := append([]rune(nil), m.structural...)
	for _, span := range m.codeSpans {
		for i := m.byteRunes[span.Start]; i < m.byteRunes[span.Stop]; i++ {
			if rs[i] == '.' || rs[i] == '!' || rs[i] == '?' {
				rs[i] = ' '
			}
		}
	}
	return string(rs)
}

func (m *markdownSource) mask(rs []rune, start, end int) {
	if start < 0 || end < start {
		return
	}
	for i := m.byteRunes[start]; i < m.byteRunes[end]; i++ {
		if rs[i] != '\n' && rs[i] != '\r' {
			rs[i] = ' '
		}
	}
}

func (m *markdownSource) omit(start, end int) {
	for i := m.byteRunes[start]; i < m.byteRunes[end]; i++ {
		m.omitted[i] = true
	}
}

func (m *markdownSource) hide(start, end int) {
	m.mask(m.structural, start, end)
	m.mask(m.controls, start, end)
}

type sourceEmphasisParser struct {
	parser.InlineParser

	source *markdownSource
}

func (p *sourceEmphasisParser) Parse(parent ast.Node, reader text.Reader, pc parser.Context) ast.Node {
	n := p.InlineParser.Parse(parent, reader, pc)
	if d, ok := n.(*parser.Delimiter); ok {
		p.source.delimiters[d] = &[2]int{d.Segment.Start, d.Segment.Stop}
		d.Processor = &sourceDelimiterProcessor{DelimiterProcessor: d.Processor, source: p.source}
	}
	return n
}

type sourceDelimiterProcessor struct {
	parser.DelimiterProcessor

	source         *markdownSource
	opener, closer *parser.Delimiter
}

func (p *sourceDelimiterProcessor) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	if !p.DelimiterProcessor.CanOpenCloser(opener, closer) {
		return false
	}
	p.opener, p.closer = opener, closer
	return true
}
func (p *sourceDelimiterProcessor) OnMatch(n int) ast.Node {
	left, right := p.source.delimiters[p.opener], p.source.delimiters[p.closer]
	for i := left[1] - n; i < left[1]; i++ {
		p.source.omitted[p.source.byteRunes[i]] = true
	}
	for i := right[0]; i < right[0]+n; i++ {
		p.source.omitted[p.source.byteRunes[i]] = true
	}
	left[1] -= n
	right[0] += n
	return p.DelimiterProcessor.OnMatch(n)
}

type projectedMarkdown struct {
	runes                   []rune
	positions, ends, prefix []int
}

func (m *markdownSource) project(rs []rune) projectedMarkdown {
	p := projectedMarkdown{prefix: make([]int, len(rs)+1)}
	for i := 0; i < len(rs); {
		p.prefix[i] = len(p.runes)
		if ref, ok := m.references[i]; ok {
			intact := true
			for j := i; j < ref.end; j++ {
				if rs[j] != m.original[j] || m.omitted[j] {
					intact = false
					break
				}
			}
			if intact {
				for j := i + 1; j < ref.end; j++ {
					p.prefix[j] = len(p.runes)
				}
				for _, r := range ref.visible {
					p.runes = append(p.runes, r)
					p.positions = append(p.positions, i)
					p.ends = append(p.ends, ref.end)
				}
				i = ref.end
				continue
			}
		}
		if !m.omitted[i] {
			p.runes = append(p.runes, rs[i])
			p.positions = append(p.positions, i)
			p.ends = append(p.ends, i+1)
		}
		i++
	}
	p.prefix[len(rs)] = len(p.runes)
	return p
}

func (p projectedMarkdown) escapePolicy(literal []bool) []bool {
	allowed := make([]bool, len(p.runes))
	for i := 0; i+1 < len(p.runes); i++ {
		allowed[i] = !literal[p.positions[i]] && p.ends[i] == p.positions[i]+1 && p.ends[i] == p.positions[i+1]
	}
	return allowed
}

// Goldmark owns link recognition; the delegate records its consumed source range.
type sourceLinkParser struct {
	parser.InlineParser

	source *markdownSource
}

func (m *markdownSource) omitLink(n ast.Node, labelEnd, end int) {
	m.mask(m.controls, n.Pos(), end)
	openerEnd := n.Pos() + 1
	if n.Kind() == ast.KindImage {
		openerEnd++
	}
	m.omit(n.Pos(), openerEnd)
	m.omit(labelEnd, end)
}

func (p *sourceLinkParser) Parse(parent ast.Node, reader text.Reader, pc parser.Context) ast.Node {
	_, labelEnd := reader.Position()
	n := p.InlineParser.Parse(parent, reader, pc)
	if n != nil && (n.Kind() == ast.KindLink || n.Kind() == ast.KindImage) {
		_, end := reader.Position()
		p.source.omitLink(n, labelEnd.Start, end.Start)
	}
	return n
}

func (p *sourceLinkParser) CloseBlock(parent ast.Node, reader text.Reader, pc parser.Context) {
	if closer, ok := p.InlineParser.(parser.CloseBlocker); ok {
		closer.CloseBlock(parent, reader, pc)
	}
}
