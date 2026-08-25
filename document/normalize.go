package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	goldmarkast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
	"golang.org/x/net/html"
)

const (
	headingSentinelStart = '\ue000'
	headingSentinelEnd   = '\ue001'
	headingMarkerClose   = "\ue000E\ue001"
)

// NormalizeDocument converts transient provider Markdown into deterministic,
// inert canonical text plus exact unit spans. It never retains raw responses.
func NormalizeDocument(source SourceDocument, policy NormalizePolicy) (NormalizedDocument, error) {
	if source.Family == "" || source.UnitKind == "" || len(source.Units) == 0 {
		return NormalizedDocument{}, errors.New("document normalization requires family, unit kind, and units")
	}
	if err := validateDocumentIdentifiers(source.Family, source.UnitKind); err != nil {
		return NormalizedDocument{}, err
	}
	if err := policy.validate(); err != nil {
		return NormalizedDocument{}, err
	}
	if err := validateSourceUnits(source.Units); err != nil {
		return NormalizedDocument{}, err
	}

	result := NormalizedDocument{
		PolicyVersion: normalizationPolicyVersion, Family: source.Family, UnitKind: source.UnitKind,
		Units: make([]NormalizedUnit, 0, len(source.Units)),
	}
	remaining := policy.maxDocumentChars
	for i, unit := range source.Units {
		text, headings, sourceTruncated, err := sanitizeMarkdown(
			unit.Markdown, policy.maxLinkChars, policy.maxSourceUnitBytes,
		)
		if err != nil {
			return NormalizedDocument{}, fmt.Errorf("normalize document source unit %d: %w", i, err)
		}
		header, _, headerSourceTruncated, err := sanitizeMarkdown(
			unit.Header, policy.maxLinkChars, policy.maxMetadataSourceBytes,
		)
		if err != nil {
			return NormalizedDocument{}, fmt.Errorf("normalize document source unit %d header: %w", i, err)
		}
		footer, _, footerSourceTruncated, err := sanitizeMarkdown(
			unit.Footer, policy.maxLinkChars, policy.maxMetadataSourceBytes,
		)
		if err != nil {
			return NormalizedDocument{}, fmt.Errorf("normalize document source unit %d footer: %w", i, err)
		}
		unitTruncated := sourceTruncated || headerSourceTruncated || footerSourceTruncated
		header, truncated := truncateRunes(header, min(policy.maxUnitChars, 16_384))
		unitTruncated = unitTruncated || truncated
		footer, truncated = truncateRunes(footer, min(policy.maxUnitChars, 16_384))
		unitTruncated = unitTruncated || truncated
		text, bodyOffset := joinDocumentUnitEvidence(header, text, footer)
		for headingIndex := range headings {
			headings[headingIndex].CharOffset += bodyOffset
			headings[headingIndex].EndOffset += bodyOffset
		}
		text, truncated = truncateRunes(text, min(policy.maxUnitChars, remaining))
		unitTruncated = unitTruncated || truncated
		if truncated {
			result.Truncated = true
		}
		combinedChars := utf8.RuneCountInString(text)
		if combinedChars == 0 && remaining == 0 && truncated {
			break
		}
		remaining -= combinedChars
		boundedHeadings := boundHeadingMarks(text, headings)
		normalized := NormalizedUnit{
			Index: unit.Index, SourceKey: fmt.Sprintf("%s:%06d", source.UnitKind, unit.Index), Kind: source.UnitKind,
			Text: text, Header: header, Footer: footer, Dimensions: unit.Dimensions,
			CharCount: utf8.RuneCountInString(text), Truncated: unitTruncated, HeadingMarks: boundedHeadings,
		}
		normalized.Checksum = checksumNormalizedUnit(normalized)
		result.Units = append(result.Units, normalized)
		result.Truncated = result.Truncated || unitTruncated
	}
	if len(result.Units) == 0 {
		return NormalizedDocument{}, errors.New("document normalization produced no units")
	}

	chunks, chunksTruncated := chunkNormalizedUnits(result.Units, policy)
	result.Chunks = chunks
	result.Truncated = result.Truncated || chunksTruncated
	result.Checksum = checksumNormalizedDocument(result)
	return result, nil
}

// ValidateNormalizedDocument verifies that a normalized document is a
// structurally complete, internally consistent version-3 normalization
// result. It detects stale identities after callers deserialize or copy the
// public evidence structs.
func ValidateNormalizedDocument(normalized NormalizedDocument) error {
	if normalized.PolicyVersion != normalizationPolicyVersion || normalized.Family == "" ||
		normalized.UnitKind == "" || len(normalized.Units) == 0 {
		return errors.New("normalized document identity is incomplete")
	}
	if err := validateDocumentIdentifiers(normalized.Family, normalized.UnitKind); err != nil {
		return err
	}
	anyTruncated := false
	unitRunes := make([][]rune, len(normalized.Units))
	for index, unit := range normalized.Units {
		if err := validateNormalizedUnit(normalized.UnitKind, index, unit); err != nil {
			return err
		}
		unitRunes[index] = []rune(unit.Text)
		anyTruncated = anyTruncated || unit.Truncated
	}
	for index, chunk := range normalized.Chunks {
		if err := validateNormalizedChunk(normalized, unitRunes, index, chunk); err != nil {
			return err
		}
		anyTruncated = anyTruncated || chunk.Truncated
	}
	if normalized.Checksum != checksumNormalizedDocument(normalized) {
		return errors.New("normalized document checksum is invalid")
	}
	if anyTruncated && !normalized.Truncated {
		return errors.New("normalized document truncation state is invalid")
	}
	return nil
}

func validateDocumentIdentifiers(family, unitKind string) error {
	identifiers := [...]struct{ name, value string }{
		{name: "family", value: family},
		{name: "unit kind", value: unitKind},
	}
	for _, identifier := range identifiers {
		if !utf8.ValidString(identifier.value) {
			return fmt.Errorf("document %s contains invalid UTF-8", identifier.name)
		}
		if strings.IndexFunc(identifier.value, unicode.IsControl) >= 0 {
			return fmt.Errorf("document %s contains a control character", identifier.name)
		}
	}
	return nil
}

func checksumNormalizedDocument(normalized NormalizedDocument) string {
	checksumParts := []string{
		fmt.Sprintf("v%d", normalized.PolicyVersion), normalized.Family, normalized.UnitKind,
		fmt.Sprintf("truncated:%t", normalized.Truncated),
	}
	for _, unit := range normalized.Units {
		checksumParts = append(checksumParts, unit.Checksum)
	}
	for _, chunk := range normalized.Chunks {
		checksumParts = append(checksumParts, chunk.Checksum)
	}
	return checksumStrings(checksumParts...)
}

func checksumNormalizedUnit(unit NormalizedUnit) string {
	checksumParts := []string{
		unit.SourceKey, unit.Text, unit.Header, unit.Footer,
		fmt.Sprintf("dimensions:%d:%d:%d", unit.Dimensions.DPI, unit.Dimensions.Height, unit.Dimensions.Width),
		fmt.Sprintf("truncated:%t", unit.Truncated),
		fmt.Sprintf("heading-marks:%d", len(unit.HeadingMarks)),
	}
	for _, mark := range unit.HeadingMarks {
		checksumParts = append(checksumParts,
			fmt.Sprintf("offset:%d", mark.CharOffset),
			fmt.Sprintf("path-parts:%d", len(mark.Path)),
		)
		checksumParts = append(checksumParts, mark.Path...)
	}
	return checksumStrings(checksumParts...)
}

