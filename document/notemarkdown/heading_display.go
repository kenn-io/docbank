package notemarkdown

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

type displayToken struct {
	text               string
	char               byte
	width, front, back int
	open, closing      bool
}

type headingDisplay struct {
	ctx   context.Context
	src   []byte
	links map[int]Link
	b     *budget
}

func headingText(ctx context.Context, src []byte, span Span, links map[int]Link, b *budget) (string, error) {
	// Account for token strings, assembled display and whitespace normalization
	// before materializing any of them. Input is a source span, never a rendition.
	if err := b.reserveStrings(span.End - span.Start); err != nil {
		return "", err
	}
	d := headingDisplay{ctx: ctx, src: src, links: links, b: b}
	text, err := d.render(span.Start, span.End, 0)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	pending := false
	for _, r := range text {
		if err := b.step(ctx, 1); err != nil {
			return "", err
		}
		if unicode.IsSpace(r) {
			pending = out.Len() > 0
			continue
		}
		if pending {
			out.WriteByte(' ')
			pending = false
		}
		out.WriteRune(r)
	}
	return out.String(), nil
}

func (d *headingDisplay) render(start, end, depth int) (string, error) {
	if depth > maxDepth {
		return "", &LimitError{Resource: "ast_depth"}
	}
	if err := d.b.reserveStrings(4 * (end - start)); err != nil {
		return "", err
	}
	var tokens []displayToken
	for i := start; i < end; {
		// Admit the syntax token before materializing text or growing tokens.
		if err := d.b.node(d.ctx, depth+1); err != nil {
			return "", err
		}
		c := d.src[i]
		if link, ok := d.links[i]; ok && link.Span.End <= end {
			first := i
			if c == '!' {
				first++
			}
			var text string
			if link.Form == "wiki" {
				labelStart := -1
				for j := first + 2; j < link.Span.End-2; j++ {
					if err := d.b.step(d.ctx, 1); err != nil {
						return "", err
					}
					if d.src[j] == '\\' && j+1 < link.Span.End-2 && strings.ContainsRune("\\]|", rune(d.src[j+1])) {
						j++
						continue
					}
					if d.src[j] == '|' {
						labelStart = j + 1
						break
					}
				}
				if labelStart < 0 {
					text = link.Target
				} else {
					var err error
					text, err = d.render(labelStart, link.Span.End-2, depth+1)
					if err != nil {
						return "", err
					}
				}
			} else if d.src[first] == '<' {
				text = link.RawTarget
			} else {
				closing, err := d.labelEnd(first, link.Span.End)
				if err != nil {
					return "", err
				}
				text, err = d.render(first+1, closing, depth+1)
				if err != nil {
					return "", err
				}
			}
			tokens = append(tokens, displayToken{text: text})
			i = link.Span.End
			continue
		}
		if c == '\\' && i+1 < end && asciiPunctuation(d.src[i+1]) {
			tokens = append(tokens, displayToken{text: string(d.src[i+1 : i+2])})
			i += 2
			continue
		}
		if c == '`' {
			width, err := byteRun(d.ctx, d.src[i:end], '`', d.b)
			if err != nil {
				return "", err
			}
			closing, err := d.codeEnd(i+width, end, width)
			if err != nil {
				return "", err
			}
			if closing >= 0 {
				text := strings.ReplaceAll(string(d.src[i+width:closing]), "\n", " ")
				if len(text) > 1 && text[0] == ' ' && text[len(text)-1] == ' ' && strings.Trim(text, " ") != "" {
					text = text[1 : len(text)-1]
				}
				tokens = append(tokens, displayToken{text: text})
				i = closing + width
				continue
			}
			tokens = append(tokens, displayToken{text: string(d.src[i : i+width])})
			i += width
			continue
		}
		if c == '<' {
			scanner := linkScanner{ctx: d.ctx, src: d.src, b: d.b}
			finish, err := scanner.inlineHTML(i, end)
			if err != nil {
				return "", err
			}
			if finish > i {
				i = finish
				continue
			}
		}
		if c == '*' || c == '_' {
			width, err := byteRun(d.ctx, d.src[i:end], c, d.b)
			if err != nil {
				return "", err
			}
			before, after := rune(' '), rune(' ')
			if i > start {
				before, _ = utf8.DecodeLastRune(d.src[start:i])
			}
			if i+width < end {
				after, _ = utf8.DecodeRune(d.src[i+width : end])
			}
			bs, as := unicode.IsSpace(before), unicode.IsSpace(after)
			bp, ap := unicode.IsPunct(before) || unicode.IsSymbol(before), unicode.IsPunct(after) || unicode.IsSymbol(after)
			left := !as && (!ap || bs || bp)
			right := !bs && (!bp || as || ap)
			open, closing := left, right
			if c == '_' {
				open = left && (!right || bp)
				closing = right && (!left || ap)
			}
			tokens = append(tokens, displayToken{text: string(d.src[i : i+width]), char: c, width: width, open: open, closing: closing})
			i += width
			continue
		}
		if c == '&' {
			semi := bytes.IndexByte(d.src[i:min(end, i+34)], ';')
			if semi >= 0 {
				raw := string(d.src[i : i+semi+1])
				if decoded := decodeEntity(raw); decoded != raw {
					tokens = append(tokens, displayToken{text: decoded})
					i += semi + 1
					continue
				}
			}
		}
		// One ordinary run avoids one allocation per UTF-8 byte.
		j := i + 1
		for j < end && !strings.ContainsRune("\\`<*_&[!", rune(d.src[j])) {
			if err := d.b.step(d.ctx, 1); err != nil {
				return "", err
			}
			j++
		}
		tokens = append(tokens, displayToken{text: string(d.src[i:j])})
		i = j
	}
	// Metered CommonMark flanking and rule-of-three matching. Unmatched markers
	// remain visible, and code/escaped/entity tokens never become delimiters.
	var active []int
	for i := range tokens {
		closer := &tokens[i]
		if closer.char == 0 {
			continue
		}
		for closer.closing && closer.width-closer.front-closer.back > 0 {
			matched := -1
			for a := range slices.Backward(active) {
				if err := d.b.step(d.ctx, 1); err != nil {
					return "", err
				}
				opener := &tokens[active[a]]
				remain := opener.width - opener.front - opener.back
				if !opener.open || remain == 0 || opener.char != closer.char {
					continue
				}
				if (opener.closing || closer.open) && (opener.width+closer.width)%3 == 0 && closer.width%3 != 0 && opener.width%3 != 0 {
					continue
				}
				matched = a
				break
			}
			if matched < 0 {
				break
			}
			opener := &tokens[active[matched]]
			consume := 1
			if opener.width-opener.front-opener.back >= 2 && closer.width-closer.front-closer.back >= 2 {
				consume = 2
			}
			opener.back += consume
			closer.front += consume
			if err := d.b.node(d.ctx, depth+1); err != nil {
				return "", err
			}
			active = active[:matched+1]
			if opener.width == opener.front+opener.back {
				active = active[:matched]
			}
		}
		if closer.open && closer.width > closer.front+closer.back {
			active = append(active, i)
		}
	}
	var out strings.Builder
	for _, token := range tokens {
		if token.char != 0 {
			out.WriteString(token.text[token.front : len(token.text)-token.back])
		} else {
			out.WriteString(token.text)
		}
	}
	return out.String(), nil
}

