package notemarkdown

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/text"
)

type blockSourceSnapshot struct {
	kind     string
	pos      int
	blank    bool
	level    int
	source   []text.Segment
	children []blockSourceSnapshot
}

func snapshotBlocks(n ast.Node) blockSourceSnapshot {
	block := n.(ast.BlockNode)
	s := blockSourceSnapshot{
		kind:   fmt.Sprintf("%T", n),
		pos:    n.Pos(),
		blank:  block.HasBlankPreviousLines(),
		source: append([]text.Segment(nil), block.Source()...),
	}
	if heading, ok := n.(*ast.Heading); ok {
		s.level = heading.Level
	}
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		if _, ok := child.(ast.BlockNode); ok {
			s.children = append(s.children, snapshotBlocks(child))
		}
	}
	return s
}

func TestBlockDriverMatchesGoldmarkBlockSources(t *testing.T) {
	fixtures := []struct {
		name string
		src  string
	}{
		{"setext and ordinary continuation", "first line\r\nsecond line\r\n---\r\n\r\n# next\r\n"},
		{"nested quote list and lazy continuation", "> - first\n>   second\n>\n>   - nested\n>     lazy text\noutside\n"},
		{"ordered interruption and blank list item", "paragraph\n\n1. first\n   continuation\n\n   second paragraph\n2. next\n"},
		{"reference removal before setext retry", "[ref]: target.md\n---\n\nkept paragraph\n"},
		{"indented and fenced code source-less nodes", "    indented\n\n```go\n\nbody\n```\n\n# heading\n"},
		{"html block interruption", "paragraph\n<div>\nraw\n</div>\n\n> quote\n"},
		{"ATX markers trimming and disabled attributes", "#\n### \n# ###\n# text   ### \t\n####### rejected\n#no space\n\n# {#literal}\n"},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := t.Context()
			src := []byte(fixture.src)
			goldEnds := map[ast.Node]int{}
			goldGuarded := newBlockParserSet(ctx, &budget{}, goldEnds)
			goldRefs := map[string]string{}
			goldTransformer := &referenceGuard{ctx: ctx, b: &budget{}, refs: goldRefs}
			goldRoot := newGoldmarkBlockParser(goldGuarded, goldTransformer).Parse(src)

			driverEnds := map[ast.Node]int{}
			driverBudget := &budget{}
			driverGuarded := newBlockParserSet(ctx, driverBudget, driverEnds)
			driverRefs := map[string]string{}
			driverTransformer := &referenceGuard{ctx: ctx, b: driverBudget, refs: driverRefs}
			driverRoot := parseBlockDocument(ctx, src, driverGuarded, driverTransformer, driverBudget)

			require.Equal(t, goldRefs, driverRefs)
			require.Equal(t, snapshotBlocks(goldRoot), snapshotBlocks(driverRoot))
		})
	}
}
