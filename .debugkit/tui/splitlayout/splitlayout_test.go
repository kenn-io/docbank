package splitlayout_test

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/kit/tui/splitlayout"
	"go.kenn.io/kit/tui/termtext"
)

// The two known consumers' sizing policies, pinned here so geometry
// changes surface as test failures rather than silent drift.
var (
	detailFirst = splitlayout.Config{
		ListMinWidth:        50,
		ListMaxWidth:        90,
		DetailReservedWidth: 100,
		MinBodyHeight:       5,
	}
	wideList = splitlayout.Config{
		ListMinWidth:        68,
		ListMaxWidth:        110,
		DetailReservedWidth: 100,
		DetailMinWidth:      20,
		MinBodyHeight:       4,
	}
)

func TestPickLayout(t *testing.T) {
	tests := []struct {
		name          string
		width, height int
		wanted        splitlayout.Mode
	}{
		{"fits both thresholds", 140, 36, splitlayout.Split},
		{"one column short", 139, 36, splitlayout.Stacked},
		{"one row short", 140, 35, splitlayout.Stacked},
		{"large terminal", 300, 80, splitlayout.Split},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.wanted, splitlayout.PickLayout(test.width, test.height))
		})
	}
}

func TestGeometry(t *testing.T) {
	tests := []struct {
		name                       string
		config                     splitlayout.Config
		width, height, footerLines int
		wanted                     splitlayout.Geom
	}{
		{
			name:   "detail-first config at the breakpoint",
			config: detailFirst, width: 140, height: 36, footerLines: 2,
			wanted: splitlayout.Geom{
				ListOuterW: 50, DetailOuterW: 90, BodyH: 32,
				ListInnerW: 48, ListInnerH: 30, DetailInnerW: 88, DetailInnerH: 30,
			},
		},
		{
			name:   "wide-list config at the breakpoint",
			config: wideList, width: 140, height: 36, footerLines: 2,
			wanted: splitlayout.Geom{
				ListOuterW: 68, DetailOuterW: 72, BodyH: 32,
				ListInnerW: 66, ListInnerH: 30, DetailInnerW: 70, DetailInnerH: 30,
			},
		},
		{
			name:   "mid-range width hands the detail pane its reservation",
			config: detailFirst, width: 180, height: 40, footerLines: 1,
			wanted: splitlayout.Geom{
				ListOuterW: 80, DetailOuterW: 100, BodyH: 37,
				ListInnerW: 78, ListInnerH: 35, DetailInnerW: 98, DetailInnerH: 35,
			},
		},
		{
			name:   "mid-range width with the wide-list config",
			config: wideList, width: 180, height: 40, footerLines: 1,
			wanted: splitlayout.Geom{
				ListOuterW: 80, DetailOuterW: 100, BodyH: 37,
				ListInnerW: 78, ListInnerH: 35, DetailInnerW: 98, DetailInnerH: 35,
			},
		},
		{
			name:   "list grows with the terminal until its cap",
			config: detailFirst, width: 200, height: 40, footerLines: 1,
			wanted: splitlayout.Geom{
				ListOuterW: 90, DetailOuterW: 110, BodyH: 37,
				ListInnerW: 88, ListInnerH: 35, DetailInnerW: 108, DetailInnerH: 35,
			},
		},
		{
			name:   "body floor binds under a tall footer",
			config: detailFirst, width: 140, height: 36, footerLines: 30,
			wanted: splitlayout.Geom{
				ListOuterW: 50, DetailOuterW: 90, BodyH: 5,
				ListInnerW: 48, ListInnerH: 3, DetailInnerW: 88, DetailInnerH: 3,
			},
		},
		{
			name:   "disabled detail floor lets an undersized call go negative",
			config: detailFirst, width: 40, height: 10, footerLines: 1,
			wanted: splitlayout.Geom{
				ListOuterW: 50, DetailOuterW: -10, BodyH: 7,
				ListInnerW: 48, ListInnerH: 5, DetailInnerW: -12, DetailInnerH: 5,
			},
		},
		{
			name:   "enabled detail floor binds on an undersized call",
			config: wideList, width: 40, height: 10, footerLines: 1,
			wanted: splitlayout.Geom{
				ListOuterW: 68, DetailOuterW: 20, BodyH: 7,
				ListInnerW: 66, ListInnerH: 5, DetailInnerW: 18, DetailInnerH: 5,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.config.Geometry(test.width, test.height, test.footerLines)
			assert.Equal(t, test.wanted, got)
			assert.Equal(t, test.wanted.ListOuterW, test.config.ListWidth(test.width))
		})
	}
}

func TestPaneStyleWrapsBorderBox(t *testing.T) {
	rendered := splitlayout.PaneStyle(nil, 10, 4).Render("ab")

	assert.Equal(t,
		"┌────────┐\n"+
			"│ab      │\n"+
			"│        │\n"+
			"└────────┘",
		rendered)
}

func TestPaneStyleColorsOnlyTheBorder(t *testing.T) {
	plain := splitlayout.PaneStyle(nil, 8, 3).Render("x")
	colored := splitlayout.PaneStyle(lipgloss.Color("205"), 8, 3).Render("x")

	assert.NotEqual(t, plain, colored)
	assert.Equal(t, plain, termtext.StripANSI(colored))
}
