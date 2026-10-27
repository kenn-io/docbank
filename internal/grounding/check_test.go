package grounding

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.kenn.io/docbank/document"
)

func TestFiguresMatchWholeNormalizedValues(t *testing.T) {
	cases := []struct{ answer, source, verdict string }{
		{"30", "130", "ungrounded"}, {"1,999", "1999", "grounded"}, {"１２", "12", "grounded"},
		{"1,5", "1.5", "grounded"}, {"1.200", "1.2", "grounded"}, {"0042", "42", "ungrounded"},
		{"K5", "K50", "ungrounded"}, {"ZQ 3S", "zq3s", "grounded"},
		{"INV-2026-0042", "inv/2026/0042", "grounded"}, {"INV-2026-0042", "INV 2026 0042", "ungrounded"},
		{`\$20`, "20", "grounded"}, {"-12", "12", "ungrounded"}, {"-12", "-12", "grounded"},
		{"12345678901234567890.123456789", "12345678901234567890.123456788", "ungrounded"},
		{"2026-03-15", "2026-03-15", "grounded"}, {"4K", "4", "grounded"},
		{"ZQ 3S", "ZQ 3S-4", "ungrounded"}, {"A 1", "A 1.5", "ungrounded"},
		{"USD 1,200", "1200", "grounded"}, {"1\u202f234.50", "1234.5", "grounded"},
		{"12-34", "12 and 34", "grounded"},
		{"ZQ 3S", "ZQ 3S--4", "ungrounded"}, {"A 1", "A 1..5", "ungrounded"}, {"A 1", "A 1\u202f234", "ungrounded"},
		{"30,000", "30,000abc-def", "ungrounded"}, {"1.5mg", "1.5", "grounded"},
		{"ZQ 3S", "zq 3s", "grounded"}, {"zq 3s", "ZQ 3S", "grounded"},
		{"ZQ3S [1]", "ZQ 3Sα", "ungrounded"}, {"ZQ3S [1]", "ZQ 3Sβ", "ungrounded"},
		{"ZQ3S [1]", "ZQ 3Ś", "ungrounded"}, {"ZQ3S [1]", "ZQ 3S-α", "ungrounded"}, {"ZQ3S [1]", "ZQ 3S/́4", "ungrounded"},
		{"Fee 20", "20", "grounded"}, {"fee 20", "20", "grounded"},
		{"USD 20", "20", "grounded"}, {"usd 20", "20", "grounded"},
	}
	for _, tc := range cases {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != tc.verdict {
				t.Fatalf("got %+v", r)
			}
			if tc.verdict == "grounded" {
				want := map[string][]string{
					"1,999": {"1,999"}, "１２": {"１２"}, "1,5": {"1,5"}, "1.200": {"1.200"},
					"ZQ 3S": {"ZQ 3S"}, "zq 3s": {"zq 3s"}, "INV-2026-0042": {"INV-2026-0042"},
					`\$20`: {"20"}, "-12": {"-12"}, "2026-03-15": {"2026-03-15"}, "4K": {"4K"},
					"USD 1,200": {"1,200"}, "1\u202f234.50": {"1\u202f234.50"}, "12-34": {"12", "34"},
					"1.5mg": {"1.5mg"}, "Fee 20": {"20"}, "fee 20": {"20"}, "USD 20": {"20"}, "usd 20": {"20"},
				}[tc.answer]
				if len(want) == 0 || len(r.Claims) != 1 || len(r.Claims[0].Figures) != len(want) {
					t.Fatalf("expected figures %v: %+v", want, r)
				}
				for i, f := range r.Claims[0].Figures {
					if f.Text != want[i] || f.Status != "found" {
						t.Fatalf("expected found figure %q: %+v", want[i], f)
					}
				}
			}
		})
	}
}

func TestAdjacentUnitQuantitiesRemainSeparate(t *testing.T) {
	text := "é 12GB,34GB"
	figures := extract(text)
	if len(figures) != 2 {
		t.Fatalf("figures: %+v", figures)
	}
	for i, want := range []string{"12GB", "34GB"} {
		f := figures[i]
		if f.Text != want || f.Text != string([]rune(text)[f.Start:f.End]) {
			t.Fatalf("original span: %+v", f)
		}
	}
	if r := Check("12 [1]", []Citation{{Index: 1, Text: "112GB,34GB"}}); r.Status != "ungrounded" {
		t.Fatalf("partial quantity: %+v", r)
	}
	for _, tc := range []struct{ answer, source, figure string }{
		{"12 [1]", "12GB,34GB", "12"}, {"34 [1]", "12GB,34GB", "34"},
		{"1200 [1]", "1,200GB,34GB", "1200"}, {"1.5 [1]", "1.5GB,34GB", "1.5"},
		{"123-45-6789 [1]", "123-45-6789,34GB", "123-45-6789"}, {"34 [1]", "123-45-6789,34GB", "34"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != "grounded" || len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 {
				t.Fatalf("%+v", r)
			}
			f := r.Claims[0].Figures[0]
			if f.Text != tc.figure || f.Status != "found" || f.Text != string([]rune(tc.answer)[f.Start:f.End]) {
				t.Fatalf("original span: %+v", f)
			}
		})
	}
}

func TestClaimsMarkersAndFailures(t *testing.T) {
	r := Check("1. Fee 20. [1] [2] Next 30 [3]. Link [label](url).", []Citation{{Index: 1, Text: "20"}, {Index: 2, Err: "citation_unavailable"}})
	if len(r.Claims) != 3 {
		t.Fatalf("claims: %+v", r.Claims)
	}
	if len(r.Claims[0].Citations) != 2 || r.Claims[0].Verdict != "grounded" || len(r.Claims[0].Figures) != 1 {
		t.Fatalf("first: %+v", r.Claims[0])
	}
	if r.Claims[1].Verdict != "ungrounded" || len(r.Claims[1].UnknownMarkers) != 1 {
		t.Fatalf("second: %+v", r.Claims[1])
	}
	if r.Claims[2].Verdict != "no_figures" {
		t.Fatalf("third: %+v", r.Claims[2])
	}
	if r.Status != "ungrounded" {
		t.Fatalf("status: %s", r.Status)
	}
	b, err := json.Marshal(r)
	if err != nil || strings.Contains(string(b), ":null") {
		t.Fatalf("JSON: %s %v", b, err)
	}
}

func TestPreflightBounds(t *testing.T) {
	for _, s := range []string{"", strings.Repeat("a", 16001), strings.Repeat("1 ", 513), strings.Repeat("[1] ", 513), strings.Repeat("x. ", 257), "x [999999999999999999999999999999999999]"} {
		if Preflight(s) == nil {
			t.Fatalf("accepted %q", s[:min(len(s), 40)])
		}
	}
	if err := Preflight("one [1]"); err != nil {
		t.Fatal(err)
	}
}

func TestGroupingSeparatorsOnlyJoinDigitGroups(t *testing.T) {
	for _, separator := range []string{"\u00a0", "\u202f", "'"} {
		answer := "20" + separator + "total"
		if got := Check(answer, []Citation{{Index: 1, Text: "20total"}}); got.Status != "ungrounded" {
			t.Fatalf("%q merged prose into an identifier: %+v", answer, got)
		}
		if got := Check(answer, []Citation{{Index: 1, Text: "20"}}); got.Status != "grounded" {
			t.Fatalf("%q lost its numeric figure: %+v", answer, got)
		}
	}
}

