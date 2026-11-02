package screen_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/screen"
)

const (
	red          = "\x1b[31m"
	redBG        = "\x1b[41m"
	blue         = "\x1b[34m"
	reset        = "\x1b[0m"
	hideCursor   = "\x1b[?25l"
	showCursor   = "\x1b[?25h"
	openLink     = "\x1b]8;;https://example.test\x1b\\"
	closeLink    = "\x1b]8;;\x1b\\"
	openLinkBEL  = "\x1b]8;;https://example.test\a"
	closeLinkBEL = "\x1b]8;;\a"
)

func TestWidth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "empty", in: "", want: 0},
		{name: "ascii", in: "abcd", want: 4},
		{name: "wide rune", in: "a界b", want: 4},
		{name: "styled", in: red + "abcd" + reset, want: 4},
		{name: "CSI control", in: hideCursor + "ab" + showCursor, want: 2},
		{name: "OSC with string terminator", in: openLink + "link" + closeLink, want: 4},
		{name: "OSC with BEL terminator", in: openLinkBEL + "link" + closeLinkBEL, want: 4},
		{name: "widest line", in: "abc\n界界\nx", want: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, screen.Width(tt.in))
		})
	}
}

func TestPrefixAndTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		maxWidth  int
		want      string
		wantWidth int
	}{
		{name: "empty", in: "", maxWidth: 3, want: "", wantWidth: 0},
		{name: "non-positive width", in: red + "abc" + reset, maxWidth: 0, want: "", wantWidth: 0},
		{name: "negative width", in: "abc", maxWidth: -1, want: "", wantWidth: 0},
		{name: "short input unchanged", in: "abc", maxWidth: 5, want: "abc", wantWidth: 3},
		{name: "ascii truncation", in: "abcde", maxWidth: 3, want: "abc", wantWidth: 3},
		{name: "styled truncation retains complete CSI", in: red + "abcde" + reset, maxWidth: 3, want: red + "abc" + reset, wantWidth: 3},
		{name: "wide rune exact boundary", in: red + "ab界cd" + reset, maxWidth: 4, want: red + "ab界" + reset, wantWidth: 4},
		{name: "wide rune crossing boundary", in: red + "ab界cd" + reset, maxWidth: 3, want: red + "ab" + reset, wantWidth: 2},
		{name: "CSI controls preserved", in: hideCursor + "abc" + showCursor, maxWidth: 1, want: hideCursor + "a" + showCursor, wantWidth: 1},
		{name: "OSC string terminator preserved", in: openLink + "link" + closeLink, maxWidth: 2, want: openLink + "li" + closeLink, wantWidth: 2},
		{name: "OSC BEL terminator preserved", in: openLinkBEL + "link" + closeLinkBEL, maxWidth: 2, want: openLinkBEL + "li" + closeLinkBEL, wantWidth: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)

			got, gotWidth := screen.Prefix(tt.in, tt.maxWidth)
			require.Equal(tt.want, got)
			require.Equal(tt.wantWidth, gotWidth)
			require.Equal(tt.want, screen.Truncate(tt.in, tt.maxWidth))
		})
	}
}

func TestSuffix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		skip int
		want string
	}{
		{name: "empty", in: "", skip: 3, want: ""},
		{name: "zero skip", in: red + "abc" + reset, skip: 0, want: red + "abc" + reset},
		{name: "negative skip", in: "abc", skip: -1, want: "abc"},
		{name: "ascii boundary", in: "abcde", skip: 2, want: "cde"},
		{name: "past end", in: red + "abc" + reset, skip: 20, want: ""},
		{name: "style active at boundary is replayed", in: red + "ab界cd" + reset, skip: 2, want: red + "界cd" + reset},
		{name: "wide rune crossing boundary is dropped", in: red + "ab界cd" + reset, skip: 3, want: red + "cd" + reset},
		{name: "wide rune exact boundary", in: red + "ab界cd" + reset, skip: 4, want: red + "cd" + reset},
		{name: "reset before suffix is replayed", in: red + "ab" + reset + "cd", skip: 2, want: red + reset + "cd"},
		{name: "CSI controls are complete", in: hideCursor + "abc" + showCursor, skip: 1, want: hideCursor + "bc" + showCursor},
		{name: "OSC string terminator is complete", in: openLink + "link" + closeLink, skip: 2, want: openLink + "nk" + closeLink},
		{name: "OSC BEL terminator is complete", in: openLinkBEL + "link" + closeLinkBEL, skip: 2, want: openLinkBEL + "nk" + closeLinkBEL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, screen.Suffix(tt.in, tt.skip))
		})
	}
}

