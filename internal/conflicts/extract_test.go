package conflicts

import (
	"math/big"
	"strings"
	"testing"
)

func TestExtractKinds(t *testing.T) {
	for _, tt := range []struct{ text, kind string }{{"$7", "money"}, {"7%", "percent"}, {"2026-07-01", "date"}, {"Fee $7 日本語", "money"}} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Kind != tt.kind || got[0].Text != string([]rune(tt.text)[got[0].Start:got[0].End]) {
			t.Errorf("typed figure %q: %+v", tt.text, got)
		}
	}
	if got := Extract("料金 $7"); len(got) != 0 {
		t.Errorf("non-Latin clause: %+v", got)
	}

	if g := Extract("Cost is 7€."); len(g) != 1 || g[0].Unit != "€" {
		t.Fatalf("currency suffix: %+v", g)
	}

	text := "The fee is €7."
	if got := Extract(text); len(got) != 1 || got[0].Text != "€7" || string([]rune(text)[got[0].Start:got[0].End]) != "€7" {
		t.Fatalf("currency prefix: %+v", got)
	}
	text = "The fee is $1,000.50. The wait is 2 days."
	got := Extract(text)
	if len(got) != 2 {
		t.Fatalf("got %d figures: %+v", len(got), got)
	}
	if got[0].Value != "1000.5" || got[0].Text != "$1,000.50" || got[1].Value != "2880" {
		t.Fatalf("unexpected figures: %+v", got)
	}
	for _, f := range got {
		if string([]rune(text)[f.Start:f.End]) != f.Text {
			t.Fatalf("bad span: %+v", f)
		}
	}

	tests := []struct{ text, kind, unit, value, end, qualifier string }{
		{"Fee is 2 percent.", "percent", "percent", "2", "", ""},
		{"Fee is 2%.", "percent", "percent", "2", "", ""},
		{"Rate rose 2 percentage points.", "percent", "percentage point", "2", "", ""},
		{"Wait is between 7 and 10 business days.", "duration", "business days", "7", "10", ""},
		{"Wait is 1 year.", "duration", "months", "12", "", ""},
		{"Wait is zero days.", "duration", "minutes", "0", "", ""},
		{"Wait is one day.", "duration", "minutes", "1440", "", ""},
		{"Wait is twenty days.", "duration", "minutes", "28800", "", ""},
		{"There are one free relocation.", "count", "relocation", "1", "", ""},
		{"Policy number: AB-1234 applies.", "identifier", "policy", "ab1234", "", ""},
		{"Effective date is 1 July 2026.", "date", "date", "2026-07-01", "", ""},
		{"Effective date is July 2026.", "date", "date", "2026-07", "", ""},
		{"Effective date is 13/07/2026.", "date", "date", "2026-07-13", "", ""},
		{"Cost is EUR 7.", "money", "eur", "7", "", ""}, {"Cost is 7 euros.", "money", "euro", "7", "", ""},
		{"Cost is 7 rubles.", "money", "rouble", "7", "", ""},
		{"Wait is 3 min.", "duration", "minutes", "3", "", ""}, {"Wait is 2 weeks.", "duration", "minutes", "20160", "", ""},
		{"Start is 2026-07-01.", "date", "date", "2026-07-01", "", ""}, {"Start is July 1, 2026.", "date", "date", "2026-07-01", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			g := Extract(tt.text)
			if len(g) != 1 {
				t.Fatalf("got %+v", g)
			}
			f := g[0]
			if f.Kind != tt.kind || f.Unit != tt.unit || f.Value != tt.value || f.EndValue != tt.end || f.Qualifier != tt.qualifier {
				t.Fatalf("got %+v", f)
			}
		})
	}
	for _, adjective := range []string{"free", "paid"} {
		g := Extract("There are 2 " + adjective + " devices.")
		if len(g) != 1 || g[0].Modifier != adjective {
			t.Fatalf("modifier %q: %+v", adjective, g)
		}
	}
	{
		s := "Ｆｅｅ is １２ days. Cost is 7 EUR."
		got := Extract(s)
		if len(got) != 2 || got[0].Value != "17280" || got[0].Text != "１２ days" || got[1].Unit != "eur" {
			t.Fatalf("got %+v", got)
		}
		r := []rune(s)
		for _, f := range got {
			if string(r[f.Start:f.End]) != f.Text || string(r[f.ClauseStart:f.ClauseEnd]) != f.Clause {
				t.Fatalf("bad original span %+v", f)
			}
		}
	}
	for _, tt := range []struct{ date, value, precision string }{
		{"1\tJuly\t2026", "2026-07-01", "day"},
		{"July\t2026", "2026-07", "month"},
		{"July\t1,\t2026", "2026-07-01", "day"},
	} {
		text := "The start date is " + tt.date + "."
		got := Extract(text)
		if len(got) != 1 || got[0].Kind != "date" || got[0].Value != tt.value || got[0].Precision != tt.precision || got[0].Text != tt.date || string([]rune(text)[got[0].Start:got[0].End]) != got[0].Text || got[0].Clause != text {
			t.Errorf("date whitespace %q: %+v", text, got)
		}
	}
}
func TestExtractConservative(t *testing.T) {
	{
		if got := Extract("The wait is about [7 days]."); len(got) != 0 {
			t.Errorf("approximate bracketed figure: %+v", got)
		}
	}

	{
		for _, figure := range []string{"≈7 days", "≈$5", "≈(7 days)", "7 days ≈"} {
			text := "The value is " + figure + ". The independent fee is $3."
			got := Extract(text)
			if len(got) != 1 || got[0].Text != "$3" {
				t.Errorf("%q: %+v", text, got)
			}
		}
		got := Extract("The wait is ≈7 days and the fee is $3.")
		if len(got) != 1 || got[0].Text != "$3" {
			t.Errorf("independent figure: %+v", got)
		}
	}

	for _, tt := range []struct{ text, want string }{{"The wait is not\n\n7 days.", "7 days"}, {"- The wait is not\n- Wait is 7 days.", "7 days"}, {"Header | Value\nWait | 7 days\nFee is $5.", "$5"}} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Text != tt.want {
			t.Errorf("hard boundary %q: %+v", tt.text, got)
		}
	}
	for _, text := range []string{"The wait is not\n7 days.", "The wait is approximately\n7 days."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("wrapped qualifier %q: %+v", text, got)
		}
	}
	t.Run("give or take", func(t *testing.T) {
		for _, text := range []string{"The wait is 7 days and 3 hours, give or take 1 hour.", "The wait is 7 days and 3 hours, give or take 1 day.", "The fee is $5/month, give or take 1 dollar.", "The fee is $5/month, give or take $1."} {
			if got := Extract(text); len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
		for _, figure := range []string{"7 days", "$7", "7 percent", "7 devices"} {
			text := "The value is " + figure + ", give or take 1 day."
			if got := Extract(text); len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
		text := "The wait is ７ days, give or take 1 day and the fee is $5 per 2 devices."
		got := Extract(text)
		if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 devices" {
			t.Fatalf("independent rate: %+v", got)
		}
		for _, f := range got {
			if string([]rune(text)[f.Start:f.End]) != f.Text {
				t.Fatalf("source offset: %+v", f)
			}
		}
		if got := Extract("The wait is 7 days, more or lessening over time."); len(got) != 1 || got[0].Text != "7 days" {
			t.Fatalf("approximation word boundary: %+v", got)
		}
		if got := Extract("The wait is 7 days (standard service)."); len(got) != 1 || got[0].Text != "7 days" {
			t.Fatalf("parenthetical prose: %+v", got)
		}
	})

	for _, s := range []string{"About 7 days apply.", "This is not 7 days.", "For example 7 days apply.", "row | 7 days | cell", "Plan | Wait\n--- | ---\nBasic | 7 days", "Policy: XXXX1234 applies.", "The date is 07/01/2026.", "Plan 59 applies."} {
		if got := Extract(s); len(got) != 0 {
			t.Errorf("%q: %+v", s, got)
		}
	}
	for _, text := range []string{
		"The wait is 7 days [approximately].", "The wait is (7 days) approximately.", `The wait is "7 days" approximately.`,
		"The wait is 7 days and longer.", "The fee is $5 and change.",
		"The wait is 7 days and 3 hours (approximately).", "The wait is 7 days (approximately) and 3 hours.", "The wait is (approximately) 7 days, 3 hours.",
		"The wait is 7 days and 3 hours approximately.", "The wait is 7 days, 3 hours approximately.",
		"The wait is 7 days, more or less.", "The wait is 7 days and 3 hours, more or less.",
		"The wait is about 7 days and 3 hours.",
		"The wait is 7 days approximately and 3 hours.",
		"The wait is 3 hours and about 7 days.",
		"The wait is about 7 days and\t3 hours.",
		"The wait is about 7 days and (3 hours).",
	} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("approximate duration %q: %+v", text, got)
		}
	}
	for _, figure := range []string{"7 days", "$5", "5 percent", "5 devices"} {
		for _, bound := range []string{"longer", "shorter", "greater", "less", "higher", "lower"} {
			text := "The value is " + figure + " and " + bound + "."
			if got := Extract(text); len(got) != 0 {
				t.Errorf("bounded value %q: %+v", text, got)
			}
		}
	}
	for _, tt := range []struct{ text, first, second string }{
		{"The fee is $5 and change is $2.", "$5", "$2"},
		{"The fee is $5 and greater savings cost $2.", "$5", "$2"},
		{"The wait is 7 days and longer waits cost $5.", "7 days", "$5"},
	} {
		got := Extract(tt.text)
		if len(got) != 2 || got[0].Text != tt.first || got[1].Text != tt.second || string([]rune(tt.text)[got[1].Start:got[1].End]) != tt.second {
			t.Errorf("independent figures %q: %+v", tt.text, got)
		}
	}

	f := Extract("From 1 July 2024 to 30 June 2026 the wait is 7 days.")
	if len(f) != 1 || f[0].Period.From != "2024-07-01" || f[0].Period.To != "2026-06-30" {
		t.Fatalf("period: %+v", f)
	}
	t.Run("NegatedContractions", func(t *testing.T) {
		for _, apostrophe := range []string{"'", "’", "‘", "ʼ"} {
			t.Run(apostrophe, func(t *testing.T) {
				clause := "The customer" + apostrophe + "s cooling period is 15 days."
				text := "The cooling period isn" + apostrophe + "t 7 days. " + clause
				got := Extract(text)
				if len(got) != 1 || got[0].Text != "15 days" || got[0].Clause != clause {
					t.Fatalf("negated contraction extracted or source quote changed: %+v", got)
				}
				runes := []rune(text)
				f := got[0]
				if string(runes[f.Start:f.End]) != "15 days" || string(runes[f.ClauseStart:f.ClauseEnd]) != clause {
					t.Fatalf("original offsets changed: %+v", f)
				}
			})
		}
	})
	t.Run("SkipsCannotNegation", func(t *testing.T) {
		text := "The wait cannot be 7 days. The wait is 2 days."
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "2 days" || got[0].Clause != "The wait is 2 days." {
			t.Fatalf("cannot emitted an affirmative figure: %+v", got)
		}
		if got := Extract("The cannotary wait is 7 days."); len(got) != 1 {
			t.Fatalf("negation must match a whole word: %+v", got)
		}
	})
	t.Run("SkipsStandaloneNoAndWithout", func(t *testing.T) {
		for _, text := range []string{"There is no $7 fee.", "The policy is without a 7 day waiting period."} {
			t.Run(text, func(t *testing.T) {
				if got := Extract(text); len(got) != 0 {
					t.Fatalf("negated assertion retained: %+v", got)
				}
			})
		}
	})
	t.Run("SkipsApproximateExpressions", func(t *testing.T) {
		for _, text := range []string{
			"The wait is between about 7 and 10 days.",
			`The wait is about "7 days".`,
			"The wait is about ‘7 days’.",
			"The wait is approximately at least 7 days.",
			"The wait is approximately ≤ 7 days.",
			"The fee is between approximately 7 and 10 dollars.",
			"The rate is between nearly 7 and 10 percent.",
			"There are between roughly 7 and 10 devices.",
			"The wait is about 7 to 10 days.",
			"The wait is between 7 and about 10 days.",
		} {
			t.Run(text, func(t *testing.T) {
				if got := Extract(text); len(got) != 0 {
					t.Fatalf("approximate expression retained precise components: %+v", got)
				}
			})
		}
		for _, text := range []string{
			"The estimated fee is $5.",
			"The estimated monthly fee is $5 and the wait is 7 days.",
			"The estimated wait will be 7 days and the fee is $5.",
		} {
			got := Extract(text)
			want := []string{}
			if strings.Contains(text, "monthly") {
				want = []string{"7 days"}
			}
			if strings.Contains(text, "will be") {
				want = []string{"$5"}
			}
			if len(got) != len(want) {
				t.Errorf("estimated subject %q: %+v", text, got)
				continue
			}
			for i, f := range got {
				if f.Text != want[i] || string([]rune(text)[f.Start:f.End]) != f.Text {
					t.Errorf("independent figure %q: %+v", text, f)
				}
			}
		}
		for _, text := range []string{"The estimated fee changed and the charge is $5.", "The fee for estimated demand is $5."} {
			got := Extract(text)
			if len(got) != 1 || got[0].Text != "$5" {
				t.Errorf("independent prose %q: %+v", text, got)
			}
		}
		for _, phrase := range []string{"estimated to be", "approximately equal to"} {
			for _, figure := range []string{"7 days", "$7", "7 percent", "7 devices", "7-10 days", "at least 7 days"} {
				text := "The value is " + phrase + " " + figure + " and the separate fee is $3."
				got := Extract(text)
				if len(got) != 1 || got[0].Text != "$3" || string([]rune(text)[got[0].Start:got[0].End]) != "$3" {
					t.Errorf("%q: %+v", text, got)
				}
			}
		}
		for _, text := range []string{"The estimated to be higher fee is $7.", "The approximately equal to demand fee is $7.", "The approximately equal today fee is $7."} {
			if got := Extract(text); len(got) != 1 || got[0].Text != "$7" {
				t.Errorf("independent prose %q: %+v", text, got)
			}
		}
		for _, phrase := range []string{"estimated at", "about a maximum of"} {
			text := "The fee is " + phrase + " $5 and the charge is $8."
			got := Extract(text)
			if len(got) != 1 || got[0].Text != "$8" || got[0].Qualifier != "" {
				t.Errorf("approximation and independent figure %q: %+v", text, got)
			}
		}
		{
			if got := Extract("The fee is between about $7 and $10."); len(got) != 0 {
				t.Fatalf("%+v", got)
			}
		}
		for _, w := range []string{"about", "approximately", "around", "roughly", "nearly", "almost", "~"} {
			if g := Extract("Wait is " + w + " 7 days."); len(g) != 0 {
				t.Errorf("%q: %+v", w, g)
			}
		}
		for _, figure := range []string{"7 days", "$５", "7 percent", "7 devices", "7-10 days"} {
			for _, phrase := range []string{"approximately a total of", "around a total of", "AROUND  A TOTAL OF"} {
				text := "The value is " + phrase + " " + figure + "."
				if got := Extract(text); len(got) != 0 {
					t.Errorf("%q: %+v", text, got)
				}
			}
		}
		for _, text := range []string{"The wait is approximately a total of 7 days and 3 hours.", "The wait is approximately a total of ７ days and 3 hours and the fee is $5 per 2 devices."} {
			got := Extract(text)
			if strings.Contains(text, "$5") {
				if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 devices" || string([]rune(text)[got[0].Start:got[0].End]) != "$5" {
					t.Errorf("%q: %+v", text, got)
				}
			} else if len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
		if got := Extract("The total is a total of 7 days."); len(got) != 1 || got[0].Text != "7 days" {
			t.Errorf("exact total: %+v", got)
		}
		for _, figure := range []string{"7 days", "$７", "7 percent", "7 devices", "7-10 days"} {
			for _, modifier := range []string{"almost exactly", "ALMOST  EXACTLY", "about exactly"} {
				text := "The value is " + modifier + " " + figure + "."
				if got := Extract(text); len(got) != 0 {
					t.Errorf("%q: %+v", text, got)
				}
			}
		}
		for _, text := range []string{"The wait is almost exactly 7 days and 3 hours.", "The wait is almost exactly ７ days and 3 hours and the fee is $5 per 2 devices."} {
			got := Extract(text)
			if strings.Contains(text, "$5") {
				if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 devices" || string([]rune(text)[got[0].Start:got[0].End]) != "$5" {
					t.Errorf("%q: %+v", text, got)
				}
			} else if len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
		if got := Extract("The wait is exactly 7 days."); len(got) != 1 || got[0].Text != "7 days" {
			t.Errorf("exact: %+v", got)
		}
		for _, figure := range []string{"$5", "7 days", "7 percent", "7 devices"} {
			for _, text := range []string{"The value is " + figure + " (approximately).", "The value is (approximately) " + figure + ".", "The value is approximately (" + figure + ")."} {
				if got := Extract(text); len(got) != 0 {
					t.Errorf("%q: %+v", text, got)
				}
			}
		}
		text := "The rate is $5 per 2 devices and the fee is about $10."
		got := Extract(text)
		if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 devices" {
			t.Fatalf("independent rate: %+v", got)
		}
		text = "The wait is ７ days and ３ hours (approximately) and the fee is $5 per 2 devices."
		got = Extract(text)
		if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 devices" {
			t.Fatalf("independent rate: %+v", got)
		}
		for _, f := range got {
			if string([]rune(text)[f.Start:f.End]) != f.Text {
				t.Fatalf("source offset: %+v", f)
			}
		}
		text = "The wait is ７ days and ３ hours approximately and the fee is $5 per 2 devices."
		got = Extract(text)
		if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 devices" {
			t.Fatalf("independent rate: %+v", got)
		}
		for _, f := range got {
			if string([]rune(text)[f.Start:f.End]) != f.Text {
				t.Fatalf("source offset: %+v", f)
			}
		}
		for _, tt := range []struct{ text, want string }{
			{"The wait is 7 days and 3 hours approximately and the timeout is 2 days.", "2 days"},
			{"The wait is 7 days and 3 hours, more or less, and the fee is $5.", "$5"},
			{"The fee is $5 and the wait is approximately 7 days, 3 hours.", "$5"},
			{"The fee is about $5 and the charge is $10.", "$10"},
			{"The rate is about 5 percent and the charge is 10 percent.", "10 percent"},
			{"The wait is about 7 days and the fee is $5.", "$5"},
			{"The wait is about 7 days, $3 fee.", "$3"},
		} {
			got := Extract(tt.text)
			if len(got) != 1 || got[0].Text != tt.want || string([]rune(tt.text)[got[0].Start:got[0].End]) != tt.want {
				t.Errorf("independent fact %q: %+v", tt.text, got)
			}
		}
		for _, text := range []string{
			"The fees are about $5 and $10.", "The fees are $5 and $10 approximately.",
			"The rates are about 5 percent and 10 percent.", "There are about 2 devices and 3 users.",
			`The wait is about "7 days" and "3 hours".`, `The fees are about "$5" and "$10".`, `The rates are about "5 percent" and "10 percent".`, `There are about "2 devices" and "3 users".`,
		} {
			if got := Extract(text); len(got) != 0 {
				t.Errorf("shared approximation %q: %+v", text, got)
			}
		}
		if got := Extract("The wait is about 7 days, 3 hours."); len(got) != 0 {
			t.Errorf("approximate comma duration: %+v", got)
		}
		for _, figure := range []string{"$7", "7 days", "7 percent", "7 devices", "1 July 2026", "Policy: AB1234"} {
			for _, text := range []string{"The value is about: " + figure + ".", "The value is CIRCA : " + figure + ".", "The value is " + figure + ", estimated."} {
				if got := Extract(text); len(got) != 0 {
					t.Errorf("%q: %+v", text, got)
				}
			}
		}
		for _, text := range []string{"The wait is circa 7 days and 3 hours.", "The wait is 7 days and 3 hours, estimated."} {
			if got := Extract(text); len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
		text = "The wait is CIRCA: ７ days and 3 hours and the fee is $5 per 2 devices."
		got = Extract(text)
		if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 devices" {
			t.Errorf("%q: %+v", text, got)
		} else {
			for _, f := range got {
				if string([]rune(text)[f.Start:f.End]) != f.Text {
					t.Errorf("bad span: %+v", f)
				}
			}
		}
		for _, marker := range []string{"about", "approximately", "around", "roughly", "nearly", "almost", "~", ", approximately"} {
			for _, figure := range []string{"7 days", "$7", "7 percent", "7 devices", "1 July 2026", "Policy: AB1234"} {
				t.Run(figure+" "+marker, func(t *testing.T) {
					if got := Extract("The value is " + figure + " " + marker + "."); len(got) != 0 {
						t.Fatalf("approximate figure treated as exact: %+v", got)
					}
				})
			}
		}
	})
}

func TestExtractEllipses(t *testing.T) {
	for _, tt := range []struct {
		name, text string
		partial    []bool
	}{
		{"first fragment", "The period is 7 days...", []bool{true}},
		{"unicode newline", "…\nThe wait is 7 days.", []bool{true}},
		{"unicode fragments", "… the wait is 7 days …", []bool{true}},
		{"empty lines", "...\n\nThe wait is 7 days.", []bool{true}},
		{"last fragment", "...The period is 7 days.", []bool{true}},
		{"joined fragments", "The period is 7 days... The fee is $5.", []bool{true, true}},
		{"longer dot run", "The period is 7 days.... The fee is $5.", []bool{true, true}},
		{"complete surrounding clauses", "The wait is 1 day. ... The period is 7 days. The fee is $5.", []bool{false, true, false}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.partial) {
				t.Fatalf("figures: %+v", got)
			}
			runes := []rune(tt.text)
			for i, f := range got {
				if f.Partial != tt.partial[i] || string(runes[f.Start:f.End]) != f.Text || string(runes[f.ClauseStart:f.ClauseEnd]) != f.Clause {
					t.Errorf("figure %d: partial or original offsets: %+v", i, f)
				}
			}
		})
	}
}
func FuzzExtract(f *testing.F) {
	f.Add("The wait is " + strings.Repeat("9", 128) + " weeks.")
	f.Add("The wait is 7 days - 10 days.")
	f.Add("The fee is $7 to about $10.")
	f.Add("Policy: XXXX-2026-07-01 applies.")
	for _, s := range []string{"The fee is $1,000.50.", "From 1 July 2026 wait 15 days.", "\xff 2 days …", "The rate is one point five percent.", "The rate is one percent point five.",
		"Wait is twenty-one days.", "Wait is thirty-one days.", "Wait is 10-7 days.",
		"Wait is 20-1 days.", "Wait is 7-10 days.", "The period isn’t 7 days.", "The period is 7 days...", "...The period is 7 days.", "From １ July ２０２６ the fee is $5 per 2 months.", "Wait is ½ day.", "Cost is $1/2.", "The wait cannot be 7 days.", "Wait is 7 days approximately.", "Account number: 1234 5678.", "The fee is $7 and 2 devices are included.", "Policy: USD1234A.", "The fee is $fourteen."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r := []rune(s)
		var byteOffsets []int
		for i := range s {
			byteOffsets = append(byteOffsets, i)
		}
		byteOffsets = append(byteOffsets, len(s))
		prev := 0
		for _, v := range Extract(s) {
			if v.Start < prev || v.End < v.Start || v.End > len(r) || v.ClauseStart < 0 || v.ClauseStart > v.Start || v.ClauseEnd < v.End || v.ClauseEnd > len(r) {
				t.Fatalf("invalid span %+v", v)
			}
			if s[byteOffsets[v.Start]:byteOffsets[v.End]] != v.Text || s[byteOffsets[v.ClauseStart]:byteOffsets[v.ClauseEnd]] != v.Clause {
				t.Fatalf("bad figure span %+v", v)
			}
			if v.Kind == "money" || v.Kind == "percent" || v.Kind == "duration" || v.Kind == "count" {
				assertCanonicalNumber(t, v.Value)
				if v.EndValue != "" {
					assertCanonicalNumber(t, v.EndValue)
				}
			}
			prev = v.End
		}
	})
}

func assertCanonicalNumber(t *testing.T, value string) {
	t.Helper()
	scale := 0
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		scale = len(value) - dot - 1
		if strings.HasSuffix(value, "0") {
			t.Fatalf("non-canonical decimal %q", value)
		}
	}
	r, ok := new(big.Rat).SetString(value)
	if !ok || r.Sign() < 0 || r.FloatString(scale) != value {
		t.Fatalf("non-idempotent value %q", value)
	}
}

