package conflicts

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestFindAlignment(t *testing.T) {
	tests := []struct {
		name, query, a, b string
		want              Status
	}{
		{"different values", "cooling period", "The cooling period is 7 days.", "The cooling period is 15 days.", Conflicting},
		{"equal units", "cooling period", "The cooling period is 24 hours.", "The cooling period is 1 day.", NoConflict},
		{"different units", "cooling period", "The cooling period is 7 business days.", "The cooling period is 15 days.", NotComparable},
		{"query anchor", "refund", "The cooling period is 7 days.", "The cooling period is 15 days.", NotComparable},
		{"discriminator", "customer", "New customers receive 1 free relocation.", "Existing customers receive 2 free relocations.", NotComparable},
		{"count anchor", "relocation", "Customers receive 1 free relocation.", "Customers receive 2 free relocations.", Conflicting},
		{"modifier", "relocation", "Customers receive 1 free relocation.", "Customers receive 2 paid relocations.", NotComparable},
		{"negated", "period", "The period is not 7 days.", "The period is 15 days.", NotComparable},
		{"reordered", "fee", "The fee is 1% and 2%.", "The fee is 2% and 1%.", NotComparable},
		{"partial", "period", "… the cooling period is 7 days.", "The cooling period is 15 days.", NotComparable},
		{"qualifier", "period", "The cooling period is at least 7 days.", "The cooling period is at most 15 days.", NotComparable},
		{"month precision", "policy", "The policy effective date is July 2026.", "The policy effective date is 1 July 2026.", NoConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Find(Input{Query: tt.query, Passages: []Passage{{Document: "a", Text: tt.a}, {Document: "b", Text: tt.b}}})
			if got.Status != tt.want {
				t.Fatalf("status = %s, want %s: %+v", got.Status, tt.want, got)
			}
		})
	}
}

func TestFindPairEdgesAndPeriods(t *testing.T) {
	in := Input{Query: "policy", Passages: []Passage{
		{Document: "a", Text: "The policy effective date is 1 July 2026."},
		{Document: "b", Text: "The policy effective date is July 2026."},
		{Document: "c", Text: "The policy effective date is 2 July 2026."},
	}}
	got := Find(in)
	if got.Status != Conflicting || got.Stats.AlignedPairs != 3 || got.Stats.AgreeingPairs != 2 || len(got.Conflicts) != 1 || got.Conflicts[0].Sides[0].Passage != 0 || got.Conflicts[0].Sides[1].Passage != 2 {
		t.Fatalf("date edges: %+v", got)
	}
	p := Find(Input{Query: "cooling period", Passages: []Passage{
		{Document: "a", Text: "From 1 July 2026 the cooling period is 15 days."},
		{Document: "b", Text: "From 1 July 2024 to 30 June 2026 the cooling period is 7 days."},
		{Document: "c", Text: "The cooling period is 10 days."},
	}})
	if len(p.Conflicts) != 3 || p.Conflicts[0].PeriodsOverlap != "false" || p.Conflicts[1].PeriodsOverlap != "unknown" {
		t.Fatalf("period edges: %+v", p)
	}
}

func TestFindSameDocumentAndOffsets(t *testing.T) {
	in := Input{Query: "period", Passages: []Passage{{Document: "a", Text: "Élan period is 7 days."}, {Document: "a", Text: "Élan period is 9 days."}, {Document: "b", Text: "Élan period is 15 days."}}}
	got := Find(in)
	if len(got.Conflicts) != 2 {
		t.Fatalf("edges: %+v", got)
	}
	for _, edge := range got.Conflicts {
		for _, side := range edge.Sides {
			r := []rune(in.Passages[side.Passage].Text)
			f := side.Figure
			if string(r[f.Start:f.End]) != f.Text || string(r[f.ClauseStart:f.ClauseEnd]) != f.Clause {
				t.Fatalf("bad offsets: %+v", side)
			}
		}
	}
}

func TestFindEmptyContentQueryDoesNotAnchor(t *testing.T) {
	got := Find(Input{Query: "the", Passages: []Passage{{Document: "a", Text: "The period is 7 days."}, {Document: "b", Text: "The period is 15 days."}}})
	if got.Status != NotComparable {
		t.Fatalf("status = %s", got.Status)
	}
}

