package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
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

// ErrRenditionXHTMLBudget means XHTML could not be normalized completely within its limits.
var ErrRenditionXHTMLBudget = errors.New("XHTML rendition exceeds its work or output limit")

// RenditionMarkdownFromXHTML converts a complete UTF-8 XML document to bounded Markdown.
func RenditionMarkdownFromXHTML(source []byte, maxRunes int) (string, error) {
	return RenditionMarkdownFromXHTMLContext(context.Background(), source, maxRunes)
}

// RenditionMarkdownFromXHTMLContext converts XHTML while observing cancellation.
func RenditionMarkdownFromXHTMLContext(ctx context.Context, source []byte, maxRunes int) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if maxRunes < 0 || len(source) > 100<<20 {
		return "", ErrRenditionXHTMLBudget
	}
	if !utf8.Valid(source) {
		return "", errors.New("XHTML must be UTF-8")
	}
	if err := checkRenditionXHTMLAttributeBound(ctx, source); err != nil {
		return "", err
	}
	maxRunes = min(maxRunes, maxEvidenceTextBytes)
	inlineAllocation := int64(unsafe.Sizeof(renditionInline{}))
	inlineAllowance := 4*inlineAllocation + 4
	budget := min(int64(100<<20), int64(len(source))+(int64(maxRunes)+1)*inlineAllowance)
	writer := renditionHTMLWriter{ctx: ctx, maxLinkChars: renditionMaxLinkChars, work: &renditionXHTMLWork{remaining: budget}}
	decoder := xml.NewDecoder(contextReader{ctx: ctx, reader: bytes.NewReader(bytes.TrimPrefix(source, []byte{0xef, 0xbb, 0xbf}))})
	decoder.Entity = xml.HTMLEntity
	depth, roots, head := 0, 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("decode XHTML: %w", err)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
			return "", errors.New("XHTML XML is invalid or uses an unsupported encoding")
		}
		if !writer.charge(1) {
			return "", ErrRenditionXHTMLBudget
		}
		switch token := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots != 1 || token.Name.Local != "html" || token.Name.Space != "http://www.w3.org/1999/xhtml" {
					return "", errors.New("XHTML requires one XHTML html root")
				}
			}
			depth++
			if depth > maxRenditionInlineDepth {
				return "", ErrRenditionXHTMLBudget
			}
			if head > 0 || token.Name.Local == "head" {
				head++
				continue
			}
			converted := html.Token{Data: token.Name.Local}
			for _, attr := range token.Attr {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if !writer.charge(int64(len(attr.Name.Local)) + int64(len(attr.Value)) + 2*int64(unsafe.Sizeof(html.Attribute{}))) {
					return "", ErrRenditionXHTMLBudget
				}
				converted.Attr = append(converted.Attr, html.Attribute{Key: attr.Name.Local, Val: attr.Value})
			}
			writer.startTag(converted, 0, false)
			if err := writer.contextError(); err != nil {
				return "", err
			}
		case xml.EndElement:
			depth--
			if head > 0 {
				head--
				continue
			}
			writer.endTag(token.Name.Local)
			if err := writer.contextError(); err != nil {
				return "", err
			}
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(token)) > 0 {
				return "", errors.New("XHTML has text outside its root")
			}
			if head == 0 && writer.skipDepth == 0 {
				if !writer.charge(int64(len(token))) {
					return "", ErrRenditionXHTMLBudget
				}
				value, err := canonicalEvidenceStringContext(ctx, string(token))
				if err != nil {
					return "", err
				}
				if err := writer.writeTextContext(ctx, value); err != nil {
					return "", err
				}
			}
		}
		if writer.work.exceeded || writer.linkDepthTruncated {
			return "", ErrRenditionXHTMLBudget
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if roots != 1 || depth != 0 {
		return "", errors.New("XHTML document is incomplete")
	}
	writer.finalize()
	if err := canonicalizeRenditionBlocks(ctx, writer.blocks); err != nil {
		return "", err
	}
	fits, err := renditionXHTMLSerializationFits(ctx, writer.blocks, budget)
	if err != nil {
		return "", err
	}
	if writer.work.exceeded || !fits {
		return "", ErrRenditionXHTMLBudget
	}
	text, truncated, err := serializeRenditionBlocksContext(ctx, writer.blocks, maxRunes)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text, err = canonicalEvidenceStringContext(ctx, text)
	if err != nil {
		return "", err
	}
	if truncated || len(text) > maxEvidenceTextBytes || utf8.RuneCountInString(text) > maxRunes {
		return "", ErrRenditionXHTMLBudget
	}
	return text, nil
}