func TestCompactCurrencyNegativeSign(t *testing.T) {
	for _, tc := range []struct{ answer, source, status, figure string }{
		{"Balance -USD20 [1]", "Balance USD20", "ungrounded", "-USD20"},
		{"Balance -USD20k [1]", "Balance USD20k", "ungrounded", "-USD20k"},
		{"USD20k [1]", "-USD20k", "ungrounded", "USD20k"},
		{"-USD20k [1]", "-20", "grounded", "-USD20k"},
		{"-20 [1]", "-USD20k", "grounded", "-20"},
		{"-USD20k [1]", "-USD 20k", "grounded", "-USD20k"},
		{"-USD20k [1]", "-$20k", "grounded", "-USD20k"},
		{"-USD20k [1]", "-20000", "ungrounded", "-USD20k"},
		{"-USD1..2k [1]", "-1..2", "grounded", "-USD1..2k"},
		{"-USD20k [1]", "-USD20k,34GB", "grounded", "-USD20k"},
		{"é −USD1.5GB [1]", "-1.5", "grounded", "−USD1.5GB"},
		{"-USD20ABCD [1]", "USD20ABCD", "grounded", "USD20ABCD"},
		{"-USD20kα [1]", "USD20kα", "grounded", "USD20kα"},
		{"-USD20k-4 [1]", "USD20k-4", "grounded", "USD20k-4"},
		{"USD20 [1]", "-USD20", "ungrounded", "USD20"},
		{"é −USD20 [1]", "-20", "grounded", "−USD20"},
		{"-20 [1]", "-USD20", "grounded", "-20"},
		{"-USD1,200 [1]", "-1200", "grounded", "-USD1,200"},
		{"-USD1.5 [1]", "-1.5", "grounded", "-USD1.5"},
		{"Balance - USD20\u00a0balance [1]", "-20", "grounded", "- USD20"},
		{"-USD1\u202f200 [1]", "-1200", "grounded", "-USD1\u202f200"},
		{"-USD1..2 [1]", "-1..2", "grounded", "-USD1..2"},
		{"-USD20A2 [1]", "USD20A2", "grounded", "USD20A2"},
		{"-USD20A2", "20A2", "ungrounded", "USD20A2"},
		{"-USD20", "USD20", "ungrounded", "-USD20"}, {"-USD 20", "-20", "grounded", "-USD 20"},
		{"-USD20-4 [1]", "USD20-4", "grounded", "USD20-4"},
		{"-USD20--A [1]", "USD20--A", "grounded", "USD20--A"},
		{"-ABC20 [1]", "ABC20", "grounded", "ABC20"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != tc.status || len(r.Claims) == 0 || len(r.Claims[0].Figures) != 1 {
				t.Fatalf("%+v", r)
			}
			f := r.Claims[0].Figures[0]
			if f.Text != tc.figure || f.Text != string([]rune(tc.answer)[f.Start:f.End]) {
				t.Fatalf("figure span: %+v", f)
			}
		})
	}
}

func TestCurrencyHorizontalWhitespacePreservesNumericSigns(t *testing.T) {
	answers := []string{"-$20", "-US$20", "-C$20", "-A$20", "-NZ$20", "-HK$20", "-SG$ 20", "-nz$20", "Balance - $20", "-$ 20"}
	for _, space := range []string{" ", "\t", "\u00a0", "\u202f"} {
		for _, currency := range []string{"$", "US$", "USD"} {
			answers = append(answers, "Balance -"+space+currency+space+"20")
		}
	}
	for _, answer := range answers {
		for _, tc := range []struct{ source, status string }{{"20", "ungrounded"}, {"$20", "ungrounded"}, {"-20", "grounded"}} {
			t.Run(answer+"/"+tc.source, func(t *testing.T) {
				if got := Check(answer, []Citation{{Index: 1, Text: tc.source}}); got.Status != tc.status {
					t.Fatalf("%+v", got)
				}
			})
		}
	}
}

func TestNumberBeforeEllipsis(t *testing.T) {
	for _, tc := range []struct{ answer, source, status, figure string }{
		{"The fee is 20...", "20", "grounded", "20"},
		{"-20kg... [1]", "20kg...", "ungrounded", "-20kg"},
		{"20kg... [1]", "-20kg...", "ungrounded", "20kg"},
		{"-20kg... [1]", "-20kg", "grounded", "-20kg"},
		{"-20kg [1]", "-20kg...", "grounded", "-20kg"},
		{"128GB... [1]", "128GB", "grounded", "128GB"},
		{"128GB [1]", "128GB...", "grounded", "128GB"},
		{"-20kg.. [1]", "20kg..", "ungrounded", "-20kg.."},
		{"20kg.. [1]", "-20kg..", "ungrounded", "20kg.."},
		{"-20kg.. [1]", "-20", "ungrounded", "-20kg.."},
		{"−20kg... [1]", "-20kg", "grounded", "−20kg"},
		{"-20kg.. [1]", "-20kg..", "grounded", "-20kg.."},
		{"-1..2kg... [1]", "1..2kg", "ungrounded", "-1..2kg"},
		{"-1..2kg... [1]", "-1..2kg", "grounded", "-1..2kg"},
		{"20", "The fee is 20...", "grounded", "20"},
		{"é -1,200... [1]", "-1200", "grounded", "-1,200"},
		{"1..2...", "12", "ungrounded", "1..2"},
		{"1..2...", "1..2", "grounded", "1..2"},
		{"1..2", "12", "ungrounded", "1..2"},
		{"K50...", "K5", "ungrounded", "K50"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != tc.status || len(r.Claims) == 0 || len(r.Claims[0].Figures) != 1 {
				t.Fatalf("%+v", r)
			}
			f := r.Claims[0].Figures[0]
			if f.Text != tc.figure || f.Text != string([]rune(tc.answer)[f.Start:f.End]) {
				t.Fatalf("figure span: %+v", f)
			}
		})
	}
}

