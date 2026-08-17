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
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"golang.org/x/net/html"
)

const (
	headingSentinelStart = '\ue000'
	headingSentinelEnd   = '\ue001'
)

// NormalizeDocument converts transient provider Markdown into deterministic,
// inert canonical text plus exact unit spans. It never retains raw responses.
func NormalizeDocument(source SourceDocument, policy NormalizePolicy) (NormalizedDocument, error) {
	if source.Family == "" || source.UnitKind == "" || len(source.Units) == 0 {
		return NormalizedDocument{}, errors.New("document normalization requires family, unit kind, and units")
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
		text, headings, sourceTruncated, err := canonicalMarkdown(
			unit.Markdown, policy.maxLinkChars, policy.maxSourceUnitBytes,
		)
		if err != nil {
			return NormalizedDocument{}, fmt.Errorf("normalize document source unit %d: %w", i, err)
		}
		header, _, headerSourceTruncated, err := canonicalMarkdown(
			unit.Header, policy.maxLinkChars, policy.maxMetadataSourceBytes,
		)
		if err != nil {
			return NormalizedDocument{}, fmt.Errorf("normalize document source unit %d header: %w", i, err)
		}
		footer, _, footerSourceTruncated, err := canonicalMarkdown(
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
		}
		text, truncated = truncateRunes(text, min(policy.maxUnitChars, remaining))
		unitTruncated = unitTruncated || truncated
		if truncated {
			result.Truncated = true
		}
		combinedChars := utf8.RuneCountInString(text)
		if combinedChars == 0 && remaining == 0 {
			result.Truncated = true
			break
		}
		remaining -= combinedChars
		boundedHeadings := boundHeadingMarks(text, headings)
		normalized := NormalizedUnit{
			Index: unit.Index, SourceKey: fmt.Sprintf("%s:%06d", source.UnitKind, unit.Index), Kind: source.UnitKind,
			Text: text, Header: header, Footer: footer, Dimensions: unit.Dimensions,
			CharCount: utf8.RuneCountInString(text), Truncated: unitTruncated, HeadingMarks: boundedHeadings,
		}
		normalized.Checksum = checksumStrings(normalized.SourceKey, normalized.Text, normalized.Header, normalized.Footer)
		result.Units = append(result.Units, normalized)
		result.Truncated = result.Truncated || unitTruncated
	}
	if len(result.Units) == 0 {
		return NormalizedDocument{}, errors.New("document normalization produced no units")
	}

	chunks, chunksTruncated := chunkNormalizedUnits(result.Units, policy)
	result.Chunks = chunks
	result.Truncated = result.Truncated || chunksTruncated
	checksumParts := []string{fmt.Sprintf("v%d", result.PolicyVersion), result.Family, result.UnitKind}
	for _, unit := range result.Units {
		checksumParts = append(checksumParts, unit.Checksum)
	}
	for _, chunk := range result.Chunks {
		checksumParts = append(checksumParts, chunk.Checksum)
	}
	result.Checksum = checksumStrings(checksumParts...)
	return result, nil
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

func canonicalMarkdown(markdown string, maxLinkChars, maxSourceBytes int) (string, []canonicalHeadingMark, bool, error) {
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
	writer := canonicalHTMLWriter{maxLinkChars: maxLinkChars}
	if err := writer.consume(bytes.NewReader(rendered.Bytes())); err != nil {
		return "", nil, false, err
	}
	text, headings := canonicalWhitespace(writer.output.String())
	return text, headings, sourceTruncated, nil
}

type canonicalHTMLWriter struct {
	output       strings.Builder
	maxLinkChars int
	inPre        bool
	skipTags     []string
	cellIndex    int
	links        []string
	preFenceOpen bool
	pendingSpace bool
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
			if len(w.skipTags) == 0 {
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
	if len(w.skipTags) > 0 {
		if !selfClosing && !isHTMLVoidElement(tag) {
			w.skipTags = append(w.skipTags, tag)
		}
		return
	}
	if tag == "script" || tag == "style" {
		w.skipTags = append(w.skipTags, tag)
		return
	}
	if tag == "svg" {
		if !selfClosing {
			w.skipTags = append(w.skipTags, tag)
		}
		return
	}
	switch tag {
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
		w.line()
		w.cellIndex = 0
	case "td", "th":
		if w.cellIndex > 0 {
			w.output.WriteString(" | ")
		}
		w.cellIndex++
	case "pre":
		w.block()
		w.output.WriteString("```")
		w.inPre = true
		w.preFenceOpen = true
	case "code":
		if w.inPre && w.preFenceOpen {
			for _, attribute := range token.Attr {
				if attribute.Key == "class" && strings.HasPrefix(attribute.Val, "language-") {
					if language := safeCodeLanguage(strings.TrimPrefix(attribute.Val, "language-")); language != "" {
						w.output.WriteString(language)
					}
				}
			}
			w.output.WriteByte('\n')
			w.preFenceOpen = false
		} else if !w.inPre {
			w.flushPendingSpace()
			w.output.WriteByte('`')
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
			if checked {
				w.output.WriteString("[x] ")
			} else {
				w.output.WriteString("[ ] ")
			}
		}
	case "a":
		link := ""
		for _, attribute := range token.Attr {
			if attribute.Key == "href" {
				link = safeStoredLink(attribute.Val, w.maxLinkChars)
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

func (w *canonicalHTMLWriter) endTag(tag string) {
	if len(w.skipTags) > 0 {
		for index, v := range slices.Backward(w.skipTags) {
			if v == tag {
				w.skipTags = w.skipTags[:index]
				break
			}
		}
		return
	}
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.block()
	case "li", "tr":
		w.line()
	case "pre":
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
			w.output.WriteByte('`')
		}
	case "a":
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

func isHTMLVoidElement(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
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
		if w.preFenceOpen {
			w.output.WriteByte('\n')
			w.preFenceOpen = false
		}
		w.output.WriteString(stripUnsafeControls(value))
		return
	}
	value = stripUnsafeControls(value)
	for _, character := range value {
		if unicode.IsSpace(character) {
			w.pendingSpace = true
			continue
		}
		w.flushPendingSpace()
		w.output.WriteRune(character)
	}
}

func (w *canonicalHTMLWriter) flushPendingSpace() {
	if w.pendingSpace && w.output.Len() > 0 &&
		!strings.HasSuffix(w.output.String(), "\n") && !strings.HasSuffix(w.output.String(), " ") {
		w.output.WriteByte(' ')
	}
	w.pendingSpace = false
}

func (w *canonicalHTMLWriter) line() {
	w.pendingSpace = false
	if w.output.Len() > 0 && !strings.HasSuffix(w.output.String(), "\n") {
		w.output.WriteByte('\n')
	}
}

func (w *canonicalHTMLWriter) block() {
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
	if utf8.RuneCountInString(value) > maxChars {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

type canonicalHeadingMark struct {
	CharOffset int
	Level      int
}

func canonicalWhitespace(value string) (string, []canonicalHeadingMark) {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	var output strings.Builder
	headings := make([]canonicalHeadingMark, 0)
	blank := false
	endsWithNewline := false
	runeOffset := 0
	writeNewline := func() {
		output.WriteByte('\n')
		endsWithNewline = true
		runeOffset++
	}
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			if output.Len() == 0 || blank {
				continue
			}
			blank = true
			writeNewline()
			continue
		}
		level := 0
		prefix := string(headingSentinelStart) + "H"
		if strings.HasPrefix(line, prefix) {
			end := strings.IndexRune(line, headingSentinelEnd)
			if end == len(prefix)+1 && line[len(prefix)] >= '1' && line[len(prefix)] <= '6' {
				level = int(line[len(prefix)] - '0')
				line = line[end+utf8.RuneLen(headingSentinelEnd):]
			}
		}
		blank = false
		if output.Len() > 0 && !endsWithNewline {
			writeNewline()
		}
		offset := runeOffset
		output.WriteString(line)
		runeOffset += utf8.RuneCountInString(line)
		endsWithNewline = false
		if level > 0 {
			headings = append(headings, canonicalHeadingMark{CharOffset: offset, Level: level})
		}
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
		end := heading.CharOffset
		for end < len(textRunes) && textRunes[end] != '\n' {
			end++
		}
		title := strings.TrimSpace(strings.TrimLeft(string(textRunes[heading.CharOffset:end]), "#"))
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
			chunk.Checksum = checksumStrings(chunk.Key, chunk.Text, strings.Join(chunk.HeadingPath, "\x00"))
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