func TestExtractNormalizedValueExceedsSourceLimit(t *testing.T) {
	text := "The wait is " + strings.Repeat("9", 128) + " weeks."
	got := Extract(text)
	// 10080 * (10^128 - 1), written independently of the normalizer.
	want := "10079" + strings.Repeat("9", 123) + "89920"
	if len(got) != 1 || got[0].Value != want || got[0].Unit != "minutes" {
		t.Fatalf("source-limit normalization: %+v", got)
	}
	assertCanonicalNumber(t, got[0].Value)
}

func TestExtractSentenceEndingInAbbreviationSuffix(t *testing.T) {
	{
		got := Extract("The fee is $5. The wait is 7 days.")
		if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "7 days" {
			t.Errorf("sentence boundary: %+v", got)
		}
	}

	if g := Extract("Policy No. is AB1234."); len(g) != 1 || g[0].Kind != "identifier" || g[0].Value != "ab1234" || g[0].Clause != "Policy No. is AB1234." {
		t.Fatalf("abbreviation: %+v", g)
	}
	t.Run("multiple abbreviations", func(t *testing.T) {
		text := "Policy No. is AB1234 and account No. is CD5678."
		got := Extract(text)
		if len(got) != 2 || got[0].Value != "ab1234" || got[1].Value != "cd5678" || got[1].Clause != text || string([]rune(text)[got[1].Start:got[1].End]) != got[1].Text {
			t.Fatalf("multiple abbreviations: %+v", got)
		}
	})

	answer := "No. The fee is $5."
	figures := Extract(answer)
	if len(figures) != 1 || figures[0].Text != "$5" || figures[0].Clause != "The fee is $5." || string([]rune(answer)[figures[0].ClauseStart:figures[0].ClauseEnd]) != "The fee is $5." {
		t.Errorf("standalone answer: %+v", figures)
	}
	text := "This is not a casino. The wait is 7 days."
	got := Extract(text)
	if len(got) != 1 || got[0].Text != "7 days" || got[0].Clause != "The wait is 7 days." {
		t.Fatalf("abbreviation suffix merged sentences: %+v", got)
	}
}

func TestExtractRejectsContinuedNumericExpressions(t *testing.T) {
	{
		for _, text := range []string{
			"The fee is $5 less a discount.",
			"The fee is $7 less a $2 discount.",
			"The fee is $7 less a discount of $2.",
			"The wait is 7 days less a 2 day reduction.",
		} {
			got := Extract(text + " The independent fee is $3.")
			if len(got) != 1 || got[0].Text != "$3" {
				t.Errorf("%q: %+v", text, got)
			}
		}
		if got := Extract("The fee is $5 less the rebate."); len(got) != 0 {
			t.Errorf("unresolved deduction: %+v", got)
		}
		if got := Extract("The fee is $5 less tax. The separate fee is $3."); len(got) != 1 || got[0].Text != "$3" {
			t.Errorf("bare deduction: %+v", got)
		}
		text := "The fee is $5, and tax is $3."
		if got := Extract(text); len(got) != 2 || got[0].Text != "$5" || got[1].Text != "$3" || string([]rune(text)[got[1].Start:got[1].End]) != "$3" {
			t.Errorf("independent tax: %+v", got)
		}
		text = "The fee is $5, and a discount is offered."
		if got := Extract(text); len(got) != 1 || got[0].Text != "$5" || string([]rune(text)[got[0].Start:got[0].End]) != "$5" {
			t.Errorf("independent discount %q: %+v", text, got)
		}
		for _, tt := range []struct{ text, first, second, qualifier string }{
			{"The wait is 7 days with less effort and the fee is $5.", "7 days", "$5", ""},
			{"The wait is less than 7 days and the fee is $5.", "7 days", "$5", "<"},
			{"The fee is $7 and a discount is offered for $5.", "$7", "$5", ""},
			{"The fee is $7 and the lessons cost $5.", "$7", "$5", ""},
		} {
			got := Extract(tt.text)
			if len(got) != 2 || got[0].Text != tt.first || got[1].Text != tt.second || got[0].Qualifier != tt.qualifier {
				t.Errorf("%q: %+v", tt.text, got)
			}
		}
	}

	for _, tt := range []struct{ text, want string }{
		{"The fee is $5 times are hard.", "$5"},
		{"The fee is $5 xylophones are included.", "$5"},
		{"The twice yearly fee is $5.", "$5"},
		{"The fee is $5 multiplied by demand.", "$5"},
		{"The fee is $5 divided by demand.", "$5"},
		{"The fee is $5 divided bystander votes.", "$5"},
		{"The fee is $5 for higher education.", "$5"},
		{"Demand is $5 per customer.", "$5"},
		{"The fee is $5 per month.", "$5"},
		{"Policy: AX1234 applies.", "Policy: AX1234"},
	} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Text != tt.want {
			t.Errorf("independent prose or identifier %q: %+v", tt.text, got)
		}
	}

	if got := Extract("The fee is $5 and the wait is 7 days."); len(got) != 2 || got[0].Text != "$5" || got[1].Text != "7 days" {
		t.Errorf("independent figures: %+v", got)
	}

	for _, text := range []string{"There are 10^3 devices.", "The fee is $10^3.", "There are 10 devices^3.", "The fee is 10^$3.", "The charge is two thirds of $100.", "The charge is three quarters of $100.", "The fee is $7, ±50 cents.", "The rate is 7 percent, ∓2 percent."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("unsupported expression %q: %+v", text, got)
		}
	}
	for _, numerator := range []string{"5", "five"} {
		for _, denominator := range []string{"10 devices", "ten devices", "10 days", "10 percent", "$10"} {
			text := "The fee is $3. The value is " + numerator + " out of " + denominator + ". The wait is 2 days."
			got := Extract(text)
			if len(got) != 2 || got[0].Text != "$3" || got[1].Text != "2 days" {
				t.Errorf("proportion %q: %+v", text, got)
			}
		}
	}

	for _, text := range []string{
		"The service is available 5 days out of 10 days and the fee is $3. The wait is 2 days.",
		"There are 5 devices out of 10 devices and the fee is $3. The wait is 2 days.",
		"The rate is 5 percent out of 10 percent and the fee is $3. The wait is 2 days.",
	} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "2 days" || string([]rune(text)[got[0].Start:got[0].End]) != got[0].Text {
			t.Errorf("typed proportion %q: %+v", text, got)
		}
	}

	for _, text := range []string{"The wait is 7 days to (10 days).", `The wait is 7 days through "10 days".`, "The deadline is 7 days after 10 days.", "The deadline is 7 days before 10 days.", "The deadline is 7 days prior to 10 days."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("relative operands %q: %+v", text, got)
		}
	}
	for _, s := range []string{"The fee is fifty dollars less $10.", "The charge is $100 less $10.", "The wait is 7 days less 2 hours."} {
		if got := Extract(s); len(got) != 0 {
			t.Errorf("%q: %+v", s, got)
		}
	}

	for _, text := range []string{
		"The wait is (7 days) or (10 days).", "The wait is ‘7 days’ or ‘10 days’.", "The wait is 7 days (or 10 days).",
		"The wait is 7 or 10 days.", "The wait is seven or ten days.",
		"The fee is 7 or 10 dollars.", "The fee is $7 or $10.",
		"There are 7 or 10 devices.", "The rate is 7 or 10 percent.",
	} {
		t.Run(text, func(t *testing.T) {
			if got := Extract(text); len(got) != 0 {
				t.Fatalf("alternatives retained an exact component: %+v", got)
			}
		})
	}

	for _, text := range []string{
		"Wait is 1/2 day.", "Wait is 1 / 2 day.", "Wait is ½ day.",
		"Cost is $1/2.", "Cost is $1 / 2.", "Cost is 1/2 dollars.",
		"Rate is 1/2 percent.", "There are 1/2 devices.",
		"The fee is $5/2 devices + $10.", "The fee is $5/month + $10.",
		"The fee is $5/" + strings.Repeat("9", 129) + " devices.",
		"Rate is 10-7 devices/5 days.", "Rate is about 2 devices/5 days.",
	} {
		t.Run(text, func(t *testing.T) {
			if got := Extract(text); len(got) != 0 {
				t.Fatalf("partial numeric expression: %+v", got)
			}
		})
	}

	t.Run("minus", func(t *testing.T) {
		for _, text := range []string{"The fee is $7 minus 50 cents.", "The wait is 7 days minus 50 seconds.", "The rate is 7 percent minus 50 basis points.", "There are 7 devices minus 50 parts."} {
			if got := Extract(text); len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
	})

	clauses := []string{
		"The fee is $5 per month + 2.", "The fee is $5 per 2.",
		"The fee is $5/month + 2.", "The fee is $5/2.",
		"The fee is $5 divided by 2.", "The wait is 10 days divided by 2.",
		"The fee is 10 divided by $5.", "The fee is $5 divided by 2 days.",
		"The fee is $5 divided  by -2.", "The fee is 10 divided\tby $5.",
		"The fee is 3 multiplied by $5.", "The fee is $5 multiplied by -2.",
		"The fee is $5 x 3.", "The fee is 3 x $5.", "The fee is $5 times 3.", "The fee is 3 times $5.",
		"The charge is half of $100.", "The charge is a quarter of $100.",
		"The charge is 1/2 of $100.", "The charge is 1 / 2 of $100.",
		"The wait is 7 days < 10 days.", "The wait is 7 days >= 10 days.",
		"The fee is $7 to 30 June 2026/2027.",
		"The fee is 50% off$100.", "The fee is 50% off $100.", "The fee is 50 percent off $100.",
		"The price is 5% higher than $100.",
		"The fee is 5 dollars less than 10 dollars.", "The fee is 5 dollars more than 10 dollars.", "The fee is 5 dollars greater than 10 dollars.",
		"The wait is 5 days less than 10 days.", "The wait is 5 days more than 10 days.", "The wait is 5 days greater than 10 days.",
		"The fee is $7 - $10.", "The fee is $7 – $10.", "The fee is $7 + $10.",
		"The fee is 7 dollars + 10 dollars.", "The fee is $7 plus $10.",
		"The fee is fifty dollars plus $10.",
		"The fee is $7 * $10.", "The fee is $7 × $10.", "The fee is $7 ÷ $10.",
		"The wait is 7 days - 10 days.", "The wait is 7 days–10 days.",
		"The rate is 7%-10%.", "The rate is 7%–10%.", "There are 7 devices - 10 devices.",
		"The rate is 1% 1% 1%.", "The wait is 7 days and an hour.", "The rate is 7%2.", "The wait is 7 days and a third.", "The wait is 7 days and a fifth.", "The wait is 7 days and a half.", "The rate is 7 percent and a half.",
		"The fee is $7 to about $10.", "The wait is from 7 to at most 10 days.",
		"The wait is between 7 and at most 10 days.",
		"The fee is between $7 and $10.", "The fee is $7 through about $10.",
		"The wait is 7 days up to 10 days.", "The rate is 7 percent up to 10 percent.", "There are 7 devices up to 10 devices.", "The fee is $7 up to $10.",
		"The wait is 7 through 10 days.", "The wait is 7 until 10 days.", "The wait is 7 till 10 days.",
		"The fee is $7 until about $10.", "The fee is $7 till about $10.",
	}
	for _, figure := range []string{"$7", "7 days", "7 percent", "7 devices"} {
		clauses = append(clauses, "Value is twice "+figure+".", "Value is "+figure+" multiplied by 2.", "Value is "+figure+" multiplied by two.")
		for _, op := range []string{"x", "times"} {
			clauses = append(clauses, "Value is "+figure+" "+op+" 3.", "Value is 3 "+op+" "+figure+".")
		}
	}
	for _, connector := range []string{"higher than", "lower than"} {
		for _, endpoints := range [][2]string{{"5%", "$100"}, {"7 days", "10 days"}, {"7 devices", "10 devices"}} {
			clauses = append(clauses, "Value is ("+endpoints[0]+") "+connector+" "+endpoints[1]+".")
		}
	}
	for _, connector := range []string{"through", "until", "till"} {
		for _, endpoints := range [][2]string{{"7 days", "10 days"}, {"$7", "$10"}, {"7 percent", "10 percent"}, {"7 devices", "10 devices"}} {
			clauses = append(clauses, "Value is "+endpoints[0]+" "+connector+" "+endpoints[1]+".")
		}
	}
	for _, dash := range []string{"—", "‐", "‒", "―"} {
		clauses = append(clauses, "The wait is 7 "+dash+" 10 days.", "The wait is 7"+dash+"10 days.", "The wait is 7 days "+dash+" 10 days.", "The fee is $7"+dash+"$10.")
	}
	for _, op := range []string{"±", "∓", "plus or minus"} {
		for _, pair := range [][2]string{{"7", "1 days"}, {"7", "1 percent"}, {"7", "1 devices"}, {"$7", "$1"}, {"7 days", "2 days"}, {"7 percent", "1 percent"}, {"7 devices", "1 devices"}} {
			clauses = append(clauses, "The value is "+pair[0]+" "+op+" "+pair[1]+".")
		}
	}
	for _, clause := range clauses {
		t.Run(clause, func(t *testing.T) {
			text := "The fee is $5. " + clause + " The wait is 2 days."
			got := Extract(text)
			if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 days" {
				t.Fatalf("unsupported expression retained exact components: %+v", got)
			}
			for _, f := range got {
				if string([]rune(text)[f.Start:f.End]) != f.Text || string([]rune(text)[f.ClauseStart:f.ClauseEnd]) != f.Clause {
					t.Fatalf("surrounding source spans changed: %+v", f)
				}
			}
		})
	}
	t.Run("RejectsPartiallyJoinedToRanges", func(t *testing.T) {
		for _, text := range []string{"Wait is 7 to10 days.", "Wait is 7to 10 days."} {
			if got := Extract(text); len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
	})
}

func TestExtractRejectsContinuedDates(t *testing.T) {
	for _, tt := range []struct{ text, want string }{
		{"The period is 2026-07-01/2026-07-31.", ""},
		{"Date is 2026-07-01 / 2026-07-31.", ""},
		{"From 2026-07-01 / 2026-07-31 the fee is $7.", "$7"},
		{"From July 2026 / July 2027 the fee is $7.", "$7"},
	} {
		got := Extract(tt.text)
		if tt.want == "" {
			if len(got) != 0 {
				t.Errorf("%q: %+v", tt.text, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Text != tt.want || got[0].Kind != "money" || got[0].Period != (Period{}) || string([]rune(tt.text)[got[0].Start:got[0].End]) != tt.want {
			t.Errorf("%q: %+v", tt.text, got)
		}
	}

	for _, date := range []string{"2026-07-01-02", "1 July 2026/2027", "1 July 2026_extra"} {
		t.Run(date, func(t *testing.T) {
			if got := Extract("The start date is " + date + "."); len(got) != 0 {
				t.Fatalf("continued date retained a prefix: %+v", got)
			}
			got := Extract("From " + date + " the fee is $7.")
			if len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) {
				t.Fatalf("continued date became an exact period: %+v", got)
			}
		})
	}
}

func TestExtractQualifierVocabulary(t *testing.T) {
	{
		for _, tt := range []struct{ text, figure string }{
			{"The fee is max. $7.", "$7"}, {"The wait is max. 7 days.", "7 days"},
		} {
			got := Extract(tt.text + " The separate fee is $3.")
			if len(got) != 2 || got[0].Text != tt.figure || got[0].Qualifier != "≤" || got[1].Text != "$3" {
				t.Errorf("dotted maximum and independent sentence %q: %+v", tt.text, got)
			}
		}
	}

	{
		if got := Extract("The wait is at most [7 days]."); len(got) != 0 {
			t.Errorf("bounded bracketed figure: %+v", got)
		}
		for _, text := range []string{"The wait is [7 days].", "The wait is [7 days]. The fee is $5."} {
			got := Extract(text)
			want := 1
			if strings.Contains(text, "$5") {
				want = 2
			}
			if len(got) != want || got[0].Text != "7 days" || got[0].Qualifier != "" || want == 2 && got[1].Text != "$5" {
				t.Errorf("exact bracketed figure %q: %+v", text, got)
			}
		}
	}

	{
		for _, tt := range []struct{ text, qualifier string }{
			{"There is at most a 7 day wait.", "≤"},
			{"There is at least a 7 day wait.", "≥"},
			{"There is a 7 day wait.", ""},
			{"There is the 7 day wait.", ""},
		} {
			got := Extract(tt.text)
			if len(got) != 1 || got[0].Text != "7 day" || got[0].Qualifier != tt.qualifier {
				t.Errorf("%q: %+v", tt.text, got)
			}
		}
		for _, text := range []string{
			"The wait is approximately at most a 7 day wait and the fee is $5.",
			"The wait is at least a 7 day and 3 hour wait and the fee is $5.",
		} {
			got := Extract(text)
			if len(got) != 1 || got[0].Text != "$5" {
				t.Errorf("%q: %+v", text, got)
			}
		}
	}

	for _, figure := range []string{"7 days", "$7", "7 percent", "7 devices"} {
		text := "The value is at most (" + figure + ")."
		if got := Extract(text); len(got) != 0 {
			t.Errorf("punctuated prefix %q: %+v", text, got)
		}
	}

	for _, text := range []string{"The wait is at\nleast 7 days.", "The wait is at least 7\ndays."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Qualifier != "≥" || string([]rune(text)[got[0].Start:got[0].End]) != got[0].Text || got[0].Clause != text {
			t.Errorf("wrapped bound and source span %q: %+v", text, got)
		}
	}
	if got := Extract("The wait is at least\n7 days."); len(got) != 1 || got[0].Qualifier != "≥" {
		t.Errorf("wrapped bound: %+v", got)
	}
	for _, figure := range []string{"3 days", "$3", "3 percent", "3 devices"} {
		for _, bound := range []struct{ phrase, want string }{{"greater than", ">"}, {"more than or equal to", "≥"}, {"fewer than or equal to", "≤"}} {
			text := "The value is " + bound.phrase + " " + figure + " and the fee is $5 per 2 devices."
			got := Extract(text)
			if len(got) != 3 || got[0].Text != figure || got[0].Qualifier != bound.want || got[1].Text != "$5" || got[2].Text != "2 devices" {
				t.Errorf("bound %q: %+v", text, got)
			}
		}
	}
	for _, tt := range []struct{ text, qualifier string }{{"The waiting period is greater than 3 days.", ">"}, {"The wait is more than or equal to 3 days.", "≥"}} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Text != "3 days" || got[0].Qualifier != tt.qualifier {
			t.Errorf("standalone bound %q: %+v", tt.text, got)
		}
	}
	for _, text := range []string{"The wait is greater than 3 days and 2 hours.", "The wait is more than or equal to 3 days and 2 hours."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("coordinated bound %q: %+v", text, got)
		}
	}

	got := Extract("The wait is less than 7 days.")
	if len(got) != 1 || got[0].Text != "7 days" || got[0].Qualifier != "<" {
		t.Fatalf("bound: %+v", got)
	}

	t.Run("punctuated bounds", func(t *testing.T) {
		for _, text := range []string{"The wait is 7 days (up to 10 days).", "The wait is 7 days (maximum 10 days)."} {
			got := Extract(text)
			if len(got) != 1 || got[0].Text != "10 days" || got[0].Qualifier != "≤" {
				t.Errorf("%q: %+v", text, got)
			}
		}
		for _, figure := range []string{"10 days", "$10", "10 percent", "10 devices"} {
			for _, bound := range []string{"up to", "maximum", "≤"} {
				text := "The value is (" + bound + " " + figure + ") and the fee is $３ per 2 devices."
				got := Extract(text)
				if len(got) != 3 || got[0].Text != figure || got[0].Qualifier != "≤" || got[1].Text != "$３" || got[2].Text != "2 devices" {
					t.Errorf("%q: %+v", text, got)
					continue
				}
				for _, f := range got {
					if string([]rune(text)[f.Start:f.End]) != f.Text {
						t.Errorf("source offset: %+v", f)
					}
				}
			}
		}
	})

	got = Extract(`The wait is at most ("7 days"). The fee is $5.`)
	if len(got) != 1 || got[0].Text != "$5" {
		t.Errorf("combined wrappers: %+v", got)
	}

	for _, tt := range []struct{ text, qualifier string }{{"Wait is at  least 7 days.", "≥"}, {"Wait is up\tto 7 days.", "≤"}} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Qualifier != tt.qualifier || got[0].Period.From != "" {
			t.Errorf("phrase whitespace %q: %+v", tt.text, got)
		}
	}
	for _, text := range []string{`The wait is at most "7 days".`, "The wait is at most “7 days”."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Qualifier != "≤" || got[0].Text != "7 days" || string([]rune(text)[got[0].Start:got[0].End]) != "7 days" {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, figure := range []string{"7 days", "$７", "7 percent", "7 devices"} {
		for _, bound := range []struct{ phrase, want string }{{"a minimum of", "≥"}, {"maximum of", "≤"}, {"A MINIMUM  OF", "≥"}} {
			text := "The value is " + bound.phrase + " " + figure + " and the fee is $3 per 2 devices."
			got := Extract(text)
			if len(got) != 3 || got[0].Text != figure || got[0].Qualifier != bound.want || got[1].Text != "$3" || got[2].Text != "2 devices" {
				t.Errorf("%q: %+v", text, got)
				continue
			}
			for _, f := range got {
				if string([]rune(text)[f.Start:f.End]) != f.Text {
					t.Errorf("bad span: %+v", f)
				}
			}
		}
	}
	for _, figure := range []string{"7 days", "$７", "7 percent", "7 devices"} {
		for _, bound := range []struct{ phrase, want string }{{"less than or equal to", "≤"}, {"greater than or equal to", "≥"}, {"LESS  THAN OR EQUAL TO", "≤"}} {
			text := "The value is " + bound.phrase + " " + figure + " and the fee is $3 per 2 devices."
			got := Extract(text)
			if len(got) != 3 || got[0].Text != figure || got[0].Qualifier != bound.want || got[1].Text != "$3" || got[2].Text != "2 devices" {
				t.Errorf("%q: %+v", text, got)
				continue
			}
			for _, f := range got {
				if string([]rune(text)[f.Start:f.End]) != f.Text {
					t.Errorf("bad span: %+v", f)
				}
			}
		}
	}
	for _, figure := range []string{"$５", "7 days", "7 percent", "7 devices"} {
		for _, bound := range []string{"at most:", "AT  MOST :"} {
			text := "The value is " + bound + " " + figure + " and the fee is $3 per 2 devices."
			got := Extract(text)
			if len(got) != 3 || got[0].Text != figure || got[0].Qualifier != "≤" || got[1].Text != "$3" || got[2].Text != "2 devices" {
				t.Errorf("%q: %+v", text, got)
				continue
			}
			for _, f := range got {
				if string([]rune(text)[f.Start:f.End]) != f.Text {
					t.Errorf("bad span: %+v", f)
				}
			}
		}
	}
	for _, group := range []struct {
		words []string
		want  string
	}{
		{[]string{"up to", "at most", "no more than", "maximum", "max"}, "≤"},
		{[]string{"at least", "minimum", "no less than"}, "≥"},
		{[]string{"more than", "over", "exceeding"}, ">"},
		{[]string{"less than", "under", "fewer than"}, "<"},
		{[]string{"within"}, "within"},
	} {
		for _, w := range group.words {
			g := Extract("Wait is " + w + " 7 days.")
			if len(g) != 1 || g[0].Qualifier != group.want {
				t.Errorf("%q: %+v", w, g)
			}
		}
	}
	for _, text := range []string{"The fee exceeds $5.", "The fee exceeded $5.", "Fees exceed $5."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$5" || got[0].Qualifier != ">" {
			t.Errorf("bound %q: %+v", text, got)
		}
	}
	for _, text := range []string{"The fee is $5 and demand exceeds expectations.", "The fee of $5 exceeds the budget.", "The exceeded budget is $5."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$5" || got[0].Qualifier != "" {
			t.Errorf("independent exact figure %q: %+v", text, got)
		}
	}
	for _, tc := range []struct{ text, qualifier string }{
		{"The fee is >= $7.", "≥"}, {"The fee is <= $7.", "≤"},
		{"The wait is >= 7 days.", "≥"}, {"The rate is <= 7 percent.", "≤"}, {"There are >= 7 devices.", "≥"},
	} {
		got := Extract(tc.text)
		if len(got) != 1 || got[0].Qualifier != tc.qualifier {
			t.Errorf("%q: %+v", tc.text, got)
		}
	}
	for _, q := range []string{"≤", "≥", ">", "<"} {
		g := Extract("Wait is " + q + " 7 days.")
		if len(g) != 1 || g[0].Qualifier != q {
			t.Errorf("%s: %+v", q, g)
		}
	}
}

