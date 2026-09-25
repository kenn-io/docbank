package report

import (
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
		{"version", func(r *Request) { r.Version = 3 }},
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

func TestReportSealedRequestKeepsExactDocuments(t *testing.T) {
	r := validRequest()
	r.Version = 2
	r.AllDocuments = false
	r.SelectedDocuments = []Identity{{NodeID: 2, VersionID: "version-b", SHA256: strings.Repeat("b", 64)},
		{NodeID: 1, VersionID: "version-a", SHA256: strings.Repeat("a", 64)}}
	got, err := NormalizeRequest(r)
	if err != nil || len(got.SelectedDocuments) != 2 || got.SelectedDocuments[0] != r.SelectedDocuments[0] {
		t.Fatalf("sealed selection changed: %+v, %v", got.SelectedDocuments, err)
	}
	r.SelectedDocuments[0].VersionID = "changed"
	if got.SelectedDocuments[0].VersionID != "version-b" {
		t.Fatal("normalized request aliases caller selection")
	}
	for _, edit := range []func(*Request){
		func(r *Request) { r.SelectedDocuments[1] = r.SelectedDocuments[0] },
		func(r *Request) { r.SelectedDocuments[1].NodeID = r.SelectedDocuments[0].NodeID },
		func(r *Request) { r.SelectedDocuments[1].SHA256 = "bad" },
		func(r *Request) { r.CollectionIDs = []string{"collection"} },
		func(r *Request) { r.AllDocuments = true },
	} {
		candidate := got
		candidate.SelectedDocuments = append([]Identity(nil), got.SelectedDocuments...)
		edit(&candidate)
		if _, err := NormalizeRequest(candidate); err == nil {
			t.Fatalf("accepted invalid sealed selection: %+v", candidate)
		}
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
