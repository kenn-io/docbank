package report

import (
	"errors"
	"strings"
	"testing"
)

func TestReviewedChoiceDistinguishesInvalidInputFromStaleEvidence(t *testing.T) {
	identity := Identity{NodeID: 1, VersionID: "v1", SHA256: strings.Repeat("a", 64)}
	candidate := DateCandidate{ID: "candidate", Document: identity, Value: "2024-06-01",
		Locator: Locator{EvidenceSHA256: strings.Repeat("b", 64)}}
	choice := DateChoice{Document: identity, CandidateID: candidate.ID,
		EvidenceSHA256: candidate.Locator.EvidenceSHA256, Action: "select"}
	_, malformed := SelectDate("other", []DateCandidate{candidate}, &choice, Request{Timezone: "UTC"})
	if !errors.Is(malformed, ErrInvalidChoice) {
		t.Fatalf("missing reason must be invalid input: %v", malformed)
	}
	choice.Reason = "Reviewed source date"
	choice.EvidenceSHA256 = strings.Repeat("c", 64)
	_, stale := SelectDate("other", []DateCandidate{candidate}, &choice, Request{Timezone: "UTC"})
	if !errors.Is(stale, ErrStaleChoice) || errors.Is(stale, ErrInvalidChoice) {
		t.Fatalf("stale evidence must differ from invalid input: %v", stale)
	}
}

func TestEffectiveDateDoesNotReplaceCreation(t *testing.T) {
	candidates := []DateCandidate{
		{ID: "created", Role: "created", SourceClass: "native", Value: "2024-05-02", Precision: "date"},
		{ID: "effective", Role: "effective", SourceClass: "content", Value: "2023-01-01", Precision: "date"},
	}
	got, err := SelectDate("other", candidates, nil, Request{Timezone: "UTC"})
	if err != nil || got.CandidateID != "created" || got.Date != "2024-05-02" {
		t.Fatalf("selection=%+v err=%v", got, err)
	}
}

func TestDatePolicyUsesSentAndDocumentDateBeforeMetadata(t *testing.T) {
	cases := []struct {
		kind       string
		candidates []DateCandidate
		want       string
	}{
		{"email", []DateCandidate{
			{ID: "sent", Role: "sent", SourceClass: "native", Value: "2024-01-03", Precision: "date"},
			{ID: "metadata", Role: "created", SourceClass: "source_metadata", Value: "2022-01-01", Precision: "date"},
		}, "sent"},
		{"other", []DateCandidate{
			{ID: "document", Role: "document_date", SourceClass: "content", Value: "2024-02-03", Precision: "date"},
			{ID: "metadata", Role: "created", SourceClass: "source_metadata", Value: "2022-01-01", Precision: "date"},
		}, "document"},
		{"other", []DateCandidate{
			{ID: "addition", Role: "imported", SourceClass: "vault_addition", Value: "2024-08-02", Precision: "date"},
			{ID: "observation", Role: "vault_recorded", SourceClass: "vault_observation", Value: "2025-01-01", Precision: "date"},
		}, "addition"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got, err := SelectDate(tc.kind, tc.candidates, nil, Request{Timezone: "UTC"})
			if err != nil || got.CandidateID != tc.want {
				t.Fatalf("selection=%+v err=%v", got, err)
			}
		})
	}
}

func TestDatePolicyRequiresReviewForConflictingWinningEvidence(t *testing.T) {
	candidates := []DateCandidate{
		{ID: "a", Role: "created", SourceClass: "native", Value: "2024-05-02", Precision: "date"},
		{ID: "b", Role: "created", SourceClass: "native", Value: "2024-05-03", Precision: "date"},
		{ID: "fallback", Role: "imported", SourceClass: "vault_addition", Value: "2024-06-01", Precision: "date"},
	}
	_, err := SelectDate("other", candidates, nil, Request{Timezone: "UTC"})
	if !errors.Is(err, ErrAmbiguousDate) {
		t.Fatalf("conflicting native dates returned %v", err)
	}
}

func TestDatePolicyConvertsTimestampIntoReportTimezone(t *testing.T) {
	got, err := SelectDate("other", []DateCandidate{{
		ID: "created", Role: "created", SourceClass: "native",
		Value: "2024-03-10T23:30:00-07:00", Precision: "second",
	}}, nil, Request{Timezone: "UTC"})
	if err != nil || got.Date != "2024-03-11" {
		t.Fatalf("selection=%+v err=%v", got, err)
	}
}

func TestDatePolicyDoesNotUseUnusableNativeEvidence(t *testing.T) {
	got, err := SelectDate("other", []DateCandidate{
		{ID: "bad", Role: "created", SourceClass: "native", Value: "2024-02-30", Precision: "date", Rejection: "invalid_calendar_date"},
		{ID: "metadata", Role: "created", SourceClass: "source_metadata", Value: "2024-02-02", Precision: "date"},
	}, nil, Request{Timezone: "UTC"})
	if err != nil || got.CandidateID != "metadata" {
		t.Fatalf("selection=%+v err=%v", got, err)
	}
}

