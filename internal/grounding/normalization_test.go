package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizedFigures(t *testing.T) {
	for _, tc := range []struct {
		text    string
		figures []normalizedFigure
	}{
		{"CAD , : \t$40", []normalizedFigure{{text: "$40"}}},
		{"40\nUSD", []normalizedFigure{{text: "40"}}},
		{"USD\n40", []normalizedFigure{{text: "40"}}},
		{"$40,\nCAD", []normalizedFigure{{"$40", "amount", "USD 40"}}},
		{"CAD :\n$40", []normalizedFigure{{"$40", "amount", "USD 40"}}},
		{"CAD$40", []normalizedFigure{{text: "CAD$40"}}},
		{"CAD USD 40", []normalizedFigure{{text: "40"}}},
		{"2 USD40", []normalizedFigure{{text: "2"}, {"USD40", "amount", "USD 40"}}},
		{"'-USD,' $40", []normalizedFigure{{text: "$40"}}},
		{"$40 '-USD,' 50", []normalizedFigure{{text: "$40"}, {text: "50"}}},
		{"$40CAD", []normalizedFigure{{text: "$40CAD"}}},
		{"-_USD5.00_", []normalizedFigure{{text: "USD5.00"}}},
		{"+_$5.00_", []normalizedFigure{{text: "$5.00"}}},
		{"−_5.00 USD", []normalizedFigure{{text: "5.00"}}},
		{"'±_.,USD5.00_'", []normalizedFigure{{text: "USD5.00"}}},
		{"USD-5.00", []normalizedFigure{{text: "USD-5.00"}}},
		{"-USD 5.00 USD", []normalizedFigure{{text: "5.00"}}},
		{"15\u2009March\u00852026", []normalizedFigure{{"15\u2009March\u00852026", "date", "2026-03-15"}}},
		{"1,250 USD", []normalizedFigure{{"1,250 USD", "amount", "USD 1250"}}},
		{"$0040.50", []normalizedFigure{{"$0040.50", "amount", "USD 40.5"}}},
		{"$5 million", []normalizedFigure{{"$5", "amount", "USD 5"}}},
		{"CAD 40", []normalizedFigure{{"CAD 40", "amount", "CAD 40"}}},
		{"2026-03-15T11:00:00Z", []normalizedFigure{{text: "2026-03-15T11:00:00Z"}}},
		{"2026-02-30", []normalizedFigure{{text: "2026-02-30"}}},
		{`"$40..."`, []normalizedFigure{{"$40", "amount", "USD 40"}}},
		{"$40..5", []normalizedFigure{{text: "$40..5"}}},
		{"$1,2,3", []normalizedFigure{{text: "$1,2,3"}}},
		{"$40 CAD.", []normalizedFigure{{text: "$40"}}},
		{"_March 15, 2026_", []normalizedFigure{{"March 15, 2026", "date", "2026-03-15"}}},
		{"INV_$40_", []normalizedFigure{{text: "INV_$40"}}},
		{"USD 40CAD 30", []normalizedFigure{{text: "40CAD"}, {text: "30"}}},
		// Only ISO codes can follow an amount as a currency suffix.
		{"1250$", []normalizedFigure{{text: "1250$"}}},
		{"$10,000", []normalizedFigure{{"$10,000", "amount", "USD 10000"}}},
		// A comma group's leading segment has at most three digits.
		{"$1250,000", []normalizedFigure{{text: "$1250,000"}}},
		{"$1.25", []normalizedFigure{{"$1.25", "amount", "USD 1.25"}}},
		{"$1.234", []normalizedFigure{{text: "$1.234"}}},
		{"Sept. 5, 2026", []normalizedFigure{{"Sept. 5, 2026", "date", "2026-09-05"}}},
		{"June\n5, 2026", []normalizedFigure{{"June\n5, 2026", "date", "2026-06-05"}}},
		{"USD 20, CAD .5", []normalizedFigure{{text: "20"}, {text: ".5"}}},
		{"$20 CAD30", []normalizedFigure{{"$20", "amount", "USD 20"}, {"CAD30", "amount", "CAD 30"}}},
		{"40USD 30CAD", []normalizedFigure{{"40USD", "amount", "USD 40"}, {"30CAD", "amount", "CAD 30"}}},
		{"-5 USD 30 CAD", []normalizedFigure{{text: "-5"}, {"30 CAD", "amount", "CAD 30"}}},
		{"-5 USD 30", []normalizedFigure{{text: "-5"}, {text: "30"}}},
		{"-$5 USD 30", []normalizedFigure{{text: "-$5"}, {text: "30"}}},
		{"1.234 USD 30", []normalizedFigure{{text: "1.234"}, {text: "30"}}},
		{"USD 30 February 2026", []normalizedFigure{{text: "30"}, {text: "2026"}}},
		{"30 February 2026 USD 30", []normalizedFigure{{text: "30"}, {text: "2026"}, {text: "30"}}},
		{"February 30, 2026 USD 30", []normalizedFigure{{text: "30"}, {text: "2026"}, {text: "30"}}},
		{"2026-02-30 USD 30", []normalizedFigure{{text: "2026-02-30"}, {text: "30"}}},
		{"USD 30 February 2026 USD 30", []normalizedFigure{{text: "30"}, {text: "2026"}, {text: "30"}}},
		{"CAD 15 March 2026", []normalizedFigure{{"15 March 2026", "date", "2026-03-15"}}},
		{"40 CAD 15 March 2026", []normalizedFigure{{"40 CAD", "amount", "CAD 40"}, {"15 March 2026", "date", "2026-03-15"}}},
		{"USD 20\n\nCAD 30", []normalizedFigure{{"USD 20", "amount", "USD 20"}, {"CAD 30", "amount", "CAD 30"}}},
		{"USD 40 2026-03-15", []normalizedFigure{{"USD 40", "amount", "USD 40"}, {"2026-03-15", "date", "2026-03-15"}}},
		// A marker after an amount leaves that amount literal, whatever follows.
		{"$40 CAD 15 March 2026", []normalizedFigure{{text: "$40"}, {"15 March 2026", "date", "2026-03-15"}}},
		{"USD -5 EUR 30", []normalizedFigure{{text: "-5"}, {text: "30"}}},
	} {
		t.Run(strings.ReplaceAll(tc.text, "\n", `\n`), func(t *testing.T) {
			require.Equal(t, tc.figures, normalizedFigures(tc.text))
		})
	}
}
