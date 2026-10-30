package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizedFigures(t *testing.T) {
	type testCase struct {
		answer, source, overall, status, text string
		second                                bool
	}
	cases := []testCase{}
	for _, tc := range []struct{ answer, source, overall, figure string }{
		{"March 15, 2026", "2026-03-15", "grounded", "found"},
		{"15 March 2026", "2026/03/15", "grounded", "found"},
		{"2026-03-15", "2026-03-15T10:00:00Z", "grounded", "found"},
		{"2026-03-15", "2026-03-15T10:00:00+24:00", "ungrounded", "missing"},
		{"2026-03-15T10:00:00+24:00", "2026-03-15T10:00:00+24:00", "grounded", "found"},
		{"2026-03-15", "2026-03-15T10:00:00+05:60", "ungrounded", "missing"},
		{"2026-03-15", "2026-03-15T10:00:00,5Z", "ungrounded", "missing"},
		{"2026-03-15", "2026-03-15T1:00:00Z", "ungrounded", "missing"},
		{"2026-03-15", "2026-03-15T10:00:00.5Z", "grounded", "found"},
		{"2026-03-15", "2026-03-15T10:00:00+23:59", "grounded", "found"},
		{"2026-03-15", "2026-03-15T10:00:60Z", "ungrounded", "missing"},
		{"2026-03-15T11:00:00Z", "2026-03-15T10:00:00Z", "ungrounded", "missing"},
		{"March 2026", "2026-03-15", "ungrounded", "missing"},
		{"2026-03-15", "INV-2026-03-15", "ungrounded", "missing"},
		{"2026-02-30", "2026-02-30", "grounded", "found"},
		{"$1.2M", "USD 1,200,000", "grounded", "found"},
		{"$40", "$40...", "grounded", "found"},
		{"USD 40", `"$40..."`, "grounded", "found"},
		{`"$40..."`, "USD 40", "grounded", "found"},
		{"USD 40", `"$40..5"`, "ungrounded", "missing"},
		{"$40", "±USD40", "ungrounded", "missing"},
		{"±USD40", "$40", "ungrounded", "missing"},
		{"USD40", "±USD40", "ungrounded", "missing"},
		{"±USD40", "USD40", "ungrounded", "missing"},
		{"±$40", "±USD40", "grounded", "found"},
		{"USD ±40", "±$40", "grounded", "found"},
		{"±$40", "±40", "ungrounded", "value_only"},
		{"USD 40 USD/GBP", "EUR 40", "ungrounded", "value_only"},
		{"USD 40 USD_X", "EUR 40", "ungrounded", "value_only"},
		{"USD 40", "$40...", "grounded", "found"},
		{"$40...", "USD 40", "grounded", "found"},
		{"USD 40", "$40..5", "ungrounded", "missing"},
		{"USD 123", "$1,2,3...", "ungrounded", "missing"},
		{"USD 40", "INV-$40...", "ungrounded", "missing"},
		{"$40", "$40 EUR...", "ungrounded", "value_only"},
		{"$40", "$40 EUR", "ungrounded", "value_only"},
		{"$40", "_$40_", "grounded", "found"},
		{"_$40_", "USD 40", "grounded", "found"},
		{"2026-03-15", "_March 15, 2026_", "grounded", "found"},
		{"_2026-03-15_", "March 15, 2026", "grounded", "found"},
		{"$40", "INV_$40_", "ungrounded", "missing"},
		{"2026-03-15", "INV_2026-03-15", "ungrounded", "missing"},
		{"40 USD", "40 USD 2026-03-15", "grounded", "found"},
		{"CAD 40", "USD 40CAD 30", "ungrounded", "value_only"},
		{"USD 40 CAD%", "EUR 40", "ungrounded", "value_only"},
		{"USD 40 CAD:report", "EUR 40", "ungrounded", "value_only"},
		{"USD 40 CAD$%", "EUR 40", "ungrounded", "value_only"},
		{"USD 40", "$40 USD$", "grounded", "found"},
		{"USD 40", "$40USD 50", "grounded", "found"},
		{"CAD 40", "$40 USD 2026-03-15", "ungrounded", "value_only"},
		{"USD 40", "$40 USD 2026-03-15", "grounded", "found"},
		{"CAD 40", "$40 USD 2026-03-15T10:00:00Z", "ungrounded", "value_only"},
		{"CAD 40", "$40 USD 12:30", "ungrounded", "value_only"},
		{"CAD 40", "$40 USD -12:30", "ungrounded", "value_only"},
		{"CAD 40", "$40 USD 50-60", "ungrounded", "value_only"},
		{"CAD 40", "$40 USD 123ABC", "ungrounded", "value_only"},
		{"CAD 40", "$40 USD 50", "grounded", "found"},
		{"C$40", "CAD 40", "grounded", "found"},
		{"USD 40", "CAD 40", "ungrounded", "value_only"},
		{"$40", "40", "ungrounded", "value_only"},
		{"+$40", "USD 40", "ungrounded", "missing"},
		{"-$0", "USD 0", "ungrounded", "missing"},
		{"USD 1.2 million EUR", "USD 1200000", "ungrounded", "value_only"},
		{"USD 20", "USD\n20", "ungrounded", "value_only"},
		{"-$40", "USD 40", "ungrounded", "missing"},
		{"$40", "USD 400", "ungrounded", "missing"},
		{"USD 40", "INV-USD40", "ungrounded", "missing"},
		{"USD 20", "$ : 20", "ungrounded", "value_only"},
		{"USD 20", "$[20]", "ungrounded", "value_only"},
		{"4K", "4", "ungrounded", "missing"},
		{"40", "USD 40", "grounded", "found"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, tc.overall, tc.figure, "", false})
	}
	cases = append(cases, testCase{"March 15, 2026 [1]", "2026-03-15", "", "found", "March 15, 2026", false})
	for _, space := range []string{"\u00a0", "\u202f"} {
		grouped := "$1" + space + "205"
		cases = append(cases, testCase{grouped, "USD 1205", "grounded", "", grouped, false}, testCase{"USD 1205", grouped, "grounded", "", "USD 1205", false})
	}
	for _, tc := range []struct{ answer, source, status string }{
		{"USD 40", "USD 40 EUR", "value_only"},
		{"-USD40", "USD40", "missing"}, {"+USD40", "USD40", "missing"},
		{"- USD40", "USD -40", "found"}, {"-USD40", "USD -40", "found"}, {"+USD40", "USD +40", "found"},
		{"$1200", "1\u00a0200", "value_only"}, {"$200", "1\u00a0\u00a0200", "missing"}, {"$1200", "1,200", "value_only"}, {"$40", "40.00", "value_only"},
		{"−$40", "−USD 40", "found"}, {"−$40", "USD 40", "missing"},
		{"5.00", "5", "missing"}, {"$40", "INV-40", "missing"}, {"$40", "40%", "missing"}, {"$40", "40,0,0", "missing"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, "", tc.status, "", false})
	}
	for _, tc := range []struct{ answer, source string }{
		{"USD20 CAD30", "USD 20 and CAD 30"}, {"USD 20 CAD 30", "USD 20 and CAD 30"}, {"USD 20 and CAD 30", "USD 20 CAD 30"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, "grounded", "found", "", true})
	}
	for _, sign := range []string{"-", "+", "−"} {
		for _, tc := range []struct{ answer, source, text, status, overall string }{
			{`"` + sign + `USD40"`, "USD 40", sign + "USD40", "missing", "ungrounded"},
			{"USD 40", `"` + sign + `USD40"`, "USD 40", "missing", "ungrounded"},
			{`"` + sign + `USD40"`, "USD " + sign + "40", sign + "USD40", "found", "grounded"},
			{"USD " + sign + "40", `"` + sign + `USD40"`, "USD " + sign + "40", "found", "grounded"},
			{sign + " 40 USD", "USD 40", sign + " 40 USD", "missing", "ungrounded"},
			{"USD 40", "Cost: " + sign + " 40 USD", "USD 40", "missing", "ungrounded"},
			{sign + " 40 USD", "USD " + sign + "40", sign + " 40 USD", "found", "grounded"},
			{"USD " + sign + "40", "Cost: " + sign + " 40 USD", "USD " + sign + "40", "found", "grounded"},
			{"(" + sign + " .5 USD)", "USD " + sign + "0.5", sign + " .5 USD", "found", "grounded"},
		} {
			cases = append(cases, testCase{tc.answer, tc.source, tc.overall, tc.status, tc.text, false})
		}
	}
	for _, tc := range []struct{ answer, figure string }{{`"-INV5"`, "INV5"}, {"- 40", "40"}} {
		cases = append(cases, testCase{tc.answer, tc.figure, "", "found", tc.figure, false})
	}
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