func TestExtractMoneyKeepsIndependentlyTypedConjunctions(t *testing.T) {
	for _, amount := range []string{"$7", "USD 7", "7 dollars", "7 USD", "7$"} {
		for _, other := range []string{"2 devices", "two free relocations", "3 hours", "2 percent", "2 dollars"} {
			t.Run(amount+" and "+other, func(t *testing.T) {
				got := Extract("The fee is " + amount + " and " + other + " are included.")
				if len(got) != 2 || got[0].Kind != "money" || got[0].Value != "7" || got[0].Text != amount || got[1].Text != other {
					t.Fatalf("independent typed figure treated as monetary continuation: %+v", got)
				}
			})
		}
	}
}

func TestExtractIdentifierPrecedesEmbeddedMoney(t *testing.T) {
	if g := Extract("Policy: 2026-07-01 applies."); len(g) != 0 {
		t.Fatalf("ambiguous date and identifier: %+v", g)
	}

	for _, tt := range []struct{ text, kind, figure string }{
		{"The invoice is 1000 dollars.", "money", "1000 dollars"},
		{"The order is 1000 devices.", "count", "1000 devices"},
		{"The invoice number is 1000 dollars.", "identifier", "invoice number is 1000"},
		{"The tax ID is 1000 dollars.", "identifier", "tax ID is 1000"},
	} {
		if got := Extract(tt.text); len(got) != 1 || got[0].Kind != tt.kind || got[0].Text != tt.figure {
			t.Errorf("label or amount %q: %+v", tt.text, got)
		}
	}
	for _, identifier := range []string{"USD1234A", "EUR1234", "GBP1234B"} {
		text := "Policy: " + identifier + "."
		got := Extract(text)
		if len(got) != 1 || got[0].Kind != "identifier" || got[0].Value != strings.ToLower(identifier) || got[0].Text != strings.TrimSuffix(text, ".") || got[0].Clause != text {
			t.Fatalf("identifier lost to embedded money in %q: %+v", text, got)
		}
	}
}

func TestExtractMoneySupportsEveryNumberWord(t *testing.T) {
	for _, tt := range []struct{ word, value string }{
		{"zero", "0"}, {"one", "1"}, {"two", "2"}, {"three", "3"},
		{"four", "4"}, {"five", "5"}, {"six", "6"}, {"seven", "7"},
		{"eight", "8"}, {"nine", "9"}, {"ten", "10"}, {"eleven", "11"},
		{"twelve", "12"}, {"thirteen", "13"}, {"fourteen", "14"}, {"fifteen", "15"},
		{"sixteen", "16"}, {"seventeen", "17"}, {"eighteen", "18"}, {"nineteen", "19"}, {"twenty", "20"},
	} {
		for _, prefix := range []string{"$", "USD "} {
			t.Run(prefix+tt.word, func(t *testing.T) {
				got := Extract("The fee is " + prefix + tt.word + ".")
				if len(got) != 1 || got[0].Kind != "money" || got[0].Value != tt.value || got[0].Text != prefix+tt.word {
					t.Fatalf("supported money number word: %+v", got)
				}
			})
		}
	}
}

func TestExtractSkipsOversizedPassage(t *testing.T) {
	s := strings.Repeat("a", MaxPassageBytes) + " 7 days."
	if got := Extract(s); len(got) != 0 {
		t.Fatalf("oversized passage: %+v", got)
	}
}

func TestExtractNumericBoundariesAndCurrencyCodes(t *testing.T) {
	for _, text := range []string{"The fee is CAD $5.", "The fee is USD$10.", "The fee is CAD $5 per 2 devices.", "The fee is $5 CAD.", "The fee is 5 CAD$.", "The fee is $5 and USD $10 per 2 devices."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, s := range []string{"Cost is USD 7.", "Cost is 7 USD."} {
		g := Extract(s)
		if len(g) != 1 || g[0].Unit != "usd" {
			t.Errorf("%s: %+v", s, g)
		}
	}
	for _, s := range []string{"Cost is 1,00 dollars.", "Wait is 1,00 days.", "Wait is " + strings.Repeat("9", 129) + " days.", "The wait is " + strings.Repeat("9", 129) + " days and 3 hours.", "The wait is 1,00 days and 3 hours."} {
		if g := Extract(s); len(g) != 0 {
			t.Errorf("%s: %+v", s, g)
		}
	}
}

func TestExtractRejectsCompoundWordNumerals(t *testing.T) {
	for _, text := range []string{
		"Wait is twenty-one days.",
		"Wait is twenty one days.",
		"Wait is thirty-one days.",
		"Wait is thirty one days.",
		"Wait is one hundred one days.",
		"Wait is one hundred and one days.",
		"Wait is twenty-five days.",
		"Wait is twenty–one days.",
		"Cost is $twenty-one.",
		"Cost is $twenty one.",
		"Cost is $one hundred.",
		"Cost is $twentyone.",
		"Cost is twenty-one dollars.",
		"Rate is twenty-one percent.",
		"There are twenty-one devices.",
	} {
		t.Run(text, func(t *testing.T) {
			if got := Extract(text); len(got) != 0 {
				t.Fatalf("unsupported compound numeral: %+v", got)
			}
		})
	}
}

func TestExtractRejectsIdentifierContinuations(t *testing.T) {
	for _, text := range []string{"The policy is effective 2026-07-01.", "The policy is effective 1 July 2026."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Kind != "date" || got[0].Value != "2026-07-01" || string([]rune(text)[got[0].Start:got[0].End]) != got[0].Text {
			t.Errorf("effective date %q: %+v", text, got)
		}
	}
	for _, text := range []string{
		"Policy number: AB-1234-.", "Policy number: 1234-.",
		"Policy: AB-1234-.", "Policy: USD1234-.", "Policy: 2026-07-01-.",
	} {
		got := Extract(text + " The fee is $5.")
		if len(got) != 1 || got[0].Text != "$5" {
			t.Errorf("incomplete identifier %q: %+v", text, got)
		}
	}
	got := Extract("Policy number: AB-1234- and the fee is $5.")
	if len(got) != 1 || got[0].Text != "$5" {
		t.Errorf("independent figure: %+v", got)
	}
	for _, text := range []string{"Account: 1234 ending in 1234.", "Account: 1234 (ending in 1234).", "Policy: USD1234 (ending in 1234).", "Policy: 2026-07-01 (ending in 0701)."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("masked identifier %q: %+v", text, got)
		}
	}
	for _, text := range []string{
		"Policy: **** (ending in EUR1234).",
		"Policy: **** ending in 2026-07-01, and the fee is $5.",
		"Policy: **** (ending in 1 July 2026), and the fee is $5.",
	} {
		got := Extract(text)
		want := 0
		if strings.Contains(text, "$5") {
			want = 1
		}
		if len(got) != want || want == 1 && (got[0].Text != "$5" || string([]rune(text)[got[0].Start:got[0].End]) != "$5" || got[0].Period != (Period{})) {
			t.Errorf("masked suffix %q: %+v", text, got)
		}
	}
	for _, tt := range []struct{ text, figure, value string }{
		{"Account: 1234 applies.", "Account: 1234", "1234"},
		{"Policy number: AB-1234 applies.", "Policy number: AB-1234", "ab1234"},
		{"Policy: USD1234 (active).", "Policy: USD1234", "usd1234"},
	} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Kind != "identifier" || got[0].Text != tt.figure || got[0].Value != tt.value || string([]rune(tt.text)[got[0].Start:got[0].End]) != tt.figure {
			t.Errorf("full identifier %q: %+v", tt.text, got)
		}
	}
	for _, text := range []string{
		"Policy: AB1234/56 applies.",
		"Policy: AB1234_CD applies.",
		"Policy: AB1234.CD applies.",
		"Policy: AB1234+56 applies.",
		"Policy: AB1234é applies.",
	} {
		t.Run(text, func(t *testing.T) {
			if got := Extract(text); len(got) != 0 {
				t.Fatalf("truncated identifier: %+v", got)
			}
		})
	}
	for _, text := range []string{"Policy: AB1234.", "Policy: AB1234, applies."} {
		if got := Extract(text); len(got) != 1 || got[0].Value != "ab1234" {
			t.Fatalf("identifier punctuation %q: %+v", text, got)
		}
	}
	{
		for _, token := range []string{"USD1234*", "USD1234_CD", "EUR1234/56", "GBP1234.CD", "USD1234 5678", "AB1234/USD7"} {
			t.Run(token, func(t *testing.T) {
				if got := Extract("Policy: " + token + "."); len(got) != 0 {
					t.Fatalf("rejected identifier became money: %+v", got)
				}
			})
		}
	}
	{
		for _, token := range []string{"XXXX-2026-07-01", "AB-2026-07-01", "2026-07-01/AB", "AB1234/2026-07-01", "invalid/2026-07-01"} {
			if got := Extract("Policy: " + token + " applies."); len(got) != 0 {
				t.Fatalf("identifier %q became another figure: %+v", token, got)
			}
		}
	}
	t.Run("RejectsWhitespaceIdentifierGroups", func(t *testing.T) {
		for _, text := range []string{
			"Account number: 1234 5678.", "Account number: 1234\t5678.",
			"Policy: AB1234 CD5678.", "Account number: １２３４\u202f５６７８.",
			"Account number: 1234 ABCD.", "Policy: AB1234 XYZ.",
		} {
			t.Run(text, func(t *testing.T) {
				if got := Extract(text); len(got) != 0 {
					t.Fatalf("truncated grouped identifier: %+v", got)
				}
			})
		}
		{
			if got := Extract("Policy: AB1234 2026-07-01 applies."); len(got) != 0 {
				t.Fatalf("%+v", got)
			}
		}
	})
}

func TestExtractRejectsSignedNumericPrefixes(t *testing.T) {
	t.Run("spoken signs", func(t *testing.T) {
		for _, text := range []string{"Rate is minus 7 percent.", "Rate is negative 7 percent.", "Rate is minus7 percent.", "Rate is negative7 percent.", "Rate is minus at most 7 percent.", "Fee is negative $7.", "Wait is minus 7-10 days."} {
			if got := Extract(text); len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
		for _, tt := range []struct {
			text    string
			figures []string
		}{
			{"The negative outlook has a fee of $7.", []string{"$7"}},
			{"Policy: NEGATIVE1234 applies.", []string{"Policy: NEGATIVE1234"}},
			{"The wait is 7 days and the fee is $3.", []string{"7 days", "$3"}},
		} {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Errorf("independent fact %q: %+v", tt.text, got)
				continue
			}
			for i, want := range tt.figures {
				if got[i].Text != want {
					t.Errorf("independent fact %q: %+v", tt.text, got)
				}
			}
		}
	})
	t.Run("accounting", func(t *testing.T) {
		for _, text := range []string{"The adjustment is ($7).", "The adjustment is (($7-10)).",
			"The adjustment is ($7/month).", "The adjustment is ($7 per month).",
			"The adjustment is ($7/2 devices).", "The adjustment is ($7 per 2 devices)."} {
			if got := Extract(text + " The separate fee is $3."); len(got) != 1 || got[0].Text != "$3" {
				t.Errorf("%q: %+v", text, got)
			}
		}
	})

	for _, text := range []string{
		"Wait is -7 days.",
		"-7 days is unsupported.",
		"Wait is −7 days.",
		"Wait is - 7 days.",
		"Wait is +7 days.",
		"Wait is −7–10 days.",
		"Cost is -$7.",
		"Cost is −7 dollars.",
		"Rate is −7 percent.",
		"There are −7 devices.",
	} {
		t.Run(text, func(t *testing.T) {
			if got := Extract(text); len(got) != 0 {
				t.Fatalf("unsupported signed figure: %+v", got)
			}
		})
	}
}

func TestExtractPreservesAscendingNumericRanges(t *testing.T) {
	for _, figure := range []string{"5-10 days", "$5-10", "5-10 percent", "5-10 devices"} {
		for _, bound := range []string{"at least", "up to", "more than", "less than"} {
			text := "The value is " + bound + " " + figure + ". The fee is $3."
			if got := Extract(text); len(got) != 1 || got[0].Text != "$3" {
				t.Errorf("qualified range %q: %+v", text, got)
			}
		}
	}
	if got := Extract("The wait is at least 5 days and the fee is $3."); len(got) != 2 || got[0].Text != "5 days" || got[0].Qualifier != "≥" || got[1].Text != "$3" {
		t.Errorf("independent scalar bound: %+v", got)
	}
	t.Run("reversed ranges", func(t *testing.T) {
		for _, text := range []string{
			"Wait is 10-7 days.",
			"The fee is $10-7 and 3 devices.",
			"The wait is 10-7 days and $5.",
			"The wait is 10-7 days and 3 hours.",
			"Wait is 10–7 days.",
			"Wait is 10 to 7 days.",
			"Wait is between 10 and 7 days.",
			"Wait is ten to seven days.",
			"Cost is $10-7.",
			"Cost is 10-7 dollars.",
			"Rate is 10-7 percent.",
			"There are 10-7 devices.",
			"Wait is 10.5-10.25 days.",
		} {
			t.Run(text, func(t *testing.T) {
				if got := Extract(text); len(got) != 0 {
					t.Fatalf("reversed range: %+v", got)
				}
			})
		}
	})
	for _, tt := range []struct{ text, kind, unit, value, end string }{
		{"Wait is 7-10 days.", "duration", "minutes", "10080", "14400"},
		{"Wait is one-two days.", "duration", "minutes", "1440", "2880"},
		{"Wait is 7-ten days.", "duration", "minutes", "10080", "14400"},
		{"Wait is seven-10 days.", "duration", "minutes", "10080", "14400"},
		{"Cost is $seven-ten.", "money", "$", "7", "10"},
		{"Rate is seven-ten percent.", "percent", "percent", "7", "10"},
		{"There are seven-ten devices.", "count", "device", "7", "10"},
		{"Wait is 7–10 days.", "duration", "minutes", "10080", "14400"},
		{"Wait is 7 to 10 days.", "duration", "minutes", "10080", "14400"},
		{"Wait is seven to ten days.", "duration", "minutes", "10080", "14400"},
		{"Wait is between seven and ten days.", "duration", "minutes", "10080", "14400"},
		{"Wait is between 7 and 10 days.", "duration", "minutes", "10080", "14400"},
		{"Wait is 7-7 days.", "duration", "minutes", "10080", "10080"},
		{"Wait is 10.25-10.5 days.", "duration", "minutes", "14760", "15120"},
		{"Cost is $7-10.", "money", "$", "7", "10"},
		{"Rate is 7-10 percent.", "percent", "percent", "7", "10"},
		{"There are 7-10 devices.", "count", "device", "7", "10"},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != 1 || got[0].Kind != tt.kind || got[0].Unit != tt.unit || got[0].Value != tt.value || got[0].EndValue != tt.end || got[0].Qualifier != "" {
				t.Fatalf("ascending range: %+v", got)
			}
		})
	}
	{
		for _, tt := range []struct {
			text, figure, value, end string
			start, finish            int
		}{
			{"Wait is seven–ten days.", "seven–ten days", "10080", "14400", 8, 22},
			{"Wait is seven-ten days.", "seven-ten days", "10080", "14400", 8, 22},
			{"The cooling period is seven-ten days.", "seven-ten days", "10080", "14400", 22, 36},
			{"Wait is 7–ten days.", "7–ten days", "10080", "14400", 8, 18},
			{"Wait is seven–10 days.", "seven–10 days", "10080", "14400", 8, 21},
		} {
			t.Run(tt.text, func(t *testing.T) {
				got := Extract(tt.text)
				if len(got) != 1 || got[0].Kind != "duration" || got[0].Unit != "minutes" || got[0].Qualifier != "" || got[0].Value != tt.value || got[0].EndValue != tt.end || got[0].Text != tt.figure || got[0].Start != tt.start || got[0].End != tt.finish || string([]rune(tt.text)[got[0].Start:got[0].End]) != tt.figure {
					t.Fatalf("range: %+v", got)
				}
			})
		}
		text := "The fee is $5/seven–ten devices."
		got := Extract(text)
		if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "seven–ten devices" || got[1].Value != "7" || got[1].EndValue != "10" || got[1].Start != 14 || got[1].End != 31 || string([]rune(text)[got[1].Start:got[1].End]) != got[1].Text {
			t.Fatalf("rate denominator: %+v", got)
		}
	}
}

func TestExtractOmitsCompoundMoneyClauses(t *testing.T) {
	for _, text := range []string{"The fee is $1,00 and membership includes 3 devices.", "The fee is USD7€.", "The fee is €USD7.", "The fee is 7USD€.", "The fee is $7€.", "The fee is $€7."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, amount := range []string{
		"$1,00 and membership includes 3 devices", "USD7€", "$7 and 50 cents", "$7 and fifty cents", "USD 7 and fifty cents",
		"7 dollars and 50 cents", "seven dollars and fifty cents", "7 USD and fifty cents",
		"7$ and fifty cents", "7€ and fifty cents", "7 euros and fifty cents",
		"$seven fifty", "7 dollars fifty cents", "$7 50 cents",
		"$7 AND\tfifty cents", "＄７ and fifty cents",
		"$7 and a half", "7 dollars and a half", "USD 7 and a half",
		"seven dollars, fifty cents", "$7, plus 50 cents", "$7 plus 50 cents", "$7 + 50 cents",
	} {
		t.Run(amount, func(t *testing.T) {
			// Dropping only the first amount must not leave another figure in
			// the unsupported clause available for comparison.
			text := "Wait is ２ days. Cost is " + amount + " and $8 for 3 devices in 4 hours; The fee is １２ dollars."
			got := Extract(text)
			if len(got) != 2 || got[0].Text != "２ days" || got[0].Value != "2880" || got[1].Text != "１２ dollars" || got[1].Value != "12" {
				t.Fatalf("unsupported money clause left partial figures: %+v", got)
			}
			runes := []rune(text)
			for _, f := range got {
				if string(runes[f.Start:f.End]) != f.Text || string(runes[f.ClauseStart:f.ClauseEnd]) != f.Clause {
					t.Fatalf("surrounding original offsets changed: %+v", f)
				}
			}
		})
	}
	for _, text := range []string{"The fee is $5 and a cent.", "The fee is $5 and one cent."} {
		got := Extract(text + " The wait is 7 days.")
		if len(got) != 1 || got[0].Text != "7 days" || got[0].Clause != "The wait is 7 days." {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, text := range []string{"The fee is $5 and a cent is refunded.", "The fee is $5 and a central office handles it."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$5" || string([]rune(text)[got[0].Start:got[0].End]) != "$5" {
			t.Errorf("independent statement %q: %+v", text, got)
		}
	}
}

func TestExtractCompoundDurationKeepsComponents(t *testing.T) {
	text := "The service cooling period is 7 days and 3 hours."
	got := Extract(text)
	if len(got) != 2 || got[0].Text != "7 days" || got[0].Value != "10080" || got[1].Text != "3 hours" || got[1].Value != "180" {
		t.Fatalf("compound duration lost components: %+v", got)
	}
	for _, f := range got {
		if f.Kind != "duration" || f.Unit != "minutes" || string([]rune(text)[f.Start:f.End]) != f.Text || f.Clause != text {
			t.Fatalf("duration components changed kind, unit or source: %+v", f)
		}
	}
}

func TestExtractDateImmediatelyBeforeFigure(t *testing.T) {
	for _, tt := range []struct{ text, figure, kind string }{{"From 1 July 2026: 7 days.", "7 days", "duration"}, {"From 1 July 2026: $7.", "$7", "money"}} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Text != tt.figure || got[0].Kind != tt.kind || got[0].Period != (Period{From: "2026-07-01"}) || string([]rune(tt.text)[got[0].Start:got[0].End]) != tt.figure {
			t.Errorf("date separator %q: %+v", tt.text, got)
		}
	}
	for _, text := range []string{"From 31 February 2026: 7 days.", "The value is 1: 7 days."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("unsupported prefix %q: %+v", text, got)
		}
	}
	t.Run("masked date", func(t *testing.T) {
		for _, tt := range []struct{ text, want string }{
			{"USD July 2026 7 days.", "7 days"}, {"USD July 2026 7 devices.", "7 devices"},
		} {
			got := Extract(tt.text)
			if len(got) != 1 || got[0].Text != tt.want || string([]rune(tt.text)[got[0].Start:got[0].End]) != tt.want {
				t.Errorf("%q: %+v", tt.text, got)
			}
		}
	})

	text := "From 1 July 2026 15 days are allowed."
	got := Extract(text)
	if len(got) != 1 {
		t.Fatalf("dated figure: %+v", got)
	}
	f := got[0]
	if f.Kind != "duration" || f.Value != "21600" || f.Unit != "minutes" || f.Text != "15 days" || f.Start != 17 || f.End != 24 || f.Clause != text || f.ClauseStart != 0 || f.ClauseEnd != 37 || f.Period != (Period{From: "2026-07-01"}) {
		t.Fatalf("dated figure lost source or period provenance: %+v", f)
	}
}

func TestExtractUncertainDateScope(t *testing.T) {
	if got := Extract("The date is 1 July 2026, give or take 2 days."); len(got) != 0 {
		t.Errorf("uncertain date: %+v", got)
	}
	text := "From 1 July 2026 the fee is $7 and from 1 August 2026, give or take 2 days, the fee is $8."
	got := Extract(text)
	if len(got) != 2 {
		t.Fatalf("figures: %+v", got)
	}
	for i, want := range []string{"$7", "$8"} {
		f := got[i]
		if f.Text != want || f.Period != (Period{}) || len(f.acceptedDates) != 1 || f.Clause[f.acceptedDates[0].start:f.acceptedDates[0].end] != "1 July 2026" || string([]rune(text)[f.Start:f.End]) != f.Text {
			t.Errorf("uncertain date scope: %+v", f)
		}
	}
}

