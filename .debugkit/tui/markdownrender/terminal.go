package markdownrender

import (
	"fmt"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	extast "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/text"

	"go.kenn.io/kit/tui/termtext"
)

const (
	ansiBoldOn       = "\x1b[1m"
	ansiBoldOff      = "\x1b[22m"
	ansiItalicOn     = "\x1b[3m"
	ansiItalicOff    = "\x1b[23m"
	ansiUnderlineOn  = "\x1b[4m"
	ansiUnderlineOff = "\x1b[24m"
	ansiStrikeOn     = "\x1b[9m"
	ansiStrikeOff    = "\x1b[29m"
)

type inlineStyle uint8

const (
	inlineBold inlineStyle = 1 << iota
	inlineItalic
	inlineUnderline
	inlineStrike
)

type terminalRenderer struct {
	source []byte
	opts   Options
}

type listItemLine struct {
	text            string
	hasParentIndent bool
}

func renderMarkdownDocument(markdown string, opts Options) (string, error) {
	source := []byte(markdown)
	p := parser.New(parser.WithExtensions(
		extension.GFMParser,
		extension.DefinitionListParser,
	))
	document := p.Parse(source)
	renderer := terminalRenderer{source: source, opts: opts}
	out := strings.TrimRight(renderer.renderBlocks(document), "\n")
	if out == "" {
		return "", nil
	}
	return out + "\n", nil
}

func (r terminalRenderer) renderBlocks(parent ast.Node) string {
	var out strings.Builder
	var previous ast.Node
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		block := r.renderBlock(node)
		block = strings.TrimRight(block, "\n")
		if block != "" {
			if out.Len() > 0 {
				separator := "\n\n"
				if _, ok := previous.(*ast.Heading); ok {
					separator = "\n"
				}
				out.WriteString(separator)
			}
			out.WriteString(block)
			previous = node
		}
	}
	return out.String()
}

func (r terminalRenderer) renderBlock(node ast.Node) string {
	switch node := node.(type) {
	case *ast.Heading:
		return ansiBoldOn + r.renderInlinesStyled(node, inlineBold) + ansiBoldOff
	case *ast.Paragraph:
		return r.wrap(r.renderInlines(node), r.opts.Width)
	case *ast.CodeBlock:
		return r.renderCodeBlock(node.Value)
	case *ast.HTMLBlock:
		return r.renderHTMLBlock(node)
	case *ast.Blockquote:
		const prefix = "| "
		quoted := r
		quoted.opts.Width = max(1, r.opts.Width-ansi.StringWidth(prefix))
		return prefixLines(
			hardWrapLines(quoted.renderBlocks(node), quoted.opts.Width), prefix,
		)
	case *ast.List:
		return r.renderList(node, 0)
	case *extast.Table:
		return r.renderTable(node)
	case *extast.DefinitionList:
		return r.renderDefinitionList(node)
	case *ast.ThematicBreak:
		return ""
	default:
		if node.HasChildren() {
			return r.renderBlocks(node)
		}
		return ""
	}
}

