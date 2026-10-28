package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGrounding(t *testing.T) {
	for _, tc := range []struct {
		name, answer, source, status, figureStatus string
		figures                                    []string
		unknownMarkers                             []int
	}{
		{"escapes", `\$1\,205 [1]`, `$1,205`, "grounded", "found", []string{"$1,205"}, []int{}},
		{"cited escapes", "$1,205", `\$1\,205`, "grounded", "found", []string{"$1,205"}, []int{}},
		{"space group is whole", "200", "1 200", "ungrounded", "missing", []string{"200"}, []int{}},
		{"space group is literal", "1200", "1 200", "ungrounded", "missing", []string{"1200"}, []int{}},
		{"space group match", "1 200", "1 200", "grounded", "found", []string{"1 200"}, []int{}},
		{"quoted space group", "“1 200”", "1 200", "grounded", "found", []string{"1 200"}, []int{}},
		{"long space group", "1 200 300", "1 200 300", "grounded", "found", []string{"1 200 300"}, []int{}},
		{"NBSP group", "200", "1\u00a0200", "ungrounded", "missing", []string{"200"}, []int{}},
		{"narrow NBSP group", "200", "1\u202f200", "ungrounded", "missing", []string{"200"}, []int{}},
		{"multiple spaces separate", "200", "1  200", "grounded", "found", []string{"200"}, []int{}},
		{"long digits separate", "200", "1234 200", "grounded", "found", []string{"200"}, []int{}},
		{"tab columns", "250 and 5", "Widget\t5\t250\t1,250", "grounded", "found", []string{"250", "5"}, []int{}},
		{"newline cannot join", "1200", "1\n200", "ungrounded", "missing", []string{"1200"}, []int{}},
		{"range stays whole", "1,500", "1,200-1,500", "ungrounded", "missing", []string{"1,500"}, []int{}},
		{"range matches", "1,200-1,500", "1,200-1,500", "grounded", "found", []string{"1,200-1,500"}, []int{}},
		{"comma sequence", "5,6,7", "5, 6, 7", "ungrounded", "missing", []string{"5,6,7"}, []int{}},
		{"leading decimal", ".5", "5", "ungrounded", "missing", []string{".5"}, []int{}},
		{"negative sign", "-5", "5", "ungrounded", "missing", []string{"-5"}, []int{}},
		{"Unicode minus", "−5", "5", "ungrounded", "missing", []string{"−5"}, []int{}},
		{"identifier before amount", "INV-5 had 200", "INV-5 200", "grounded", "found", []string{"INV-5", "200"}, []int{}},
		{"suffix currency", "20€", "20$", "ungrounded", "missing", []string{"20€"}, []int{}},
		{"hyphen before lowercase", "5-year", "5 years", "grounded", "found", []string{"5"}, []int{}},
		{"slash before lowercase", "$5/month", "$5 per month", "grounded", "found", []string{"$5"}, []int{}},
		{"identifier stays whole", "INV-5", "5", "ungrounded", "missing", []string{"INV-5"}, []int{}},
		{"identifier lowercase suffix", "INV-5-ab", "INV-5-cd", "ungrounded", "missing", []string{"INV-5-ab"}, []int{}},
		{"identifier slash suffix", "INV-5/ab", "INV-5/cd", "ungrounded", "missing", []string{"INV-5/ab"}, []int{}},
		{"swapped dates", "2024-01-05", "2024-05-01", "ungrounded", "missing", []string{"2024-01-05"}, []int{}},
		{"uppercase suffix stays whole", "12-AB", "12-CD", "ungrounded", "missing", []string{"12-AB"}, []int{}},
		{"marker separates figures", "5[1]6", "5 6", "grounded", "found", []string{"5", "6"}, []int{}},
		{"marker separates three digits", "1[1]200", "1\n200", "grounded", "found", []string{"1", "200"}, []int{}},
		{"em dash separates", "$1,250—matching", "$1,250", "grounded", "found", []string{"$1,250"}, []int{}},
		{"horizontal bar separates", "$1,250―matching", "$1,250", "grounded", "found", []string{"$1,250"}, []int{}},
		{"bare URL", "https://example.com/2024", "2024", "ungrounded", "missing", []string{"https://example.com/2024"}, []int{}},
		{"URL Unicode boundary", "https://example.com/invoice\u00a0$1,250 [1]", "$1,205", "ungrounded", "missing", []string{"$1,250"}, []int{}},
		{"cited brackets", "20", "Reference [20]", "grounded", "found", []string{"20"}, []int{}},
		{"unknown marker", "20 [2]", "20", "ungrounded", "found", []string{"20"}, []int{2}},
		{"three digit marker", "100 [100]", "100", "ungrounded", "found", []string{"100"}, []int{100}},
		{"six digit marker", "20 [999999]", "20", "ungrounded", "found", []string{"20"}, []int{999999}},
		{"zero marker", "20 [0]", "20", "ungrounded", "found", []string{"20"}, []int{0}},
		{"no figures", "An answer [1]", "20", "no_figures", "", []string{}, []int{}},
		{"trim punctuation", "(**$1,205**) 20%", "$1,205 20%", "grounded", "found", []string{"$1,205", "20%"}, []int{}},
		{"Markdown link text", "1205 and $1,250", "Totals: [Invoice 1205](<https://example.test/i/1205>) and **$1,250**.", "grounded", "found", []string{"1205", "$1,250"}, []int{}},
		{"Markdown linked amount", "$1,205", "See [$1,205](https://example.test/inv)", "grounded", "found", []string{"$1,205"}, []int{}},
		{"Markdown link query", "42", "[Safe](<https://example.test/report?id=42>)", "grounded", "found", []string{"42"}, []int{}},
		{"Markdown bold neighbours", "5", "**5**:items", "grounded", "found", []string{"5"}, []int{}},
		{"Markdown table pipes", "5", "a|5|b", "grounded", "found", []string{"5"}, []int{}},
		{"Markdown code neighbours", "5", "a`5`b", "grounded", "found", []string{"5"}, []int{}},
		{"Markdown strike neighbours", "5", "a~~5~~b", "grounded", "found", []string{"5"}, []int{}},
		{"HTML delimiters", "5", "<b>5</b>", "grounded", "found", []string{"5"}, []int{}},
		{"assignment delimiters", "5 and 6", "x=5;y=6", "grounded", "found", []string{"5", "6"}, []int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Prepare(tc.answer)
			require.NoError(t, err)
			report := a.Check([]Citation{{Text: tc.source, TextSHA256: "digest"}})
			require.Equal(t, tc.status, report.Status)
			figures := []Figure{}
			for _, value := range tc.figures {
				figures = append(figures, Figure{Text: value, Status: tc.figureStatus})
			}
			require.Equal(t, figures, report.Figures)
			require.Equal(t, []CitationResult{{Index: 1, Status: "resolved", TextSHA256: "digest"}}, report.Citations)
			require.Equal(t, tc.unknownMarkers, report.UnknownMarkers)
		})
	}
	t.Run("all resolved citations contribute", func(t *testing.T) {
		a, err := Prepare("Fee 20 [1] rate 30 [2]")
		require.NoError(t, err)
		report := a.Check([]Citation{{Text: "30", TextSHA256: "digest-30"}, {Text: "20", TextSHA256: "digest-20"}})
		require.Equal(t, "grounded", report.Status)
		require.Equal(t, []Figure{{Text: "20", Status: "found"}, {Text: "30", Status: "found"}}, report.Figures)
		require.Equal(t, []CitationResult{{Index: 1, Status: "resolved", TextSHA256: "digest-30"}, {Index: 2, Status: "resolved", TextSHA256: "digest-20"}}, report.Citations)
	})
	t.Run("unavailable evidence", func(t *testing.T) {
		for _, tc := range []struct{ answer, status string }{
			{"20 [1]", "incomplete"}, {"30 [1]", "incomplete"}, {"words", "incomplete"}, {"20 [3]", "ungrounded"},
		} {
			a, err := Prepare(tc.answer)
			require.NoError(t, err)
			report := a.Check([]Citation{{Text: "20", TextSHA256: "digest"}, {Err: "citation_unavailable"}})
			require.Equal(t, tc.status, report.Status)
			require.Equal(t, CitationResult{Index: 2, Status: "citation_unavailable"}, report.Citations[1])
		}
	})
	t.Run("limits", func(t *testing.T) {
		for _, token := range []string{"K5 ", "[1] "} {
			_, err := Prepare(strings.Repeat(token, 512))
			require.NoError(t, err)
			_, err = Prepare(strings.Repeat(token, 513))
			require.Error(t, err)
		}
	})
}