const maxRenditionXHTMLAttributes = 1 << 18

func checkRenditionXHTMLAttributeBound(ctx context.Context, source []byte) error {
	for index := 0; index < len(source); {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if source[index] != '<' || index+1 >= len(source) {
			index++
			continue
		}
		if bytes.HasPrefix(source[index:], []byte("<!--")) {
			index += len("<!--")
			for index+2 < len(source) && !bytes.Equal(source[index:index+3], []byte("-->")) {
				if index&1023 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				index++
			}
			index += min(3, len(source)-index)
			continue
		}
		if bytes.HasPrefix(source[index:], []byte("<![CDATA[")) {
			index += len("<![CDATA[")
			for index+2 < len(source) && !bytes.Equal(source[index:index+3], []byte("]]>")) {
				if index&1023 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				index++
			}
			index += min(3, len(source)-index)
			continue
		}
		if source[index+1] == '?' {
			index += 2
			for index+1 < len(source) && (source[index] != '?' || source[index+1] != '>') {
				if index&1023 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				index++
			}
			index += min(2, len(source)-index)
			continue
		}
		if source[index+1] == '!' {
			index++
			quote := byte(0)
			for index < len(source) {
				if index&1023 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				if quote == 0 && bytes.HasPrefix(source[index:], []byte("<!--")) {
					index += len("<!--")
					for index+2 < len(source) && !bytes.Equal(source[index:index+3], []byte("-->")) {
						if index&1023 == 0 {
							if err := ctx.Err(); err != nil {
								return err
							}
						}
						index++
					}
					index += min(3, len(source)-index)
					continue
				}
				character := source[index]
				if quote != 0 {
					if character == quote {
						quote = 0
					}
				} else if character == '\'' || character == '"' {
					quote = character
				} else if character == '>' {
					index++
					break
				}
				index++
			}
			continue
		}
		if source[index+1] == '/' {
			index++
			for index < len(source) && source[index] != '>' {
				if index&1023 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				index++
			}
			index += min(1, len(source)-index)
			continue
		}

		index++
		attributes := 0
		quote := byte(0)
		for index < len(source) {
			if index&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			character := source[index]
			if quote != 0 {
				if character == quote {
					quote = 0
				}
			} else if character == '\'' || character == '"' {
				quote = character
			} else if character == '=' {
				attributes++
				if attributes > maxRenditionXHTMLAttributes {
					return ErrRenditionXHTMLBudget
				}
			} else if character == '>' {
				index++
				break
			}
			index++
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(p []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := reader.reader.Read(p)
	if contextErr := reader.ctx.Err(); contextErr != nil {
		return n, contextErr
	}
	return n, err
}

func canonicalizeRenditionBlocks(ctx context.Context, blocks []renditionBlock) error {
	for index := range blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		block := &blocks[index]
		canonical, err := canonicalEvidenceStringContext(ctx, block.code)
		if err != nil {
			return err
		}
		block.code = canonical
		inlines, err := canonicalizeRenditionInlines(ctx, block.inlines)
		if err != nil {
			return err
		}
		block.inlines = inlines
		for rowIndex, row := range block.rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			for cellIndex, cell := range row {
				if err := ctx.Err(); err != nil {
					return err
				}
				inlines, err := canonicalizeRenditionInlines(ctx, cell)
				if err != nil {
					return err
				}
				row[cellIndex] = inlines
			}
			block.rows[rowIndex] = row
		}
		if block.list == nil {
			continue
		}
		for _, item := range block.list.items {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := canonicalizeRenditionBlocks(ctx, item.blocks); err != nil {
				return err
			}
		}
	}
	return nil
}

func canonicalizeRenditionInlines(ctx context.Context, values []renditionInline) ([]renditionInline, error) {
	result := values[:0]
	var text strings.Builder
	flushText := func() error {
		if text.Len() == 0 {
			return nil
		}
		canonical, err := canonicalEvidenceStringContext(ctx, text.String())
		if err != nil {
			return err
		}
		result = append(result, renditionInline{kind: renditionText, text: canonical})
		text.Reset()
		return nil
	}
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if value.kind == renditionText {
			text.WriteString(value.text)
			continue
		}
		if err := flushText(); err != nil {
			return nil, err
		}
		if value.kind == renditionInlineCode {
			var err error
			value.text, err = canonicalEvidenceStringContext(ctx, value.text)
			if err != nil {
				return nil, err
			}
		} else {
			children, err := canonicalizeRenditionInlines(ctx, value.children)
			if err != nil {
				return nil, err
			}
			value.children = children
		}
		result = append(result, value)
	}
	if err := flushText(); err != nil {
		return nil, err
	}
	return result, nil
}