func checksumNormalizedChunk(chunk Chunk) string {
	return checksumStrings(
		chunk.Key, chunk.Text, strings.Join(chunk.HeadingPath, "\x00"),
		fmt.Sprintf("truncated:%t", chunk.Truncated),
	)
}

func validateNormalizedUnit(unitKind string, index int, unit NormalizedUnit) error {
	expectedKey := fmt.Sprintf("%s:%06d", unitKind, index)
	if unit.Index != index || unit.SourceKey != expectedKey || unit.Kind != unitKind ||
		!utf8.ValidString(unit.Text) || !utf8.ValidString(unit.Header) || !utf8.ValidString(unit.Footer) ||
		unit.CharCount != utf8.RuneCountInString(unit.Text) {
		return fmt.Errorf("normalized document unit %d is invalid", index)
	}
	if unit.Dimensions.DPI < 0 || unit.Dimensions.Height < 0 || unit.Dimensions.Width < 0 ||
		unit.Dimensions.DPI > 100_000 || unit.Dimensions.Height > 10_000_000 || unit.Dimensions.Width > 10_000_000 {
		return fmt.Errorf("normalized document unit %d has invalid dimensions", index)
	}
	previousOffset := -1
	for _, mark := range unit.HeadingMarks {
		if mark.CharOffset <= previousOffset || mark.CharOffset < 0 || mark.CharOffset >= unit.CharCount {
			return fmt.Errorf("normalized document unit %d has invalid heading marks", index)
		}
		if slices.Contains(mark.Path, "") {
			return fmt.Errorf("normalized document unit %d has invalid heading marks", index)
		}
		previousOffset = mark.CharOffset
	}
	if unit.Checksum != checksumNormalizedUnit(unit) {
		return fmt.Errorf("normalized document unit %d checksum is invalid", index)
	}
	return nil
}

func validateNormalizedChunk(normalized NormalizedDocument, unitRunes [][]rune, index int, chunk Chunk) error {
	if chunk.Ordinal != index || chunk.Text == "" || !utf8.ValidString(chunk.Text) ||
		chunk.CharCount != utf8.RuneCountInString(chunk.Text) || len(chunk.Spans) != 1 {
		return fmt.Errorf("normalized document chunk %d is invalid", index)
	}
	span := chunk.Spans[0]
	if span.UnitIndex < 0 || span.UnitIndex >= len(normalized.Units) || span.CharStart < 0 || span.CharEnd <= span.CharStart {
		return fmt.Errorf("normalized document chunk %d has an invalid source span", index)
	}
	unit := normalized.Units[span.UnitIndex]
	unitText := unitRunes[span.UnitIndex]
	if span.CharEnd > len(unitText) || string(unitText[span.CharStart:span.CharEnd]) != chunk.Text {
		return fmt.Errorf("normalized document chunk %d does not match its source span", index)
	}
	expectedKey := fmt.Sprintf("%s:%06d-%06d", unit.SourceKey, span.CharStart, span.CharEnd)
	expectedHeadingPath := headingPathAt(unit.HeadingMarks, span.CharStart)
	if chunk.Key != expectedKey || !slices.Equal(chunk.HeadingPath, expectedHeadingPath) || chunk.Truncated != unit.Truncated {
		return fmt.Errorf("normalized document chunk %d identity is invalid", index)
	}
	expectedChecksum := checksumNormalizedChunk(chunk)
	if chunk.Checksum != expectedChecksum {
		return fmt.Errorf("normalized document chunk %d checksum is invalid", index)
	}
	return nil
}

func validateSourceUnits(units []SourceUnit) error {
	for i, unit := range units {
		if unit.Index != i {
			return fmt.Errorf("document source unit %d has noncontiguous index %d", i, unit.Index)
		}
		if unit.Dimensions.DPI < 0 || unit.Dimensions.Height < 0 || unit.Dimensions.Width < 0 ||
			unit.Dimensions.DPI > 100_000 || unit.Dimensions.Height > 10_000_000 || unit.Dimensions.Width > 10_000_000 {
			return fmt.Errorf("document source unit %d has invalid dimensions", i)
		}
		if !utf8.ValidString(unit.Markdown) {
			return fmt.Errorf("normalize document source unit %d: provider Markdown is invalid UTF-8", i)
		}
		if !utf8.ValidString(unit.Header) {
			return fmt.Errorf("normalize document source unit %d header: provider Markdown is invalid UTF-8", i)
		}
		if !utf8.ValidString(unit.Footer) {
			return fmt.Errorf("normalize document source unit %d footer: provider Markdown is invalid UTF-8", i)
		}
	}
	return nil
}

func joinDocumentUnitEvidence(header, body, footer string) (string, int) {
	parts := make([]string, 0, 3)
	bodyOffset := 0
	if header != "" {
		parts = append(parts, header)
		bodyOffset = utf8.RuneCountInString(header)
		if body != "" || footer != "" {
			bodyOffset += 2
		}
	}
	if body != "" {
		parts = append(parts, body)
	}
	if footer != "" {
		parts = append(parts, footer)
	}
	return strings.Join(parts, "\n\n"), bodyOffset
}

// sanitizeMarkdown converts untrusted provider Markdown into inert canonical
// Markdown-like text. It preserves NormalizeDocument's frozen behavior.
func sanitizeMarkdown(markdown string, maxLinkChars, maxSourceBytes int) (string, []canonicalHeadingMark, bool, error) {
	return sanitizeMarkdownWithActiveHTML(markdown, maxLinkChars, maxSourceBytes, false)
}

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

