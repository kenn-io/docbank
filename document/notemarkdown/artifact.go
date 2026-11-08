package notemarkdown

import (
	"context"
	"reflect"
	"strconv"
	"strings"
)

// CanonicalArtifact validates and serializes the semantic data in a Note.
// It checks exact JSON size before allocation. Empty arrays always encode as [].
// It cannot validate UTF-8 source boundaries without the original; Parse does.
func CanonicalArtifact(ctx context.Context, note Note) ([]byte, error) {
	return canonicalArtifact(ctx, note, &budget{})
}

func canonicalArtifact(ctx context.Context, n Note, b *budget) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := admitArtifact(ctx, n); err != nil {
		return nil, err
	}
	if err := validateArtifact(ctx, n, b); err != nil {
		return nil, err
	}
	enc := artifactEncoder{ctx: ctx, budget: b}
	if err := enc.value(reflect.ValueOf(n)); err != nil {
		return nil, err
	}
	enc.out = make([]byte, 0, enc.size)
	enc.writing = true
	if err := enc.value(reflect.ValueOf(n)); err != nil {
		return nil, err
	}
	return enc.out, nil
}

func validateArtifact(ctx context.Context, n Note, b *budget) error {
	if err := walkArtifactStrings(reflect.ValueOf(n), func(s string) error { return validUTF8(ctx, s, b) }); err != nil {
		return err
	}
	if n.Recipe != Recipe || n.SourceBytes < 0 || n.SourceBytes > maxInput || len(n.SourceSHA256) != 64 {
		return ErrInvalidArtifact
	}
	for _, c := range n.SourceSHA256 {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ErrInvalidArtifact
		}
	}
	if !validSpan(n.Frontmatter, n.SourceBytes) || n.BodyStart < 0 || n.BodyStart > n.SourceBytes ||
		n.FrontmatterState != "absent" && n.FrontmatterState != "valid" && n.FrontmatterState != "invalid" {
		return ErrInvalidArtifact
	}

	used := stringSet{}
	previous := -1
	for _, h := range n.Headings {
		if h.Level < 1 || h.Level > 6 || !validSpan(h.Span, n.SourceBytes) ||
			!validSpan(h.Section, n.SourceBytes) || h.Section.Start != h.Span.Start ||
			h.Section.End < h.Span.End || h.Span.Start < previous || h.Anchor == "" {
			return ErrInvalidArtifact
		}
		duplicate, err := used.add(ctx, h.Anchor, b)
		if err != nil {
			return err
		}
		if duplicate {
			return ErrInvalidArtifact
		}
		previous = h.Span.End
	}
	previous = -1
	for _, l := range n.Links {
		if !validSpan(l.Span, n.SourceBytes) || !validSpan(l.DestinationSpan, n.SourceBytes) ||
			l.Span.Start < previous || l.Form != "markdown" && l.Form != "markdown_reference" && l.Form != "wiki" {
			return ErrInvalidArtifact
		}
		previous = l.Span.Start
		if len(l.RawTarget) > maxTarget || len(l.Target) > maxTarget {
			return &LimitError{Resource: "target_bytes"}
		}
	}
	for _, d := range n.Diagnostics {
		if !validSpan(d.Span, n.SourceBytes) || len(d.Code) == 0 || len(d.Code) > 64 {
			return ErrInvalidArtifact
		}
		for _, c := range d.Code {
			if (c < 'a' || c > 'z') && c != '_' {
				return ErrInvalidArtifact
			}
		}
	}
	return nil
}

func validSpan(s Span, size int) bool { return s.Start >= 0 && s.End >= s.Start && s.End <= size }

// The value tree contains only these fixed structs/slices/scalars, never maps
// or user-defined marshalers. The counting pass uses precisely the same writer.
type artifactEncoder struct {
	ctx     context.Context
	budget  *budget
	derived int
	size    int
	writing bool
	out     []byte
}

func (e *artifactEncoder) emit(data string) error {
	if e.writing {
		e.out = append(e.out, data...)
		return nil
	}
	return addLimit("artifact_bytes", &e.size, len(data), maxArtifact)
}

