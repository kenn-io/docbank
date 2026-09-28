package document

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"github.com/yuin/goldmark"
	goldmarkast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
	"golang.org/x/net/html"
)

// sanitizeRenditionMarkdown applies the normalizer's frozen text rules plus
// removes the body of active HTML elements from a durable rendition.
func sanitizeRenditionMarkdown(markdown string, maxLinkChars, maxSourceBytes, maxRunes int) (string, bool, bool, error) {
	if markdown == "" {
		return "", false, false, nil
	}
	if !utf8.ValidString(markdown) {
		return "", false, false, errors.New("provider Markdown is invalid UTF-8")
	}
	markdown, sourceTruncated := truncateUTF8Bytes(markdown, maxSourceBytes)
	var rendered bytes.Buffer
	listTightness := make(map[int]bool)
	rawSpans := make([]renditionRawSpan, 0)
	parser := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(
			goldmarkhtml.WithUnsafe(),
			renderer.WithNodeRenderers(util.Prioritized(&renditionHTMLRenderer{
				output: &rendered, tightness: listTightness, rawSpans: &rawSpans,
			}, 0)),
		),
	)
	source := []byte(markdown)
	if err := parser.Convert(source, &rendered); err != nil {
		return "", false, false, fmt.Errorf("parse provider Markdown: %w", err)
	}
	writer := renditionHTMLWriter{maxLinkChars: maxLinkChars, listTightness: listTightness}
	if err := writer.consumeFragments(rendered.Bytes(), rawSpans); err != nil {
		return "", false, false, err
	}
	text, renditionTruncated := serializeRenditionBlocks(writer.blocks, maxRunes)
	renditionTruncated = renditionTruncated || writer.linkDepthTruncated
	return text, sourceTruncated, renditionTruncated, nil
}

// renditionHTMLRenderer records parser-derived list tightness and hard
// boundaries around every provider-controlled raw HTML node. Each bounded
// fragment gets an independent tokenizer so raw-text state cannot escape the
// source AST node that introduced it.
type renditionHTMLRenderer struct {
	output    *bytes.Buffer
	tightness map[int]bool
	rawSpans  *[]renditionRawSpan
}

type renditionRawSpan struct {
	start            int
	end              int
	inline           bool
	linkDestination  string
	closesLink       bool
	startsActiveBody bool
	endsActiveBody   bool
}

func (r *renditionHTMLRenderer) RegisterFuncs(registerer renderer.NodeRendererFuncRegisterer) {
	registerer.Register(goldmarkast.KindList, r.renderList)
	registerer.Register(goldmarkast.KindHTMLBlock, r.renderHTMLBlock)
	registerer.Register(goldmarkast.KindRawHTML, r.renderRawHTML)
}

func (r *renditionHTMLRenderer) offset(writer util.BufWriter) int {
	return r.output.Len() + writer.Buffered()
}

func (r *renditionHTMLRenderer) appendRawSpan(start int, writer util.BufWriter, inline bool) {
	end := r.offset(writer)
	if end > start {
		*r.rawSpans = append(*r.rawSpans, renditionRawSpan{start: start, end: end, inline: inline})
	}
}

func (r *renditionHTMLRenderer) renderList(
	writer util.BufWriter,
	_ []byte,
	node goldmarkast.Node,
	entering bool,
) (goldmarkast.WalkStatus, error) {
	list, ok := node.(*goldmarkast.List)
	if !ok {
		return goldmarkast.WalkStop, fmt.Errorf("render list node: unexpected %T", node)
	}
	tag := "ul"
	if list.IsOrdered() {
		tag = "ol"
	}
	if entering {
		r.tightness[r.offset(writer)] = list.IsTight
		_ = writer.WriteByte('<')
		_, _ = writer.WriteString(tag)
		if list.IsOrdered() && list.Start != 1 {
			_, _ = fmt.Fprintf(writer, " start=\"%d\"", list.Start)
		}
		if list.Attributes() != nil {
			goldmarkhtml.RenderAttributes(writer, list, goldmarkhtml.ListAttributeFilter)
		}
		_, _ = writer.WriteString(">\n")
	} else {
		_, _ = writer.WriteString("</")
		_, _ = writer.WriteString(tag)
		_, _ = writer.WriteString(">\n")
	}
	return goldmarkast.WalkContinue, nil
}

