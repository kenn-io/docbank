package splitlayout

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Mode discriminates the stacked single-view layout from the two-pane
// split layout.
type Mode int

const (
	Stacked Mode = iota
	Split
)

// MinWidth and MinHeight are the terminal thresholds below which split
// layout degrades to stacked.
const (
	MinWidth  = 140
	MinHeight = 36
)

// PickLayout returns Split exactly when the terminal meets both
// thresholds.
func PickLayout(width, height int) Mode {
	if width >= MinWidth && height >= MinHeight {
		return Split
	}
	return Stacked
}

// Config carries an application's split-pane sizing constants. Values are
// nonnegative with ListMinWidth <= ListMaxWidth; the zero value applies
// literal zeros rather than selecting defaults.
type Config struct {
	// ListMinWidth and ListMaxWidth clamp the list pane's border-box
	// width.
	ListMinWidth int
	ListMaxWidth int
	// DetailReservedWidth is the detail pane's first-allocation budget:
	// the list pane receives what remains of the terminal beyond it,
	// within its clamps.
	DetailReservedWidth int
	// DetailMinWidth floors the detail pane's width; zero disables the
	// floor and lets an undersized terminal produce a zero or negative
	// detail width.
	DetailMinWidth int
	// MinBodyHeight floors the pane body height.
	MinBodyHeight int
}

// Geom is the split-pane rectangle set for one terminal size. Outer
// dimensions are border-box; inner dimensions subtract the 1-cell border
// on each side.
type Geom struct {
	ListOuterW   int
	DetailOuterW int
	BodyH        int
	ListInnerW   int
	ListInnerH   int
	DetailInnerW int
	DetailInnerH int
}

// ListWidth returns the list pane's border-box width for the terminal
// width: the remainder beyond DetailReservedWidth, clamped to the list
// pane's bounds.
func (c Config) ListWidth(width int) int {
	return min(max(width-c.DetailReservedWidth, c.ListMinWidth), c.ListMaxWidth)
}

// Geometry computes pane rectangles for the terminal size. Bands:
// title(1) + body(BodyH) + info(1) + footer(footerLines); footerLines
// comes from the caller's actual footer layout.
func (c Config) Geometry(width, height, footerLines int) Geom {
	listW := c.ListWidth(width)
	detailW := width - listW
	if c.DetailMinWidth > 0 {
		detailW = max(detailW, c.DetailMinWidth)
	}
	bodyH := max(height-2-footerLines, c.MinBodyHeight)
	return Geom{
		ListOuterW:   listW,
		DetailOuterW: detailW,
		BodyH:        bodyH,
		ListInnerW:   listW - 2,
		ListInnerH:   bodyH - 2,
		DetailInnerW: detailW - 2,
		DetailInnerH: bodyH - 2,
	}
}

// PaneStyle wraps already-rendered pane content in the standard
// single-line border at the given border-box dimensions. Callers resolve
// the border color from their theme and focus state; a nil color leaves
// the border uncolored.
func PaneStyle(border color.Color, outerW, outerH int) lipgloss.Style {
	style := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		Width(outerW).
		Height(outerH)
	if border != nil {
		style = style.BorderForeground(border)
	}
	return style
}