func FuzzFind(f *testing.F) {
	f.Add(strings.Repeat("Period fee $10. ", 20), strings.Repeat("Period fee $20. ", 20), "period")
	f.Add("The cooling period is 7 days.", "The cooling period is 15 days.", "period")
	f.Add("Élan has 2 free devices.", "Élan has 3 free devices.", "devices")
	f.Add("From １ July ２０２６ the fee is $5.", "From １ July ２０２４ the fee is $6.", "fee")
	f.Add("The fee from 31 February 2024 is $10.", "The fee from 31 February 2025 is $20.", "fee")
	f.Add("The cooling period isn’t 7 days.", "The cooling period is 15 days.", "period")
	f.Add("The fee is $10 per 2 months.", "The fee is $20 per 4 months.", "fee")
	f.Add("The period is 7 days...", "The period is 15 days.", "period")
	f.Fuzz(func(t *testing.T, a, b, q string) {
		if len(a) > 512 || len(b) > 512 || len(q) > 128 {
			t.Skip()
		}
		in := Input{Query: q, Passages: []Passage{{Document: "a", Text: a}, {Document: "b", Text: b}}}
		first := Find(in)
		second := Find(in)
		if first.Stats.Comparisons > MaxComparisons || len(first.Conflicts) > MaxConflicts {
			t.Fatal("comparison or output bound exceeded")
		}
		if first.Truncated && first.Status == NoConflict {
			t.Fatal("incomplete comparison asserted agreement")
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatal("nondeterministic report")
		}
		for _, edge := range first.Conflicts {
			for _, s := range edge.Sides {
				if s.Passage < 0 || s.Passage >= len(in.Passages) {
					t.Fatal("passage out of bounds")
				}
				text := []rune(in.Passages[s.Passage].Text)
				fig := s.Figure
				if fig.Start < 0 || fig.End < fig.Start || fig.End > len(text) || fig.ClauseStart < 0 || fig.ClauseEnd < fig.ClauseStart || fig.ClauseEnd > len(text) {
					t.Fatal("offset out of bounds")
				}
				if string(text[fig.Start:fig.End]) != fig.Text || string(text[fig.ClauseStart:fig.ClauseEnd]) != fig.Clause {
					t.Fatal("inexact offsets")
				}
			}
		}
		in.Passages[1].Document = "a"
		if len(Find(in).Conflicts) != 0 {
			t.Fatal("same document conflict")
		}
	})
}

func TestFindNontransitiveSubjects(t *testing.T) {
	got := Find(Input{Passages: []Passage{
		{Document: "a", Text: "Alpha beta is 1%."},
		{Document: "b", Text: "Alpha beta gamma is 2%."},
		{Document: "c", Text: "Beta gamma is 3%."},
	}})
	// The endpoints do not align even though both align with the middle passage.
	if len(got.Conflicts) != 2 {
		t.Fatalf("edges = %+v", got.Conflicts)
	}
}

func TestFindComparableFigures(t *testing.T) {
	tests := []struct {
		a, b string
		want Status
	}{
		{"The late fee is 1%.", "The late fee is 2%.", Conflicting},
		{"The late fee is 1%.", "The late fee is 2 percentage points.", NotComparable},
		{"The policy number: AB1234.", "The policy number: XY9876.", Conflicting},
		{"The subscription costs $1000.00.", "The subscription costs $1,000.", NoConflict},
		{"The subscription costs $1000.", "The subscription costs €1000.", NotComparable},
		{"Plan 59 includes 2 devices.", "Plan 59 Plus includes 3 devices.", NotComparable},
		{"The renewal period is approximately 7 days.", "The renewal period is 15 days.", NotComparable},
		{"For example the renewal period is 7 days.", "The renewal period is 15 days.", NotComparable},
	}
	for _, tt := range tests {
		got := Find(Input{Passages: []Passage{{Document: "a", Text: tt.a}, {Document: "b", Text: tt.b}}})
		if got.Status != tt.want {
			t.Errorf("%q / %q: got %s want %s", tt.a, tt.b, got.Status, tt.want)
		}
	}
}

func TestFindSkippedReasons(t *testing.T) {
	got := Find(Input{Passages: []Passage{
		{Document: "a", Text: "… the renewal period is 7 days."},
		{Document: "b", Text: "The renewal period is not 15 days."},
		{Document: "c", Text: "The policy contains ordinary prose."},
	}})
	for i, want := range []string{"partial", "negated", "no_figures"} {
		if got.Skipped[i] != want {
			t.Errorf("passage %d: reason = %q, want %q", i, got.Skipped[i], want)
		}
	}
}

func TestFindNegatedContractions(t *testing.T) {
	for _, apostrophe := range []string{"'", "’", "‘", "ʼ"} {
		t.Run(apostrophe, func(t *testing.T) {
			got := Find(Input{Query: "cooling period", Passages: []Passage{
				{Document: "a", Text: "The cooling period isn" + apostrophe + "t 7 days."},
				{Document: "b", Text: "The cooling period is 15 days."},
			}})
			if got.Status != NotComparable || got.Stats.AlignedPairs != 0 || got.Skipped[0] != "negated" {
				t.Fatalf("negated contraction compared or skip reason diverged: %+v", got)
			}
		})
	}
}