func sanitizeMarkdownWithActiveHTML(
	markdown string,
	maxLinkChars, maxSourceBytes int,
	dropActiveHTML bool,
) (string, []canonicalHeadingMark, bool, error) {
	if markdown == "" {
		return "", nil, false, nil
	}
	if !utf8.ValidString(markdown) {
		return "", nil, false, errors.New("provider Markdown is invalid UTF-8")
	}
	markdown, sourceTruncated := truncateUTF8Bytes(markdown, maxSourceBytes)
	parser := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
	)
	var rendered bytes.Buffer
	if err := parser.Convert([]byte(markdown), &rendered); err != nil {
		return "", nil, false, fmt.Errorf("parse provider Markdown: %w", err)
	}
	writer := canonicalHTMLWriter{
		maxLinkChars: maxLinkChars, dropActiveHTML: dropActiveHTML, escapeMarkdown: dropActiveHTML,
	}
	if err := writer.consume(bytes.NewReader(rendered.Bytes())); err != nil {
		return "", nil, false, err
	}
	text, headings := canonicalWhitespace(writer.output.String())
	return text, headings, sourceTruncated, nil
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
			w.endTag(tokenizer.Token().Data)
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
			w.table = append(w.table, append([][]renditionInline(nil), w.tableRow...))
			w.tableRow = nil
			w.inRow = false
		}
	case "td", "th":
		if w.inTable && w.inCell {
			w.flushPendingSpace()
			w.tableRow = append(w.tableRow, append([]renditionInline(nil), w.tableCell...))
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
				w.appendInline(renditionInline{kind: renditionInlineCode, text: strings.Join(strings.Fields(content), " ")})
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
		w.preText.WriteString(value)
		return
	}
	if w.inlineCode {
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

type renditionBuilder struct {
	strings.Builder

	runes int
}

func (b *renditionBuilder) WriteString(value string) {
	b.Builder.WriteString(value)
	b.runes += utf8.RuneCountInString(value)
}

func serializeRenditionBlocks(blocks []renditionBlock, limit int) (string, bool) {
	var output renditionBuilder
	truncated := false
	previousList := false
	previousListOrdered := false
	previousListAlternate := false
	previousKind := renditionParagraph
	for _, block := range blocks {
		separator := ""
		listAlternate := false
		if output.Len() > 0 {
			separator = "\n"
			if previousKind == renditionParagraph && block.kind == renditionParagraph ||
				previousKind == renditionTable && block.kind == renditionTable ||
				previousKind == renditionListBlock && block.kind == renditionParagraph ||
				previousKind == renditionParagraph && block.kind == renditionListBlock && block.list != nil &&
					block.list.ordered && block.list.start != "1" {
				separator = "\n\n"
			}
			if previousList && block.kind == renditionListBlock && block.list != nil &&
				block.list.ordered == previousListOrdered {
				listAlternate = !previousListAlternate
			}
		}
		available := limit - output.runes - utf8.RuneCountInString(separator)
		if available < 0 {
			return finishRenditionMarkdown(output.String()), true
		}
		value, blockTruncated := serializeRenditionBlock(block, available, listAlternate)
		if value == "" && blockTruncated {
			return finishRenditionMarkdown(output.String()), true
		}
		if value != "" {
			output.WriteString(separator)
			output.WriteString(value)
			previousKind = block.kind
			previousList = block.kind == renditionListBlock && block.list != nil
			if previousList {
				previousListOrdered = block.list.ordered
				previousListAlternate = listAlternate
			}
		}
		if blockTruncated {
			truncated = true
			break
		}
	}
	return finishRenditionMarkdown(output.String()), truncated
}

func finishRenditionMarkdown(value string) string {
	return strings.TrimRight(value, "\n")
}

func canonicalNonnegativeDecimal(value string) (string, bool) {
	value = strings.TrimPrefix(value, "+")
	if value == "" {
		return "", false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return "", false
		}
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0", true
	}
	return value, true
}

func incrementNonnegativeDecimal(value string) string {
	digits := []byte(value)
	for index := len(digits) - 1; index >= 0; index-- {
		if digits[index] < '9' {
			digits[index]++
			return string(digits)
		}
		digits[index] = '0'
	}
	return "1" + string(digits)
}

func serializeRenditionBlock(block renditionBlock, available int, listAlternate bool) (string, bool) {
	switch block.kind {
	case renditionCodeBlock:
		value := serializeRenditionCodeBlock(block.language, block.code)
		if utf8.RuneCountInString(value) > available {
			return "", true
		}
		return value, false
	case renditionTable:
		value := serializeRenditionTable(block.rows)
		if utf8.RuneCountInString(value) > available {
			return "", true
		}
		return value, false
	case renditionListBlock:
		if block.list == nil {
			return "", false
		}
		return serializeRenditionList(*block.list, available, listAlternate)
	case renditionParagraph, renditionHeading:
		prefix := ""
		switch block.kind {
		case renditionParagraph:
			// Paragraphs have no generated block marker.
		case renditionHeading:
			prefix = strings.Repeat("#", block.level) + " "
		case renditionCodeBlock, renditionTable, renditionListBlock:
			return "", false
		}
		if utf8.RuneCountInString(prefix) > available {
			return "", true
		}
		value, truncated := serializeRenditionInlines(block.inlines, available-utf8.RuneCountInString(prefix), false)
		if value == "" && truncated {
			return "", true
		}
		return prefix + value, truncated
	default:
		return "", false
	}
}

func serializeRenditionList(list renditionList, available int, alternate bool) (string, bool) {
	var output renditionBuffer
	result := appendRenditionList(&output, list, available, 0, alternate)
	return output.String(), result.truncated
}

type renditionListResult struct {
	truncated        bool
	emitted          int
	contentIndent    int
	looseFallback    renditionBufferCheckpoint
	hasLooseFallback bool
}

type renditionSerializedItemRange struct {
	ordinal string
	start   int
	end     int
}

func appendRenditionList(
	output *renditionBuffer,
	list renditionList,
	limit int,
	indent int,
	alternate bool,
) renditionListResult {
	start := output.checkpoint()
	var result renditionListResult
	if list.ordered && len(list.start) <= maxOrderedListMarkerDigits {
		result = appendRepresentableOrderedListItems(output, list, limit, indent, alternate)
	} else {
		result = appendRenditionListItems(output, list, limit, indent, alternate, list.ordered)
	}
	if list.tight || result.emitted != 1 {
		return result
	}
	looseBoundary := renditionLooseBoundary(result.contentIndent)
	if output.runes+utf8.RuneCountInString(looseBoundary) > limit {
		if !result.hasLooseFallback {
			output.rollback(start.bytes, start.runes)
			return renditionListResult{truncated: true}
		}
		output.rollback(result.looseFallback.bytes, result.looseFallback.runes)
		result.truncated = true
	}
	output.WriteString(looseBoundary)
	return result
}

func renditionLooseBoundary(contentIndent int) string {
	return "\n\n" + strings.Repeat(" ", contentIndent) + "<!-- -->"
}

func appendRepresentableOrderedListItems(
	output *renditionBuffer,
	list renditionList,
	limit int,
	indent int,
	alternate bool,
) renditionListResult {
	listStart := output.checkpoint()
	entries := make([]renditionSerializedItemRange, 0, len(list.items))
	result := renditionListResult{}
	ordinal := list.start
	degraded := false
	var fallback []byte
	fallbackRunes := 0
	for _, item := range list.items {
		if !item.present {
			continue
		}
		if !degraded && len(ordinal) > maxOrderedListMarkerDigits {
			fallback = append([]byte(nil), output.bytes[listStart.bytes:]...)
			fallbackRunes = output.runes - listStart.runes
			var converted renditionBuffer
			for index, entry := range entries {
				if index > 0 {
					converted.WriteString(renditionListItemSeparator(list.tight))
				}
				converted.WriteString(degradeOrderedListItem(serializedOrderedListItem{
					ordinal: entry.ordinal,
					value:   string(output.bytes[entry.start:entry.end]),
				}, indent, alternate))
			}
			output.rollback(listStart.bytes, listStart.runes)
			output.WriteString(converted.String())
			if output.runes > limit {
				output.rollback(listStart.bytes, listStart.runes)
				output.WriteBytes(fallback, fallbackRunes)
				result.truncated = true
				return result
			}
			degraded = true
		}

		itemStart := output.checkpoint()
		if result.emitted > 0 {
			output.WriteString(renditionListItemSeparator(list.tight))
		}
		marker := ordinal + "."
		if alternate {
			marker = ordinal + ")"
		}
		labelPrefix := ""
		if degraded {
			marker = "-"
			if alternate {
				marker = "*"
			}
			labelPrefix = ordinal + "\\. "
		}
		itemFallback := newRenditionLooseItemFallback(list, limit, indent, marker, result.emitted, output.runes)
		valueStart := len(output.bytes)
		wrote, truncated := appendRenditionListItem(output, item, limit, indent, marker, labelPrefix, itemFallback)
		if !wrote {
			output.rollback(itemStart.bytes, itemStart.runes)
			if fallback != nil {
				output.rollback(listStart.bytes, listStart.runes)
				output.WriteBytes(fallback, fallbackRunes)
			}
			result.truncated = true
			return result
		}
		if !degraded {
			entries = append(entries, renditionSerializedItemRange{ordinal: ordinal, start: valueStart, end: len(output.bytes)})
		}
		fallback = nil
		result.emitted++
		if result.emitted == 1 {
			result.contentIndent = indent + utf8.RuneCountInString(marker) + 1
			result.looseFallback, result.hasLooseFallback = itemFallback.result()
		}
		if truncated {
			result.truncated = true
			return result
		}
		ordinal = incrementNonnegativeDecimal(ordinal)
	}
	return result
}