func TestSplice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		background  string
		replacement string
		column      int
		width       int
		want        string
	}{
		{name: "replace cells", background: "0123456789", replacement: "XX", column: 3, width: 2, want: "012XX56789"},
		{name: "pad short replacement", background: "0123456789", replacement: "XX", column: 3, width: 4, want: "012XX  789"},
		{name: "truncate long replacement", background: "0123456789", replacement: "ABCDE", column: 2, width: 3, want: "01ABC56789"},
		{name: "pad short background", background: "ab", replacement: "X", column: 4, width: 1, want: "ab  X"},
		{name: "clip left", background: "0123456789", replacement: "ABCDE", column: -2, width: 5, want: "CDE3456789"},
		{name: "clip right by replacement width", background: "0123456789", replacement: "ABCDE", column: 8, width: 5, want: "01234567ABCDE"},
		{name: "fully left is no-op", background: "0123", replacement: "ABCDE", column: -5, width: 5, want: "0123"},
		{name: "non-positive width is no-op", background: "0123", replacement: "X", column: 1, width: 0, want: "0123"},
		{name: "empty replacement erases cells", background: "012345", replacement: "", column: 2, width: 2, want: "01  45"},
		{name: "boundary through wide background rune", background: "A界BC", replacement: "X", column: 2, width: 1, want: "A XBC"},
		{name: "left clip through wide replacement preserves cell", background: "....", replacement: "界X", column: -1, width: 3, want: " X.."},
		{name: "right cut through wide background preserves cell", background: "A界BC", replacement: "X", column: 1, width: 1, want: "AX BC"},
		{name: "both cuts through wide background preserve cells", background: "界A界B", replacement: "XYZ", column: 1, width: 3, want: " XYZ B"},
		{
			name:        "left clipped wide replacement styles synthetic cell",
			background:  "....",
			replacement: redBG + "界X" + reset,
			column:      -1,
			width:       3,
			want:        redBG + " X" + reset + "..",
		},
		{
			name:        "right cut through wide background styles synthetic cell",
			background:  redBG + "A界BC" + reset,
			replacement: "X",
			column:      1,
			width:       1,
			want:        redBG + "A" + reset + "X" + redBG + " BC" + reset,
		},
		{
			name:        "left cut through wide background styles synthetic cell",
			background:  redBG + "界BC" + reset,
			replacement: "X",
			column:      1,
			width:       1,
			want:        redBG + " " + reset + "X" + redBG + "BC" + reset,
		},
		{
			name:        "right clipped wide replacement styles synthetic cell",
			background:  "....",
			replacement: redBG + "A界" + reset,
			column:      0,
			width:       2,
			want:        redBG + "A " + reset + "..",
		},
		{
			name:        "styled background state is replayed after replacement",
			background:  red + "012345" + reset,
			replacement: blue + "X" + reset,
			column:      2,
			width:       2,
			want:        red + "01" + reset + blue + "X" + reset + " " + red + "45" + reset,
		},
		{
			name:        "OSC background remains linked after replacement",
			background:  openLink + "abcdef" + closeLink,
			replacement: "X",
			column:      2,
			width:       2,
			want:        openLink + "ab" + closeLink + "X " + openLink + "ef" + closeLink,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, screen.Splice(
				tt.background, tt.replacement, tt.column, tt.width,
			))
		})
	}
}