func (r *renditionHTMLRenderer) renderHTMLBlock(
	writer util.BufWriter,
	source []byte,
	node goldmarkast.Node,
	entering bool,
) (goldmarkast.WalkStatus, error) {
	block, ok := node.(*goldmarkast.HTMLBlock)
	if !ok {
		return goldmarkast.WalkStop, fmt.Errorf("render HTML block node: unexpected %T", node)
	}
	start := r.offset(writer)
	if entering {
		lines := block.Lines()
		for index := range lines.Len() {
			segment := lines.At(index)
			goldmarkhtml.DefaultWriter.SecureWrite(writer, segment.Value(source))
		}
	} else if block.HasClosure() {
		goldmarkhtml.DefaultWriter.SecureWrite(writer, block.ClosureLine.Value(source))
	}
	r.appendRawSpan(start, writer, false)
	return goldmarkast.WalkContinue, nil
}

func (r *renditionHTMLRenderer) renderRawHTML(
	writer util.BufWriter,
	source []byte,
	node goldmarkast.Node,
	entering bool,
) (goldmarkast.WalkStatus, error) {
	if !entering {
		return goldmarkast.WalkContinue, nil
	}
	raw, ok := node.(*goldmarkast.RawHTML)
	if !ok {
		return goldmarkast.WalkStop, fmt.Errorf("render raw HTML node: unexpected %T", node)
	}
	start := r.offset(writer)
	for index := range raw.Segments.Len() {
		segment := raw.Segments.At(index)
		_, _ = writer.Write(segment.Value(source))
	}
	r.appendRawSpan(start, writer, true)
	return goldmarkast.WalkSkipChildren, nil
}

type renditionBlockKind uint8

const (
	renditionParagraph renditionBlockKind = iota
	renditionHeading
	renditionCodeBlock
	renditionTable
	renditionListBlock
)

type renditionInlineKind uint8

const (
	renditionText renditionInlineKind = iota
	renditionInlineCode
	renditionLinkInline
)

type renditionInline struct {
	kind        renditionInlineKind
	text        string
	destination string
	children    []renditionInline
}

const maxRenditionInlineDepth = 64

type renditionBlock struct {
	kind     renditionBlockKind
	level    int
	inlines  []renditionInline
	language string
	code     string
	rows     [][][]renditionInline
	list     *renditionList
}

type renditionList struct {
	ordered bool
	start   string
	tight   bool
	items   []renditionListItem
}

type renditionListItem struct {
	present bool
	blocks  []renditionBlock
}

type renditionListFrame struct {
	list      *renditionList
	itemIndex int
}

// renditionHTMLWriter builds the safe rendition's semantic blocks before any
// Markdown is emitted. Its output is intentionally separate from the frozen
// canonicalHTMLWriter used by NormalizeDocument.
type renditionHTMLWriter struct {
	work           *renditionXHTMLWork
	maxLinkChars   int
	rawFragment    bool
	blocks         []renditionBlock
	listTightness  map[int]bool
	renderedOffset int
	currentKind    renditionBlockKind
	currentLevel   int
	current        []renditionInline
	pendingSpace   bool

	skipTag            string
	skipDepth          int
	inPre              bool
	preInCell          bool
	preLang            string
	preText            strings.Builder
	inlineCode         bool
	inlineText         strings.Builder
	ignoredLinkDepth   int
	linkDepthTruncated bool
	suppressedTags     []string

	inTable          bool
	inRow            bool
	inCell           bool
	nestedTableDepth int
	tableListDepth   int
	table            [][][]renditionInline
	tableRow         [][]renditionInline
	tableCell        []renditionInline
	links            []renditionInline
	lists            []renditionListFrame
}