func TestNormalizedPrecedence(t *testing.T) {
	answer, err := Prepare("$40 [1]")
	require.NoError(t, err)
	report := answer.Check([]Citation{{Text: "40"}, {Err: "citation_unavailable"}})
	require.Equal(t, "incomplete", report.Status)
	require.Equal(t, "value_only", report.Figures[0].Status)
	report = answer.Check([]Citation{{Text: "40"}, {Text: "USD 40"}})
	require.Equal(t, "grounded", report.Status)
	require.Equal(t, "found", report.Figures[0].Status)
}

func TestLongMalformedCurrencyRuns(t *testing.T) {
	answer, err := Prepare(strings.Repeat("$ ", 7998) + "40%")
	require.NoError(t, err)
	require.Equal(t, []Figure{{Text: "40%", Status: "found"}}, answer.Check([]Citation{{Text: "40%"}}).Figures)
	answer, err = Prepare("USD 40 " + strings.Repeat("$", 15900) + "%")
	require.NoError(t, err)
	require.Equal(t, []Figure{{Text: "USD 40", Status: "found"}}, answer.Check([]Citation{{Text: "USD 40"}}).Figures)
	for _, text := range []string{strings.Repeat("USD ", 4000), strings.Repeat("$", 16000)} {
		a, err := Prepare(text)
		require.NoError(t, err)
		require.Equal(t, "no_figures", a.Check([]Citation{{Text: text}}).Status)
	}
}