func TestMalformedNumberNotGuessed(t *testing.T) {
	cases := []struct{ answer, source, verdict string }{
		{"1,2,3", "1,2,3", "grounded"}, {"1,2,3", "123", "ungrounded"},
		{"200", "1,,200", "ungrounded"}, {"1200", "1,200,,", "ungrounded"},
		{"1,234.50'0", "1,234.50'0", "grounded"}, {"1,234.50'0", "1,234.50'00", "ungrounded"},
	}
	for _, separator := range []string{"\u00a0", "\u202f", "'"} {
		cases = append(cases, struct{ answer, source, verdict string }{"200", "1" + separator + separator + "200", "ungrounded"})
	}
	for _, malformed := range []string{"1'\u202f234", "1'\u00a0234", "1\u00a0\u202f234"} {
		cases = append(cases,
			struct{ answer, source, verdict string }{malformed, "1234", "ungrounded"},
			struct{ answer, source, verdict string }{malformed, malformed, "grounded"})
	}
	for _, tc := range cases {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != tc.verdict || len(r.Claims[0].Figures) != 1 {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestEscapedFiguresRetainOriginalOffsets(t *testing.T) {
	r := Check(`a \$1\.5 b`, []Citation{{Index: 1, Text: "1.5"}})
	if len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 {
		t.Fatalf("%+v", r)
	}
	f := r.Claims[0].Figures[0]
	if f.Start != 4 || f.End != 8 || f.Text != `1\.5` || f.Status != "found" {
		t.Fatalf("%+v", f)
	}
}
func TestStandaloneMarkersAttachToPriorClaim(t *testing.T) {
	r := Check("20.\n[1]\nNext 30.", []Citation{{Index: 1, Text: "20"}, {Index: 2, Text: "30"}})
	if len(r.Claims) != 2 || len(r.Claims[0].Citations) != 1 || r.Claims[0].Citations[0] != 1 {
		t.Fatalf("%+v", r.Claims)
	}
}

func TestSourceBracketsAreEvidence(t *testing.T) {
	if r := Check("42", []Citation{{Index: 1, Text: "[42]"}}); r.Status != "grounded" {
		t.Fatalf("bracket evidence: %+v", r)
	}
	for _, source := range []string{"Measurement: [42]", "é **[42]**", "Value [42](https://example.test)"} {
		r := Check("The measurement was 42 [1].", []Citation{{Index: 1, Text: source}})
		if r.Status != "grounded" || len(r.Claims) != 1 || len(r.Claims[0].Citations) != 1 || r.Claims[0].Citations[0] != 1 || len(r.Claims[0].Figures) != 1 || r.Claims[0].Figures[0].Text != "42" {
			t.Fatalf("%q: %+v", source, r)
		}
	}
	r := Check("The measurement was 42 [1].", []Citation{{Index: 1, Text: "[1]"}, {Index: 2, Text: "[42]"}})
	if r.Status != "ungrounded" || len(r.Claims) != 1 || len(r.Claims[0].Citations) != 1 || r.Claims[0].Citations[0] != 1 || len(r.Claims[0].Figures) != 1 {
		t.Fatalf("answer marker scope: %+v", r)
	}
}

func TestMultilineBracketsAreNotMarkers(t *testing.T) {
	if m := markers("Cost 20 [1,\n9]."); len(m) != 0 {
		t.Fatalf("markers: %+v", m)
	}
}

func TestLeadingDecimalsKeepValueSignAndOffsets(t *testing.T) {
	for _, tc := range []struct{ answer, source, status string }{
		{"Rate .5 [1]", "5", "ungrounded"}, {"Rate .5 [1]", "0.5", "grounded"},
		{"Rate -.5 [1]", "0.5", "ungrounded"}, {"Rate -.5 [1]", "-0.5", "grounded"},
		{"Rate .5mg [1]", "0.5", "grounded"},
	} {
		r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
		if r.Status != tc.status || len(r.Claims[0].Figures) != 1 || r.Claims[0].Figures[0].Start != 5 || r.Claims[0].Figures[0].End != len([]rune(tc.answer))-4 {
			t.Fatalf("%q against %q: %+v", tc.answer, tc.source, r)
		}
	}
}

func TestUnicodeMinusRetainsNumericSignAndOffsets(t *testing.T) {
	for _, tc := range []struct{ source, status string }{
		{"The balance is 20.", "ungrounded"}, {"-20", "grounded"}, {"−20", "grounded"},
	} {
		r := Check("The balance is −20 [1].", []Citation{{Index: 1, Text: tc.source}})
		if r.Status != tc.status || len(r.Claims[0].Figures) != 1 {
			t.Fatalf("%q: %+v", tc.source, r)
		}
		f := r.Claims[0].Figures[0]
		if f.Start != 15 || f.End != 18 || f.Text != "−20" || f.Values[0] != "-20" {
			t.Fatalf("lost signed span: %+v", f)
		}
	}
}

func TestMarkdownBulletsDoNotNegateFigures(t *testing.T) {
	t.Run("normalized list rendition", func(t *testing.T) {
		source, err := document.RenditionMarkdownFromXHTML([]byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body><ul><li>20 invoices</li></ul></body></html>`), 16000)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ answer, status string }{{"20 invoices [1].", "grounded"}, {"-20 invoices [1].", "ungrounded"}} {
			if r := Check(tc.answer, []Citation{{Index: 1, Text: source}}); r.Status != tc.status {
				t.Fatalf("%q: %+v", tc.answer, r)
			}
		}
	})
	for _, tc := range []struct{ answer, source, status string }{{"20", "- 20", "grounded"}, {"-20", "- 20", "ungrounded"}, {"-20", "-20", "grounded"}} {
		if r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}}); r.Status != tc.status {
			t.Fatalf("%q/%q: %+v", tc.answer, tc.source, r)
		}
	}
	for _, bullet := range []string{"- ", "  -\t", "* ", "+ "} {
		for _, token := range []string{"20", "$20", "NZ$20"} {
			for _, tc := range []struct{ answer, source, status string }{
				{bullet + token + " invoices [1].", "20 invoices", "grounded"},
				{bullet + token + " invoices [1].", "-20 invoices", "ungrounded"},
				{"20 invoices [1].", bullet + token + " invoices", "grounded"},
				{"-20 invoices [1].", bullet + token + " invoices", "ungrounded"},
			} {
				t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
					if r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}}); r.Status != tc.status {
						t.Fatalf("%+v", r)
					}
				})
			}
		}
	}
	for _, answer := range []string{"-20", "−20", "Balance - 20", "Balance - $20", "Balance - USD 20", "- -20", "\\- 20"} {
		for _, tc := range []struct{ source, status string }{{"20", "ungrounded"}, {"-20", "grounded"}} {
			t.Run(answer+"/"+tc.source, func(t *testing.T) {
				if r := Check(answer, []Citation{{Index: 1, Text: tc.source}}); r.Status != tc.status {
					t.Fatalf("%+v", r)
				}
			})
		}
	}
}

func TestEscapedCitationLabelsRemainFigures(t *testing.T) {
	for _, slashes := range []string{`\`, `\\\`} {
		answer := "The displayed value is " + slashes + "[1] [1]."
		for _, tc := range []struct{ source, status string }{{"2", "ungrounded"}, {"1", "grounded"}} {
			t.Run(answer+"/"+tc.source, func(t *testing.T) {
				r := Check(answer, []Citation{{Index: 1, Text: tc.source}})
				if r.Status != tc.status || len(r.Claims[0].Figures) != 1 {
					t.Fatalf("%+v", r)
				}
				f := r.Claims[0].Figures[0]
				if f.Text != "1" || f.Start != 24+len(slashes) || f.End != 25+len(slashes) {
					t.Fatalf("bad displayed span: %+v", f)
				}
			})
		}
		if r := Check("1", []Citation{{Index: 1, Text: slashes + "[1]"}}); r.Status != "grounded" {
			t.Fatalf("escaped source label ignored: %+v", r)
		}
	}
	for _, answer := range []string{`The display is \\[1].`, `The display is \\\\[1].`} {
		r := Check(answer, []Citation{{Index: 1, Text: "2"}})
		if r.Status != "grounded" || len(r.Claims[0].Figures) != 0 || len(r.Claims[0].Citations) != 1 {
			t.Fatalf("even escape parity lost marker: %+v", r)
		}
	}
}

func TestLiteralCodeLabelsRemainFigures(t *testing.T) {
	t.Run("adjacent visible figures", func(t *testing.T) {
		for _, source := range []string{"K\\`5\\`", "K`5"} {
			if r := Check("K5 [1]", []Citation{{Index: 1, Text: source}}); r.Status != "ungrounded" {
				t.Errorf("literal backticks %q: %+v", source, r)
			}
		}
		for _, tc := range []struct{ answer, source, status, figure string }{
			{"Balance -`20` [1].", "20", "ungrounded", "-`20"},
			{"20 [1]", "Balance -`20`", "ungrounded", "20"},
			{"é K`5` [1]", "K5", "grounded", "K`5"},
			{"K`5` [1]", "K50", "ungrounded", "K`5"},
			{"1`2` [1]", "12", "grounded", "1`2"},
			{"K` 5 ` [1]", "K5", "grounded", "K` 5"},
			{"1` 2 ` [1]", "12", "grounded", "1` 2"},
			{"` 1 `2 [1]", "12", "grounded", "1 `2"},
			{"12 [1]", "1`2`", "grounded", "12"},
			{"-``20`` [1]", "-20", "grounded", "-``20"},
			{"K`` `5` `` [1]", "K5", "ungrounded", "5"},
		} {
			t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
				r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
				if r.Status != tc.status || len(r.Claims) == 0 || len(r.Claims[0].Figures) != 1 || !slices.Equal(r.Claims[0].Citations, []int{1}) {
					t.Fatalf("%+v", r)
				}
				f := r.Claims[0].Figures[0]
				if f.Text != tc.figure || f.Text != string([]rune(tc.answer)[f.Start:f.End]) {
					t.Fatalf("figure span: %+v", f)
				}
			})
		}
	})
	t.Run("backslashes remain literal", func(t *testing.T) {
		assertFigures := func(answer string, r Report, want []string, status string) {
			t.Helper()
			var figures []Figure
			for _, claim := range r.Claims {
				figures = append(figures, claim.Figures...)
			}
			if len(figures) != len(want) {
				t.Fatalf("%q: expected figures %v: %+v", answer, want, r)
			}
			for i, f := range figures {
				if f.Text != want[i] || f.Text != string([]rune(answer)[f.Start:f.End]) || status != "" && f.Status != status {
					t.Fatalf("%q: figure: %+v", answer, f)
				}
			}
		}
		for _, code := range []string{"`1\\.5`", "```\n1\\.5\n```", "    1\\.5"} {
			r := Check(code, []Citation{{Index: 1, Text: code}})
			if r.Status != "grounded" {
				t.Errorf("literal self-match %q: %+v", code, r)
			}
			assertFigures(code, r, []string{"1", ".5"}, "found")
			marked := code + " [1]"
			want, status, figureStatus := []string{"1", ".5"}, "ungrounded", "missing"
			if strings.HasPrefix(code, "```") {
				marked = code + "\n[1]"
				status, figureStatus = "incomplete", "unverifiable"
			} else if strings.HasPrefix(code, "    ") {
				want = append(want, "1")
			}
			for _, tc := range []struct {
				answer, source, status, figureStatus string
				want                                 []string
			}{{marked, "1.5", status, figureStatus, want}, {"1.5 [1]", code, "ungrounded", "missing", []string{"1.5"}}} {
				r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
				if r.Status != tc.status {
					t.Errorf("%q / %q: %+v", tc.answer, tc.source, r)
				}
				assertFigures(tc.answer, r, tc.want, tc.figureStatus)
			}
		}
		for _, tc := range []struct {
			answer, source, status string
			want                   []string
		}{
			{`1\.5 [1]`, "1.5", "grounded", []string{`1\.5`}}, {"1.5 [1]", `1\.5`, "grounded", []string{"1.5"}},
			{"1&#92;.5 [1]", "1.5", "ungrounded", []string{"1", ".5"}}, {"1.5 [1]", "1&#92;.5", "ungrounded", []string{"1.5"}},
		} {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != tc.status {
				t.Errorf("%q / %q: %+v", tc.answer, tc.source, r)
			}
			figureStatus := ""
			if tc.status == "grounded" {
				figureStatus = "found"
			}
			assertFigures(tc.answer, r, tc.want, figureStatus)
		}
	})
	t.Run("scope", func(t *testing.T) {
		r := Check("The literal value is `[2]` [1].", []Citation{{Index: 1, Text: "3"}, {Index: 2, Text: "2"}})
		if r.Status != "ungrounded" || len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 || len(r.Claims[0].Citations) != 1 || r.Claims[0].Citations[0] != 1 || r.Claims[0].Figures[0].Status != "missing" {
			t.Fatalf("literal code changed citation scope: %+v", r)
		}
	})
	for _, tc := range []struct {
		answer     string
		start, end int
	}{
		{"`[42]`", 2, 4}, {"```\n[42]\n```", 5, 7}, {"    [42]", 5, 7}, {"é literal `[42]` [1].", 12, 14},
	} {
		t.Run("source/"+tc.answer, func(t *testing.T) {
			if got := Check("42", []Citation{{Index: 1, Text: tc.answer}}); got.Status != "grounded" || len(got.Claims) != 1 || len(got.Claims[0].Figures) != 1 || got.Claims[0].Figures[0].Text != "42" || got.Claims[0].Figures[0].Status != "found" {
				t.Fatalf("literal source evidence lost: %+v", got)
			}
		})
		t.Run(tc.answer, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: "3"}})
			var figures []Figure
			for _, claim := range r.Claims {
				if len(claim.UnknownMarkers) != 0 {
					t.Fatalf("literal became unknown marker: %+v", r)
				}
				figures = append(figures, claim.Figures...)
			}
			if r.Status != "ungrounded" || len(figures) != 1 {
				t.Fatalf("literal figure disappeared: %+v", r)
			}
			f := figures[0]
			if f.Values[0] != "42" || f.Text != "42" || f.Start != tc.start || f.End != tc.end || f.Text != string([]rune(tc.answer)[f.Start:f.End]) {
				t.Fatalf("bad literal span: %+v", f)
			}
		})
	}
}

