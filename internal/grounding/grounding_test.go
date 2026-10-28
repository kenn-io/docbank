package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGrounding(t *testing.T) {
	for _, tc := range []struct {
		name, answer, status string
		citations            []Citation
		figures              int
		figureStatus         string
	}{
		{"invoice mismatch", "$1,250 [1]", "ungrounded", []Citation{{Index: 1, Text: `\$1\,205`}}, 1, "missing"},
		{"invoice match", "$1,250 [1]", "grounded", []Citation{{Index: 1, Text: `\$1\,250`}}, 1, "found"},
		{"grouped space", "1200 [1]", "grounded", []Citation{{Index: 1, Text: "1 200"}}, 1, "found"},
		{"grouped boundary", "200 [1]", "ungrounded", []Citation{{Index: 1, Text: "1 200"}}, 1, "missing"},
		{"grouped comma", "1200 [1]", "grounded", []Citation{{Index: 1, Text: "1,200"}}, 1, "found"},
		{"compact unit", "Weight is 20kg [1]", "grounded", []Citation{{Index: 1, Text: "20kg"}}, 1, "found"},
		{"spaced unit", "20kg [1]", "ungrounded", []Citation{{Index: 1, Text: "20 kg"}}, 1, "missing"},
		{"url and email", "https://example.com/doc/123 user42@example.com 7 [1]", "grounded", []Citation{{Index: 1, Text: "7"}}, 1, "found"},
		{"cited url", "123 [1]", "ungrounded", []Citation{{Index: 1, Text: "https://example.com/doc/123"}}, 1, "missing"},
		{"signed", "-20 [1]", "ungrounded", []Citation{{Index: 1, Text: "20"}}, 1, "missing"},
		{"unicode sign", "−20 [1]", "grounded", []Citation{{Index: 1, Text: "-20"}}, 1, "found"},
		{"currencies", "$5 [1]", "ungrounded", []Citation{{Index: 1, Text: "€5"}}, 1, "missing"},
		{"currency retained", "$5 [1]", "ungrounded", []Citation{{Index: 1, Text: "5"}}, 1, "missing"},
		{"leading decimal", ".5 [1]", "grounded", []Citation{{Index: 1, Text: "0.5"}}, 1, "found"},
		{"decimal zeros", "1.200 [1]", "grounded", []Citation{{Index: 1, Text: "1.2"}}, 1, "found"},
		{"leading zeros", "0042 [1]", "ungrounded", []Citation{{Index: 1, Text: "42"}}, 1, "missing"},
		{"whole identifier", "K5 [1]", "ungrounded", []Citation{{Index: 1, Text: "K50"}}, 1, "missing"},
		{"identifier case", "INV-2024-0042 [1]", "grounded", []Citation{{Index: 1, Text: "inv-2024-0042"}}, 1, "found"},
		{"identifier punctuation", "K-5 [1]", "ungrounded", []Citation{{Index: 1, Text: "K5"}}, 1, "missing"},
		{"unicode grouping", "1200 [1]", "grounded", []Citation{{Index: 1, Text: "1\u202f200"}}, 1, "found"},
		{"list ordinal", "1. Fee 20 [1]", "grounded", []Citation{{Index: 1, Text: "20"}}, 1, "found"},
		{"empty list item", "1.\nFee 20 [1]", "grounded", []Citation{{Index: 1, Text: "20"}}, 1, "found"},
		{"cited ordinal", "1 [1]", "ungrounded", []Citation{{Index: 1, Text: "1. Fee"}}, 1, "missing"},
		{"no figures", "See invoice [1]", "no_figures", []Citation{{Index: 1, Text: "20"}}, 0, ""},
		{"swapped scope", "Fee 20.[1] Rate 30.[2]", "ungrounded", []Citation{{Index: 1, Text: "30"}, {Index: 2, Text: "20"}}, 2, "missing"},
		{"unknown marker", "20 [2]", "ungrounded", []Citation{{Index: 1, Text: "20"}}, 1, "unverifiable"},
		{"unmarked tail", "20 [1] then 30", "incomplete", []Citation{{Index: 1, Text: "20 30"}}, 2, "found"},
		{"marker spans sentences", "Fee 20. Rate 30 [1]", "grounded", []Citation{{Index: 1, Text: "20 30"}}, 2, "found"},
		{"no markers", "20", "grounded", []Citation{{Index: 1, Text: "20"}}, 1, "found"},
		{"unavailable", "20 [1]", "incomplete", []Citation{{Index: 1, Err: "citation_unavailable"}}, 1, "unverifiable"},
		{"found with unavailable", "20 [1, 2]", "incomplete", []Citation{{Index: 1, Text: "20"}, {Index: 2, Err: "citation_unavailable"}}, 1, "found"},
		{"missing precedes unavailable", "20 [1]", "ungrounded", []Citation{{Index: 1, Text: "30"}, {Index: 2, Err: "citation_unavailable"}}, 1, "missing"},
		{"no figures unavailable", "See invoice", "incomplete", []Citation{{Index: 1, Err: "citation_unavailable"}}, 0, ""},
		{"long brackets", "[123]", "grounded", []Citation{{Index: 1, Text: "123"}}, 1, "found"},
		{"link label literal", "[20](url)", "grounded", []Citation{{Index: 1, Text: "20"}}, 1, "found"},
		{"cited markers ignored", "20 [1]", "ungrounded", []Citation{{Index: 1, Text: "[20]"}}, 1, "missing"},
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
	t.Run("reuse prepared answer", func(t *testing.T) {
		a, err := Prepare("é 20 [1]")
		require.NoError(t, err)
		found := a.Check([]Citation{{Index: 1, Text: "20"}})
		require.Equal(t, []int{1}, found.Figures[0].FoundIn)
		require.Equal(t, 2, found.Figures[0].Start)
		missing := a.Check([]Citation{{Index: 1, Text: "30"}})
		require.Equal(t, "missing", missing.Figures[0].Status)
		require.Empty(t, missing.Figures[0].FoundIn)
	})
}
