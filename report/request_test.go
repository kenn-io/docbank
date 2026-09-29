package report

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func validRequest() Request {
	return Request{
		Version:      1,
		AllDocuments: true,
		Timezone:     "UTC",
		CoverageMode: "strict",
		Terms: []Term{{
			Number: 1, Expression: "alpha AND beta", Syntax: "advanced",
			Dates: DateRange{Start: "2024-01-01", End: "2024-12-31"},
		}},
	}
}

func TestRequestRequiresExplicitCutoff(t *testing.T) {
	r := validRequest()
	r.Terms[0].Dates.End = "present"
	if _, err := NormalizeRequest(r); err == nil {
		t.Fatal("accepted a moving cutoff")
	}
}

func TestRequestRejectsInvalidScopeDatesAndTerms(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Request)
	}{
		{"version", func(r *Request) { r.Version = 2 }},
		{"no scope", func(r *Request) { r.AllDocuments = false }},
		{"both scopes", func(r *Request) { r.CollectionIDs = []string{"c1"} }},
		{"duplicate collection", func(r *Request) { r.AllDocuments = false; r.CollectionIDs = []string{"c1", "c1"} }},
		{"unknown timezone", func(r *Request) { r.Timezone = "Earth/Atlantis" }},
		{"reversed range", func(r *Request) { r.Terms[0].Dates.End = "2023-12-31" }},
		{"impossible date", func(r *Request) { r.Terms[0].Dates.Start = "2024-02-30" }},
		{"unknown syntax", func(r *Request) { r.Terms[0].Syntax = "magic" }},
		{"zero number", func(r *Request) { r.Terms[0].Number = 0 }},
		{"duplicate number", func(r *Request) { r.Terms = append(r.Terms, r.Terms[0]) }},
		{"empty expression", func(r *Request) { r.Terms[0].Expression = "  " }},
		{"long expression", func(r *Request) { r.Terms[0].Expression = strings.Repeat("界", 8193) }},
		{"too many terms", func(r *Request) {
			for i := 2; i <= 129; i++ {
				r.Terms = append(r.Terms, Term{Number: i, Expression: "x", Syntax: "simple", Dates: r.Terms[0].Dates})
			}
		}},
		{"unknown coverage", func(r *Request) { r.CoverageMode = "guess" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRequest()
			tc.edit(&r)
			if _, err := NormalizeRequest(r); err == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
}

func TestRequestPreservesOrderedRowsAndLiteralDates(t *testing.T) {
	r := validRequest()
	r.AllDocuments = false
	r.CollectionIDs = []string{"c2", "c1"}
	r.Terms = []Term{
		{Number: 20, Expression: "alpha", Syntax: "simple", Dates: DateRange{"2024-01-01", "2024-01-31"}},
		{Number: 3, Expression: "beta OR gamma", Syntax: "advanced", Dates: DateRange{"2024-02-01", "2024-02-29"}},
	}
	got, err := NormalizeRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Terms[0].Number != 20 || got.Terms[1].Number != 3 || got.Terms[1].Dates.End != "2024-02-29" {
		t.Fatalf("row order or cutoff changed: %+v", got.Terms)
	}
	if got.CollectionIDs[0] != "c2" || got.CollectionIDs[1] != "c1" {
		t.Fatalf("collection order changed: %v", got.CollectionIDs)
	}
}

func TestRequestValidatesReviewedChoiceShape(t *testing.T) {
	base := DateChoice{
		Document:    Identity{NodeID: 1, VersionID: "v1", SHA256: strings.Repeat("a", 64)},
		CandidateID: "candidate", EvidenceSHA256: strings.Repeat("b", 64),
		Reason: "Reviewed source date", Action: "select",
	}
	cases := []struct {
		name  string
		edit  func(*DateChoice)
		valid bool
	}{
		{"select", func(*DateChoice) {}, true},
		{"missing reason", func(c *DateChoice) { c.Reason = "" }, false},
		{"missing binding", func(c *DateChoice) { c.CandidateID = "" }, false},
		{"select replacement", func(c *DateChoice) { c.ReviewedDate = "2024-01-01" }, false},
		{"interpret", func(c *DateChoice) { c.Action = "interpret"; c.ReviewedDate = "2024-03-04"; c.ReviewedTimezone = "UTC" }, true},
		{"interpret missing date", func(c *DateChoice) { c.Action = "interpret"; c.ReviewedTimezone = "UTC" }, false},
		{"reclassify", func(c *DateChoice) { c.Action = "reclassify"; c.ReviewedRole = "document_date" }, true},
		{"reclassify missing role", func(c *DateChoice) { c.Action = "reclassify" }, false},
		{"unknown action", func(c *DateChoice) { c.Action = "replace" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRequest()
			choice := base
			tc.edit(&choice)
			r.DateChoices = []DateChoice{choice}
			_, err := NormalizeRequest(r)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestRequestSelectedDocuments(t *testing.T) {
	first := Identity{1, "20000000-0000-4000-8000-000000000001", strings.Repeat("a", 64)}
	second := Identity{2, "20000000-0000-4000-8000-000000000002", strings.Repeat("b", 64)}
	r := validRequest()
	r.AllDocuments = false
	r.SelectedDocuments = &SelectedDocuments{Documents: []Identity{second, first}}
	got, err := NormalizeRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.SelectedDocuments.Documents, []Identity{first, second}) {
		t.Fatalf("selection: %+v", got.SelectedDocuments)
	}
	if r.SelectedDocuments.Documents[0] != second {
		t.Fatal("sorted caller's selection")
	}
	r.SelectedDocuments.Documents[1].NodeID = 99
	r.SelectedDocuments.Documents = nil
	if got.SelectedDocuments.Documents[0] != first {
		t.Fatal("normalized selection aliases input")
	}
	for _, tc := range []struct {
		name string
		edit func(*Request)
	}{
		{"all and selected", func(r *Request) { r.AllDocuments = true }},
		{"collection and selected", func(r *Request) { r.CollectionIDs = []string{"c1"} }},
		{"missing list", func(r *Request) { r.SelectedDocuments.Documents = nil }},
		{"empty list", func(r *Request) { r.SelectedDocuments.Documents = []Identity{} }},
		{"duplicate node", func(r *Request) { r.SelectedDocuments.Documents = append(r.SelectedDocuments.Documents, first) }},
		{"two versions", func(r *Request) {
			r.SelectedDocuments.Documents = append(r.SelectedDocuments.Documents, Identity{1, second.VersionID, second.SHA256})
		}},
		{"nonpositive node", func(r *Request) { r.SelectedDocuments.Documents[0].NodeID = 0 }},
		{"invalid version", func(r *Request) { r.SelectedDocuments.Documents[0].VersionID = "v1" }},
		{"uppercase version", func(r *Request) { r.SelectedDocuments.Documents[0].VersionID = "ABCDEF00-0000-4000-8000-000000000001" }},
		{"wrong version", func(r *Request) { r.SelectedDocuments.Documents[0].VersionID = "20000000-0000-5000-8000-000000000001" }},
		{"wrong variant", func(r *Request) { r.SelectedDocuments.Documents[0].VersionID = "20000000-0000-4000-0000-000000000001" }},
		{"invalid hash", func(r *Request) { r.SelectedDocuments.Documents[0].SHA256 = "bad" }},
		{"uppercase hash", func(r *Request) { r.SelectedDocuments.Documents[0].SHA256 = strings.Repeat("A", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := validRequest()
			bad.AllDocuments = false
			bad.SelectedDocuments = &SelectedDocuments{Documents: []Identity{first}}
			tc.edit(&bad)
			if _, err := NormalizeRequest(bad); err == nil {
				t.Fatal("accepted invalid selection")
			}
		})
	}
	r.SelectedDocuments.Documents = make([]Identity, 50001)
	for i := range r.SelectedDocuments.Documents {
		r.SelectedDocuments.Documents[i] = Identity{int64(i + 1), fmt.Sprintf("20000000-0000-4000-8000-%012d", i+1), first.SHA256}
	}
	if _, err := NormalizeRequest(r); !errors.Is(err, ErrReportLimit) {
		t.Fatalf("selected limit: %v", err)
	}
	r.SelectedDocuments.Documents = r.SelectedDocuments.Documents[:50000]
	if _, err := NormalizeRequest(r); err != nil {
		t.Fatalf("maximum selection: %v", err)
	}
	r.SelectedDocuments = nil
	r.CollectionIDs = make([]string, 50001)
	if _, err := NormalizeRequest(r); err == nil || errors.Is(err, ErrReportLimit) {
		t.Fatalf("collection limit changed: %v", err)
	}
}