func appendRenditionListItems(
	output *renditionBuffer,
	list renditionList,
	limit int,
	indent int,
	alternate bool,
	degraded bool,
) renditionListResult {
	result := renditionListResult{}
	ordinal := list.start
	for _, item := range list.items {
		if !item.present {
			continue
		}
		itemStart := output.checkpoint()
		if result.emitted > 0 {
			output.WriteString(renditionListItemSeparator(list.tight))
		}
		marker := "-"
		if alternate {
			marker = "*"
		}
		labelPrefix := ""
		if list.ordered && !degraded {
			marker = ordinal + "."
			if alternate {
				marker = ordinal + ")"
			}
		} else if list.ordered {
			labelPrefix = ordinal + "\\. "
		}
		itemFallback := newRenditionLooseItemFallback(list, limit, indent, marker, result.emitted, output.runes)
		wrote, truncated := appendRenditionListItem(output, item, limit, indent, marker, labelPrefix, itemFallback)
		if !wrote {
			output.rollback(itemStart.bytes, itemStart.runes)
			result.truncated = true
			return result
		}
		result.emitted++
		if result.emitted == 1 {
			result.contentIndent = indent + utf8.RuneCountInString(marker) + 1
			result.looseFallback, result.hasLooseFallback = itemFallback.result()
		}
		if truncated {
			result.truncated = true
			return result
		}
		if list.ordered {
			ordinal = incrementNonnegativeDecimal(ordinal)
		}
	}
	return result
}

func newRenditionLooseItemFallback(
	list renditionList,
	limit int,
	indent int,
	marker string,
	emitted int,
	itemStartRunes int,
) *renditionBufferFallback {
	if list.tight || emitted != 0 {
		return nil
	}
	contentIndent := indent + utf8.RuneCountInString(marker) + 1
	return &renditionBufferFallback{
		limit:    limit - utf8.RuneCountInString(renditionLooseBoundary(contentIndent)),
		minRunes: itemStartRunes,
	}
}

func appendRenditionListItem(
	output *renditionBuffer,
	item renditionListItem,
	limit int,
	indent int,
	marker string,
	labelPrefix string,
	fallback *renditionBufferFallback,
) (bool, bool) {
	start := output.checkpoint()
	markerLine := strings.Repeat(" ", indent) + marker
	markerAnchor := markerLine
	if labelPrefix != "" {
		markerAnchor += " " + strings.TrimSuffix(labelPrefix, " ")
	}
	contentIndent := indent + utf8.RuneCountInString(marker) + 1
	if len(item.blocks) == 0 {
		if output.runes+utf8.RuneCountInString(markerAnchor) > limit {
			return false, true
		}
		output.WriteString(markerAnchor)
		fallback.mark(output)
		return true, false
	}
	previousList := false
	previousListOrdered := false
	previousListAlternate := false
	previousListIndent := contentIndent
	for _, block := range item.blocks {
		separator := ""
		listAlternate := false
		listIndent := contentIndent
		if output.runes > start.runes {
			if block.kind == renditionListBlock {
				separator = "\n"
				if previousList {
					listIndent = previousListIndent
				}
				if block.list != nil && previousList && block.list.ordered == previousListOrdered {
					listAlternate = !previousListAlternate
				} else if block.list != nil && !previousList && block.list.ordered && block.list.start != "1" {
					separator = "\n\n"
				}
			} else {
				separator = "\n\n"
			}
		}

		if block.kind == renditionListBlock {
			if block.list == nil {
				continue
			}
			if output.runes == start.runes {
				if block.list.ordered && block.list.start != "1" {
					listIndent += 2
				}
				if output.runes+utf8.RuneCountInString(markerAnchor) > limit {
					return false, true
				}
				output.WriteString(markerAnchor)
				if output.runes+1 > limit {
					return true, true
				}
				separator = "\n"
			}
			separatorStart := output.checkpoint()
			if output.runes+utf8.RuneCountInString(separator) > limit {
				return output.runes > start.runes, true
			}
			output.WriteString(separator)
			nestedStartRunes := output.runes
			result := appendRenditionList(output, *block.list, limit, listIndent, listAlternate)
			if output.runes == nestedStartRunes {
				output.rollback(separatorStart.bytes, separatorStart.runes)
				return output.runes > start.runes, result.truncated
			}
			previousList = true
			previousListOrdered = block.list.ordered
			previousListAlternate = listAlternate
			previousListIndent = listIndent
			fallback.mark(output)
			if result.truncated {
				return true, true
			}
			continue
		}

		separatorStart := output.checkpoint()
		if output.runes+utf8.RuneCountInString(separator) > limit {
			return output.runes > start.runes, true
		}
		output.WriteString(separator)
		firstPrefix := strings.Repeat(" ", contentIndent)
		if output.runes == start.runes {
			firstPrefix = markerLine + " " + labelPrefix
		}
		wrote, truncated := appendRenditionItemBlock(
			output, block, limit, firstPrefix, strings.Repeat(" ", contentIndent), fallback,
		)
		if !wrote {
			output.rollback(separatorStart.bytes, separatorStart.runes)
		}
		if !wrote && truncated {
			return output.runes > start.runes, true
		}
		if wrote {
			previousList = false
		}
		if truncated {
			return output.runes > start.runes, true
		}
	}
	return output.runes > start.runes, false
}

const maxOrderedListMarkerDigits = 9

type serializedOrderedListItem struct {
	ordinal string
	value   string
}

func renditionListItemSeparator(tight bool) string {
	if tight {
		return "\n"
	}
	return "\n\n"
}

func degradeOrderedListItem(item serializedOrderedListItem, indent int, alternate bool) string {
	normalMarker := item.ordinal + "."
	degradedMarker := "-"
	if alternate {
		normalMarker = item.ordinal + ")"
		degradedMarker = "*"
	}
	normalPrefix := strings.Repeat(" ", indent) + normalMarker
	degradedPrefix := strings.Repeat(" ", indent) + degradedMarker + " " + item.ordinal + "\\."
	lines := strings.Split(item.value, "\n")
	lines[0] = degradedPrefix + strings.TrimPrefix(lines[0], normalPrefix)
	normalIndent := strings.Repeat(" ", indent+utf8.RuneCountInString(normalMarker)+1)
	degradedIndent := strings.Repeat(" ", indent+2)
	for index := 1; index < len(lines); index++ {
		if after, ok := strings.CutPrefix(lines[index], normalIndent); ok {
			lines[index] = degradedIndent + after
		}
	}
	return strings.Join(lines, "\n")
}

