package notemarkdown

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var errYAML = errors.New("invalid frontmatter syntax or type")

func parseFrontmatter(ctx context.Context, original []byte, b *budget) (state string, header Span, bodyStart int, metadata Metadata, diagnostics []Diagnostic, err error) {
	bodyStart = 0
	if bytes.HasPrefix(original, []byte{0xef, 0xbb, 0xbf}) {
		bodyStart = 3
	}
	state = "absent"
	first, next, lineErr := sourceLineAt(ctx, original, bodyStart, b)
	if lineErr != nil {
		err = lineErr
		return state, header, bodyStart, metadata, diagnostics, err
	}
	if string(bytes.TrimRight(original[first.Start:first.End], " \t")) != "---" {
		return state, header, bodyStart, metadata, diagnostics, err
	}
	start, contentStart := bodyStart, next
	for next < len(original) {
		line, end, lineErr := sourceLineAt(ctx, original, next, b)
		if lineErr != nil {
			err = lineErr
			return state, header, bodyStart, metadata, diagnostics, err
		}
		if end-start > maxFrontmatter {
			err = &LimitError{Resource: "frontmatter_bytes"}
			return state, header, bodyStart, metadata, diagnostics, err
		}
		text := bytes.TrimRight(original[line.Start:line.End], " \t")
		if bytes.Equal(text, []byte("---")) || bytes.Equal(text, []byte("...")) {
			header, bodyStart = Span{start, end}, end
			var node yamlValue
			node, err = parseYAML(ctx, original[contentStart:next], b)
			if err == nil {
				metadata, err = yamlMetadata(node)
			}
			if errors.Is(err, errYAML) {
				state, metadata, err = "invalid", Metadata{}, nil
				diagnostics = []Diagnostic{{Code: "invalid_frontmatter", Span: header}}
				return state, header, bodyStart, metadata, diagnostics, err
			}
			if err != nil {
				return state, header, bodyStart, metadata, diagnostics, err
			}
			state = "valid"
			return state, header, bodyStart, metadata, diagnostics, err
		}
		next = end
	}
	if len(original)-start > maxFrontmatter {
		err = &LimitError{Resource: "frontmatter_bytes"}
		return state, header, bodyStart, metadata, diagnostics, err
	}
	state, header = "invalid", Span{start, len(original)}
	diagnostics = []Diagnostic{{Code: "unclosed_frontmatter", Span: header}}
	return state, header, bodyStart, metadata, diagnostics, err
}

// sourceLineAt excludes CR/LF in the span; next includes the complete EOL.
func sourceLineAt(ctx context.Context, src []byte, start int, b *budget) (line Span, next int, err error) {
	next = start
	for next < len(src) && src[next] != '\n' && src[next] != '\r' {
		if err := b.step(ctx, 1); err != nil {
			return Span{}, 0, err
		}
		next++
	}
	end := next
	if next < len(src) {
		if src[next] == '\r' && next+1 < len(src) && src[next+1] == '\n' {
			next++
		}
		next++
	}
	return Span{start, end}, next, nil
}

type yamlValue struct {
	kind   byte // s string, v other scalar, m mapping, q sequence
	text   string
	items  []yamlValue
	fields map[string]yamlValue
}

type yamlLine struct {
	text   string
	indent int
}
type yamlParser struct {
	ctx        context.Context
	b          *budget
	lines      []yamlLine
	pos, nodes int
}

