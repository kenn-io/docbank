package notemarkdown

import (
	"bytes"
	"context"
)

type sourceBlock struct {
	span                      Span
	content                   Span
	depth, level              int
	root                      bool
	paragraph, paragraphStart bool
	continuation              bool
}

// Containers own fenced/raw blocks. Dropping an enclosing quote/list ends
// its unclosed child block before the next physical line is classified.
type blockContainer struct {
	kind   byte // q quote, l list item
	marker byte // list delimiter; siblings need not start at 1
	indent int  // list content indentation relative to its parent
}

func scanBlocks(ctx context.Context, original []byte, bodyStart int, b *budget) ([]sourceBlock, error) {
	var blocks []sourceBlock
	var containers []blockContainer
	fenceChar := byte(0)
	fenceWidth := 0
	blockDepth := 0
	htmlEnd := ""
	inHTML := false
	paragraph := false
	paragraphFirst := -1
	nonDefinitions := map[int]bool{}
	probes := map[int]*definitionProbe{}
	for pos := bodyStart; pos < len(original); {
		line, next, err := sourceLineAt(ctx, original, pos, b)
		if err != nil {
			return nil, err
		}
		raw := original[line.Start:line.End]
		content := 0
		// A partially consumed tab can leave spaces before the next byte.
		virtualIndent := 0
		if paragraph && len(blocks) > 0 && blocks[len(blocks)-1].span.End == line.Start {
			check, candidate, indent, err := definitionBoundary(ctx, raw, containers, b)
			if err != nil {
				return nil, err
			}
			if check {
				only, err := paragraphDefinitionsOnly(ctx, original, blocks, paragraphFirst, nonDefinitions, probes, b)
				if err != nil {
					return nil, err
				}
				if only {
					title := false
					if indent < 4 && candidate < len(raw) && (raw[candidate] == '\'' || raw[candidate] == '"' || raw[candidate] == '(') {
						scanner := linkScanner{ctx: ctx, src: original, b: b}
						_, title, err = scanner.title(line.Start+candidate, len(original))
						if err != nil {
							return nil, err
						}
					}
					if !title {
						paragraph = false
					}
				}
			}
		}
		matched := 0
		lazy := false
		for matched < len(containers) {
			c := containers[matched]
			remainingBlank := len(bytes.Trim(raw[content:], " \t")) == 0
			if c.kind == 'q' {
				end, columns, err := leadingIndent(ctx, raw, content, b)
				if err != nil {
					return nil, err
				}
				columns += virtualIndent
				if columns <= 3 && end < len(raw) && raw[end] == '>' {
					content, virtualIndent, err = quoteSpace(ctx, raw, end+1, b)
					if err != nil {
						return nil, err
					}
				} else if interrupt, err := blockInterrupt(ctx, raw[content:], 0, b); err != nil {
					return nil, err
				} else if paragraph && !remainingBlank && !interrupt {
					lazy = true
					matched = len(containers)
					break
				} else {
					break
				}
			} else {
				if remainingBlank {
					matched++
					continue
				}
				end, columns, err := leadingIndent(ctx, raw, content, b)
				if err != nil {
					return nil, err
				}
				columns += virtualIndent
				if columns >= c.indent {
					used := min(virtualIndent, c.indent)
					virtualIndent -= used
					column, err := visualColumn(ctx, raw, content, b)
					if err != nil {
						return nil, err
					}
					for content < end && used < c.indent {
						if raw[content] == '\t' {
							width := 4 - column%4
							used += width
							column += width
						} else {
							used++
							column++
						}
						content++
					}
					virtualIndent += used - c.indent
				} else if interrupt, err := blockInterrupt(ctx, raw[content:], c.marker, b); err != nil {
					return nil, err
				} else if paragraph && !interrupt {
					lazy = true
					matched = len(containers)
					break
				} else {
					break
				}
			}
			matched++
		}
		if matched < len(containers) {
			containers = containers[:matched]
			paragraph = false
			if blockDepth > matched {
				fenceChar = 0
				inHTML = false
				htmlEnd = ""
			}
		}
		blank := len(bytes.Trim(raw[content:], " \t")) == 0
		if blank {
			if fenceChar != 0 {
				pos = next
				continue
			}
			if inHTML && htmlEnd == "" {
				inHTML = false
			}
			paragraph = false
			pos = next
			continue
		}
		// Reference definitions alone do not keep a paragraph open for block
		// interruption, including inside a container.
		if paragraph && !lazy && fenceChar == 0 && !inHTML && len(blocks) > 0 {
			previous := &blocks[len(blocks)-1]
			start, indent, err := leadingIndent(ctx, raw, content, b)
			if err != nil {
				return nil, err
			}
			_, list := listMarker(raw, start)
			_, html, err := htmlBlockStart(ctx, bytes.Trim(raw[start:], " \t"), true, b)
			if err != nil {
				return nil, err
			}
			if (list || html) && indent+virtualIndent <= 3 && previous.paragraph && previous.depth == len(containers) && previous.span.End == line.Start {
				only, err := paragraphDefinitionsOnly(ctx, original, blocks, paragraphFirst, nonDefinitions, probes, b)
				if err != nil {
					return nil, err
				}
				if only {
					paragraph = false
				}
			}
		}
		if fenceChar == 0 && !inHTML && !lazy {
			for {
				end, columns, err := leadingIndent(ctx, raw, content, b)
				if err != nil {
					return nil, err
				}
				columns += virtualIndent
				if columns >= 4 {
					break
				}
				var c blockContainer
				after := end
				if end < len(raw) && raw[end] == '>' {
					c.kind = 'q'
					after, virtualIndent, err = quoteSpace(ctx, raw, after+1, b)
					if err != nil {
						return nil, err
					}
				} else if markerEnd, ok := listMarker(raw, end); ok && (!paragraph || listCanInterrupt(raw, end, markerEnd)) {
					candidate := bytes.Trim(raw[end:], " \t")
					setext, err := setextLevel(ctx, candidate, b)
					if err != nil {
						return nil, err
					}
					thematic, err := thematicBreak(ctx, candidate, b)
					if err != nil {
						return nil, err
					}
					if thematic || paragraph && setext > 0 {
						break
					}
					c.kind = 'l'
					c.marker = raw[markerEnd-1]
					after = markerEnd
					virtualIndent = 0
					padEnd, padding, err := leadingIndent(ctx, raw, after, b)
					if err != nil {
						return nil, err
					}
					if padding == 0 {
						padding = 1
					} else if padding > 4 {
						if raw[after] == '\t' {
							column, err := visualColumn(ctx, raw, after, b)
							if err != nil {
								return nil, err
							}
							virtualIndent = 3 - column%4
						}
						padding = 1
						padEnd = after + 1
					}
					c.indent = columns + markerEnd - end + padding
					after = padEnd
				} else {
					break
				}
				if len(containers) >= maxDepth {
					return nil, &LimitError{Resource: "ast_depth"}
				}
				if err := b.node(ctx, len(containers)+1); err != nil {
					return nil, err
				}
				containers = append(containers, c)
				content = after
				paragraph = false
			}
		}
		if len(bytes.Trim(raw[content:], " \t")) == 0 {
			paragraph = false
			pos = next
			continue
		}
		depth := len(containers)
		root := depth == 0
		if err := b.node(ctx, depth+1); err != nil {
			return nil, err
		}
		text := raw[content:]
		trimmed := bytes.Trim(text, " \t")
		if inHTML {
			if htmlEnd != "" {
				matched, err := htmlEndMatches(ctx, text, htmlEnd, b)
				if err != nil {
					return nil, err
				}
				if matched {
					inHTML = false
					htmlEnd = ""
				}
			}
			pos = next
			continue
		}
		if fenceChar != 0 {
			_, indent, err := leadingIndent(ctx, raw, content, b)
			if err != nil {
				return nil, err
			}
			n, err := byteRun(ctx, trimmed, fenceChar, b)
			if err != nil {
				return nil, err
			}
			if indent+virtualIndent <= 3 && n >= fenceWidth && len(bytes.Trim(trimmed[n:], " \t")) == 0 {
				fenceChar = 0
			}
			pos = next
			continue
		}
		prefixEnd := content
		localStart, indent, err := leadingIndent(ctx, raw, content, b)
		if err != nil {
			return nil, err
		}
		indent += virtualIndent
		if indent >= 4 && !paragraph {
			pos = next
			continue
		}
		if indent <= 3 {
			content = localStart
			text = raw[content:]
			trimmed = bytes.Trim(text, " \t")
		}
		if indent <= 3 {
			marker, n, err := fenceOpener(ctx, trimmed, b)
			if err != nil {
				return nil, err
			}
			if marker != 0 {
				fenceChar, fenceWidth, blockDepth = marker, n, depth
				paragraph = false
				pos = next
				continue
			}
		}
		genericHTML := !paragraph
		end, yes, err := htmlBlockStart(ctx, trimmed, genericHTML, b)
		if err != nil {
			return nil, err
		}
		if yes && indent <= 3 {
			inHTML, htmlEnd, blockDepth = true, end, depth
			paragraph = false
			if end != "" {
				matched, err := htmlEndMatches(ctx, trimmed, end, b)
				if err != nil {
					return nil, err
				}
				if matched {
					inHTML = false
					htmlEnd = ""
				}
			}
			pos = next
			continue
		}
		level, headingStart, headingEnd, err := atxHeading(ctx, text, b)
		if err != nil {
			return nil, err
		}
		if level > 0 && indent <= 3 {
			block := sourceBlock{span: Span{line.Start, next}, content: Span{line.Start + content + headingStart, line.Start + content + headingEnd}, depth: depth, root: root}
			if root {
				block.level = level
			}
			blocks = append(blocks, block)
			paragraph = false
			pos = next
			continue
		}
		setext, err := setextLevel(ctx, trimmed, b)
		if err != nil {
			return nil, err
		}
		if paragraph && len(blocks) > 0 && setext > 0 && indent <= 3 {
			previous := &blocks[len(blocks)-1]
			if previous.depth == depth && previous.level == 0 && previous.span.End == line.Start {
				if root {
					headingStart, err := leadingDefinitionsEnd(ctx, original, previous.content, b)
					if err != nil {
						return nil, err
					}
					if headingStart == previous.content.Start {
						previous.paragraph = false
						previous.level = setext
						previous.span.End = next
					} else if headingStart < previous.content.End {
						end := previous.content.End
						previous.content.End = headingStart
						previous.span.End = headingStart
						blocks = append(blocks, sourceBlock{span: Span{headingStart, next}, content: Span{headingStart, end}, root: true, level: setext})
					} else {
						paragraph = false
					}
				}
				if paragraph {
					paragraph = false
					pos = next
					continue
				}
			}
		}
		thematic, err := thematicBreak(ctx, trimmed, b)
		if err != nil {
			return nil, err
		}
		if thematic && indent <= 3 {
			paragraph = false
			pos = next
			continue
		}
		block := sourceBlock{span: Span{line.Start, next}, content: Span{line.Start + content, line.End}, depth: depth, root: root, paragraph: true, paragraphStart: !paragraph}
		if paragraph && len(blocks) > 0 {
			previous := &blocks[len(blocks)-1]
			if previous.level == 0 && previous.root == root && previous.depth == depth && previous.span.End == line.Start {
				if prefixEnd == 0 {
					previous.span.End = next
					previous.content.End = line.End
					pos = next
					continue
				}
				block.continuation = true
			}
		}
		if !block.continuation {
			paragraphFirst = len(blocks)
		}
		blocks = append(blocks, block)
		paragraph = true
		pos = next
	}
	return blocks, nil
}