func appendRenditionItemBlock(
	output *renditionBuffer,
	block renditionBlock,
	limit int,
	firstPrefix string,
	continuationPrefix string,
	fallback *renditionBufferFallback,
) (bool, bool) {
	start := output.checkpoint()
	if block.kind == renditionParagraph || block.kind == renditionHeading {
		if block.kind == renditionHeading {
			firstPrefix += strings.Repeat("#", block.level) + " "
		}
		prefixRunes := utf8.RuneCountInString(firstPrefix)
		if output.runes+prefixRunes > limit {
			return false, true
		}
		output.WriteString(firstPrefix)
		inlineStart := output.checkpoint()
		truncated := appendRenditionInlinesWithFallback(
			output, block.inlines, limit-output.runes, false, fallback,
		)
		if output.runes == inlineStart.runes && truncated {
			output.rollback(start.bytes, start.runes)
			return false, true
		}
		fallback.mark(output)
		return true, truncated
	}

	var value string
	switch block.kind {
	case renditionCodeBlock:
		value = serializeRenditionCodeBlock(block.language, block.code)
	case renditionTable:
		value = serializeRenditionTable(block.rows)
	default:
		return false, false
	}
	value = prefixRenditionLines(value, firstPrefix, continuationPrefix)
	if output.runes+utf8.RuneCountInString(value) > limit {
		return false, true
	}
	output.WriteString(value)
	fallback.mark(output)
	return true, false
}

func prefixRenditionLines(value, firstPrefix, continuationPrefix string) string {
	return firstPrefix + strings.ReplaceAll(value, "\n", "\n"+continuationPrefix)
}

func serializeRenditionInlines(inlines []renditionInline, available int, inTable bool) (string, bool) {
	var output renditionBuffer
	truncated := appendRenditionInlines(&output, inlines, available, inTable)
	return output.String(), truncated
}

type renditionBuffer struct {
	bytes []byte
	runes int
}

type renditionBufferCheckpoint struct {
	bytes int
	runes int
}

type renditionBufferFallback struct {
	limit      int
	minRunes   int
	checkpoint renditionBufferCheckpoint
	valid      bool
}

func (f *renditionBufferFallback) mark(output *renditionBuffer) {
	if f == nil || output.runes <= f.minRunes || output.runes > f.limit {
		return
	}
	f.checkpoint = output.checkpoint()
	f.valid = true
}

func (f *renditionBufferFallback) markEscapedText(
	start renditionBufferCheckpoint,
	value string,
) {
	if f == nil || start.runes >= f.limit {
		return
	}
	prefix := truncateEscapedRenditionText(value, f.limit-start.runes)
	if prefix == "" {
		return
	}
	f.checkpoint = renditionBufferCheckpoint{
		bytes: start.bytes + len(prefix),
		runes: start.runes + utf8.RuneCountInString(prefix),
	}
	f.valid = true
}

func (f *renditionBufferFallback) result() (renditionBufferCheckpoint, bool) {
	if f == nil {
		return renditionBufferCheckpoint{}, false
	}
	return f.checkpoint, f.valid
}

func (b *renditionBuffer) WriteString(value string) {
	b.bytes = append(b.bytes, value...)
	b.runes += utf8.RuneCountInString(value)
}

func (b *renditionBuffer) WriteBytes(value []byte, runes int) {
	b.bytes = append(b.bytes, value...)
	b.runes += runes
}

func (b *renditionBuffer) String() string {
	return string(b.bytes)
}

func (b *renditionBuffer) rollback(bytes, runes int) {
	b.bytes = b.bytes[:bytes]
	b.runes = runes
}

func (b *renditionBuffer) checkpoint() renditionBufferCheckpoint {
	return renditionBufferCheckpoint{bytes: len(b.bytes), runes: b.runes}
}

func appendRenditionInlines(
	output *renditionBuffer,
	inlines []renditionInline,
	available int,
	inTable bool,
) bool {
	return appendRenditionInlinesWithFallback(output, inlines, available, inTable, nil)
}

func appendRenditionInlinesWithFallback(
	output *renditionBuffer,
	inlines []renditionInline,
	available int,
	inTable bool,
	fallback *renditionBufferFallback,
) bool {
	startRunes := output.runes
	for _, inline := range inlines {
		if inline.kind == renditionLinkInline && inline.destination == "" {
			remaining := available
			if available >= 0 {
				remaining -= output.runes - startRunes
			}
			if appendRenditionInlinesWithFallback(output, inline.children, remaining, inTable, fallback) {
				return true
			}
			continue
		}
		remaining := available
		if available >= 0 {
			remaining -= output.runes - startRunes
		}
		switch inline.kind {
		case renditionText:
			textStart := output.checkpoint()
			value := escapeRenditionText(inline.text)
			if available >= 0 && utf8.RuneCountInString(value) > remaining {
				output.WriteString(truncateEscapedRenditionText(inline.text, max(0, remaining)))
				fallback.markEscapedText(textStart, inline.text)
				return true
			}
			output.WriteString(value)
			fallback.markEscapedText(textStart, inline.text)
		case renditionInlineCode:
			value := serializeRenditionInlineCode(inline.text, inTable)
			if available >= 0 && utf8.RuneCountInString(value) > remaining {
				return true
			}
			output.WriteString(value)
			fallback.mark(output)
		case renditionLinkInline:
			overhead := utf8.RuneCountInString(inline.destination) + 4
			if available >= 0 && overhead > remaining {
				appendRenditionPlainLabel(output, inline.children, remaining, fallback)
				return true
			}
			markBytes, markRunes := len(output.bytes), output.runes
			output.WriteString("[")
			labelStart := output.runes
			labelBudget := -1
			if available >= 0 {
				labelBudget = remaining - overhead
			}
			if appendRenditionInlinesWithFallback(output, inline.children, labelBudget, inTable, nil) {
				output.rollback(markBytes, markRunes)
				appendRenditionPlainLabel(output, inline.children, remaining, fallback)
				return true
			}
			if output.runes == labelStart {
				output.rollback(markBytes, markRunes)
				continue
			}
			output.WriteString("](")
			output.WriteString(inline.destination)
			output.WriteString(")")
			fallback.mark(output)
		}
	}
	return false
}

func appendRenditionPlainLabel(
	output *renditionBuffer,
	inlines []renditionInline,
	available int,
	fallback *renditionBufferFallback,
) bool {
	startRunes := output.runes
	for _, inline := range inlines {
		remaining := available
		if available >= 0 {
			remaining -= output.runes - startRunes
		}
		switch inline.kind {
		case renditionText, renditionInlineCode:
			textStart := output.checkpoint()
			value := escapeRenditionText(inline.text)
			if available >= 0 && utf8.RuneCountInString(value) > remaining {
				output.WriteString(truncateEscapedRenditionText(inline.text, max(0, remaining)))
				fallback.markEscapedText(textStart, inline.text)
				return true
			}
			output.WriteString(value)
			fallback.markEscapedText(textStart, inline.text)
		case renditionLinkInline:
			if appendRenditionPlainLabel(output, inline.children, remaining, fallback) {
				return true
			}
		}
	}
	return false
}

func escapeRenditionText(value string) string {
	var output strings.Builder
	for _, character := range value {
		if isMarkdownASCIIPunctuation(character) {
			output.WriteByte('\\')
		}
		output.WriteRune(character)
	}
	return output.String()
}

func truncateEscapedRenditionText(value string, limit int) string {
	var output strings.Builder
	used := 0
	for _, character := range value {
		cost := 1
		if isMarkdownASCIIPunctuation(character) {
			cost++
		}
		if used+cost > limit {
			break
		}
		if cost == 2 {
			output.WriteByte('\\')
		}
		output.WriteRune(character)
		used += cost
	}
	return output.String()
}