func (w *renditionHTMLWriter) consumeFragments(rendered []byte, rawSpans []renditionRawSpan) error {
	sort.Slice(rawSpans, func(left, right int) bool {
		return rawSpans[left].start < rawSpans[right].start
	})
	pairRenditionRawActiveElements(rendered, rawSpans)
	pairRenditionRawLinks(rendered, rawSpans, w.maxLinkChars)
	offset := 0
	activeDepth := 0
	for _, span := range rawSpans {
		if span.start < offset || span.end > len(rendered) {
			continue
		}
		if span.start > offset {
			if err := w.consumeFragment(rendered[offset:span.start], activeDepth > 0); err != nil {
				return err
			}
		}
		switch {
		case span.startsActiveBody:
			w.pendingSpace = false
			activeDepth++
		case span.endsActiveBody:
			if activeDepth > 0 {
				activeDepth--
			}
		case activeDepth > 0:
			// Provider raw HTML inside an active element is not searchable evidence.
		case span.linkDestination != "":
			w.flushPendingSpace()
			w.startLink(span.linkDestination)
		case span.closesLink:
			w.endTag("a")
		default:
			if err := w.consumeRawFragment(rendered[span.start:span.end], span.inline); err != nil {
				return err
			}
		}
		w.renderedOffset = span.end
		offset = span.end
	}
	if offset < len(rendered) {
		if err := w.consumeFragment(rendered[offset:], activeDepth > 0); err != nil {
			return err
		}
	}
	w.finalize()
	return nil
}

func pairRenditionRawActiveElements(rendered []byte, spans []renditionRawSpan) {
	type activeElement struct {
		tag   string
		index int
	}
	stack := make([]activeElement, 0)
	for index := range spans {
		span := &spans[index]
		fragment := rendered[span.start:span.end]
		if tag, ok := isolatedRawActiveStart(fragment); ok {
			stack = append(stack, activeElement{tag: tag, index: index})
			continue
		}
		tag, ok := isolatedRawActiveEnd(fragment)
		if !ok || len(stack) == 0 {
			continue
		}
		matching := len(stack) - 1
		for matching >= 0 && stack[matching].tag != tag {
			matching--
		}
		if matching < 0 {
			continue
		}
		opening := stack[matching]
		stack = stack[:matching]
		spans[opening.index].startsActiveBody = true
		span.endsActiveBody = true
	}
}

func isolatedRawActiveStart(fragment []byte) (string, bool) {
	fragment = bytes.TrimSpace(fragment)
	tokenizer := html.NewTokenizer(bytes.NewReader(fragment))
	tokenType := tokenizer.Next()
	if tokenType != html.StartTagToken && tokenType != html.SelfClosingTagToken {
		return "", false
	}
	tag := tokenizer.Token().Data
	if !isRenditionActiveHTML(tag) || isHTMLVoidElement(tag) || tokenizer.Next() != html.ErrorToken ||
		!errors.Is(tokenizer.Err(), io.EOF) {
		return "", false
	}
	return tag, true
}

func isolatedRawActiveEnd(fragment []byte) (string, bool) {
	fragment = bytes.TrimSpace(fragment)
	tokenizer := html.NewTokenizer(bytes.NewReader(fragment))
	if tokenizer.Next() != html.EndTagToken {
		return "", false
	}
	tag := tokenizer.Token().Data
	if !isRenditionActiveHTML(tag) || tokenizer.Next() != html.ErrorToken || !errors.Is(tokenizer.Err(), io.EOF) {
		return "", false
	}
	return tag, true
}

func isRenditionActiveHTML(tag string) bool {
	return tag == "script" || tag == "style" || tag == "svg" || isActiveHTML(tag)
}

func pairRenditionRawLinks(rendered []byte, spans []renditionRawSpan, maxLinkChars int) {
	for index := 0; index+1 < len(spans); index++ {
		opening := &spans[index]
		closing := &spans[index+1]
		if !opening.inline || !closing.inline {
			continue
		}
		destination, ok := isolatedRawLinkStart(rendered[opening.start:opening.end], maxLinkChars)
		if !ok || !isolatedRawLinkEnd(rendered[closing.start:closing.end]) ||
			!isInlineGeneratedHTML(rendered[opening.end:closing.start]) {
			continue
		}
		opening.linkDestination = destination
		closing.closesLink = true
		index++
	}
}