func TestFindASCIIDotRunEllipses(t *testing.T) {
	for _, text := range []string{"The cooling period is 7 days...", "...The cooling period is 7 days."} {
		t.Run(text, func(t *testing.T) {
			got := Find(Input{Query: "cooling period", Passages: []Passage{{Document: "a", Text: text}, {Document: "b", Text: "The cooling period is 15 days."}}})
			if got.Status != NotComparable || got.Skipped[0] != "partial" {
				t.Fatalf("ellipsis fragment compared: %+v", got)
			}
		})
	}
}

func TestFindDifferentRateDenominators(t *testing.T) {
	for _, tt := range []struct{ name, a, b string }{
		{"per time", " per month", " per year"},
		{"each time", " each month", " each year"},
		{"slash time", "/month", "/year"},
		{"article time", " a month", " a year"},
		{"per count", " per device", " per account"},
		{"each count", " each device", " each account"},
		{"slash count", "/device", "/account"},
		{"article count", " a device", " an account"},
		{"per modified time", " per business day", " per business month"},
		{"slash modified time", "/business day", "/business month"},
		{"article modified count", " a free device", " a free account"},
		{"per numeric time", " per 2 months", " per 4 months"},
		{"slash numeric time", "/2 months", "/4 months"},
		{"per numeric count", " per 2 devices", " per 4 devices"},
		{"slash numeric count", "/2 devices", "/4 devices"},
	} {
		for _, amount := range []string{"5", "60"} {
			t.Run(tt.name+"/numerator "+amount, func(t *testing.T) {
				got := Find(Input{Query: "fee", Passages: []Passage{
					{Document: "a", Text: "The service fee is $5" + tt.a + "."},
					{Document: "b", Text: "The service fee is $" + amount + tt.b + "."},
				}})
				if got.Status != NotComparable || got.Stats.AlignedPairs != 0 {
					t.Fatalf("different rate denominators compared: %+v", got)
				}
			})
		}
	}
}

func TestFindNumericDenominatorsAreNotStandaloneFigures(t *testing.T) {
	for _, pair := range [][2]string{
		{"The service fee is $10 per 2 months.", "The service fee is $20 per 4 months."},
		{"The service fee is $10/2 devices.", "The service fee is $20/4 devices."},
		{"The service fee is $10 per 2 months.", "The service fee is €10 per 4 months."},
		{"The service fee is $10 per 2 devices.", "The service fee is €10 per 4 devices."},
	} {
		t.Run(pair[0], func(t *testing.T) {
			got := Find(Input{Query: "fee", Passages: []Passage{{Document: "a", Text: pair[0]}, {Document: "b", Text: pair[1]}}})
			if got.Status != NotComparable || got.Stats.AlignedPairs != 0 {
				t.Fatalf("numeric denominator treated as an independent figure: %+v", got)
			}
		})
	}
}

func TestFindDoesNotAlignSwappedFigureRoles(t *testing.T) {
	got := Find(Input{Query: "fee", Passages: []Passage{
		{Document: "a", Text: "The fee is $5 and the tax is 3%."},
		{Document: "b", Text: "The fee is 3% and the tax is $5."},
	}})
	if got.Status != NotComparable || got.Stats.AlignedPairs != 0 {
		t.Fatalf("swapped roles compared: %+v", got)
	}
}

func TestFindSkipsOversizedPassage(t *testing.T) {
	got := Find(Input{Query: "fee", Passages: []Passage{
		{Document: "a", Text: "The fee is $5. " + strings.Repeat("x", MaxPassageBytes)},
		{Document: "b", Text: "The fee is $6."},
	}})
	if got.Skipped[0] != "too_long" || got.Stats.Comparisons != 0 || got.Status != NotComparable {
		t.Fatalf("oversized passage compared: %+v", got)
	}
}

func TestFindRateDenominatorRole(t *testing.T) {
	got := Find(Input{Query: "fee", Passages: []Passage{
		{Document: "a", Text: "The device and account fee is $5 per device."},
		{Document: "b", Text: "The device and account fee is $60 per account."},
	}})
	if got.Status != NotComparable || got.Stats.AlignedPairs != 0 {
		t.Fatalf("shared nouns hid denominator roles: %+v", got)
	}
}