func (e *artifactEncoder) quoted(s string, derived bool) error {
	if !e.writing && derived {
		if err := addLimit("derived_bytes", &e.derived, len(s), maxDerived); err != nil {
			return err
		}
	}
	if err := e.emit(`"`); err != nil {
		return err
	}
	start := 0
	for i := 0; i < len(s); {
		if err := e.budget.step(e.ctx, 1); err != nil {
			return err
		}
		c := s[i]
		width := 1
		escaped := ""
		switch c {
		case '"':
			escaped = `\"`
		case '\\':
			escaped = `\\`
		case '\b':
			escaped = `\b`
		case '\f':
			escaped = `\f`
		case '\n':
			escaped = `\n`
		case '\r':
			escaped = `\r`
		case '\t':
			escaped = `\t`
		default:
			if c < 0x20 {
				const digits = "0123456789abcdef"
				escaped = "\\u00" + string([]byte{digits[c>>4], digits[c&15]})
			} else if c == 0xe2 && (strings.HasPrefix(s[i:], "\u2028") || strings.HasPrefix(s[i:], "\u2029")) {
				width = 3
				if s[i+2] == 0xa8 {
					escaped = `\u2028`
				} else {
					escaped = `\u2029`
				}
			}
		}
		if escaped != "" {
			if err := e.emit(s[start:i]); err != nil {
				return err
			}
			if err := e.emit(escaped); err != nil {
				return err
			}
			start = i + width
		}
		i += width
	}
	if err := e.emit(s[start:]); err != nil {
		return err
	}
	return e.emit(`"`)
}

func (e *artifactEncoder) value(v reflect.Value) error {
	if err := e.budget.step(e.ctx, 1); err != nil {
		return err
	}
	switch v.Kind() {
	case reflect.Struct:
		if err := e.emit("{"); err != nil {
			return err
		}
		for i := range v.NumField() {
			if i > 0 {
				if err := e.emit(","); err != nil {
					return err
				}
			}
			if err := e.quoted(v.Type().Field(i).Tag.Get("json"), false); err != nil {
				return err
			}
			if err := e.emit(":"); err != nil {
				return err
			}
			if err := e.value(v.Field(i)); err != nil {
				return err
			}
		}
		return e.emit("}")
	case reflect.Slice:
		if err := e.emit("["); err != nil {
			return err
		}
		for i := range v.Len() {
			if i > 0 {
				if err := e.emit(","); err != nil {
					return err
				}
			}
			if err := e.value(v.Index(i)); err != nil {
				return err
			}
		}
		return e.emit("]")
	case reflect.String:
		return e.quoted(v.String(), true)
	case reflect.Int:
		return e.emit(strconv.FormatInt(v.Int(), 10))
	case reflect.Bool:
		return e.emit(strconv.FormatBool(v.Bool()))
	default:
		return ErrInvalidArtifact
	}
}

// Admission traverses only bounded slice lengths and string descriptors. It
// neither scans caller strings nor hashes them before the aggregate check.
func admitArtifact(ctx context.Context, n Note) error {
	for _, check := range []struct {
		code   string
		n, max int
	}{
		{"headings", len(n.Headings), maxHeadings}, {"links", len(n.Links), maxLinks},
		{"aliases", len(n.Metadata.Aliases), maxMetadata}, {"tags", len(n.Metadata.Tags), maxMetadata},
		{"diagnostics", len(n.Diagnostics), maxDiagnostics}, {"title_bytes", len(n.Metadata.Title), maxTitle},
	} {
		if err := limitCount(check.code, check.n, check.max); err != nil {
			return err
		}
	}
	for _, values := range [][]string{n.Metadata.Aliases, n.Metadata.Tags} {
		for _, value := range values {
			if err := limitCount("metadata_value_bytes", len(value), maxValue); err != nil {
				return err
			}
		}
	}
	total, steps := 0, 0
	return walkArtifactStrings(reflect.ValueOf(n), func(s string) error {
		steps++
		if steps%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return addLimit("derived_bytes", &total, len(s), maxDerived)
	})
}
func walkArtifactStrings(v reflect.Value, visit func(string) error) error {
	switch v.Kind() {
	case reflect.String:
		return visit(v.String())
	case reflect.Struct:
		for _, field := range v.Fields() {
			if err := walkArtifactStrings(field, visit); err != nil {
				return err
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			if err := walkArtifactStrings(v.Index(i), visit); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	return nil
}