func isolatedRawLinkStart(fragment []byte, maxLinkChars int) (string, bool) {
	tokenizer := html.NewTokenizer(bytes.NewReader(fragment))
	if tokenizer.Next() != html.StartTagToken {
		return "", false
	}
	token := tokenizer.Token()
	if token.Data != "a" || tokenizer.Next() != html.ErrorToken || !errors.Is(tokenizer.Err(), io.EOF) {
		return "", false
	}
	for _, attribute := range token.Attr {
		if attribute.Key == "href" {
			destination := safeRenditionLink(attribute.Val, maxLinkChars)
			return destination, destination != ""
		}
	}
	return "", false
}

func isolatedRawLinkEnd(fragment []byte) bool {
	tokenizer := html.NewTokenizer(bytes.NewReader(fragment))
	if tokenizer.Next() != html.EndTagToken || tokenizer.Token().Data != "a" {
		return false
	}
	return tokenizer.Next() == html.ErrorToken && errors.Is(tokenizer.Err(), io.EOF)
}

func isInlineGeneratedHTML(fragment []byte) bool {
	tokenizer := html.NewTokenizer(bytes.NewReader(fragment))
	for {
		switch tokenizer.Next() {
		case html.TextToken, html.CommentToken:
			continue
		case html.ErrorToken:
			return errors.Is(tokenizer.Err(), io.EOF)
		default:
			return false
		}
	}
}

func (w *renditionHTMLWriter) consumeRawFragment(fragment []byte, inline bool) error {
	raw := renditionHTMLWriter{maxLinkChars: w.maxLinkChars, rawFragment: true}
	if err := raw.consumeFragment(fragment, false); err != nil {
		return err
	}
	raw.finalize()
	w.linkDepthTruncated = w.linkDepthTruncated || raw.linkDepthTruncated
	raw.blocks = readableRawBlocks(raw.blocks)
	if inline {
		w.appendRawInlineBlocks(raw.blocks)
		return nil
	}
	if len(raw.blocks) == 0 {
		return nil
	}
	w.startBlockBoundary()
	for _, block := range raw.blocks {
		w.appendBlock(block)
	}
	return nil
}

func readableRawBlocks(blocks []renditionBlock) []renditionBlock {
	readable := blocks[:0]
	for _, block := range blocks {
		switch block.kind {
		case renditionParagraph, renditionHeading:
			inlines := block.inlines[:0]
			for _, inline := range block.inlines {
				if inline.kind != renditionText && strings.TrimSpace(inline.text) == "" && len(inline.children) == 0 {
					continue
				}
				inlines = append(inlines, inline)
			}
			block.inlines = inlines
			if len(block.inlines) == 0 {
				continue
			}
		case renditionCodeBlock:
			if strings.TrimSpace(block.code) == "" {
				continue
			}
		case renditionTable, renditionListBlock:
			// Their child topology determines readability during serialization.
		}
		readable = append(readable, block)
	}
	return readable
}

func (w *renditionHTMLWriter) appendRawInlineBlocks(blocks []renditionBlock) {
	for _, block := range blocks {
		if w.targetHasContent() {
			w.pendingSpace = true
		}
		switch block.kind {
		case renditionParagraph, renditionHeading:
			w.flushPendingSpace()
			w.appendFlattenedInlines(block.inlines)
		case renditionCodeBlock:
			w.writeText(block.code)
		case renditionTable:
			for _, row := range block.rows {
				for _, cell := range row {
					w.flushPendingSpace()
					w.appendFlattenedInlines(cell)
					w.pendingSpace = true
				}
			}
		case renditionListBlock:
			if block.list != nil {
				for _, item := range block.list.items {
					w.appendRawInlineBlocks(item.blocks)
				}
			}
		}
	}
}

func (w *renditionHTMLWriter) consumeFragment(fragment []byte, suppressText bool) error {
	tokenizer := html.NewTokenizer(bytes.NewReader(fragment))
	for {
		tokenType := tokenizer.Next()
		tokenOffset := w.renderedOffset
		w.renderedOffset += len(tokenizer.Raw())
		switch tokenType {
		case html.ErrorToken:
			if errors.Is(tokenizer.Err(), io.EOF) {
				return nil
			}
			return fmt.Errorf("tokenize rendition HTML: %w", tokenizer.Err())
		case html.TextToken:
			if w.skipDepth == 0 && !suppressText {
				w.writeText(string(tokenizer.Text()))
			}
		case html.StartTagToken:
			w.startTag(tokenizer.Token(), tokenOffset, suppressText)
		case html.SelfClosingTagToken:
			w.startTag(tokenizer.Token(), tokenOffset, suppressText)
		case html.EndTagToken:
			tag := tokenizer.Token().Data
			if !w.endSuppressedTag(tag) {
				w.endTag(tag)
			}
		case html.CommentToken, html.DoctypeToken:
			// Not searchable evidence.
		}
	}
}