func TestFindUnsupportedRateSyntax(t *testing.T) {
	for _, pair := range [][2]string{
		{"The fee is $5 every month.", "The fee is $60 every year."},
		{"The fee is $5 every month.", "The fee is $5 every year."},
		{"The fee is $5 per first month.", "The fee is $60 per first year."},
		{"The fee is $5 per the first month.", "The fee is $60 per the first year."},
		{"The fee is $5 per your first month.", "The fee is $60 per your first year."},
		{"The service fee is $5/per month.", "The service fee is $6/per year."},
		{"The device and account fee is $5/each device.", "The device and account fee is $6/each account."},
		{"Per device the service fee is $5.", "Per account the service fee is $5."},
		{"Per device the service fee is $5.", "Per account the service fee is $60."},
		{"Each month the service fee is $5.", "Each year the service fee is $60."},
		{"The service fee per device is $5.", "The service fee per account is $60."},
		{"The service fee/device is $5.", "The service fee/account is $60."},
		{"The service fee is $5 per 2 months per account.", "The service fee is $60 per 2 months per device."},
	} {
		t.Run(pair[0], func(t *testing.T) {
			got := Find(Input{Query: "fee", Passages: []Passage{{Document: "a", Text: pair[0]}, {Document: "b", Text: pair[1]}}})
			if got.Status != NotComparable || got.Stats.AlignedPairs != 0 {
				t.Fatalf("unsupported rate syntax compared: %+v", got)
			}
		})
	}
}

func TestFindSlashRateDoesNotAcceptArithmetic(t *testing.T) {
	for _, text := range []string{"The fee is $5/2.", "The fee is $5/2 devices + $10.", "The fee is $5/month + $10."} {
		got := Find(Input{Query: "fee", Passages: []Passage{{Document: "a", Text: text}, {Document: "b", Text: "The fee is $6/month."}}})
		if got.Status != NotComparable || got.Stats.AlignedPairs != 0 {
			t.Errorf("arithmetic treated as a rate for %q: %+v", text, got)
		}
	}
}

func TestFindRejectsUnitlessRateDenominators(t *testing.T) {
	for _, suffix := range []string{" per 2", " each 2", " a 2", " per five", " each five", " a five"} {
		t.Run(suffix, func(t *testing.T) {
			got := Find(Input{Query: "fee", Passages: []Passage{
				{Document: "a", Text: "The service fee is $5" + suffix + "."},
				{Document: "b", Text: "The service fee is $6" + suffix + "."},
			}})
			if got.Status != NotComparable || got.Stats.AlignedPairs != 0 {
				t.Fatalf("unitless rate compared: %+v", got)
			}
		})
	}
}

func TestFindSameRateDenominator(t *testing.T) {
	for _, suffix := range []string{" per month", " each month", "/month", " a month", " per device", "/device", " a device", " per business day", " a free device", " per 2 months", "/2 devices"} {
		t.Run(suffix, func(t *testing.T) {
			got := Find(Input{Query: "fee", Passages: []Passage{
				{Document: "a", Text: "The service fee is $5" + suffix + "."},
				{Document: "b", Text: "The service fee is $6" + suffix + "."},
			}})
			if got.Status != Conflicting || len(got.Conflicts) != 1 {
				t.Fatalf("same-denominator conflict omitted: %+v", got)
			}
		})
	}
	got := Find(Input{Query: "period", Passages: []Passage{
		{Document: "a", Text: "The cooling period is 24 hours per device."},
		{Document: "b", Text: "The cooling period is 1 day per device."},
	}})
	if got.Status != NoConflict || got.Stats.AgreeingPairs != 1 {
		t.Fatalf("duration conversion changed: %+v", got)
	}
}

func TestFindSyntheticFixtures(t *testing.T) {
	cases := []struct {
		a, b   string
		status Status
	}{
		{"The grace period is 7 days.", "The grace period is 10 days.", Conflicting},
		{"Restoration takes 24 hours.", "Restoration takes 2 hours.", Conflicting},
		{"The late fee is 2%.", "The late fee is 1%.", Conflicting},
		{"Members receive one free relocation.", "Members receive two free relocations.", Conflicting},
		{"The policy date is 1 July 2026.", "The policy date is 2 July 2026.", Conflicting},
		{"The renewal period is 1 week.", "The renewal period is 7 days.", NoConflict},
		{"The renewal period is 1 year.", "The renewal period is 12 months.", NoConflict},
		{"The fee is at least 2%.", "The fee is at least 3%.", Conflicting},
		{"The fee is at least 2%.", "The fee is at most 3%.", NotComparable},
		{"The service grants 1 free device.", "The service grants 2 paid devices.", NotComparable},
		{"The monthly fee is $5.", "The annual fee is $10.", NotComparable},
		{"The R3 reset period is 5 days.", "The R5 reset period is 8 days.", NotComparable},
		{"The plan 59 service allows 2 devices.", "The plan 60 service allows 3 devices.", NotComparable},
		{"The renewal period is around 7 days.", "The renewal period is 15 days.", NotComparable},
		{"The renewal period is never 7 days.", "The renewal period is 15 days.", NotComparable},
		{"The renewal period is 7 days; for example the fee is 2%.", "The fee is 4%.", NotComparable},
		{"The renewal period is 7 days …", "The renewal period is 15 days.", NotComparable},
		{"The fee is 1% and 2%.", "The fee is 3% and 4%.", NotComparable},
		{"The fee is $10.", "The fee is €15.", NotComparable},
		{"The fee is 7%.", "The fee is 7 percentage points.", NotComparable},
	}
	for _, tt := range cases {
		t.Run(tt.a+" / "+tt.b, func(t *testing.T) {
			got := Find(Input{Passages: []Passage{{Document: "a", Text: tt.a}, {Document: "b", Text: tt.b}}})
			if got.Status != tt.status {
				t.Errorf("got %s, want %s; %+v", got.Status, tt.status, got)
			}
		})
	}
}