func isMarkdownASCIIPunctuation(character rune) bool {
	return character >= '!' && character <= '/' || character >= ':' && character <= '@' ||
		character >= '[' && character <= '`' || character >= '{' && character <= '~'
}

func serializeRenditionInlineCode(content string, inTable bool) string {
	content = strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", " "), "\n", " ")
	if inTable {
		content = strings.ReplaceAll(content, "|", "\\|")
	}
	fence := strings.Repeat("`", maxBacktickRun(content)+1)
	if strings.HasPrefix(content, "`") || strings.HasSuffix(content, "`") {
		content = " " + content + " "
	}
	return fence + content + fence
}

func serializeRenditionCodeBlock(language, content string) string {
	fence := strings.Repeat("`", max(3, maxBacktickRun(content)+1))
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return fence + language + "\n" + content + fence
}

func serializeRenditionTable(rows [][][]renditionInline) string {
	if len(rows) == 0 {
		return ""
	}
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	if columns == 0 {
		return ""
	}
	format := func(row [][]renditionInline) string {
		cells := make([]string, columns)
		for index, cell := range row {
			cells[index], _ = serializeRenditionInlines(cell, -1, true)
		}
		return "| " + strings.Join(cells, " | ") + " |"
	}
	delimiter := make([]string, columns)
	for index := range delimiter {
		delimiter[index] = "---"
	}
	lines := []string{format(rows[0]), "| " + strings.Join(delimiter, " | ") + " |"}
	for _, row := range rows[1:] {
		lines = append(lines, format(row))
	}
	return strings.Join(lines, "\n")
}

type canonicalHTMLWriter struct {
	output         strings.Builder
	maxLinkChars   int
	inPre          bool
	skipTag        string
	skipDepth      int
	cellIndex      int
	links          []string
	renditionLinks []*renditionLink
	preFenceOpen   bool
	pendingSpace   bool
	dropActiveHTML bool
	escapeMarkdown bool
	preLanguage    string
	preContent     strings.Builder
	inlineCode     bool
	inlineContent  strings.Builder
	inTable        bool
	inTableCell    bool
	tableRows      [][]string
	tableRow       []string
	tableCell      strings.Builder
}

type renditionLink struct {
	destination string
	text        strings.Builder
}

func (w *canonicalHTMLWriter) consume(reader io.Reader) error {
	tokenizer := html.NewTokenizer(reader)
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if errors.Is(tokenizer.Err(), io.EOF) {
				return nil
			}
			return fmt.Errorf("tokenize normalized document HTML: %w", tokenizer.Err())
		case html.TextToken:
			if w.skipDepth == 0 {
				w.writeText(string(tokenizer.Text()))
			}
		case html.StartTagToken:
			w.startTag(tokenizer.Token(), false)
		case html.SelfClosingTagToken:
			w.startTag(tokenizer.Token(), true)
		case html.EndTagToken:
			w.endTag(tokenizer.Token().Data)
		case html.CommentToken, html.DoctypeToken:
			// Comments and document declarations are not searchable evidence.
		}
	}
}

func (w *canonicalHTMLWriter) startTag(token html.Token, selfClosing bool) {
	tag := token.Data
	if w.skipDepth > 0 {
		if tag == w.skipTag && !selfClosing {
			w.skipDepth++
		}
		return
	}
	if tag == "script" || tag == "style" {
		w.skipTag = tag
		w.skipDepth = 1
		return
	}
	if w.dropActiveHTML && isActiveHTML(tag) {
		if !selfClosing && !isHTMLVoidElement(tag) {
			w.skipTag = tag
			w.skipDepth = 1
		}
		return
	}
	if tag == "svg" {
		if !selfClosing {
			w.skipTag = tag
			w.skipDepth = 1
		}
		return
	}
	switch tag {
	case "table":
		if w.escapeMarkdown {
			w.block()
			w.inTable = true
			w.tableRows = nil
			return
		}
		w.block()
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.block()
		level := int(tag[1] - '0')
		_, _ = fmt.Fprintf(&w.output, "%cH%d%c", headingSentinelStart, level, headingSentinelEnd)
		w.output.WriteString(strings.Repeat("#", level) + " ")
	case "li":
		w.line()
		w.output.WriteString("- ")
	case "br":
		w.line()
	case "tr":
		if w.escapeMarkdown && w.inTable {
			w.tableRow = nil
			return
		}
		w.line()
		w.cellIndex = 0
	case "td", "th":
		if w.escapeMarkdown && w.inTable {
			w.inTableCell = true
			w.tableCell.Reset()
			return
		}
		if w.cellIndex > 0 {
			w.output.WriteString(" | ")
		}
		w.cellIndex++
	case "pre":
		w.block()
		if w.escapeMarkdown {
			w.inPre = true
			w.preFenceOpen = true
			w.preLanguage = ""
			w.preContent.Reset()
			return
		}
		w.output.WriteString("```")
		w.inPre = true
		w.preFenceOpen = true
	case "code":
		if w.inPre && w.preFenceOpen {
			for _, attribute := range token.Attr {
				if attribute.Key == "class" && strings.HasPrefix(attribute.Val, "language-") {
					if language := safeCodeLanguage(strings.TrimPrefix(attribute.Val, "language-")); language != "" {
						if w.escapeMarkdown {
							w.preLanguage = language
						} else {
							w.output.WriteString(language)
						}
					}
				}
			}
			if !w.escapeMarkdown {
				w.output.WriteByte('\n')
			}
			w.preFenceOpen = false
		} else if !w.inPre {
			w.flushPendingSpace()
			if w.escapeMarkdown {
				w.inlineCode = true
				w.inlineContent.Reset()
			} else {
				w.output.WriteByte('`')
			}
		}
	case "img":
		for _, attribute := range token.Attr {
			if attribute.Key == "alt" {
				w.writeText(attribute.Val)
				break
			}
		}
	case "input":
		isCheckbox := false
		checked := false
		for _, attribute := range token.Attr {
			if attribute.Key == "type" && attribute.Val == "checkbox" {
				isCheckbox = true
			}
			if attribute.Key == "checked" {
				checked = true
			}
		}
		if isCheckbox {
			w.flushPendingSpace()
			if checked {
				w.output.WriteString("[x] ")
			} else {
				w.output.WriteString("[ ] ")
			}
		}
	case "a":
		if w.escapeMarkdown {
			w.flushPendingSpace()
			link := ""
			for _, attribute := range token.Attr {
				if attribute.Key == "href" {
					link = safeRenditionLink(attribute.Val, w.maxLinkChars)
					break
				}
			}
			w.renditionLinks = append(w.renditionLinks, &renditionLink{destination: link})
			return
		}
		link := ""
		for _, attribute := range token.Attr {
			if attribute.Key == "href" {
				if w.escapeMarkdown {
					link = safeRenditionLink(attribute.Val, w.maxLinkChars)
				} else {
					link = safeStoredLink(attribute.Val, w.maxLinkChars)
				}
				break
			}
		}
		w.links = append(w.links, link)
	default:
		if isHTMLBlockElement(tag) {
			w.block()
		}
	}
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

func (w *canonicalHTMLWriter) endTag(tag string) {
	if w.skipDepth > 0 {
		if tag == w.skipTag {
			w.skipDepth--
			if w.skipDepth == 0 {
				w.skipTag = ""
			}
		}
		return
	}
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.flushPendingSpace()
		w.output.WriteString(headingMarkerClose)
		w.block()
	case "li":
		w.line()
	case "tr":
		if w.escapeMarkdown && w.inTable {
			w.tableRows = append(w.tableRows, append([]string(nil), w.tableRow...))
			return
		}
		w.line()
	case "td", "th":
		if w.escapeMarkdown && w.inTable && w.inTableCell {
			w.tableRow = append(w.tableRow, w.tableCell.String())
			w.tableCell.Reset()
			w.inTableCell = false
			return
		}
	case "table":
		if w.escapeMarkdown && w.inTable {
			w.inTable = false
			w.writeRenditionRaw(serializeGFMTable(w.tableRows))
			w.tableRows = nil
			w.block()
			return
		}
		w.block()
	case "pre":
		if w.escapeMarkdown {
			w.writeSafeCodeFence(w.preLanguage, w.preContent.String())
			w.inPre = false
			w.preFenceOpen = false
			w.preLanguage = ""
			w.preContent.Reset()
			w.block()
			return
		}
		if w.preFenceOpen {
			w.output.WriteByte('\n')
		}
		w.inPre = false
		w.preFenceOpen = false
		w.line()
		w.output.WriteString("```")
		w.block()
	case "code":
		if !w.inPre {
			w.flushPendingSpace()
			if w.escapeMarkdown {
				w.writeSafeInlineCode(w.inlineContent.String())
				w.inlineCode = false
				w.inlineContent.Reset()
			} else {
				w.output.WriteByte('`')
			}
		}
	case "a":
		if w.escapeMarkdown {
			if len(w.renditionLinks) == 0 {
				return
			}
			link := w.renditionLinks[len(w.renditionLinks)-1]
			w.renditionLinks = w.renditionLinks[:len(w.renditionLinks)-1]
			if link.destination == "" {
				w.writeRenditionRaw(link.text.String())
			} else {
				w.writeRenditionRaw("[" + link.text.String() + "](" + link.destination + ")")
			}
			return
		}
		w.flushPendingSpace()
		if len(w.links) == 0 {
			return
		}
		link := w.links[len(w.links)-1]
		w.links = w.links[:len(w.links)-1]
		if link != "" {
			if w.output.Len() > 0 &&
				!strings.HasSuffix(w.output.String(), "\n") && !strings.HasSuffix(w.output.String(), " ") {
				w.output.WriteByte(' ')
			}
			w.output.WriteString("(" + link + ")")
		}
	default:
		if isHTMLBlockElement(tag) {
			w.block()
		}
	}
}

