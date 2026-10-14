package store

import (
	"errors"
	"strings"
	"testing"
)

// These cases catch permissive digit/initial edits, excessive edit budgets,
// and a matcher that greedily spends an edit on an ambiguous name word.
func TestFuzzyWordEdits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query, name string
		match       bool
		edits       int
	}{
		{"invocie", "invoice.pdf", true, 1}, {"invoce", "invoice", true, 1},
		{"invoicce", "invoice", true, 1}, {"invoixe", "invoice", true, 1},
		{"xnvoice", "invoice", false, 0}, {"invxixe", "invoice", false, 0},
		{"mian", "main.go", false, 0}, {"2027", "2026", false, 0},
		{"report2027", "report2026", false, 0}, {"report2", "report", false, 0},
		{"repor2", "report", false, 0}, {"invoice", "invoice", true, 0},
		{"invoice invoice", "invoice", true, 0}, {"invoice", "invoixe invoice", true, 0},
		{"invoice", "invoice-longer invoice", true, 0}, {"invoice", "invoicé.pdf", false, 0},
		{"screnshot", "Screenshot 2026-01-01 at 10.00.00\u202fAM.png", true, 1},
		{"invocie", "invoice – final.pdf", true, 1}, {"invoice", "Invoice’s copy.pdf", true, 0},
		{"invoice", "invoice ① copy.pdf", false, 0}, {"invoice", "invoice\ue000.pdf", false, 0},
		{"invoice", "invoicex\u0301s.pdf", false, 0},
	} {
		t.Run(tc.query+"/"+tc.name, func(t *testing.T) {
			q, err := parseFuzzyQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			r, ok := matchFuzzyComponents(q, []string{tc.name})
			if ok != tc.match || ok && r.edits != tc.edits {
				t.Fatalf("rank=%+v match=%v", r, ok)
			}
		})
	}
}

// Only blank text is an error. Each bound below is exercised at its limit and
// one step past it, where the query must fall back to ordinary search alone.
func TestFuzzyParse(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "   "} {
		if _, err := parseFuzzyQuery(raw); !errors.Is(err, ErrFuzzyQuery) {
			t.Errorf("%q: got %v, want ErrFuzzyQuery", raw, err)
		}
	}
	words := func(n, size int) string {
		return strings.TrimSpace(strings.Repeat(strings.Repeat("a", size)+" ", n))
	}
	for _, tc := range []struct {
		raw      string
		eligible bool
	}{
		{"a", false}, {"ab", false}, {"invoice", true}, {"a/b", true}, {"/invoice", true},
		{"é//!!!", false}, {strings.Repeat("é", 256), false}, {string([]byte{255}), false},
		{words(15, 15) + " " + strings.Repeat("b", 16), true},
		{words(15, 15) + " " + strings.Repeat("b", 17), false},
		{strings.Repeat("a", 64), true}, {strings.Repeat("a", 65), false},
		{words(16, 5), true}, {words(17, 5), false},
		{"a/b/c/d/e/f/g/h", true}, {"a/b/c/d/e/f/g/h/i", false},
		{"cases//invoice", false}, {"cases/invoice/", false}, {"report 2024/", false},
		{"https://example.com", false}, {"cases/./invoice", false}, {"../invoice", false},
		{"cases/!!!", false}, {"!!!", false}, {"/", false},
	} {
		q, err := parseFuzzyQuery(tc.raw)
		if err != nil || q.eligible != tc.eligible {
			t.Errorf("%q: %+v %v", tc.raw, q, err)
		}
	}
	q, err := parseFuzzyQuery(`invoice" OR *`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fuzzyAnchor(q); got != `"i"* "or"*` {
		t.Fatalf("unsafe/wrong anchor %q", got)
	}
}

