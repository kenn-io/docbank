package document

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/net/html"
)

// ErrRenditionXHTMLBudget means XHTML could not be normalized completely within its limits.
var ErrRenditionXHTMLBudget = errors.New("XHTML rendition exceeds its work or output limit")

// RenditionMarkdownFromXHTML converts a complete UTF-8 XML document to bounded Markdown.
func RenditionMarkdownFromXHTML(source []byte, maxRunes int) (string, error) {
	if maxRunes < 0 || len(source) > 100<<20 {
		return "", ErrRenditionXHTMLBudget
	}
	if !utf8.Valid(source) {
		return "", errors.New("XHTML must be UTF-8")
	}
	if err := checkRenditionXHTMLAttributeBound(source); err != nil {
		return "", err
	}
	maxRunes = min(maxRunes, maxEvidenceTextBytes)
	inlineAllocation := int64(unsafe.Sizeof(renditionInline{}))
	inlineAllowance := 4*inlineAllocation + 4
	budget := min(int64(100<<20), int64(len(source))+(int64(maxRunes)+1)*inlineAllowance)
	writer := renditionHTMLWriter{maxLinkChars: renditionMaxLinkChars, work: &renditionXHTMLWork{remaining: budget}}
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(source, []byte{0xef, 0xbb, 0xbf})))
	decoder.Entity = xml.HTMLEntity
	depth, roots, head := 0, 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
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
				if !writer.charge(int64(len(attr.Name.Local)) + int64(len(attr.Value)) + 2*int64(unsafe.Sizeof(html.Attribute{}))) {
					return "", ErrRenditionXHTMLBudget
				}
				converted.Attr = append(converted.Attr, html.Attribute{Key: attr.Name.Local, Val: attr.Value})
			}
			writer.startTag(converted, 0, false)
		case xml.EndElement:
			depth--
			if head > 0 {
				head--
				continue
			}
			writer.endTag(token.Name.Local)
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(token)) > 0 {
				return "", errors.New("XHTML has text outside its root")
			}
			if head == 0 && writer.skipDepth == 0 {
				if !writer.charge(int64(len(token))) {
					return "", ErrRenditionXHTMLBudget
				}
				writer.writeText(canonicalEvidenceString(string(token)))
			}
		}
		if writer.work.exceeded || writer.linkDepthTruncated {
			return "", ErrRenditionXHTMLBudget
		}
	}
	if roots != 1 || depth != 0 {
		return "", errors.New("XHTML document is incomplete")
	}
	writer.finalize()
	canonicalizeRenditionBlocks(writer.blocks)
	if writer.work.exceeded || !renditionXHTMLSerializationFits(writer.blocks, budget) {
		return "", ErrRenditionXHTMLBudget
	}
	text, truncated := serializeRenditionBlocks(writer.blocks, maxRunes)
	text = canonicalEvidenceString(text)
	if truncated || len(text) > maxEvidenceTextBytes || utf8.RuneCountInString(text) > maxRunes {
		return "", ErrRenditionXHTMLBudget
	}
	return text, nil
}

const maxRenditionXHTMLAttributes = 1 << 18

