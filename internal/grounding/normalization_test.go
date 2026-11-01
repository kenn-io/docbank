package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizedFigures(t *testing.T) {
	type testCase struct {
		answer, source string
		figures        []Figure
	}
	cases := []testCase{}
	for _, ambiguous := range []string{"CAD , : \t$40", "$40 , : \tCAD"} {
		cases = append(cases,
			testCase{"USD40", ambiguous, []Figure{{Text: "USD40", Status: "missing"}}},
			testCase{ambiguous, "USD40", []Figure{{Text: "$40", Status: "missing"}}},
			testCase{ambiguous, ambiguous, []Figure{{Text: "$40", Status: "found"}}},
		)
	}
	for _, tc := range []struct{ answer, source, status string }{
		{"$40", "40\nUSD", "missing"},
		{"$40", "USD\n40", "missing"},
		{"$40", "$\n40", "missing"},
		{"USD40", "$40,\nCAD", "found"},
		{"USD40", "CAD\n$40", "found"},
		{"USD40", "CAD :\n$40", "found"},
		{"USD40", "$40 :\nCAD", "found"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, []Figure{{Text: tc.answer, Status: tc.status}}})
	}
	for _, tc := range []struct{ answer, source, text, figure string }{
		{"USD40", "$40, CAD", "USD40", "missing"},
		{"USD20", "$20 CAD -30", "USD20", "found"},
		{"USD20", "$20 CAD +30", "USD20", "found"},
		{"USD20", "$20 CAD ±.5", "USD20", "found"},
		{"USD20", "$20 CAD −30", "USD20", "found"},
		{"USD20", "$20 CAD 30EUR", "USD20", "found"},
		{"CAD30", "$20 CAD 30EUR", "CAD30", "missing"},
		{"CAD30", "USD -5 EUR 30CAD", "CAD30", "missing"},
		{"EUR30", "USD -5 EUR 30", "EUR30", "found"},
		{"EUR30", "USD +5 EUR 30", "EUR30", "found"},
		{"EUR30", "USD ±.5 EUR 30", "EUR30", "found"},
		{"EUR30", "USD −5 EUR 30", "EUR30", "found"},
		{"USD40", "$40 CAD : end", "USD40", "missing"},
		{"$40, CAD", "USD40", "$40", "missing"},
		{"2026-03-155", "2026-03-15", "2026-03-155", "missing"},
		{"2026-03-15", "2026-03-155", "2026-03-15", "missing"},
		{"2026-03-155", "2026-03-155", "2026-03-155", "found"},
		{"USD40", "CAD $40", "USD40", "missing"},
		{"USD40", "'CAD $40'", "USD40", "missing"},
		{"USD40", "$40 'CAD'", "USD40", "missing"},
		{"USD40", "CAD$40", "USD40", "missing"},
		{"USD40", "CAD USD 40", "USD40", "missing"},
		{"$40", "$ USD40", "$40", "missing"},
		{"USD40", "$40CAD", "USD40", "missing"},
		{"CAD $40", "USD40", "$40", "missing"},
		{"-USD5.00", "$5", "USD5.00", "missing"},
		{"$5", "-USD5.00", "$5", "missing"},
		{"+USD5.00", "$5", "USD5.00", "missing"},
		{"−USD5.00", "$5", "USD5.00", "missing"},
		{"±USD5.00", "$5", "USD5.00", "missing"},
		{"USD-5.00", "$5", "USD-5.00", "missing"},
		{"-5.00 USD", "$5", "-5.00", "missing"},
		{"$5", "-USD 5.00 USD", "$5", "missing"},
		{"$5", "-USD 5USD", "$5", "missing"},
		{"March\u00a015,\u00a02026", "2026-03-15", "March\u00a015,\u00a02026", "found"},
		{"2026-03-15", "15\u2009March\u00852026", "2026-03-15", "found"},
		{"March 15, 2026", "2026-03-15", "March 15, 2026", "found"},
		{"March 16, 2026", "2026-03-15", "March 16, 2026", "missing"},
		{"$1,250", "1,250 USD", "$1,250", "found"},
		{"$1,250", "1,251 USD", "$1,250", "missing"},
		{"$0040", "USD 40", "$0040", "found"},
		{"$5000000", "$5 m", "$5000000", "missing"},
		{"$5 million", "USD 5000000", "$5", "missing"},
		{"$40", "CAD 40", "$40", "missing"},
		{"CAD 40", "$40", "CAD 40", "missing"},
		{"2026-03-15T11:00:00Z", "2026-03-15T10:00:00Z", "2026-03-15T11:00:00Z", "missing"},
		{"2026-03-15", "INV-2026-03-15", "2026-03-15", "missing"},
		{"2026-02-30", "March 2, 2026", "2026-02-30", "missing"},
		{"USD 40", `"$40..."`, "USD 40", "found"},
		{`"$40..."`, "USD 40", "$40", "found"},
		{"USD 40", "$40..5", "USD 40", "missing"},
		{"USD 123", "$1,2,3...", "USD 123", "missing"},
		{"USD40", "$40 CAD.", "USD40", "missing"},
		{"_$40_", "USD 40", "$40", "found"},
		{"2026-03-15", "_March 15, 2026_", "2026-03-15", "found"},
		{"_2026-03-15_", "March 15, 2026", "2026-03-15", "found"},
		{"$40", "INV_$40_", "$40", "missing"},
		{"2026-03-15", "INV_2026-03-15", "2026-03-15", "missing"},
		{"40 USD", "40 USD 2026-03-15", "40 USD", "found"},
		{"CAD 40", "USD 40CAD 30", "CAD 40", "missing"},
		{"CAD 40", "$40 USD 2026-03-15T10:00:00Z", "CAD 40", "missing"},
		{"CAD 40", "$40 USD 12:30", "CAD 40", "missing"},
		{"CAD 40", "$40 USD 50-60", "CAD 40", "missing"},
		{"USD 40", "CAD 40", "USD 40", "found"},
		{"$40", "40", "$40", "missing"},
		{"$40", "USD 400", "$40", "missing"},
		{"2026-03-15", "March" + strings.Repeat(" ", 64) + "15, 2026", "2026-03-15", "found"},
		{"$1,250", "USD 1250", "$1,250", "found"},
		{"$10,000", "$10", "$10,000", "missing"},
		{"$10", "$10,000", "$10", "missing"},
		{"$1,250", "$1.25", "$1,250", "missing"},
		{"$1.234", "$1,234", "$1.234", "missing"},
		{"2026-09-05", "Sept. 5, 2026", "2026-09-05", "found"},
		{"2026-06-05", "June 5,\n2026", "2026-06-05", "found"},
		{"2026-06-05", "June\n5, 2026", "2026-06-05", "found"},
		{"2026-05-03", "| May 3 | 2026 |", "2026-05-03", "missing"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, []Figure{{Text: tc.text, Status: tc.figure}}})
	}
	cases = append(cases,
		testCase{"CAD ($40", "CAD ($40", []Figure{{Text: "$40", Status: "found"}}},
		testCase{"$40 (CAD", "USD40", []Figure{{Text: "$40", Status: "missing"}}},
		testCase{"$40 (CAD", "$40 (CAD", []Figure{{Text: "$40", Status: "found"}}},
		testCase{"$20 CAD0.5", "USD 20, CAD .5", []Figure{{Text: "$20", Status: "found"}, {Text: "CAD0.5", Status: "found"}}},
		testCase{"$20 CAD30", "USD 20, CAD 30", []Figure{{Text: "$20", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"USD 20, CAD 30", "$20 CAD30", []Figure{{Text: "USD 20", Status: "found"}, {Text: "CAD 30", Status: "found"}}},
		testCase{"$20 CAD30", "USD 20; CAD 30", []Figure{{Text: "$20", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"$40 on March 15, 2026", "40 USD\n2026-03-15", []Figure{{Text: "$40", Status: "found"}, {Text: "March 15, 2026", Status: "found"}}},
		testCase{"$40 CAD30", "40USD 30CAD", []Figure{{Text: "$40", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"CAD30", "-5 USD 30 CAD", []Figure{{Text: "CAD30", Status: "missing"}}},
		testCase{"USD40 on March 15, 2026", "$40 CAD 2026-03-15", []Figure{{Text: "USD40", Status: "missing"}, {Text: "March 15, 2026", Status: "found"}}},
		testCase{"CAD15 on 2026-03-15", "CAD 15 March 2026", []Figure{{Text: "CAD15", Status: "missing"}, {Text: "2026-03-15", Status: "found"}}},
		testCase{"CAD40 on 2026-03-15", "40 CAD 15 March 2026", []Figure{{Text: "CAD40", Status: "found"}, {Text: "2026-03-15", Status: "found"}}},
		testCase{"$20 CAD30", "USD 20\n\nCAD 30", []Figure{{Text: "$20", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"USD 20\n\nCAD 30", "$20 CAD30", []Figure{{Text: "USD 20", Status: "found"}, {Text: "CAD 30", Status: "found"}}},
		testCase{"$40 on March 15, 2026", "USD 40 2026-03-15", []Figure{{Text: "$40", Status: "found"}, {Text: "March 15, 2026", Status: "found"}}},
		testCase{"March 15, 2026 [1]", "2026-03-15", []Figure{{Text: "March 15, 2026", Status: "found"}}},
		testCase{"USD20 CAD30", "USD 20 and CAD 30", []Figure{{Text: "USD20", Status: "found"}, {Text: "CAD30", Status: "found"}}},
	)
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.answer+"/"+tc.source, "\n", `\n`), func(t *testing.T) {
			answer, err := Prepare(tc.answer)
			require.NoError(t, err)
			report := answer.Check([]Citation{{Text: tc.source}})
			require.Equal(t, tc.figures, report.Figures)
		})
	}
}
