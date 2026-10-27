package grounding

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzExtract(f *testing.F) {
	seeds := []string{"INV-2026-0042 1,200", `\$20`, "１２.５", "x [1]"}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			t.Skip()
		}
		a := extract(s)
		if slices.Contains(seeds, s) && len(a) == 0 {
			t.Fatalf("seed %q lost its figures", s)
		}
		if !reflect.DeepEqual(a, extract(s)) {
			t.Fatal("nondeterministic")
		}
		end := 0
		for _, v := range a {
			if v.Start < end || v.End <= v.Start || v.End > utf8.RuneCountInString(s) {
				t.Fatalf("bad span %+v", v)
			}
			end = v.End
		}
	})
}
func FuzzSelfGrounded(f *testing.F) {
	figureSeeds := []string{"42 K5", "[1] 20", "2. [1] Next 30", "`[42]`", "```\n[42]\n```", "    [42]"}
	seeds := append(append([]string(nil), figureSeeds...), strings.Repeat("[1", 8000), strings.Repeat("1.", 8000), strings.Repeat("A ", 8000), strings.Repeat("-", 15999)+"1")
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) || Preflight(s) != nil {
			t.Skip()
		} // Unknown markers do not have a valid scope.
		ms := markers(s)
		for _, m := range ms {
			for _, n := range m.indexes {
				if n != 1 {
					t.Skip()
				}
			}
		}
		r := Check(s, []Citation{{Index: 1, Text: s}})
		count := 0
		for _, c := range r.Claims {
			count += len(c.Figures)
			for _, v := range c.Figures {
				want := "found"
				if len(ms) > 0 && len(c.Citations) == 0 {
					want = "unverifiable"
				}
				if v.Status != want {
					t.Fatalf("%q: want %s: %+v", s, want, v)
				}
			}
		}
		if slices.Contains(figureSeeds, s) && count == 0 {
			t.Fatalf("seed %q lost its figures", s)
		}
	})
}
func FuzzSourceMonotone(f *testing.F) {
	f.Add("30", "30", "130")
	f.Add("K5", "K5", "anything")
	f.Add("42", "`[42]`", "0")
	f.Fuzz(func(t *testing.T, answer, source, extra string) {
		if !utf8.ValidString(answer) || !utf8.ValidString(source) || !utf8.ValidString(extra) || utf8.RuneCountInString(source) < 1 || utf8.RuneCountInString(source) > 16000 || utf8.RuneCountInString(extra) < 1 || utf8.RuneCountInString(extra) > 16000 || Preflight(answer) != nil || len(markers(answer)) > 0 {
			t.Skip()
		}
		a := Check(answer, []Citation{{Index: 1, Text: source}})
		b := Check(answer, []Citation{{Index: 1, Text: source}, {Index: 2, Text: extra}})
		found := false
		for i, c := range a.Claims {
			for j, v := range c.Figures {
				if v.Status == "found" {
					found = true
				}
				if v.Status == "found" && b.Claims[i].Figures[j].Status != "found" {
					t.Fatalf("found lost for %s", strings.TrimSpace(answer))
				}
			}
		}
		if (answer == "30" && source == "30" && extra == "130" || answer == "K5" && source == "K5" && extra == "anything" || answer == "42" && source == "`[42]`" && extra == "0") && !found {
			t.Fatalf("seed %q lost its found figure", answer)
		}
	})
}
