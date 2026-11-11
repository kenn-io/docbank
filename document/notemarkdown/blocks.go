package notemarkdown

import (
	"bytes"
	"context"
	"sort"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

type sourceBlock struct {
	span, content Span // raw physical heading span, compact inline source
	level         int
}

type blockPart struct {
	segments        []text.Segment
	level, pos, end int
}

// Goldmark's callbacks have no error return. Only this private sentinel is
// recovered; unexpected panics retain their normal behavior.
type parseAbort struct{ err error }

func mustParse(err error) {
	if err != nil {
		panic(parseAbort{err})
	}
}

type blockGuard struct {
	parser.BlockParser
	ctx  context.Context
	b    *budget
	ends map[ast.Node]int
	kind blockParserKind
}

func (g *blockGuard) Open(parent ast.Node, r text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := r.PeekLine()
	mustParse(g.b.step(g.ctx, openWorkFactor[g.kind]*len(line)+1))
	// Open implementations construct at most one AST node. Reserve before
	// invoking one, and refund unsuccessful candidates; at the cap we fail
	// closed without calling a candidate that might allocate.
	if len(pc.OpenedBlocks()) > maxDepth {
		panic(parseAbort{&LimitError{Resource: "ast_depth"}})
	}
	mustParse(g.b.reserveNode(g.ctx))
	_, before := r.Position()
	n, state := g.BlockParser.Open(parent, r, pc)
	if n == nil {
		g.b.refundNode()
	} else {
		if len(pc.OpenedBlocks())+1 > maxDepth {
			panic(parseAbort{&LimitError{Resource: "ast_depth"}})
		}
		if _, ok := n.(*ast.Heading); ok {
			// Save the Setext underline before Goldmark replaces its Source
			// with the paragraph's inline content during Close.
			g.ends[n] = before.Stop
		}
	}
	_, after := r.Position()
	mustParse(g.b.step(g.ctx, max(0, after.Start-before.Start)))
	return n, state
}
func (g *blockGuard) Continue(n ast.Node, r text.Reader, pc parser.Context) parser.State {
	line, _ := r.PeekLine()
	mustParse(g.b.step(g.ctx, continueWorkFactor[g.kind]*len(line)+1))
	_, before := r.Position()
	state := g.BlockParser.Continue(n, r, pc)
	_, after := r.Position()
	mustParse(g.b.step(g.ctx, max(0, after.Start-before.Start)))
	return state
}
func (g *blockGuard) Close(n ast.Node, r text.Reader, pc parser.Context) {
	mustParse(closeWork(g, n))
	g.BlockParser.Close(n, r, pc)
}

type referenceGuard struct {
	ctx  context.Context
	b    *budget
	refs map[string]string
}

func (g *referenceGuard) Transform(n *ast.Paragraph, r text.Reader, _ parser.Context) {
	mustParse(g.b.step(g.ctx, 1))
	segments := n.Source()
	if len(segments) == 0 {
		return
	}
	first := bytes.TrimLeft(r.Source()[segments[0].Start:segments[0].Stop], " \t\r\n")
	if len(first) == 0 || first[0] != '[' {
		return
	}
	// Use the adopted metered reference recognizer on Goldmark's paragraph
	// segments. Goldmark calls this during block interruption, before deciding
	// whether a following list, HTML block or Setext underline starts a block.
	var view []byte
	ends := make([]int, 0, len(segments))
	for _, seg := range segments {
		mustParse(g.b.step(g.ctx, seg.Stop-seg.Start))
		mustParse(g.b.reserveStrings(seg.Stop - seg.Start))
		view = append(view, r.Source()[seg.Start:seg.Stop]...)
		ends = append(ends, len(view))
	}
	scanner := linkScanner{ctx: g.ctx, src: view, b: g.b, refs: g.refs}
	pos, removed := 0, 0
	for pos < len(view) {
		line, next, err := sourceLineAt(g.ctx, view, pos, g.b)
		mustParse(err)
		end, ok, err := scanner.definition(line, len(view))
		mustParse(err)
		if !ok {
			break
		}
		pos = max(next, definitionNext(view, end))
		removed = sort.SearchInts(ends, pos) + 1
		removed = min(removed, len(segments))
	}
	if removed > 0 {
		if removed == len(segments) {
			// Goldmark may Close a transformed paragraph again. Leave its
			// original Source attached, as its own transformer does.
			n.Parent().RemoveChild(n)
		} else {
			n.SetSource(segments[removed:])
		}
	}

}

func newBlockParserSet(ctx context.Context, b *budget, ends map[ast.Node]int) []parser.BlockParser {
	parsers := []parser.BlockParser{
		parser.NewSetextHeadingParser(), parser.NewThematicBreakParser(),
		parser.NewListParser(), parser.NewListItemParser(), parser.NewCodeBlockParser(),
		parser.NewATXHeadingParser(), parser.NewFencedCodeBlockParser(),
		parser.NewBlockquoteParser(), parser.NewHTMLBlockParser(), parser.NewParagraphParser(),
	}
	guarded := make([]parser.BlockParser, 0, len(parsers))
	for i, bp := range parsers {
		guarded = append(guarded, &blockGuard{BlockParser: bp, ctx: ctx, b: b, ends: ends, kind: blockParserKind(i)})
	}
	return guarded
}

func newGoldmarkBlockParser(parsers []parser.BlockParser, transformer parser.ParagraphTransformer) parser.Parser {
	priorities := make([]util.PrioritizedValue[parser.BlockParser], 0, len(parsers))
	for i, bp := range parsers {
		priorities = append(priorities, util.Prioritized(bp, (i+1)*100))
	}
	return parser.New(parser.WithDefaultParsers(false), parser.WithBlockParsers(priorities...),
		parser.WithParagraphTransformers(util.Prioritized[parser.ParagraphTransformer](transformer, 100)))
}

// scanBlocks builds one compact inline view from Goldmark's eligible blocks.
// Each source segment retains an original-byte change point; container markers
// disappear only from the view. Neither source bytes nor returned spans change.
func scanBlocks(ctx context.Context, original []byte, bodyStart int, refs map[string]string, b *budget) (view []byte, blocks []sourceBlock, offsets []linkSourceOffset, err error) {
	defer func() {
		if caught := recover(); caught != nil {
			if abort, ok := caught.(parseAbort); ok {
				view, blocks, offsets, err = nil, nil, nil, abort.err
			} else {
				panic(caught)
			}
		}
	}()
	if err := b.step(ctx, 1); err != nil {
		return nil, nil, nil, err
	}
	if err := b.reserveStrings(len(original) - bodyStart); err != nil {
		return nil, nil, nil, err
	}
	src := bytes.Clone(original[bodyStart:])
	// Bare CR is a CommonMark line ending; Goldmark expects LF. Replacement
	// preserves byte widths, and CRLF is already understood by Goldmark.
	for i := range src {
		if err := b.step(ctx, 1); err != nil {
			return nil, nil, nil, err
		}
		if src[i] == '\r' && (i+1 == len(src) || src[i+1] != '\n') {
			src[i] = '\n'
		}
	}
	var parts []blockPart
	ends := map[ast.Node]int{}
	transformer := &referenceGuard{ctx: ctx, b: b, refs: refs}
	guarded := newBlockParserSet(ctx, b, ends)
	root := parseBlockDocument(ctx, src, guarded, transformer, b)
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	var visit func(ast.Node, int) error
	visit = func(n ast.Node, depth int) error {
		if depth > maxDepth {
			return &LimitError{Resource: "ast_depth"}
		}
		if err := b.step(ctx, 1); err != nil {
			return err
		}
		part := blockPart{pos: n.Pos()}
		switch n := n.(type) {
		case *ast.Heading:
			part.segments, part.level, part.end = n.Source(), n.Level, ends[n]
		case *ast.Paragraph:
			part.segments = n.Source()
		}
		if part.level != 0 || len(part.segments) > 0 {
			parts = append(parts, part)
		}
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			if _, ok := child.(ast.BlockNode); ok {
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(root, 0); err != nil {
		return nil, nil, nil, err
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].pos < parts[j].pos })
	for _, part := range parts {
		start := len(view)
		for _, seg := range part.segments {
			if err := b.step(ctx, seg.Stop-seg.Start); err != nil {
				return nil, nil, nil, err
			}
			if err := b.reserveStrings(seg.Stop - seg.Start); err != nil {
				return nil, nil, nil, err
			}
			offsets = append(offsets, linkSourceOffset{original: bodyStart + seg.Start, view: len(view)})
			view = append(view, src[seg.Start:seg.Stop]...)
		}
		content := Span{start, len(view)}
		if err := b.reserveStrings(2); err != nil {
			return nil, nil, nil, err
		}
		view = append(view, '\n', '\n')
		block := sourceBlock{content: content, level: part.level}
		if part.level > 0 {
			pos := bodyStart + part.pos
			if len(part.segments) > 0 {
				pos = bodyStart + part.segments[0].Start
			}
			first := pos
			for first > bodyStart && original[first-1] != '\n' && original[first-1] != '\r' {
				first--
				if err := b.step(ctx, 1); err != nil {
					return nil, nil, nil, err
				}
			}
			end := bodyStart + part.end
			if end > first && (original[end-1] == '\n' || original[end-1] == '\r') {
				block.span = Span{first, end}
			} else {
				_, next, err := sourceLineAt(ctx, original, max(first, end), b)
				if err != nil {
					return nil, nil, nil, err
				}
				block.span = Span{first, next}
			}
		}
		blocks = append(blocks, block)
	}
	return view, blocks, offsets, nil
}

func byteRun(ctx context.Context, s []byte, c byte, b *budget) (int, error) {
	i := 0
	for i < len(s) && s[i] == c {
		if err := b.step(ctx, 1); err != nil {
			return 0, err
		}
		i++
	}
	return i, nil
}