func TestFuzzyPathAndRank(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query                   string
		path                    []string
		match                   bool
		edits, prefixes, suffix int
	}{
		{"cases/acme/invocie", []string{"nested", "cases", "acme", "invoice.pdf"}, true, 1, 0, 0},
		{"/cases/acme/invoice", []string{"nested", "cases", "acme", "invoice.pdf"}, false, 0, 0, 0},
		{"/cases/acme/invoice", []string{"cases", "acme", "invoice.pdf"}, true, 0, 0, 0},
		{"caess/acme/invoice", []string{"cases", "acme", "invoice.pdf"}, true, 1, 0, 0},
		{"caess/acme/invocie", []string{"cases", "acme", "invoice.pdf"}, false, 0, 0, 0},
		{"cases/acme/invoice", []string{"資料", "cases", "acme", "invoice.pdf"}, true, 0, 0, 0},
		{"cases/acme/invoice", []string{"cases", "acmé", "invoice.pdf"}, false, 0, 0, 0},
		{"invoice", []string{"invoices.pdf"}, true, 0, 1, 1},
		{"invoice", []string{"invoices-invoice.pdf"}, true, 0, 0, 0},
		{"cases", []string{"cases", "invoice.pdf"}, false, 0, 0, 0},
	} {
		q, err := parseFuzzyQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		r, ok := matchFuzzyComponents(q, tc.path)
		if ok != tc.match || ok && r != (fuzzyRank{tc.edits, tc.prefixes, tc.suffix}) {
			t.Errorf("%q %v: %+v %v", tc.query, tc.path, r, ok)
		}
	}
}

// referenceFuzzyWord enumerates legal edits rather than sharing verification
// or character-count code with the optimized matcher.
func referenceFuzzyWord(query, candidate string) (fuzzyRank, bool) {
	if strings.HasPrefix(candidate, query) {
		suffix := len(candidate) - len(query)
		p := 0
		if suffix > 0 {
			p = 1
		}
		return fuzzyRank{0, p, suffix}, true
	}
	if len(query) < 5 || len(candidate) == 0 || query[0] != candidate[0] {
		return fuzzyRank{}, false
	}
	variants := map[string]bool{}
	letter := func(b byte) bool { return b >= 'a' && b <= 'z' }
	for i := 1; i < len(query); i++ {
		if letter(query[i]) {
			variants[query[:i]+query[i+1:]] = true
			for c := byte('a'); c <= 'z'; c++ {
				variants[query[:i]+string(c)+query[i+1:]] = true
			}
		}
		if i+1 < len(query) && letter(query[i]) && letter(query[i+1]) {
			variants[query[:i]+string([]byte{query[i+1], query[i]})+query[i+2:]] = true
		}
	}
	for i := 1; i <= len(query); i++ {
		for c := byte('a'); c <= 'z'; c++ {
			variants[query[:i]+string(c)+query[i:]] = true
		}
	}
	best := fuzzyRank{1, 2, 1000}
	ok := false
	for v := range variants {
		if strings.HasPrefix(candidate, v) {
			suffix := len(candidate) - len(v)
			p := 0
			if suffix > 0 {
				p = 1
			}
			r := fuzzyRank{1, p, suffix}
			if !ok || r.prefixes < best.prefixes || r.prefixes == best.prefixes && r.suffix < best.suffix {
				best = r
				ok = true
			}
		}
	}
	return best, ok
}

func checkFuzzyReference(t *testing.T, query, candidate string) {
	t.Helper()
	// Materialize arbitrary bytes into the supported ASCII alphanumeric domain.
	word := func(s string) string {
		b := []byte(s)
		for i := range b {
			b[i] = "abcdefghijklmnopqrstuvwxyz0123456789"[int(b[i])%36]
		}
		return string(b)
	}
	if len(query) == 0 || len(query) > 64 || len(candidate) == 0 || len(candidate) > 512 {
		return
	}
	query, candidate = word(query), word(candidate)
	q, err := parseFuzzyQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	want, wok := referenceFuzzyWord(query, candidate)
	got, ok := matchFuzzyComponents(q, []string{candidate})
	if ok != wok || ok && got != want {
		t.Fatalf("query=%q candidate=%q: %+v %v want %+v %v", query, candidate, got, ok, want, wok)
	}
}

func FuzzFuzzyMatchesReference(f *testing.F) {
	for _, pair := range [][2]string{{"invocie", "invoice"}, {"invoce", "invoice"}, {"invoicce", "invoice"}, {"invoixe", "invoice"}, {"report2027", "report2026"}, {"aaaaa", "aaaaaa"}, {"prefix", "prefixes"}} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(checkFuzzyReference)
}

func TestFuzzyMatchesReference(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"invoice", "aaaaa", "report2026", "abcdefghi"} {
		for i := 1; i < len(q); i++ {
			for _, c := range []byte{'a', 'z', '0'} {
				for _, v := range []string{q[:i] + string(c) + q[i+1:], q[:i] + string(c) + q[i:], q[:i] + q[i+1:]} {
					checkFuzzyReference(t, q, v)
					checkFuzzyReference(t, v, q)
				}
			}
		}
	}
}