// conflictEdges compares document and value pairs independently of rank and duplicate edges.
func conflictEdges(in Input, report Report) []string {
	set := map[string]bool{}
	for _, e := range report.Conflicts {
		sides := make([]string, 0, 2)
		for _, s := range e.Sides {
			f := s.Figure
			sides = append(sides, fmt.Sprintf("%q/%q/%q/%q/%q/%q", in.Passages[s.Passage].Document, f.Value, f.EndValue, f.Precision, f.Qualifier, f.Text))
		}
		sort.Strings(sides)
		set[fmt.Sprintf("%q/%q/%q/%q", e.Kind, e.Unit, sides[0], sides[1])] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func FuzzFindInvariance(f *testing.F) {
	var manyA, manyB strings.Builder
	for i := range 20 {
		fmt.Fprintf(&manyA, "Period fee $%d. ", i+10)
		fmt.Fprintf(&manyB, "Period fee $%d. ", i+100)
	}
	f.Add(manyA.String(), manyB.String())
	f.Add("The period is 7 days.", "The period is 15 days.")
	f.Add("The period is 2%.", "The period is 3%.")
	f.Add("The period fee is $10 per 2 devices.", "The period fee is $20 per 4 devices.")
	f.Add("From １ July ２０２６ the period is 7 days.", "From １ July ２０２４ the period is 15 days.")
	f.Fuzz(func(t *testing.T, a, b string) {
		if len(a) > 512 || len(b) > 512 {
			t.Skip()
		}
		in := Input{Query: "period", Passages: []Passage{{Document: "a", Text: a}, {Document: "b", Text: b}}}
		baselineReport := Find(in)
		// Ranked cutoffs may retain different subsets after reordering or adding
		// passages. Edge-set invariance describes complete comparisons only.
		if baselineReport.Truncated {
			t.Skip("incomplete comparison has rank-dependent coverage")
		}
		baseline := conflictEdges(in, baselineReport)
		reversed := Input{Query: in.Query, Passages: []Passage{in.Passages[1], in.Passages[0]}}
		reversedReport := Find(reversed)
		if got := conflictEdges(reversed, reversedReport); !reversedReport.Truncated && !reflect.DeepEqual(got, baseline) {
			t.Fatalf("permutation changed edges: %v vs %v", got, baseline)
		}
		duplicate := Input{Query: in.Query, Passages: append(append([]Passage{}, in.Passages...), in.Passages[0])}
		duplicateReport := Find(duplicate)
		if got := conflictEdges(duplicate, duplicateReport); !duplicateReport.Truncated && !reflect.DeepEqual(got, baseline) {
			t.Fatalf("duplicate changed edges: %v vs %v", got, baseline)
		}
		unrelated := Input{Query: in.Query, Passages: append(append([]Passage{}, in.Passages...), Passage{Document: "c", Text: "The unrelated zqv is 11 days."})}
		unrelatedReport := Find(unrelated)
		if got := conflictEdges(unrelated, unrelatedReport); !unrelatedReport.Truncated && !reflect.DeepEqual(got, baseline) {
			t.Fatalf("unrelated passage changed edges: %v vs %v", got, baseline)
		}
	})
}

func TestFindCapitalizedSubjectAfterFigure(t *testing.T) {
	for _, pair := range [][2]string{
		{"$10 Premium service fee.", "$20 Standard service fee."},
		{"１０ dollars Premium service fee.", "２０ dollars Standard service fee."},
	} {
		got := Find(Input{Passages: []Passage{{Document: "a", Text: pair[0]}, {Document: "b", Text: pair[1]}}})
		if got.Status != NotComparable || len(got.Conflicts) != 0 {
			t.Fatalf("%q: variant subjects aligned: %+v", pair, got)
		}
	}
}

func TestFindFullwidthPeriodDates(t *testing.T) {
	in := Input{Query: "cooling-off period", Passages: []Passage{
		{Document: "a", Text: "From １ July ２０２６ the cooling-off period is １５ days."},
		{Document: "b", Text: "From １ July ２０２４ to ３０ June ２０２６ the cooling-off period is ７ days."},
	}}
	got := Find(in)
	if got.Status != Conflicting || len(got.Conflicts) != 1 {
		t.Fatalf("normalized period comparison: %+v", got)
	}
	edge := got.Conflicts[0]
	if edge.PeriodsOverlap != "false" || !reflect.DeepEqual(edge.Subject, []string{"cooling", "off", "period"}) {
		t.Fatalf("periods or subjects retain date words: %+v", edge)
	}
	for _, side := range edge.Sides {
		text := []rune(in.Passages[side.Passage].Text)
		if string(text[side.Figure.Start:side.Figure.End]) != side.Figure.Text {
			t.Fatalf("inexact original figure: %+v", side)
		}
	}
}

func TestInvalidDateKeepsYearDiscriminator(t *testing.T) {
	for _, tt := range []struct{ name, a, b string }{
		{"impossible calendar date", "The fee from 31 February 2024 is $10.", "The fee from 31 February 2025 is $20."},
		{"ambiguous numeric date", "The fee from 01/02/2024 is $10.", "The fee from 01/02/2025 is $20."},
		{"numeric continuation", "The fee from 2024-07-01.2 is $10.", "The fee from 2025-07-01.2 is $20."},
		{"approximate date", "The fee from about 1 July 2024 is $10.", "The fee from about 1 July 2025 is $20."},
		{"overlapping identifier", "Policy: 2024-07-01 fee is $10.", "Policy: 2025-07-01 fee is $20."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Find(Input{Query: "fee", Passages: []Passage{{Document: "one", Text: tt.a}, {Document: "two", Text: tt.b}}})
			if got.Status != NotComparable || got.Stats.AlignedPairs != 0 {
				t.Fatalf("rejected dates erased different year subjects: %+v", got)
			}
		})
	}
}

func TestFindBoundsConflictOutput(t *testing.T) {
	in := Input{Passages: []Passage{
		{Document: "a", Text: strings.Repeat("The fee is $10. ", 12)},
		{Document: "b", Text: strings.Repeat("The fee is $20. ", 12)},
	}}
	got := Find(in)
	if got.Status != Conflicting || len(got.Conflicts) != MaxConflicts || !got.Truncated {
		t.Fatalf("unbounded conflict output: status %s, conflicts %d", got.Status, len(got.Conflicts))
	}
	if got.Stats.Comparisons != 101 || !reflect.DeepEqual(got, Find(in)) {
		t.Fatalf("output cutoff changed work or stable order: %+v", got.Stats)
	}
	for i, edge := range got.Conflicts {
		if edge.Sides[0].Figure.Start != 11+(i/12)*16 || edge.Sides[1].Figure.Start != 11+(i%12)*16 {
			t.Fatalf("edge %d did not retain rank/figure order: %+v", i, edge)
		}
	}
}

func TestFindRateDenominatorsDoNotConsumePairBudget(t *testing.T) {
	text := strings.Repeat("The fee is $5 per 2 months. ", 75)
	got := Find(Input{Query: "fee", Passages: []Passage{{Document: "a", Text: text}, {Document: "b", Text: text}}})
	if got.Truncated || got.Status != NoConflict || got.Stats.Comparisons != 75*75 {
		t.Fatalf("denominators consumed comparison budget: %+v", got.Stats)
	}
}

func TestFindBoundsComparisonWork(t *testing.T) {
	for _, differing := range []bool{false, true} {
		t.Run(strconv.FormatBool(differing), func(t *testing.T) {
			last := "The fee is $10."
			if differing {
				last = "The fee is $20."
			}
			in := Input{Passages: []Passage{
				{Document: "a", Text: strings.Repeat("The fee is $10. ", 100)},
				{Document: "b", Text: strings.Repeat("The fee is $10. ", 100)},
				{Document: "c", Text: last},
			}}
			got := Find(in)
			if got.Status != NotComparable || got.Stats.AlignedPairs != MaxComparisons || got.Stats.Comparisons != MaxComparisons || len(got.Conflicts) != 0 || !got.Truncated {
				t.Fatalf("incomplete comparison asserted agreement or continued: status %s, aligned %d, conflicts %d", got.Status, got.Stats.AlignedPairs, len(got.Conflicts))
			}
		})
	}
}

func TestFindPassageLimitDoesNotAssertAgreement(t *testing.T) {
	in := Input{}
	for i := range 21 {
		text := "The fee is $10."
		if i == 20 {
			text = "The fee is $20."
		}
		in.Passages = append(in.Passages, Passage{Document: strconv.Itoa(i), Text: text})
	}
	if got := Find(in); got.Status != NotComparable || !got.Truncated || got.Stats.PassagesRead != 20 {
		t.Fatalf("incomplete comparison asserted agreement: %s", got.Status)
	}
}

func TestFindExactLimitsAreComplete(t *testing.T) {
	for _, tt := range []struct {
		name, a, b             string
		want                   Status
		comparisons, conflicts int
	}{
		{"comparison", strings.Repeat("The fee is $10. ", 100), strings.Repeat("The fee is $10. ", 100), NoConflict, 10000, 0},
		{"output", strings.Repeat("The fee is $10. ", 10), strings.Repeat("The fee is $20. ", 10), Conflicting, 100, 100},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Find(Input{Passages: []Passage{{Document: "a", Text: tt.a}, {Document: "b", Text: tt.b}}})
			if got.Status != tt.want || got.Truncated || got.Stats.Comparisons != tt.comparisons || len(got.Conflicts) != tt.conflicts {
				t.Fatalf("complete comparison incorrectly truncated: status %s, truncated %v, comparisons %d, conflicts %d", got.Status, got.Truncated, got.Stats.Comparisons, len(got.Conflicts))
			}
		})
	}
}

