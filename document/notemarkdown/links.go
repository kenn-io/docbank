package notemarkdown

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/v2/util"
)

type linkOpener struct {
	start, label  int
	embed, active bool
}

type linkScanner struct {
	ctx   context.Context
	src   []byte
	b     *budget
	refs  map[string]string
	links []parsedLink
}

func extractLinks(ctx context.Context, src []byte, blocks []sourceBlock, refs map[string]string, b *budget) ([]parsedLink, error) {
	// The block guard has already admitted every definition before any use.
	s := linkScanner{ctx: ctx, src: src, b: b, refs: refs}
	for _, block := range blocks {
		if err := s.inline(block.content.Start, block.content.End); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(s.links, func(i, j int) bool { return s.links[i].Span.Start < s.links[j].Span.Start })
	return s.links, nil
}

// Include the final line ending of a multi-line definition when resuming.
func definitionNext(src []byte, end int) int {
	if end < len(src) && src[end] == '\r' {
		end++
	}
	if end < len(src) && src[end] == '\n' {
		end++
	}
	return end
}

func linkSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (s *linkScanner) referenceKey(start, end int) (string, error) {
	if end-start > 4*999 || start == end || utf8.RuneCount(s.src[start:end]) > 999 {
		return "", nil
	}
	if err := s.b.step(s.ctx, end-start); err != nil {
		return "", err
	}
	// Full Unicode case-folding can expand UTF-8; account before calling the
	// maintained CommonMark reference-label normalizer on a bounded input.
	if err := s.b.reserveStrings(4 * (end - start)); err != nil {
		return "", err
	}
	return util.ToLinkReference(s.src[start:end]), nil
}

func (s *linkScanner) definition(line Span, blockEnd int) (int, bool, error) {
	i := line.Start
	for i < line.End && s.src[i] == ' ' && i-line.Start < 3 {
		i++
	}
	if i == line.End || s.src[i] != '[' {
		return 0, false, nil
	}
	label := i + 1
	for i++; i < blockEnd && i-label <= 4*999; i++ {
		if err := s.b.step(s.ctx, 1); err != nil {
			return 0, false, err
		}
		if s.src[i] == '\\' && i+1 < blockEnd {
			i++
			continue
		}
		if s.src[i] == '[' {
			return 0, false, nil
		}
		if s.src[i] != ']' {
			continue
		}
		closingLine, _, err := sourceLineAt(s.ctx, s.src, i, s.b)
		if err != nil {
			return 0, false, err
		}
		closingLine.End = min(closingLine.End, blockEnd)
		line = closingLine
		if i+1 >= line.End || s.src[i+1] != ':' {
			return 0, false, nil
		}
		key, err := s.referenceKey(label, i)
		if err != nil || key == "" {
			return 0, false, err
		}
		j := i + 2
		for j < line.End && linkSpace(s.src[j]) {
			j++
		}
		// A definition destination may start on the immediately following line.
		if j == line.End {
			_, next, err := sourceLineAt(s.ctx, s.src, line.Start, s.b)
			if err != nil {
				return 0, false, err
			}
			if next >= blockEnd {
				return 0, false, nil
			}
			line, _, err = sourceLineAt(s.ctx, s.src, next, s.b)
			if err != nil {
				return 0, false, err
			}
			line.End = min(line.End, blockEnd)
			j = line.Start
			for j < line.End && linkSpace(s.src[j]) {
				j++
			}
		}
		dest, tail, ok, err := s.destination(j, line.End)
		if err != nil || !ok {
			return 0, false, err
		}
		beforeSpace := tail
		for tail < line.End && linkSpace(s.src[tail]) {
			tail++
		}
		if dest.Start == dest.End && (j >= line.End || s.src[j] != '<') {
			return 0, false, nil
		}
		if tail < line.End && tail == beforeSpace {
			return 0, false, nil
		}
		definitionEnd := line.End
		if tail == line.End {
			_, next, err := sourceLineAt(s.ctx, s.src, line.Start, s.b)
			if err != nil {
				return 0, false, err
			}
			if next < blockEnd {
				titleLine, _, err := sourceLineAt(s.ctx, s.src, next, s.b)
				if err != nil {
					return 0, false, err
				}
				titleLine.End = min(titleLine.End, blockEnd)
				k := titleLine.Start
				for k < titleLine.End && linkSpace(s.src[k]) {
					k++
				}
				finish, yes, err := s.definitionTitle(k, blockEnd)
				if err != nil {
					return 0, false, err
				}
				if yes {
					definitionEnd = finish
				}
			}
		}
		if tail < line.End {
			finish, yes, err := s.definitionTitle(tail, blockEnd)
			if err != nil || !yes {
				return 0, false, err
			}
			definitionEnd = finish
		}
		if _, exists := s.refs[key]; !exists {
			raw, err := s.rawDestination(dest)
			if err != nil {
				return 0, false, err
			}
			s.refs[key] = raw
		}
		return definitionEnd, true, nil
	}
	return 0, false, nil
}

// definitionTitle returns the end of the title's final physical line.
func (s *linkScanner) definitionTitle(start, blockEnd int) (int, bool, error) {
	finish, ok, err := s.title(start, blockEnd)
	if err != nil || !ok {
		return 0, false, err
	}
	line, _, err := sourceLineAt(s.ctx, s.src, finish, s.b)
	if err != nil {
		return 0, false, err
	}
	line.End = min(line.End, blockEnd)
	for finish < line.End && (s.src[finish] == ' ' || s.src[finish] == '\t') {
		if err := s.b.step(s.ctx, 1); err != nil {
			return 0, false, err
		}
		finish++
	}
	return finish, finish == line.End, nil
}

// destination recognizes an angle destination or balanced bare destination.
// Every examined byte is charged, including failed speculative parses.
func (s *linkScanner) destination(start, end int) (Span, int, bool, error) {
	i := start
	angle := i < end && s.src[i] == '<'
	if angle {
		i++
		start++
	}
	depth := 0
	for i < end {
		if err := s.b.step(s.ctx, 1); err != nil {
			return Span{}, 0, false, err
		}
		c := s.src[i]
		if c == '\\' && i+1 < end && asciiPunctuation(s.src[i+1]) {
			i += 2
			continue
		}
		if angle {
			if c == '>' {
				return Span{start, i}, i + 1, true, nil
			}
			if c == '<' || c == '\n' || c == '\r' {
				return Span{}, 0, false, nil
			}
		} else {
			if linkSpace(c) {
				break
			}
			if c < 0x20 || c == 0x7f {
				return Span{}, 0, false, nil
			}
			if c == '(' {
				depth++
				if depth > maxDepth {
					return Span{}, 0, false, &LimitError{Resource: "ast_depth"}
				}
			}
			if c == ')' {
				if depth == 0 {
					break
				}
				depth--
			}
		}
		i++
	}
	if angle || depth != 0 {
		return Span{}, 0, false, nil
	}
	return Span{start, i}, i, true, nil
}

func (s *linkScanner) title(start, end int) (int, bool, error) {
	if start == end {
		return start, false, nil
	}
	closing := s.src[start]
	if closing == '(' {
		closing = ')'
	} else if closing != '\'' && closing != '"' {
		return start, false, nil
	}
	for i := start + 1; i < end; i++ {
		if err := s.b.step(s.ctx, 1); err != nil {
			return 0, false, err
		}
		if s.src[i] == '\\' && i+1 < end {
			i++
			continue
		}
		// CommonMark titles do not nest parentheses. Literal punctuation stays
		// valid through the escaped-character branch above.
		if closing == ')' && s.src[i] == '(' {
			return start, false, nil
		}
		if s.src[i] == closing {
			return i + 1, true, nil
		}
	}
	return start, false, nil
}

func (s *linkScanner) inlineTarget(start, end int) (Span, int, bool, error) {
	dest, finish, ok, err := s.inlineTargetWithDestination(start, end)
	if err != nil || ok {
		return dest, finish, ok, err
	}
	i := start + 1
	for i < end && linkSpace(s.src[i]) {
		if err := s.b.step(s.ctx, 1); err != nil {
			return Span{}, 0, false, err
		}
		i++
	}
	if i == start+1 {
		return Span{}, 0, false, nil
	}
	finish, ok, err = s.title(i, end)
	if err != nil || !ok {
		return Span{}, 0, false, err
	}
	for finish < end && linkSpace(s.src[finish]) {
		if err := s.b.step(s.ctx, 1); err != nil {
			return Span{}, 0, false, err
		}
		finish++
	}
	if finish < end && s.src[finish] == ')' {
		return Span{i, i}, finish + 1, true, nil
	}
	return Span{}, 0, false, nil
}

func (s *linkScanner) inlineTargetWithDestination(start, end int) (Span, int, bool, error) {
	i := start + 1 // opening parenthesis
	for i < end && linkSpace(s.src[i]) {
		if err := s.b.step(s.ctx, 1); err != nil {
			return Span{}, 0, false, err
		}
		i++
	}
	dest, i, ok, err := s.destination(i, end)
	if err != nil || !ok {
		return Span{}, 0, false, err
	}
	beforeSpace := i
	for i < end && linkSpace(s.src[i]) {
		if err := s.b.step(s.ctx, 1); err != nil {
			return Span{}, 0, false, err
		}
		i++
	}
	if i < end && s.src[i] != ')' && i > beforeSpace {
		i, ok, err = s.title(i, end)
		if err != nil || !ok {
			return Span{}, 0, false, err
		}
		for i < end && linkSpace(s.src[i]) {
			if err := s.b.step(s.ctx, 1); err != nil {
				return Span{}, 0, false, err
			}
			i++
		}
	}
	if i < end && s.src[i] == ')' {
		return dest, i + 1, true, nil
	}
	return Span{}, 0, false, nil
}

func (s *linkScanner) rawDestination(dest Span) (string, error) {
	if err := limitCount("target_bytes", dest.End-dest.Start, maxTarget); err != nil {
		return "", err
	}
	if err := s.b.reserveStrings(dest.End - dest.Start); err != nil {
		return "", err
	}
	return string(s.src[dest.Start:dest.End]), nil
}

func (s *linkScanner) emit(use Span, raw string, labelSpan Span, form string) error {
	if err := limitCount("links", len(s.links)+1, s.b.limits.MaxLinks); err != nil {
		return err
	}
	target, err := s.decodeTarget(raw, form == "wikilink" || form == "embed")
	if err != nil {
		return err
	}
	target, fragment, err := s.targetParts(target)
	if err != nil {
		return err
	}
	label, err := rawText(s.ctx, s.src[labelSpan.Start:labelSpan.End], s.b)
	if err != nil {
		return err
	}
	s.links = append(s.links, parsedLink{Span: use, Form: form, Target: target, Fragment: fragment, Label: label})
	return nil
}

// Split syntax-decoded structural delimiters before percent-decoding each
// component once. Encoded hashes remain filename/fragment characters.
func (s *linkScanner) targetParts(authored string) (string, string, error) {
	if err := s.b.step(s.ctx, len(authored)); err != nil {
		return "", "", err
	}
	if err := s.b.reserveStrings(len(authored)); err != nil {
		return "", "", err
	}
	target, fragment, _ := strings.Cut(authored, "#")
	decode := func(value string) string {
		if decoded, err := url.PathUnescape(value); err == nil && utf8.ValidString(decoded) && !strings.ContainsRune(decoded, 0) {
			return decoded
		}
		return value
	}
	return decode(target), decode(fragment), nil
}

func (s *linkScanner) decodeTarget(raw string, wiki bool) (string, error) {
	// The decoded byte count is bounded before constructing the result. HTML
	// entities are individually tiny; one entity can expand to at most 8 bytes.
	var size int
	walk := func(write func(string)) error {
		for i := 0; i < len(raw); {
			if err := s.b.step(s.ctx, 1); err != nil {
				return err
			}
			j := i + 1
			value := raw[i:j]
			if raw[i] == '\\' && j < len(raw) && ((!wiki && asciiPunctuation(raw[j])) || wiki && strings.ContainsRune("\\]|", rune(raw[j]))) {
				value = raw[j : j+1]
				j++
			} else if !wiki && raw[i] == '&' {
				if semi := strings.IndexByte(raw[i:min(len(raw), i+34)], ';'); semi >= 0 {
					candidateEnd := i + semi + 1
					decoded := decodeEntity(raw[i:candidateEnd])
					if decoded != raw[i:candidateEnd] {
						j = candidateEnd
						value = decoded
					}
				}
			}
			write(value)
			i = j
		}
		return nil
	}
	if err := walk(func(v string) { size += len(v) }); err != nil {
		return "", err
	}
	if err := limitCount("target_bytes", size, maxTarget); err != nil {
		return "", err
	}
	if err := s.b.reserveStrings(size); err != nil {
		return "", err
	}
	var out strings.Builder
	out.Grow(size)
	if err := walk(func(v string) { out.WriteString(v) }); err != nil {
		return "", err
	}
	return out.String(), nil
}

func (s *linkScanner) inline(start, end int) error {
	// Index runs once, so an unmatched code delimiter never searches the
	// complete suffix repeatedly. Keys are exact run lengths.
	nextRun := map[int]int{}
	closer := map[int]int{}
	for i := end - 1; i >= start; {
		if err := s.b.step(s.ctx, 1); err != nil {
			return err
		}
		if s.src[i] != '`' {
			i--
			continue
		}
		last := i + 1
		for i >= start && s.src[i] == '`' {
			if err := s.b.step(s.ctx, 1); err != nil {
				return err
			}
			i--
		}
		first := i + 1
		width := last - first
		if width > 1 {
			slashes := 0
			for j := first - 1; j >= start && s.src[j] == '\\'; j-- {
				if err := s.b.step(s.ctx, 1); err != nil {
					return err
				}
				slashes++
			}
			if slashes%2 == 1 {
				if n, ok := nextRun[width-1]; ok {
					closer[first+1] = n
				}
			}
		}
		if n, ok := nextRun[width]; ok {
			closer[first] = n
		}
		nextRun[width] = first
	}
	var stack []linkOpener
	for i := start; i < end; {
		if err := s.b.step(s.ctx, 1); err != nil {
			return err
		}
		c := s.src[i]
		if c == '\\' && i+1 < end && asciiPunctuation(s.src[i+1]) {
			i += 2
			continue
		}
		if c == '`' {
			width, err := byteRun(s.ctx, s.src[i:end], '`', s.b)
			if err != nil {
				return err
			}
			if closing, ok := closer[i]; ok {
				i = closing + width
			} else {
				i += width
			}
			continue
		}
		if c == '<' {
			linksBefore := len(s.links)
			finish, err := s.inlineHTML(i, end)
			if err != nil {
				return err
			}
			if finish > i {
				if len(s.links) > linksBefore {
					for j := range stack {
						if !stack[j].embed {
							stack[j].active = false
						}
					}
				}
				i = finish
				continue
			}
		}
		if c == '[' || c == '!' && i+1 < end && s.src[i+1] == '[' {
			embed := c == '!'
			open := i
			if embed {
				open++
			}
			if open+1 < end && s.src[open+1] == '[' {
				finish, dest, ok, err := s.wiki(open, end)
				if err != nil {
					return err
				}
				if ok {
					raw, err := s.rawDestination(dest)
					if err != nil {
						return err
					}
					label := dest
					if dest.End < finish-2 {
						label = Span{dest.End + 1, finish - 2}
					}
					form := "wikilink"
					if embed {
						form = "embed"
					}
					if err := s.emit(Span{i, finish}, raw, label, form); err != nil {
						return err
					}
					i = finish
					continue
				}
			}
			if err := s.b.node(s.ctx, len(stack)+1); err != nil {
				return err
			}
			stack = append(stack, linkOpener{start: i, label: open + 1, embed: embed, active: true})
			i = open + 1
			continue
		}
		if c == ']' && len(stack) > 0 {
			op := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if !op.active {
				i++
				continue
			}
			dest, finish, form, ok, err := s.closeLink(op, i, end)
			if err != nil {
				return err
			}
			if ok {
				if err := s.emit(Span{op.start, finish}, dest, Span{op.label, i}, form); err != nil {
					return err
				}
				if !op.embed {
					for j := range stack {
						if !stack[j].embed {
							stack[j].active = false
						}
					}
				}
				i = finish
				continue
			}
		}
		i++
	}
	return nil
}

func (s *linkScanner) closeLink(op linkOpener, closing, end int) (string, int, string, bool, error) {
	if closing+1 < end && s.src[closing+1] == '(' {
		dest, finish, ok, err := s.inlineTarget(closing+1, end)
		if err != nil {
			return "", 0, "", false, err
		}
		if ok {
			raw, err := s.rawDestination(dest)
			form := "markdown"
			if op.embed {
				form = "image"
			}
			return raw, finish, form, true, err
		}
	}
	labelStart, labelEnd, finish := op.label, closing, closing+1
	if finish < end && s.src[finish] == '[' {
		j := finish + 1
		for j < end && j-finish <= 4*999+1 && s.src[j] != ']' && s.src[j] != '[' {
			if err := s.b.step(s.ctx, 1); err != nil {
				return "", 0, "", false, err
			}
			if s.src[j] == '\\' && j+1 < end {
				j++
			}
			j++
		}
		if j < end && s.src[j] == ']' {
			if j > finish+1 {
				labelStart, labelEnd = finish+1, j
			}
			finish = j + 1
		}
	}
	key, err := s.referenceKey(labelStart, labelEnd)
	if err != nil {
		return "", 0, "", false, err
	}
	ref, ok := s.refs[key]
	form := "reference"
	if op.embed {
		form = "image"
	}
	return ref, finish, form, ok, nil
}

func (s *linkScanner) wiki(open, end int) (int, Span, bool, error) {
	targetStart, targetEnd := open+2, -1
	for i := targetStart; i < end; i++ {
		if err := s.b.step(s.ctx, 1); err != nil {
			return 0, Span{}, false, err
		}
		if s.src[i] == '\n' || s.src[i] == '\r' {
			return 0, Span{}, false, nil
		}
		if s.src[i] == '\\' && i+1 < end && strings.ContainsRune("\\]|", rune(s.src[i+1])) {
			i++
			continue
		}
		if s.src[i] == '|' && targetEnd < 0 {
			targetEnd = i
		}
		if s.src[i] == ']' && i+1 < end && s.src[i+1] == ']' {
			if targetEnd < 0 {
				targetEnd = i
			}
			return i + 2, Span{targetStart, targetEnd}, true, nil
		}
	}
	return 0, Span{}, false, nil
}
