package notemarkdown

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// These anchored RE2 patterns validate one already bounded/metered tag. They
// cannot backtrack exponentially and never parse an entire Markdown document.
var openingHTMLTag = regexp.MustCompile(`(?s)^<[A-Za-z][A-Za-z0-9-]*(?:[ \t\n\r]+[A-Za-z_:][A-Za-z0-9_.:-]*(?:[ \t\n\r]*=[ \t\n\r]*(?:[^ \t\n\r"'=<>\x60]+|'[^']*'|"[^"]*"))?)*[ \t\n\r]*/?>$`)
var closingHTMLTag = regexp.MustCompile(`^</[A-Za-z][A-Za-z0-9-]*[ \t\n\r]*>$`)

func htmlTagEnd(ctx context.Context, src []byte, start, end int, b *budget) (int, error) {
	quote := byte(0)
	for i := start + 1; i < end; i++ {
		if err := b.step(ctx, 1); err != nil {
			return 0, err
		}
		c := src[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c != '>' {
			continue
		}
		tag := src[start : i+1]
		for _, pattern := range []*regexp.Regexp{openingHTMLTag, closingHTMLTag} {
			reader := htmlRuneReader{ctx: ctx, b: b, src: tag}
			matched := pattern.MatchReader(&reader)
			if reader.err != nil {
				return 0, reader.err
			}
			if matched {
				return i + 1, nil
			}
		}
		return 0, nil
	}
	return 0, nil
}

func (s *linkScanner) inlineHTML(start, end int) (int, error) {
	for _, short := range []string{"<!-->", "<!--->"} {
		if bytes.HasPrefix(s.src[start:end], []byte(short)) {
			return start + len(short), nil
		}
	}
	for _, special := range []struct{ prefix, close string }{{"<!--", "-->"}, {"<?", "?>"}, {"<![CDATA[", "]]>"}} {
		if bytes.HasPrefix(s.src[start:end], []byte(special.prefix)) {
			for i := start + len(special.prefix); i+len(special.close) <= end; i++ {
				if err := s.b.step(s.ctx, 1); err != nil {
					return 0, err
				}
				if bytes.HasPrefix(s.src[i:end], []byte(special.close)) {
					return i + len(special.close), nil
				}
			}
			return 0, nil
		}
	}
	// Autolinks end at the first angle close. Their interior is literal; entity
	// and backslash syntax is not interpreted inside a CommonMark autolink.
	for i := start + 1; i < end; i++ {
		if err := s.b.step(s.ctx, 1); err != nil {
			return 0, err
		}
		if s.src[i] == '<' || s.src[i] == '\n' || s.src[i] == '\r' {
			break
		}
		if s.src[i] != '>' {
			continue
		}
		inside := s.src[start+1 : i]
		uri, err := validAutolinkURI(s.ctx, inside, s.b)
		if err != nil {
			return 0, err
		}
		email := false
		if !uri {
			email, err = validAutolinkEmail(s.ctx, inside, s.b)
			if err != nil {
				return 0, err
			}
		}
		if uri || email {
			if err := s.emitAutolink(Span{start, i + 1}, Span{start + 1, i}, email); err != nil {
				return 0, err
			}
			return i + 1, nil
		}
		break
	}
	if start+2 < end && s.src[start+1] == '!' && asciiLetter(s.src[start+2]) {
		for i := start + 3; i < end; i++ {
			if err := s.b.step(s.ctx, 1); err != nil {
				return 0, err
			}
			if s.src[i] == '>' {
				return i + 1, nil
			}
		}
		return 0, nil
	}
	return htmlTagEnd(s.ctx, s.src, start, end, s.b)
}

func (s *linkScanner) emitAutolink(use, dest Span, email bool) error {
	if err := limitCount("links", len(s.links)+1, maxLinks); err != nil {
		return err
	}
	size := dest.End - dest.Start
	targetSize := size
	if email {
		targetSize += 7
	}
	if err := limitCount("target_bytes", targetSize, maxTarget); err != nil {
		return err
	}
	if err := s.b.reserveStrings(size + targetSize); err != nil {
		return err
	}
	raw := string(s.src[dest.Start:dest.End])
	target := raw
	if email {
		target = "mailto:" + raw
	}
	s.links = append(s.links, Link{Span: use, DestinationSpan: dest, Form: "markdown", RawTarget: raw, Target: target})
	return nil
}

func validAutolinkURI(ctx context.Context, value []byte, b *budget) (bool, error) {
	colon := -1
	for i := 0; i < len(value) && i <= 32; i++ {
		if value[i] == ':' {
			colon = i
			break
		}
	}
	if colon < 2 || !asciiLetter(value[0]) {
		return false, nil
	}
	for i, c := range value {
		if err := b.step(ctx, 1); err != nil {
			return false, err
		}
		if c <= 0x20 || c == '<' || c == '>' || c == 0x7f {
			return false, nil
		}
		if i > 0 && i < colon && !asciiLetter(c) && (c < '0' || c > '9') && c != '+' && c != '-' && c != '.' {
			return false, nil
		}
	}
	return true, nil
}
func validAutolinkEmail(ctx context.Context, value []byte, b *budget) (bool, error) {
	at := -1
	for i, c := range value {
		if err := b.step(ctx, 1); err != nil {
			return false, err
		}
		if c == '@' {
			at = i
			break
		}
	}
	if at < 1 || at == len(value)-1 {
		return false, nil
	}
	for _, c := range value[:at] {
		if err := b.step(ctx, 1); err != nil {
			return false, err
		}
		if !asciiLetter(c) && (c < '0' || c > '9') && !strings.ContainsRune(".!#$%&'*+-/=?^_`{|}~", rune(c)) {
			return false, nil
		}
	}
	labelStart := at + 1
	for i := at + 1; i <= len(value); i++ {
		if err := b.step(ctx, 1); err != nil {
			return false, err
		}
		if i == len(value) || value[i] == '.' {
			if i == labelStart || i-labelStart > 63 || value[labelStart] == '-' || value[i-1] == '-' {
				return false, nil
			}
			labelStart = i + 1
			continue
		}
		c := value[i]
		if !asciiLetter(c) && (c < '0' || c > '9') && c != '-' {
			return false, nil
		}
	}
	return true, nil
}

// Regexp's reader path consumes bounded runes; cancellation is checked while
// validating a tag rather than in a bulk accounting pass before validation.
type htmlRuneReader struct {
	ctx context.Context
	b   *budget
	src []byte
	pos int
	err error
}

func (r *htmlRuneReader) ReadRune() (rune, int, error) {
	if r.pos == len(r.src) {
		return 0, 0, io.EOF
	}
	if err := r.b.step(r.ctx, 1); err != nil {
		r.err = err
		return 0, 0, err
	}
	c, size := utf8.DecodeRune(r.src[r.pos:])
	r.pos += size
	return c, size, nil
}

func htmlEndMatches(ctx context.Context, text []byte, end string, b *budget) (bool, error) {
	if end == "raw-tags" {
		for _, tag := range []string{"</script>", "</style>", "</pre>", "</textarea>"} {
			matched, err := htmlEndMatches(ctx, text, tag, b)
			if err != nil || matched {
				return matched, err
			}
		}
		return false, nil
	}
	fold := strings.HasPrefix(end, "</")
	for i := 0; i+len(end) <= len(text); i++ {
		if err := b.step(ctx, len(end)); err != nil {
			return false, err
		}
		if fold && bytes.EqualFold(text[i:i+len(end)], []byte(end)) || !fold && string(text[i:i+len(end)]) == end {
			return true, nil
		}
	}
	return false, nil
}

// Only type-7 complete standalone tags require a paragraph boundary. Raw
// block tags may interrupt an ordinary paragraph.
func htmlBlockStart(ctx context.Context, text []byte, generic bool, b *budget) (string, bool, error) {
	if bytes.HasPrefix(text, []byte("<!--")) {
		return "-->", true, nil
	}
	if bytes.HasPrefix(text, []byte("<?")) {
		return "?>", true, nil
	}
	if bytes.HasPrefix(text, []byte("<![CDATA[")) {
		return "]]>", true, nil
	}
	if len(text) > 2 && text[0] == '<' && text[1] == '!' && asciiLetter(text[2]) {
		return ">", true, nil
	}
	if len(text) < 2 || text[0] != '<' {
		return "", false, nil
	}
	start := 1
	if text[start] == '/' {
		start++
	}
	i := start
	for i < len(text) && (asciiLetter(text[i]) || i > start && (text[i] >= '0' && text[i] <= '9' || text[i] == '-')) {
		if err := b.step(ctx, 1); err != nil {
			return "", false, err
		}
		i++
	}
	if i == start || i < len(text) && !strings.ContainsRune(" \t>/", rune(text[i])) {
		return "", false, nil
	}
	tag := text[start:i]
	for _, name := range []string{"script", "style", "pre", "textarea"} {
		if start == 1 && bytes.EqualFold(tag, []byte(name)) {
			if i < len(text) && text[i] == '/' {
				return "", false, nil
			}
			return "raw-tags", true, nil
		}
	}
	for _, name := range []string{"address", "article", "aside", "base", "basefont", "blockquote", "body", "caption", "center", "col", "colgroup", "dd", "details", "dialog", "dir", "div", "dl", "dt", "fieldset", "figcaption", "figure", "footer", "form", "frame", "frameset", "h1", "h2", "h3", "h4", "h5", "h6", "head", "header", "hr", "html", "iframe", "legend", "li", "link", "main", "menu", "menuitem", "nav", "noframes", "ol", "optgroup", "option", "p", "param", "search", "section", "summary", "table", "tbody", "td", "tfoot", "th", "thead", "title", "tr", "track", "ul"} {
		if bytes.EqualFold(tag, []byte(name)) {
			return "", true, nil
		}
	}
	if generic {
		finish, err := htmlTagEnd(ctx, text, 0, len(text), b)
		if err != nil {
			return "", false, err
		}
		if finish == len(text) {
			return "", true, nil
		}
	}
	return "", false, nil
}