func (r terminalRenderer) renderList(list *ast.List, indentWidth int) string {
	lines := make([]string, 0, list.ChildCount())
	itemNumber := list.Start
	for child := list.FirstChild(); child != nil; child = child.NextSibling() {
		item, ok := child.(*ast.ListItem)
		if !ok {
			continue
		}
		marker := "- "
		if list.IsOrdered() {
			marker = fmt.Sprintf("%d. ", itemNumber)
			itemNumber++
		}
		taskMarker := taskListMarker(item)
		if taskMarker != "" {
			if list.IsOrdered() {
				marker += taskMarker
			} else {
				marker = taskMarker
			}
		}
		indent := strings.Repeat(" ", indentWidth)
		continuationWidth := indentWidth + ansi.StringWidth(marker)
		continuation := strings.Repeat(" ", continuationWidth)
		itemLines := r.renderListItem(
			item,
			continuationWidth,
			max(1, r.opts.Width-continuationWidth),
			!list.IsTight,
		)
		if len(itemLines) == 0 {
			lines = append(lines, indent+marker)
			continue
		}
		lines = append(lines, indent+marker+itemLines[0].text)
		for _, line := range itemLines[1:] {
			if line.text == "" {
				lines = append(lines, "")
			} else if line.hasParentIndent {
				lines = append(lines, line.text)
			} else {
				lines = append(lines, continuation+line.text)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func taskListMarker(item *ast.ListItem) string {
	status, ok := extension.TaskStatusOf(item)
	if !ok {
		return ""
	}
	if status == extension.TaskStatusCompleted {
		return "[x] "
	}
	return "[ ] "
}

func (r terminalRenderer) renderListItem(
	item *ast.ListItem,
	nestedIndent, width int,
	separateBlocks bool,
) []listItemLine {
	var lines []listItemLine
	for child := item.FirstChild(); child != nil; child = child.NextSibling() {
		var childLines []string
		hasParentIndent := false
		switch child := child.(type) {
		case *ast.Paragraph:
			content := r.renderInlines(child)
			paragraph := r.wrap(content, width)
			childLines = strings.Split(paragraph, "\n")
		case *ast.List:
			nested := r.renderList(child, nestedIndent)
			childLines = strings.Split(nested, "\n")
			hasParentIndent = true
		default:
			blockRenderer := r
			blockRenderer.opts.Width = width
			block := blockRenderer.renderBlock(child)
			if block != "" {
				block = hardWrapLines(block, width)
				childLines = strings.Split(block, "\n")
			}
		}
		if len(childLines) == 0 {
			continue
		}
		if separateBlocks && len(lines) > 0 {
			lines = append(lines, listItemLine{})
		}
		for _, line := range childLines {
			lines = append(lines, listItemLine{
				text:            line,
				hasParentIndent: hasParentIndent,
			})
		}
	}
	return lines
}

func (r terminalRenderer) renderCodeBlock(value text.Lines) string {
	raw := strings.TrimSuffix(value.Str(r.source), "\n")
	if r.opts.CodeBlockBackground == nil || *r.opts.CodeBlockBackground == "" {
		return raw
	}
	prefix := "\x1b[48;5;" + *r.opts.CodeBlockBackground + "m"
	lines := strings.Split(raw, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i] + "\x1b[49m"
	}
	return strings.Join(lines, "\n")
}

func (r terminalRenderer) renderHTMLBlock(block *ast.HTMLBlock) string {
	return stripHTMLTags(block.Value.Str(r.source))
}

func stripHTMLTags(value string) string {
	var out strings.Builder
	for index := 0; index < len(value); {
		if tagLength, ok := htmlTagLength(value[index:]); ok {
			index += tagLength
			continue
		}
		out.WriteByte(value[index])
		index++
	}
	return strings.TrimSpace(unescapeTerminalText(out.String()))
}

func htmlTagLength(value string) (int, bool) {
	if len(value) < 3 || value[0] != '<' {
		return 0, false
	}
	switch value[1] {
	case '/':
		if len(value) < 4 || !isASCIIAlpha(value[2]) {
			return 0, false
		}
	case '!', '?':
	default:
		if !isASCIIAlpha(value[1]) {
			return 0, false
		}
	}
	var quote byte
	for index := 2; index < len(value); index++ {
		char := value[index]
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case '>':
			return index + 1, true
		}
	}
	return 0, false
}

func isASCIIAlpha(char byte) bool {
	return char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z'
}

func (r terminalRenderer) renderTable(tbl *extast.Table) string {
	var headers []string
	var rows [][]string
	renderCells := func(row ast.Node) []string {
		cells := make([]string, 0, row.ChildCount())
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, r.renderInlines(cell))
		}
		return cells
	}
	for child := tbl.FirstChild(); child != nil; child = child.NextSibling() {
		switch child := child.(type) {
		case *extast.TableHeader:
			headers = renderCells(child)
		case *extast.TableBody:
			for row := child.FirstChild(); row != nil; row = row.NextSibling() {
				rows = append(rows, renderCells(row))
			}
		}
	}
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		Headers(headers...).
		Rows(rows...)
	return t.String()
}

