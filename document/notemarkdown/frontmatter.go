package notemarkdown

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var errYAML = errors.New("unsupported or invalid frontmatter")

func parseFrontmatter(ctx context.Context, src []byte, b *budget) (*Frontmatter, int, []Diagnostic, error) {
	start := 0
	if bytes.HasPrefix(src, []byte{0xef, 0xbb, 0xbf}) {
		start = 3
	}
	first, next, err := sourceLineAt(ctx, src, start, b)
	if err != nil {
		return nil, 0, nil, err
	}
	if !bytes.Equal(bytes.TrimRight(src[first.Start:first.End], " \t"), []byte("---")) {
		return nil, start, nil, nil
	}
	contentStart := next
	for next < len(src) {
		line, end, err := sourceLineAt(ctx, src, next, b)
		if err != nil {
			return nil, 0, nil, err
		}
		if err := limitCount("frontmatter_bytes", end-start, maxFrontmatter); err != nil {
			return nil, 0, nil, err
		}
		text := bytes.TrimRight(src[line.Start:line.End], " \t")
		if bytes.Equal(text, []byte("---")) || bytes.Equal(text, []byte("...")) {
			fm := &Frontmatter{Start: start, End: end}
			if err := decodeFrontmatter(ctx, src[contentStart:next], fm, b); err != nil {
				if !errors.Is(err, errYAML) {
					return nil, 0, nil, err
				}
				return &Frontmatter{Start: start, End: end}, end, []Diagnostic{{Code: "invalid_frontmatter", Span: Span{start, end}}}, nil
			}
			return fm, end, nil, nil
		}
		next = end
	}
	if err := limitCount("frontmatter_bytes", len(src)-start, maxFrontmatter); err != nil {
		return nil, 0, nil, err
	}
	// Without a closing fence there is no reliable YAML/body boundary. Exclude
	// only the opener so authored headings and links can still be read.
	return &Frontmatter{Start: start, End: contentStart}, contentStart,
		[]Diagnostic{{Code: "unclosed_frontmatter", Span: Span{start, len(src)}}}, nil
}

func decodeFrontmatter(ctx context.Context, src []byte, fm *Frontmatter, b *budget) error {
	if err := b.step(ctx, len(src)); err != nil {
		return err
	}
	// The decoder sees at most 64 KiB. Decode to nodes first: never ask YAML to
	// expand aliases into Go values before our depth/node/byte admissions.
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return errYAML
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		fm.Raw = map[string]any{}
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return errYAML
	}
	if err := limitCount("frontmatter_keys", len(root.Content)/2, b.limits.MaxFrontmatterKeys); err != nil {
		return err
	}
	d := yamlDecoder{ctx: ctx, b: b, active: map[*yaml.Node]bool{}}
	value, err := d.value(root, 0)
	if err != nil {
		return err
	}
	raw, ok := value.(map[string]any)
	if !ok {
		return errYAML
	}
	fm.Raw = raw
	if err := limitCount("frontmatter_keys", len(fm.Raw), b.limits.MaxFrontmatterKeys); err != nil {
		return err
	}
	keys := make([]string, 0, len(fm.Raw))
	for key := range fm.Raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values, ok := fm.Raw[key].([]any)
		if !ok {
			values = []any{fm.Raw[key]}
		}
		for ordinal, value := range values {
			text, ok := scalarText(value)
			if !ok {
				continue
			}
			if err := limitCount("frontmatter_value_bytes", len(text), b.limits.MaxValueBytes); err != nil {
				return err
			}
			if err := b.reserveStrings(len(key) + len(text)); err != nil {
				return err
			}
			fm.Fields = append(fm.Fields, Field{Key: key, Value: text, Ordinal: ordinal})
			if key == "aliases" {
				fm.Aliases = append(fm.Aliases, text)
			}
			if key == "tags" {
				fm.Tags = append(fm.Tags, text)
			}
		}
	}
	return nil
}

type yamlDecoder struct {
	ctx    context.Context
	b      *budget
	nodes  int
	active map[*yaml.Node]bool
}

func (d *yamlDecoder) value(n *yaml.Node, depth int) (any, error) {
	if depth > maxYAMLDepth {
		return nil, &LimitError{Resource: "yaml_depth"}
	}
	if err := addLimit("yaml_nodes", &d.nodes, 1, maxYAMLNodes); err != nil {
		return nil, err
	}
	if err := d.b.step(d.ctx, 1); err != nil {
		return nil, err
	}
	if d.active[n] {
		return nil, errYAML
	}
	d.active[n] = true
	defer delete(d.active, n)
	if n.Kind == yaml.AliasNode {
		return d.value(n.Alias, depth+1)
	}
	switch n.Kind {
	case yaml.MappingNode:
		if n.Tag != "!!map" {
			return nil, errYAML
		}
		out := map[string]any{}
		var merge *yaml.Node
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind == yaml.ScalarNode && key.Tag == "!!merge" {
				if merge != nil {
					return nil, errYAML
				}
				merge = n.Content[i+1]
				continue
			}
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return nil, errYAML
			}
			if err := d.admitScalar(key.Value); err != nil {
				return nil, err
			}
			if _, exists := out[key.Value]; exists {
				return nil, errYAML
			}
			v, err := d.value(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			out[key.Value] = v
		}
		if merge != nil {
			v, err := d.value(merge, depth+1)
			if err != nil {
				return nil, err
			}
			maps, ok := v.([]any)
			if !ok {
				maps = []any{v}
			}
			// Explicit keys win, then earlier maps in a merge sequence win.
			for _, item := range maps {
				m, ok := item.(map[string]any)
				if !ok {
					return nil, errYAML
				}
				for key, value := range m {
					if err := d.b.step(d.ctx, 1); err != nil {
						return nil, err
					}
					if _, exists := out[key]; !exists {
						out[key] = value
					}
				}
			}
		}

		return out, nil
	case yaml.SequenceNode:
		if n.Tag != "!!seq" {
			return nil, errYAML
		}
		if err := limitCount("frontmatter_values", len(n.Content), d.b.limits.MaxValuesPerKey); err != nil {
			return nil, err
		}
		out := make([]any, 0, len(n.Content))
		for _, child := range n.Content {
			v, err := d.value(child, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		if err := d.admitScalar(n.Value); err != nil {
			return nil, err
		}
		switch n.Tag {
		case "!!str", "!!bool", "!!int", "!!float", "!!null", "!!timestamp":
		default:
			return nil, errYAML
		}
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, errYAML
		}
		if date, ok := v.(time.Time); ok {
			v = date.Format(time.RFC3339Nano)
		}
		if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
			return nil, errYAML
		}
		if text, ok := scalarText(v); ok {
			if err := d.admitScalar(text); err != nil {
				return nil, err
			}
		}
		return v, nil
	default:
		return nil, errYAML
	}
}

func (d *yamlDecoder) admitScalar(s string) error {
	if !utf8.ValidString(s) || bytes.IndexByte([]byte(s), 0) >= 0 {
		return errYAML
	}
	if err := limitCount("frontmatter_value_bytes", len(s), d.b.limits.MaxValueBytes); err != nil {
		return err
	}
	return d.b.reserveStrings(len(s))
}

func scalarText(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		return "", false
	}
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
