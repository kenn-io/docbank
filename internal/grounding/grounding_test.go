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
	}{
		{"escapes", `\$1\,205 [1]`, `$1,205`, "grounded", "found", []string{"$1,205"}},
		{"cited escapes", "$1,205", `\$1\,205`, "grounded", "found", []string{"$1,205"}},
		{"space group is whole", "200", "1 200", "ungrounded", "missing", []string{"200"}},
		{"space group is literal", "1200", "1 200", "ungrounded", "missing", []string{"1200"}},
		{"space group match", "1 200", "1 200", "grounded", "found", []string{"1 200"}},
		{"long space group", "1 200 300", "1 200 300", "grounded", "found", []string{"1 200 300"}},
		{"NBSP group", "200", "1\u00a0200", "ungrounded", "missing", []string{"200"}},
		{"narrow NBSP group", "200", "1\u202f200", "ungrounded", "missing", []string{"200"}},
		{"multiple spaces separate", "200", "1  200", "grounded", "found", []string{"200"}},
		{"long digits separate", "200", "1234 200", "grounded", "found", []string{"200"}},
		{"tab columns", "250 and 5", "Widget\t5\t250\t1,250", "grounded", "found", []string{"250", "5"}},
		{"newline separates", "1\n200", "1\n200", "grounded", "found", []string{"1", "200"}},
		{"newline cannot join", "1200", "1\n200", "ungrounded", "missing", []string{"1200"}},
		{"range stays whole", "1,500", "1,200-1,500", "ungrounded", "missing", []string{"1,500"}},
		{"range matches", "1,200-1,500", "1,200-1,500", "grounded", "found", []string{"1,200-1,500"}},
		{"comma sequence", "5,6,7", "5, 6, 7", "ungrounded", "missing", []string{"5,6,7"}},
		{"leading decimal", ".5", "5", "ungrounded", "missing", []string{".5"}},
		{"negative sign", "-5", "5", "ungrounded", "missing", []string{"-5"}},
		{"Unicode minus", "−5", "5", "ungrounded", "missing", []string{"−5"}},
		{"identifier before amount", "INV-5 had 200", "INV-5 200", "grounded", "found", []string{"INV-5", "200"}},
		{"quarter before amount", "500", "Q1 500 units", "grounded", "found", []string{"500"}},
		{"suffix currency", "20€", "20$", "ungrounded", "missing", []string{"20€"}},
		{"hyphen before lowercase", "5-year", "5 years", "grounded", "found", []string{"5"}},
		{"slash before lowercase", "$5/month", "$5 per month", "grounded", "found", []string{"$5"}},
		{"identifier stays whole", "INV-5", "5", "ungrounded", "missing", []string{"INV-5"}},
		{"identifier lowercase suffix", "INV-5-ab", "INV-5-cd", "ungrounded", "missing", []string{"INV-5-ab"}},
		{"identifier slash suffix", "INV-5/ab", "INV-5/cd", "ungrounded", "missing", []string{"INV-5/ab"}},
		{"swapped dates", "2024-01-05", "2024-05-01", "ungrounded", "missing", []string{"2024-01-05"}},
		{"uppercase suffix stays whole", "12-AB", "12-CD", "ungrounded", "missing", []string{"12-AB"}},
		{"marker separates figures", "5[1]6", "5 6", "grounded", "found", []string{"5", "6"}},
		{"em dash separates", "$1,250—matching", "$1,250", "grounded", "found", []string{"$1,250"}},
		{"horizontal bar separates", "$1,250―matching", "$1,250", "grounded", "found", []string{"$1,250"}},
		{"bare URL", "https://example.com/2024", "2024", "ungrounded", "missing", []string{"example.com/2024"}},
		{"URL Unicode boundary", "https://example.com/invoice\u00a0$1,250 [1]", "$1,205", "ungrounded", "missing", []string{"$1,250"}},
		{"cited brackets", "20", "Reference [20]", "grounded", "found", []string{"20"}},
		{"unknown marker", "20 [2]", "20", "ungrounded", "found", []string{"20"}},
		{"three digit marker", "100 [100]", "100", "ungrounded", "found", []string{"100"}},
		{"six digit marker", "20 [999999]", "20", "ungrounded", "found", []string{"20"}},
		{"zero marker", "20 [0]", "20", "ungrounded", "found", []string{"20"}},
		{"no figures", "An answer [1]", "20", "no_figures", "", []string{}},
		{"trim punctuation", "(**$1,205**) 20%", "$1,205 20%", "grounded", "found", []string{"$1,205", "20%"}},
		{"Markdown link text", "1205 and $1,250", "Totals: [Invoice 1205](<https://example.test/i/1205>) and **$1,250**.", "grounded", "found", []string{"1205", "$1,250"}},
		{"Markdown linked amount", "$1,205", "See [$1,205](https://example.test/inv)", "grounded", "found", []string{"$1,205"}},
		{"Markdown link query", "42", "[Safe](<https://example.test/report?id=42>)", "grounded", "found", []string{"42"}},
		{"Markdown bold neighbours", "5", "**5**:items", "grounded", "found", []string{"5"}},
		{"Markdown table pipes", "5", "a|5|b", "grounded", "found", []string{"5"}},
		{"Markdown code neighbours", "5", "a`5`b", "grounded", "found", []string{"5"}},
		{"Markdown strike neighbours", "5", "a~~5~~b", "grounded", "found", []string{"5"}},
		{"HTML delimiters", "5", "<b>5</b>", "grounded", "found", []string{"5"}},
		{"assignment delimiters", "5 and 6", "x=5;y=6", "grounded", "found", []string{"5", "6"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Prepare(tc.answer)
			require.NoError(t, err)
			report := a.Check([]Citation{{Index: 1, Text: tc.source, TextSHA256: "digest"}})
			require.Equal(t, tc.status, report.Status)
			figures := []Figure{}
			for _, value := range tc.figures {
				figures = append(figures, Figure{Text: value, Status: tc.figureStatus})
			}
			require.Equal(t, figures, report.Figures)
			require.Equal(t, []CitationResult{{Index: 1, Status: "resolved", TextSHA256: "digest"}}, report.Citations)
			unknown := []int{}
			switch tc.name {
			case "unknown marker":
				unknown = []int{2}
			case "zero marker":
				unknown = []int{0}
			case "three digit marker":
				unknown = []int{100}
			case "six digit marker":
				unknown = []int{999999}
			}
			require.Equal(t, unknown, report.UnknownMarkers)
		})
	}
	t.Run("all resolved citations contribute", func(t *testing.T) {
		a, err := Prepare("Fee 20 [1] rate 30 [2]")
		require.NoError(t, err)
		report := a.Check([]Citation{{Text: "30"}, {Text: "20"}})
		require.Equal(t, "grounded", report.Status)
		require.Equal(t, []Figure{{Text: "20", Status: "found"}, {Text: "30", Status: "found"}}, report.Figures)
		require.Equal(t, []CitationResult{{Index: 1, Status: "resolved"}, {Index: 2, Status: "resolved"}}, report.Citations)
	})
	t.Run("unavailable evidence", func(t *testing.T) {
		for _, tc := range []struct{ answer, status string }{
			{"20 [1]", "incomplete"}, {"30 [1]", "incomplete"}, {"words", "incomplete"}, {"20 [3]", "ungrounded"},
		} {
			a, err := Prepare(tc.answer)
			require.NoError(t, err)
			report := a.Check([]Citation{{Index: 1, Text: "20"}, {Index: 2, Err: "citation_unavailable"}})
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
	t.Run("maximum quotations stay bounded", func(t *testing.T) {
		a, err := Prepare("1 [1][2][3][4][5][6][7][8]")
		require.NoError(t, err)
		citations := make([]Citation, 8)
		for i := range citations {
			citations[i] = Citation{Index: i + 1, Text: "1" + strings.Repeat(",200", 3999)}
		}
		report := a.Check(citations)
		require.Equal(t, "ungrounded", report.Status)
		require.Equal(t, []Figure{{Text: "1", Status: "missing"}}, report.Figures)
	})
}
