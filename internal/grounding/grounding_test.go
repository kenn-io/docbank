package grounding

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGrounding(t *testing.T) {
	for _, tc := range []struct {
		name, answer, status string
		citations            []Citation
		figures              int
		figureStatus         string
	}{
		{"answer escapes", `\$1\,205 [1]`, "grounded", []Citation{{Index: 1, Text: "$1,205"}}, 1, "found"},
		{"literal email identifier itself", `A12\_B34 [1]`, "grounded", []Citation{{Index: 1, Text: `A12\_B34`}}, 1, "found"},
		{"literal email escape limitation", "A12_B34 [1]", "grounded", []Citation{{Index: 1, Text: `A12\_B34`}}, 1, "found"},
		{"literal email dash limitation", "INV-42 [1]", "grounded", []Citation{{Index: 1, Text: `INV\-42`}}, 1, "found"},
		{"space grouped answer limitation", "2 100 units [1]", "ungrounded", []Citation{{Index: 1, Text: "2 units and 100 units"}}, 1, "missing"},
		{"plain space columns limitation", "5250 [1]", "grounded", []Citation{{Index: 1, Text: "Widget 5 250 1,250"}}, 1, "found"},
		{"tab columns", "Unit price 250 for 5 widgets [1]", "grounded", []Citation{{Index: 1, Text: "Widget	5	250	1,250"}}, 2, "found"},
		{"tab columns invented amount", "5,250 [1]", "ungrounded", []Citation{{Index: 1, Text: "Widget	5	250	1,250"}}, 1, "missing"},
		{"newline grouping", "1200 [1]", "ungrounded", []Citation{{Index: 1, Text: "1\n200"}}, 1, "missing"},
		{"range stays whole", "1,200-1,500 [1]", "ungrounded", []Citation{{Index: 1, Text: "1,500"}}, 1, "missing"},
		{"range tail stays whole", "500 [1]", "ungrounded", []Citation{{Index: 1, Text: "1,200-1,500"}}, 1, "missing"},
		{"range endpoint limitation", "1,500 [1]", "ungrounded", []Citation{{Index: 1, Text: "1,200-1,500"}}, 1, "missing"},
		{"range grouping", "1200-1500 [1]", "grounded", []Citation{{Index: 1, Text: "1,200-1,500"}}, 1, "found"},
		{"unicode dash", "3–5 [1]", "grounded", []Citation{{Index: 1, Text: "3-5"}}, 1, "found"},
		{"range separate endpoints limitation", "2020 to 2024 [1]", "ungrounded", []Citation{{Index: 1, Text: "2020-2024"}}, 2, "missing"},
		{"leading answer amount", "1250. That's the total for 2024 [1]", "ungrounded", []Citation{{Index: 1, Text: "1205 for 2024"}}, 2, "missing"},
		{"standalone leading amount", "1250.\nPaid in full [1]", "ungrounded", []Citation{{Index: 1, Text: "1205"}}, 1, "missing"},
		{"leading year", "2024) revenue was up [1]", "ungrounded", []Citation{{Index: 1, Text: "2025"}}, 1, "missing"},
		{"suffix currency", "20€ [1]", "ungrounded", []Citation{{Index: 1, Text: "20$"}}, 1, "missing"},
		{"suffix versus bare", "20€ [1]", "ungrounded", []Citation{{Index: 1, Text: "20"}}, 1, "missing"},
		{"currency before sign", "$-5 [1]", "ungrounded", []Citation{{Index: 1, Text: "€-5"}}, 1, "missing"},
		{"currency grouping", "$1,250 [1]", "grounded", []Citation{{Index: 1, Text: "$1250"}}, 1, "found"},
		{"currency decimals deferred", "$1,250 [1]", "ungrounded", []Citation{{Index: 1, Text: "$1,250.00"}}, 1, "missing"},
		{"currency bare limitation", "1250 [1]", "ungrounded", []Citation{{Index: 1, Text: "$1,250"}}, 1, "missing"},
		{"swapped dates", "2024-01-05 [1]", "ungrounded", []Citation{{Index: 1, Text: "2024-05-01"}}, 1, "missing"},
		{"leading marker limitation", "Per [1], total was $1,250.", "incomplete", []Citation{{Index: 1, Text: "$1205"}}, 1, "unverifiable"},
		{"unsigned versus signed", "5 [1]", "ungrounded", []Citation{{Index: 1, Text: "-5"}}, 1, "missing"},
		{"percent suffix", "20% [1]", "ungrounded", []Citation{{Index: 1, Text: "20"}}, 1, "missing"},
		{"code point offsets", "é 20 [1]", "grounded", []Citation{{Index: 1, Text: "20"}}, 1, "found"},
		{"invoice mismatch", "$1,250 [1]", "ungrounded", []Citation{{Index: 1, Text: `\$1\,205`}}, 1, "missing"},
		{"invoice match", "$1,250 [1]", "grounded", []Citation{{Index: 1, Text: `\$1\,250`}}, 1, "found"},
		{"grouped space", "1200 [1]", "grounded", []Citation{{Index: 1, Text: "1 200"}}, 1, "found"},
		{"grouped boundary", "200 [1]", "ungrounded", []Citation{{Index: 1, Text: "1 200"}}, 1, "missing"},
		{"grouped comma", "1200 [1]", "grounded", []Citation{{Index: 1, Text: "1,200"}}, 1, "found"},
		{"single comparison value", "1,250 [1]", "grounded", []Citation{{Index: 1, Text: "1250"}}, 1, "found"},
		{"compact unit", "Weight is 20kg [1]", "grounded", []Citation{{Index: 1, Text: "20kg"}}, 1, "found"},
		{"spaced unit", "20kg [1]", "ungrounded", []Citation{{Index: 1, Text: "20 kg"}}, 1, "missing"},
		{"url and email", "https://example.com/doc/123 user42@example.com 7 [1]", "grounded", []Citation{{Index: 1, Text: "7"}}, 1, "found"},
		{"cited url", "123 [1]", "ungrounded", []Citation{{Index: 1, Text: "https://example.com/doc/123"}}, 1, "missing"},
		{"cited url figure", "2024 [1]", "grounded", []Citation{{Index: 1, Text: "https://example.com/?year=2024"}}, 1, "found"},
		{"cited email figure", "123 [1]", "grounded", []Citation{{Index: 1, Text: "123@example.com"}}, 1, "found"},
		{"signed", "-20 [1]", "ungrounded", []Citation{{Index: 1, Text: "20"}}, 1, "missing"},
		{"unicode sign", "−20 [1]", "grounded", []Citation{{Index: 1, Text: "-20"}}, 1, "found"},
		{"currencies", "$5 [1]", "ungrounded", []Citation{{Index: 1, Text: "€5"}}, 1, "missing"},
		{"currency retained", "$5 [1]", "ungrounded", []Citation{{Index: 1, Text: "5"}}, 1, "missing"},
		{"leading decimal stays literal", ".5 [1]", "ungrounded", []Citation{{Index: 1, Text: "0.5"}}, 1, "missing"},
		{"decimal zeros stay literal", "1.200 [1]", "ungrounded", []Citation{{Index: 1, Text: "1.2"}}, 1, "missing"},
		{"leading zeros", "0042 [1]", "ungrounded", []Citation{{Index: 1, Text: "42"}}, 1, "missing"},
		{"whole identifier", "K5 [1]", "ungrounded", []Citation{{Index: 1, Text: "K50"}}, 1, "missing"},
		{"identifier case", "INV-2024-0042 [1]", "grounded", []Citation{{Index: 1, Text: "inv-2024-0042"}}, 1, "found"},
		{"identifier punctuation", "K-5 [1]", "ungrounded", []Citation{{Index: 1, Text: "K5"}}, 1, "missing"},
		{"mixed space grouping", "1200300 [1]", "grounded", []Citation{{Index: 1, Text: "1 200\u202f300"}}, 1, "found"},
		{"unicode grouping", "1200 [1]", "grounded", []Citation{{Index: 1, Text: "1\u202f200"}}, 1, "found"},
		{"list ordinal", "1. Fee 20 [1]", "ungrounded", []Citation{{Index: 1, Text: "20"}}, 2, "missing"},
		{"empty list item", "1.\nFee 20 [1]", "ungrounded", []Citation{{Index: 1, Text: "20"}}, 2, "missing"},
		{"cited ordinal", "1250 [1]", "grounded", []Citation{{Index: 1, Text: "1250. Paid in full"}}, 1, "found"},
		{"cited year", "2024 [1]", "grounded", []Citation{{Index: 1, Text: `2024\. Revenue`}}, 1, "found"},
		{"tab boundary", "5250 [1]", "ungrounded", []Citation{{Index: 1, Text: "5\t250"}}, 1, "missing"},
		{"line boundary", "5250 [1]", "ungrounded", []Citation{{Index: 1, Text: "5\n250"}}, 1, "missing"},
		{"table columns", "5 and 250 [1]", "grounded", []Citation{{Index: 1, Text: "5\t250"}}, 2, "found"},
		{"comma list", "5,6,7 [1]", "grounded", []Citation{{Index: 1, Text: "5, 6, 7"}}, 3, "found"},
		{"stacked markers", "1250 [1][2]", "grounded", []Citation{{Index: 1, Text: "1205"}, {Index: 2, Text: "1250"}}, 1, "found"},
		{"whitespace markers", "1250 [1] \t\n[2]", "grounded", []Citation{{Index: 1, Text: "1205"}, {Index: 2, Text: "1250"}}, 1, "found"},
		{"long url", "https://example.com/" + strings.Repeat("x", 15900), "no_figures", []Citation{{Index: 1, Text: "7"}}, 0, ""},
		{"no figures", "See invoice [1]", "no_figures", []Citation{{Index: 1, Text: "20"}}, 0, ""},
		{"swapped scope", "Fee 20.[1] Rate 30.[2]", "ungrounded", []Citation{{Index: 1, Text: "30"}, {Index: 2, Text: "20"}}, 2, "missing"},
		{"unknown marker without figures", "See invoice [2]", "ungrounded", []Citation{{Index: 1, Text: "20"}}, 0, ""},
		{"unknown marker", "20 [2]", "ungrounded", []Citation{{Index: 1, Text: "20"}}, 1, "unverifiable"},
		{"unmarked tail", "20 [1] then 30", "incomplete", []Citation{{Index: 1, Text: "20 30"}}, 2, "found"},
		{"marker spans sentences", "Fee 20. Rate 30 [1]", "grounded", []Citation{{Index: 1, Text: "20 30"}}, 2, "found"},
		{"no markers", "20", "grounded", []Citation{{Index: 1, Text: "20"}}, 1, "found"},
		{"unavailable", "20 [1]", "incomplete", []Citation{{Index: 1, Err: "citation_unavailable"}}, 1, "unverifiable"},
		{"found with unavailable", "20 [1, 2]", "grounded", []Citation{{Index: 1, Text: "20"}, {Index: 2, Err: "citation_unavailable"}}, 1, "found"},
		{"missing precedes unavailable", "20 [1]", "ungrounded", []Citation{{Index: 1, Text: "30"}, {Index: 2, Err: "citation_unavailable"}}, 1, "missing"},
		{"no figures unavailable", "See invoice", "no_figures", []Citation{{Index: 1, Err: "citation_unavailable"}}, 0, ""},
		{"long brackets", "[123]", "grounded", []Citation{{Index: 1, Text: "123"}}, 1, "found"},
		{"link label literal", "[20](url)", "grounded", []Citation{{Index: 1, Text: "20"}}, 1, "found"},
		{"cited marker", "20 [1]", "grounded", []Citation{{Index: 1, Text: "Reference [20]"}}, 1, "found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Prepare(tc.answer)
			require.NoError(t, err)
			report := a.Check(tc.citations)
			require.Equal(t, tc.status, report.Status)
			require.Len(t, report.Figures, tc.figures)
			if tc.figures > 0 {
				require.Equal(t, tc.figureStatus, report.Figures[0].Status)
			}
			for _, f := range report.Figures {
				require.Equal(t, f.Text, string([]rune(tc.answer)[f.Start:f.End]))
			}
			if tc.name == "stacked markers" {
				require.Equal(t, []int{1, 2}, report.Figures[0].Citations)
				require.Equal(t, []int{2}, report.Figures[0].FoundIn)
			}
			if tc.name == "code point offsets" {
				require.Equal(t, 2, report.Figures[0].Start)
			}
			if tc.name == "unknown marker" {
				require.Equal(t, []Marker{{Index: 2, Start: 3, End: 6}}, report.UnknownMarkers)
			}
			if tc.name == "unmarked tail" {
				require.Empty(t, report.Figures[1].Citations)
				require.Equal(t, "unverifiable", report.Figures[1].Status)
			}
		})
	}
	t.Run("limits", func(t *testing.T) {
		for _, token := range []string{"K5 ", "[1] "} {
			_, err := Prepare(strings.Repeat(token, 512))
			require.NoError(t, err)
			_, err = Prepare(strings.Repeat(token, 513))
			require.Error(t, err)
		}
	})
	t.Run("maximum quotations stay bounded", func(t *testing.T) {
		a, err := Prepare("1 [1, 2, 3, 4, 5, 6, 7, 8]")
		require.NoError(t, err)
		citations := make([]Citation, 8)
		for i := range citations {
			citations[i] = Citation{Index: i + 1, Text: "1" + strings.Repeat(",200", 3999)}
		}
		start := time.Now()
		report := a.Check(citations)
		require.Equal(t, "ungrounded", report.Status)
		require.Empty(t, report.Figures[0].FoundIn)
		require.Less(t, time.Since(start), 2*time.Second)
	})
}