func parseYAML(ctx context.Context, src []byte, b *budget) (yamlValue, error) {
	if err := b.reserveStrings(len(src)); err != nil {
		return yamlValue{}, err
	}
	text := string(src)
	if err := b.reserveStrings(2 * len(text)); err != nil {
		return yamlValue{}, err
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	p := yamlParser{ctx: ctx, b: b}
	physicalLines := strings.Split(text, "\n")
	if len(physicalLines) > 0 && physicalLines[len(physicalLines)-1] == "" {
		physicalLines = physicalLines[:len(physicalLines)-1]
	}
	for _, raw := range physicalLines {
		if err := b.step(ctx, len(raw)+1); err != nil {
			return yamlValue{}, err
		}
		raw = strings.TrimSuffix(raw, "\r")
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		if strings.HasPrefix(raw[indent:], "\t") {
			return yamlValue{}, errYAML
		}
		p.lines = append(p.lines, yamlLine{raw, indent})
	}
	p.skipBlank()
	if p.pos == len(p.lines) {
		return yamlValue{}, errYAML
	}
	if strings.HasPrefix(strings.TrimSpace(p.lines[p.pos].text), "{") {
		n, err := p.flow(strings.TrimSpace(text), 1)
		if err != nil {
			return yamlValue{}, err
		}
		if n.kind != 'm' {
			return yamlValue{}, errYAML
		}
		return n, nil
	}
	n, err := p.block(p.lines[p.pos].indent, 1)
	if err != nil {
		return yamlValue{}, err
	}
	p.skipBlank()
	if p.pos != len(p.lines) || n.kind != 'm' {
		return yamlValue{}, errYAML
	}
	return n, nil
}

func (p *yamlParser) enter(depth int) error {
	if depth > maxYAMLDepth {
		return &LimitError{Resource: "yaml_depth"}
	}
	if err := p.b.step(p.ctx, 1); err != nil {
		return err
	}
	return addLimit("yaml_nodes", &p.nodes, 1, maxYAMLNodes)
}

func yamlBlank(s string) bool { s = strings.TrimSpace(s); return s == "" || strings.HasPrefix(s, "#") }
func (p *yamlParser) skipBlank() {
	for p.pos < len(p.lines) && yamlBlank(p.lines[p.pos].text) {
		p.pos++
	}
}
func yamlSequence(s string) bool { return s == "-" || strings.HasPrefix(s, "- ") }

func (p *yamlParser) block(indent, depth int) (yamlValue, error) {
	if err := p.enter(depth); err != nil {
		return yamlValue{}, err
	}
	p.skipBlank()
	if p.pos == len(p.lines) {
		return yamlValue{kind: 'v'}, nil
	}
	first := p.lines[p.pos]
	if first.indent != indent {
		return yamlValue{}, errYAML
	}
	seq := yamlSequence(first.text[indent:])
	n := yamlValue{kind: 'm', fields: map[string]yamlValue{}}
	if seq {
		n = yamlValue{kind: 'q'}
	}
	for {
		p.skipBlank()
		if p.pos == len(p.lines) || p.lines[p.pos].indent < indent {
			break
		}
		line := p.lines[p.pos]
		if line.indent != indent {
			return yamlValue{}, errYAML
		}
		text := line.text[indent:]
		if err := p.b.step(p.ctx, len(text)+1); err != nil {
			return yamlValue{}, err
		}
		p.pos++
		var key, rest string
		if seq {
			if !yamlSequence(text) {
				p.pos--
				break
			}
			rest = strings.TrimSpace(text[1:])
		} else {
			colon := yamlMappingColon(text)
			if colon < 0 {
				return yamlValue{}, errYAML
			}
			k, err := p.flow(strings.TrimSpace(text[:colon]), depth+1)
			if err != nil {
				return yamlValue{}, err
			}
			if k.kind != 's' || k.text == "<<" {
				return yamlValue{}, errYAML
			}
			key, rest = k.text, strings.TrimSpace(text[colon+1:])
			if _, exists := n.fields[key]; exists {
				return yamlValue{}, errYAML
			}
		}
		value, err := p.lineValue(rest, indent, depth+1, !seq)
		if err != nil {
			return yamlValue{}, err
		}
		if seq {
			n.items = append(n.items, value)
		} else {
			n.fields[key] = value
		}
	}
	return n, nil
}

func yamlMappingColon(s string) int {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		if quote != 0 {
			if s[i] == '\\' && quote == '"' {
				i++
				continue
			}
			if s[i] == quote {
				if quote == '\'' && i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if s[i] == '\'' || s[i] == '"' {
			quote = s[i]
			continue
		}
		if s[i] == ':' && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t') {
			return i
		}
	}
	return -1
}

func yamlStripComment(s string) string {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		if quote != 0 {
			if s[i] == '\\' && quote == '"' {
				i++
				continue
			}
			if s[i] == quote {
				if quote == '\'' && i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if i == 0 && (s[i] == '\'' || s[i] == '"') {
			quote = s[i]
		}
		if s[i] == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

func (p *yamlParser) lineValue(rest string, indent, depth int, mapping bool) (yamlValue, error) {
	if !strings.HasPrefix(rest, "[") && !strings.HasPrefix(rest, "{") {
		rest = yamlStripComment(rest)
	}
	if rest == "" {
		p.skipBlank()
		if p.pos < len(p.lines) && (p.lines[p.pos].indent > indent || mapping && p.lines[p.pos].indent == indent && yamlSequence(p.lines[p.pos].text[indent:])) {
			return p.block(p.lines[p.pos].indent, depth)
		}
		if err := p.enter(depth); err != nil {
			return yamlValue{}, err
		}
		return yamlValue{kind: 'v'}, nil
	}
	if rest[0] == '|' || rest[0] == '>' {
		return p.blockScalar(rest, indent, depth)
	}
	// Flow and quoted values can continue across physical lines. Collection
	// tokenization below still bounds every node before constructing it.
	if rest[0] == '[' || rest[0] == '{' || rest[0] == '\'' || rest[0] == '"' {
		for {
			closed, err := p.closed(rest)
			if err != nil {
				return yamlValue{}, err
			}
			if closed || p.pos == len(p.lines) {
				break
			}
			line := p.lines[p.pos]
			p.pos++
			if err := p.b.step(p.ctx, len(rest)+len(line.text)+1); err != nil {
				return yamlValue{}, err
			}
			if err := p.b.reserveStrings(len(rest) + len(line.text) + 1); err != nil {
				return yamlValue{}, err
			}
			rest += "\n" + line.text
		}
	}
	return p.flow(rest, depth)
}

func (p *yamlParser) closed(s string) (bool, error) {
	quote := byte(0)
	depth := 0
	for i := 0; i < len(s); i++ {
		if err := p.b.step(p.ctx, 1); err != nil {
			return false, err
		}
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				if quote == '\'' && i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			for i < len(s) && s[i] != '\n' {
				if err := p.b.step(p.ctx, 1); err != nil {
					return false, err
				}
				i++
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
	return quote == 0 && depth == 0, nil
}

func (p *yamlParser) blockScalar(marker string, indent, depth int) (yamlValue, error) {
	if err := p.enter(depth); err != nil {
		return yamlValue{}, err
	}
	if len(marker) > 2 || len(marker) == 2 && marker[1] != '-' && marker[1] != '+' {
		return yamlValue{}, errYAML
	}
	start := p.pos
	contentIndent := -1
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if strings.TrimSpace(line.text) != "" {
			if line.indent <= indent {
				break
			}
			if contentIndent < 0 {
				contentIndent = line.indent
			}
			if line.indent < contentIndent {
				return yamlValue{}, errYAML
			}
		}
		p.pos++
	}
	var out strings.Builder
	for i := start; i < p.pos; i++ {
		line := p.lines[i]
		text := ""
		if strings.TrimSpace(line.text) != "" && len(line.text) >= contentIndent && contentIndent >= 0 {
			text = line.text[contentIndent:]
		}
		if err := p.b.step(p.ctx, len(text)+1); err != nil {
			return yamlValue{}, err
		}
		if err := p.b.reserveStrings(len(text) + 1); err != nil {
			return yamlValue{}, err
		}
		out.WriteString(text)
		separator := byte('\n')
		if marker[0] == '>' && text != "" && line.indent == contentIndent && i+1 < p.pos {
			next := i + 1
			for next < p.pos && strings.TrimSpace(p.lines[next].text) == "" {
				next++
			}
			if next < p.pos && p.lines[next].indent == contentIndent {
				if next == i+1 {
					separator = ' '
				} else {
					separator = 0
				}
			}
		}
		if separator != 0 {
			out.WriteByte(separator)
		}
	}
	value := out.String()
	if strings.HasSuffix(marker, "-") {
		value = strings.TrimRight(value, "\n")
	} else if !strings.HasSuffix(marker, "+") {
		value = strings.TrimRight(value, "\n")
		if p.pos > start {
			value += "\n"
		}
	}
	return yamlValue{kind: 's', text: value}, nil
}

type yamlFlow struct {
	p    *yamlParser
	text string
	pos  int
}

func (p *yamlParser) flow(text string, depth int) (yamlValue, error) {
	f := yamlFlow{p: p, text: text}
	n, err := f.value(depth, false)
	if err != nil {
		return yamlValue{}, err
	}
	f.spaces()
	if f.pos != len(f.text) {
		return yamlValue{}, errYAML
	}
	return n, nil
}
func (f *yamlFlow) spaces() {
	for f.pos < len(f.text) {
		if strings.ContainsRune(" \t\r\n", rune(f.text[f.pos])) {
			f.pos++
			continue
		}
		if f.text[f.pos] == '#' {
			for f.pos < len(f.text) && f.text[f.pos] != '\n' {
				f.pos++
			}
			continue
		}
		break
	}
}
func (f *yamlFlow) value(depth int, inFlow bool) (yamlValue, error) {
	if err := f.p.enter(depth); err != nil {
		return yamlValue{}, err
	}
	f.spaces()
	if f.pos == len(f.text) {
		return yamlValue{kind: 'v'}, nil
	}
	c := f.text[f.pos]
	if c == '[' || c == '{' {
		f.pos++
		n := yamlValue{kind: 'q'}
		closing := byte(']')
		if c == '{' {
			n = yamlValue{kind: 'm', fields: map[string]yamlValue{}}
			closing = '}'
		}
		for {
			f.spaces()
			if f.pos == len(f.text) {
				return yamlValue{}, errYAML
			}
			if f.text[f.pos] == closing {
				f.pos++
				return n, nil
			}
			if c == '[' {
				v, err := f.value(depth+1, true)
				if err != nil {
					return yamlValue{}, err
				}
				n.items = append(n.items, v)
			} else {
				key, err := f.key(depth + 1)
				if err != nil {
					return yamlValue{}, err
				}
				if key == "<<" {
					return yamlValue{}, errYAML
				}
				if _, ok := n.fields[key]; ok {
					return yamlValue{}, errYAML
				}
				v, err := f.value(depth+1, true)
				if err != nil {
					return yamlValue{}, err
				}
				n.fields[key] = v
			}
			f.spaces()
			if f.pos == len(f.text) {
				return yamlValue{}, errYAML
			}
			if f.text[f.pos] == closing {
				f.pos++
				return n, nil
			}
			if f.text[f.pos] != ',' {
				return yamlValue{}, errYAML
			}
			f.pos++
		}
	}
	if c == '\'' || c == '"' {
		text, err := f.quoted()
		return yamlValue{kind: 's', text: text}, err
	}
	if strings.ContainsRune("&*!|>@`", rune(c)) {
		return yamlValue{}, errYAML
	}
	start := f.pos
	for f.pos < len(f.text) && f.text[f.pos] != '\n' && (!inFlow || !strings.ContainsRune(",]}{[", rune(f.text[f.pos]))) {
		if f.text[f.pos] == '#' && (f.pos == start || f.text[f.pos-1] == ' ' || f.text[f.pos-1] == '\t') {
			break
		}
		if err := f.p.b.step(f.p.ctx, 1); err != nil {
			return yamlValue{}, err
		}
		f.pos++
	}
	raw := yamlStripComment(f.text[start:f.pos])
	if raw == "" || yamlMappingColon(raw) >= 0 {
		return yamlValue{}, errYAML
	}
	if err := f.p.b.reserveStrings(len(raw)); err != nil {
		return yamlValue{}, err
	}
	text := strings.Clone(raw)
	kind := byte('s')
	if yamlNonString(text) {
		kind = 'v'
	}
	return yamlValue{kind: kind, text: text}, nil
}
func (f *yamlFlow) key(depth int) (string, error) {
	f.spaces()
	if err := f.p.enter(depth); err != nil {
		return "", err
	}
	var key string
	if f.pos < len(f.text) && (f.text[f.pos] == '\'' || f.text[f.pos] == '"') {
		var err error
		key, err = f.quoted()
		if err != nil {
			return "", err
		}
	} else {
		start := f.pos
		for f.pos < len(f.text) && f.text[f.pos] != ':' && !strings.ContainsRune(",{}[]", rune(f.text[f.pos])) {
			f.pos++
		}
		key = strings.TrimSpace(f.text[start:f.pos])
		if key == "" || yamlNonString(key) || strings.ContainsRune("&*!|>@`?", rune(key[0])) {
			return "", errYAML
		}
		if err := f.p.b.reserveStrings(len(key)); err != nil {
			return "", err
		}
		key = strings.Clone(key)
	}
	f.spaces()
	if f.pos == len(f.text) || f.text[f.pos] != ':' {
		return "", errYAML
	}
	f.pos++
	return key, nil
}
func yamlNonString(s string) bool {
	lower := strings.ToLower(s)
	if lower == "null" || lower == "~" || lower == "true" || lower == "false" || lower == ".nan" || lower == ".inf" || lower == "-.inf" || lower == "+.inf" {
		return true
	}
	if _, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64); err == nil {
		return true
	}
	if _, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 0, 64); err == nil {
		return true
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339Nano, "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05"} {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}
func (f *yamlFlow) quoted() (string, error) {
	quote := f.text[f.pos]
	f.pos++
	var out strings.Builder
	for f.pos < len(f.text) {
		if err := f.p.b.step(f.p.ctx, 1); err != nil {
			return "", err
		}
		c := f.text[f.pos]
		f.pos++
		escaped := false
		if c == quote {
			if quote == '\'' && f.pos < len(f.text) && f.text[f.pos] == '\'' {
				f.pos++
			} else {
				return out.String(), nil
			}
		} else if c == '\\' && quote == '"' {
			escaped = true
			if f.pos == len(f.text) {
				return "", errYAML
			}
			c = f.text[f.pos]
			f.pos++
			codes := map[byte]byte{'0': 0, 'a': 7, 'b': 8, 't': 9, 'n': 10, 'v': 11, 'f': 12, 'r': 13, 'e': 27, ' ': 32, '"': '"', '/': '/', '\\': '\\'}
			if replacement, ok := codes[c]; ok {
				c = replacement
			} else if c == 'x' || c == 'u' || c == 'U' {
				count := 2
				if c == 'u' {
					count = 4
				}
				if c == 'U' {
					count = 8
				}
				if len(f.text)-f.pos < count {
					return "", errYAML
				}
				r, err := strconv.ParseUint(f.text[f.pos:f.pos+count], 16, 32)
				if err != nil || r > utf8.MaxRune || !utf8.ValidRune(rune(r)) {
					return "", errYAML
				}
				f.pos += count
				if err := f.p.b.reserveStrings(utf8.RuneLen(rune(r))); err != nil {
					return "", err
				}
				out.WriteRune(rune(r))
				continue
			} else if c == '\n' {
				for f.pos < len(f.text) && (f.text[f.pos] == ' ' || f.text[f.pos] == '\t') {
					if err := f.p.b.step(f.p.ctx, 1); err != nil {
						return "", err
					}
					f.pos++
				}
				continue
			} else {
				return "", errYAML
			}
		}
		if !escaped && (c == ' ' || c == '\t') {
			start, end := f.pos-1, f.pos
			for end < len(f.text) && (f.text[end] == ' ' || f.text[end] == '\t') {
				if err := f.p.b.step(f.p.ctx, 1); err != nil {
					return "", err
				}
				end++
			}
			f.pos = end
			if end < len(f.text) && f.text[end] == '\n' {
				continue
			}
			if err := f.p.b.reserveStrings(end - start); err != nil {
				return "", err
			}
			out.WriteString(f.text[start:end])
			continue
		}
		if c == '\n' && !escaped {
			breaks := 1
			for f.pos < len(f.text) {
				if f.text[f.pos] != ' ' && f.text[f.pos] != '\t' && f.text[f.pos] != '\n' {
					break
				}
				if err := f.p.b.step(f.p.ctx, 1); err != nil {
					return "", err
				}
				if f.text[f.pos] == '\n' {
					breaks++
				}
				f.pos++
			}
			if breaks > 1 {
				if err := f.p.b.reserveStrings(breaks - 1); err != nil {
					return "", err
				}
				out.WriteString(strings.Repeat("\n", breaks-1))
				continue
			}
			c = ' '
		}
		if err := f.p.b.reserveStrings(1); err != nil {
			return "", err
		}
		out.WriteByte(c)
	}
	return "", errYAML
}

func yamlMetadata(n yamlValue) (Metadata, error) {
	if n.kind != 'm' {
		return Metadata{}, errYAML
	}
	// Resource admission precedes evolving type policy for the entire group.
	if title, ok := n.fields["title"]; ok {
		if err := limitCount("title_bytes", len(title.text), maxTitle); err != nil {
			return Metadata{}, err
		}
	}
	for _, name := range []string{"aliases", "tags"} {
		v, ok := n.fields[name]
		if !ok {
			continue
		}
		if err := limitCount(name, len(v.items), maxMetadata); err != nil {
			return Metadata{}, err
		}
		if err := limitCount("metadata_value_bytes", len(v.text), maxValue); err != nil {
			return Metadata{}, err
		}
		for _, item := range v.items {
			if err := limitCount("metadata_value_bytes", len(item.text), maxValue); err != nil {
				return Metadata{}, err
			}
		}
	}
	var result Metadata
	if title, ok := n.fields["title"]; ok {
		if title.kind != 's' {
			return Metadata{}, errYAML
		}
		if err := limitCount("title_bytes", len(title.text), maxTitle); err != nil {
			return Metadata{}, err
		}
		result.Title = title.text
	}
	for _, field := range []struct {
		name   string
		target *[]string
	}{{"aliases", &result.Aliases}, {"tags", &result.Tags}} {
		v, ok := n.fields[field.name]
		if !ok {
			continue
		}
		if v.kind == 's' {
			v = yamlValue{kind: 'q', items: []yamlValue{v}}
		}
		if v.kind != 'q' {
			return Metadata{}, errYAML
		}
		if err := limitCount(field.name, len(v.items), maxMetadata); err != nil {
			return Metadata{}, err
		}
		for _, item := range v.items {
			if item.kind != 's' {
				return Metadata{}, errYAML
			}
			if err := limitCount("metadata_value_bytes", len(item.text), maxValue); err != nil {
				return Metadata{}, err
			}
			*field.target = append(*field.target, item.text)
		}
	}
	return result, nil
}