func definitionBoundary(ctx context.Context, raw []byte, containers []blockContainer, b *budget) (bool, int, int, error) {
	pos, virtual := 0, 0
	for _, container := range containers {
		end, columns, err := leadingIndent(ctx, raw, pos, b)
		if err != nil {
			return false, 0, 0, err
		}
		columns += virtual
		if container.kind == 'q' {
			if columns > 3 || end == len(raw) || raw[end] != '>' {
				return true, end, columns, nil
			}
			pos, virtual, err = quoteSpace(ctx, raw, end+1, b)
			if err != nil {
				return false, 0, 0, err
			}
			continue
		}
		if columns < container.indent {
			return true, end, columns, nil
		}
		used := min(virtual, container.indent)
		virtual -= used
		column, err := visualColumn(ctx, raw, pos, b)
		if err != nil {
			return false, 0, 0, err
		}
		for pos < end && used < container.indent {
			width := 1
			if raw[pos] == '\t' {
				width = 4 - column%4
			}
			used += width
			column += width
			pos++
		}
		virtual += used - container.indent
	}
	end, indent, err := leadingIndent(ctx, raw, pos, b)
	if err != nil {
		return false, 0, 0, err
	}
	indent += virtual
	_, list := listMarker(raw, end)
	return indent >= 4 || list || end < len(raw) && raw[end] == '<' || len(containers) > 0 && end == len(raw), end, indent, nil
}