func TestFenceInfoIsNeitherFigureNorMarker(t *testing.T) {
	if r := Check("123", []Citation{{Index: 1, Text: "```123\nno numeric value\n```"}}); r.Status != "ungrounded" {
		t.Fatalf("fence metadata: %+v", r)
	}
	t.Run("normalized rendition", func(t *testing.T) {
		source, err := document.RenditionMarkdownFromXHTML([]byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body><pre><code class="language-123">42</code></pre></body></html>`), 16000)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(source, "```123\n42\n```") {
			t.Fatalf("missing normalized metadata: %q", source)
		}
		if r := Check("123", []Citation{{Index: 1, Text: source}}); r.Status != "ungrounded" {
			t.Fatalf("metadata became evidence: %+v", r)
		}
		if r := Check("42", []Citation{{Index: 1, Text: source}}); r.Status != "grounded" || len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 || r.Claims[0].Figures[0].Text != "42" || r.Claims[0].Figures[0].Status != "found" {
			t.Fatalf("body lost: %+v", r)
		}
	})
	for _, info := range []string{"python3", "123", "[2]"} {
		source := "é\n```" + info + "\n42\n```"
		if r := Check("42", []Citation{{Index: 1, Text: source}}); r.Status != "grounded" || len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 || r.Claims[0].Figures[0].Text != "42" || r.Claims[0].Figures[0].Status != "found" {
			t.Fatalf("body lost: %+v", r)
		}
		if r := Check(info, []Citation{{Index: 1, Text: source}}); info != "[2]" && r.Status != "ungrounded" {
			t.Fatalf("metadata became evidence: %+v", r)
		}
		r := Check(source, []Citation{{Index: 1, Text: "42"}})
		var figures []Figure
		for _, c := range r.Claims {
			if len(c.UnknownMarkers) != 0 || !slices.Equal(c.Citations, []int{1}) {
				t.Fatalf("metadata became marker: %+v", r)
			}
			figures = append(figures, c.Figures...)
		}
		if r.Status != "grounded" || len(figures) != 1 || figures[0].Text != "42" || figures[0].Start != len([]rune("é\n```"+info+"\n")) || figures[0].Text != string([]rune(source)[figures[0].Start:figures[0].End]) {
			t.Fatalf("metadata or body figure: %+v", r)
		}
	}
}

func TestInlineCodePunctuationKeepsCitationScope(t *testing.T) {
	for _, answer := range []string{"`Fee 20. Due` [1]", "é `Fee 20! Due` [1]", "é ``Fee 20? Due`` [1]"} {
		r := Check(answer, []Citation{{Index: 1, Text: "10"}, {Index: 2, Text: "20"}})
		if r.Status != "ungrounded" || len(r.Claims) != 1 || len(r.Claims[0].Citations) != 1 || r.Claims[0].Citations[0] != 1 || len(r.Claims[0].Figures) != 1 {
			t.Fatalf("%q: %+v", answer, r)
		}
		f := r.Claims[0].Figures[0]
		if f.Text != "20" || f.Text != string([]rune(answer)[f.Start:f.End]) || f.Status != "missing" {
			t.Fatalf("figure: %+v", f)
		}
		if got := Check("20", []Citation{{Index: 1, Text: answer}}); got.Status != "grounded" {
			t.Fatalf("source figure lost: %+v", got)
		}
	}
}

func TestEmphasisPreservesFigureAndClaimScope(t *testing.T) {
	for _, tc := range []struct{ answer, source, status string }{
		{"Balance -**20** [1].", "20", "ungrounded"}, {"Balance -**20** [1].", "-20", "grounded"},
		{"-20", "Balance -**20**", "grounded"}, {"20", "Balance -**20**", "ungrounded"},
		{"K**5**", "K5", "grounded"}, {"K**5**", "5", "ungrounded"},
		{"é ***20***", "20", "grounded"}, {"**20***", "20", "grounded"},
		{"\\*20\\*", "20", "grounded"}, {"*20", "20", "grounded"},
		{"`**20**`", "20", "grounded"}, {"```\n**20**\n```", "20", "grounded"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != tc.status {
				t.Fatalf("%+v", r)
			}
			for _, c := range r.Claims {
				for _, f := range c.Figures {
					if f.Text != string([]rune(tc.answer)[f.Start:f.End]) {
						t.Fatalf("span: %+v", f)
					}
				}
			}
		})
	}
	for _, tc := range []struct {
		answer, source, text string
		start, end           int
	}{
		{"é K**5**", "K5", "K**5", 2, 6},
		{"Balance -**20**", "-20", "-**20", 8, 13},
	} {
		report := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
		if len(report.Claims) != 1 || len(report.Claims[0].Figures) != 1 {
			t.Fatalf("figure: %+v", report)
		}
		f := report.Claims[0].Figures[0]
		if f.Start != tc.start || f.End != tc.end || f.Text != tc.text {
			t.Fatalf("figure offset: %+v", f)
		}
	}
	parsed := analyzeMarkdown("**20***")
	projected := parsed.project(parsed.structural)
	if string(projected.runes) != "20*" || len(projected.positions) != 3 || projected.positions[2] != 6 {
		t.Fatalf("partial projection: %+v", projected)
	}
	partial := Check("**20***", []Citation{{Index: 1, Text: "20"}})
	if len(partial.Claims) != 1 || partial.Claims[0].Start != 0 || partial.Claims[0].End != 7 {
		t.Fatalf("partial span: %+v", partial)
	}
}

