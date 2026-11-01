package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStandaloneCurrencyMarkers(t *testing.T) {
	for _, marker := range []string{"-€", "+£", "±$", "−EUR", ".€", "€.:", "'-USD,'"} {
		t.Run(marker, func(t *testing.T) {
			require.Equal(t, []normalizedFigure{{text: "$40"}}, normalizedFigures(marker+" $40"))
			require.Equal(t, []normalizedFigure{{text: "$40"}, {text: "50"}}, normalizedFigures("$40 "+marker+" 50"))
		})
	}
}

func TestNormalizedFigures(t *testing.T) {
	for _, tc := range []struct {
		text    string
		figures []normalizedFigure
	}{
		{"CAD , : \t$40", []normalizedFigure{{text: "$40"}}},
		{"$40 , : \tCAD", []normalizedFigure{{text: "$40"}}},
		{"40\nUSD", []normalizedFigure{{text: "40"}}},
		{"USD\n40", []normalizedFigure{{text: "40"}}},
		{"$\n40", []normalizedFigure{{text: "40"}}},
		{"$40,\nCAD", []normalizedFigure{{"$40", "amount", "USD 40"}}},
		{"CAD :\n$40", []normalizedFigure{{"$40", "amount", "USD 40"}}},
		// A marker after an amount leaves that amount literal, whatever follows.
		{"$20 CAD -30EUR", []normalizedFigure{{text: "$20"}, {text: "-30EUR"}}},
		{"$20 CAD .5EUR", []normalizedFigure{{text: "$20"}, {text: ".5EUR"}}},
		{"$40 CAD 15 March 2026", []normalizedFigure{{text: "$40"}, {"15 March 2026", "date", "2026-03-15"}}},
		{"USD -5 EUR 30", []normalizedFigure{{text: "-5"}, {"EUR 30", "amount", "EUR 30"}}},
		{"USD +5 EUR 30", []normalizedFigure{{text: "+5"}, {"EUR 30", "amount", "EUR 30"}}},
		{"USD ±.5 EUR 30", []normalizedFigure{{text: "±.5"}, {"EUR 30", "amount", "EUR 30"}}},
		{"USD −5 EUR 30", []normalizedFigure{{text: "−5"}, {"EUR 30", "amount", "EUR 30"}}},
		{"2026-03-155", []normalizedFigure{{text: "2026-03-155"}}},
		{"'CAD $40'", []normalizedFigure{{text: "$40"}}},
		{"CAD$40", []normalizedFigure{{text: "CAD$40"}}},
		{"CAD USD 40", []normalizedFigure{{text: "40"}}},
		{"-$ USD 40", []normalizedFigure{{text: "40"}}},
		{"$40CAD", []normalizedFigure{{text: "$40CAD"}}},
		{"-USD5.00", []normalizedFigure{{text: "USD5.00"}}},
		{"+USD5.00", []normalizedFigure{{text: "USD5.00"}}},
		{"−USD5.00", []normalizedFigure{{text: "USD5.00"}}},
		{"±USD5.00", []normalizedFigure{{text: "USD5.00"}}},
		{"USD-5.00", []normalizedFigure{{text: "USD-5.00"}}},
		{"-5.00 USD", []normalizedFigure{{text: "-5.00"}}},
		{"-USD 5.00 USD", []normalizedFigure{{text: "5.00"}}},
		{"15\u2009March\u00852026", []normalizedFigure{{"15\u2009March\u00852026", "date", "2026-03-15"}}},
		{"1,250 USD", []normalizedFigure{{"1,250 USD", "amount", "USD 1250"}}},
		{"$0040", []normalizedFigure{{"$0040", "amount", "USD 40"}}},
		{"$5 million", []normalizedFigure{{"$5", "amount", "USD 5"}}},
		{"CAD 40", []normalizedFigure{{"CAD 40", "amount", "CAD 40"}}},
		{"2026-03-15T11:00:00Z", []normalizedFigure{{text: "2026-03-15T11:00:00Z"}}},
		{"INV-2026-03-15", []normalizedFigure{{text: "INV-2026-03-15"}}},
		{"2026-02-30", []normalizedFigure{{text: "2026-02-30"}}},
		{`"$40..."`, []normalizedFigure{{"$40", "amount", "USD 40"}}},
		{"$40..5", []normalizedFigure{{text: "$40..5"}}},
		{"$1,2,3...", []normalizedFigure{{text: "$1,2,3"}}},
		{"$40 CAD.", []normalizedFigure{{text: "$40"}}},
		{"_March 15, 2026_", []normalizedFigure{{"March 15, 2026", "date", "2026-03-15"}}},
		{"INV_$40_", []normalizedFigure{{text: "INV_$40"}}},
		{"USD 40CAD 30", []normalizedFigure{{text: "40CAD"}, {text: "30"}}},
		{"$40 USD 50-60", []normalizedFigure{{text: "$40"}, {text: "50-60"}}},
		// Only ISO codes can follow an amount as a currency suffix.
		{"1250$", []normalizedFigure{{text: "1250$"}}},
		{"$10,000", []normalizedFigure{{"$10,000", "amount", "USD 10000"}}},
		// A comma group's leading segment has at most three digits.
		{"$1250,000", []normalizedFigure{{text: "$1250,000"}}},
		{"$1.25", []normalizedFigure{{"$1.25", "amount", "USD 1.25"}}},
		{"$1.234", []normalizedFigure{{text: "$1.234"}}},
		{"Sept. 5, 2026", []normalizedFigure{{"Sept. 5, 2026", "date", "2026-09-05"}}},
		{"June\n5, 2026", []normalizedFigure{{"June\n5, 2026", "date", "2026-06-05"}}},
		{"| May 3 | 2026 |", []normalizedFigure{{text: "3"}, {text: "2026"}}},
		{"USD 20, CAD .5", []normalizedFigure{{text: "20"}, {"CAD .5", "amount", "CAD 0.5"}}},
		{"$20 CAD30", []normalizedFigure{{"$20", "amount", "USD 20"}, {"CAD30", "amount", "CAD 30"}}},
		{"40USD 30CAD", []normalizedFigure{{"40USD", "amount", "USD 40"}, {"30CAD", "amount", "CAD 30"}}},
		{"-5 USD 30 CAD", []normalizedFigure{{text: "-5"}, {text: "30"}}},
		{"CAD 15 March 2026", []normalizedFigure{{"15 March 2026", "date", "2026-03-15"}}},
		{"40 CAD 15 March 2026", []normalizedFigure{{"40 CAD", "amount", "CAD 40"}, {"15 March 2026", "date", "2026-03-15"}}},
		{"USD 20\n\nCAD 30", []normalizedFigure{{"USD 20", "amount", "USD 20"}, {"CAD 30", "amount", "CAD 30"}}},
		{"USD 40 2026-03-15", []normalizedFigure{{"USD 40", "amount", "USD 40"}, {"2026-03-15", "date", "2026-03-15"}}},
		{"USD 20 and CAD 30", []normalizedFigure{{"USD 20", "amount", "USD 20"}, {"CAD 30", "amount", "CAD 30"}}},
	} {
		t.Run(strings.ReplaceAll(tc.text, "\n", `\n`), func(t *testing.T) {
			require.Equal(t, tc.figures, normalizedFigures(tc.text))
		})
	}
}