func TestExtractRatesAndBoundContext(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"The fee is about $5 each 2 devices and the wait is 3 days.", []string{"3 days"}},
		{"The fee is about $5 a 2 devices and the wait is 3 days.", []string{"3 days"}},
		{"The fee is $5 each 2 devices maximum and the wait is 3 days.", []string{"3 days"}},
		{"The fee is $5 a 2 devices maximum and the wait is 3 days.", []string{"3 days"}},
		{"The fee is $5 each 2 devices and the wait is 3 days.", []string{"$5", "2 devices", "3 days"}},
		{"The fee is $5 a 2 devices and the wait is 3 days.", []string{"$5", "2 devices", "3 days"}},
		{"The fee is $5 each month and the wait is 3 days.", []string{"$5", "3 days"}},
		{"The fee is $5 eachother and the wait is 3 days.", []string{"$5", "3 days"}},
		{"The fees are about $5/month and $10/year.", nil},
		{"The fees are about $5 per 2 devices and $10 per 3 devices.", nil},
		{"The fees are $5 per 2 devices and $10 per 3 devices approximately.", nil},
		{"The fees are $5 per 2 devices and $10 per 3 devices approximately and the wait is 7 days.", []string{"7 days"}},
		{"The fees are about $5 per 2 devices and $10 per 3 devices and the wait is 7 days.", []string{"7 days"}},
		{"The fees are $5 per 2 devices and $10 per 3 devices.", []string{"$5", "2 devices", "$10", "3 devices"}},
		{"The fees are $5/month and $10/year.", []string{"$5", "$10"}},
		{"The fees are about $5 and $10 per 2 devices.", nil},
		{"The fees are about $5 and $10/2 devices.", nil},
		{"The fees are about $5 and $10 per 2 devices and the wait is 3 days.", []string{"3 days"}},
		{"Rate is about 2 devices per 5 days and the fee is $3. The wait is 7 days.", []string{"$3", "7 days"}},
		{"Rate is about 2 devices/5 days and the fee is $3. The wait is 7 days.", []string{"$3", "7 days"}},
		{"The fee is about $5 per 2 devices and the wait is 3 days. The fee is $7.", []string{"3 days", "$7"}},
		{"The fee is about $5/2 devices and the wait is 3 days. The fee is $7.", []string{"3 days", "$7"}},
		{"The adjustment is negative (at least $7). The separate fee is $3.", []string{"$3"}},
		{"The wait is 7 to (at most 10 days). The separate fee is $3.", []string{"$3"}},
		{"The fee is $5/month maximum. The separate fee is $3.", []string{"$3"}},
		{"The fee is $5/month approximately. The separate fee is $3.", []string{"$3"}},
		{"The fee is $5/2 devices maximum. The separate fee is $3.", []string{"$3"}},
		{"The fee is $5/2 devices approximately. The separate fee is $3.", []string{"$3"}},
		{"The wait is (7 days) less than 10 days. The separate fee is $3.", []string{"$3"}},
		{"The wait is ‘7 days’ less than 10 days. The separate fee is $3.", []string{"$3"}},
		{"The fee is $5/month maximum and the wait is 7 days.", []string{"7 days"}},
		{"The fee is $5/2 devices approximately and the wait is 7 days.", []string{"7 days"}},
		{"The fee is $5/month maximums apply.", []string{"$5"}},
		{"The fee is $5/month.", []string{"$5"}},
		{"The fee is $5/2 devices.", []string{"$5", "2 devices"}},
		{"The fee is $5 / 2-3 free devices.", []string{"$5", "2-3 free devices"}},
		{"The fee is $5 per 2 devices.", []string{"$5", "2 devices"}},
		{"The fee is $5 each month.", []string{"$5"}},
		{"The fee is $5 a month.", []string{"$5"}},
		{"The fee is $5 per 2 devices approximately and the wait is 3 days.", []string{"3 days"}},
		{"The fee is $5 per 2 devices maximum and the wait is 3 days.", []string{"3 days"}},
		{"The fee is $5 per month approximately and the wait is 3 days.", []string{"3 days"}},
		{"The fee is $5 per 2 percentage points approximately and the wait is 3 days.", []string{"3 days"}},
		{"The fee is $5/2 percentage points approximately and the wait is 3 days.", []string{"3 days"}},
		{"The fee is $5/2 per cent maximum and the wait is 3 days.", []string{"3 days"}},
		{"Estimated at a maximum of $7. The wait is 3 days.", []string{"3 days"}},
		{"Minus a minimum of $7. The wait is 3 days.", []string{"3 days"}},
		{"The fee is a maximum of $7.", []string{"$7"}},
		{"The fee is $5 per 2 devices and the wait is 3 days.", []string{"$5", "2 devices", "3 days"}},
		{"The fee is $5/2 percentage points and the wait is 3 days.", []string{"$5", "2 percentage points", "3 days"}},
		{"The fee is $5/2 per cent and the wait is 3 days.", []string{"$5", "2 per cent", "3 days"}},
		{"The fee is $5 per 2 devices approximatelyness and the wait is 3 days.", []string{"$5", "2 devices", "3 days"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, f := range got {
				if f.Text != tt.figures[i] || string([]rune(tt.text)[f.Start:f.End]) != f.Text || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause {
					t.Fatalf("figure lost source: %+v", f)
				}
			}
		})
	}
	{
		for _, tt := range []struct {
			text    string
			figures []struct {
				text, kind string
				start, end int
			}
		}{
			{"Rate is 2 devices/5 days.", []struct {
				text, kind string
				start, end int
			}{{"2 devices", "count", 8, 17}, {"5 days", "duration", 18, 24}}},
			{"Rate is 5 days/2 devices.", []struct {
				text, kind string
				start, end int
			}{{"5 days", "duration", 8, 14}, {"2 devices", "count", 15, 24}}},
			{"Rate is 5 percent/2 devices.", []struct {
				text, kind string
				start, end int
			}{{"5 percent", "percent", 8, 17}, {"2 devices", "count", 18, 27}}},
			{"Rate is 2 devices/5 users.", []struct {
				text, kind string
				start, end int
			}{{"2 devices", "count", 8, 17}, {"5 users", "count", 18, 25}}},
		} {
			t.Run(tt.text, func(t *testing.T) {
				got := Extract(tt.text)
				if len(got) != len(tt.figures) {
					t.Fatalf("rate: %+v", got)
				}
				for i, want := range tt.figures {
					f := got[i]
					if f.Text != want.text || f.Kind != want.kind || f.Start != want.start || f.End != want.end || f.Clause != tt.text || string([]rune(tt.text)[f.Start:f.End]) != want.text {
						t.Fatalf("figure: %+v", f)
					}
				}
			})
		}
	}
}

func TestExtractUncertainDateSuffix(t *testing.T) {
	for _, suffix := range []string{"or later", "or earlier"} {
		text := "The deadline is 1 July 2026 " + suffix + "."
		if got := Extract(text); len(got) != 0 {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, text := range []string{
		"The fee is $5 until 1 July 2026 or later.",
		"The fee is $5 from 1 July 2026 or earlier.",
		"From 1 July 2026 or later the fee is $5 and the deposit is $7.",
		"Until 1 July 2026 or earlier the fee is $5 and the deposit is $7.",
	} {
		got := Extract(text)
		wantCount := 1
		if strings.Contains(text, "deposit") {
			wantCount = 2
		}
		if len(got) != wantCount {
			t.Errorf("%q: %+v", text, got)
			continue
		}
		for i, f := range got {
			want := []string{"$5", "$7"}[i]
			if f.Text != want || f.Period != (Period{}) {
				t.Errorf("%q: %+v", text, f)
			}
		}
	}
	text := "The fee is $5 until 1 July 2026. From 1 August 2026 the deposit is $7."
	got := Extract(text)
	if len(got) != 2 || got[0].Text != "$5" || got[0].Period != (Period{To: "2026-07-01"}) || got[1].Text != "$7" || got[1].Period != (Period{From: "2026-08-01"}) {
		t.Errorf("independent precise dates: %+v", got)
	}
	text = "The fee is $5 until 1 July 2026 or later. From 1 August 2026 the deposit is $7."
	got = Extract(text)
	if len(got) != 2 || got[0].Text != "$5" || got[0].Period != (Period{}) || got[1].Text != "$7" || got[1].Period != (Period{From: "2026-08-01"}) {
		t.Errorf("independent clause: %+v", got)
	}
}

func TestExtractExplicitDateBoundaryModifiers(t *testing.T) {
	for _, text := range []string{
		"The fee is $5 until 1 July 2026 exclusive.",
		"From 1 July 2026 (exclusive) the fee is $5 and the deposit is $7.",
		"The fee is $5 until July 2026, exclusive.",
		"The fee is $5 before 1 July 2026 inclusive.",
	} {
		got := Extract(text)
		want := []string{"$5"}
		if strings.Contains(text, "deposit") {
			want = append(want, "$7")
		}
		if len(got) != len(want) {
			t.Errorf("%q: %+v", text, got)
			continue
		}
		for i, f := range got {
			if f.Text != want[i] || f.Period != (Period{}) {
				t.Errorf("%q: %+v", text, f)
			}
		}
	}
	text := "The fee is $5 until 1 July 2026 inclusive. From 1 August 2026 the deposit is $7."
	got := Extract(text)
	if len(got) != 2 || got[0].Text != "$5" || got[0].Period != (Period{}) || got[1].Text != "$7" || got[1].Period != (Period{From: "2026-08-01"}) {
		t.Errorf("inclusive modifier and independent clause: %+v", got)
	}
}

func TestExtractDenseClauseBudget(t *testing.T) {
	text := strings.Repeat("The fee is $1 and the wait is 2 days and ", 33) + "; The fee is $5."
	got := Extract(text)
	if len(got) != 1 || got[0].Text != "$5" || got[0].Clause != "The fee is $5." {
		t.Errorf("over budget: %d figures", len(got))
	}
	text = strings.Repeat("The fee is $1 and the wait is 2 days and ", 32)
	got = Extract(text)
	if len(got) != 64 || got[0].Text != "$1" || got[len(got)-1].Text != "2 days" {
		t.Errorf("within budget: %d figures", len(got))
	}
}

func TestExtractRejectsNumericGrouping(t *testing.T) {
	for _, text := range []string{
		"Wait is 1 000 days.", "Cost is 1 000 dollars.", "Cost is $1 000.",
		"Rate is 1\u202f000 percent.", "There are 1\t000 devices.",
	} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("grouped %q: %+v", text, got)
		}
	}

	for _, separator := range []string{"_", "'", "’", "‘", "ʼ"} {
		for _, text := range []string{"Cost is $1" + separator + "000.", "Cost is CHF 1" + separator + "000.", "Cost is 1" + separator + "000 dollars."} {
			if got := Extract(text); len(got) != 0 {
				t.Errorf("grouped %q: %+v", text, got)
			}
		}
	}
	for _, text := range []string{"Cost is '$7'.", "Cost is ‘$7’.", `Cost is "$7".`} {
		if got := Extract(text); len(got) != 1 || got[0].Text != "$7" {
			t.Errorf("quoted %q: %+v", text, got)
		}
	}
	t.Run("RejectsCommaSpaceGrouping", func(t *testing.T) {
		if got := Extract("The fee is $1, 500.50 dollars."); len(got) != 0 {
			t.Errorf("decimal grouping: %+v", got)
		}
		if got := Extract("The fee is $1, 500 dollars."); len(got) != 0 {
			t.Errorf("%+v", got)
		}
		if got := Extract("The fee is 1, 500 dollars."); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
		for _, tt := range []struct {
			text    string
			figures []string
		}{
			{"Fee is $1, 500 devices.", []string{"$1", "500 devices"}},
			{"The fees are $1, $500.", []string{"$1", "$500"}},
			{"On 1 July 2026, 500 devices apply.", []string{"500 devices"}},
		} {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Errorf("independent figure %q: %+v", tt.text, got)
				continue
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("independent figure %q: %+v", tt.text, got)
				}
			}
		}
	})
}

func TestExtractSentenceBoundaryAfterClosingPunctuation(t *testing.T) {
	for _, tt := range []struct{ text, want string }{{"“The date is 1 July 2026.”", "1 July 2026"}, {"“Policy: ABC1234.”", "Policy: ABC1234"}} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Text != tt.want || string([]rune(tt.text)[got[0].Start:got[0].End]) != tt.want {
			t.Errorf("%q: %+v", tt.text, got)
		}
	}
	for _, tt := range []struct {
		text    string
		figures []struct {
			quote, clause string
			period        Period
		}
	}{
		{`"The fee is $7." From 1 July 2026 the wait is 2 days.`, []struct {
			quote, clause string
			period        Period
		}{{"$7", `"The fee is $7."`, Period{}}, {"2 days", "From 1 July 2026 the wait is 2 days.", Period{From: "2026-07-01"}}}},
		{`(No fee applies.) The wait is 2 days.`, []struct {
			quote, clause string
			period        Period
		}{{"2 days", "The wait is 2 days.", Period{}}}},
	} {
		got := Extract(tt.text)
		if len(got) != len(tt.figures) {
			t.Errorf("%q: %+v", tt.text, got)
			continue
		}
		for i, want := range tt.figures {
			f := got[i]
			if f.Text != want.quote || f.Clause != want.clause || f.Period != want.period || string([]rune(tt.text)[f.Start:f.End]) != want.quote || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != want.clause {
				t.Errorf("%q: %+v", tt.text, f)
			}
		}
	}
}

func TestExtractIndependentPeriodScopes(t *testing.T) {
	dateText := func(f Figure) string {
		s, _ := normalizeClause(f.Clause)
		if len(f.acceptedDates) != 1 || f.acceptedDates[0].start < 0 || f.acceptedDates[0].end > len(s) || f.acceptedDates[0].start > f.acceptedDates[0].end {
			return ""
		}
		return s[f.acceptedDates[0].start:f.acceptedDates[0].end]
	}

	{
		text := "The fee is $7 but the office closes before 1 July 2026."
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) || dateText(got[0]) != "1 July 2026" {
			t.Errorf("independent date: %+v", got)
		}
	}

	for _, text := range []string{"As  of 1 July 2026 wait is 7 days.", "As\tof 1 July 2026 wait is 7 days."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Qualifier != "" || got[0].Period.From != "2026-07-01" {
			t.Errorf("phrase whitespace %q: %+v", text, got)
		}
	}
	for _, tt := range []struct{ date, value string }{
		{"1\tJuly\t2026", "2026-07-01"},
		{"July\t2026", "2026-07"},
		{"July\t1,\t2026", "2026-07-01"},
	} {
		text := "From " + tt.date + " the fee is $7."
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$7" || got[0].Period.From != tt.value || string([]rune(text)[got[0].Start:got[0].End]) != got[0].Text || got[0].Clause != text {
			t.Errorf("period date whitespace %q: %+v", text, got)
		}
	}
	eligibility := "The fee is $5 for customers registered before 1 July 2026."
	if got := Extract(eligibility); len(got) != 1 || got[0].Text != "$5" || got[0].Period != (Period{}) || string([]rune(eligibility)[got[0].Start:got[0].End]) != "$5" {
		t.Errorf("eligibility date scope: %+v", got)
	}
	eligibility = "The fee is $7 for orders before 1 July 2026."
	if got := Extract(eligibility); len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) || string([]rune(eligibility)[got[0].Start:got[0].End]) != "$7" {
		t.Errorf("eligibility date scope: %+v", got)
	}
	conditional := "A refund of $5 applies if ordered before 2026-01-01."
	if got := Extract(conditional); len(got) != 1 || got[0].Text != "$5" || got[0].Period != (Period{}) || string([]rune(conditional)[got[0].Start:got[0].End]) != "$5" {
		t.Errorf("conditional event date: %+v", got)
	}
	scopeText := "The fee is $7 and from 1 July 2026 the office is closed."
	if got := Extract(scopeText); len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) {
		t.Errorf("coordinated date scope: %+v", got)
	}

	for _, text := range []string{
		"The fee is $7 before 1 July 2026 or after 1 August 2026.",
		"Before 1 July 2026 or after 1 August 2026 the fee is $7.",
		"The fee is $7 after 1 August 2026 or before 1 July 2026.",
	} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) {
			t.Errorf("alternative periods %q: %+v", text, got)
		}
	}
	for _, tt := range []struct {
		text, first, second string
		period              Period
	}{
		{"The fee from 1 July 2026 is $7 and the deposit was $10.", "$7", "$10", Period{}},
		{"The fee is $5 on or before 1 July 2026 and the deposit is $7.", "$5", "$7", Period{}},
		{"From 1 July 2026 the fee is $7 and the deposit was $10.", "$7", "$10", Period{From: "2026-07-01"}},
		{"Effective  on 1 July 2026 the fee is $7 and the deposit was $10.", "$7", "$10", Period{From: "2026-07-01"}},
		{"The fee is $7 and the deposit was $10 from 1 July 2026.", "$7", "$10", Period{}},
		{"The wait from 1 July 2026 is 7 days and 3 hours.", "7 days", "3 hours", Period{From: "2026-07-01"}},
	} {
		got := Extract(tt.text)
		if len(got) != 2 || got[0].Text != tt.first || got[1].Text != tt.second || got[0].Period != tt.period || got[1].Period != tt.period {
			t.Errorf("assertion scope %q: %+v", tt.text, got)
		}
	}

	for _, text := range []string{
		"The fee is $7 effective on 1 July 2026.",
		"The fee is $7 starting on 1 July 2026.",
	} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{From: "2026-07-01"}) {
			t.Errorf("effective date %q: %+v", text, got)
		}
	}

	got := Extract("The fee is $7 effective on 1 July 2026 and the wait is 2 days.")
	if len(got) != 2 || got[0].Text != "$7" || got[1].Text != "2 days" || got[0].Period != (Period{}) || got[1].Period != (Period{}) {
		t.Errorf("independent effective date: %+v", got)
	}
	got = Extract("The fee is $7 from 1 July 2026 and the charge is $8 from 1 July 2026 and the wait is 2 days.")
	if len(got) != 3 {
		t.Fatalf("repeated dates: %+v", got)
	}
	for i, want := range []string{"$7", "$8", "2 days"} {
		if got[i].Text != want || got[i].Period != (Period{}) {
			t.Errorf("figure %d: %+v", i, got[i])
		}
	}
	got = Extract("The fee is $7 per 31 February 2026 days.")
	if len(got) != 2 || got[0].Text != "$7" || got[1].Text != "2026 days" || got[0].Period != (Period{}) || got[1].Period != (Period{}) {
		t.Errorf("overlapping rejected date: %+v", got)
	}

	textWithIdentifier := "From 1 July 2026 policy: 2026-02-30 applies and the fee is $5."
	identifierFigures := Extract(textWithIdentifier)
	if len(identifierFigures) != 2 || identifierFigures[0].Kind != "identifier" || identifierFigures[0].Text != "policy: 2026-02-30" || identifierFigures[1].Text != "$5" || identifierFigures[0].Period.From != "2026-07-01" || identifierFigures[1].Period.From != "2026-07-01" || len(identifierFigures[0].acceptedDates) != 1 || len(identifierFigures[1].acceptedDates) != 1 || dateText(identifierFigures[0]) != "1 July 2026" {
		t.Errorf("identifier date shape shared period: %+v", identifierFigures)
	}

	for _, text := range []string{
		"An introduction. From １ July 2026 the wait is about 2 days and from 31 February 2026 the fee is $7.",
		"From 1 July 2026 the wait is about 2 days and from 31 February 2026 the fee is $7.",
		"From 1 July 2026 the wait is about 2 days and from 1 August 2026/2027 the fee is $7.",
		"From 1 July 2026 the wait is about 2 days and from 1 July 2026/2027 the fee is $7.",
	} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) || len(got[0].acceptedDates) != 1 || dateText(got[0]) != "1 July 2026" {
			t.Errorf("rejected date scope and provenance %q: %+v", text, got)
		}
	}

	textWithApproximateDate := "From 1 July 2026 the fee is $7 and from about 1 August 2026 the fee is $8."
	figures := Extract(textWithApproximateDate)
	if len(figures) != 2 {
		t.Fatalf("approximate date scope: %+v", figures)
	}
	for _, f := range figures {
		if f.Period != (Period{}) || len(f.acceptedDates) != 1 || dateText(f) != "1 July 2026" {
			t.Errorf("approximate date scope and provenance: %+v", f)
		}
	}
	got = Extract("The wait is 7 days with less effort and the fee is $5 per 2 devices.")
	if len(got) != 3 || got[0].Text != "7 days" || got[1].Text != "$5" || got[2].Text != "2 devices" {
		t.Fatalf("independent figures: %+v", got)
	}
	for _, tt := range []struct{ text, end string }{
		{"The fee is $7 until 1 July 2026.", "2026-07-01"},
		{"The fee is $7 to 1 July 2026.", "2026-07-01"},
		{"The fee is $7 to 30 June 2026.", "2026-06-30"},
	} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Text != "$7" || got[0].Period.To != tt.end {
			t.Errorf("%q: %+v", tt.text, got)
		}
	}

	for _, s := range []string{"The notice is due (2 days) before 1 July 2026.", "The notice is due \"2 days\" prior to 1 July 2026.", "The notice period is 2 days before 1 July 2026.", "From 1 January 2026 the notice is 2 days before 1 July 2026.", "The notice is due 2 days prior to 1 July 2026.", "From 1 January 2026 the notice is due 2 days prior to 1 July 2026."} {
		got := Extract(s)
		if len(got) != 1 || got[0].Text != "2 days" || got[0].Period != (Period{}) {
			t.Errorf("%q: %+v", s, got)
			continue
		}
		if string([]rune(s)[got[0].Start:got[0].End]) != "2 days" {
			t.Errorf("bad span: %+v", got[0])
		}
	}

	for _, text := range []string{
		"The cooling-off period is 7 days from 1 July 2026 until 30 June 2027.",
		"The wait is 7 days from 1 July 2026 through 30 June 2027.",
	} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "7 days" || got[0].Period != (Period{From: "2026-07-01", To: "2027-06-30"}) || string([]rune(text)[got[0].Start:got[0].End]) != got[0].Text {
			t.Errorf("bounded duration %q: %+v", text, got)
		}
	}

	for _, text := range []string{"The deadline is 7 days from July 1, 2026.", "The notice is due 2 days from 1 July 2026."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Kind != "duration" || got[0].Period != (Period{}) {
			t.Errorf("relative date %q: %+v", text, got)
		}
	}

	text := "The wait is 7 days and 3 hours and 5 minutes from 1 July 2026."
	got = Extract(text)
	if len(got) != 3 {
		t.Fatalf("components: %+v", got)
	}
	for i, want := range []string{"7 days", "3 hours", "5 minutes"} {
		if got[i].Text != want || got[i].Period.From != "2026-07-01" || string([]rune(text)[got[i].Start:got[i].End]) != want {
			t.Errorf("component %d: %+v", i, got[i])
		}
	}

	for _, text := range []string{"The wait is about 2 days from 1 July 2026 and the fee is $7.", "The fee is $7 and the wait is about 2 days from 1 July 2026."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, text := range []string{"The fee is $7 and notice is about 2 days before 1 July 2026.", "From 1 January 2026 the fee is $7 and notice is about 2 days before 1 July 2026."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"From 1 July 2026 the wait is about 2 days and the fee is $7.", []string{"$7"}},
		{"From 1 July 2026 the fee is $5 per 2 devices and the wait is about 3 days.", []string{"$5", "2 devices"}},
	} {
		got := Extract(tt.text)
		if len(got) != len(tt.figures) {
			t.Errorf("shared prefix %q: %+v", tt.text, got)
			continue
		}
		for i, want := range tt.figures {
			if got[i].Text != want || got[i].Period != (Period{From: "2026-07-01"}) {
				t.Errorf("shared prefix %q: %+v", tt.text, got)
			}
		}
	}
	text = "The fee is $7 from 1 July 2026 and the wait is 2 days until 30 June 2027."
	got = Extract(text)
	if len(got) != 2 || got[0].Text != "$7" || got[1].Text != "2 days" || got[0].Period != (Period{}) || got[1].Period != (Period{}) {
		t.Fatalf("%+v", got)
	}
	got = Extract("The fee is $7 from 1 July 2026 and the wait is 2 days.")
	if len(got) != 2 || got[0].Period != (Period{}) || got[1].Period != (Period{}) {
		t.Fatalf("single scoped bound: %+v", got)
	}
	got = Extract("The fee is $7 and the wait is 2 days until 30 June 2027.")
	if len(got) != 2 || got[0].Period != (Period{}) || got[1].Period != (Period{}) {
		t.Fatalf("trailing scoped bound: %+v", got)
	}
	for _, text := range []string{"From 1 July 2026 the fee is $5/2 devices.", "The fee is $5/2 devices from 1 July 2026.", "From 1 July 2026 the wait is 7 days and 3 hours.", "The wait is 7 days and 3 hours from 1 July 2026."} {
		got := Extract(text)
		if len(got) != 2 || got[0].Period.From != "2026-07-01" || got[1].Period.From != "2026-07-01" {
			t.Errorf("shared period %q: %+v", text, got)
		}
	}
	t.Run("RejectsConflictingPeriodBounds", func(t *testing.T) {
		for _, clause := range []string{
			"From 1 July 2024 the fee is $7 and from 1 July 2026 the fee is $8.",
			"The fee is $7 until 1 July 2024 and the fee is $8 until 1 July 2026.",
		} {
			text := "The wait is 2 days. " + clause + " The fee is $9."
			got := Extract(text)
			if len(got) != 2 || got[0].Text != "2 days" || got[1].Text != "$9" {
				t.Fatalf("conflicting period bounds retained: %+v", got)
			}
		}
		got := Extract("From 1 July 2026 the fee is $7 and from 1 July 2026 it is $8.")
		if len(got) != 2 || got[0].Period.From != "2026-07-01" || got[1].Period.From != "2026-07-01" {
			t.Fatalf("repeated equal period bounds: %+v", got)
		}
	})
	t.Run("AlternativeDateBounds", func(t *testing.T) {
		if got := Extract("From 1 July 2026 or about 1 August 2026 the fee is $7. The wait is 2 days."); len(got) != 1 || got[0].Text != "2 days" || got[0].Period != (Period{}) {
			t.Errorf("approximate alternative scope: %+v", got)
		}
		for _, text := range []string{"From 1 July 2026 or (1 August 2026) the fee is $7.", "From 1 July 2026 or 1 August 2026 approximately the fee is $7.", "From 1 July 2026 or 1 August 2026 the fee is $7.", "The fee is $7 from 1 July 2026 or 1 August 2026.", "Until 1 July 2026 or 1 August 2026 the fee is $7.", "The fee is $7 until 1 July 2026 or 1 August 2026."} {
			got := Extract(text)
			if len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) || string([]rune(text)[got[0].Start:got[0].End]) != "$7" {
				t.Errorf("alternative bounds %q: %+v", text, got)
			}
		}
	})
	t.Run("ExclusivePeriodEnd", func(t *testing.T) {
		{
			for _, tt := range []struct {
				text, end string
				figures   []string
			}{
				{"The fee is $5 on or before 1 July 2026.", "2026-07-01", []string{"$5"}},
				{"On or before 1 July 2026 the fee is $5 and the deposit is $7.", "2026-07-01", []string{"$5", "$7"}},
				{"The fee is $5 on or before July 2026.", "2026-07", []string{"$5"}},
				{"On or before July 2026 the fee is $5 and the deposit is $7.", "2026-07", []string{"$5", "$7"}},
				{"Before 1 July 2026 the fee is $5 and the deposit is $7.", "2026-06-30", []string{"$5", "$7"}},
				{"Prior to 1 July 2026 the fee is $7 and the deposit is $10.", "2026-06-30", []string{"$7", "$10"}},
				{"Before July 2026 the fee is $5 and the deposit is $7.", "2026-06-30", []string{"$5", "$7"}},
			} {
				got := Extract(tt.text)
				if len(got) != len(tt.figures) {
					t.Errorf("%q: %+v", tt.text, got)
					continue
				}
				for i, f := range got {
					if f.Text != tt.figures[i] || f.Period != (Period{To: tt.end}) {
						t.Errorf("%q: %+v", tt.text, f)
					}
				}
			}
		}

		for _, tt := range []struct{ text, end string }{
			{"The fee is $7 before 1 July 2026.", "2026-06-30"},
			{"The fee is $7 prior to 1 July 2026.", "2026-06-30"},
			{"The fee is $7 through 1 July 2026.", "2026-07-01"},
			{"The fee is $7 before March 2024.", "2024-02-29"},
		} {
			got := Extract(tt.text)
			if len(got) != 1 || got[0].Period.To != tt.end || string([]rune(tt.text)[got[0].Start:got[0].End]) != "$7" {
				t.Errorf("%q: %+v", tt.text, got)
			}
		}
		for _, text := range []string{"From 1 January 0001 the fee is $7 before January 0001.", "From 1 July 2026 the fee is $7 before 1 July 2026."} {
			got := Extract(text)
			if len(got) != 1 || got[0].Period != (Period{}) {
				t.Errorf("unrepresentable period %q: %+v", text, got)
			}
		}
	})
	t.Run("MixedPrecisionPeriodBounds", func(t *testing.T) {
		for _, tt := range []struct {
			text string
			want Period
		}{
			{"From 1 July 2026 until July 2026 the fee is $5.", Period{From: "2026-07-01", To: "2026-07"}},
			{"From 31 July 2026 until July 2026 the fee is $5.", Period{From: "2026-07-31", To: "2026-07"}},
			{"From 1 August 2026 until July 2026 the fee is $5.", Period{}},
			{"From July 2026 until 1 July 2026 the fee is $5.", Period{From: "2026-07", To: "2026-07-01"}},
		} {
			got := Extract(tt.text)
			if len(got) != 1 || got[0].Text != "$5" || got[0].Period != tt.want {
				t.Errorf("%q: %+v", tt.text, got)
			}
		}
	})
	{
		for _, tt := range []struct {
			text string
			want Period
		}{
			{"The fee is $7 for customers registered before 1 July 2026.", Period{}},
			{"The fee is $7 for orders placed before 1 July 2026.", Period{}},
			{"The office closes before 1 July 2026 and the fee is $7.", Period{}},
			{"The office closes before 1 July 2026, and the fee is $7.", Period{}},
			{"The office closes before 1 July 2026, while the fee is $7.", Period{}},
			{"The fee is $7 while the office closes before 1 July 2026.", Period{}},
			{"Before 1 July 2026, the fee is $7.", Period{To: "2026-06-30"}},
			{"The fee is $7 and the office closes before 1 July 2026.", Period{}},
			{"The deadline is before July 2026 and the fee is $7.", Period{}},
			{"The deadline will be before July 2026 and the fee is $7.", Period{}},
			{"The fee is $7 and the deadline will be before July 2026.", Period{}},
			{"The fee is $7 and the deadline is before July 2026.", Period{}},
			{"Before July 2026 the fee is $7.", Period{To: "2026-06-30"}},
			{"The fee is $7 before July 2026.", Period{To: "2026-06-30"}},
		} {
			t.Run(tt.text, func(t *testing.T) {
				got := Extract(tt.text)
				if len(got) != 1 || got[0].Text != "$7" || got[0].Period != tt.want {
					t.Fatalf("got %+v, want fee with period %+v", got, tt.want)
				}
			})
		}
	}

	{
		for _, tt := range []struct {
			text  string
			want  Period
			count int
		}{
			{"The fee is $5 starting 1 July 2026 and ending 30 June 2027.", Period{From: "2026-07-01", To: "2027-06-30"}, 1},
			{"The fee is $5 beginning on 1 July 2026 and ending 30 June 2027.", Period{From: "2026-07-01", To: "2027-06-30"}, 1},
			{"Starting 1 July 2026 and ending 30 June 2027, the fee is $5 and the wait is 2 days.", Period{From: "2026-07-01", To: "2027-06-30"}, 2},
			{"The fee is $5 and ending 30 June 2027 the office closes.", Period{}, 1},
			{"The fee is $5 starting 1 July 2026 and ending 30 June 2027 the office closes.", Period{}, 1},
			{"The fee is $5 starting 1 July 2026 and the office closes ending 30 June 2027.", Period{}, 1},
			{"The fee is $5 starting 1 July 2026 and ending 30 June 2027 the wait is 2 days.", Period{}, 2},
			{"Beginning on 1 July 2026, the fee is $5.", Period{From: "2026-07-01"}, 1},
			{"The fee is $5 beginning on 1 July 2026.", Period{From: "2026-07-01"}, 1},
			{"Beginning on 1 July 2026, the fee is $5 and the wait is 2 days.", Period{From: "2026-07-01"}, 2},
			{"The fee is $5 beginning on 1 July 2026 and the wait is 2 days.", Period{}, 2},
			{"Beginning on 1 July 2026 and until 31 July 2026, the fee is $5.", Period{From: "2026-07-01", To: "2026-07-31"}, 1},
		} {
			t.Run(tt.text, func(t *testing.T) {
				got := Extract(tt.text)
				if len(got) != tt.count || got[0].Text != "$5" {
					t.Fatalf("got %+v, want %d figures beginning with $5", got, tt.count)
				}
				for _, f := range got {
					if f.Period != tt.want || string([]rune(tt.text)[f.Start:f.End]) != f.Text {
						t.Errorf("got %+v, want period %+v with original span", f, tt.want)
					}
				}
			})
		}
	}

	{
		for _, connector := range []string{"effective", "effective on", "as of", "starting", "starting on", "beginning", "beginning on", "from", "since", "until", "till", "to", "through", "ending", "before", "prior to"} {
			text := "The fee is $7 and the contract is " + connector + " 1 July 2026."
			got := Extract(text)
			if len(got) != 1 || got[0].Text != "$7" || got[0].Period != (Period{}) {
				t.Errorf("%q: %+v", text, got)
			}
		}
	}
}