func TestFindRepeatedFiguresAtPassageBounds(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(strconv.FormatBool(ambiguous), func(t *testing.T) {
			in := Input{}
			for i := range 20 {
				value := 10 + i
				clause := fmt.Sprintf("Fee $%d. ", value)
				if ambiguous {
					clause = fmt.Sprintf("Fee $%d and $%d. ", value, value+1)
				}
				in.Passages = append(in.Passages, Passage{Document: strconv.Itoa(i), Text: strings.Repeat(clause, MaxPassageBytes/len(clause))})
			}
			got := Find(in)
			if got.Stats.PassagesRead != 20 || got.Stats.PassagesWithFigures != 20 || got.Stats.Comparisons > MaxComparisons || len(got.Conflicts) > MaxConflicts {
				t.Fatalf("passage bounds exceeded: %+v, conflicts %d", got.Stats, len(got.Conflicts))
			}
			if ambiguous {
				if got.Truncated || got.Status != NotComparable || got.Stats.Comparisons != 0 || len(got.Conflicts) != 0 {
					t.Fatalf("ambiguous figures entered comparison: %+v", got.Stats)
				}
			} else if !got.Truncated || got.Status != Conflicting || len(got.Conflicts) != MaxConflicts {
				t.Fatalf("repeated figures bypassed output bound: status %s, truncated %v, conflicts %d", got.Status, got.Truncated, len(got.Conflicts))
			}
		})
	}
}