func (w *renditionHTMLWriter) finalize() {
	if w.inlineCode {
		w.appendInline(renditionInline{kind: renditionInlineCode, text: stripUnsafeControls(w.inlineText.String())})
		w.inlineCode = false
	}
	if w.inPre {
		w.endTag("pre")
	}
	w.closeLinks()
	w.nestedTableDepth = 0
	if w.inCell {
		w.endTag("td")
	}
	if w.inTable {
		if w.inRow {
			w.endTag("tr")
		}
		w.endTag("table")
	}
	w.finishBlock()
}

func (w *renditionHTMLWriter) startTag(token html.Token, tokenOffset int, suppressText bool) {
	tag := token.Data
	if w.skipDepth > 0 {
		if tag == w.skipTag {
			w.skipDepth++
		}
		return
	}
	if suppressText {
		if isRenditionStatefulTag(tag) {
			w.suppressedTags = append(w.suppressedTags, tag)
		}
		return
	}
	if tag == "input" && !w.rawFragment && !suppressText {
		if marker, ok := parserGeneratedCheckboxMarker(token); ok {
			w.writeText(marker)
			return
		}
	}
	if isRenditionActiveHTML(tag) {
		if !isHTMLVoidElement(tag) {
			w.skipTag = tag
			w.skipDepth = 1
		}
		return
	}
	if w.nestedTableDepth > 0 {
		switch tag {
		case "table":
			w.nestedTableDepth++
			w.pendingSpace = true
			return
		case "thead", "tbody", "tfoot", "tr", "td", "th":
			w.pendingSpace = true
			return
		}
	}
	switch tag {
	case "ul", "ol":
		if w.inTable {
			w.pendingSpace = true
			w.tableListDepth++
			return
		}
		w.startBlockBoundary()
		start := "1"
		tight := true
		if parserTightness, ok := w.listTightness[tokenOffset]; ok {
			tight = parserTightness
		}
		if tag == "ol" {
			for _, attribute := range token.Attr {
				if attribute.Key == "start" {
					if parsed, ok := canonicalNonnegativeDecimal(attribute.Val); ok {
						start = parsed
					}
				}
			}
		}
		if !w.charge(2*int64(unsafe.Sizeof(renditionList{})) + 2*int64(unsafe.Sizeof(renditionListFrame{}))) {
			return
		}
		list := &renditionList{ordered: tag == "ol", start: start, tight: tight}
		w.appendBlock(renditionBlock{kind: renditionListBlock, list: list})
		w.lists = append(w.lists, renditionListFrame{list: list, itemIndex: -1})
	case "table":
		if w.inTable {
			w.nestedTableDepth = 1
			w.pendingSpace = true
			return
		}
		w.startBlockBoundary()
		w.inTable = true
		w.inRow = false
		w.table = nil
	case "thead", "tbody", "tfoot":
		// Table row and cell tokens carry the retained structure.
	case "tr":
		if w.inTable {
			if w.inRow {
				w.endTag("tr")
			}
			w.inRow = true
			w.tableRow = nil
		}
	case "td", "th":
		if w.inTable {
			if w.inCell {
				w.endTag("td")
			}
			if !w.inRow {
				w.inRow = true
				w.tableRow = nil
			}
			w.inCell = true
			w.tableCell = nil
		}
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.startBlock(renditionHeading, int(tag[1]-'0'))
	case "li":
		if w.inTable {
			w.pendingSpace = true
			return
		}
		w.startListItem()
	case "p", "div", "article", "section", "header", "footer", "main", "aside", "figure", "figcaption", "blockquote":
		if tag == "p" && len(w.lists) > 0 && w.lists[len(w.lists)-1].itemIndex >= 0 {
			w.lists[len(w.lists)-1].list.tight = false
		}
		w.startBlock(renditionParagraph, 0)
	case "br":
		w.pendingSpace = true
	case "pre":
		if !w.inCell {
			w.closeLinks()
			w.startBlockBoundary()
		}
		w.inPre = true
		w.preInCell = w.inCell
		w.preLang = ""
		w.preText.Reset()
	case "code":
		if w.inPre {
			for _, attribute := range token.Attr {
				if attribute.Key == "class" && strings.HasPrefix(attribute.Val, "language-") {
					w.preLang = safeCodeLanguage(strings.TrimPrefix(attribute.Val, "language-"))
				}
			}
			return
		}
		w.flushPendingSpace()
		w.inlineCode = true
		w.inlineText.Reset()
	case "img":
		if suppressText {
			return
		}
		for _, attribute := range token.Attr {
			if attribute.Key == "alt" {
				w.writeText(attribute.Val)
				break
			}
		}
	case "a":
		w.flushPendingSpace()
		destination := ""
		for _, attribute := range token.Attr {
			if attribute.Key == "href" {
				destination = safeRenditionLink(attribute.Val, w.maxLinkChars)
				break
			}
		}
		w.startLink(destination)
	default:
		if isHTMLBlockElement(tag) {
			w.startBlock(renditionParagraph, 0)
		}
	}
}