type renditionXHTMLWork struct {
	remaining int64
	exceeded  bool
}

func (w *renditionHTMLWriter) charge(amount int64) bool {
	if w.work == nil {
		return true
	}
	if amount > w.work.remaining {
		w.work.exceeded = true
		return false
	}
	w.work.remaining -= amount
	return true
}

// Bound rectangular table padding and temporary serialization before allocation.
func renditionXHTMLSerializationFits(ctx context.Context, blocks []renditionBlock, budget int64) (bool, error) {
	var inlines func([]renditionInline) (int64, error)
	inlines = func(values []renditionInline) (int64, error) {
		var size int64
		for _, value := range values {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			size += int64(len(value.text))
			switch value.kind {
			case renditionText:
				runes := 0
				for _, r := range value.text {
					if runes&1023 == 0 {
						if err := ctx.Err(); err != nil {
							return 0, err
						}
					}
					runes++
					if isMarkdownASCIIPunctuation(r) {
						size++
					}
				}
			case renditionInlineCode:
				backticks, err := maxBacktickRunContext(ctx, value.text)
				if err != nil {
					return 0, err
				}
				size += 2*int64(backticks+1) + 4 + int64(strings.Count(value.text, "|"))
			case renditionLinkInline:
				children, err := inlines(value.children)
				if err != nil {
					return 0, err
				}
				size += int64(len(value.destination)) + 8 + children
			}
		}
		return size, nil
	}
	var cost func([]renditionBlock, int) (int64, error)
	cost = func(blocks []renditionBlock, indent int) (int64, error) {
		var total int64
		for _, block := range blocks {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			inlineSize, err := inlines(block.inlines)
			if err != nil {
				return 0, err
			}
			total += 16 + int64(indent)*int64(3+strings.Count(block.code, "\n")+len(block.rows)) + inlineSize
			if block.kind == renditionCodeBlock {
				backticks, err := maxBacktickRunContext(ctx, block.code)
				if err != nil {
					return 0, err
				}
				total += int64(len(block.code) + len(block.language) + 3 + 2*max(3, backticks+1))
			}
			columns := 0
			for _, row := range block.rows {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				columns = max(columns, len(row))
				for _, cell := range row {
					if err := ctx.Err(); err != nil {
						return 0, err
					}
					cellSize, err := inlines(cell)
					if err != nil {
						return 0, err
					}
					total += cellSize
				}
			}
			total += int64(len(block.rows)+1) * (int64(columns)*6 + 3)
			if block.list != nil {
				for _, item := range block.list.items {
					if err := ctx.Err(); err != nil {
						return 0, err
					}
					itemCost, err := cost(item.blocks, indent+maxOrderedListMarkerDigits+4)
					if err != nil {
						return 0, err
					}
					total += itemCost + int64(len(block.list.start)) + 16
				}
			}
			if total > budget {
				return total, nil
			}
		}
		return total, nil
	}
	total, err := cost(blocks, 0)
	if err != nil {
		return false, err
	}
	return total <= budget, nil
}

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
	text, renditionTruncated, err := serializeRenditionBlocksContext(context.Background(), writer.blocks, maxRunes)
	if err != nil {
		return "", false, false, err
	}
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

