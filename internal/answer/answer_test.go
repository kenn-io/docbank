package answer

import (
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
