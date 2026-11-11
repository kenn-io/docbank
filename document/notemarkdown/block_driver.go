package notemarkdown

import (
	"context"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

type blockParserKind uint8

const (
	blockSetext blockParserKind = iota
	blockThematicBreak
	blockList
	blockListItem
	blockIndentedCode
	blockATX
	blockFencedCode
	blockQuote
	blockHTML
	blockParagraph
)

// These conservative byte-pass bounds cover the pinned parser callbacks.
// The driver charges each callback before entering its line-scanning code.
var openWorkFactor = [...]int{4, 3, 4, 6, 3, 8, 8, 3, 16, 2}
var continueWorkFactor = [...]int{1, 1, 8, 6, 4, 1, 8, 3, 4, 3}

func parseBlockDocument(ctx context.Context, source []byte, parsers []parser.BlockParser, transformer parser.ParagraphTransformer, b *budget) *ast.Document {
	// Goldmark's Parser.Parse allocates this root before either parse pass.
	mustParse(b.node(ctx, 0))
	root := ast.NewDocument()
	reader := text.NewReader(source, text.NewDecoder())
	pc := parser.NewContext()
	pc.SetOpenedBlocks(nil)
	mustParse(b.step(ctx, len(parsers)+256))

	var free []parser.BlockParser
	var byTrigger [256][]parser.BlockParser
	for _, bp := range parsers {
		triggers := bp.Trigger()
		if triggers == nil {
			free = append(free, bp)
			continue
		}
		for _, trigger := range triggers {
			byTrigger[trigger] = append(byTrigger[trigger], bp)
		}
	}
	// Goldmark appends free parsers after triggered parsers in every bucket.
	for i := range byTrigger {
		if byTrigger[i] != nil {
			byTrigger[i] = append(byTrigger[i], free...)
		}
	}

	state := blockDriver{ctx: ctx, b: b, free: free, byTrigger: byTrigger, transformer: transformer, reader: reader, pc: pc}
	state.parseBlocks(root)
	return root
}

type blockDriver struct {
	ctx         context.Context
	b           *budget
	free        []parser.BlockParser
	byTrigger   [256][]parser.BlockParser
	transformer parser.ParagraphTransformer
	reader      text.Reader
	pc          parser.Context
}

type blockOpenResult int

const (
	paragraphContinuation blockOpenResult = iota + 1
	newBlocksOpened
	noBlocksOpened
)

func (d *blockDriver) parseBlocks(parent ast.Node) {
	d.pc.SetOpenedBlocks(nil)
	var prevLineBlank, curLineBlank []bool
	for {
		if !d.skipBlankLines() {
			return
		}
		if d.openBlocks(parent, true) != newBlocksOpened {
			return
		}
		mustParse(d.b.step(d.ctx, 1))
		d.reader.AdvanceLine()
		prevLineBlank = prevLineBlank[:0]
		for {
			opened := d.pc.OpenedBlocks()
			l := len(opened)
			if l == 0 {
				break
			}
			lastIndex := l - 1
			curLineBlank = curLineBlank[:0]
			for i := range l {
				be := opened[i]
				line, _ := d.reader.PeekLine()
				if line == nil {
					mustParse(d.b.step(d.ctx, 1))
					d.closeBlocks(lastIndex, 0)
					mustParse(d.b.step(d.ctx, 1))
					d.reader.AdvanceLine()
					return
				}
				mustParse(d.b.step(d.ctx, len(line)+1)) // blank test and line bookkeeping
				curLineBlank = append(curLineBlank, util.IsBlank(line))
				// Paragraph continuation is decided by openBlocks, as in Goldmark.
				if !ast.IsParagraph(be.Node) {
					continueState := be.Parser.Continue(be.Node, d.reader, d.pc)
					if continueState&parser.Continue != 0 {
						if continueState&parser.HasChildren != 0 && i == lastIndex {
							isBlank := isBlankLine(i+1, prevLineBlank)
							d.openBlocks(be.Node, isBlank)
							break
						}
						continue
					}
				}
				isBlank := isBlankLine(i, prevLineBlank)
				thisParent := parent
				if i != 0 {
					thisParent = opened[i-1].Node
				}
				lastNode := opened[lastIndex].Node
				result := d.openBlocks(thisParent, isBlank)
				if result != paragraphContinuation {
					if opened[lastIndex].Node != lastNode {
						lastIndex--
					}
					d.closeBlocks(lastIndex, i)
				}
				break
			}
			mustParse(d.b.step(d.ctx, 1))
			d.reader.AdvanceLine()
			prevLineBlank, curLineBlank = curLineBlank, prevLineBlank
		}
	}
}

func (d *blockDriver) skipBlankLines() bool {
	for {
		line, _ := d.reader.PeekLine()
		if line == nil {
			return false
		}
		mustParse(d.b.step(d.ctx, len(line)+1))
		if !util.IsBlank(line) {
			return true
		}
		mustParse(d.b.step(d.ctx, 1))
		d.reader.AdvanceLine()
	}
}

func (d *blockDriver) openBlocks(parent ast.Node, blankLine bool) blockOpenResult {
	result := blockOpenResult(noBlocksOpened)
	continuable := false
	lastBlock := d.pc.LastOpenedBlock()
	if lastBlock.Node != nil {
		continuable = ast.IsParagraph(lastBlock.Node)
	}
retry:
	line, seg := d.reader.PeekLine()
	mustParse(d.b.step(d.ctx, len(line)+1)) // indent and line-offset scan
	w, pos := util.IndentWidth(line, d.reader.LineOffset())
	if len(line) == 0 {
		d.pc.SetBlockOffset(-1)
		d.pc.SetBlockIndent(-1)
	} else {
		d.pc.SetBlockOffset(pos)
		d.pc.SetBlockIndent(w)
	}
	if line == nil || line[0] == '\n' {
		goto continuable
	}
	bps := d.free
	if pos < len(line) {
		if triggered := d.byTrigger[line[pos]]; triggered != nil {
			bps = triggered
		}
	}
	for _, bp := range bps {
		mustParse(d.b.step(d.ctx, 1))
		if continuable && result == noBlocksOpened && !bp.CanInterruptParagraph() {
			continue
		}
		if w > 3 && !bp.CanAcceptIndentedLine() {
			continue
		}
		lastBlock = d.pc.LastOpenedBlock()
		last := lastBlock.Node
		_, blockPos := d.reader.Position()
		node, state := bp.Open(parent, d.reader, d.pc)
		if node == nil {
			continue
		}
		node.SetPos(blockPos.Start + max(d.pc.BlockOffset()-seg.Padding, 0))
		if state&parser.RequireParagraph != 0 && last == parent.LastChild() {
			lastBlock.Parser.Close(last, d.reader, d.pc)
			blocks := d.pc.OpenedBlocks()
			d.pc.SetOpenedBlocks(blocks[:len(blocks)-1])
			if d.transformParagraph(last.(*ast.Paragraph)) {
				continuable = false
				goto retry
			}
		}
		node.(ast.BlockNode).SetBlankPreviousLines(blankLine)
		if last != nil && last.Parent() == nil {
			lastPos := len(d.pc.OpenedBlocks()) - 1
			d.closeBlocks(lastPos, lastPos)
		}
		parent.AppendChild(node)
		result = newBlocksOpened
		d.pc.SetOpenedBlocks(append(d.pc.OpenedBlocks(), parser.Block{Node: node, Parser: bp}))
		if state&parser.HasChildren != 0 {
			parent = node
			goto retry
		}
		break
	}

continuable:
	if result == noBlocksOpened && continuable {
		state := lastBlock.Parser.Continue(lastBlock.Node, d.reader, d.pc)
		if state&parser.Continue != 0 {
			result = paragraphContinuation
		}
	}
	return result
}

func (d *blockDriver) closeBlocks(from, to int) {
	blocks := d.pc.OpenedBlocks()
	for i := from; i >= to; i-- {
		node := blocks[i].Node
		blocks[i].Parser.Close(node, d.reader, d.pc)
		paragraph, ok := node.(*ast.Paragraph)
		if ok && node.Parent() != nil {
			d.transformParagraph(paragraph)
			continue
		}
	}
	if from == len(blocks)-1 {
		blocks = blocks[:to]
	} else {
		blocks = append(blocks[:to], blocks[from+1:]...)
	}
	d.pc.SetOpenedBlocks(blocks)
}

func (d *blockDriver) transformParagraph(node *ast.Paragraph) bool {
	d.transformer.Transform(node, d.reader, d.pc)
	return node.Parent() == nil
}

func isBlankLine(level int, previous []bool) bool {
	if len(previous) == 0 {
		return false
	}
	if level >= len(previous) {
		level = len(previous) - 1
	}
	return previous[level]
}

func closeWork(g *blockGuard, node ast.Node) error {
	if err := g.b.step(g.ctx, 1); err != nil {
		return err
	}
	switch g.kind {
	case blockList:
		list := node.(*ast.List)
		visits := 0
		tight := list.IsTight
		for c := node.FirstChild(); c != nil && tight; c = c.NextSibling() {
			if err := g.b.step(g.ctx, 1); err != nil {
				return err
			}
			visits++
			if c.FirstChild() != nil && c.FirstChild() != c.LastChild() {
				for c1 := c.FirstChild().NextSibling(); c1 != nil; c1 = c1.NextSibling() {
					if err := g.b.step(g.ctx, 1); err != nil {
						return err
					}
					visits++
					if c1.(ast.BlockNode).HasBlankPreviousLines() {
						tight = false
						break
					}
				}
			}
			if c != node.FirstChild() && c.(ast.BlockNode).HasBlankPreviousLines() {
				tight = false
			}
		}
		// List.Close repeats this bounded direct-child walk.
		return g.b.step(g.ctx, visits)
	case blockIndentedCode:
		segments := node.(*ast.CodeBlock).Value.Segments()
		work := 0
		for _, segment := range segments {
			if err := g.b.step(g.ctx, 1); err != nil {
				return err
			}
			work += segment.Len()
			if segment.ForceNewline {
				work++
			}
		}
		// CodeBlock.Close can inspect every trailing segment if all are blank.
		return g.b.step(g.ctx, work)
	case blockParagraph:
		segments := node.(*ast.Paragraph).Source()
		if len(segments) != 0 {
			return g.b.step(g.ctx, segments[len(segments)-1].Len())
		}
	case blockSetext:
		// In this configuration Setext.Close always reuses a surviving
		// paragraph's nonempty source; charge the underline access defensively.
		segments := node.(*ast.Heading).Source()
		if len(segments) != 0 {
			return g.b.step(g.ctx, segments[0].Len())
		}
	}
	return nil
}