func (d *headingDisplay) codeEnd(start, end, width int) (int, error) {
	for i := start; i < end; {
		if err := d.b.step(d.ctx, 1); err != nil {
			return -1, err
		}
		if d.src[i] != '`' {
			i++
			continue
		}
		run, err := byteRun(d.ctx, d.src[i:end], '`', d.b)
		if err != nil {
			return 0, err
		}
		if err := d.b.step(d.ctx, run); err != nil {
			return -1, err
		}
		if run == width {
			return i, nil
		}
		i += run
	}
	return -1, nil
}

func (d *headingDisplay) labelEnd(open, end int) (int, error) {
	depth := 1
	for i := open + 1; i < end; i++ {
		if err := d.b.step(d.ctx, 1); err != nil {
			return 0, err
		}
		if child, ok := d.links[i]; ok && child.Span.End <= end {
			i = child.Span.End - 1
			continue
		}
		if d.src[i] == '\\' && i+1 < end {
			i++
			continue
		}
		if d.src[i] == '`' {
			width, err := byteRun(d.ctx, d.src[i:end], '`', d.b)
			if err != nil {
				return 0, err
			}
			closing, err := d.codeEnd(i+width, end, width)
			if err != nil {
				return 0, err
			}
			if closing >= 0 {
				i = closing + width - 1
			} else {
				i += width - 1
			}
			continue
		}
		if d.src[i] == '<' {
			scanner := linkScanner{ctx: d.ctx, src: d.src, b: d.b}
			finish, err := scanner.inlineHTML(i, end)
			if err != nil {
				return 0, err
			}
			if finish > i {
				i = finish - 1
				continue
			}
		}
		if d.src[i] == '[' {
			depth++
		}
		if d.src[i] == ']' {
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, ErrInvalidArtifact
}
