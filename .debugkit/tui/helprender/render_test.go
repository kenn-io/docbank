package helprender

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/termtext"
)

func item(key, description string) helplayout.HelpItem {
	return helplayout.HelpItem{Key: key, Description: description}
}

func TestRenderHelpTable(t *testing.T) {
	tests := []struct {
		name   string
		rows   [][]helplayout.HelpItem
		width  int
		wanted string
	}{
		{
			name:   "nil input",
			rows:   nil,
			width:  40,
			wanted: "",
		},
		{
			name:   "two columns share the border and padding gap",
			rows:   [][]helplayout.HelpItem{{item("a", "one"), item("b", "two")}},
			width:  40,
			wanted: "a one▕ b two",
		},
		{
			name: "short row pads to the grid without a border",
			rows: [][]helplayout.HelpItem{
				{item("a", "one"), item("b", "two")},
				{item("c", "three")},
			},
			width:  40,
			wanted: "a one  ▕ b two\nc three       ",
		},
		{
			name:   "present item with empty text keeps its border",
			rows:   [][]helplayout.HelpItem{{item("a", "one"), item("", "")}},
			width:  40,
			wanted: "a one▕ ",
		},
		{
			name:   "key-only item renders without description spacing",
			rows:   [][]helplayout.HelpItem{{item("q", ""), item("x", "y")}},
			width:  40,
			wanted: "q▕ x y",
		},
		{
			name:   "wide runes size cells by terminal width",
			rows:   [][]helplayout.HelpItem{{item("日本", ""), item("b", "")}},
			width:  40,
			wanted: "日本▕ b",
		},
		{
			name:   "exact-fit width keeps one row",
			rows:   [][]helplayout.HelpItem{{item("a", "one"), item("b", "two")}},
			width:  12,
			wanted: "a one▕ b two",
		},
		{
			name:   "one cell below fit splits into single-column rows",
			rows:   [][]helplayout.HelpItem{{item("a", "one"), item("b", "two")}},
			width:  11,
			wanted: "a one\nb two",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reflowed := helplayout.ReflowRows(test.rows, test.width, ColumnGap)
			assert.Equal(t, test.wanted, RenderHelpTable(reflowed, Styles{}))
		})
	}
}

func TestRenderHelpTableStylesKeepGeometry(t *testing.T) {
	rows := [][]helplayout.HelpItem{
		{item("enter", "open"), item("q", "quit")},
		{item("g/G", "top/bottom")},
	}
	reflowed := helplayout.ReflowRows(rows, 60, ColumnGap)

	plain := RenderHelpTable(reflowed, Styles{})

	tests := []struct {
		name   string
		styles Styles
	}{
		{"key", Styles{Key: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("246"))}},
		{"description", Styles{Description: lipgloss.NewStyle().Foreground(lipgloss.Color("240"))}},
		{"border color", Styles{BorderColor: lipgloss.Color("242")}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			styled := RenderHelpTable(reflowed, test.styles)
			assert.NotEqual(t, plain, styled)
			assert.Equal(t, plain, termtext.StripANSI(styled))
		})
	}
}

func TestRenderHelpTableDoesNotMutateInput(t *testing.T) {
	rows := [][]helplayout.HelpItem{{item("a", "one"), item("b", "two")}}
	RenderHelpTable(rows, Styles{})
	assert.Equal(t, [][]helplayout.HelpItem{{item("a", "one"), item("b", "two")}}, rows)
}
