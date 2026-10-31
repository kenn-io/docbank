package grounding

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNormalizedFigures(t *testing.T) {
	type testCase struct {
		answer, source, overall string
		figures                 []Figure
	}
	cases := []testCase{}
	for _, gap := range []string{" : ", " , : \t", " ' : ' ", " (", ") ", "*", "|", ";", "[", "]", "<", ">", "~", "`", "=", "\u2014", "\u2015"} {
		for _, ambiguous := range []string{"CAD" + gap + "$40", "$40" + gap + "CAD"} {
			cases = append(cases,
				testCase{"USD40", ambiguous, "ungrounded", []Figure{{Text: "USD40", Status: "missing"}}},
				testCase{ambiguous, "USD40", "ungrounded", []Figure{{Text: "$40", Status: "missing"}}},
				testCase{ambiguous, ambiguous, "grounded", []Figure{{Text: "$40", Status: "found"}}},
			)
		}
	}
	for _, lineBreak := range []string{"\n", "\r\n", "\r", "\u0085", "\u2028", "\u2029", "\f", "\v"} {
		for _, tc := range []struct{ answer, source, overall, status string }{
			{"$40", "40" + lineBreak + "USD", "ungrounded", "missing"},
			{"$40", "USD" + lineBreak + "40", "ungrounded", "missing"},
			{"$40", "$" + lineBreak + "40", "ungrounded", "missing"},
			{"USD40", "$40," + lineBreak + "CAD", "grounded", "found"},
			{"USD40", "CAD" + lineBreak + "$40", "grounded", "found"},
			{"USD40", "CAD :" + lineBreak + "$40", "grounded", "found"},
			{"USD40", "$40 :" + lineBreak + "CAD", "grounded", "found"},
		} {
			cases = append(cases, testCase{tc.answer, tc.source, tc.overall, []Figure{{Text: tc.answer, Status: tc.status}}})
		}
	}
	for _, tc := range []struct{ answer, source, overall, text, figure string }{
		{"USD40", "$40, CAD", "ungrounded", "USD40", "missing"},
		{"USD40", "$40 CAD : end", "ungrounded", "USD40", "missing"},
		{"$40, CAD", "USD40", "ungrounded", "$40", "missing"},
		{"2026-03-155", "2026-03-15", "ungrounded", "2026-03-155", "missing"},
		{"2026-03-15", "2026-03-155", "ungrounded", "2026-03-15", "missing"},
		{"2026-03-155", "2026-03-155", "grounded", "2026-03-155", "found"},
		{"USD40", "CAD $40", "ungrounded", "USD40", "missing"},
		{"USD40", "CAD. $40", "ungrounded", "USD40", "missing"},
		{"USD40", "'CAD $40'", "ungrounded", "USD40", "missing"},
		{"USD40", "$40 'CAD'", "ungrounded", "USD40", "missing"},
		{"USD40", "CAD$40", "ungrounded", "USD40", "missing"},
		{"USD40", "CAD USD 40", "ungrounded", "USD40", "missing"},
		{"$40", "$ USD40", "ungrounded", "$40", "missing"},
		{"USD40", "$40CAD", "ungrounded", "USD40", "missing"},
		{"CAD $40", "USD40", "ungrounded", "$40", "missing"},
		{"-USD5.00", "$5", "ungrounded", "USD5.00", "missing"},
		{"$5", "-USD5.00", "ungrounded", "$5", "missing"},
		{"+USD5.00", "$5", "ungrounded", "USD5.00", "missing"},
		{"−USD5.00", "$5", "ungrounded", "USD5.00", "missing"},
		{"±USD5.00", "$5", "ungrounded", "USD5.00", "missing"},
		{"USD-5.00", "$5", "ungrounded", "USD-5.00", "missing"},
		{"-5.00 USD", "$5", "ungrounded", "-5.00", "missing"},
		{"$5", "-USD 5.00 USD", "ungrounded", "$5", "missing"},
		{"March\u00a015,\u00a02026", "2026-03-15", "grounded", "March\u00a015,\u00a02026", "found"},
		{"2026-03-15", "15\u2009March\u00852026", "grounded", "2026-03-15", "found"},
		{"March 15, 2026", "2026-03-15", "grounded", "March 15, 2026", "found"},
		{"March 16, 2026", "2026-03-15", "ungrounded", "March 16, 2026", "missing"},
		{"$1,250", "1,250 USD", "grounded", "$1,250", "found"},
		{"$1,250", "1,251 USD", "ungrounded", "$1,250", "missing"},
		{"$0040", "USD 40", "grounded", "$0040", "found"},
		{"$5000000", "$5 m", "ungrounded", "$5000000", "missing"},
		{"$5 million", "USD 5000000", "ungrounded", "$5", "missing"},
		{"$40", "CAD 40", "ungrounded", "$40", "missing"},
		{"CAD 40", "$40", "ungrounded", "CAD 40", "missing"},
		{"2026-03-15T11:00:00Z", "2026-03-15T10:00:00Z", "ungrounded", "2026-03-15T11:00:00Z", "missing"},
		{"2026-03-15", "INV-2026-03-15", "ungrounded", "2026-03-15", "missing"},
		{"2026-02-30", "March 2, 2026", "ungrounded", "2026-02-30", "missing"},
		{"USD 40", `"$40..."`, "grounded", "USD 40", "found"},
		{`"$40..."`, "USD 40", "grounded", "$40", "found"},
		{"USD 40", `"$40..5"`, "ungrounded", "USD 40", "missing"},
		{"USD 40", "$40...", "grounded", "USD 40", "found"},
		{"$40...", "USD 40", "grounded", "$40", "found"},
		{"USD 40", "$40..5", "ungrounded", "USD 40", "missing"},
		{"USD 123", "$1,2,3...", "ungrounded", "USD 123", "missing"},
		{"USD40", "$40 CAD.", "ungrounded", "USD40", "missing"},
		{"USD40", "$40 CAD...", "ungrounded", "USD40", "missing"},
		{"_$40_", "USD 40", "grounded", "$40", "found"},
		{"2026-03-15", "_March 15, 2026_", "grounded", "2026-03-15", "found"},
		{"_2026-03-15_", "March 15, 2026", "grounded", "2026-03-15", "found"},
		{"$40", "INV_$40_", "ungrounded", "$40", "missing"},
		{"2026-03-15", "INV_2026-03-15", "ungrounded", "2026-03-15", "missing"},
		{"40 USD", "40 USD 2026-03-15", "grounded", "40 USD", "found"},
		{"CAD 40", "USD 40CAD 30", "ungrounded", "CAD 40", "missing"},
		{"CAD 40", "$40 USD 2026-03-15T10:00:00Z", "ungrounded", "CAD 40", "missing"},
		{"CAD 40", "$40 USD 12:30", "ungrounded", "CAD 40", "missing"},
		{"CAD 40", "$40 USD 50-60", "ungrounded", "CAD 40", "missing"},
		{"CAD 40", "$40 USD 123ABC", "ungrounded", "CAD 40", "missing"},
		{"USD 40", "CAD 40", "grounded", "USD 40", "found"},
		{"$40", "40", "ungrounded", "$40", "missing"},
		{"$40", "USD 400", "ungrounded", "$40", "missing"},
		{"4K", "4", "ungrounded", "4K", "missing"},
		{"2026-03-15", "March" + strings.Repeat(" ", 64) + "15, 2026", "grounded", "2026-03-15", "found"},
		{"$1,250", "USD 1250", "grounded", "$1,250", "found"},
		{"$10,000", "$10", "ungrounded", "$10,000", "missing"},
		{"$10", "$10,000", "ungrounded", "$10", "missing"},
		{"$1,250", "$1.25", "ungrounded", "$1,250", "missing"},
		{"$1.234", "$1,234", "ungrounded", "$1.234", "missing"},
		{"2026-09-05", "Sept. 5, 2026", "grounded", "2026-09-05", "found"},
		{"2026-06-05", "June 5,\n2026", "grounded", "2026-06-05", "found"},
		{"2026-06-05", "June\n5, 2026", "grounded", "2026-06-05", "found"},
		{"2026-05-03", "| May 3 | 2026 |", "ungrounded", "2026-05-03", "missing"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, tc.overall, []Figure{{Text: tc.text, Status: tc.figure}}})
	}
	cases = append(cases,
		testCase{"$20 CAD0.5", "USD 20, CAD .5", "grounded", []Figure{{Text: "$20", Status: "found"}, {Text: "CAD0.5", Status: "found"}}},
		testCase{"$20 CAD30", "USD 20, CAD 30", "grounded", []Figure{{Text: "$20", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"USD 20, CAD 30", "$20 CAD30", "grounded", []Figure{{Text: "USD 20", Status: "found"}, {Text: "CAD 30", Status: "found"}}},
		testCase{"$20 CAD30", "USD 20; CAD 30", "grounded", []Figure{{Text: "$20", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"$40 on March 15, 2026", "40 USD\n2026-03-15", "", []Figure{{Text: "$40", Status: "found"}, {Text: "March 15, 2026", Status: "found"}}},
		testCase{"$40 CAD30", "40USD 30CAD", "", []Figure{{Text: "$40", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"$40 CAD30", "40 USD 30 CAD", "", []Figure{{Text: "$40", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"CAD30", "-5 USD 30 CAD", "", []Figure{{Text: "CAD30", Status: "found"}}},
		testCase{"USD40 on March 15, 2026", "$40 CAD 2026-03-15", "", []Figure{{Text: "USD40", Status: "missing"}, {Text: "March 15, 2026", Status: "found"}}},
		testCase{"$20 CAD30", "USD 20\n\nCAD 30", "grounded", []Figure{{Text: "$20", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		testCase{"USD 20\n\nCAD 30", "$20 CAD30", "grounded", []Figure{{Text: "USD 20", Status: "found"}, {Text: "CAD 30", Status: "found"}}},
		testCase{"$40 on March 15, 2026", "USD 40 2026-03-15", "grounded", []Figure{{Text: "$40", Status: "found"}, {Text: "March 15, 2026", Status: "found"}}},
		testCase{"March 15, 2026 [1]", "2026-03-15", "", []Figure{{Text: "March 15, 2026", Status: "found"}}},
		testCase{"5.00", "5", "", []Figure{{Text: "5.00", Status: "missing"}}},
		testCase{"USD20 CAD30", "USD 20 and CAD 30", "grounded", []Figure{{Text: "USD20", Status: "found"}, {Text: "CAD30", Status: "found"}}},
	)
	for _, tc := range cases {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			answer, err := Prepare(tc.answer)
			require.NoError(t, err)
			report := answer.Check([]Citation{{Text: tc.source}})
			if tc.overall != "" {
				require.Equal(t, tc.overall, report.Status)
			}
			require.Equal(t, tc.figures, report.Figures)
		})
	}
}

func TestNormalizedPunctuationGap(t *testing.T) {
	answer, err := Prepare("USD40")
	require.NoError(t, err)
	citations := make([]Citation, 8)
	for i := range citations {
		citations[i].Text = "CAD " + strings.Repeat(": ", 7900) + "$40"
	}
	start := time.Now()
	require.Equal(t, "ungrounded", answer.Check(citations).Status)
	require.Less(t, time.Since(start), 2*time.Second)
}