func paragraphDefinitionsOnly(ctx context.Context, src []byte, blocks []sourceBlock, first int, nonDefinitions map[int]bool, probes map[int]*definitionProbe, b *budget) (bool, error) {
	last := len(blocks) - 1
	if first < 0 || first > last || !blocks[last].paragraph {
		return false, nil
	}
	key := blocks[first].span.Start
	if nonDefinitions[key] {
		return false, nil
	}
	if blocks[first].content.Start >= len(src) || src[blocks[first].content.Start] != '[' {
		nonDefinitions[key] = true
		return false, nil
	}
	probe := probes[key]
	if probe == nil {
		probe = &definitionProbe{block: first, offset: blocks[first].content.Start + 1}
		probes[key] = probe
	}
	for !probe.closed && probe.block <= last {
		block := blocks[probe.block]
		probe.offset = max(probe.offset, block.content.Start)
		for probe.offset < block.content.End {
			if err := b.step(ctx, 1); err != nil {
				return false, err
			}
			c := src[probe.offset]
			probe.offset++
			probe.bytes++
			if probe.bytes > 4*999 {
				nonDefinitions[key] = true
				return false, nil
			}
			if probe.escaped {
				probe.escaped = false
				continue
			}
			if c == '\\' {
				probe.escaped = true
				continue
			}
			if c == '[' {
				nonDefinitions[key] = true
				return false, nil
			}
			if c == ']' {
				probe.closed = true
				break
			}
		}
		if !probe.closed && probe.block < last {
			probe.block++
			probe.offset = blocks[probe.block].content.Start
			probe.escaped = false
		} else {
			break
		}
	}
	if !probe.closed {
		return false, nil
	}
	if first == last {
		end, err := leadingDefinitionsEnd(ctx, src, blocks[last].content, b)
		if err == nil && end < blocks[last].content.End && !couldBeDefinition(src[end:blocks[last].content.End]) {
			nonDefinitions[key] = true
		}
		return end >= blocks[last].content.End, err
	}
	start, end := blocks[first].span.Start, blocks[last].span.End
	rebased := make([]sourceBlock, 0, last-first+1)
	for _, block := range blocks[first : last+1] {
		if err := b.node(ctx, 1); err != nil {
			return false, err
		}
		block.span.Start -= start
		block.span.End -= start
		block.content.Start -= start
		block.content.End -= start
		rebased = append(rebased, block)
	}
	view, joined, _, err := prepareLinkSource(ctx, src[start:end], rebased, b)
	if err != nil {
		return false, err
	}
	content := joined[len(joined)-1].content
	position, err := leadingDefinitionsEnd(ctx, view, content, b)
	if err == nil && position < content.End && !couldBeDefinition(view[position:content.End]) {
		nonDefinitions[key] = true
	}
	return position >= content.End, err
}

