package answer

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClassifyVisibleEvidence(t *testing.T) {
	tests := []struct {
		name                string
		evidence            Evidence
		complete, truncated bool
		want                Status
	}{
		{"empty complete", Evidence{}, true, false, NoMatch},
		{"empty incomplete", Evidence{}, false, false, IncompleteCoverage},
		{"lead", Evidence{Leads: []Lead{{}}}, true, false, IncompleteCoverage},
		{"truncated", Evidence{}, true, true, IncompleteCoverage},
		{"passage", Evidence{Passages: []Passage{{Text: "yes"}}}, false, false, Answered},
		{"outdated", Evidence{Passages: []Passage{{Text: "old", Outdated: &Supersession{}}}}, true, false, OutdatedOnly},
		{"outdated with omitted candidates", Evidence{Passages: []Passage{{Text: "old", Outdated: &Supersession{}}}}, true, true, IncompleteCoverage},
		{"outdated with lead", Evidence{Passages: []Passage{{Text: "old", Outdated: &Supersession{}}}, Leads: []Lead{{}}}, true, false, IncompleteCoverage},
		{"one conflict", Evidence{Passages: []Passage{{Text: "one", ConflictGroup: "a"}}}, true, false, Answered},
		{"two conflicts", Evidence{Passages: []Passage{{Text: "one", ConflictGroup: "a"}, {Text: "two", ConflictGroup: "a"}}}, true, false, Conflicting},
		{"dropped", Evidence{Passages: []Passage{{Text: "one", Dropped: "threshold"}}}, true, false, NoMatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.evidence, tt.complete, tt.truncated)
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func FuzzBudgetPrefix(f *testing.F) {
	f.Add("é|abc|x", uint32(4))
	f.Add("|😀|", uint32(0))
	f.Add("", uint32(65536))
	f.Fuzz(func(t *testing.T, raw string, budget uint32) {
		// Cap materialized input to keep a single fuzz case inexpensive.
		if len(raw) > 4096 {
			raw = raw[:4096]
		}
		raw = strings.ToValidUTF8(raw, "�")
		texts := strings.Split(raw, "|")
		if raw == "" {
			texts = nil
		}
		if len(texts) > 128 {
			texts = texts[:128]
		}
		passages := make([]Passage, len(texts))
		for i, text := range texts {
			passages[i] = Passage{Text: text, NodeID: int64(i + 1), Rank: i + 1, EvidenceIDs: []string{"synthetic"}}
		}
		before := slices.Clone(passages)
		for i := range before {
			before[i].EvidenceIDs = slices.Clone(before[i].EvidenceIDs)
		}
		got, n, omitted := Budget(passages, int(budget))
		sum := 0
		if len(got)+omitted != len(passages) {
			t.Fatal("lost passages")
		}
		for i, p := range got {
			if !reflect.DeepEqual(p, before[i]) || !utf8.ValidString(p.Text) {
				t.Fatal("invalid prefix")
			}
			sum += len(p.Text)
		}
		if sum != n || n > int(budget) {
			t.Fatal("incorrect byte count")
		}
		if len(got) < len(before) && n+len(before[len(got)].Text) <= int(budget) {
			t.Fatal("prefix is not maximal")
		}
	})
}

func FuzzClassify(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3}, true, false)
	f.Add([]byte{}, true, false)
	f.Add([]byte{4, 4}, false, true)
	f.Add([]byte{2, 32, 96}, true, false)
	f.Fuzz(func(t *testing.T, raw []byte, complete, truncated bool) {
		if len(raw) > 128 {
			raw = raw[:128]
		}
		var e Evidence
		for _, b := range raw {
			if b&32 != 0 {
				l := Lead{}
				if b&64 != 0 {
					l.Dropped = "filtered"
				}
				e.Leads = append(e.Leads, l)
				continue
			}
			p := Passage{Text: "x"}
			if b&1 != 0 {
				p.Dropped = "filtered"
			}
			if b&2 != 0 {
				p.Outdated = &Supersession{}
			}
			switch (b >> 2) & 3 {
			case 1:
				p.ConflictGroup = "a"
			case 2:
				p.ConflictGroup = "b"
			case 3:
				p.ConflictGroup = "c"
			}
			e.Passages = append(e.Passages, p)
		}
		// These predicates describe visible witnesses, independently of the
		// classifier's counting implementation. Conflicts require a pair.
		current := func(p Passage) bool { return p.Dropped == "" && p.Outdated == nil }
		hasCurrent := slices.ContainsFunc(e.Passages, current)
		hasOutdated := slices.ContainsFunc(e.Passages, func(p Passage) bool {
			return p.Dropped == "" && p.Outdated != nil
		})
		hasLead := slices.ContainsFunc(e.Leads, func(l Lead) bool { return l.Dropped == "" })
		conflict := false
		for i, a := range e.Passages {
			for _, b := range e.Passages[i+1:] {
				if current(a) && current(b) && a.ConflictGroup != "" && a.ConflictGroup == b.ConflictGroup {
					conflict = true
				}
			}
		}
		s := Classify(e, complete, truncated)
		switch s {
		case Answered:
			if !hasCurrent || conflict {
				t.Fatal("invalid answer")
			}
		case Conflicting:
			if !conflict {
				t.Fatal("invalid conflict")
			}
		case OutdatedOnly:
			if hasCurrent || !hasOutdated || hasLead || !complete || truncated {
				t.Fatal("invalid outdated-only")
			}
		case NoMatch:
			if hasCurrent || hasOutdated || hasLead || !complete || truncated {
				t.Fatal("false no match")
			}
		case IncompleteCoverage:
			if hasCurrent || (complete && !truncated && !hasLead) {
				t.Fatal("invalid incomplete status")
			}
		default:
			t.Fatalf("unknown status %q", s)
		}
	})
}