func isRenditionStatefulTag(tag string) bool {
	switch tag {
	case "ul", "ol", "li", "table", "tr", "td", "th", "pre", "code", "a":
		return true
	default:
		return false
	}
}

func (w *renditionHTMLWriter) endSuppressedTag(tag string) bool {
	if len(w.suppressedTags) == 0 || w.suppressedTags[len(w.suppressedTags)-1] != tag {
		return false
	}
	w.suppressedTags = w.suppressedTags[:len(w.suppressedTags)-1]
	return true
}

func parserGeneratedCheckboxMarker(token html.Token) (string, bool) {
	checkbox, checked := false, false
	for _, attribute := range token.Attr {
		switch attribute.Key {
		case "type":
			checkbox = attribute.Val == "checkbox"
		case "checked":
			checked = true
		}
	}
	if !checkbox {
		return "", false
	}
	if checked {
		return "[x] ", true
	}
	return "[ ] ", true
}

func (w *renditionHTMLWriter) endTag(tag string) {
	if w.skipDepth > 0 {
		if tag == w.skipTag {
			w.skipDepth--
			if w.skipDepth == 0 {
				w.skipTag = ""
			}
		}
		return
	}
	if w.nestedTableDepth > 0 {
		switch tag {
		case "table":
			w.nestedTableDepth--
			w.pendingSpace = true
			return
		case "thead", "tbody", "tfoot", "tr", "td", "th":
			w.pendingSpace = true
			return
		}
	}
	switch tag {
	case "ul", "ol":
		if w.inTable {
			if w.tableListDepth > 0 {
				w.tableListDepth--
			}
			return
		}
		w.closeLinks()
		w.finishBlock()
		if len(w.lists) > 0 {
			w.lists = w.lists[:len(w.lists)-1]
		}
	case "h1", "h2", "h3", "h4", "h5", "h6", "li", "p", "div", "article", "section", "header", "footer", "main", "aside", "figure", "figcaption", "blockquote":
		if tag == "li" && w.inTable {
			return
		}
		w.finishBlock()
		if tag == "li" && len(w.lists) > 0 {
			w.lists[len(w.lists)-1].itemIndex = -1
		}
	case "tr":
		if w.inTable && w.inRow {
			if w.inCell {
				w.endTag("td")
			}
			if !w.charge(2 * int64(unsafe.Sizeof([][]renditionInline{}))) {
				return
			}
			w.table = append(w.table, w.tableRow)
			w.tableRow = nil
			w.inRow = false
		}
	case "td", "th":
		if w.inTable && w.inCell {
			w.flushPendingSpace()
			if !w.charge(2 * int64(unsafe.Sizeof([]renditionInline{}))) {
				return
			}
			w.tableRow = append(w.tableRow, w.tableCell)
			w.tableCell = nil
			w.inCell = false
		}
	case "table":
		if w.inTable {
			if w.inCell {
				w.endTag("td")
			}
			if w.inRow {
				w.endTag("tr")
			}
			w.inTable = false
			w.inRow = false
			w.nestedTableDepth = 0
			w.tableListDepth = 0
			if len(w.table) > 0 {
				w.appendBlock(renditionBlock{kind: renditionTable, rows: w.table})
			}
			w.table = nil
		}
	case "pre":
		if w.inPre {
			content := stripUnsafeControls(w.preText.String())
			if w.preInCell {
				w.appendInline(renditionInline{kind: renditionInlineCode, text: w.collapseWhitespace(content)})
			} else {
				w.appendBlock(renditionBlock{kind: renditionCodeBlock, language: w.preLang, code: content})
			}
			w.inPre = false
			w.preInCell = false
			w.preLang = ""
			w.preText.Reset()
		}
	case "code":
		if !w.inPre && w.inlineCode {
			w.appendInline(renditionInline{kind: renditionInlineCode, text: stripUnsafeControls(w.inlineText.String())})
			w.inlineCode = false
			w.inlineText.Reset()
		}
	case "a":
		if w.ignoredLinkDepth > 0 {
			w.ignoredLinkDepth--
			return
		}
		if len(w.links) == 0 {
			return
		}
		link := w.links[len(w.links)-1]
		w.links = w.links[:len(w.links)-1]
		if link.destination == "" {
			w.appendFlattenedInlines(link.children)
			return
		}
		w.appendInline(link)
	}
}