func TestOverlayAt(t *testing.T) {
	t.Parallel()

	dots6x3 := strings.Join([]string{"......", "......", "......"}, "\n")
	dots4x2 := strings.Join([]string{"....", "...."}, "\n")

	tests := []struct {
		name          string
		background    string
		panel         string
		width, height int
		row, column   int
		want          string
	}{
		{
			name:       "places panel at zero-based coordinates",
			background: dots6x3,
			panel:      "X",
			width:      6,
			height:     3,
			row:        1,
			column:     2,
			want:       strings.Join([]string{"......", "..X...", "......"}, "\n"),
		},
		{
			name:       "pads short background to height",
			background: "row1",
			panel:      "X",
			width:      6,
			height:     3,
			row:        1,
			column:     1,
			want:       "row1\n X\n",
		},
		{
			name:       "empty background",
			background: "",
			panel:      "X",
			width:      2,
			height:     2,
			row:        0,
			column:     0,
			want:       "X\n",
		},
		{
			name:       "empty panel is exact no-op",
			background: "short",
			panel:      "",
			width:      20,
			height:     5,
			row:        2,
			column:     3,
			want:       "short",
		},
		{
			name:       "oversized panel clips to viewport",
			background: dots4x2,
			panel:      "ABCDEFGHIJ\nKLMNOPQRST\nUVWXYZ",
			width:      4,
			height:     2,
			row:        0,
			column:     0,
			want:       "ABCD\nKLMN",
		},
		{
			name:       "negative position clips top and left",
			background: dots4x2,
			panel:      "ABCDE\nFGHIJ\nKLMNO",
			width:      4,
			height:     2,
			row:        -1,
			column:     -2,
			want:       "HIJ.\nMNO.",
		},
		{
			name:       "left clip through wide panel preserves cell",
			background: "....",
			panel:      "界X",
			width:      4,
			height:     1,
			row:        0,
			column:     -1,
			want:       " X..",
		},
		{
			name:       "left clipped wide panel styles synthetic cell",
			background: "....",
			panel:      redBG + "界X" + reset,
			width:      4,
			height:     1,
			row:        0,
			column:     -1,
			want:       redBG + " X" + reset + "..",
		},
		{
			name:       "both viewport cuts through wide panel preserve cells",
			background: "....",
			panel:      "界A界",
			width:      3,
			height:     1,
			row:        0,
			column:     -1,
			want:       " A .",
		},
		{
			name:       "right viewport cut through wide panel preserves cell",
			background: "....",
			panel:      "A界",
			width:      2,
			height:     1,
			row:        0,
			column:     0,
			want:       "A ..",
		},
		{
			name:       "right clipped wide panel styles synthetic cell",
			background: "....",
			panel:      redBG + "A界" + reset,
			width:      2,
			height:     1,
			row:        0,
			column:     0,
			want:       redBG + "A " + reset + "..",
		},
		{
			name:       "right and bottom overflow clips",
			background: dots4x2,
			panel:      "AB\nCD",
			width:      4,
			height:     2,
			row:        1,
			column:     3,
			want:       "....\n...A",
		},
		{
			name:       "fully offscreen right is no-op",
			background: dots4x2,
			panel:      "AB",
			width:      4,
			height:     2,
			row:        0,
			column:     4,
			want:       dots4x2,
		},
		{
			name:       "fully offscreen bottom is no-op",
			background: dots4x2,
			panel:      "AB",
			width:      4,
			height:     2,
			row:        2,
			column:     0,
			want:       dots4x2,
		},
		{
			name:       "exact bottom-right boundary",
			background: dots4x2,
			panel:      "X",
			width:      4,
			height:     2,
			row:        1,
			column:     3,
			want:       "....\n...X",
		},
		{
			name:       "jagged panel is rectangular",
			background: dots4x2,
			panel:      "AB\nX",
			width:      4,
			height:     2,
			row:        0,
			column:     1,
			want:       ".AB.\n.X .",
		},
		{
			name:       "styled background",
			background: red + "......" + reset,
			panel:      "X",
			width:      6,
			height:     1,
			row:        0,
			column:     2,
			want:       red + ".." + reset + "X" + red + "..." + reset,
		},
		{
			name:       "independently balanced multiline SGR and OSC state",
			background: red + "abcd" + reset + "\n" + openLink + "efgh" + closeLink,
			panel:      "X\nY",
			width:      4,
			height:     2,
			row:        0,
			column:     1,
			want: red + "a" + reset + "X" + red + "cd" + reset + "\n" +
				openLink + "e" + closeLink + "Y" + openLink + "gh" + closeLink,
		},
		{
			name:       "rows beyond viewport are unchanged",
			background: "....\nkeep",
			panel:      "XX\nYY",
			width:      4,
			height:     1,
			row:        0,
			column:     1,
			want:       ".XX.\nkeep",
		},
		{
			name:       "zero-sized viewport is no-op",
			background: "abc",
			panel:      "X",
			width:      0,
			height:     0,
			row:        0,
			column:     0,
			want:       "abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, screen.OverlayAt(
				tt.background, tt.panel, tt.width, tt.height, tt.row, tt.column,
			))
		})
	}
}

func TestOverlayCentered(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		background    string
		panel         string
		width, height int
		want          string
	}{
		{
			name:       "centers with extra cells on bottom and right",
			background: strings.Join([]string{"......", "......", "......", "......", "......"}, "\n"),
			panel:      "XX\nYY",
			width:      6,
			height:     5,
			want:       strings.Join([]string{"......", "..XX..", "..YY..", "......", "......"}, "\n"),
		},
		{
			name:       "pads a short background before centering",
			background: "top",
			panel:      "X",
			width:      5,
			height:     3,
			want:       "top\n  X\n",
		},
		{
			name:       "oversized panel is centered and clipped",
			background: "....\n....",
			panel:      "abcdef\nghijkl\nmnopqr\nstuvwx",
			width:      4,
			height:     2,
			want:       "hijk\nnopq",
		},
		{
			name:       "odd oversized difference biases top-left",
			background: "....",
			panel:      "ABCDE",
			width:      4,
			height:     1,
			want:       "ABCD",
		},
		{
			name:       "empty panel is exact no-op",
			background: "short",
			panel:      "",
			width:      10,
			height:     10,
			want:       "short",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, screen.OverlayCentered(
				tt.background, tt.panel, tt.width, tt.height,
			))
		})
	}
}

func TestOverlayIsDeterministic(t *testing.T) {
	t.Parallel()

	background := red + "abcdef" + reset + "\n" + openLink + "ghijkl" + closeLink
	panel := blue + "界" + reset + "\nX"
	want := screen.OverlayAt(background, panel, 6, 2, 0, 2)

	for range 20 {
		require.Equal(t, want, screen.OverlayAt(background, panel, 6, 2, 0, 2))
	}
}
