package report

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func oracleFrame() Frame {
	request := Request{
		Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "strict",
		Terms: []Term{
			{Number: 1, Expression: "A", Syntax: "simple", Dates: DateRange{"2024-01-01", "2024-12-31"}},
			{Number: 2, Expression: "B", Syntax: "simple", Dates: DateRange{"2024-01-01", "2024-12-31"}},
		},
	}
	definitions := []struct {
		family string
		match  []bool
	}{
		{"v1", []bool{true, false}},
		{"v1", []bool{false, true}},
		{"v1", []bool{false, false}},
		{"v4", []bool{true, false}},
		{"v4", []bool{false, false}},
		{"", []bool{true, true}},
		{"", []bool{false, false}},
	}
	members := make([]Member, len(definitions))
	for i, definition := range definitions {
		members[i] = Member{
			Identity: Identity{NodeID: int64(i + 1), VersionID: fmt.Sprintf("v%d", i+1), SHA256: strings.Repeat("a", 64)},
			Kind:     "other", FamilyID: definition.family,
			Coverage:   MemberCoverage{SearchState: "complete", DateEvidenceState: "complete", FamilyState: "complete"},
			Selection:  DateSelection{CandidateID: "date", Date: "2024-06-01", RuleID: DateRuleV1, Reason: "vault_addition", Mode: "automatic"},
			RawMatches: definition.match,
		}
		members[i].Candidates = []DateCandidate{{ID: "date", Document: members[i].Identity,
			Role: "imported", SourceClass: "vault_addition", Value: "2024-06-01", Precision: "date",
			Locator: Locator{EvidenceSHA256: strings.Repeat("b", 64)}}}
	}
	relations := []Relation{
		{Parent: members[0].Identity, Child: members[1].Identity, EvidenceID: "r1", EvidenceSHA256: strings.Repeat("b", 64)},
		{Parent: members[1].Identity, Child: members[2].Identity, EvidenceID: "r2", EvidenceSHA256: strings.Repeat("b", 64)},
		{Parent: members[3].Identity, Child: members[4].Identity, EvidenceID: "r3", EvidenceSHA256: strings.Repeat("b", 64)},
	}
	return Frame{VaultID: "synthetic-vault", GenerationKind: "native",
		ObservedAt: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
		Request:    request, Members: members, Relations: relations}
}

func TestCountsMatchSevenDocumentOracle(t *testing.T) {
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	got, err := Calculate(context.Background(), budget, oracleFrame())
	want := []Counts{{3, 6, 2, 2, 5}, {2, 4, 1, 0, 3}}
	if err != nil || !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("counts=%+v err=%v", got.Counts, err)
	}
	if !got.Frame.Members[0].Eligible[0] || !got.Frame.Members[0].Hits[0] || got.Frame.Members[0].Hits[1] {
		t.Fatalf("member populations wrong: %+v", got.Frame.Members[0])
	}
}

func TestCountsKeepFamilyInsideCollectionScope(t *testing.T) {
	frame := oracleFrame()
	frame.Members = append(frame.Members[:1], frame.Members[2:]...) // b is outside the selected collection.
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	got, err := Calculate(context.Background(), budget, frame)
	want := []Counts{{3, 5, 2, 4, 4}, {1, 1, 0, 0, 0}}
	if err != nil || !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("counts=%+v err=%v", got.Counts, err)
	}
}

func TestCountsKeepFamilyInsideEachTermDateRange(t *testing.T) {
	frame := oracleFrame()
	frame.Members[1].Selection.Date = "2024-06-01" // b matches B only.
	for i := range frame.Members {
		if i != 1 {
			frame.Members[i].Selection.Date = "2024-06-03"
		}
	}
	frame.Request.Terms[0].Dates.Start = "2024-06-02"
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	got, err := Calculate(context.Background(), budget, frame)
	want := []Counts{{3, 5, 2, 4, 4}, {2, 4, 1, 0, 3}}
	if err != nil || !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("counts=%+v err=%v", got.Counts, err)
	}
}