func TestWholeDigitIdentityAcrossKinds(t *testing.T) {
	for _, tc := range []struct{ answer, source, status string }{
		{"123-45-6789", "123456789", "grounded"}, {"123456789", "123-45-6789", "grounded"},
		{"001-23-456", "00123456", "grounded"}, {"001-23-456", "123456", "ungrounded"},
		{"１２３-４５-６７８９", "123456789", "grounded"}, {`123\-45\-6789`, "123456789", "grounded"}, {"123-45-6789", "1234567890", "ungrounded"},
		{"123-456", "-123456", "ungrounded"}, {"-123456", "123-456", "ungrounded"}, {"123-456", "+123456", "ungrounded"}, {"+123456", "123-456", "ungrounded"},
		{"123-456", "123456.0", "ungrounded"}, {"123-456", "123,456", "ungrounded"},
		{"123-456", "123456GB", "ungrounded"}, {"123-456", "$123456", "ungrounded"}, {"123-456", "USD 123456", "ungrounded"}, {"123-456", "123456$", "ungrounded"}, {"123-456", "123456 USD", "ungrounded"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != tc.status {
				t.Fatalf("%+v", r)
			}
			if tc.status == "grounded" && (len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 || r.Claims[0].Figures[0].Text != tc.answer || r.Claims[0].Figures[0].Status != "found") {
				t.Fatalf("expected found figure %q: %+v", tc.answer, r)
			}
		})
	}
}

func TestSourceOrderedOrdinalsArePossibleEvidence(t *testing.T) {
	if r := Check("2024", []Citation{{Index: 1, Text: "2023. Revenue increased"}}); r.Status != "ungrounded" {
		t.Fatalf("negative source: %+v", r)
	}
	r := Check("2024. Item [1]", []Citation{{Index: 1, Text: "other"}})
	if r.Status != "grounded" {
		t.Fatalf("answer ordinal: %+v", r)
	}
	for _, c := range r.Claims {
		if len(c.Figures) != 0 {
			t.Fatalf("answer ordinal became figure: %+v", r)
		}
	}
	for _, source := range []string{"2024. Revenue increased", "2024. Item", "2024) Item"} {
		if r := Check("2024", []Citation{{Index: 1, Text: source}}); r.Status != "grounded" {
			t.Fatalf("%q: %+v", source, r)
		}
	}
}

func TestMarkedAndUnmarkedScopePrecedence(t *testing.T) {
	t.Run("abbreviation", func(t *testing.T) {
		r := Check("The fee was 1250, per Dr. Smith [1]. Another fee was 1250 [2].", []Citation{{Index: 1, Text: "1205"}, {Index: 2, Text: "1250"}})
		if r.Status != "incomplete" || len(r.Claims) < 2 || len(r.Claims[0].Figures) != 1 || r.Claims[0].Figures[0].Status != "unverifiable" || r.Claims[0].Verdict != "unverifiable" {
			t.Fatalf("%+v", r)
		}
	})
	for _, tc := range []struct{ answer, status, first string }{
		{"Fee 20. Rate 30.", "grounded", "grounded"},
		{"Fee 20. Rate 30 [1].", "incomplete", "unverifiable"},
		{"Fee 20. Rate 30 [9].", "ungrounded", "unverifiable"},
		{"Fee 20 [1]. Rate 30 [2].", "grounded", "grounded"},
		{"Fee 99 [1]. Rate 30.", "ungrounded", "ungrounded"},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: "20 and 30"}, {Index: 2, Text: "30"}})
			if r.Status != tc.status || len(r.Claims) < 2 || r.Claims[0].Verdict != tc.first {
				t.Fatalf("%+v", r)
			}
		})
	}
	r := Check("Intro. Fee 20 [1].", []Citation{{Index: 1, Text: "20"}})
	if r.Status != "grounded" || len(r.Claims) != 2 || r.Claims[0].Verdict != "no_figures" {
		t.Fatalf("figure-free: %+v", r)
	}
}