func TestFindMalformedUTF8BeforeFigure(t *testing.T) {
	in := Input{Passages: []Passage{{Document: "a", Text: "\xe4B 0$"}, {Document: "b", Text: "B 1$"}}}
	got := Find(in)
	if got.Status != Conflicting || len(got.Conflicts) != 1 || !reflect.DeepEqual(got.Conflicts[0].Subject, []string{"b"}) {
		t.Fatalf("malformed prefix changed subject or comparison: %+v", got)
	}
	for _, side := range got.Conflicts[0].Sides {
		text := []rune(in.Passages[side.Passage].Text)
		if string(text[side.Figure.Start:side.Figure.End]) != side.Figure.Text {
			t.Fatalf("bad figure offset: %+v", side)
		}
	}
}

func TestFindCompoundMoneyDoesNotComparePartialAmounts(t *testing.T) {
	const subject = "The residential service subscription renewal administration processing fee is "
	for _, pair := range [][2]string{
		{"$7 and fifty cents", "$7.50"},
		{"$7 and 50 cents", "$7.50"},
		{"7 dollars and fifty cents", "7.50 dollars"},
		{"USD 7 and fifty cents", "USD 7.50"},
		{"7$ and fifty cents", "7.50$"},
		{"$7 and fifty cents and $8", "$8.50"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				passages := []Passage{{Document: "a", Text: subject + pair[0] + "."}, {Document: "b", Text: subject + pair[1] + "."}}
				if reverse {
					passages[0], passages[1] = passages[1], passages[0]
				}
				got := Find(Input{Query: "fee", Passages: passages})
				if got.Status != NotComparable || got.Stats.AlignedPairs != 0 || got.Stats.Comparisons != 0 || len(got.Conflicts) != 0 {
					t.Fatalf("unsupported money addition compared a partial amount (reverse=%v): %+v", reverse, got)
				}
			}
		})
	}
}