func TestExtractRejectsPostfixBounds(t *testing.T) {
	{
		got := Extract("The wait is 7 days max. The fee is $3.")
		if len(got) != 1 || got[0].Text != "$3" {
			t.Errorf("dotted postfix bound and independent sentence: %+v", got)
		}
	}

	t.Run("punctuated postfix bounds", func(t *testing.T) {
		for _, figure := range []string{"7 days", "$7", "7 percent", "7 devices"} {
			for _, text := range []string{"The value is (" + figure + ") maximum.", "The value is " + figure + " (maximum).", "The value is " + figure + ", max."} {
				if got := Extract(text); len(got) != 0 {
					t.Errorf("%q: %+v", text, got)
				}
			}
		}
	})

	for _, text := range []string{"The fee is $5 and up.", "The delay is 7 days and above.", "The wait is 7 days and over.", "The fee is $7 and over."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, figure := range []string{"7 days", "$7", "7 percent", "7 devices"} {
		for _, bound := range []string{"maximum", "minimum", "max", "at most", "at  most", "at\tmost", "at least", "no more than", "no less than", "at a maximum", "at the maximum", "at minimum", "at maximum", "(at minimum)", ", at maximum", "a maximum", "the minimum", "upwards", "and up", "and upwards", "and above", "and over", "and\tover"} {
			text := "Value is " + figure + " " + bound + "."
			if got := Extract(text); len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
	}
	for _, text := range []string{"The wait is 7 days upwards and the fee is $3.", "The wait is 7 days and upwards. The fee is $3.", "The wait is 7 days and upwards and the fee is $3."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$3" {
			t.Errorf("independent figure after bound %q: %+v", text, got)
		}
	}
	if got := Extract("The wait is 7 days upwardsness continues."); len(got) != 1 || got[0].Text != "7 days" {
		t.Errorf("independent prose: %+v", got)
	}
	if got := Extract("The wait is 7 days and upward trends continue."); len(got) != 1 || got[0].Text != "7 days" {
		t.Errorf("independent prose: %+v", got)
	}
	got := Extract("The wait is 7 days and over. The fee is $3.")
	if len(got) != 1 || got[0].Text != "$3" {
		t.Fatalf("independent sentence after bound: %+v", got)
	}
	got = Extract("The wait is 7 days and the maximum fee is $3.")
	if len(got) != 1 || got[0].Text != "7 days" {
		t.Fatalf("independent wait with bounded fee: %+v", got)
	}
	got = Extract("The wait is 7 days over summer.")
	if len(got) != 1 || got[0].Text != "7 days" {
		t.Fatalf("unrelated preposition: %+v", got)
	}
	t.Run("CompoundDurationBounds", func(t *testing.T) {
		for _, s := range []string{
			"The service period is a minimum of 7 days and 3 hours.",
			"The service period is up to 7 days and 3 hours.",
			"The service period is at least 7 days and 3 months.",
			"The service period is 7 days and 3 hours maximum.",
			"The service period is 7 days at  most, and 3 hours.",
			"The service period is 7 days at\tmost, and 3 hours.",
			"The service period is 7 days maximum, and 3 hours.",
			"The service period is 7 days at a maximum and 3 hours.",
			"The service period is 7 days and 3 hours at the maximum.",
		} {
			if got := Extract(s); len(got) != 0 {
				t.Errorf("%q: %+v", s, got)
			}
		}
		s := "The wait is up to 7 days and 3 hours, and the fee is $5 per 2 devices."
		got := Extract(s)
		if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 devices" {
			t.Fatalf("%+v", got)
		}
		r := []rune(s)
		for _, f := range got {
			if string(r[f.Start:f.End]) != f.Text {
				t.Fatalf("bad span: %+v", f)
			}
		}
		s = "The wait is 7 days maximum, and 3 hours, and the fee is $5 per 2 devices."
		got = Extract(s)
		if len(got) != 2 || got[0].Text != "$5" || got[1].Text != "2 devices" {
			t.Fatalf("%+v", got)
		}
		r = []rune(s)
		for _, f := range got {
			if string(r[f.Start:f.End]) != f.Text {
				t.Fatalf("bad span: %+v", f)
			}
		}
	})
}

func TestExtractCalendarQualifiedDurations(t *testing.T) {
	for _, text := range []string{"The wait is 7 days excluding (the 3 hours of setup).", `The wait is 7 days except for "the 3 hours of setup".`, "The wait is 7 days excluding 3 hours.", "The wait is 7 days except for 3 hours.", "The wait excludes time, excluding the 3 hours."} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("numeric exclusion %q: %+v", text, got)
		}
	}
	for _, day := range []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"} {
		for _, suffix := range []string{"", "s"} {
			text := "Wait is 7 days excluding " + day + suffix + "."
			if got := Extract(text); len(got) != 0 {
				t.Errorf("%q: %+v", text, got)
			}
		}
	}
	for _, text := range []string{
		"The delivery time is 5 days other than weekends.", "The delivery time is 5 days excepting weekends.",
		"The wait is 7 days and 3 hours other than weekends.", "The wait is 7 days excepting weekends and 3 hours.",
		"The wait is (7 days) excluding weekends.", `The wait is "7 days" excluding weekends.`,
		"The wait is 7 days excluding the weekend.", "The wait is 7 days excluding Christmas.",
		"The wait is 7 days excluding the winter holiday and 3 hours.",
		"The cooling-off period is 7 days excluding weekends.",
		"The cancellation period is 7 days excluding Saturday and Sunday.",
		"The wait is 7 days excluding Sundays.", "The wait is 7 days, except holidays.",
		"The wait is 7 days (excluding Mondays) and 3 hours.",
		"The wait is 7 days and 3 hours, except Fridays.",
		"The wait is 7 days excluding holidays.",
		"The wait is 7 days excluding public holidays.",
		"The wait is 7 days and 3 hours excluding weekends and holidays.",
		"The wait is 7 days excluding weekends and 3 hours.",
		"The wait is 7 days excluding\nweekends and 3 hours.",
		"The cooling period is 7 days less weekends.",
		"The wait is 7 days less weekends and 3 hours.",
	} {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, tt := range []struct {
		text string
		want []string
	}{
		{"The wait is 7 days other than weekends and the fee is $5 per 2 devices.", []string{"$5", "2 devices"}},
		{"The wait is 7 days excepting weekends and the fee is $5 per 2 devices.", []string{"$5", "2 devices"}},
		{"The fee is $20 excluding $3 and 5 percent.", []string{"$20"}},
		{"The wait is 7 days less weekends and the fee is $5 per 2 devices.", []string{"$5", "2 devices"}},
		{"The wait is 7 days excluding setup lasting 3 hours, and the fee is $5.", []string{"$5"}},
		{"The wait is 7 days excluding (the 3 hours of setup) and the fee is $5 per 2 devices.", []string{"$5", "2 devices"}},
		{"The wait is 2 days excluding Christmas before 1 July 2026 and the fee is $5 per 2 devices.", []string{"$5", "2 devices"}},
		{"The wait is 7 days excluding weekends and the fee is $5 per 2 devices.", []string{"$5", "2 devices"}},
		{"The wait is 2 days excluding holidays before 1 July 2026 and the fee is $5.", []string{"$5"}},
		{"The fee is $5 and notice is 2 days excluding holidays before 1 July 2026.", []string{"$5"}},
		{"The fee is $5 and notice is 2 days, except Sundays, before 1 July 2026.", []string{"$5"}},
	} {
		text := tt.text
		got := Extract(text)
		if len(got) != len(tt.want) {
			t.Errorf("%q: %+v", text, got)
			continue
		}
		for i, f := range got {
			if f.Text != tt.want[i] || f.Period != (Period{}) || string([]rune(text)[f.Start:f.End]) != f.Text {
				t.Errorf("figure and source span: %+v", f)
			}
		}
	}
	for _, tt := range []struct{ text, figure, unit, value string }{
		{"Wait is 7 business days.", "7 business days", "business days", "7"},
		{"Wait is 7 working days.", "7 working days", "business days", "7"},
		{"Wait is 7 calendar days.", "7 calendar days", "minutes", "10080"},
		{"Wait is 7 days including weekends.", "7 days", "minutes", "10080"},
	} {
		got := Extract(tt.text)
		if len(got) != 1 || got[0].Kind != "duration" || got[0].Text != tt.figure || got[0].Unit != tt.unit || got[0].Value != tt.value || string([]rune(tt.text)[got[0].Start:got[0].End]) != tt.figure {
			t.Errorf("%q: %+v", tt.text, got)
		}
	}
	got := Extract("Wait is 7 days excluding\nweekends and 3 hours. Fee is $5.")
	if len(got) != 1 || got[0].Text != "$5" {
		t.Errorf("independent clause: %+v", got)
	}
	{
		for _, text := range []string{
			"The processing period is 7 days apart from holidays.",
			"The processing period is 7 days and 3 hours apart from holidays.",
			"The processing period is 7 days apart from holidays and 3 hours.",
			"The processing period is 7 days apart from the winter holiday and 3 hours.",
		} {
			got := Extract(text + " The fee is $5.")
			if len(got) != 1 || got[0].Text != "$5" {
				t.Errorf("%q: %+v", text, got)
			}
		}
		for _, text := range []string{
			"The processing period is 7 days other than the fee of $5.",
			"The processing period is 7 days apart from the fee of $5.",
			"The processing period is 7 days apart fromholidays.",
		} {
			got := Extract(text)
			if len(got) == 0 || got[0].Text != "7 days" {
				t.Errorf("independent or word boundary %q: %+v", text, got)
			}
		}
	}
}

func TestExtractRejectsUnquantifiedCompoundComponents(t *testing.T) {
	for _, text := range []string{
		"The wait is 7 days and a few hours.",
		"The fee is $5 and some cents.",
		"The wait is 7 days, a few hours.",
		"The fee is $5, some cents.",
	} {
		if got := Extract(text + " The separate fee is $3."); len(got) != 1 || got[0].Text != "$3" {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"The wait is 7 days and a few hours are free.", []string{"7 days"}},
		{"The fee is $5 and some cents are refunded.", []string{"$5"}},
		{"The fee is $5, some cents are refunded.", []string{"$5"}},
		{"The wait is 7 days, a few hours are reserved.", []string{"7 days"}},
		{"The wait is 7 days and a few hourly updates follow.", []string{"7 days"}},
		{"The wait is 7 days and some users receive $3.", []string{"7 days", "$3"}},
	} {
		got := Extract(tt.text)
		if len(got) != len(tt.figures) {
			t.Errorf("independent statement %q: %+v", tt.text, got)
			continue
		}
		for i, want := range tt.figures {
			if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
				t.Errorf("independent statement %q: %+v", tt.text, got)
			}
		}
	}
}

func TestExtractInvalidExplicitIdentifierToken(t *testing.T) {
	for _, text := range []string{
		"Policy number: $ 1234. The fee is $5.",
		"Policy number: USD 1234. The fee is $5.",
		"Policy number: $ 1234 devices. The fee is $5.",
		"Policy number: USD 1234 devices. The fee is $5.",
		"Policy number: $1234. The fee is $5.",
		"Account ID is €1234. The fee is $5.",
		"Policy No. is $1234. The fee is $5.",
	} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$5" || got[0].Clause != "The fee is $5." || string([]rune(text)[got[0].ClauseStart:got[0].ClauseEnd]) != got[0].Clause {
			t.Errorf("invalid identifier %q: %+v", text, got)
		}
	}
	got := Extract("Policy number: ABC123 and the fee is $5.")
	if len(got) != 2 || got[0].Text != "Policy number: ABC123" || got[1].Text != "$5" {
		t.Errorf("independent identifier: %+v", got)
	}
	for _, text := range []string{"Policy: $5.", "The policy fee is $5."} {
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$5" {
			t.Errorf("independent money %q: %+v", text, got)
		}
	}
}

func TestExtractPercentageOfAmount(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"The fee is 5% of $100.", []string{"5%"}},
		{"The fee is 5% on $100.", []string{"5%"}},
		{"The fee is 5% on the $100 and the charge is $7.", []string{"5%", "$7"}},
		{"The fee is 5% on a $100 purchase.", []string{"5%"}},
		{"The fee is 5% on an $100 purchase.", []string{"5%"}},
		{"The fee is 5% on revenue and the charge is $100.", []string{"5%", "$100"}},
		{"The fee is 5 percent of $200 and the wait is 3 days.", []string{"5 percent", "3 days"}},
		{"The fee is 5% of the $100 and the charge is $7.", []string{"5%", "$7"}},
		{"The discount is 50% of a $100 purchase and the fee is $7.", []string{"50%", "$7"}},
		{"The discount is 50% of an $100 purchase.", []string{"50%"}},
		{"The fee is 5% of a purchase and the charge is $7.", []string{"5%", "$7"}},
		{"The discount covers 5% of 100 devices and the wait is 3 days.", []string{"5%", "3 days"}},
		{"The fee is 5% of revenue and the charge is $100.", []string{"5%", "$100"}},
		{"The fee is 5% and the cost of service is $100.", []string{"5%", "$100"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, f := range got {
				if f.Text != tt.figures[i] || string([]rune(tt.text)[f.Start:f.End]) != f.Text {
					t.Fatalf("unexpected figure: %+v", f)
				}
			}
		})
	}
}