func (w *renditionHTMLWriter) startLink(destination string) {
	if w.ignoredLinkDepth > 0 || len(w.links) >= maxRenditionInlineDepth {
		w.ignoredLinkDepth++
		w.linkDepthTruncated = true
		return
	}
	w.links = append(w.links, renditionInline{kind: renditionLinkInline, destination: destination})
}

func (w *renditionHTMLWriter) writeText(value string) {
	value = stripUnsafeControls(value)
	if w.inPre {
		if !w.charge(2 * int64(len(value))) {
			return
		}
		w.preText.WriteString(value)
		return
	}
	if w.inlineCode {
		if !w.charge(2 * int64(len(value))) {
			return
		}
		w.inlineText.WriteString(value)
		return
	}
	var chunk strings.Builder
	flushChunk := func() {
		if chunk.Len() == 0 {
			return
		}
		w.appendText(chunk.String())
		chunk.Reset()
	}
	for _, character := range value {
		if unicode.IsSpace(character) {
			flushChunk()
			w.pendingSpace = true
			continue
		}
		if w.pendingSpace {
			flushChunk()
			w.flushPendingSpace()
		}
		chunk.WriteRune(character)
	}
	flushChunk()
}

func (w *renditionHTMLWriter) collapseWhitespace(value string) string {
	var size int64
	for field := range strings.FieldsSeq(value) {
		if size > 0 {
			size++
		}
		size += int64(len(field))
	}
	if !w.charge(size) {
		return ""
	}
	var output strings.Builder
	output.Grow(int(size))
	for field := range strings.FieldsSeq(value) {
		if output.Len() > 0 {
			output.WriteByte(' ')
		}
		output.WriteString(field)
	}
	return output.String()
}

func (w *renditionHTMLWriter) startBlock(kind renditionBlockKind, level int) {
	if w.inTable {
		w.pendingSpace = true
		return
	}
	w.closeLinks()
	w.finishBlock()
	w.currentKind = kind
	w.currentLevel = level
}

func (w *renditionHTMLWriter) startListItem() {
	if !w.charge(2 * int64(unsafe.Sizeof(renditionListItem{}))) {
		return
	}
	w.closeLinks()
	w.finishBlock()
	if len(w.lists) == 0 {
		w.currentKind = renditionParagraph
		return
	}
	frame := &w.lists[len(w.lists)-1]
	frame.list.items = append(frame.list.items, renditionListItem{present: true})
	frame.itemIndex = len(frame.list.items) - 1
	w.currentKind = renditionParagraph
}

func (w *renditionHTMLWriter) startBlockBoundary() {
	if !w.inTable {
		w.closeLinks()
		w.finishBlock()
	}
}

