package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizedAmountSuffixNeighbors(t *testing.T) {
	for _, tc := range []struct {
		name, answer, source string
		figures              []Figure
	}{
		{"suffix before date", "$40 on March 15, 2026", "40 USD\n2026-03-15", []Figure{{Text: "$40", Status: "found"}, {Text: "March 15, 2026", Status: "found"}}},
		{"adjacent attached suffixes", "$40 CAD30", "40USD 30CAD", []Figure{{Text: "$40", Status: "found"}, {Text: "CAD30", Status: "found"}}},
		{"conflicting marker before date", "USD40 on March 15, 2026", "$40 CAD 2026-03-15", []Figure{{Text: "USD40", Status: "missing"}, {Text: "March 15, 2026", Status: "found"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, err := Prepare(tc.answer)
			require.NoError(t, err)
			require.Equal(t, tc.figures, answer.Check([]Citation{{Text: tc.source}}).Figures)
		})
	}
}

func TestNormalizedCitationBullets(t *testing.T) {
	for _, tc := range []struct{ answer, source, status string }{
		{"40 [1]", "- 40", "grounded"},
		{"$40 [1]", "- $40", "grounded"},
		{"$40", "Items:\n- $40 apples\n- $50 pears", "grounded"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			answer, err := Prepare(tc.answer)
			require.NoError(t, err)
			require.Equal(t, tc.status, answer.Check([]Citation{{Text: tc.source}}).Status)
		})
	}
}

func TestNormalizedBoundedCheck(t *testing.T) {
	for _, tc := range []struct{ answer, source, status string }{
		{"$1 ", "$1 ", "grounded"},
		{"$1 ", "1 ", "ungrounded"},
		{"2026-03-15 ", "March 15, 2026 ", "grounded"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			answer, err := Prepare(strings.Repeat(tc.answer, 512))
			require.NoError(t, err)
			citations := make([]Citation, 8)
			for i := range citations {
				citations[i].Text = strings.Repeat(tc.source, 64000/len(tc.source))
			}
			report := answer.Check(citations)
			require.Len(t, report.Figures, 512)
			require.Len(t, report.Citations, 8)
			require.Equal(t, tc.status, report.Status)
		})
	}
}