func TestFindCompoundDurationRemainsAmbiguous(t *testing.T) {
	for _, other := range []string{"7 days", "3 hours", "171 hours"} {
		got := Find(Input{Query: "period", Passages: []Passage{
			{Document: "a", Text: "The service cooling period is 7 days and 3 hours."},
			{Document: "b", Text: "The service cooling period is " + other + "."},
		}})
		if got.Status != NotComparable || got.Stats.AlignedPairs != 0 || got.Stats.Comparisons != 0 || len(got.Conflicts) != 0 {
			t.Fatalf("compound duration component compared against %q: %+v", other, got)
		}
	}
}

func TestFindEquivalentRateMarkers(t *testing.T) {
	for _, suffixes := range [][2]string{
		{" per month", " a month"},
		{"/month", " each month"},
		{" each month", " per month"},
		{" per account", " an account"},
		{" per business day", "/business day"},
		{" each free device", " a free device"},
		{" per 2 months", " each 2 months"},
		{"/2 devices", " per 2 devices"},
	} {
		for _, amount := range []string{"5", "6"} {
			t.Run(suffixes[0]+"/"+suffixes[1]+"/"+amount, func(t *testing.T) {
				in := Input{Query: "fee", Passages: []Passage{
					{Document: "a", Text: "The service fee is $5" + suffixes[0] + "."},
					{Document: "b", Text: "The service fee is $" + amount + suffixes[1] + "."},
				}}
				got := Find(in)
				want := Conflicting
				if amount == "5" {
					want = NoConflict
				}
				if got.Status != want || got.Stats.AlignedPairs != 1 {
					t.Fatalf("equivalent rate marker changed alignment: %+v", got)
				}
				if amount == "5" {
					if got.Stats.AgreeingPairs != 1 || len(got.Conflicts) != 0 {
						t.Fatalf("equal rates did not agree: %+v", got)
					}
					return
				}
				if len(got.Conflicts) != 1 || !reflect.DeepEqual(got.Conflicts[0].Subject, []string{"fee", "service"}) {
					t.Fatalf("accepted rate syntax remained in subject: %+v", got)
				}
				for _, side := range got.Conflicts[0].Sides {
					runes := []rune(in.Passages[side.Passage].Text)
					if string(runes[side.Figure.Start:side.Figure.End]) != side.Figure.Text || string(runes[side.Figure.ClauseStart:side.Figure.ClauseEnd]) != side.Figure.Clause {
						t.Fatalf("rate masking changed source offsets: %+v", side)
					}
				}
			})
		}
	}
}

func TestFindCrossMarkerRateConstraints(t *testing.T) {
	for _, pair := range [][2]string{
		{"The service fee is $5 per month.", "The service fee is $6 a year."},
		{"The device and account fee is $5/device.", "The device and account fee is $6 each account."},
		{"The service fee is $5 per 2 months.", "The service fee is $6 each 4 months."},
		{"The service fee is $5 each free device.", "The service fee is $6 a paid device."},
		{"Per account the service fee is $5 per month.", "The service fee is $6 a month."},
		{"Each customer the service fee is $5/month.", "The service fee is $6 each month."},
	} {
		t.Run(pair[0]+"/"+pair[1], func(t *testing.T) {
			got := Find(Input{Query: "fee", Passages: []Passage{{Document: "a", Text: pair[0]}, {Document: "b", Text: pair[1]}}})
			if got.Status != NotComparable || got.Stats.AlignedPairs != 0 || len(got.Conflicts) != 0 {
				t.Fatalf("different denominator or unbound marker lost its constraint: %+v", got)
			}
		})
	}
}

func TestFindDisclosesAmbiguousPassage(t *testing.T) {
	got := Find(Input{Query: "fee", Passages: []Passage{
		{Document: "a", Text: "The fee is $10."},
		{Document: "b", Text: "The fee is $500 and $250 for students."},
		{Document: "c", Text: "The fee is $10."},
	}})
	if got.Status != NoConflict || got.Stats.AlignedPairs != 1 || got.Stats.AgreeingPairs != 1 || got.Skipped[1] != "ambiguous" {
		t.Fatalf("agreeing companions hid an unexamined ambiguous passage: %+v", got)
	}
	if len(got.Skipped) != 1 {
		t.Fatalf("eligible companions incorrectly skipped: %+v", got.Skipped)
	}
}