func TestDigitIdentityKeepsPublicFigureAndOffsets(t *testing.T) {
	r := Check("é **123-45-6789**", []Citation{{Index: 1, Text: "123456789"}})
	if r.Status != "grounded" || len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 {
		t.Fatalf("%+v", r)
	}
	f := r.Claims[0].Figures[0]
	if f.Kind != "identifier" || f.Start != 4 || f.End != 15 || f.Text != "123-45-6789" || len(f.Values) != 1 || f.Values[0] != "123456789" {
		t.Fatalf("%+v", f)
	}
	b, err := json.Marshal(f)
	if err != nil || strings.Contains(string(b), "identity") {
		t.Fatalf("private identity leaked: %s %v", b, err)
	}
}

func TestHiddenHTMLDoesNotChangeFigureOrMarkerScope(t *testing.T) {
	citations := []Citation{{Index: 1, Text: "30"}, {Index: 2, Text: "20"}}
	for _, answer := range []string{"Fee 20 <!-- [2] --> [1].", `Fee 20 <span data-id="[2]"> [1].`} {
		r := Check(answer, citations)
		if r.Status != "ungrounded" || len(r.Claims) != 1 || !slices.Equal(r.Claims[0].Citations, []int{1}) {
			t.Errorf("%q: %+v", answer, r)
		}
	}
	for _, source := range []string{"<!-- 20 -->", "<!-- hidden\n20 [2] -->\n", "<div>20 [2]</div>\n"} {
		if r := Check("20 [1]", []Citation{{Index: 1, Text: source}}); r.Status != "ungrounded" {
			t.Errorf("hidden source %q: %+v", source, r)
		}
	}
	for _, source := range []string{"12<!-- 9 -->34", "12<span>34</span>"} {
		if r := Check("1234 [1]", []Citation{{Index: 1, Text: source}}); r.Status != "grounded" {
			t.Errorf("visible adjacency %q: %+v", source, r)
		}
	}
	answer := "é 12<!-- 9 -->34 [1]."
	r := Check(answer, []Citation{{Index: 1, Text: "1234"}})
	if r.Status != "grounded" || len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 {
		t.Fatalf("answer adjacency: %+v", r)
	}
	f := r.Claims[0].Figures[0]
	if f.Text != "12<!-- 9 -->34" || f.Text != string([]rune(answer)[f.Start:f.End]) {
		t.Fatalf("original figure span: %+v", f)
	}
}

func TestClaimsReportCitationScope(t *testing.T) {
	citations := []Citation{{Index: 1, Text: "20"}, {Index: 2, Text: "20"}, {Index: 3, Err: "citation_unavailable"}}
	for _, tc := range []struct {
		answer string
		scopes [][]int
	}{
		{"Fee 20. Intro.", [][]int{{1, 2, 3}, {1, 2, 3}}},
		{"Fee 20. Rate 20 [2].", [][]int{{}, {2}}},
		{"Fee 20 [9].", [][]int{{}}},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			r := Check(tc.answer, citations)
			if len(r.Claims) != len(tc.scopes) {
				t.Fatalf("claims: %+v", r.Claims)
			}
			for i, want := range tc.scopes {
				if got := r.Claims[i].Citations; !slices.Equal(got, want) {
					t.Errorf("claim %d scope = %v, want %v", i, got, want)
				}
			}
		})
	}
}

func TestListPrefixes(t *testing.T) {
	t.Run("empty and line endings", func(t *testing.T) {
		// Pinned Goldmark needs a space after an empty CRLF list marker; otherwise its ordinal is displayed text.
		for _, tc := range []struct {
			answer  string
			figures int
		}{
			{"2.", 0}, {"2)", 0}, {"2) Nothing numeric here", 0}, {"Text.\n2.\n", 1}, {"Text.\n\n2.\n", 0},
			{"  2.\r\n", 1}, {"  2. \r\n", 0}, {"Text\r2.", 1}, {"Text\r\r2.", 0}, {"Text.\r\n2.\r\n", 1},
		} {
			r := Check(tc.answer, []Citation{{Index: 1, Text: "Text"}})
			count := 0
			for _, claim := range r.Claims {
				count += len(claim.Figures)
			}
			if count != tc.figures {
				t.Fatalf("%q: got %d figures: %+v", tc.answer, count, r)
			}
		}
	})
	t.Run("indented", func(t *testing.T) {
		r := Check("  1. Fee 20.", []Citation{{Index: 1, Text: "20"}})
		if len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 {
			t.Fatalf("%+v", r.Claims)
		}
	})
	t.Run("tabs preserve figures", func(t *testing.T) {
		for _, tc := range []struct {
			answer, figure string
			start, end     int
		}{{"*\t  0", "0", 4, 5}, {"*\t  12345", "12345", 4, 9}} {
			t.Run(tc.figure, func(t *testing.T) {
				if err := Preflight(tc.answer); err != nil {
					t.Fatal(err)
				}
				r := Check(tc.answer, []Citation{{Index: 1, Text: tc.figure}})
				if r.Status != "grounded" || len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 {
					t.Fatalf("answer: %+v", r)
				}
				f := r.Claims[0].Figures[0]
				if f.Text != tc.figure || len(f.Values) != 1 || f.Values[0] != tc.figure || f.Start != tc.start || f.End != tc.end {
					t.Fatalf("figure: %+v", f)
				}
				if r := Check(tc.figure, []Citation{{Index: 1, Text: tc.answer}}); r.Status != "grounded" {
					t.Fatalf("source: %+v", r)
				}
			})
		}
	})
	t.Run("multiline content", func(t *testing.T) {
		if err := Preflight("*\r\t#"); err != nil {
			t.Fatal(err)
		}
		for _, answer := range []string{"*\r\t# 12345", "*\r\t1. 12345"} {
			r := Check(answer, []Citation{{Index: 1, Text: "12345"}})
			if r.Status != "grounded" {
				t.Fatalf("answer %q: %+v", answer, r)
			}
			count := 0
			for _, claim := range r.Claims {
				for _, f := range claim.Figures {
					count++
					if f.Text != "12345" || f.Text != string([]rune(answer)[f.Start:f.End]) {
						t.Fatalf("figure: %+v", f)
					}
				}
			}
			if count != 1 {
				t.Fatalf("figures: %+v", r)
			}
			if r := Check("12345", []Citation{{Index: 1, Text: answer}}); r.Status != "grounded" {
				t.Fatalf("source: %+v", r)
			}
		}
	})
}

