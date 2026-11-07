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
	for pos := bodyStart; pos < len(original); {
		line, next, err := sourceLineAt(ctx, original, pos, b)
		if err != nil {
			return nil, err
		}
		raw := original[line.Start:line.End]
		blank := len(bytes.TrimSpace(raw)) == 0
		content := 0
		matched := 0
		lazy := false
		for matched < len(containers) {
			c := containers[matched]
			if c.kind == 'q' {
				end, columns, err := leadingIndent(ctx, raw, content, b)
				if err != nil {
					return nil, err
				}
				if columns <= 3 && end < len(raw) && raw[end] == '>' {
					content = end + 1
					if content < len(raw) && (raw[content] == ' ' || raw[content] == '\t') {
						content++
					}
				} else if interrupt, err := blockInterrupt(ctx, raw[content:], 0, b); err != nil {
					return nil, err
				} else if paragraph && !blank && !interrupt {
					lazy = true
					matched = len(containers)
					break
				} else {
					break
				}
			} else {
				if blank {
					matched++
					continue
				}
				end, columns, err := leadingIndent(ctx, raw, content, b)
				if err != nil {
					return nil, err
				}
				if columns >= c.indent {
					used := 0
					for content < end && used < c.indent {
						if raw[content] == '\t' {
							used += 4 - used%4
						} else {
							used++
						}
						content++
					}
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
		if blank {
			if inHTML && htmlEnd == "" {
				inHTML = false
			}
			paragraph = false
			pos = next
			continue
		}
		if fenceChar == 0 && !inHTML && !lazy {
			for {
				end, columns, err := leadingIndent(ctx, raw, content, b)
				if err != nil {
					return nil, err
				}
				if columns >= 4 {
					break
				}
				var c blockContainer
				after := end
				if end < len(raw) && raw[end] == '>' {
					c.kind = 'q'
					after++
					if after < len(raw) && (raw[after] == ' ' || raw[after] == '\t') {
						after++
					}
				} else if markerEnd, ok := listMarker(raw, end); ok && (!paragraph || listCanInterrupt(raw, end, markerEnd)) {
					c.kind = 'l'
					c.marker = raw[markerEnd-1]
					after = markerEnd
					padEnd, padding, err := leadingIndent(ctx, raw, after, b)
					if err != nil {
						return nil, err
					}
					if padding == 0 {
						padding = 1
					} else if padding > 4 {
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
		depth := len(containers)
		root := depth == 0
		if err := b.node(ctx, depth+1); err != nil {
			return nil, err
		}
		text := raw[content:]
		trimmed := bytes.TrimSpace(text)
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
			_, indent, err := leadingIndent(ctx, text, 0, b)
			if err != nil {
				return nil, err
			}
			n, err := byteRun(ctx, trimmed, fenceChar, b)
			if err != nil {
				return nil, err
			}
			if indent <= 3 && n >= fenceWidth && len(bytes.TrimSpace(trimmed[n:])) == 0 {
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
		if indent >= 4 && !paragraph {
			pos = next
			continue
		}
		if indent <= 3 {
			content = localStart
			text = raw[content:]
			trimmed = bytes.TrimSpace(text)
		}
		if indent <= 3 && len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n, err := byteRun(ctx, trimmed, trimmed[0], b)
			if err != nil {
				return nil, err
			}
			if n >= 3 && (trimmed[0] != '`' || !bytes.ContainsRune(trimmed[n:], '`')) {
				fenceChar, fenceWidth, blockDepth = trimmed[0], n, depth
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
		if root && paragraph && len(blocks) > 0 && setext > 0 && indent <= 3 {
			previous := &blocks[len(blocks)-1]
			if previous.root && previous.level == 0 && previous.span.End == line.Start {
				previous.paragraph = false
				previous.level = setext
				previous.span.End = next
				paragraph = false
				pos = next
				continue
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
		blocks = append(blocks, block)
		paragraph = true
		pos = next
	}
	return blocks, nil
}

func leadingIndent(ctx context.Context, raw []byte, start int, b *budget) (end, columns int, err error) {
	end = start
	for end < len(raw) && (raw[end] == ' ' || raw[end] == '\t') {
		if err := b.step(ctx, 1); err != nil {
			return 0, 0, err
		}
		if raw[end] == '\t' {
			columns += 4 - columns%4
		} else {
			columns++
		}
		end++
	}
	return
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
	if text[0] == '`' || text[0] == '~' {
		n, err := byteRun(ctx, text, text[0], b)
		if err != nil {
			return false, err
		}
		if n >= 3 {
			return true, nil
		}
	}
	_, html, err := htmlBlockStart(ctx, text, false, b)
	return html, err
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