func FuzzClassifyDroppedEvidence(f *testing.F) {
	f.Add(uint8(0), uint8(0), false, false)
	f.Add(uint8(1), uint8(2), true, true)
	f.Fuzz(func(t *testing.T, passages, leads uint8, complete, truncated bool) {
		e := Evidence{}
		for i := range int(passages) {
			e.Passages = append(e.Passages, Passage{Text: "x", ConflictGroup: string(rune('a' + i%3))})
		}
		for range int(leads) {
			e.Leads = append(e.Leads, Lead{})
		}
		want := Classify(e, complete, truncated)
		e.Passages = append(e.Passages,
			Passage{Text: "dropped", Dropped: "threshold", ConflictGroup: "a"},
			Passage{Text: "dropped old", Dropped: "threshold", Outdated: &Supersession{}})
		e.Leads = append(e.Leads, Lead{Dropped: "threshold"})
		if got := Classify(e, complete, truncated); got != want {
			t.Fatalf("dropped evidence changed %s to %s", want, got)
		}
	})
}

func FuzzClassifyCurrentEvidence(f *testing.F) {
	f.Add(uint8(0), uint8(0), false, false)
	f.Fuzz(func(t *testing.T, outdated, leads uint8, complete, truncated bool) {
		e := Evidence{Passages: []Passage{{Text: "current"}}}
		for range int(outdated) {
			e.Passages = append(e.Passages, Passage{Text: "old", Outdated: &Supersession{}})
		}
		for range int(leads) {
			e.Leads = append(e.Leads, Lead{})
		}
		if got := Classify(e, complete, truncated); got != Answered {
			t.Fatalf("current evidence classified as %s", got)
		}
	})
}

func FuzzClassifyConflictPair(f *testing.F) {
	f.Add("a", true, false)
	f.Add("", false, true)
	f.Fuzz(func(t *testing.T, group string, complete, truncated bool) {
		// Every generated group is nonempty, including an empty fuzz string.
		group = "group:" + group
		e := Evidence{Passages: []Passage{{Text: "a", ConflictGroup: group}, {Text: "b", ConflictGroup: group}}}
		if got := Classify(e, complete, truncated); got != Conflicting {
			t.Fatalf("visible conflict classified as %s", got)
		}
		for _, second := range []Passage{
			{Text: "old", ConflictGroup: group, Outdated: &Supersession{}},
			{Text: "dropped", ConflictGroup: group, Dropped: "threshold"},
		} {
			e.Passages[1] = second
			if got := Classify(e, complete, truncated); got != Answered {
				t.Fatalf("one current conflict member classified as %s", got)
			}
		}
	})
}

type testGate struct{ fail bool }

func (testGate) ID() string      { return "test" }
func (testGate) Version() string { return "1" }
func (g testGate) Apply(_ context.Context, _ Question, e Evidence) (Evidence, GateOutcome, error) {
	if g.fail {
		return Evidence{}, GateOutcome{}, errors.New("gate failed")
	}
	e.Passages[0].Dropped = "filtered"
	return e, GateOutcome{Outcome: "applied", Dropped: 1}, nil
}
func TestApplyGates(t *testing.T) {
	e := Evidence{Passages: []Passage{{Text: "a"}, {Text: "b"}}}
	got, receipts, err := ApplyGates(context.Background(), Question{}, e, []Gate{testGate{}})
	if err != nil || len(got.Passages) != 1 || got.Passages[0].Text != "b" || len(receipts) != 1 || receipts[0].Dropped != 1 {
		t.Fatalf("got %+v %+v %v", got, receipts, err)
	}
	_, _, err = ApplyGates(context.Background(), Question{}, e, []Gate{testGate{fail: true}})
	if err == nil {
		t.Fatal("gate failure was ignored")
	}
}