func TestLinkLabelsRemainFigures(t *testing.T) {
	t.Run("adjacent visible figures", func(t *testing.T) {
		for _, tc := range []struct{ answer, source, status, figure string }{
			{"Balance -[20](https://example.test) [1].", "20", "ungrounded", "-[20"},
			{"20 [1]", "Balance -[20](https://example.test)", "ungrounded", "20"},
			{"é K[5](url) [1]", "K5", "grounded", "K[5"},
			{"K[5](url) [1]", "K50", "ungrounded", "K[5"},
			{"1[2](url) [1]", "12", "grounded", "1[2"},
			{"12 [1]", "1[2](url)", "grounded", "12"},
			{"-[20][fee] [1]\n\n[fee]: url", "-20", "grounded", "-[20"},
			{"-[20][] [1]\n\n[20]: url", "-20", "grounded", "-[20"},
			{"-[20] [1]\n\n[20]: url", "-20", "grounded", "-[20"},
			{"-![20](url) [1]", "20", "ungrounded", "-![20"},
		} {
			t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
				r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
				if r.Status != tc.status || len(r.Claims) == 0 || len(r.Claims[0].Figures) != 1 || !slices.Equal(r.Claims[0].Citations, []int{1}) {
					t.Fatalf("%+v", r)
				}
				f := r.Claims[0].Figures[0]
				if f.Text != tc.figure || f.Text != string([]rune(tc.answer)[f.Start:f.End]) {
					t.Fatalf("figure span: %+v", f)
				}
			})
		}
	})
	t.Run("inline", func(t *testing.T) {
		for _, tc := range []struct {
			answer    string
			citations []Citation
			status    string
			values    []string
		}{
			{"Value [30](url).", []Citation{{Index: 1, Text: "130"}}, "ungrounded", []string{"30"}},
			{"The fee was 30 [details [1]](https://example.test/fee).", []Citation{{Index: 1, Text: "1"}, {Index: 2, Text: "30"}}, "grounded", []string{"30", "1"}},
		} {
			t.Run(tc.answer, func(t *testing.T) {
				r := Check(tc.answer, tc.citations)
				if r.Status != tc.status || len(r.Claims) != 1 || len(r.Claims[0].Figures) != len(tc.values) || len(r.Claims[0].Citations) != len(tc.citations) || len(r.Claims[0].UnknownMarkers) != 0 {
					t.Fatalf("%+v", r)
				}
				for i, f := range r.Claims[0].Figures {
					if f.Values[0] != tc.values[i] || f.Text != string([]rune(tc.answer)[f.Start:f.End]) {
						t.Fatalf("bad displayed figure: %+v", f)
					}
				}
				for _, value := range tc.values {
					if got := Check(value, []Citation{{Index: 1, Text: tc.answer}}); got.Status != "grounded" {
						t.Fatalf("source label lost figure %s: %+v", value, got)
					}
				}
			})
		}
	})
	t.Run("reference", func(t *testing.T) {
		for _, answer := range []string{
			"Value [30][fee].\n\n[fee]: https://example.test/fee",
			"Value [30][].\n\n[30]: https://example.test/fee",
			"Value [30].\n\n[30]: https://example.test/fee",
			"é Value [30][1].\n\n[1]: https://example.test/fee",
		} {
			for _, tc := range []struct{ source, status string }{{"130", "ungrounded"}, {"30", "grounded"}} {
				t.Run(answer+"/"+tc.source, func(t *testing.T) {
					r := Check(answer, []Citation{{Index: 1, Text: tc.source}})
					count := 0
					for _, c := range r.Claims {
						count += len(c.Figures)
					}
					if r.Status != tc.status || count != 1 || len(r.Claims[0].UnknownMarkers) != 0 || !slices.Equal(r.Claims[0].Citations, []int{1}) || len(r.Claims[0].Figures) != 1 {
						t.Fatalf("%+v", r)
					}
					f := r.Claims[0].Figures[0]
					if f.Values[0] != "30" || f.Text != "30" || f.Text != string([]rune(answer)[f.Start:f.End]) {
						t.Fatalf("bad displayed span: %+v", f)
					}
					if strings.HasPrefix(answer, "é") && (f.Start != 9 || f.End != 11) {
						t.Fatalf("lost Unicode offset: %+v", f)
					}
				})
			}
			if r := Check("30", []Citation{{Index: 1, Text: answer}}); r.Status != "grounded" {
				t.Fatalf("displayed source label lost: %+v", r)
			}
			if r := Check("1", []Citation{{Index: 1, Text: answer}}); r.Status != "ungrounded" {
				t.Fatalf("reference metadata became evidence: %+v", r)
			}
		}
	})
}

func TestSentenceMarkerScope(t *testing.T) {
	t.Run("closing quotes", func(t *testing.T) {
		for _, tc := range []struct{ answer, first, second string }{
			{`"Fee 20." [1] Rate 30 [2].`, `"Fee 20." [1]`, `Rate 30 [2].`},
			{"é «‘Fee 20.’»[1] Rate 30 [2].", "é «‘Fee 20.’»[1]", "Rate 30 [2]."},
			{"‘Fee 20!’ [1] Rate 30 [2].", "‘Fee 20!’ [1]", "Rate 30 [2]."},
		} {
			t.Run(tc.answer, func(t *testing.T) {
				r := Check(tc.answer, []Citation{{Index: 1, Text: "30"}, {Index: 2, Text: "20"}})
				if r.Status != "ungrounded" || len(r.Claims) != 2 {
					t.Fatalf("swapped citations: %+v", r)
				}
				for i, want := range []string{tc.first, tc.second} {
					c := r.Claims[i]
					if c.Verdict != "ungrounded" || !slices.Equal(c.Citations, []int{i + 1}) || string([]rune(tc.answer)[c.Start:c.End]) != want {
						t.Fatalf("claim %d: %+v", i, c)
					}
				}
			})
		}
	})
	t.Run("closing emphasis", func(t *testing.T) {
		answer := "**Fee 20.** [1] Rate 30 [2]."
		r := Check(answer, []Citation{{Index: 1, Text: "30"}, {Index: 2, Text: "20"}})
		if r.Status != "ungrounded" || len(r.Claims) != 2 || len(r.Claims[0].Citations) != 1 || r.Claims[0].Citations[0] != 1 || len(r.Claims[1].Citations) != 1 || r.Claims[1].Citations[0] != 2 || r.Claims[0].Verdict != "ungrounded" || r.Claims[1].Verdict != "ungrounded" {
			t.Fatalf("scope: %+v", r)
		}
	})
	t.Run("immediate punctuation", func(t *testing.T) {
		for _, answer := range []string{"Fee 20.[1] Rate 30.[2]", "Fee 20![1] Rate 30?[2]"} {
			r := Check(answer, []Citation{{Index: 1, Text: "30"}, {Index: 2, Text: "20"}})
			if r.Status != "ungrounded" || len(r.Claims) != 2 || len(r.Claims[0].Citations) != 1 || len(r.Claims[1].Citations) != 1 {
				t.Fatalf("%q: %+v", answer, r)
			}
		}
	})
	t.Run("formatted markers", func(t *testing.T) {
		for _, answer := range []string{"é Fee 30. **[1]** Rate 20 [2].", "é Fee 30. _[1]_ Rate 20 [2].", "é Fee 30. ***[1]*** Rate 20 [2].", "é Fee 30. ___[1]___ Rate 20 [2].", "é Fee 30. **_[1]_** Rate 20 [2].", "é Fee 30.**[1]** Rate 20 [2]."} {
			r := Check(answer, []Citation{{Index: 1, Text: "20"}, {Index: 2, Text: "30"}})
			if len(r.Claims) != 2 || r.Claims[0].Verdict != "ungrounded" || len(r.Claims[0].Citations) != 1 || r.Claims[0].Citations[0] != 1 || len(r.Claims[1].Citations) != 1 || r.Claims[1].Citations[0] != 2 {
				t.Fatalf("%q: %+v", answer, r)
			}
			f := r.Claims[0].Figures[0]
			if f.Text != "30" || f.Start != 6 || f.End != 8 || f.Text != string([]rune(answer)[f.Start:f.End]) {
				t.Fatalf("offsets: %+v", f)
			}
		}
	})
}