type definitionProbe struct {
	block, offset, bytes int
	closed, escaped      bool
}

func couldBeDefinition(text []byte) bool {
	text = bytes.TrimLeft(text, " \t\r\n")
	return len(text) > 0 && text[0] == '[' && possibleReferenceDefinition(text)
}

// An unfinished label may become a definition on a continuation line.
func possibleReferenceDefinition(text []byte) bool {
	limit := min(len(text), 4*999+1)
	for i := 1; i < limit; i++ {
		if text[i] == '\\' && i+1 < limit {
			i++
			continue
		}
		if text[i] == '[' {
			return false
		}
		if text[i] == ']' {
			return i+1 == len(text) || text[i+1] == ':'
		}
	}
	return len(text) <= 4*999
}

func quoteSpace(ctx context.Context, raw []byte, pos int, b *budget) (int, int, error) {
	if pos >= len(raw) || raw[pos] != ' ' && raw[pos] != '\t' {
		return pos, 0, nil
	}
	if raw[pos] == ' ' {
		return pos + 1, 0, nil
	}
	column, err := visualColumn(ctx, raw, pos, b)
	if err != nil {
		return 0, 0, err
	}
	return pos + 1, 3 - column%4, nil
}

// Leading reference definitions belong to the paragraph, not its heading.
func leadingDefinitionsEnd(ctx context.Context, src []byte, content Span, b *budget) (int, error) {
	scanner := linkScanner{ctx: ctx, src: src, b: b, refs: map[string]referenceDefinition{}}
	pos := content.Start
	for pos < content.End {
		line, next, err := sourceLineAt(ctx, src, pos, b)
		if err != nil {
			return 0, err
		}
		line.End = min(line.End, content.End)
		end, ok, err := scanner.definition(line, content.End)
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		pos = max(next, definitionNext(src, end))
	}
	return pos, nil
}