func (w *renditionHTMLWriter) finishBlock() {
	w.flushPendingSpace()
	if len(w.current) > 0 {
		w.appendBlock(renditionBlock{kind: w.currentKind, level: w.currentLevel, inlines: w.current})
	}
	w.current = nil
	w.currentKind = renditionParagraph
	w.currentLevel = 0
	w.pendingSpace = false
}

func (w *renditionHTMLWriter) closeLinks() {
	for len(w.links) > 0 {
		link := w.links[len(w.links)-1]
		w.links = w.links[:len(w.links)-1]
		w.appendFlattenedInlines(link.children)
	}
}

func (w *renditionHTMLWriter) appendBlock(block renditionBlock) {
	if !w.charge(2 * int64(unsafe.Sizeof(renditionBlock{}))) {
		return
	}
	if len(w.lists) > 0 {
		frame := &w.lists[len(w.lists)-1]
		if frame.itemIndex >= 0 {
			item := &frame.list.items[frame.itemIndex]
			item.blocks = append(item.blocks, block)
			return
		}
	}
	w.blocks = append(w.blocks, block)
}

func (w *renditionHTMLWriter) appendFlattenedInlines(inlines []renditionInline) {
	for _, inline := range inlines {
		switch inline.kind {
		case renditionText, renditionInlineCode:
			w.appendText(inline.text)
		case renditionLinkInline:
			w.appendFlattenedInlines(inline.children)
		}
	}
}

func (w *renditionHTMLWriter) flushPendingSpace() {
	if w.pendingSpace && w.targetHasContent() {
		w.appendText(" ")
	}
	w.pendingSpace = false
}

func (w *renditionHTMLWriter) targetHasContent() bool {
	if len(w.links) > 0 {
		return len(w.links[len(w.links)-1].children) > 0
	}
	if w.inCell {
		return len(w.tableCell) > 0
	}
	return len(w.current) > 0
}

func (w *renditionHTMLWriter) appendText(value string) {
	if value == "" {
		return
	}
	w.appendInline(renditionInline{kind: renditionText, text: value})
}

func (w *renditionHTMLWriter) appendInline(inline renditionInline) {
	if !w.charge(int64(len(inline.text)) + int64(len(inline.destination)) + 2*int64(unsafe.Sizeof(renditionInline{}))) {
		return
	}
	if len(w.links) > 0 {
		last := len(w.links) - 1
		w.links[last].children = appendRenditionInline(w.links[last].children, inline)
		return
	}
	if w.inCell {
		w.tableCell = appendRenditionInline(w.tableCell, inline)
		return
	}
	w.current = appendRenditionInline(w.current, inline)
}

func appendRenditionInline(target []renditionInline, inline renditionInline) []renditionInline {
	return append(target, inline)
}

func isActiveHTML(tag string) bool {
	switch tag {
	case "applet", "audio", "button", "embed", "form", "iframe", "input", "object", "select", "textarea", "video":
		return true
	default:
		return false
	}
}

func isHTMLVoidElement(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

func safeRenditionLink(value string, maxChars int) string {
	if hasStructuralLinkCharacter(value) {
		return ""
	}
	stored := safeStoredLink(value, maxChars)
	if stored == "" {
		return ""
	}
	if hasStructuralLinkCharacter(stored) {
		return ""
	}
	return "<" + encodeRenditionURL(stored) + ">"
}

func encodeRenditionURL(value string) string {
	var output strings.Builder
	for _, byteValue := range []byte(value) {
		if (byteValue >= 'a' && byteValue <= 'z') || (byteValue >= 'A' && byteValue <= 'Z') ||
			(byteValue >= '0' && byteValue <= '9') || strings.ContainsRune(":/?&=#%@;,+-.", rune(byteValue)) {
			output.WriteByte(byteValue)
			continue
		}
		_, _ = fmt.Fprintf(&output, "%%%02X", byteValue)
	}
	return output.String()
}

func hasStructuralLinkCharacter(value string) bool {
	for range 4 {
		if strings.ContainsAny(value, "<>[]\\`") {
			return true
		}
		decoded := html.UnescapeString(value)
		if decoded == value {
			return false
		}
		value = decoded
	}
	return strings.ContainsAny(value, "<>[]\\`")
}