// sanitizeMarkdown converts untrusted provider Markdown into inert canonical
// Markdown-like text. It preserves NormalizeDocument's frozen behavior.
func sanitizeMarkdown(
	markdown string, maxLinkChars, maxSourceBytes int,
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
	writer := canonicalHTMLWriter{maxLinkChars: maxLinkChars}
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
	ctx            context.Context
	err            error
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
		if err := w.contextError(); err != nil {
			return err
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
	return w.contextError()
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
	raw := renditionHTMLWriter{ctx: w.context(), maxLinkChars: w.maxLinkChars, rawFragment: true}
	if err := raw.consumeFragment(fragment, false); err != nil {
		return err
	}
	raw.finalize()
	if err := raw.contextError(); err != nil {
		return err
	}
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
		if err := w.contextError(); err != nil {
			return err
		}
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
				if err := w.writeTextContext(w.context(), string(tokenizer.Text())); err != nil {
					return err
				}
			}
		case html.StartTagToken:
			w.startTag(tokenizer.Token(), tokenOffset, suppressText)
			if err := w.contextError(); err != nil {
				return err
			}
		case html.SelfClosingTagToken:
			w.startTag(tokenizer.Token(), tokenOffset, suppressText)
			if err := w.contextError(); err != nil {
				return err
			}
		case html.EndTagToken:
			tag := tokenizer.Token().Data
			if !w.endSuppressedTag(tag) {
				w.endTag(tag)
			}
			if err := w.contextError(); err != nil {
				return err
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
		marker, ok, err := parserGeneratedCheckboxMarkerContext(w.context(), token)
		if err != nil {
			w.err = err
			return
		}
		if ok {
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
				if err := w.contextError(); err != nil {
					w.err = err
					return
				}
				if attribute.Key == "start" {
					parsed, ok, err := canonicalNonnegativeDecimalContext(w.context(), attribute.Val)
					if err != nil {
						w.err = err
						return
					}
					if ok {
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
				if err := w.contextError(); err != nil {
					w.err = err
					return
				}
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
			if err := w.contextError(); err != nil {
				w.err = err
				return
			}
			if attribute.Key == "alt" {
				w.writeText(attribute.Val)
				break
			}
		}
	case "a":
		w.flushPendingSpace()
		destination := ""
		for _, attribute := range token.Attr {
			if err := w.contextError(); err != nil {
				w.err = err
				return
			}
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

func parserGeneratedCheckboxMarkerContext(ctx context.Context, token html.Token) (string, bool, error) {
	checkbox, checked := false, false
	for _, attribute := range token.Attr {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		switch attribute.Key {
		case "type":
			checkbox = attribute.Val == "checkbox"
		case "checked":
			checked = true
		}
	}
	if !checkbox {
		return "", false, nil
	}
	if checked {
		return "[x] ", true, nil
	}
	return "[ ] ", true, nil
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
			content, err := stripUnsafeControlsContext(w.context(), w.preText.String())
			if err != nil {
				w.err = err
				return
			}
			if w.preInCell {
				collapsed, err := collapseRenditionWhitespaceContext(w.context(), content, w.charge)
				if err != nil {
					w.err = err
					return
				}
				w.appendInline(renditionInline{kind: renditionInlineCode, text: collapsed})
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
			content, err := stripUnsafeControlsContext(w.context(), w.inlineText.String())
			if err != nil {
				w.err = err
				return
			}
			w.appendInline(renditionInline{kind: renditionInlineCode, text: content})
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
	if err := w.writeTextContext(w.context(), value); err != nil {
		w.err = err
	}
}

func (w *renditionHTMLWriter) writeTextContext(ctx context.Context, value string) error {
	var err error
	value, err = stripUnsafeControlsContext(ctx, value)
	if err != nil {
		return err
	}
	if w.inPre {
		if !w.charge(2 * int64(len(value))) {
			return ErrRenditionXHTMLBudget
		}
		w.preText.WriteString(value)
		return nil
	}
	if w.inlineCode {
		if !w.charge(2 * int64(len(value))) {
			return ErrRenditionXHTMLBudget
		}
		w.inlineText.WriteString(value)
		return nil
	}
	var chunk strings.Builder
	flushChunk := func() {
		if chunk.Len() == 0 {
			return
		}
		w.appendText(chunk.String())
		chunk.Reset()
	}
	runes := 0
	for _, character := range value {
		if runes&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		runes++
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
	return ctx.Err()
}

func collapseRenditionWhitespaceContext(ctx context.Context, value string, charge func(int64) bool) (string, error) {
	var size int64
	pendingSpace, wrote := false, false
	for index, character := range value {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		if unicode.IsSpace(character) {
			if wrote {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			size++
		}
		size += int64(utf8.RuneLen(character))
		pendingSpace = false
		wrote = true
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !charge(size) {
		return "", ErrRenditionXHTMLBudget
	}
	var output strings.Builder
	output.Grow(int(size))
	pendingSpace, wrote = false, false
	for index, character := range value {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		if unicode.IsSpace(character) {
			if wrote {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			output.WriteByte(' ')
		}
		output.WriteRune(character)
		pendingSpace = false
		wrote = true
	}
	return output.String(), nil
}

func (w *renditionHTMLWriter) context() context.Context {
	if w.ctx == nil {
		return context.Background()
	}
	return w.ctx
}

func (w *renditionHTMLWriter) contextError() error {
	if w.err != nil {
		return w.err
	}
	return w.context().Err()
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
		if err := w.contextError(); err != nil {
			w.err = err
			return
		}
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

type renditionBuilder struct {
	strings.Builder

	ctx   context.Context
	err   error
	runes int
}

func (b *renditionBuilder) WriteString(value string) {
	b.Builder.WriteString(value)
	b.runes += utf8.RuneCountInString(value)
}

func serializeRenditionBlocks(blocks []renditionBlock, limit int) (string, bool) {
	text, truncated, _ := serializeRenditionBlocksContext(context.Background(), blocks, limit)
	return text, truncated
}

func serializeRenditionBlocksContext(ctx context.Context, blocks []renditionBlock, limit int) (string, bool, error) {
	output := renditionBuilder{ctx: ctx}
	truncated := false
	previousList := false
	previousListOrdered := false
	previousListAlternate := false
	previousKind := renditionParagraph
	for _, block := range blocks {
		if err := output.contextError(); err != nil {
			return "", false, err
		}
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
			return finishRenditionMarkdown(output.String()), true, nil
		}
		value, blockTruncated, err := serializeRenditionBlock(ctx, block, available, listAlternate)
		if err != nil {
			return "", false, err
		}
		if value == "" && blockTruncated {
			return finishRenditionMarkdown(output.String()), true, nil
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
	return finishRenditionMarkdown(output.String()), truncated, nil
}

func (b *renditionBuilder) contextError() error {
	if b.err != nil {
		return b.err
	}
	if b.ctx != nil {
		b.err = b.ctx.Err()
	}
	return b.err
}

func finishRenditionMarkdown(value string) string {
	return strings.TrimRight(value, "\n")
}

func canonicalNonnegativeDecimal(value string) (string, bool) {
	parsed, ok, _ := canonicalNonnegativeDecimalContext(context.Background(), value)
	return parsed, ok
}

func canonicalNonnegativeDecimalContext(ctx context.Context, value string) (string, bool, error) {
	value = strings.TrimPrefix(value, "+")
	if value == "" {
		return "", false, nil
	}
	for index, character := range value {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", false, err
			}
		}
		if character < '0' || character > '9' {
			return "", false, nil
		}
	}
	first := 0
	for first < len(value) && value[first] == '0' {
		if first&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", false, err
			}
		}
		first++
	}
	if first == len(value) {
		return "0", true, nil
	}
	return value[first:], true, nil
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

func serializeRenditionBlock(ctx context.Context, block renditionBlock, available int, listAlternate bool) (string, bool, error) {
	switch block.kind {
	case renditionCodeBlock:
		value, err := serializeRenditionCodeBlockContext(ctx, block.language, block.code)
		if err != nil {
			return "", false, err
		}
		if utf8.RuneCountInString(value) > available {
			return "", true, nil
		}
		return value, false, nil
	case renditionTable:
		value, err := serializeRenditionTable(ctx, block.rows)
		if err != nil {
			return "", false, err
		}
		if utf8.RuneCountInString(value) > available {
			return "", true, nil
		}
		return value, false, nil
	case renditionListBlock:
		if block.list == nil {
			return "", false, nil
		}
		value, truncated, err := serializeRenditionListContext(ctx, *block.list, available, listAlternate)
		return value, truncated, err
	case renditionParagraph, renditionHeading:
		prefix := ""
		switch block.kind {
		case renditionParagraph:
			// Paragraphs have no generated block marker.
		case renditionHeading:
			prefix = strings.Repeat("#", block.level) + " "
		case renditionCodeBlock, renditionTable, renditionListBlock:
			return "", false, nil
		}
		if utf8.RuneCountInString(prefix) > available {
			return "", true, nil
		}
		value, truncated, err := serializeRenditionInlinesContext(ctx, block.inlines, available-utf8.RuneCountInString(prefix), false)
		if err != nil {
			return "", false, err
		}
		if value == "" && truncated {
			return "", true, nil
		}
		return prefix + value, truncated, nil
	default:
		return "", false, nil
	}
}

func serializeRenditionList(list renditionList, available int, alternate bool) (string, bool) {
	value, truncated, _ := serializeRenditionListContext(context.Background(), list, available, alternate)
	return value, truncated
}

func serializeRenditionListContext(ctx context.Context, list renditionList, available int, alternate bool) (string, bool, error) {
	output := renditionBuffer{ctx: ctx}
	result := appendRenditionList(&output, list, available, 0, alternate)
	if output.err != nil {
		return "", false, output.err
	}
	return output.String(), result.truncated, nil
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
	if output.contextError() != nil {
		return renditionListResult{truncated: true}
	}
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
		if output.contextError() != nil {
			return renditionListResult{truncated: true}
		}
		if !item.present {
			continue
		}
		if !degraded && len(ordinal) > maxOrderedListMarkerDigits {
			fallback = append([]byte(nil), output.bytes[listStart.bytes:]...)
			fallbackRunes = output.runes - listStart.runes
			var converted renditionBuffer
			for index, entry := range entries {
				if output.contextError() != nil {
					return renditionListResult{truncated: true}
				}
				if index > 0 {
					converted.WriteString(renditionListItemSeparator(list.tight))
				}
				degradedItem, err := degradeOrderedListItem(output.ctx, serializedOrderedListItem{
					ordinal: entry.ordinal,
					value:   string(output.bytes[entry.start:entry.end]),
				}, indent, alternate)
				if err != nil {
					output.err = err
					return renditionListResult{truncated: true}
				}
				converted.WriteString(degradedItem)
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
		if output.contextError() != nil {
			return renditionListResult{truncated: true}
		}
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
		if output.contextError() != nil {
			return output.runes > start.runes, true
		}
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

func degradeOrderedListItem(ctx context.Context, item serializedOrderedListItem, indent int, alternate bool) (string, error) {
	normalMarker := item.ordinal + "."
	degradedMarker := "-"
	if alternate {
		normalMarker = item.ordinal + ")"
		degradedMarker = "*"
	}
	normalPrefix := strings.Repeat(" ", indent) + normalMarker
	degradedPrefix := strings.Repeat(" ", indent) + degradedMarker + " " + item.ordinal + "\\."
	normalIndent := strings.Repeat(" ", indent+utf8.RuneCountInString(normalMarker)+1)
	degradedIndent := strings.Repeat(" ", indent+2)
	var result strings.Builder
	lineStart := 0
	for offset := 0; offset <= len(item.value); offset++ {
		if offset&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		if offset != len(item.value) && item.value[offset] != '\n' {
			continue
		}
		if lineStart == 0 {
			result.WriteString(degradedPrefix)
			result.WriteString(strings.TrimPrefix(item.value[lineStart:offset], normalPrefix))
		} else if after, ok := strings.CutPrefix(item.value[lineStart:offset], normalIndent); ok {
			result.WriteString(degradedIndent)
			result.WriteString(after)
		} else {
			result.WriteString(item.value[lineStart:offset])
		}
		if offset < len(item.value) {
			result.WriteByte('\n')
			lineStart = offset + 1
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return result.String(), nil
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
		var err error
		value, err = serializeRenditionCodeBlockContext(output.ctx, block.language, block.code)
		if err != nil {
			output.err = err
			return false, true
		}
	case renditionTable:
		var err error
		value, err = serializeRenditionTable(output.ctx, block.rows)
		if err != nil {
			output.err = err
			return false, true
		}
	default:
		return false, false
	}
	value, err := prefixRenditionLinesContext(output.ctx, value, firstPrefix, continuationPrefix)
	if err != nil {
		output.err = err
		return false, true
	}
	if output.runes+utf8.RuneCountInString(value) > limit {
		return false, true
	}
	output.WriteString(value)
	fallback.mark(output)
	return true, false
}

func prefixRenditionLines(value, firstPrefix, continuationPrefix string) string {
	result, _ := prefixRenditionLinesContext(context.Background(), value, firstPrefix, continuationPrefix)
	return result
}

func prefixRenditionLinesContext(ctx context.Context, value, firstPrefix, continuationPrefix string) (string, error) {
	var result strings.Builder
	result.WriteString(firstPrefix)
	for index := range len(value) {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		result.WriteByte(value[index])
		if value[index] == '\n' {
			result.WriteString(continuationPrefix)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return result.String(), nil
}

func serializeRenditionInlines(inlines []renditionInline, available int, inTable bool) (string, bool) {
	value, truncated, _ := serializeRenditionInlinesContext(context.Background(), inlines, available, inTable)
	return value, truncated
}

func serializeRenditionInlinesContext(ctx context.Context, inlines []renditionInline, available int, inTable bool) (string, bool, error) {
	output := renditionBuffer{ctx: ctx}
	truncated := appendRenditionInlines(&output, inlines, available, inTable)
	if output.err != nil {
		return "", false, output.err
	}
	return output.String(), truncated, nil
}

type renditionBuffer struct {
	bytes []byte
	runes int
	ctx   context.Context
	err   error
}

func (b *renditionBuffer) contextError() error {
	if b.err != nil {
		return b.err
	}
	if b.ctx != nil {
		b.err = b.ctx.Err()
	}
	return b.err
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
	_ = f.markEscapedTextContext(context.Background(), start, value)
}

func (f *renditionBufferFallback) markEscapedTextContext(
	ctx context.Context,
	start renditionBufferCheckpoint,
	value string,
) error {
	if f == nil || start.runes >= f.limit {
		return nil
	}
	prefix, err := truncateEscapedRenditionTextContext(ctx, value, f.limit-start.runes)
	if err != nil {
		return err
	}
	if prefix == "" {
		return nil
	}
	f.checkpoint = renditionBufferCheckpoint{
		bytes: start.bytes + len(prefix),
		runes: start.runes + utf8.RuneCountInString(prefix),
	}
	f.valid = true
	return nil
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
		if output.contextError() != nil {
			return true
		}
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
			value, err := escapeRenditionTextContext(output.ctx, inline.text)
			if err != nil {
				output.err = err
				return true
			}
			if available >= 0 && utf8.RuneCountInString(value) > remaining {
				value, err = truncateEscapedRenditionTextContext(output.ctx, inline.text, max(0, remaining))
				if err != nil {
					output.err = err
					return true
				}
				output.WriteString(value)
				if err := fallback.markEscapedTextContext(output.ctx, textStart, inline.text); err != nil {
					output.err = err
					return true
				}
				return true
			}
			output.WriteString(value)
			if err := fallback.markEscapedTextContext(output.ctx, textStart, inline.text); err != nil {
				output.err = err
				return true
			}
		case renditionInlineCode:
			value, err := serializeRenditionInlineCodeContext(output.ctx, inline.text, inTable)
			if err != nil {
				output.err = err
				return true
			}
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
		if output.contextError() != nil {
			return true
		}
		remaining := available
		if available >= 0 {
			remaining -= output.runes - startRunes
		}
		switch inline.kind {
		case renditionText, renditionInlineCode:
			textStart := output.checkpoint()
			value, err := escapeRenditionTextContext(output.ctx, inline.text)
			if err != nil {
				output.err = err
				return true
			}
			if available >= 0 && utf8.RuneCountInString(value) > remaining {
				value, err = truncateEscapedRenditionTextContext(output.ctx, inline.text, max(0, remaining))
				if err != nil {
					output.err = err
					return true
				}
				output.WriteString(value)
				if err := fallback.markEscapedTextContext(output.ctx, textStart, inline.text); err != nil {
					output.err = err
					return true
				}
				return true
			}
			output.WriteString(value)
			if err := fallback.markEscapedTextContext(output.ctx, textStart, inline.text); err != nil {
				output.err = err
				return true
			}
		case renditionLinkInline:
			if appendRenditionPlainLabel(output, inline.children, remaining, fallback) {
				return true
			}
		}
	}
	return false
}

func escapeRenditionText(value string) string {
	result, _ := escapeRenditionTextContext(context.Background(), value)
	return result
}

func escapeRenditionTextContext(ctx context.Context, value string) (string, error) {
	var output strings.Builder
	runes := 0
	for _, character := range value {
		if runes&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		runes++
		if isMarkdownASCIIPunctuation(character) {
			output.WriteByte('\\')
		}
		output.WriteRune(character)
	}
	return output.String(), nil
}

func truncateEscapedRenditionText(value string, limit int) string {
	result, _ := truncateEscapedRenditionTextContext(context.Background(), value, limit)
	return result
}

func truncateEscapedRenditionTextContext(ctx context.Context, value string, limit int) (string, error) {
	var output strings.Builder
	used := 0
	runes := 0
	for _, character := range value {
		if runes&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		runes++
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
	return output.String(), nil
}

func isMarkdownASCIIPunctuation(character rune) bool {
	return character >= '!' && character <= '/' || character >= ':' && character <= '@' ||
		character >= '[' && character <= '`' || character >= '{' && character <= '~'
}

func serializeRenditionInlineCodeContext(ctx context.Context, content string, inTable bool) (string, error) {
	content = strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", " "), "\n", " ")
	if inTable {
		content = strings.ReplaceAll(content, "|", "\\|")
	}
	backticks, err := maxBacktickRunContext(ctx, content)
	if err != nil {
		return "", err
	}
	fence := strings.Repeat("`", backticks+1)
	if strings.HasPrefix(content, "`") || strings.HasSuffix(content, "`") {
		content = " " + content + " "
	}
	return fence + content + fence, nil
}

func serializeRenditionCodeBlockContext(ctx context.Context, language, content string) (string, error) {
	backticks, err := maxBacktickRunContext(ctx, content)
	if err != nil {
		return "", err
	}
	fence := strings.Repeat("`", max(3, backticks+1))
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return fence + language + "\n" + content + fence, nil
}

func serializeRenditionTable(ctx context.Context, rows [][][]renditionInline) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	columns := 0
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		columns = max(columns, len(row))
	}
	if columns == 0 {
		return "", nil
	}
	format := func(row [][]renditionInline) (string, error) {
		cells := make([]string, columns)
		for index, cell := range row {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			value, _, err := serializeRenditionInlinesContext(ctx, cell, -1, true)
			if err != nil {
				return "", err
			}
			cells[index] = value
		}
		return "| " + strings.Join(cells, " | ") + " |", nil
	}
	delimiter := make([]string, columns)
	for index := range delimiter {
		delimiter[index] = "---"
	}
	first, err := format(rows[0])
	if err != nil {
		return "", err
	}
	lines := []string{first, "| " + strings.Join(delimiter, " | ") + " |"}
	for _, row := range rows[1:] {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line, err := format(row)
		if err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

type canonicalHTMLWriter struct {
	output       strings.Builder
	maxLinkChars int
	inPre        bool
	skipTag      string
	skipDepth    int
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
	if tag == "svg" {
		if !selfClosing {
			w.skipTag = tag
			w.skipDepth = 1
		}
		return
	}
	switch tag {
	case "table":
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
			w.flushPendingSpace()
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
		w.line()
	case "table":
		w.block()
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

func maxBacktickRunContext(ctx context.Context, value string) (int, error) {
	maximum := 0
	current := 0
	runes := 0
	for _, character := range value {
		if runes&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		runes++
		if character == '`' {
			current++
			maximum = max(maximum, current)
		} else {
			current = 0
		}
	}
	return maximum, nil
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
	cleaned, _ := stripUnsafeControlsContext(context.Background(), value)
	return cleaned
}

func stripUnsafeControlsContext(ctx context.Context, value string) (string, error) {
	var cleaned strings.Builder
	cleaned.Grow(len(value))
	iterations := 0
	for index := 0; index < len(value); {
		if iterations&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		iterations++
		if value[index] == '\r' {
			cleaned.WriteByte('\n')
			index++
			if index < len(value) && value[index] == '\n' {
				index++
			}
			continue
		}
		character, size := utf8.DecodeRuneInString(value[index:])
		index += size
		switch character {
		case '\n', '\t':
			cleaned.WriteRune(character)
		case '\f', '\v', '\u0085', '\u2028', '\u2029':
			cleaned.WriteByte('\n')
		default:
			if unicode.IsSpace(character) {
				cleaned.WriteByte(' ')
			} else if !unicode.IsControl(character) && character != headingSentinelStart && character != headingSentinelEnd {
				cleaned.WriteRune(character)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return cleaned.String(), nil
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