func leadingIndent(ctx context.Context, raw []byte, start int, b *budget) (end, columns int, err error) {
	end = start
	column, err := visualColumn(ctx, raw, start, b)
	if err != nil {
		return 0, 0, err
	}
	for end < len(raw) && (raw[end] == ' ' || raw[end] == '\t') {
		if err := b.step(ctx, 1); err != nil {
			return 0, 0, err
		}
		if raw[end] == '\t' {
			width := 4 - column%4
			columns += width
			column += width
		} else {
			columns++
			column++
		}
		end++
	}
	return
}

// Container prefixes contain only ASCII markers and whitespace. Count their
// columns so tabs after a marker use the tab stop of the original line.
func visualColumn(ctx context.Context, raw []byte, end int, b *budget) (int, error) {
	column := 0
	for i := range end {
		if err := b.step(ctx, 1); err != nil {
			return 0, err
		}
		if raw[i] == '\t' {
			column += 4 - column%4
		} else {
			column++
		}
	}
	return column, nil
}

func listMarker(raw []byte, start int) (int, bool) {
	if start >= len(raw) {
		return 0, false
	}
	end := start
	if raw[end] == '-' || raw[end] == '+' || raw[end] == '*' {
		end++
	} else {
		for end < len(raw) && end-start < 9 && raw[end] >= '0' && raw[end] <= '9' {
			end++
		}
		if end == start || end >= len(raw) || raw[end] != '.' && raw[end] != ')' {
			return 0, false
		}
		end++
	}
	return end, end == len(raw) || raw[end] == ' ' || raw[end] == '\t'
}

// Call only after listMarker validates the bounded (at most nine digits) marker.
// CommonMark's start number is numeric, so 01. may interrupt just like 1.
func listCanInterrupt(raw []byte, start, end int) bool {
	if len(bytes.Trim(raw[end:], " \t")) == 0 {
		return false
	}
	if raw[start] < '0' || raw[start] > '9' {
		return true
	}
	number := 0
	for _, digit := range raw[start : end-1] {
		number = number*10 + int(digit-'0')
	}
	return number == 1
}

