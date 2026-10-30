package grounding

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceListAmountEvidence(t *testing.T) {
	for _, tc := range []struct{ answer, source, overall, figure string }{
		{"$40", "- $40", "grounded", "found"},
		{"-$40", "- $40", "ungrounded", "missing"},
		{"$40", "+ $40", "grounded", "found"},
		{"-$40", "+ $40", "ungrounded", "missing"},
		{"$40", "- 40 USD", "grounded", "found"},
		{"$40", "-     $40", "grounded", "found"},
		{"$40", "  - $40", "grounded", "found"},
		{"$40", "- parent\n  - $40", "grounded", "found"},
		{"$40", "- - $40", "grounded", "found"},
		{"$40", "> - $40", "grounded", "found"},
		{"-$40", "```\n- $40\n```", "grounded", "found"},
		{"-$40", "- ```\n  - $40\n  ```", "grounded", "found"},
		{"-$40", "    - $40", "grounded", "found"},
		{"-$40", `\- $40`, "grounded", "found"},
		{"-$40", "Cost: - $40", "grounded", "found"},
		{"−$40", "− $40", "grounded", "found"},
		{"- 40 USD", "USD -40", "grounded", "found"},
	} {
		a, err := Prepare(tc.answer)
		require.NoError(t, err)
		r := a.Check([]Citation{{Text: tc.source, AtDocumentStart: true}})
		require.Equal(t, tc.overall, r.Status, "%q / %q: %+v", tc.answer, tc.source, r)
		require.Equal(t, tc.figure, r.Figures[0].Status)
	}
	for _, source := range []string{"- $40", "+ $40", "- $40[1]", "- 40 USD[1]", "- 40", "+ 40", "- -USD40", "- +USD40", "  - 40 USD", "text\n- $40", "```\n- $40", "  > - $40", `- \$40`} {
		for _, answer := range []string{"$40", "-$40", "+$40", "€40", "40"} {
			a, err := Prepare(answer)
			require.NoError(t, err)
			report := a.Check([]Citation{{Text: source}})
			require.Equal(t, "ungrounded", report.Status, "%q / %q: %+v", answer, source, report)
			require.Equal(t, "missing", report.Figures[0].Status, "%q / %q", answer, source)
		}
	}
	a, err := Prepare("USD 75")
	require.NoError(t, err)
	require.Equal(t, "grounded", a.Check([]Citation{{Text: "- $40 then USD 75"}}).Status)
	a, err = Prepare("$75")
	require.NoError(t, err)
	report := a.Check([]Citation{{Text: "- $40\nCost $75"}})
	require.Equal(t, "grounded", report.Status)
	a, err = Prepare("-$40")
	require.NoError(t, err)
	report = a.Check([]Citation{{Text: "- $40"}, {Text: "USD -40"}})
	require.Equal(t, "grounded", report.Status)
}