func safeCodeLanguage(language string) string {
	if language == "" || len(language) > 64 {
		return ""
	}
	for _, character := range language {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("+#-_.", character) {
			continue
		}
		return ""
	}
	return language
}

func isHTMLBlockElement(tag string) bool {
	switch tag {
	case "address", "article", "aside", "blockquote", "body", "caption", "center", "colgroup", "dd", "details", "dialog", "dir", "div", "dl", "dt", "fieldset", "figcaption", "figure", "footer", "form", "header", "hgroup", "hr", "html", "main", "menu", "nav", "ol", "p", "search", "section", "summary", "table", "tbody", "tfoot", "thead", "ul":
		return true
	default:
		return false
	}
}

func (w *canonicalHTMLWriter) writeText(value string) {
	if w.inPre {
		if w.escapeMarkdown {
			w.preContent.WriteString(stripUnsafeControls(value))
			return
		}
		if w.preFenceOpen {
			w.output.WriteByte('\n')
			w.preFenceOpen = false
		}
		w.output.WriteString(stripUnsafeControls(value))
		return
	}
	if w.inlineCode {
		w.inlineContent.WriteString(stripUnsafeControls(value))
		return
	}
	value = stripUnsafeControls(value)
	for _, character := range value {
		if unicode.IsSpace(character) {
			w.pendingSpace = true
			continue
		}
		w.flushPendingSpace()
		w.writeMarkdownRune(character)
	}
}

func (w *canonicalHTMLWriter) writeMarkdownRune(character rune) {
	if w.escapeMarkdown && strings.ContainsRune("\\\\`*_{}[]<>#!|~", character) {
		w.writeRenditionRaw("\\")
	}
	if w.escapeMarkdown {
		w.writeRenditionRaw(string(character))
		return
	}
	w.output.WriteRune(character)
}

func (w *canonicalHTMLWriter) writeRenditionRaw(value string) {
	if len(w.renditionLinks) > 0 {
		w.renditionLinks[len(w.renditionLinks)-1].text.WriteString(value)
		return
	}
	if w.inTableCell {
		w.tableCell.WriteString(value)
		return
	}
	w.output.WriteString(value)
}

func (w *canonicalHTMLWriter) writeSafeInlineCode(content string) {
	fence := strings.Repeat("`", maxBacktickRun(content)+1)
	w.writeRenditionRaw(fence)
	if strings.HasPrefix(content, "`") || strings.HasSuffix(content, "`") {
		w.writeRenditionRaw(" ")
	}
	w.writeRenditionRaw(content)
	if strings.HasPrefix(content, "`") || strings.HasSuffix(content, "`") {
		w.writeRenditionRaw(" ")
	}
	w.writeRenditionRaw(fence)
}

func serializeGFMTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	if columns == 0 {
		return ""
	}
	format := func(row []string) string {
		cells := make([]string, columns)
		copy(cells, row)
		return "| " + strings.Join(cells, " | ") + " |"
	}
	delimiter := make([]string, columns)
	for index := range delimiter {
		delimiter[index] = "---"
	}
	lines := []string{format(rows[0]), "| " + strings.Join(delimiter, " | ") + " |"}
	for _, row := range rows[1:] {
		lines = append(lines, format(row))
	}
	return strings.Join(lines, "\n")
}

func (w *canonicalHTMLWriter) writeSafeCodeFence(language, content string) {
	if w.inTableCell {
		w.writeSafeInlineCode(strings.ReplaceAll(strings.Join(strings.Fields(content), " "), "|", "\\|"))
		return
	}
	fence := strings.Repeat("`", max(3, maxBacktickRun(content)+1))
	w.writeRenditionRaw(fence)
	w.writeRenditionRaw(language)
	w.writeRenditionRaw("\n")
	w.writeRenditionRaw(content)
	if !strings.HasSuffix(content, "\n") {
		w.writeRenditionRaw("\n")
	}
	w.writeRenditionRaw(fence)
}

func maxBacktickRun(value string) int {
	maximum := 0
	current := 0
	for _, character := range value {
		if character == '`' {
			current++
			maximum = max(maximum, current)
		} else {
			current = 0
		}
	}
	return maximum
}

func (w *canonicalHTMLWriter) flushPendingSpace() {
	if w.escapeMarkdown {
		if w.pendingSpace && w.renditionTargetHasContent() {
			w.writeRenditionRaw(" ")
		}
		w.pendingSpace = false
		return
	}
	if w.pendingSpace && w.output.Len() > 0 &&
		!strings.HasSuffix(w.output.String(), "\n") && !strings.HasSuffix(w.output.String(), " ") {
		w.output.WriteByte(' ')
	}
	w.pendingSpace = false
}

func (w *canonicalHTMLWriter) renditionTargetHasContent() bool {
	if len(w.renditionLinks) > 0 {
		return w.renditionLinks[len(w.renditionLinks)-1].text.Len() > 0
	}
	if w.inTableCell {
		return w.tableCell.Len() > 0
	}
	return w.output.Len() > 0 && !strings.HasSuffix(w.output.String(), "\n")
}