func TestDateReviewCanSelectEffectiveWithoutChangingOriginal(t *testing.T) {
	identity := Identity{NodeID: 1, VersionID: "v1", SHA256: strings.Repeat("a", 64)}
	evidence := strings.Repeat("b", 64)
	candidates := []DateCandidate{
		{ID: "created", Document: identity, Role: "created", SourceClass: "native", Value: "2024-05-02", Precision: "date", Locator: Locator{EvidenceSHA256: evidence}},
		{ID: "effective", Document: identity, Role: "effective", SourceClass: "content", Value: "2023-01-01", Precision: "date", Locator: Locator{EvidenceSHA256: evidence}},
	}
	choice := &DateChoice{Document: identity, CandidateID: "effective", EvidenceSHA256: evidence, Reason: "Reviewed effective date", Action: "select"}
	got, err := SelectDate("other", candidates, choice, Request{Timezone: "UTC"})
	if err != nil || got.CandidateID != "effective" || got.Date != "2023-01-01" || got.Mode != "override" {
		t.Fatalf("selection=%+v err=%v", got, err)
	}
	if candidates[1].Role != "effective" {
		t.Fatal("review changed original candidate")
	}
	choice.EvidenceSHA256 = strings.Repeat("c", 64)
	if _, err := SelectDate("other", candidates, choice, Request{Timezone: "UTC"}); err == nil {
		t.Fatal("accepted stale reviewed evidence")
	}
}

func TestDateReviewInterpretsAmbiguousNumericTokens(t *testing.T) {
	identity := Identity{NodeID: 2, VersionID: "v2", SHA256: strings.Repeat("a", 64)}
	evidence := strings.Repeat("b", 64)
	candidate := DateCandidate{
		ID: "ambiguous", Document: identity, Role: "unclassified", SourceClass: "content",
		Raw: "03/04/2020", Rejection: "ambiguous_numeric_date", Precision: "date",
		Locator: Locator{EvidenceSHA256: evidence, Quote: "03/04/2020"},
	}
	choice := &DateChoice{
		Document: identity, CandidateID: candidate.ID, EvidenceSHA256: evidence,
		Reason: "Read as month/day/year", Action: "interpret", ReviewedDate: "2020-03-04",
		ReviewedTimezone: "UTC", ReviewedRole: "document_date",
	}
	got, err := SelectDate("other", []DateCandidate{candidate}, choice, Request{Timezone: "UTC"})
	if err != nil || got.Date != "2020-03-04" || got.Mode != "override" {
		t.Fatalf("selection=%+v err=%v", got, err)
	}
	choice.ReviewedDate = "2020-04-05"
	if _, err := SelectDate("other", []DateCandidate{candidate}, choice, Request{Timezone: "UTC"}); err == nil {
		t.Fatal("accepted a date absent from the quoted numeric tokens")
	}
}

func TestDatePolicyUsesExplicitNumericOrdering(t *testing.T) {
	candidate := DateCandidate{
		ID: "numeric", Role: "created", SourceClass: "native", Raw: "03/04/2020",
		Precision: "date", Rejection: "ambiguous_numeric_date",
	}
	for _, tc := range []struct{ order, want string }{{"MDY", "2020-03-04"}, {"DMY", "2020-04-03"}} {
		got, err := SelectDate("other", []DateCandidate{candidate}, nil, Request{Timezone: "UTC", NumericDateOrder: tc.order})
		if err != nil || got.Date != tc.want {
			t.Fatalf("%s: selection=%+v err=%v", tc.order, got, err)
		}
	}
	if _, err := SelectDate("other", []DateCandidate{candidate}, nil, Request{Timezone: "UTC"}); !errors.Is(err, ErrUnusableDate) {
		t.Fatalf("ambiguous date without order returned %v", err)
	}
}

func TestDateReviewRejectsTrailingNumericGarbage(t *testing.T) {
	identity := Identity{NodeID: 1, VersionID: "v1", SHA256: strings.Repeat("a", 64)}
	evidence := strings.Repeat("b", 64)
	candidate := DateCandidate{ID: "numeric", Document: identity, Role: "created", SourceClass: "native",
		Raw: "03/04/2020 junk", Precision: "date", Rejection: "ambiguous_numeric_date",
		Locator: Locator{EvidenceSHA256: evidence}}
	choice := &DateChoice{Document: identity, CandidateID: candidate.ID, EvidenceSHA256: evidence,
		Reason: "Interpret", Action: "interpret", ReviewedDate: "2020-03-04", ReviewedTimezone: "UTC"}
	if _, err := SelectDate("other", []DateCandidate{candidate}, choice, Request{Timezone: "UTC"}); !errors.Is(err, ErrInvalidChoice) {
		t.Fatalf("accepted malformed source tokens: %v", err)
	}
}