func TestMarkdownReferenceProjection(t *testing.T) {
	for _, tc := range []struct{ input, visible string }{
		{"&#49;", "1"}, {"&#x31;", "1"}, {"&#X000031;", "1"}, {"&#0000049;", "1"},
		{"&#00000049;", "&#00000049;"}, {"&#x0000031;", "&#x0000031;"},
		{"&#x110000;", "�"}, {"&#0;", "�"}, {"&minus;", "−"}, {"&notit;", "&notit;"},
		{"&fjlig;", "fj"}, {"&amp;#49;", "&#49;"}, {"&#49", "&#49"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			m := analyzeMarkdown(tc.input)
			p := m.project(m.structural)
			if string(p.runes) != tc.visible {
				t.Fatalf("visible %q, want %q", string(p.runes), tc.visible)
			}
			if tc.input == "&fjlig;" && (len(p.positions) != 2 || p.positions[0] != 0 || p.positions[1] != 0 || p.ends[0] != 7 || p.ends[1] != 7) {
				t.Fatalf("multi-rune mapping: %+v", p)
			}
		})
	}
	for _, tc := range []struct{ answer, source, status, value string }{
		{"é &#49;205 [1]", "1205", "grounded", "1205"}, {"1205", "&#x31;205", "grounded", "1205"},
		{"&minus;20", "-20", "grounded", "-20"}, {"-20", "&minus;20", "grounded", "-20"},
		{"&fjlig;5", "fj5", "grounded", "fj5"}, {"&lbrack;1&rbrack;", "1", "grounded", "1"},
		{`\&#49;`, "49", "grounded", "49"}, {`\\&#49;`, "1", "grounded", "1"},
		{"`&#49;`", "49", "grounded", "49"}, {"```\n&#49;\n```", "49", "grounded", "49"}, {"    &#49;", "49", "grounded", "49"},
		{"49", "`&#49;`", "grounded", "49"}, {"1", "`&#49;`", "ungrounded", "1"}, {"49", "```\n&#49;\n```", "grounded", "49"}, {"49", "    &#49;", "grounded", "49"},
		{"Invoice &#49;205 [1].", "Invoice 49 and 205", "ungrounded", "1205"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != tc.status {
				t.Fatalf("%+v", r)
			}
			matched := false
			for _, c := range r.Claims {
				for _, f := range c.Figures {
					if f.Text != string([]rune(tc.answer)[f.Start:f.End]) {
						t.Fatalf("span: %+v", f)
					}
					if slices.Contains(f.Values, tc.value) && (tc.status != "grounded" || f.Status == "found") {
						matched = true
					}
				}
			}
			if !matched {
				t.Fatalf("expected figure %q: %+v", tc.value, r)
			}
		})
	}
	multiple := Check("&fjlig;5 20", []Citation{{Index: 1, Text: "fj5 20"}})
	if len(multiple.Claims) != 1 || len(multiple.Claims[0].Figures) != 2 {
		t.Fatalf("multi-reference figures: %+v", multiple)
	}
	first, second := multiple.Claims[0].Figures[0], multiple.Claims[0].Figures[1]
	if first.Start != 0 || first.End != 8 || first.Text != "&fjlig;5" || second.Start != 9 || second.End != 11 || first.End > second.Start {
		t.Fatalf("overlapping entity spans: %+v", multiple)
	}
	if got := markers("&lbrack;1&rbrack;"); len(got) != 0 {
		t.Fatalf("decoded marker: %+v", got)
	}
	answer := "é &#49;205 [1]"
	r := Check(answer, []Citation{{Index: 1, Text: "1205"}})
	if len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 {
		t.Fatalf("%+v", r)
	}
	f := r.Claims[0].Figures[0]
	if f.Start != 2 || f.End != 10 || f.Text != "&#49;205" || f.Values[0] != "1205" {
		t.Fatalf("offset: %+v", f)
	}
	answer = "Fee 20&#46; [1] Rate 30 [2]."
	r = Check(answer, []Citation{{Index: 1, Text: "30"}, {Index: 2, Text: "20"}})
	if r.Status != "ungrounded" || len(r.Claims) != 2 || string([]rune(answer)[r.Claims[0].Start:r.Claims[0].End]) != "Fee 20&#46; [1]" || len(r.Claims[0].Citations) != 1 || r.Claims[0].Citations[0] != 1 {
		t.Fatalf("decoded punctuation: %+v", r)
	}
}

func TestUnicodeWordTokensRemainWhole(t *testing.T) {
	for _, tc := range []struct{ answer, source, status string }{
		{"α42", "β42", "ungrounded"}, {"β42", "α42", "ungrounded"}, {"α42", "42", "ungrounded"},
		{"α42", "Α42", "grounded"}, {"ΟΣ42", "ος42", "grounded"}, {"42β", "42", "ungrounded"}, {"42\u0301", "42", "ungrounded"},
		{"٤42", "42", "ungrounded"}, {"42٤", "42", "ungrounded"}, {"A٤42", "A42", "ungrounded"},
		{"α42β", "α42", "ungrounded"}, {"é42", "e\u030142", "ungrounded"},
		{"42²", "42", "ungrounded"}, {"A٤42", "a٤42", "grounded"}, {"ß42", "ß42", "grounded"},
		{"42mg", "42", "grounded"},
	} {
		t.Run(tc.answer+"/"+tc.source, func(t *testing.T) {
			r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
			if r.Status != tc.status {
				t.Fatalf("%+v", r)
			}
			if tc.status == "grounded" {
				want := map[string]string{"α42": "α42", "ΟΣ42": "οσ42", "A٤42": "a٤42", "ß42": "ß42", "42mg": "42"}[tc.answer]
				if len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 || r.Claims[0].Figures[0].Status != "found" || !slices.Contains(r.Claims[0].Figures[0].Values, want) {
					t.Fatalf("expected found figure %q: %+v", want, r)
				}
			}
		})
	}

	answer := "é α42 [1]."
	r := Check(answer, []Citation{{Index: 1, Text: "Α42"}})
	if r.Status != "grounded" || len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 {
		t.Fatalf("%+v", r)
	}
	f := r.Claims[0].Figures[0]
	if f.Kind != "identifier" || f.Start != 2 || f.End != 5 || f.Text != "α42" || f.Values[0] != "α42" {
		t.Fatalf("%+v", f)
	}
	r = Check("٤٥", []Citation{{Index: 1, Text: "45"}})
	if len(r.Claims) != 1 || len(r.Claims[0].Figures) != 0 {
		t.Fatalf("native digits became numbers: %+v", r)
	}
}

func TestBareSpacedLettersAndNumbersAreNotModels(t *testing.T) {
	for _, tc := range []struct{ answer, source string }{{"IN 24", "in 24"}, {"in 24", "IN 24"}, {"IN 24", "ON 24"}} {
		r := Check(tc.answer, []Citation{{Index: 1, Text: tc.source}})
		if r.Status != "grounded" || len(r.Claims) != 1 || len(r.Claims[0].Figures) != 1 || r.Claims[0].Figures[0].Kind != "number" || r.Claims[0].Figures[0].Text != "24" {
			t.Fatalf("%+v", r)
		}
	}
}