func (w *canonicalHTMLWriter) line() {
	if w.escapeMarkdown && w.inTable {
		w.pendingSpace = false
		return
	}
	w.pendingSpace = false
	if w.output.Len() > 0 && !strings.HasSuffix(w.output.String(), "\n") {
		w.output.WriteByte('\n')
	}
}

func (w *canonicalHTMLWriter) block() {
	if w.escapeMarkdown && w.inTable {
		w.pendingSpace = false
		return
	}
	w.line()
	if w.output.Len() > 0 && !strings.HasSuffix(w.output.String(), "\n\n") {
		w.output.WriteByte('\n')
	}
}

func stripUnsafeControls(value string) string {
	return strings.Map(func(character rune) rune {
		switch character {
		case '\n', '\t':
			return character
		case '\f', '\v', '\u0085', '\u2028', '\u2029':
			return '\n'
		}
		if unicode.IsSpace(character) {
			return ' '
		}
		if unicode.IsControl(character) || character == headingSentinelStart || character == headingSentinelEnd {
			return -1
		}
		return character
	}, strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n"))
}

func safeStoredLink(value string, maxChars int) string {
	if utf8.RuneCountInString(value) > maxChars ||
		strings.ContainsRune(value, headingSentinelStart) || strings.ContainsRune(value, headingSentinelEnd) {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return ""
	}
	return parsed.String()
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

type canonicalHeadingMark struct {
	CharOffset int
	EndOffset  int
	Level      int
}

func canonicalWhitespace(value string) (string, []canonicalHeadingMark) {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	var output strings.Builder
	headings := make([]canonicalHeadingMark, 0)
	activeHeadings := make([]int, 0, 1)
	blank := false
	endsWithNewline := false
	runeOffset := 0
	writeNewline := func() {
		output.WriteByte('\n')
		endsWithNewline = true
		runeOffset++
	}
	closeHeadings := func(count int) {
		for range count {
			if len(activeHeadings) == 0 {
				return
			}
			index := activeHeadings[len(activeHeadings)-1]
			activeHeadings = activeHeadings[:len(activeHeadings)-1]
			headings[index].EndOffset = runeOffset
		}
	}
	for _, line := range lines {
		level := 0
		prefix := string(headingSentinelStart) + "H"
		if strings.HasPrefix(line, prefix) {
			end := strings.IndexRune(line, headingSentinelEnd)
			if end == len(prefix)+1 && line[len(prefix)] >= '1' && line[len(prefix)] <= '6' {
				level = int(line[len(prefix)] - '0')
				line = line[end+utf8.RuneLen(headingSentinelEnd):]
			}
		}
		closedHeadings := strings.Count(line, headingMarkerClose)
		line = strings.ReplaceAll(line, headingMarkerClose, "")
		line = strings.TrimRight(line, " \t")
		if line == "" {
			closeHeadings(closedHeadings)
			if output.Len() == 0 || blank {
				continue
			}
			blank = true
			writeNewline()
			continue
		}
		blank = false
		if output.Len() > 0 && !endsWithNewline {
			writeNewline()
		}
		offset := runeOffset
		if level > 0 {
			headings = append(headings, canonicalHeadingMark{CharOffset: offset, Level: level})
			activeHeadings = append(activeHeadings, len(headings)-1)
		}
		output.WriteString(line)
		runeOffset += utf8.RuneCountInString(line)
		endsWithNewline = false
		closeHeadings(closedHeadings)
	}
	for _, index := range activeHeadings {
		headings[index].EndOffset = runeOffset
	}
	return strings.TrimRight(output.String(), "\n"), headings
}

func boundHeadingMarks(text string, headings []canonicalHeadingMark) []HeadingMark {
	textRunes := []rune(text)
	bounded := make([]HeadingMark, 0, len(headings))
	headingPath := make([]string, 0, 6)
	for _, heading := range headings {
		if heading.CharOffset < 0 || heading.CharOffset >= len(textRunes) || heading.Level < 1 || heading.Level > 6 {
			continue
		}
		end := min(max(heading.EndOffset, heading.CharOffset), len(textRunes))
		title := strings.Join(strings.Fields(strings.TrimLeft(string(textRunes[heading.CharOffset:end]), "#")), " ")
		for len(headingPath) < heading.Level {
			headingPath = append(headingPath, "")
		}
		headingPath = headingPath[:heading.Level]
		headingPath[heading.Level-1] = title
		bounded = append(bounded, HeadingMark{
			CharOffset: heading.CharOffset,
			Path:       compactHeadingPath(headingPath),
		})
	}
	return bounded
}

func truncateUTF8Bytes(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut], true
}

func truncateRunes(value string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	for byteOffset := range value {
		if limit == 0 {
			return value[:byteOffset], true
		}
		limit--
	}
	return value, false
}

func chunkNormalizedUnits(units []NormalizedUnit, policy NormalizePolicy) ([]Chunk, bool) {
	chunks := make([]Chunk, 0)
	truncated := false
	for _, unit := range units {
		spans := chunkUnitText(unit.Text, policy.maxChunkRunes, policy.chunkOverlap)
		for _, span := range spans {
			if len(chunks) >= policy.maxChunks {
				return chunks, true
			}
			chunk := Chunk{
				Key:     fmt.Sprintf("%s:%06d-%06d", unit.SourceKey, span.CharStart, span.CharEnd),
				Ordinal: len(chunks), Text: span.Text, HeadingPath: headingPathAt(unit.HeadingMarks, span.CharStart),
				CharCount: utf8.RuneCountInString(span.Text), Truncated: unit.Truncated,
				Spans: []ChunkSpan{{UnitIndex: unit.Index, CharStart: span.CharStart, CharEnd: span.CharEnd}},
			}
			chunk.Checksum = checksumNormalizedChunk(chunk)
			chunks = append(chunks, chunk)
		}
	}
	return chunks, truncated
}

type unitChunkSpan struct {
	Text      string
	CharStart int
	CharEnd   int
}

func chunkUnitText(text string, maxRunes, overlapRunes int) []unitChunkSpan {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return []unitChunkSpan{{Text: text, CharEnd: len(runes)}}
	}
	spans := make([]unitChunkSpan, 0, len(runes)/maxRunes+1)
	for cursor := 0; cursor < len(runes); {
		end := min(cursor+maxRunes, len(runes))
		cut := end
		if end < len(runes) {
			floor := max(cursor+(maxRunes*3/4), cursor+1)
			for i := end - 1; i >= floor; i-- {
				if runes[i] == '\n' {
					cut = i + 1
					break
				}
			}
			if cut == end {
				for i := end - 1; i >= floor; i-- {
					if unicode.IsSpace(runes[i]) {
						cut = i + 1
						break
					}
				}
			}
		}
		spans = append(spans, unitChunkSpan{Text: string(runes[cursor:cut]), CharStart: cursor, CharEnd: cut})
		if cut == len(runes) {
			break
		}
		cursor += max((cut-cursor)-overlapRunes, 1)
	}
	return spans
}

func compactHeadingPath(path []string) []string {
	result := make([]string, 0, len(path))
	for _, part := range path {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func headingPathAt(marks []HeadingMark, offset int) []string {
	var result []string
	for _, mark := range marks {
		if mark.CharOffset > offset {
			break
		}
		result = mark.Path
	}
	return append([]string(nil), result...)
}

func checksumStrings(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(hash, fmt.Sprintf("%d:", len(value)))
		_, _ = io.WriteString(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
