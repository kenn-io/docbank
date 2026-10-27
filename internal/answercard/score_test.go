package answercard

import (
	"strings"
	"testing"
)

func TestScore(t *testing.T) {
	for _, tc := range []struct {
		q, p string
		want float64
	}{
		{"When does the policy renew?", "when does policy renew", 1},
		{"ＰＯＬＩＣＹ renewal", "policy renewal", 1},
		{"policy renewal", "library hours", 0},
		{"2025 tax payment", "2024 tax payment", 0},
		{"tax payment", "2024 tax payment", 0},
		{"temperature -5 C", "temperature 5 C", 0},
		{"temperature −5 C", "temperature 5 C", 0},
		{"temperature +5 C", "temperature 5 C", 0},
		{"rate .5 percent", "rate 5 percent", 0},
		{"rate -.5 percent", "rate .5 percent", 0},
		{"rate ,5 percent", "rate 5 percent", 0},
		{"rate −,5 percent", "rate ,5 percent", 0},
		{"rate −.5 percent", "rate -.5 percent", 1},
		{"rate .5 percent", "rate .5 percent", 1},
		{"rate 1.25 percent", "rate 25.1 percent", 0},
		{"renewal 01/02/2026", "renewal 02/01/2026", 0},
		{"the and ?", "the and ?", 0},
		{"policy renewal date", "policy renewal", .8},
	} {
		if got := Score(tc.q, tc.p); got != tc.want {
			t.Errorf("Score(%q,%q)=%v want %v", tc.q, tc.p, got, tc.want)
		}
	}
	c := Card{Question: "library opening hours", Alternates: []string{"when does policy renew"}}
	if score, phrase, count := c.score("when does policy renew"); score != 1 || phrase != c.Alternates[0] || count != 1 {
		t.Fatal(score, phrase, count)
	}
}

func FuzzScore(f *testing.F) {
	f.Add("policy renewal", "POLICY renewal")
	f.Add("rate 1.25", "rate 25.1")
	f.Fuzz(func(t *testing.T, a, b string) {
		number := "31415926"
		for strings.Contains(normalize(a), number) {
			number += "9"
		}
		if Score("rate "+a+" "+number, "rate "+a+" "+number+"1") != 0 {
			t.Fatal("numeric substitution matched")
		}
		fraction := number + "1"
		for _, separator := range []string{".", ","} {
			plain := "rate " + separator + fraction + " percent"
			if Score(plain, "rate "+fraction+" percent") != 0 {
				t.Fatal("fraction matched its integer digits")
			}
			for _, sign := range []string{"-", "−", "+"} {
				if Score("rate "+sign+separator+fraction+" percent", plain) != 0 {
					t.Fatal("fraction sign discarded")
				}
			}
		}
		s := Score(a, b)
		if s < 0 || s > 1 || s != Score(b, a) {
			t.Fatalf("invalid score %v", s)
		}
		c := Card{Question: a}
		before, _, _ := c.score(b)
		c.Alternates = []string{b}
		after, _, _ := c.score(b)
		if after < before {
			t.Fatal("alternate lowered score")
		}
	})
}

func TestCardRejectsUnsearchablePhrasing(t *testing.T) {
	for _, phrase := range []string{"the and", "!!!"} {
		c, err := Parse([]byte(sample))
		if err != nil {
			t.Fatal(err)
		}
		c.Question = phrase
		if c.Validate() == nil {
			t.Fatalf("accepted unsearchable phrase %q", phrase)
		}
	}
}