func TestExtractReviewScopeRegressions(t *testing.T) {
	for _, tt := range []struct {
		text          string
		figures       []string
		unknownPeriod bool
	}{
		{"The fee is $5 per 2 days and 3 hours approximately and the wait is 7 days.", []string{"7 days"}, false},
		{"Policy number: ABCD EUR1234 applies. The fee is $5.", []string{"$5"}, false},
		{"Policy: AB1234 ABCD USD7. The fee is $5.", []string{"$5"}, false},
		{"Policy number: ABCD 2026-07-01 applies. The fee is $5.", []string{"$5"}, false},
		{"\"The estimated fee is $5 and the wait is 7 days.\"", []string{"7 days"}, false},
		{"An estimated fee is $5 and the wait is 7 days.", []string{"7 days"}, false},
		{"The fee is $7 and enrollment before 1 July 2026 is required.", []string{"$7"}, true},
		{"The extension is 5% of (7 days) and the fee is $5.", []string{"5%", "$5"}, false},
		{"The extension is 5% of 7 days and 3 hours and the fee is $5.", []string{"5%", "$5"}, false},
		{"One in ten devices fails. The fee is $5.", []string{"$5"}, false},
		{"1 in 10 devices fails. The fee is $5.", []string{"$5"}, false},
		{"Policy number: ABCD EUR1234 and the fee is $5.", []string{"$5"}, false},
		{"Policy: AB1234 ABCD USD7 and the fee is $5.", []string{"$5"}, false},
		{"The fee is $5 per 2 days and 3 hours and the wait is 7 days.", []string{"$5", "2 days", "3 hours", "7 days"}, false},
		{"The extension is 5% of [7 days and 3 hours] and the fee is $5.", []string{"5%", "$5"}, false},
		{"[A estimated fee is $5] and the wait is 7 days.", []string{"7 days"}, false},
		{"There are 10 devices in 3 days.", []string{"10 devices", "3 days"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknownPeriod && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewBoundaryRegressions(t *testing.T) {
	for _, tt := range []struct {
		text          string
		figures       []string
		unknownPeriod bool
	}{
		{"The fee is $5 per 2 days approximately and 3 hours and the wait is 7 days.", []string{"7 days"}, false},
		{"The fee is $5 per 2 days excluding weekends and the wait is 7 days.", []string{"7 days"}, false},
		{"Customers registered before 1 July 2026 pay $5.", []string{"$5"}, true},
		{"Before 1 July 2026 the fee is $5.", []string{"$5"}, false},
		{"The wait is about [7 days] and [3 hours] and the fee is $5.", []string{"$5"}, false},
		{"The wait is [7 days] and [3 hours] and the fee is $5.", []string{"7 days", "3 hours", "$5"}, false},
		{"Policy number: ABCD 1 July 2026 applies.", nil, false},
		{"Policy number: ABCD 1 July 2026 applies and the fee is $5.", []string{"$5"}, false},
		{"Policy number: ABC123 and the fee is $5.", []string{"Policy number: ABC123", "$5"}, false},
		{"The wait is (7 days), give or take 1 day and the fee is $5.", []string{"$5"}, false},
		{"The wait is (7 days) and the fee is $5.", []string{"7 days", "$5"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknownPeriod && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewFurtherBoundaries(t *testing.T) {
	for _, tt := range []struct {
		text          string
		figures       []string
		unknownPeriod bool
	}{
		{"The fee is 5% of $100 and $200 and the charge is $7.", []string{"5%", "$7"}, false},
		{"The fee is 5% of 100 devices and 200 devices and the charge is $7.", []string{"5%", "$7"}, false},
		{"The fee is 5% of $100 and the charge is $200.", []string{"5%", "$200"}, false},
		{"Policy number: AB1234 A EUR5678 applies.", nil, false},
		{"Policy number: AB1234 A 1 July 2026 applies.", nil, false},
		{"Policy number: ABCD 1234 dollars.", nil, false},
		{"Policy number: AB1234 and the fee is $5.", []string{"Policy number: AB1234", "$5"}, false},
		{"If you order before 1 July 2026 the fee is $5.", []string{"$5"}, true},
		{"Customers who join before 1 July 2026 pay $5.", []string{"$5"}, true},
		{"From 1 July 2026 the fee is $7 and from 1 August 2026,2027 the fee is $8.", []string{"$7", "$8"}, true},
		{"From 1 July 2026 the estimated fee is $5 and the wait is 7 days.", []string{"7 days"}, false},
		{"The deadlines are about 1 July 2026 and 1 August 2026.", nil, false},
		{"The deadlines are 1 July 2026 and 1 August 2026 approximately.", nil, false},
		{"The deadlines are 1 July 2026 and 1 August 2026.", []string{"1 July 2026", "1 August 2026"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if tt.text == "The deadlines are 1 July 2026 and 1 August 2026." && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknownPeriod && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewAdditionalBoundaries(t *testing.T) {
	for _, tt := range []struct {
		text          string
		figures       []string
		unknownPeriod bool
	}{
		{"From 1 July 2026, the estimated fee is $5 and the wait is 7 days.", []string{"7 days"}, false},
		{"From 1 July 2026, the fee is $5 and the wait is 7 days.", []string{"$5", "7 days"}, false},
		{"The fee is $5 per (2 days) and the deposit is $7.", []string{"$5", "2 days", "$7"}, false},
		{"The fee is $5 per (2 days approximately) and the deposit is $7.", []string{"$7"}, false},
		{"The wait is 7 days, give or take (1 day) and the fee is $5.", []string{"$5"}, false},
		{"Customers registered since 1 July 2026 pay $5.", []string{"$5"}, true},
		{"Customers registered from 1 July 2026 pay $5.", []string{"$5"}, true},
		{"The fee is $5 per 2 devices and 3 users approximately and the deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 per 2 devices and 3 users and the deposit is $7.", []string{"$5", "2 devices", "3 users", "$7"}, false},
		{"Policy number: ABCD 1234 devices.", nil, false},
		{"Policy number: ABCD 1234 days.", nil, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknownPeriod && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewRatesEligibilitySeparators(t *testing.T) {
	for _, tt := range []struct {
		text          string
		figures       []string
		unknownPeriod bool
	}{
		{"The fee is $5 per 2 percentage points and 3 percentage points approximately and the deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 per 2 percentage points and 3 percentage points and the deposit is $7.", []string{"$5", "2 percentage points", "3 percentage points", "$7"}, false},
		{"Customers registered on or before 1 July 2026 pay $5.", []string{"$5"}, true},
		{"Customers joining before 1 July 2026 pay $5.", []string{"$5"}, true},
		{"Orders made before 1 July 2026 cost $5.", []string{"$5"}, true},
		{"The fee is $1,,000. The deposit is $7.", []string{"$7"}, false},
		{"The fee is $1, and the deposit is $7.", []string{"$1", "$7"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknownPeriod && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewQualifiedToleranceAndOwnership(t *testing.T) {
	for _, tt := range []struct {
		text          string
		figures       []string
		unknownPeriod bool
	}{
		{"For orders before 1 July 2026 the fee is $5.", []string{"$5"}, true},
		{"For customers since 1 July 2026 the fee is $5.", []string{"$5"}, true},
		{"The wait is 7 days, give or take at most 1 day and the fee is $5.", []string{"$5"}, false},
		{"The wait is at most 1 day and the fee is $5.", []string{"1 day", "$5"}, false},
		{"The wait is 7 days excluding weekends but the fee is $5.", []string{"$5"}, false},
		{"The wait is 7 days excluding weekends while the fee is $5.", []string{"$5"}, false},
		{"Policy number: AB EUR1234 applies. The fee is $5.", []string{"$5"}, false},
		{"Policy number: AB 1 July 2026 applies. The fee is $5.", []string{"$5"}, false},
		{"Policy number: AB and the fee is $5.", []string{"$5"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknownPeriod && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewNominalAndQualifiedFigures(t *testing.T) {
	for _, tt := range []struct {
		text          string
		figures       []string
		unknownPeriod bool
	}{
		{"Orders before 1 July 2026 cost $5.", []string{"$5"}, true},
		{"The fee is $5 per \"2 days approximately\" and the deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 per \"2 days\" and the deposit is $7.", []string{"$5", "2 days", "$7"}, false},
		{"The fee is (5%) of $100 and the deposit is $7.", []string{"5%", "$7"}, false},
		{"Only 5 of 10 devices are included. The fee is $7.", []string{"$7"}, false},
		{"One of ten devices fails. The fee is $7.", []string{"$7"}, false},
		{"A purchase of 10 devices is included.", []string{"10 devices"}, false},
		{"The wait is 7 days and at least 2 devices are included.", []string{"7 days", "2 devices"}, false},
		{"The wait is 7 to at most 10 days. The fee is $7.", []string{"$7"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknownPeriod && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
			if tt.text == "The wait is 7 days and at least 2 devices are included." && got[1].Qualifier != "≥" {
				t.Errorf("missing bound: %+v", got[1])
			}
		})
	}
}

func TestExtractReviewFurtherScopeCases(t *testing.T) {
	for _, tt := range []struct {
		text      string
		figures   []string
		period    Period
		qualifier string
	}{
		{"The fee is $5 per almost exactly 2 days and the deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The fee is $5 per 2 days and the deposit is $7.", []string{"$5", "2 days", "$7"}, Period{}, ""},
		{"The plan includes 2 devices and at least 3 users.", []string{"2 devices", "3 users"}, Period{}, "≥"},
		{"The wait is 7 days and at least 3 hours. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"\"From 1 July 2026 the fee is $5.\"", []string{"$5"}, Period{From: "2026-07-01"}, ""},
		{"The fee is $5 multiplied by (2). The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The fee is $5 divided by [2]. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"Only 5 of the 10 devices are included. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"A purchase of the 10 devices is included.", []string{"10 devices"}, Period{}, ""},
		{"All orders before 1 July 2026 cost $5.", []string{"$5"}, Period{}, ""},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if got[i].Period != tt.period {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
			if tt.qualifier != "" && got[len(got)-1].Qualifier != tt.qualifier {
				t.Errorf("unexpected qualifier: %+v", got)
			}
		})
	}
}

func TestExtractReviewApproximateRateAndPeriodScope(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		period  Period
	}{
		{"The fee is $5 per approximately a total of 2 days and the deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 per approximately a total of 2 devices and the deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 per 2 devices and the deposit is $7.", []string{"$5", "2 devices", "$7"}, Period{}},
		{"The fee is $5 from 1 July 2026 and the deposit was $8 and from 1 July 2026 the charge is $7.", []string{"$5", "$8", "$7"}, Period{}},
		{"\"Orders before 1 July 2026 cost $5.\"", []string{"$5"}, Period{}},
		{"\"From 1 July 2026 the fee is $5.\"", []string{"$5"}, Period{From: "2026-07-01"}},
		{"The fee is 5% of at least $100 and the deposit is $7.", []string{"5%", "$7"}, Period{}},
		{"The fee is 5% and at least $100 is deposited.", []string{"5%", "$100"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if got[i].Period != tt.period {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewUncertainRateAndArithmeticOperands(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"The fee is $5 per approximately 100 pages and the deposit is $7.", []string{"$7"}},
		{"The fee is $5 per about (2 days) and the deposit is $7.", []string{"$7"}},
		{"The fee is $5 per ~2 days and the deposit is $7.", []string{"$7"}},
		{"The fee is $5 per ≈2 days and the deposit is $7.", []string{"$7"}},
		{"The fee is $5 per 100 pages and the deposit is $7.", []string{"$5", "$7"}},
		{"The fee is $5 multiplied by at least 2. The deposit is $7.", []string{"$7"}},
		{"The fee is $5 divided by at most 2. The deposit is $7.", []string{"$7"}},
		{"The fee is $5 multiplied by approximately 2. The deposit is $7.", []string{"$7"}},
		{"The fee is $5 and at least 2 devices are included.", []string{"$5", "2 devices"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewAnnotationsAndLists(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		period  Period
	}{
		{"The fee is $5 per approximately thirty pages. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 times (2). The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 x [2]. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 multiplied by at  least 2. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 divided by ≤2. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 from 1 July 2026 and the deposit is $8 and postage is $9 and from 1 July 2026 the charge is $7.", []string{"$5", "$8", "$9", "$7"}, Period{}},
		{"The wait is 7 days [maximum] and the fee is $5.", []string{"$5"}, Period{}},
		{"The wait is 7 days [excluding weekends] and the fee is $5.", []string{"$5"}, Period{}},
		{"The fee is $5 until 1 July 2026 [exclusive].", []string{"$5"}, Period{}},
		{"* $5 fee\n* $7 deposit", []string{"$5", "$7"}, Period{}},
		{"The fee is $5 times 2. The deposit is $7.", []string{"$7"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if got[i].Period != tt.period {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewEstimatedAndInterveningScopes(t *testing.T) {
	for _, tt := range []struct {
		text          string
		figures       []string
		unknownPeriod bool
	}{
		{"The fee is (2) times $5. The deposit is $7.", []string{"$7"}, false},
		{"The fee is [2] multiplied by $5. The deposit is $7.", []string{"$7"}, false},
		{"The fee is (2) divided by $5. The deposit is $7.", []string{"$7"}, false},
		{"Policy: ABCD EUR1234 applies. The fee is $7.", []string{"$7"}, false},
		{"Policy: active from 1 July 2026 the fee is $7.", []string{"$7"}, false},
		{"The fee is $5 from 1 July 2026 and\nthe deposit was $8 and from 1 July 2026 the charge is $7.", []string{"$5", "$8", "$7"}, true},
		{"The fee is $5 from 1 July 2026 and\tthe deposit was $8 and from 1 July 2026 the charge is $7.", []string{"$5", "$8", "$7"}, true},
		{"The fee is $5 from 1 July 2026 while the deposit was $8, and from 1 July 2026 the charge is $7.", []string{"$5", "$8", "$7"}, true},
		{"The estimated fee from 1 July 2026 is $5 and the wait is 7 days.", []string{"7 days"}, false},
		{"The estimated fee and deposit are $5 and $7. The wait is 3 days.", []string{"3 days"}, false},
		{"The estimated fee changed and the deposit is $7.", []string{"$7"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if tt.text == "The estimated fee changed and the deposit is $7." && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknownPeriod && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewLatestScopeCases(t *testing.T) {
	for _, tt := range []struct {
		text          string
		figures       []string
		unknownPeriod bool
	}{
		{"Estimated fee: $5. The deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 from 1 July 2026, the deposit was $8, and from 1 July 2026 the charge is $7.", []string{"$5", "$8", "$7"}, true},
		{"The deadline is 1 July 2026 or 1 August 2026.", nil, false},
		{"New customers before 1 July 2026 pay $5.", []string{"$5"}, true},
		{"The estimated shipping and handling are $5 and $7. The deposit is $9.", []string{"$9"}, false},
		{"Policy: ABCD 2026-07-01 applies. The fee is $7.", []string{"$7"}, false},
		{"The fee is about $5 per at least 2 devices and the deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 multiplied by negative 2. The deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 divided by minus 2. The deposit is $7.", []string{"$7"}, false},
		{"The wait is 7 days and ≥3 hours. The fee is $7.", []string{"$7"}, false},
		{"The wait is 7 days and ≥3 devices are included.", []string{"7 days", "3 devices"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknownPeriod && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewAlternativesAndRateOperands(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		period  Period
	}{
		{"The deadline is about 1 July 2026 or 1 August 2026.", nil, Period{}},
		{"The estimated fixed and variable fees are $5 and $7. The deposit is $9.", []string{"$9"}, Period{}},
		{"The fee is $5 each 1/2 page. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 a 2. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 per -2 pages. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 multiplied by NEGATIVE 2. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 divided by MiNuS 2. The deposit is $7.", []string{"$7"}, Period{}},
		{"The wait is 7 days from 1 July 2026 and ending 30 June 2027.", []string{"7 days"}, Period{From: "2026-07-01", To: "2027-06-30"}},
		{"From 1 July 2026 the estimated fee until 1 August 2026 is $5 and the wait is 3 days.", []string{"3 days"}, Period{From: "2026-07-01", To: "2026-08-01"}},
		{"The wait is 7 days [or 10 days]. The deposit is $7.", []string{"$7"}, Period{}},
		{"The wait is 7 days [and a half]. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 per month. The deposit is $7.", []string{"$5", "$7"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if got[i].Period != tt.period {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewRatesAndEstimatedLabels(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"The estimated time to completion is 7 days. The fee is $7.", []string{"$7"}},
		{"The fee is 5% of $100 per 2 devices and the deposit is $7.", []string{"5%", "$7"}},
		{"The fee is $5 multiplied by a factor of 2. The deposit is $7.", []string{"$7"}},
		{"The deadline is 1 July 2026 (approximately) or 1 August 2026. The fee is $7.", []string{"$7"}},
		{"From 1 July 2026, estimated fee: $5. The deposit is $7.", []string{"$7"}},
		{"The fee is $5 per at least 2. The deposit is $7.", []string{"$7"}},
		{"The fee is $5 per at least thirty pages. The deposit is $7.", []string{"$7"}},
		{"The fee is $1, 500USD. The deposit is $7.", []string{"$7"}},
		{"The fee is 5% of $100 and the deposit is $7.", []string{"5%", "$7"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewFractionAndScopeCases(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"The fee is $1, 500€. The deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 and from 1 July 2026 the office opens and from 1 July 2026 support is available.", []string{"$5"}, true},
		{"The fee is $5 per half hour. The deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 per one third of an hour. The deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 per 1/2 hour. The deposit is $7.", []string{"$7"}, false},
		{"From 1 July 2026 (approximately) or from 1 August 2026 the fee is $5.", []string{"$5"}, true},
		{"The wait is 7 days (give or take 1 day) and the fee is $5.", []string{"$5"}, false},
		{"Estimated fee: at least $5. The deposit is $7.", []string{"$7"}, false},
		{"Fee: at least $5.", []string{"$5"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewMoreRatesAndPeriods(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"The fee is $5 per three quarters of an hour. The deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 per 2 thirds of an hour. The deposit is $7.", []string{"$7"}, false},
		{"The wait is 7 days and more. The deposit is $7.", []string{"$7"}, false},
		{"The fee is $1, 500,000 dollars. The deposit is $7.", []string{"$7"}, false},
		{"The deadline is 1 July 2026, or 1 August 2026. The fee is $7.", []string{"$7"}, false},
		{"From 1 July 2026, or from 1 August 2026 the fee is $5.", []string{"$5"}, true},
		{"The fee is $5 and from 1 July 2026 the office opens and from 1 July 2026 the charge is $7.", []string{"$5", "$7"}, true},
		{"The estimated fee from about 1 July 2026 is $5 and the wait is 3 days.", []string{"3 days"}, true},
		{"The fee is $5 per at least 2-3 devices and the deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 per 2-3 devices and the deposit is $7.", []string{"$5", "2-3 devices", "$7"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewPredicatesAndContinuations(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		period  Period
	}{
		{"The office closes before 1 July 2026, the fee is $5.", []string{"$5"}, Period{}},
		{"Before 1 July 2026, the fee is $5.", []string{"$5"}, Period{To: "2026-06-30"}},
		{"The estimated time to be spent is 7 days. The fee is $5.", []string{"$5"}, Period{}},
		{"The fee is $7 and the office closes at 5pm before 1 July 2026.", []string{"$7"}, Period{}},
		{"The fee is $7, (plus 50 cents). The deposit is $5.", []string{"$5"}, Period{}},
		{"The wait is 7 days, (and a half). The fee is $5.", []string{"$5"}, Period{}},
		{"The fee is $7, and the deposit is $5.", []string{"$7", "$5"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if got[i].Period != tt.period {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewNominalRatesAndDates(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"The fee is $5 per at least 2-3 pages and the deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 per 2-3 pages and the deposit is $7.", []string{"$5", "$7"}, false},
		{"The fee is $7, the office closes before 1 July 2026.", []string{"$7"}, true},
		{"The deadline is on 1 July 2026 or on 1 August 2026.", nil, false},
		{"The deadlines are on 1 July 2026 and on 1 August 2026.", []string{"1 July 2026", "1 August 2026"}, false},
		{"An estimated fee of $5 applies and the deposit is $7.", []string{"$7"}, false},
		{"A fee of $5 applies and the deposit is $7.", []string{"$5", "$7"}, false},
		{"The fee is $5 an hour approximately and the deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 an hour maximum and the deposit is $7.", []string{"$7"}, false},
		{"The fee is $5 an hour and the deposit is $7.", []string{"$5", "$7"}, false},
		{"One out of every 10 devices fails. The fee is $7.", []string{"$7"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewNominalAndSymbolicRate(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"An estimated fee of at least $5 applies and the deposit is $7.", []string{"$7"}},
		{"From 1 July 2026 an estimated fee of $5 applies and the wait is 3 days.", []string{"3 days"}},
		{"A fee of at least $5 applies and the deposit is $7.", []string{"$5", "$7"}},
		{"The fee is $5 per ≥2-3 pages and the deposit is $7.", []string{"$7"}},
		{"The fee is $5 per <=2-3 pages and the deposit is $7.", []string{"$7"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewPossessivesAndAbbreviations(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"The fee is $5 and the office's lease ends before 1 July 2026.", []string{"$5"}, true},
		{"The fee is $5 and the office's short-term lease ends before 1 July 2026.", []string{"$5"}, true},
		{"The fee is $5 until 1 July 2026 (or later).", []string{"$5"}, true},
		{"The fee is $5 until 1 July 2026 (or earlier).", []string{"$5"}, true},
		{"There is an estimated fee of $5 and the deposit is $7.", []string{"$7"}, false},
		{"There is a fee of $5 and the deposit is $7.", []string{"$5", "$7"}, false},
		{"The wait is 7 min. approximately. The fee is $5.", []string{"$5"}, false},
		{"The wait is 7 min. or 10 min. The fee is $5.", []string{"$5"}, false},
		{"The wait is 7 min. The fee is $5.", []string{"7 min", "$5"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewAbbreviationsAndSubordinateDates(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"The wait is 7 days (approximately) or 10 days. The fee is $5.", []string{"$5"}, false},
		{"The wait is about 7 min. and 3 hr. The fee is $5.", []string{"$5"}, false},
		{"The wait is 7 min. maximum. The fee is $5.", []string{"$5"}, false},
		{"The fee is $5 because the office closes before 1 July 2026.", []string{"$5"}, true},
		{"The fee is $5 (the office closes before 1 July 2026 for repairs).", []string{"$5"}, true},
		{"The wait is 7 days and more information is available for $5.", []string{"7 days", "$5"}, false},
		{"The wait is 7 days and more. The fee is $5.", []string{"$5"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewAbbreviationsAndConditionalScopes(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"The wait is 7 min. + 3 hr. The fee is $5.", []string{"$5"}, false},
		{"The wait is 7 min. + 3 min. The fee is $5.", []string{"$5"}, false},
		{"The wait is 7 min. and a half. The fee is $5.", []string{"$5"}, false},
		{"The fee is $5 if the customer's order was placed before 1 July 2026.", []string{"$5"}, true},
		{"The fee is $5 and over 2 devices are included.", []string{"$5", "2 devices"}, false},
		{"Because the office closes before 1 July 2026 the fee is $5.", []string{"$5"}, true},
		{"The wait is 7 days and over. The fee is $5.", []string{"$5"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewRateAndIdentifierBoundaries(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"The wait is 7 min. multiplied by 2. The fee is $5.", []string{"$5"}},
		{"The wait is 7 min. divided by 2. The fee is $5.", []string{"$5"}},
		{"The wait is 7 min. times 2. The fee is $5.", []string{"$5"}},
		{"The wait is 7 min. excluding weekends. The fee is $5.", []string{"$5"}},
		{"The fee is $5 per month and over 2 devices are included.", []string{"$5", "2 devices"}},
		{"The fee is $5/hr. + 2. The deposit is $7.", []string{"$7"}},
		{"The fee is $5/hr. The deposit is $7.", []string{"$5", "$7"}},
		{"Policy number: AB1234, $5 fee.", []string{"Policy number: AB1234", "$5"}},
		{"Policy number: AB1234 ABCD EUR5678 applies. The fee is $7.", []string{"$7"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewAbbreviationIdentifierAndEqualBounds(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"The wait is 7 min. through 10 min. The fee is $5.", []string{"$5"}, false},
		{"The wait is 7 min. (give or take 1 min.). The fee is $5.", []string{"$5"}, false},
		{"The wait is 7 min. plus 3 min. The fee is $5.", []string{"$5"}, false},
		{"Policy number: AB1234: $5 fee.", []string{"Policy number: AB1234", "$5"}, false},
		{"(Policy number: AB1234) $5 fee.", []string{"Policy number: AB1234", "$5"}, false},
		{"The deposit is $9 and the fee is $5 from 1 July 2026 and the charge is $7 from 1 July 2026.", []string{"$9", "$5", "$7"}, true},
		{"The office closes before 1 July 2026 so the fee is $5.", []string{"$5"}, true},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewOverlapAndAbbreviatedRates(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"USD 7 days from 1 July 2026 and from 1 July 2026 the fee is $5.", []string{"$5"}, true},
		{"The wait is 7 min. and a third. The fee is $5.", []string{"$5"}, false},
		{"The wait is 7 min. or (10 min.). The fee is $5.", []string{"$5"}, false},
		{"The rate is about 7 min. per 2 devices. The fee is $5.", []string{"$5"}, false},
		{"The adjustment is ($5/hr.). The fee is $7.", []string{"$7"}, false},
		{"(Policy number: AB1234), $5 fee.", []string{"Policy number: AB1234", "$5"}, false},
		{"(Policy number: AB1234): $5 fee.", []string{"Policy number: AB1234", "$5"}, false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewQualifiedContinuationsAndCausalDates(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"The wait is 7 min. and about 3 hr. The fee is $5.", []string{"$5"}, false},
		{"The fees are about $5/hr. and $10/hr. The deposit is $7.", []string{"$7"}, false},
		{"The wait is 7 min. and at least 3 hr. The fee is $5.", []string{"$5"}, false},
		{"The fee is $5, the office closes at 5pm before 1 July 2026.", []string{"$5"}, true},
		{"The office closes before 1 July 2026 because the fee is $5.", []string{"$5"}, true},
		{"The office closes before 1 July 2026 if the fee is $5.", []string{"$5"}, true},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewAbbreviationRatesAndTokenBoundaries(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		unknown bool
	}{
		{"The fee is $5 for the customer's orders before 1 July 2026.", []string{"$5"}, true},
		{"The wait is 7 min. and ≥3 hr. The fee is $5.", []string{"$5"}, false},
		{"The deadline is (1 July 2026).", []string{"1 July 2026"}, false},
		{"(Policy number: AB1234).", []string{"Policy number: AB1234"}, false},
		{"The fees are about USD 5/hr. and USD 10/hr. The deposit is $7.", []string{"$7"}, false},
		{"The rate is 7 min. per about 100 pages. The fee is $5.", []string{"$5"}, false},
		{"The rate is 7 min. per at least 2-3 pages. The fee is $5.", []string{"$5"}, false},
		{"The office closes before 1 July 2026, $5 is the fee.", []string{"$5"}, true},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
				if tt.unknown && got[i].Period != (Period{}) {
					t.Errorf("unexpected period: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractNominalBoundsAndScaledAmounts(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"The minimum fee is $5 and the deposit is $7.", []string{"$7"}},
		{"The maximum fee is $5 and the deposit is $7.", []string{"$7"}},
		{"A minimum monthly fee is $5. The deposit is $7.", []string{"$7"}},
		{"The fee is $5 and the maximum capacity is unknown.", []string{"$5"}},
		{"The minimum fee changed and the deposit is $7.", []string{"$7"}},
		{"The budget is $5 bn. The fee is $7.", []string{"$7"}},
		{"The budget is $5bn. The fee is $7.", []string{"$7"}},
		{"The budget is $5 BN. The fee is $7.", []string{"$7"}},
		{"The budget is $5. The fee is $7.", []string{"$5", "$7"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractTopsAndCoordinatedStartDates(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		period  Period
	}{
		{"The fee is $5 tops. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 tops and the deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 and the deposit is $7.", []string{"$5", "$7"}, Period{}},
		{"The fee is $7 from 1 July 2026 and 1 August 2026.", []string{"$7"}, Period{}},
		{"The fee is $7 from July 2026 and August 2026.", []string{"$7"}, Period{}},
		{"The fee is $7 from 1 July 2026.", []string{"$7"}, Period{From: "2026-07-01"}},
		{"The fee is $7 from July 2026.", []string{"$7"}, Period{From: "2026-07"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want || got[i].Period != tt.period {
					t.Errorf("unexpected figure: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractAnythingButExcludesFigures(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"The fee is anything but $7. The deposit is $5.", []string{"$5"}},
		{"The wait is anything but 7 days. The fee is $5.", []string{"$5"}},
		{"The fee is ANYTHING  BUT $7. The deposit is $5.", []string{"$5"}},
		{"The fee is $7 but the deposit is $5.", []string{"$7", "$5"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractDatedBoundsApproximateGroupsAndMoneyScales(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
	}{
		{"The maximum fee from 1 July 2026 is $5 and the wait is 3 days.", []string{"3 days"}},
		{"The minimum fee from 1 July 2026 is $5 and the wait is 3 days.", []string{"3 days"}},
		{"From 1 July 2026 the estimated fee is $5 and the wait is 3 days.", []string{"3 days"}},
		{"The wait is 7 days and about 3 hours and the fee is $5.", []string{"$5"}},
		{"The wait is 7 days and about 3 hours.", nil},
		{"The wait is 7 days and about 3. The fee is $5.", []string{"$5"}},
		{"The wait is 7 days and about 3 hours + 2 and the fee is $5.", nil},
		{"The wait is 7 days and 3 hours and the fee is $5.", []string{"7 days", "3 hours", "$5"}},
		{"The budget is $5 monthly and the fee is $7.", []string{"$5", "$7"}},
		{"The budget is USD 5 m. The fee is $7.", []string{"$7"}},
		{"The budget is $5 mn. The fee is $7.", []string{"$7"}},
		{"The budget is $5 k. The fee is $7.", []string{"$7"}},
		{"The wait is 5 m. The fee is $7.", []string{"$7"}},
		{"The budget is $5. The fee is $7.", []string{"$5", "$7"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("unexpected figure: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractNominalLabelsAbbreviationAndDateRanges(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		period  Period
	}{
		{"Minimum fee: $5. The deposit is $7.", []string{"$7"}, Period{}},
		{"Maximum fee: $5. The deposit is $7.", []string{"$7"}, Period{}},
		{"There is a minimum fee of $5 and the deposit is $7.", []string{"$7"}, Period{}},
		{"There is a maximum fee of $5 and the deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5/hr. (plus $2). The deposit is $7.", []string{"$7"}, Period{}},
		{"The wait is 7 min. (plus 2 min.). The fee is $7.", []string{"$7"}, Period{}},
		{"The fee is $7 until 1 July 2026 and 1 August 2026.", []string{"$7"}, Period{}},
		{"The fee is $7 through 1 July 2026 and 1 August 2026.", []string{"$7"}, Period{}},
		{"The period is 2026-07-01 - 2026-07-31. The fee is $7.", []string{"$7"}, Period{}},
		{"The deadline is 2026-07-01.", []string{"2026-07-01"}, Period{}},
		{"The fee is $7 from 1 July 2026 until 1 August 2026.", []string{"$7"}, Period{From: "2026-07-01", To: "2026-08-01"}},
		{"The fee is $5/hr. The deposit is $7.", []string{"$5", "$7"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want || got[i].Period != tt.period {
					t.Errorf("unexpected figure: %+v", got[i])
				}
			}
		})
	}
}

func TestExtractUnsupportedUnicodeDashDateRanges(t *testing.T) {
	for _, separator := range []string{"-", "‒", "–", "—", "−", "⁓", "⁻", "₋", "/"} {
		text := "The period is 2026-07-01 " + separator + " 2026-07-31. The fee is $7."
		got := Extract(text)
		if len(got) != 1 || got[0].Text != "$7" || string([]rune(text)[got[0].Start:got[0].End]) != "$7" {
			t.Errorf("%q: %+v", separator, got)
		}
	}
	text := "The deadline is 2026-07-01. The fee is $7."
	got := Extract(text)
	if len(got) != 2 || got[0].Text != "2026-07-01" || got[1].Text != "$7" {
		t.Errorf("standalone date: %+v", got)
	}
}

func TestExtractReviewTypedContinuations(t *testing.T) {
	for _, tt := range []struct {
		text string
		want []string
	}{
		{"The service is available 5 days out of every 10 days. The fee is $7.", []string{"$7"}},
		{"The wait is 7 hr. and a few minutes. The fee is $5.", []string{"$5"}},
		{"The fee is CAD$5 per 2 devices. The deposit is $7.", []string{"$7"}},
		{"The wait is 7 days and an estimated 3 hours. The fee is $5.", []string{"$5"}},
		{"The wait is 7 days and 3 hours. The fee is $5.", []string{"7 days", "3 hours", "$5"}},
		{"The wait is 7 hr. The fee is $5.", []string{"7 hr", "$5"}},
		{"The deadline is 1 July 2026. The fee is $5.", []string{"1 July 2026", "$5"}},
		{"The code is XCAD$5. The deposit is $7.", []string{"$7"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("got %+v, want %s", got[i], want)
				}
			}
		})
	}
}

func TestExtractReviewAbbreviatedRateIndependentDate(t *testing.T) {
	text := "The fee is $5/hr. From 1 July 2026 the deposit is $7."
	got := Extract(text)
	if len(got) != 2 || got[0].Text != "$5" || got[0].Clause != "The fee is $5/hr." || got[0].Period != (Period{}) || got[1].Text != "$7" || got[1].Clause != "From 1 July 2026 the deposit is $7." || got[1].Period != (Period{From: "2026-07-01"}) {
		t.Fatalf("got %+v", got)
	}
	for _, f := range got {
		if string([]rune(text)[f.ClauseStart:f.ClauseEnd]) != f.Clause {
			t.Errorf("bad clause span: %+v", f)
		}
	}
}

func TestExtractReviewAbbreviationDateBounds(t *testing.T) {
	for _, tt := range []struct {
		connector string
		period    Period
	}{
		{"before", Period{To: "2026-06-30"}},
		{"until", Period{To: "2026-07-01"}},
		{"through", Period{To: "2026-07-01"}},
		{"from", Period{From: "2026-07-01"}},
	} {
		t.Run(tt.connector, func(t *testing.T) {
			text := "The fee is $5/hr. " + tt.connector + " 1 July 2026."
			got := Extract(text)
			if len(got) != 1 || got[0].Text != "$5" || got[0].Period != tt.period || got[0].Clause != text || string([]rune(text)[got[0].ClauseStart:got[0].ClauseEnd]) != text {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestExtractReviewAbbreviationsAndQuotedArithmetic(t *testing.T) {
	for _, tt := range []struct {
		text string
		want []string
	}{
		{"The wait is 7 min. and an estimated 3 hours. The fee is $5.", []string{"$5"}},
		{"The wait is 7 min. and an hour. The fee is $5.", []string{"$5"}},
		{"The fee is «$5» + 2. The deposit is $7.", []string{"$7"}},
		{"The fee is «$5». The deposit is $7.", []string{"$5", "$7"}},
		{"The wait is 7 min. and an hour later the fee is $5.", []string{"7 min", "$5"}},
		{"The wait is 7 min. and a day later the fee is $5.", []string{"7 min", "$5"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("got %+v, want %s", got[i], want)
				}
			}
		})
	}
}

func TestExtractReviewAbbreviatedRateIndependentBefore(t *testing.T) {
	text := "The fee is $5/hr. Before 1 July 2026 the deposit is $7."
	got := Extract(text)
	if len(got) != 2 || got[0].Text != "$5" || got[0].Clause != "The fee is $5/hr." || got[0].Period != (Period{}) || got[1].Text != "$7" || got[1].Clause != "Before 1 July 2026 the deposit is $7." || got[1].Period != (Period{To: "2026-06-30"}) {
		t.Fatalf("got %+v", got)
	}
	for _, f := range got {
		if string([]rune(text)[f.ClauseStart:f.ClauseEnd]) != f.Clause {
			t.Errorf("bad clause span: %+v", f)
		}
	}
}

func TestExtractReviewAbbreviatedNumericContexts(t *testing.T) {
	for _, tt := range []struct {
		text string
		want []string
	}{
		{"The wait is 7 min. – 10 min. The fee is $5.", []string{"$5"}},
		{"The wait is 7min. and a half. The fee is $5.", []string{"$5"}},
		{"The fee is negative «$7». The deposit is $5.", []string{"$5"}},
		{"The fee is $5 less «$2». The deposit is $7.", []string{"$7"}},
		{"The fee is $5/month + 2. The deposit is $7.", []string{"$7"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("got %+v want %s", got[i], want)
				}
			}
		})
	}
}

func TestExtractReviewRateAndRelativeDateBounds(t *testing.T) {
	for _, tt := range []struct {
		text, figure string
		period       Period
	}{
		{"The notice is due 2 hr. before 1 July 2026.", "2 hr", Period{}},
		{"The notice is due 2 hr. prior to 1 July 2026.", "2 hr", Period{}},
		{"The notice is due 2 hr. from 1 July 2026.", "2 hr", Period{}},
		{"The fee is $5/month until 1 July 2026.", "$5", Period{To: "2026-07-01"}},
		{"The fee is $5 per month until 1 July 2026.", "$5", Period{To: "2026-07-01"}},
		{"The fee is $5/hr. from 1 July 2026 until 31 July 2026.", "$5", Period{From: "2026-07-01", To: "2026-07-31"}},
		{"The notice is 2 hr. from 1 July 2026 until 31 July 2026.", "2 hr", Period{From: "2026-07-01", To: "2026-07-31"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != 1 || got[0].Text != tt.figure || got[0].Period != tt.period || got[0].Clause != tt.text || string([]rune(tt.text)[got[0].ClauseStart:got[0].ClauseEnd]) != tt.text {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestExtractReviewIndependentScopeAndRates(t *testing.T) {
	for _, tt := range []struct {
		text    string
		figures []string
		period  Period
		clauses []string
	}{
		{"The fee is $5 and the office (downtown) closes before 1 July 2026.", []string{"$5"}, Period{}, nil},
		{"Starting 1 July 2026 the fee is $5 and the wait is 2 days.", []string{"$5", "2 days"}, Period{From: "2026-07-01"}, nil},
		{"The fee is $5 per 2 months from 1 July 2026.", []string{"$5", "2 months"}, Period{From: "2026-07-01"}, nil},
		{"The notice is due 2 months before 1 July 2026.", []string{"2 months"}, Period{}, nil},
		{"The wait is 7 min. A refund is not available.", []string{"7 min"}, Period{}, []string{"The wait is 7 min."}},
		{"The wait is 7 min. A fee applies from 1 July 2026.", []string{"7 min", "1 July 2026"}, Period{}, []string{"The wait is 7 min.", "A fee applies from 1 July 2026."}},
		{"The wait is 7 min. a day.", []string{"7 min"}, Period{}, nil},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want || len(tt.clauses) > 0 && (f.Clause != tt.clauses[i] || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v", f)
				}
			}
		})
	}
}

func TestExtractReviewGuillemetQualifiers(t *testing.T) {
	for _, tt := range []struct {
		text      string
		want      []string
		qualifier string
	}{
		{"The fee is at least «$5».", []string{"$5"}, "≥"},
		{"The fee is about «$5». The deposit is $7.", []string{"$7"}, ""},
		{"The fee is at least $5.", []string{"$5"}, "≥"},
		{"The fee is «$5».", []string{"$5"}, ""},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v", got)
			}
			for i, want := range tt.want {
				if got[i].Text != want || got[i].Qualifier != tt.qualifier {
					t.Errorf("got %+v", got[i])
				}
			}
		})
	}
}

func TestExtractReviewArticleSentencesAndQuotedRelations(t *testing.T) {
	for _, tt := range []struct {
		text             string
		figures, clauses []string
	}{
		{"The wait is 7 min. An account is not required.", []string{"7 min"}, []string{"The wait is 7 min."}},
		{"The wait is 7 min. A refund is not available.", []string{"7 min"}, []string{"The wait is 7 min."}},
		{"The wait is 7 min. a day.", []string{"7 min"}, []string{"The wait is 7 min. a day."}},
		{"The wait is 7 min. an hour.", []string{"7 min"}, []string{"The wait is 7 min. an hour."}},
		{"The wait is about «7 days» and 3 hours. The fee is $5.", []string{"$5"}, []string{"The fee is $5."}},
		{"The deadline is «1 July 2026» or «1 August 2026». The fee is $5.", []string{"$5"}, []string{"The fee is $5."}},
		{"The wait is «7 days» and «3 hours».", []string{"7 days", "3 hours"}, nil},
		{"The deadlines are «1 July 2026» and «1 August 2026».", []string{"1 July 2026", "1 August 2026"}, nil},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.figures) {
				t.Fatalf("got %+v, want %v", got, tt.figures)
			}
			for i, want := range tt.figures {
				f := got[i]
				if f.Text != want || string([]rune(tt.text)[f.Start:f.End]) != want || len(tt.clauses) > 0 && (f.Clause != tt.clauses[i] || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v", f)
				}
			}
		})
	}
}

func TestExtractReviewAbbreviationsAndIdentifiers(t *testing.T) {
	for _, tt := range []struct {
		text          string
		want, clauses []string
	}{
		{"The wait is 7 min. Maximum fee is $5.", []string{"7 min"}, []string{"The wait is 7 min."}},
		{"The wait is 7 min. Maximum fee: $5.", []string{"7 min"}, []string{"The wait is 7 min."}},
		{"The wait is 7 min. maximum. The fee is $5.", []string{"$5"}, nil},
		{"The wait is 7 min. A\nrefund is not available.", []string{"7 min"}, []string{"The wait is 7 min."}},
		{"The wait is 7 min. An\taccount is not required.", []string{"7 min"}, []string{"The wait is 7 min."}},
		{"The wait is 7 min. a day.", []string{"7 min"}, nil},
		{"The wait is 7 min. an hour.", []string{"7 min"}, nil},
		{"Account number: 1234 ****. The fee is $5.", []string{"$5"}, nil},
		{"Account number: 1234 xxxx. The fee is $5.", []string{"$5"}, nil},
		{"Account number: 1234 xxxx 7 days. The fee is $5.", []string{"$5"}, nil},
		{"Policy no: AB1234 and the fee is $5.", []string{"Policy no: AB1234", "$5"}, nil},
		{"Policy no.: AB1234 and the fee is $5.", []string{"Policy no.: AB1234", "$5"}, nil},
		{"Policy no: AB1234 and no refund is available.", nil, nil},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || string([]rune(tt.text)[f.Start:f.End]) != want || len(tt.clauses) > 0 && (f.Clause != tt.clauses[i] || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v, want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewIncompleteDateAlternativesAndBoundedRates(t *testing.T) {
	for _, tt := range []struct {
		text string
		want []string
	}{
		{"The deadline is 1 or 2 July 2026. The fee is $7.", []string{"$7"}},
		{"The deadline is 1 June or 2 July 2026. The fee is $7.", []string{"$7"}},
		{"The deadline is 1 OR 2 July 2026. The fee is $7.", []string{"$7"}},
		{"The deadline is July or August 2026. The fee is $7.", []string{"$7"}},
		{"The deadline is 2 July 2026. The fee is $7.", []string{"2 July 2026", "$7"}},
		{"The fee is $5 per at least 2 days and 3 hours. The deposit is $7.", []string{"$7"}},
		{"The fee is $5 per at least 2 days and 3 hours and the deposit is $7 and 3 devices are included.", []string{"$7", "3 devices"}},
		{"The fee is $5 per 2 days and 3 hours. The deposit is $7.", []string{"$5", "2 days", "3 hours", "$7"}},
		{"The fee is $5 per at least 2 days. The deposit is $7.", []string{"$5", "2 days", "$7"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("got %+v want %q", got[i], want)
				}
			}
		})
	}
}

func TestExtractReviewComparisonsDatesAndIdentifiers(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		clause string
	}{
		{"The wait is 7 days < 10 and the fee is $5. The deposit is $7.", []string{"$7"}, ""},
		{"The wait is 7 days greater than 3 and the fee is $5. The deposit is $7.", []string{"$7"}, ""},
		{"The deadline is between 1 and 2 July 2026. The fee is $5.", []string{"$5"}, ""},
		{"The deadline is 1 to 2 July 2026. The fee is $5.", []string{"$5"}, ""},
		{"The deadline is 1 or (2 July 2026). The fee is $5.", []string{"$5"}, ""},
		{"The deadline is 1 or «2 July 2026». The fee is $5.", []string{"$5"}, ""},
		{"The deadline is 2 July 2026. The fee is $5.", []string{"2 July 2026", "$5"}, ""},
		{"The deadline is 1 July 2026 or 2 July 2026. The fee is $5.", []string{"$5"}, ""},
		{"The wait is 7 min. Maximum delay is 2 days.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. Maximum discount is 2 percent.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. Maximum devices is 2 devices.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. maximum. The fee is $5.", []string{"$5"}, ""},
		{"Policy number: AB1234 and $5 fee.", []string{"Policy number: AB1234", "$5"}, ""},
		{"Policy number: AB1234 xxxx and $5 fee.", []string{"$5"}, ""},
		{"The wait is 7 days and 3 hours. The fee is $5.", []string{"7 days", "3 hours", "$5"}, ""},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || string([]rune(tt.text)[f.Start:f.End]) != want || tt.clause != "" && (f.Clause != tt.clause || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewDateRateAndAbbreviationContinuations(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		period Period
	}{
		{"The deadline is 1 July 2026 or 2 August. The fee is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 from 1 July 2026 or 2 August. The deposit is $7.", []string{"$5", "$7"}, Period{}},
		{"The fee is $5 per 10-7 pages until 1 July 2026. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 per " + strings.Repeat("9", 129) + " pages until 1 July 2026. The deposit is $7.", []string{"$7"}, Period{}},
		{"The service is available 5 hr. out of every 10 hr. The fee is $7.", []string{"$7"}, Period{}},
		{"The wait is 7 min. < 10 and the fee is $5. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fee is $5/month < 10 and the deposit is $7. The wait is 3 days.", []string{"3 days"}, Period{}},
		{"The fee is $5/month < 10 dollars and the deposit is $7. The wait is 3 days.", []string{"3 days"}, Period{}},
		{"The wait is 7 min. The fee is $7.", []string{"7 min", "$7"}, Period{}},
		{"The fee is $5 per 7-10 pages until 1 July 2026.", []string{"$5"}, Period{To: "2026-07-01"}},
		{"The deadline is 1 July 2026. The fee is $7.", []string{"1 July 2026", "$7"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i].Text != want || got[i].Period != tt.period || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("got %+v want %q", got[i], want)
				}
			}
		})
	}
}

func TestExtractReviewQuotedGroupsAndConditionalDates(t *testing.T) {
	for _, tt := range []struct {
		text        string
		want        []string
		period      Period
		firstClause string
	}{
		{"The fee is $5 from 1 July 2026 or 2 August for renewals. The deposit is $7.", []string{"$5", "$7"}, Period{}, ""},
		{"The deadline is 1 July 2026 or 2 August for renewals. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"The deadline is 1 July 2026 or «2 August» for renewals. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"The deadline is 1 July 2026 or 2 August 2026. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"The fees are about 5% of $100 and 10% of $200. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The fees are about 5% of $100 and 10% of $200 and the deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The fees are 5% of $100 and 10% of $200. The deposit is $7.", []string{"5%", "10%", "$7"}, Period{}, ""},
		{"The wait is 7 min. Each customer has no fee.", []string{"7 min"}, Period{}, "The wait is 7 min."},
		{"The wait is 7 min. each day.", []string{"7 min"}, Period{}, ""},
		{"The fee is $5 if the office (downtown) closes before 1 July 2026.", []string{"$5"}, Period{}, ""},
		{"The fee is $5 because the office (downtown) closes before 1 July 2026.", []string{"$5"}, Period{}, ""},
		{"The wait is about 7 min. and «3 hours». The fee is $7.", []string{"$7"}, Period{}, ""},
		{"The wait is 7 min. The fee is $7.", []string{"7 min", "$7"}, Period{}, ""},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want || i == 0 && tt.firstClause != "" && (f.Clause != tt.firstClause || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewWrappedUncertaintyAndPercentageBases(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		period Period
	}{
		{"The deadline is (1 July 2026) or later. The fee is $7.", []string{"$7"}, Period{}},
		{"The deadline is «1 July 2026» or earlier. The fee is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 from (1 July 2026) or later. The deposit is $7.", []string{"$5", "$7"}, Period{}},
		{"The deadline is (1 July 2026). The fee is $7.", []string{"1 July 2026", "$7"}, Period{}},
		{"The fees are about 5% of $100 and $200 and 10% of $300. The deposit is $7.", []string{"$7"}, Period{}},
		{"The fees are about 5% of $100 and $200 and 10% of $300 and the deposit is $7.", []string{"$7"}, Period{}},
		{"The fees are 5% of $100 and $200 and 10% of $300. The deposit is $7.", []string{"5%", "10%", "$7"}, Period{}},
		{"The fee is about 5% of $100 and the deposit is $7.", []string{"$7"}, Period{}},
		{"The deadline is 7 days prior to «10 days». The fee is $7.", []string{"$7"}, Period{}},
		{"The wait is 7 days and the delay is 10 days. The fee is $7.", []string{"7 days", "10 days", "$7"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewWrappedListsAndIdentifierSeparators(t *testing.T) {
	for _, tt := range []struct {
		text string
		want []string
	}{
		{"- The wait is not\n  7 days. The fee is $5.", []string{"$5"}},
		{"- The wait is about\n  7 days. The fee is $5.", []string{"$5"}},
		{"- The wait is not\n- Wait is 7 days.", []string{"7 days"}},
		{"- The wait is not\n\n7 days.", []string{"7 days"}},
		{"- The wait is not\n7 days.", []string{"7 days"}},
		{"- The wait is not\n| Wait | 7 days |\nThe fee is $5.", []string{"$5"}},
		{"Account number: 1234 / 5678. The fee is $5.", []string{"$5"}},
		{"Account number: 1234 / USD5678. The fee is $5.", []string{"$5"}},
		{"Account number: 1234 / 2026-07-01. The fee is $5.", []string{"$5"}},
		{"Account number: 1234 - 5678. The fee is $5.", []string{"$5"}},
		{"Account number: 1234. The fee is $5.", []string{"Account number: 1234", "$5"}},
		{"Account number: 1234 and the fee is $5.", []string{"Account number: 1234", "$5"}},
		{"The date is 1 July 2026. The fee is $5.", []string{"1 July 2026", "$5"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("got %+v want %q", got[i], want)
				}
			}
		})
	}
}

func TestExtractReviewSuperscriptAndSubscriptDigits(t *testing.T) {
	for _, digit := range "⁰¹²³⁴⁵⁶⁷⁸⁹₀₁₂₃₄₅₆₇₈₉" {
		for _, tt := range []struct{ label, text string }{
			{"money", "The fee is $10" + string(digit) + " and the deposit is $5. The wait is 3 days."},
			{"duration", "The wait is 10" + string(digit) + " days and the fee is $5. The deposit is $7."},
			{"count", "The limit is 10" + string(digit) + " devices and the fee is $5. The deposit is $7."},
		} {
			t.Run(tt.label+string(digit), func(t *testing.T) {
				got := Extract(tt.text)
				want := "$7"
				if tt.label == "money" {
					want = "3 days"
				}
				if len(got) != 1 || got[0].Text != want || string([]rune(tt.text)[got[0].Start:got[0].End]) != want {
					t.Fatalf("got %+v want %s", got, want)
				}
			})
		}
	}
}

func TestExtractReviewNumericNormalizationControls(t *testing.T) {
	for _, tt := range []struct {
		text string
		want []string
	}{
		{"The limit is １０６ devices. The fee is $5.", []string{"１０６ devices", "$5"}},
		{"Note¹: The fee is $5.", []string{"$5"}},
		{"Policy number: AB10⁶ and the fee is $5.", []string{"Policy number: AB10⁶", "$5"}},
		{"Policy number: ****10⁶ and the fee is $5.", []string{"$5"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i].Text != want || string([]rune(tt.text)[got[i].Start:got[i].End]) != want {
					t.Errorf("got %+v want %q", got[i], want)
				}
			}
		})
	}
}

func TestExtractReviewInclusiveComparisonsAndWrappedDates(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		period Period
	}{
		{"The wait is 7 days greater than or equal to 10 days. The fee is $7.", []string{"$7"}, Period{}},
		{"The wait is 7 days less than or equal to 10 and the fee is $5. The deposit is $7.", []string{"$7"}, Period{}},
		{"The wait is 7 days (< 10 days). The fee is $7.", []string{"$7"}, Period{}},
		{"The wait is 7 days [>= 10 days]. The fee is $7.", []string{"$7"}, Period{}},
		{"The wait is at least 7 days. The fee is $7.", []string{"7 days", "$7"}, Period{}},
		{"The fee is $5 per 2 days and 3 hours until 1 July 2026.", []string{"$5", "2 days", "3 hours"}, Period{To: "2026-07-01"}},
		{"The deadline is (1 July 2026) or 2 August. The fee is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 from «1 July 2026» or 2 August. The deposit is $7.", []string{"$5", "$7"}, Period{}},
		{"The deadline is (1 July 2026). The fee is $7.", []string{"1 July 2026", "$7"}, Period{}},
		{"The deadline is (1 July 2026) or 2 August 2026. The fee is $7.", []string{"$7"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewLongNominalAndEligibilityDates(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		period Period
	}{
		{"Tickets before 1 July 2026 cost $5.", []string{"$5"}, Period{}},
		{"Members before 1 July 2026 pay $5.", []string{"$5"}, Period{}},
		{"The estimated total monthly service subscription fee is $5 and the deposit is $7.", []string{"$7"}, Period{}},
		{"The maximum total monthly service subscription fee is $5 and the deposit is $7.", []string{"$7"}, Period{}},
		{"The minimum total monthly service subscription fee is $5 and the deposit is $7.", []string{"$7"}, Period{}},
		{"Estimated total monthly service subscription fee: $5 and the deposit is $7.", []string{"$7"}, Period{}},
		{"An estimated total monthly service subscription fee of $5 applies and the deposit is $7.", []string{"$7"}, Period{}},
		{"The minimum fee changed and the deposit is $7.", []string{"$7"}, Period{}},
		{"The estimated fee changed and the deposit is $7.", []string{"$7"}, Period{}},
		{"The fee of $7 applies to orders placed before 1 July 2026.", []string{"$7"}, Period{}},
		{"From 1 July 2026 the fee is $5 and the deposit is $7.", []string{"$5", "$7"}, Period{From: "2026-07-01"}},
		{"Before 1 July 2026 the fee is $5.", []string{"$5"}, Period{To: "2026-06-30"}},
		{"The notice is due 2 days before 1 July 2026.", []string{"2 days"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewCompoundSubjectsAndDateSuffixes(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		period Period
	}{
		{"The estimated one-time fee is $5. The deposit is $7.", []string{"$7"}, Period{}},
		{"The minimum one-time fee is $5. The deposit is $7.", []string{"$7"}, Period{}},
		{"The maximum one-time fee is $5. The deposit is $7.", []string{"$7"}, Period{}},
		{"The estimated one-time fee changed and the deposit is $7.", []string{"$7"}, Period{}},
		{"The deadline is 1 July 2026, or later. The fee is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 until 1 July 2026, or later. The deposit is $7.", []string{"$5", "$7"}, Period{}},
		{"B2B orders before 1 July 2026 cost $5.", []string{"$5"}, Period{}},
		{"Long-term members before 1 July 2026 pay $5.", []string{"$5"}, Period{}},
		{"The fee is 5% of (at least $100). The deposit is $7.", []string{"5%", "$7"}, Period{}},
		{"The deadline is 1 July 2026 or 2. The fee is $7.", []string{"$7"}, Period{}},
		{"The fee is $5 from 1 July 2026 or 2. The deposit is $7.", []string{"$5", "$7"}, Period{}},
		{"The deadline is 1 July 2026 or 2 days. The fee is $7.", []string{"$7"}, Period{}},
		{"Account number: 1234 / ABCD. The fee is $7.", []string{"$7"}, Period{}},
		{"Account number: 1234 / ABCD and the fee is $7.", []string{"$7"}, Period{}},
		{"Account number: 1234 / valid prose. The fee is $7.", []string{"Account number: 1234", "$7"}, Period{}},
		{"The deadline is 1 July 2026. The fee is $7.", []string{"1 July 2026", "$7"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewCompoundSubjectAndCausalContinuations(t *testing.T) {
	for _, tt := range []struct {
		text        string
		want        []string
		period      Period
		firstClause string
	}{
		{"An estimated shipping and handling fee of $5 applies. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The minimum shipping and handling fee is $5. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The maximum shipping and handling fee is $5. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"Estimated shipping and handling fee: $5. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The minimum fee changed and the deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The estimated fee changed and the deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The fee is $5 until 1 July 2026 or 2 August '27. The deposit is $7.", []string{"$5", "$7"}, Period{}, ""},
		{"The deadline is 1 July 2026 or 2 August 27. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"The refund is $5 since the office closes before 1 July 2026.", []string{"$5"}, Period{}, ""},
		{"The fees are about 5% of about $100 and 10% of $200. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"The wait is 7 min. Per policy, no refund is available.", []string{"7 min"}, Period{}, "The wait is 7 min."},
		{"The wait is 7 min. per day.", []string{"7 min"}, Period{}, "The wait is 7 min. per day."},
		{"The fee is $5 since 1 July 2026.", []string{"$5"}, Period{From: "2026-07-01"}, ""},
		{"The fee is $5 until 1 July 2026 or 2 August 2027. The deposit is $7.", []string{"$5", "$7"}, Period{}, ""},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want || i == 0 && tt.firstClause != "" && (f.Clause != tt.firstClause || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewIdentifierConjunctionBoundaries(t *testing.T) {
	for _, tt := range []struct {
		text string
		want []string
	}{
		{"Policy number: AB1234 AND $5 fee.", []string{"Policy number: AB1234", "$5"}},
		{"Policy number: AB1234 aNd $5 fee.", []string{"Policy number: AB1234", "$5"}},
		{"Policy number: AB1234 and $5 fee.", []string{"Policy number: AB1234", "$5"}},
		{"Policy number: AB1234 BUT $5 fee.", []string{"Policy number: AB1234", "$5"}},
		{"Policy number: AB1234 WhIlE $5 fee.", []string{"Policy number: AB1234", "$5"}},
		{"Policy number: AB1234 / 5678 AND $5 fee.", []string{"$5"}},
		{"Policy number: AB1234 xxxx AND $5 fee.", []string{"$5"}},
		{"Policy number: AB1234 AND1234. The fee is $5.", []string{"$5"}},
		{"Policy number: AB1234 ABCD. The fee is $5.", []string{"$5"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || string([]rune(tt.text)[f.Start:f.End]) != want {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewIndependentEstimatedAndPerSentences(t *testing.T) {
	for _, tt := range []struct {
		text        string
		want        []string
		firstClause string
	}{
		{"The estimated fee increased and the deposit is $7.", []string{"$7"}, ""},
		{"The estimated fee decreased and the deposit is $7.", []string{"$7"}, ""},
		{"The estimated shipping and handling fee is $5. The deposit is $7.", []string{"$7"}, ""},
		{"The wait is 7 min. Per customer, no fee applies.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. Per policy, no fee applies.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. per day.", []string{"7 min"}, "The wait is 7 min. per day."},
		{"The wait is 7 min. Per day.", []string{"7 min"}, "The wait is 7 min. Per day."},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || string([]rune(tt.text)[f.Start:f.End]) != want || i == 0 && tt.firstClause != "" && (f.Clause != tt.firstClause || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewApproximationAbbreviationsAndRateNotation(t *testing.T) {
	for _, tt := range []struct {
		text        string
		want        []string
		firstClause string
	}{
		{"The wait is 7 days and about $5 is charged. The deposit is $7.", []string{"7 days", "$7"}, ""},
		{"The fee is $5 and about 3 devices are included. The deposit is $7.", []string{"$5", "$7"}, ""},
		{"The wait is about 7 days and 3 hours. The fee is $7.", []string{"$7"}, ""},
		{"The wait is 7 min. Maximum capacity has increased.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. At least 2 devices are included.", []string{"7 min", "2 devices"}, "The wait is 7 min."},
		{"The wait is 7 min. Per policy no refund is available.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. maximum. The fee is $7.", []string{"$7"}, ""},
		{"The wait is 7 min. per day.", []string{"7 min"}, "The wait is 7 min. per day."},
		{"The fee is $5 per 10⁶ pages. The deposit is $7.", []string{"$7"}, ""},
		{"The fee is $5 per 10₆ pages. The deposit is $7.", []string{"$7"}, ""},
		{"The fee is $5 per １０６ pages. The deposit is $7.", []string{"$5", "$7"}, ""},
		{"Account number: 1234 / A5678. The fee is $7.", []string{"$7"}, ""},
		{"Account number: 1234. The fee is $7.", []string{"Account number: 1234", "$7"}, ""},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || string([]rune(tt.text)[f.Start:f.End]) != want || i == 0 && tt.firstClause != "" && (f.Clause != tt.firstClause || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewPercentageBaseAndIndependentSentences(t *testing.T) {
	for _, tt := range []struct {
		text        string
		want        []string
		firstClause string
	}{
		{"The fee is 5% (approximately) of $100. The deposit is $7.", []string{"$7"}, ""},
		{"The fee is 5% of $100 and $200 is deposited.", []string{"5%", "$200"}, ""},
		{"The fee is 5% of $100 and $200. The deposit is $7.", []string{"5%", "$7"}, ""},
		{"The fee is 5% of $100. The deposit is $7.", []string{"5%", "$7"}, ""},
		{"The wait is 7 min. Per customer no fee applies.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. Per day.", []string{"7 min"}, "The wait is 7 min. Per day."},
		{"The wait is 7 min. Approximately 2 devices are included.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. approximately. The fee is $7.", []string{"$7"}, ""},
		{"The wait is 7 min. maximum. The fee is $7.", []string{"$7"}, ""},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || string([]rune(tt.text)[f.Start:f.End]) != want || i == 0 && tt.firstClause != "" && (f.Clause != tt.firstClause || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewIntroductorySubjectsAndIncompleteAlternatives(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		period Period
		clause string
	}{
		{"The deadline is 1 July 2026, or 2 August. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"The fee is $5 until 1 July 2026, or 2 August. The deposit is $7.", []string{"$5", "$7"}, Period{}, ""},
		{"The fee is $5 until 1 July 2026, or 2. The deposit is $7.", []string{"$5", "$7"}, Period{}, ""},
		{"One in every ten devices fails. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"For members, the estimated fee is $5. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"For members, the minimum fee is $5. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"For members, the maximum fee is $5. The deposit is $7.", []string{"$7"}, Period{}, ""},
		{"For members, the estimated fee changed and the deposit is $7.", []string{"$7"}, Period{}, ""},
		{"Policy number: AB1234 / cd5678. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"Policy number: AB1234 / lower prose. The fee is $7.", []string{"Policy number: AB1234", "$7"}, Period{}, ""},
		{"The wait is 7 min. Per policy refunds are never issued.", []string{"7 min"}, Period{}, "The wait is 7 min."},
		{"The wait is 7 min. per day.", []string{"7 min"}, Period{}, "The wait is 7 min. per day."},
		{"The fee is $5 until 1 July 2026. The deposit is $7.", []string{"$5", "$7"}, Period{To: "2026-07-01"}, ""},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || (i == 0 && f.Period != tt.period) || string([]rune(tt.text)[f.Start:f.End]) != want || i == 0 && tt.clause != "" && (f.Clause != tt.clause || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewRateDenominatorIndependentAssertions(t *testing.T) {
	for _, tt := range []struct {
		text string
		want []string
	}{
		{"The fee is about $5 per 2 devices and 3 devices are included.", []string{"3 devices"}},
		{"The fee is about $5 per 2 days and 3 days are allowed.", []string{"3 days"}},
		{"The fee is about $5 per 2 percent and 3 percent is included.", []string{"3 percent"}},
		{"The fee is $5 per 2 devices and 3 devices are included.", []string{"$5", "2 devices", "3 devices"}},
		{"The fee is about $5 per 2 days and 3 hours. The deposit is $7.", []string{"$7"}},
		{"The fee is $5 per 2 days and 3 hours. The deposit is $7.", []string{"$5", "2 days", "3 hours", "$7"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || string([]rune(tt.text)[f.Start:f.End]) != want {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewWrappedIntervalsAndIndependentSentences(t *testing.T) {
	for _, tt := range []struct {
		text        string
		want        []string
		period      Period
		firstClause string
	}{
		{"Account number: 1234 / AB-5678. The fee is $7.", []string{"$7"}, Period{}, ""},
		{"Account number: 1234 / ab-5678 and the fee is $7.", []string{"$7"}, Period{}, ""},
		{"From 1 July 2026 until (1 August 2026) the fee is $5.", []string{"$5"}, Period{From: "2026-07-01", To: "2026-08-01"}, ""},
		{"From 1 July 2026 until (1 June 2026) the fee is $5.", []string{"$5"}, Period{}, ""},
		{"From 1 July 2026 until «1 August 2026» the fee is $5.", []string{"$5"}, Period{From: "2026-07-01", To: "2026-08-01"}, ""},
		{"From 1 July 2026 the fee is $5 because the office closes until (1 August 2026).", []string{"$5"}, Period{}, ""},
		{"The wait is 7 min. Per policy refunds aren't available.", []string{"7 min"}, Period{}, "The wait is 7 min."},
		{"The wait is 7 min. Excluding tax, the fee is $5.", []string{"7 min", "$5"}, Period{}, "The wait is 7 min."},
		{"The wait is 7 min. per day.", []string{"7 min"}, Period{}, "The wait is 7 min. per day."},
		{"The wait is 7 min. excluding weekends. The fee is $5.", []string{"$5"}, Period{}, ""},
		{"The deadline is (1 July 2026). The fee is $7.", []string{"1 July 2026", "$7"}, Period{}, ""},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || (i == 0 && f.Period != tt.period) || string([]rune(tt.text)[f.Start:f.End]) != want || i == 0 && tt.firstClause != "" && (f.Clause != tt.firstClause || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewQualifiedBoundariesAndIndependentGroups(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		period Period
	}{
		{"From 1 July 2026 at least 7 days are allowed.", []string{"7 days"}, Period{From: "2026-07-01"}},
		{"From 1 July 2026 at least ７ days are allowed.", []string{"７ days"}, Period{From: "2026-07-01"}},
		{"From (1 July 2026) until (1 August 2026) the fee is $5.", []string{"$5"}, Period{From: "2026-07-01", To: "2026-08-01"}},
		{"Policy number: AB1234/56, $5 fee.", []string{"$5"}, Period{}},
		{"The notice is due 2 days before (1 July 2026).", []string{"2 days"}, Period{}},
		{"The notice is due 2 days prior to «1 July 2026».", []string{"2 days"}, Period{}},
		{"The notice is due 2 days from (1 July 2026).", []string{"2 days"}, Period{}},
		{"The fee is about $5 and $10 is deposited.", []string{"$10"}, Period{}},
		{"The wait is about 7 days and 3 days are included.", []string{"3 days"}, Period{}},
		{"The fee is $5 and at least $10 is deposited.", []string{"$5", "$10"}, Period{}},
		{"Policy number: AB1234 and at least 2 devices are covered.", []string{"Policy number: AB1234", "2 devices"}, Period{}},
		{"The wait is about 7 days and 3 hours. The fee is $5.", []string{"$5"}, Period{}},
		{"The wait is 7 days and at least 3 hours. The fee is $5.", []string{"$5"}, Period{}},
		{"From 1 July 2026 until 1 August 2026 the fee is $5.", []string{"$5"}, Period{From: "2026-07-01", To: "2026-08-01"}},
		{"Policy number: AB1234/56. The fee is $5.", []string{"$5"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewEligibilityAndWrappedNominalQualifiers(t *testing.T) {
	for _, tt := range []struct {
		text        string
		want        []string
		period      Period
		firstClause string
	}{
		{"A fee of $5 applies to orders before 1 July 2026.", []string{"$5"}, Period{}, ""},
		{"From (1 July 2026) the estimated fee is $5. The deposit is $7.", []string{"1 July 2026", "$7"}, Period{}, ""},
		{"From (1 July 2026) the minimum fee is $5. The deposit is $7.", []string{"1 July 2026", "$7"}, Period{}, ""},
		{"From «1 July 2026» the maximum fee is $5. The deposit is $7.", []string{"1 July 2026", "$7"}, Period{}, ""},
		{"From (1 July 2026) the estimated fee is $5 and the deposit is $7.", []string{"$7"}, Period{From: "2026-07-01"}, ""},
		{"From (1 July 2026) until (1 August 2026) the fee is $5.", []string{"$5"}, Period{From: "2026-07-01", To: "2026-08-01"}, ""},
		{"The fee is $5 before 1 July 2026.", []string{"$5"}, Period{To: "2026-06-30"}, ""},
		{"The wait is 7 min. Per policy refunds cannot be issued.", []string{"7 min"}, Period{}, "The wait is 7 min."},
		{"The wait is 7 min. Per policy refunds aren't issued.", []string{"7 min"}, Period{}, "The wait is 7 min."},
		{"The wait is 7 min. per day.", []string{"7 min"}, Period{}, "The wait is 7 min. per day."},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want || i == 0 && tt.firstClause != "" && (f.Clause != tt.firstClause || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewNormalizedAbbreviationsAndDatedSubjects(t *testing.T) {
	for _, tt := range []struct {
		text          string
		want, clauses []string
		period        Period
	}{
		{"The wait is 7 min. Per policy the deadline is 1 July 2026.", []string{"7 min", "1 July 2026"}, []string{"The wait is 7 min.", "Per policy the deadline is 1 July 2026."}, Period{}},
		{"The wait is 7 min. per day.", []string{"7 min"}, []string{"The wait is 7 min. per day."}, Period{}},
		{"The estimated fee on 1 July 2026 is $5 and the deposit is $7.", []string{"$7"}, nil, Period{}},
		{"The minimum fee on 1 July 2026 is $5 and the deposit is $7.", []string{"$7"}, nil, Period{}},
		{"The maximum fee on 1 July 2026 is $5 and the deposit is $7.", []string{"$7"}, nil, Period{}},
		{"The wait is ７ ｍｉｎ. approximately. The fee is $7.", []string{"$7"}, nil, Period{}},
		{"The wait is ７ ｍｉｎ. ａｐｐｒｏｘｉｍａｔｅｌｙ. The fee is $7.", []string{"$7"}, nil, Period{}},
		{"The wait is ７ ｍｉｎ. The fee is $7.", []string{"７ ｍｉｎ", "$7"}, []string{"The wait is ７ ｍｉｎ.", "The fee is $7."}, Period{}},
		{"The fee is $5 until 1 July 2026 and $10 is deposited.", []string{"$5", "$10"}, nil, Period{}},
		{"The wait is 7 days < 10 and the fee is $5. The deposit is $7.", []string{"$7"}, nil, Period{}},
		{"Policy number: AB1234/56. The fee is $7.", []string{"$7"}, nil, Period{}},
		{"From 1 July 2026 until 1 August 2026 the fee is $5.", []string{"$5"}, nil, Period{From: "2026-07-01", To: "2026-08-01"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want || len(tt.clauses) > 0 && (f.Clause != tt.clauses[i] || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewPossessivesAndWrappedDateRanges(t *testing.T) {
	for _, tt := range []struct {
		text          string
		want, clauses []string
		period        Period
	}{
		{"The wait is 7 min. Per policy refunds start from 1 July 2026.", []string{"7 min", "1 July 2026"}, []string{"The wait is 7 min.", "Per policy refunds start from 1 July 2026."}, Period{}},
		{"The wait is 7 min. per day.", []string{"7 min"}, []string{"The wait is 7 min. per day."}, Period{}},
		{"Your estimated fee is $5. The deposit is $7.", []string{"$7"}, nil, Period{}},
		{"Your minimum fee is $5. The deposit is $7.", []string{"$7"}, nil, Period{}},
		{"Our maximum fee is $5. The deposit is $7.", []string{"$7"}, nil, Period{}},
		{"Your estimated fee changed and the deposit is $7.", []string{"$7"}, nil, Period{}},
		{"The deadline is (1 July 2026) / (1 August 2026). The fee is $7.", []string{"$7"}, nil, Period{}},
		{"The deadline is «1 July 2026» / «1 August 2026». The fee is $7.", []string{"$7"}, nil, Period{}},
		{"The fee is $5 from (1 July 2026) / (1 August 2026). The deposit is $7.", []string{"$5", "$7"}, nil, Period{}},
		{"The deadline is (1 July 2026). The fee is $7.", []string{"1 July 2026", "$7"}, nil, Period{}},
		{"The fee is $5/day. The deposit is $7.", []string{"$5", "$7"}, nil, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want || len(tt.clauses) > 0 && (f.Clause != tt.clauses[i] || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewUppercasePerAssertion(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		clause string
	}{
		{"The wait is 7 min. PER POLICY NO REFUND IS AVAILABLE.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. Per policy no refund is available.", []string{"7 min"}, "The wait is 7 min."},
		{"The wait is 7 min. per day.", []string{"7 min"}, "The wait is 7 min. per day."},
		{"The wait is 7 min. Per day.", []string{"7 min"}, "The wait is 7 min. Per day."},
		{"The wait is 7 min. PER DAY.", []string{"7 min"}, "The wait is 7 min. PER DAY."},
		{"The wait is 7 min. a day.", []string{"7 min"}, "The wait is 7 min. a day."},
		{"The wait is 7 min. an hour.", []string{"7 min"}, "The wait is 7 min. an hour."},
		{"The wait is 7 min. approximately. The fee is $7.", []string{"$7"}, "The fee is $7."},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Clause != tt.clause || string([]rune(tt.text)[f.Start:f.End]) != want || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewPaymentDeadlineScope(t *testing.T) {
	for _, tt := range []struct {
		text   string
		want   []string
		period Period
	}{
		{"The fee is $5 payable before 1 July 2026.", []string{"$5"}, Period{}},
		{"The fee is $5 payable before 1 July 2026. The deposit is $7.", []string{"$5", "$7"}, Period{}},
		{"The fee is $5 payable prior to 1 July 2026.", []string{"$5"}, Period{}},
		{"The fee is $5 due before 1 July 2026.", []string{"$5"}, Period{}},
		{"The fee is $5 payable on or before 1 July 2026.", []string{"$5"}, Period{}},
		{"The fee is $5 on or before 1 July 2026.", []string{"$5"}, Period{To: "2026-07-01"}},
		{"The fee is $5 before 1 July 2026.", []string{"$5"}, Period{To: "2026-06-30"}},
		{"The fee is $5 from 1 July 2026.", []string{"$5"}, Period{From: "2026-07-01"}},
		{"The fee is $5 until 1 July 2026.", []string{"$5"}, Period{To: "2026-07-01"}},
		{"The deadline is 1 July 2026. The fee is $7.", []string{"1 July 2026", "$7"}, Period{}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || (i == 0 && f.Period != tt.period) || string([]rune(tt.text)[f.Start:f.End]) != want {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}

func TestExtractReviewNominalAndCompleteDateRanges(t *testing.T) {
	for _, tt := range []struct {
		text          string
		want, clauses []string
		period        Period
	}{
		{"For members the estimated fee is $5. The deposit is $7.", []string{"$7"}, nil, Period{}},
		{"For members the minimum fee is $5. The deposit is $7.", []string{"$7"}, nil, Period{}},
		{"For members the maximum fee is $5. The deposit is $7.", []string{"$7"}, nil, Period{}},
		{"The estimated fee is five dollars and shipping is $7.", []string{"$7"}, nil, Period{}},
		{"The estimated fee is 5 dollars and shipping is $7.", []string{"$7"}, nil, Period{}},
		{"The estimated shipping and handling fee is $5. The deposit is $7.", []string{"$7"}, nil, Period{}},
		{"The wait is 7 min. Per policy the refund deadline is 1 July 2026.", []string{"7 min", "1 July 2026"}, []string{"The wait is 7 min.", "Per policy the refund deadline is 1 July 2026."}, Period{}},
		{"The wait is 7 min. per day.", []string{"7 min"}, []string{"The wait is 7 min. per day."}, Period{}},
		{"The deadline is between 1 July 2026 and 1 August 2026. The fee is $7.", []string{"$7"}, nil, Period{}},
		{"The deadline is 1 July 2026 to 1 August 2026. The fee is $7.", []string{"$7"}, nil, Period{}},
		{"The deadlines are 1 July 2026 and 1 August 2026. The fee is $7.", []string{"1 July 2026", "1 August 2026", "$7"}, nil, Period{}},
		{"From 1 July 2026 to 1 August 2026 the fee is $5.", []string{"$5"}, nil, Period{From: "2026-07-01", To: "2026-08-01"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			got := Extract(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v want %v", got, tt.want)
			}
			for i, want := range tt.want {
				f := got[i]
				if f.Text != want || f.Period != tt.period || string([]rune(tt.text)[f.Start:f.End]) != want || len(tt.clauses) > 0 && (f.Clause != tt.clauses[i] || string([]rune(tt.text)[f.ClauseStart:f.ClauseEnd]) != f.Clause) {
					t.Errorf("got %+v want %q", f, want)
				}
			}
		})
	}
}
