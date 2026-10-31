package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizedFigures(t *testing.T) {
	type testCase struct {
		answer, source, overall string
		figures                 []Figure
	}
	cases := []testCase{}
	for _, lineBreak := range []string{"\n", "\r\n", "\r", "\u0085", "\u2028", "\u2029"} {
		for _, tc := range []struct{ answer, source, overall string }{
			{"$40", "40" + lineBreak + "USD", "ungrounded"},
			{"$40", "USD" + lineBreak + "40", "ungrounded"},
			{"$40", "$" + lineBreak + "40", "ungrounded"},
			{"USD40", "$40," + lineBreak + "CAD", "grounded"},
			{"USD40", "CAD" + lineBreak + "$40", "grounded"},
		} {
			cases = append(cases, testCase{tc.answer, tc.source, tc.overall, []Figure{{}}})
		}
	}
	for _, tc := range []struct{ answer, source, overall, figure string }{
		{"USD40", "$40, CAD", "ungrounded", "missing"},
		{"$40, CAD", "USD40", "ungrounded", "missing"},
		{"2026-03-155", "2026-03-15", "ungrounded", "missing"},
		{"2026-03-15", "2026-03-155", "ungrounded", "missing"},
		{"2026-03-155", "2026-03-155", "grounded", "found"},
		{"USD40", "CAD $40", "ungrounded", "missing"},
		{"USD40", "CAD. $40", "ungrounded", "missing"},
		{"USD40", "'CAD $40'", "ungrounded", "missing"},
		{"USD40", "$40 'CAD'", "ungrounded", "missing"},
		{"USD40", "CAD$40", "ungrounded", "missing"},
		{"USD40", "CAD USD 40", "ungrounded", "missing"},
		{"$40", "$ USD40", "ungrounded", "missing"},
		{"USD40", "$40CAD", "ungrounded", "missing"},
		{"CAD $40", "USD40", "ungrounded", "missing"},
		{"-USD5.00", "$5", "ungrounded", "missing"},
		{"$5", "-USD5.00", "ungrounded", "missing"},
		{"+USD5.00", "$5", "ungrounded", "missing"},
		{"−USD5.00", "$5", "ungrounded", "missing"},
		{"±USD5.00", "$5", "ungrounded", "missing"},
		{"USD-5.00", "$5", "ungrounded", "missing"},
		{"-5.00 USD", "$5", "ungrounded", "missing"},
		{"$5", "-USD 5.00 USD", "ungrounded", "missing"},
		{"March\u00a015,\u00a02026", "2026-03-15", "grounded", "found"},
		{"2026-03-15", "15\u2009March\u00852026", "grounded", "found"},
		{"March 15, 2026", "2026-03-15", "grounded", "found"},
		{"March 16, 2026", "2026-03-15", "ungrounded", "missing"},
		{"$1,250", "1,250 USD", "grounded", "found"},
		{"$1,250", "1,251 USD", "ungrounded", "missing"},
		{"$0040", "USD 40", "grounded", "found"},
		{"$5000000", "$5 m", "ungrounded", "missing"},
		{"$5 million", "USD 5000000", "ungrounded", "missing"},
		{"$40", "CAD 40", "ungrounded", "missing"},
		{"CAD 40", "$40", "ungrounded", "missing"},
		{"2026-03-15T11:00:00Z", "2026-03-15T10:00:00Z", "ungrounded", "missing"},
		{"2026-03-15", "INV-2026-03-15", "ungrounded", "missing"},
		{"2026-02-30", "March 2, 2026", "ungrounded", "missing"},
		{"USD 40", `"$40..."`, "grounded", "found"},
		{`"$40..."`, "USD 40", "grounded", "found"},
		{"USD 40", `"$40..5"`, "ungrounded", "missing"},
		{"USD 40", "$40...", "grounded", "found"},
		{"$40...", "USD 40", "grounded", "found"},
		{"USD 40", "$40..5", "ungrounded", "missing"},
		{"USD 123", "$1,2,3...", "ungrounded", "missing"},
		{"USD40", "$40 CAD.", "ungrounded", "missing"},
		{"USD40", "$40 CAD...", "ungrounded", "missing"},
		{"_$40_", "USD 40", "grounded", "found"},
		{"2026-03-15", "_March 15, 2026_", "grounded", "found"},
		{"_2026-03-15_", "March 15, 2026", "grounded", "found"},
		{"$40", "INV_$40_", "ungrounded", "missing"},
		{"2026-03-15", "INV_2026-03-15", "ungrounded", "missing"},
		{"40 USD", "40 USD 2026-03-15", "grounded", "found"},
		{"CAD 40", "USD 40CAD 30", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 2026-03-15T10:00:00Z", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 12:30", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 50-60", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 123ABC", "ungrounded", "missing"},
		{"USD 40", "CAD 40", "grounded", "found"},
		{"$40", "40", "ungrounded", "missing"},
		{"$40", "USD 400", "ungrounded", "missing"},
		{"4K", "4", "ungrounded", "missing"},
		{"2026-03-15", "March" + strings.Repeat(" ", 64) + "15, 2026", "grounded", "found"},
		{"$1,250", "USD 1250", "grounded", "found"},
		{"$10,000", "$10", "ungrounded", "missing"},
		{"$10", "$10,000", "ungrounded", "missing"},
		{"$1,250", "$1.25", "ungrounded", "missing"},
		{"$1.234", "$1,234", "ungrounded", "missing"},
		{"2026-09-05", "Sept. 5, 2026", "grounded", "found"},
		{"2026-06-05", "June 5,\n2026", "grounded", "found"},
		{"2026-06-05", "June\n5, 2026", "grounded", "found"},
		{"2026-05-03", "| May 3 | 2026 |", "ungrounded", "missing"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, tc.overall, []Figure{{Status: tc.figure}}})
	}
	cases = append(cases,
		testCase{"$40 on March 15, 2026", "40 USD\n2026-03-15", "", []Figure{{Text: "$40", Status: "found"}, {Text: "March 15, 2026", Status: "found"}}},
		testCase{"$40 CAD30", "40USD 30CAD", "", []Figure{{Text: "$40", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"$40 CAD30", "40 USD 30 CAD", "", []Figure{{Text: "$40", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"CAD30", "-5 USD 30 CAD", "", []Figure{{Text: "CAD30", Status: "found"}}},
		testCase{"USD40 on March 15, 2026", "$40 CAD 2026-03-15", "", []Figure{{Text: "USD40", Status: "missing"}, {Text: "March 15, 2026", Status: "found"}}},
		testCase{"$20 CAD30", "USD 20\n\nCAD 30", "grounded", []Figure{{Status: "found"}, {Status: "found"}}},
		testCase{"USD 20\n\nCAD 30", "$20 CAD30", "grounded", []Figure{{Status: "found"}, {Status: "found"}}},
		testCase{"$40 on March 15, 2026", "USD 40 2026-03-15", "grounded", []Figure{{Status: "found"}, {Status: "found"}}},
		testCase{"March 15, 2026 [1]", "2026-03-15", "", []Figure{{Text: "March 15, 2026", Status: "found"}}},
		testCase{"5.00", "5", "", []Figure{{Status: "missing"}}},
		testCase{"USD20 CAD30", "USD 20 and CAD 30", "grounded", []Figure{{Status: "found"}, {Status: "found"}}},
	)
	for _, tc := range cases {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			answer, err := Prepare(tc.answer)
			require.NoError(t, err)
			report := answer.Check([]Citation{{Text: tc.source}})
			if tc.overall != "" {
				require.Equal(t, tc.overall, report.Status)
			}
			require.Len(t, report.Figures, len(tc.figures))
			for i, figure := range tc.figures {
				if figure.Status != "" {
					require.Equal(t, figure.Status, report.Figures[i].Status)
				}
				if figure.Text != "" {
					require.Equal(t, figure.Text, report.Figures[i].Text)
				}
			}
		})
	}
}