func TestNormalizedFigures(t *testing.T) {
	type testCase struct {
		answer, source, overall, status, text string
		second                                bool
	}
	cases := []testCase{}
	for _, tc := range []struct{ answer, source, overall, figure string }{
		{"March 15, 2026", "2026-03-15", "grounded", "found"},
		{"March 16, 2026", "2026-03-15", "ungrounded", "missing"},
		{"$1,250", "1,250 USD", "grounded", "found"},
		{"$1,250", "1,251 USD", "ungrounded", "missing"},
		{"$0040", "USD 40", "grounded", "found"},
		{"$5", "$5 m", "grounded", "found"},
		{"$5000000", "$5 m", "ungrounded", "missing"},
		{"$5 million", "USD 5000000", "ungrounded", "missing"},
		{"$1.2M", "$1.2M", "grounded", "found"},
		{"$40", "CAD 40", "ungrounded", "missing"},
		{"CAD 40", "$40", "ungrounded", "missing"},
		{"2026-03-15T10:00:00+24:00", "2026-03-15T10:00:00+24:00", "grounded", "found"},
		{"2026-03-15T11:00:00Z", "2026-03-15T10:00:00Z", "ungrounded", "missing"},
		{"2026-03-15", "INV-2026-03-15", "ungrounded", "missing"},
		{"2026-02-30", "2026-02-30", "grounded", "found"},
		{"$1.2M", "USD 1,200,000", "ungrounded", "missing"},
		{"$40", "$40...", "grounded", "found"},
		{"USD 40", `"$40..."`, "grounded", "found"},
		{`"$40..."`, "USD 40", "grounded", "found"},
		{"USD 40", `"$40..5"`, "ungrounded", "missing"},
		{"USD 40 USD/GBP", "EUR 40", "grounded", "found"},
		{"USD 40 USD_X", "EUR 40", "grounded", "found"},
		{"USD 40", "$40...", "grounded", "found"},
		{"$40...", "USD 40", "grounded", "found"},
		{"USD 40", "$40..5", "ungrounded", "missing"},
		{"USD 123", "$1,2,3...", "ungrounded", "missing"},
		{"$40", "$40 EUR...", "grounded", "found"},
		{"$40", "$40 EUR", "grounded", "found"},
		{"USD40", "$40 CAD.", "ungrounded", "missing"},
		{"USD40", "$40 CAD...", "ungrounded", "missing"},
		{"$40", "_$40_", "grounded", "found"},
		{"_$40_", "USD 40", "grounded", "found"},
		{"2026-03-15", "_March 15, 2026_", "grounded", "found"},
		{"_2026-03-15_", "March 15, 2026", "grounded", "found"},
		{"$40", "INV_$40_", "ungrounded", "missing"},
		{"2026-03-15", "INV_2026-03-15", "ungrounded", "missing"},
		{"40 USD", "40 USD 2026-03-15", "grounded", "found"},
		{"CAD 40", "USD 40CAD 30", "ungrounded", "missing"},
		{"USD 40 CAD%", "EUR 40", "grounded", "found"},
		{"USD 40 CAD:report", "EUR 40", "grounded", "found"},
		{"USD 40 CAD$%", "EUR 40", "grounded", "found"},
		{"CAD 40", "$40 USD 2026-03-15T10:00:00Z", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 12:30", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 50-60", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 123ABC", "ungrounded", "missing"},
		{"USD 40", "CAD 40", "grounded", "found"},
		{"$40", "40", "ungrounded", "missing"},
		{"USD 1.2 million EUR", "USD 1200000", "ungrounded", "missing"},
		{"USD 20", "USD\n20", "grounded", "found"},
		{"$40", "USD 400", "ungrounded", "missing"},
		{"USD 20", "$ : 20", "grounded", "found"},
		{"USD 20", "$[20]", "grounded", "found"},
		{"4K", "4", "ungrounded", "missing"},
		{"40", "USD 40", "grounded", "found"},
		{"$500", "Rent $500\n12 months", "grounded", "found"},
		{"ALL 5", "5 invoices", "grounded", "found"},
		{"TOP 10", "10 customers", "grounded", "found"},
		{"$40", "Price: $40\n\n205 units", "grounded", "found"},
		{"March 15, 2026", "March" + strings.Repeat(" ", 64) + "15, 2026", "grounded", "found"},
		{"2026-03-15", "March" + strings.Repeat(" ", 64) + "15, 2026", "grounded", "found"},
		{"$1,250", "USD 1250", "grounded", "found"},
		{"€30", "USD 40 €30", "grounded", "found"},
		{"$10,000", "$10", "ungrounded", "missing"},
		{"$10", "$10,000", "ungrounded", "missing"},
		{"$1,250", "$1.25", "ungrounded", "missing"},
		{"$1.234", "$1,234", "ungrounded", "missing"},
		{"September 5, 2026", "Sept. 5, 2026", "grounded", "found"},
		{"June 5, 2026", "June 5,\n2026", "grounded", "found"},
		{"June 5, 2026", "June\n5, 2026", "grounded", "found"},
		{"May 3, 2026", "| May 3 | 2026 |", "grounded", "found"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, tc.overall, tc.figure, "", false})
	}
	cases = append(cases,
		testCase{"Paid $5 3 times", "$5 [3]", "grounded", "found", "", true},
		testCase{"$40 on March 15, 2026", "USD 40 2026-03-15", "grounded", "found", "", true},
		testCase{"March 15, 2026 [1]", "2026-03-15", "", "found", "March 15, 2026", false},
	)
	for _, tc := range []struct{ answer, source, status string }{
		{"USD 40", "USD 40 EUR", "found"},
		{"$1200", "1,200", "missing"}, {"$40", "40.00", "missing"},
		{"5.00", "5", "missing"}, {"$40", "INV-40", "missing"}, {"$40", "40%", "missing"}, {"$40", "40,0,0", "missing"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, "", tc.status, "", false})
	}
	cases = append(cases, testCase{"USD20 CAD30", "USD 20 and CAD 30", "grounded", "found", "", true})
	for _, tc := range cases {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			answer, err := Prepare(tc.answer)
			require.NoError(t, err)
			report := answer.Check([]Citation{{Text: tc.source}})
			if tc.overall != "" {
				require.Equal(t, tc.overall, report.Status)
			}
			count := 1
			if tc.second {
				count = 2
			}
			require.Len(t, report.Figures, count)
			if tc.status != "" {
				for _, figure := range report.Figures {
					require.Equal(t, tc.status, figure.Status)
				}
			}
			if tc.text != "" {
				require.Equal(t, tc.text, report.Figures[0].Text)
			}
		})
	}
}
