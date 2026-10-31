package grounding

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestNormalizedSavedListQuote(t *testing.T) {
	evidencePolicy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceComplete,
		Family:          "text",
		UnitKind:        document.EvidenceUnitSection,
		Units: []document.SourceEvidenceUnitV1{{
			Order:   0,
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorSection, IndexOrigin: document.EvidenceIndexOriginNone},
			Text:    "<p>" + strings.Repeat("x", 16_001) + "</p><ul><li>$40</li><li>40</li></ul>",
		}},
	}, evidencePolicy)
	require.NoError(t, err)
	policy, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 100_000, MaxUnitRunes: 100_000, MaxSegmentRunes: 4_000})
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	start := strings.Index(string(rendition.Markdown), "- ")
	require.Greater(t, start, 16_000)
	quote := string(rendition.Markdown)[start:]
	for _, tc := range []struct{ answer, status string }{
		{"$40 [1]", "grounded"}, {"40 [1]", "grounded"}, {"-$40 [1]", "ungrounded"},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			answer, err := Prepare(tc.answer)
			require.NoError(t, err)
			require.Equal(t, tc.status, answer.Check([]Citation{{Text: quote}}).Status)
		})
	}
}

func TestNormalizedCitationBullets(t *testing.T) {
	for _, tc := range []struct{ answer, source, status string }{
		{"40 [1]", "- 40", "grounded"},
		{"$40 [1]", "- $40", "grounded"},
		{"-$40 [1]", "- $40", "ungrounded"},
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
			start := time.Now()
			report := answer.Check(citations)
			require.Less(t, time.Since(start), time.Second)
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
		{"-$000", "USD -0", "grounded", "found"},
		{"$5", "$5 m", "grounded", "found"},
		{"$5000000", "$5 m", "ungrounded", "missing"},
		{"$5 million", "USD 5000000", "ungrounded", "missing"},
		{"$1.2M", "$1.2M", "grounded", "found"},
		{"$40", "CAD 40", "ungrounded", "missing"},
		{"CAD 40", "$40", "ungrounded", "missing"},
		{"15 March 2026", "2026/03/15", "grounded", "found"},
		{"2026-03-15T10:00:00+24:00", "2026-03-15T10:00:00+24:00", "grounded", "found"},
		{"2026-03-15T11:00:00Z", "2026-03-15T10:00:00Z", "ungrounded", "missing"},
		{"March 2026", "2026-03-15", "ungrounded", "missing"},
		{"2026-03-15", "INV-2026-03-15", "ungrounded", "missing"},
		{"2026-02-30", "2026-02-30", "grounded", "found"},
		{"$1.2M", "USD 1,200,000", "ungrounded", "missing"},
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
		{"±$40", "±40", "ungrounded", "missing"},
		{"USD 40 USD/GBP", "EUR 40", "ungrounded", "missing"},
		{"USD 40 USD_X", "EUR 40", "ungrounded", "missing"},
		{"USD 40", "$40...", "grounded", "found"},
		{"$40...", "USD 40", "grounded", "found"},
		{"USD 40", "$40..5", "ungrounded", "missing"},
		{"USD 123", "$1,2,3...", "ungrounded", "missing"},
		{"USD 40", "INV-$40...", "ungrounded", "missing"},
		{"$40", "$40 EUR...", "ungrounded", "missing"},
		{"$40", "$40 EUR", "ungrounded", "missing"},
		{"$40", "_$40_", "grounded", "found"},
		{"_$40_", "USD 40", "grounded", "found"},
		{"2026-03-15", "_March 15, 2026_", "grounded", "found"},
		{"_2026-03-15_", "March 15, 2026", "grounded", "found"},
		{"$40", "INV_$40_", "ungrounded", "missing"},
		{"2026-03-15", "INV_2026-03-15", "ungrounded", "missing"},
		{"40 USD", "40 USD 2026-03-15", "grounded", "found"},
		{"CAD 40", "USD 40CAD 30", "ungrounded", "missing"},
		{"USD 40 CAD%", "EUR 40", "ungrounded", "missing"},
		{"USD 40 CAD:report", "EUR 40", "ungrounded", "missing"},
		{"USD 40 CAD$%", "EUR 40", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 2026-03-15T10:00:00Z", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 12:30", "ungrounded", "missing"},
		{"CAD 40", "$40 USD -12:30", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 50-60", "ungrounded", "missing"},
		{"CAD 40", "$40 USD 123ABC", "ungrounded", "missing"},
		{"C$40", "CAD 40", "grounded", "found"},
		{"USD 40", "CAD 40", "ungrounded", "missing"},
		{"$40", "40", "ungrounded", "missing"},
		{"+$40", "USD 40", "ungrounded", "missing"},
		{"-$0", "USD 0", "ungrounded", "missing"},
		{"USD 1.2 million EUR", "USD 1200000", "ungrounded", "missing"},
		{"USD 20", "USD\n20", "grounded", "found"},
		{"-$40", "USD 40", "ungrounded", "missing"},
		{"$40", "USD 400", "ungrounded", "missing"},
		{"USD 40", "INV-USD40", "ungrounded", "missing"},
		{"USD 20", "$ : 20", "ungrounded", "missing"},
		{"USD 20", "$[20]", "ungrounded", "missing"},
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
		{"USD 40", "USD 40 EUR", "missing"},
		{"-USD40", "USD40", "missing"}, {"+USD40", "USD40", "missing"},
		{"- USD40", "USD -40", "missing"}, {"-USD40", "USD -40", "found"}, {"+USD40", "USD +40", "found"},
		{"$1200", "1\u00a0200", "missing"}, {"$200", "1\u00a0\u00a0200", "missing"}, {"$1200", "1,200", "missing"}, {"$40", "40.00", "missing"},
		{"−$40", "−USD 40", "found"}, {"−$40", "USD 40", "missing"},
		{"5.00", "5", "missing"}, {"$40", "INV-40", "missing"}, {"$40", "40%", "missing"}, {"$40", "40,0,0", "missing"},
	} {
		cases = append(cases, testCase{tc.answer, tc.source, "", tc.status, "", false})
	}
	cases = append(cases, testCase{"USD20 CAD30", "USD 20 and CAD 30", "grounded", "found", "", true})
	for _, sign := range []string{"-", "+", "−"} {
		for _, tc := range []struct{ answer, source, text, status, overall string }{
			{`"` + sign + `USD40"`, "USD 40", sign + "USD40", "missing", "ungrounded"},
			{"USD 40", `"` + sign + `USD40"`, "USD 40", "missing", "ungrounded"},
			{`"` + sign + `USD40"`, "USD " + sign + "40", sign + "USD40", "found", "grounded"},
			{"USD " + sign + "40", `"` + sign + `USD40"`, "USD " + sign + "40", "found", "grounded"},
			{sign + " 40 USD", "USD 40", "40 USD", "found", "grounded"},
			{"USD 40", "Cost: " + sign + " 40 USD", "USD 40", "found", "grounded"},
			{sign + " 40 USD", "USD " + sign + "40", "40 USD", "missing", "ungrounded"},
			{"USD " + sign + "40", "Cost: " + sign + " 40 USD", "USD " + sign + "40", "missing", "ungrounded"},
			{"(" + sign + " .5 USD)", "USD 0.5", ".5 USD", "found", "grounded"},
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
	require.Equal(t, "missing", report.Figures[0].Status)
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

func TestNormalizationReviewRegressions(t *testing.T) {
	for _, tc := range []struct{ answer, source, status string }{
		{"$10,000", "$10", "ungrounded"},
		{"$10", "$10,000", "ungrounded"},
		{"$1,250", "$1.25", "ungrounded"},
		{"$1.234", "$1,234", "ungrounded"},
		{"May 3, 2026", "05.03.2026", "ungrounded"},
		{"May 3, 2026", "05/03/2026", "ungrounded"},
		{"March 5, 2026", "05/03/2026", "ungrounded"},
		{"September 5, 2026", "Sept. 5, 2026", "grounded"},
		{"June 5, 2026", "June 5,\n2026", "grounded"},
		{"June 5, 2026", "June\n5, 2026", "grounded"},
		{"1,205 USD", "USD\n1,205", "grounded"},
		{"USD 1", "$1 205", "ungrounded"},
		{"$1", "$1 205", "ungrounded"},
		{"$1 205", "$1", "ungrounded"},
		{"$1 205", "$1 205", "grounded"},
		{"USD 1205", "$1 205", "ungrounded"},
		{"05.03.2026", "05.03.2026", "grounded"},
		{"13/03/2026", "March 13, 2026", "grounded"},
		{"May 3, 2026", "| May 3 | 2026 |", "ungrounded"},
		{"March 5, 2026", "3/5/2026", "ungrounded"},
		{"$1 205", "USD 1", "ungrounded"},
		{"CAD+USD 40", "CAD USD 40", "ungrounded"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			answer, err := Prepare(tc.answer)
			require.NoError(t, err)
			require.Equal(t, tc.status, answer.Check([]Citation{{Text: tc.source}}).Status)
		})
	}
}