func blockInterrupt(ctx context.Context, raw []byte, sibling byte, b *budget) (bool, error) {
	start, indent, err := leadingIndent(ctx, raw, 0, b)
	if err != nil {
		return false, err
	}
	if indent >= 4 {
		return false, nil
	}
	text := raw[start:]
	if len(text) == 0 {
		return true, nil
	}
	level, _, _, err := atxHeading(ctx, text, b)
	if err != nil {
		return false, err
	}
	thematic, err := thematicBreak(ctx, text, b)
	if err != nil {
		return false, err
	}
	if level > 0 || thematic || text[0] == '>' {
		return true, nil
	}
	if end, ok := listMarker(text, 0); ok && (text[end-1] == sibling || listCanInterrupt(text, 0, end)) {
		return true, nil
	}
	marker, _, err := fenceOpener(ctx, text, b)
	if err != nil {
		return false, err
	}
	if marker != 0 {
		return true, nil
	}
	_, html, err := htmlBlockStart(ctx, text, false, b)
	return html, err
}

func fenceOpener(ctx context.Context, text []byte, b *budget) (byte, int, error) {
	if len(text) == 0 || text[0] != '`' && text[0] != '~' {
		return 0, 0, nil
	}
	n, err := byteRun(ctx, text, text[0], b)
	if err != nil {
		return 0, 0, err
	}
	if n < 3 || text[0] == '`' && bytes.ContainsRune(text[n:], '`') {
		return 0, 0, nil
	}
	return text[0], n, nil
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
func atxHeading(ctx context.Context, text []byte, b *budget) (level, start, end int, err error) {
	// ATX openers have at most six markers; longer runs are ordinary text.
	for level < len(text) && level < 7 && text[level] == '#' {
		level++
	}
	if level < 1 || level > 6 || level < len(text) && text[level] != ' ' && text[level] != '\t' {
		return 0, 0, 0, nil
	}
	start = level
	for start < len(text) && (text[start] == ' ' || text[start] == '\t') {
		if err := b.step(ctx, 1); err != nil {
			return 0, 0, 0, err
		}
		start++
	}
	end = len(text)
	for end > start && (text[end-1] == ' ' || text[end-1] == '\t') {
		if err := b.step(ctx, 1); err != nil {
			return 0, 0, 0, err
		}
		end--
	}
	i := end
	for i > start && text[i-1] == '#' {
		if err := b.step(ctx, 1); err != nil {
			return 0, 0, 0, err
		}
		i--
	}
	if i < end && i >= start && i > level && (text[i-1] == ' ' || text[i-1] == '\t') {
		end = i
		for end > start && (text[end-1] == ' ' || text[end-1] == '\t') {
			if err := b.step(ctx, 1); err != nil {
				return 0, 0, 0, err
			}
			end--
		}
	}
	return level, start, end, nil
}
func setextLevel(ctx context.Context, text []byte, b *budget) (int, error) {
	if len(text) == 0 || text[0] != '=' && text[0] != '-' {
		return 0, nil
	}
	n, err := byteRun(ctx, text, text[0], b)
	if err != nil {
		return 0, err
	}
	if n != len(text) {
		return 0, nil
	}
	if text[0] == '=' {
		return 1, nil
	}
	return 2, nil
}
func thematicBreak(ctx context.Context, text []byte, b *budget) (bool, error) {
	if len(text) == 0 || text[0] != '-' && text[0] != '_' && text[0] != '*' {
		return false, nil
	}
	c := text[0]
	count := 0
	for _, r := range text {
		if err := b.step(ctx, 1); err != nil {
			return false, err
		}
		if r == c {
			count++
		} else if r != ' ' && r != '\t' {
			return false, nil
		}
	}
	return count >= 3, nil
}