func TestCountsCompetingTermDateRangeLimitsFamilyUniqueness(t *testing.T) {
	frame := oracleFrame()
	frame.Members[1].Selection.Date = "2024-06-01"
	for i := range frame.Members {
		if i != 1 {
			frame.Members[i].Selection.Date = "2024-06-03"
		}
	}
	frame.Request.Terms[1].Dates.Start = "2024-06-02"
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	got, err := Calculate(context.Background(), budget, frame)
	want := []Counts{{3, 6, 2, 5, 5}, {1, 1, 0, 0, 0}}
	if err != nil || !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("counts=%+v err=%v", got.Counts, err)
	}
}

func TestCountsRejectMemberWithoutSelectedCollectionWitness(t *testing.T) {
	frame := oracleFrame()
	frame.Request.AllDocuments = false
	frame.Request.CollectionIDs = []string{"selected"}
	frame.Members[0].CollectionWitnesses = []CollectionWitness{{CollectionID: "other", MembershipID: "m1", MembershipSHA256: strings.Repeat("a", 64)}}
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	if _, err := Calculate(context.Background(), budget, frame); err == nil {
		t.Fatal("counted a member lacking a selected collection witness")
	}
}

func TestCountsRejectDuplicateIdentity(t *testing.T) {
	frame := oracleFrame()
	frame.Members[1].Identity = frame.Members[0].Identity
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	if _, err := Calculate(context.Background(), budget, frame); err == nil {
		t.Fatal("counted the same document twice")
	}
}

func TestCountsEmptyPopulationIsZero(t *testing.T) {
	frame := oracleFrame()
	frame.Members = nil
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	got, err := Calculate(context.Background(), budget, frame)
	if err != nil || len(got.Counts) != 2 || got.Counts[0] != (Counts{}) || got.Counts[1] != (Counts{}) {
		t.Fatalf("counts=%+v err=%v", got.Counts, err)
	}
}

// selectedFrame reuses the date evidence fixture with canonical version identities.
func selectedFrame() Frame {
	f := oracleFrame()
	f.Members = f.Members[:2]
	f.Relations = nil
	f.Request.AllDocuments = false
	f.Request.CoverageMode = "available_only"
	f.Request.Terms = f.Request.Terms[:1]
	f.Request.Terms[0].Expression = "alpha"
	f.Request.SelectedDocuments = &SelectedDocuments{}
	for i := range f.Members {
		m := &f.Members[i]
		m.Identity.VersionID = fmt.Sprintf("20000000-0000-4000-8000-%012d", i+1)
		m.FamilyID = ""
		m.Candidates[0].Document = m.Identity
		m.RawMatches = []bool{true}
		f.Request.SelectedDocuments.Documents = append(f.Request.SelectedDocuments.Documents, m.Identity)
	}
	f.Members[1].Candidates = nil
	f.Members[1].Selection = DateSelection{}
	return f
}

func TestSelectedReportCountsAndMembership(t *testing.T) {
	budget := NewBudget(4 << 20)
	defer func() { _ = budget.Close() }()
	got, err := Calculate(t.Context(), budget, selectedFrame())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Frame.Members) != 2 || got.Counts[0].Hits != 1 || got.Frame.Coverage.Scoped != 1 {
		t.Fatalf("members=%d counts=%+v coverage=%+v", len(got.Frame.Members), got.Counts, got.Frame.Coverage)
	}
	for _, tc := range []struct {
		name string
		edit func(*Frame)
	}{
		{"missing", func(f *Frame) { f.Members = f.Members[:1] }},
		{"extra", func(f *Frame) { m := f.Members[1]; m.Identity.NodeID = 3; f.Members = append(f.Members, m) }},
		{"repeated node", func(f *Frame) { f.Members[1].Identity = f.Members[0].Identity }},
		{"substituted version", func(f *Frame) { f.Members[1].Identity.VersionID = "20000000-0000-4000-8000-000000000003" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := selectedFrame()
			tc.edit(&f)
			if _, err := Calculate(t.Context(), budget, f); err == nil {
				t.Fatal("accepted unequal member set")
			}
		})
	}
}