func checkRenditionXHTMLAttributeBound(source []byte) error {
	for index := 0; index < len(source); {
		if source[index] != '<' || index+1 >= len(source) {
			index++
			continue
		}
		if bytes.HasPrefix(source[index:], []byte("<!--")) {
			index += len("<!--")
			for index+2 < len(source) && !bytes.Equal(source[index:index+3], []byte("-->")) {
				index++
			}
			index += min(3, len(source)-index)
			continue
		}
		if bytes.HasPrefix(source[index:], []byte("<![CDATA[")) {
			index += len("<![CDATA[")
			for index+2 < len(source) && !bytes.Equal(source[index:index+3], []byte("]]>")) {
				index++
			}
			index += min(3, len(source)-index)
			continue
		}
		if source[index+1] == '?' {
			index += 2
			for index+1 < len(source) && (source[index] != '?' || source[index+1] != '>') {
				index++
			}
			index += min(2, len(source)-index)
			continue
		}
		if source[index+1] == '!' {
			if !bytes.HasPrefix(source[index:], []byte("<!DOCTYPE")) {
				return errors.New("unsupported XHTML directive")
			}
			index++
			quote := byte(0)
			for index < len(source) {
				if quote == 0 && bytes.HasPrefix(source[index:], []byte("<!--")) {
					index += len("<!--")
					for index+2 < len(source) && !bytes.Equal(source[index:index+3], []byte("-->")) {
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
				} else if character == '<' {
					return errors.New("XHTML DOCTYPE contains unsupported markup")
				} else if character == '[' {
					return errors.New("XHTML internal DTD subsets are unsupported")
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
				index++
			}
			index += min(1, len(source)-index)
			continue
		}

		index++
		attributes := 0
		quote := byte(0)
		for index < len(source) {
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

func canonicalizeRenditionBlocks(blocks []renditionBlock) {
	for index := range blocks {
		blocks[index].code = canonicalEvidenceString(blocks[index].code)
		blocks[index].inlines = canonicalizeRenditionInlines(blocks[index].inlines)
		for rowIndex := range blocks[index].rows {
			for cellIndex := range blocks[index].rows[rowIndex] {
				blocks[index].rows[rowIndex][cellIndex] = canonicalizeRenditionInlines(blocks[index].rows[rowIndex][cellIndex])
			}
		}
		if blocks[index].list == nil {
			continue
		}
		for itemIndex := range blocks[index].list.items {
			canonicalizeRenditionBlocks(blocks[index].list.items[itemIndex].blocks)
		}
	}
}

func canonicalizeRenditionInlines(values []renditionInline) []renditionInline {
	result := values[:0]
	var text strings.Builder
	flushText := func() {
		if text.Len() == 0 {
			return
		}
		result = append(result, renditionInline{kind: renditionText, text: canonicalEvidenceString(text.String())})
		text.Reset()
	}
	for _, value := range values {
		if value.kind == renditionText {
			text.WriteString(value.text)
			continue
		}
		flushText()
		if value.kind == renditionInlineCode {
			value.text = canonicalEvidenceString(value.text)
		} else {
			value.children = canonicalizeRenditionInlines(value.children)
		}
		result = append(result, value)
	}
	flushText()
	return result
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
func renditionXHTMLSerializationFits(blocks []renditionBlock, budget int64) bool {
	var inlines func([]renditionInline) int64
	inlines = func(values []renditionInline) int64 {
		var size int64
		for _, value := range values {
			size += int64(len(value.text))
			switch value.kind {
			case renditionText:
				for _, r := range value.text {
					if isMarkdownASCIIPunctuation(r) {
						size++
					}
				}
			case renditionInlineCode:
				size += 2*int64(maxBacktickRun(value.text)+1) + 4 + int64(strings.Count(value.text, "|"))
			case renditionLinkInline:
				size += int64(len(value.destination)) + 8 + inlines(value.children)
			}
		}
		return size
	}
	var cost func([]renditionBlock, int) int64
	cost = func(blocks []renditionBlock, indent int) int64 {
		var total int64
		for _, block := range blocks {
			total += 16 + int64(indent)*int64(3+strings.Count(block.code, "\n")+len(block.rows)) + inlines(block.inlines)
			if block.kind == renditionCodeBlock {
				total += int64(len(block.code) + len(block.language) + 3 + 2*max(3, maxBacktickRun(block.code)+1))
			}
			columns := 0
			for _, row := range block.rows {
				columns = max(columns, len(row))
				for _, cell := range row {
					total += inlines(cell)
				}
			}
			total += int64(len(block.rows)+1) * (int64(columns)*6 + 3)
			if block.list != nil {
				for _, item := range block.list.items {
					total += cost(item.blocks, indent+maxOrderedListMarkerDigits+4) + int64(len(block.list.start)) + 16
				}
			}
			if total > budget {
				return total
			}
		}
		return total
	}
	return cost(blocks, 0) <= budget
}