func (r terminalRenderer) renderDefinitionList(list *extast.DefinitionList) string {
	parts := make([]string, 0, list.ChildCount())
	for child := list.FirstChild(); child != nil; child = child.NextSibling() {
		var part string
		switch child := child.(type) {
		case *extast.DefinitionTerm:
			part = r.renderInlines(child)
		case *extast.DefinitionDescription:
			part = r.renderBlocks(child)
		}
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n")
}

func (r terminalRenderer) renderInlines(parent ast.Node) string {
	return r.renderInlinesStyled(parent, 0)
}

func (r terminalRenderer) renderInlinesStyled(parent ast.Node, active inlineStyle) string {
	var out strings.Builder
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		switch node := node.(type) {
		case *ast.Text:
			out.WriteString(termtext.SanitizeBlock(node.Value.Value(r.source)))
			if node.HardLineBreak() || node.SoftLineBreak() {
				out.WriteByte('\n')
			}
		case *ast.CodeSpan:
			out.WriteString("`" + node.Value.Value(r.source) + "`")
		case *ast.Strong:
			out.WriteString(r.renderInlineStyle(
				node, active, inlineBold, ansiBoldOn, ansiBoldOff,
			))
		case *ast.Emphasis:
			out.WriteString(r.renderInlineStyle(
				node, active, inlineItalic, ansiItalicOn, ansiItalicOff,
			))
		case *ast.Link:
			out.WriteString(r.renderInlinesStyled(node, active))
			if destination := visibleLinkDestination(node.Destination.Value(r.source)); destination != "" {
				out.WriteByte(' ')
				out.WriteString(ansiUnderlineOn + destination + ansiUnderlineOff)
			}
		case *ast.Image:
			out.WriteString("[image: " + r.renderInlinesStyled(node, active) + "]")
			if destination := termtext.SanitizeBlock(node.Destination.Value(r.source)); destination != "" {
				out.WriteByte(' ')
				out.WriteString(destination)
			}
		case *ast.AutoLink:
			label := termtext.SanitizeBlock(node.Label.Value(r.source))
			if strings.HasPrefix(node.Destination.Value(r.source), "mailto:") {
				out.WriteString(label)
			} else {
				out.WriteString(ansiUnderlineOn + label + ansiUnderlineOff)
			}
		case *ast.RawHTML:
			out.WriteString(r.renderRawHTML(node))
		case *extast.Strikethrough:
			out.WriteString(r.renderInlineStyle(
				node, active, inlineStrike, ansiStrikeOn, ansiStrikeOff,
			))
		default:
			if node.HasChildren() {
				out.WriteString(r.renderInlinesStyled(node, active))
			}
		}
	}
	return out.String()
}

func (r terminalRenderer) renderRawHTML(node *ast.RawHTML) string {
	value := node.Value.Str(r.source)
	trimmed := strings.TrimSpace(value)
	if len(trimmed) >= 3 && trimmed[0] == '<' && trimmed[len(trimmed)-1] == '>' {
		fields := strings.Fields(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
		if len(fields) > 0 && strings.EqualFold(strings.TrimSuffix(fields[0], "/"), "br") {
			return "\n"
		}
	}
	return stripHTMLTags(value)
}

func visibleLinkDestination(destination string) string {
	value := termtext.SanitizeBlock(destination)
	if strings.HasPrefix(value, "#") {
		return ""
	}
	return value
}

func (r terminalRenderer) renderInlineStyle(
	node ast.Node,
	active, style inlineStyle,
	on, off string,
) string {
	if active&style != 0 {
		return r.renderInlinesStyled(node, active)
	}
	return on + r.renderInlinesStyled(node, active|style) + off
}

func (r terminalRenderer) wrap(value string, width int) string {
	width = max(1, width)
	return ansi.Hardwrap(ansi.Wordwrap(value, width, ""), width, true)
}

func hardWrapLines(value string, width int) string {
	width = max(1, width)
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		wrapped = append(wrapped, strings.Split(ansi.Hardwrap(line, width, true), "\n")...)
	}
	return strings.Join(wrapped, "\n")
}

func prefixLines(value, prefix string) string {
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	for i := range lines {
		if lines[i] == "" {
			lines[i] = strings.TrimRight(prefix, " ")
		} else {
			lines[i] = prefix + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}
