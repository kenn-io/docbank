package reporting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"go.kenn.io/docbank/report"
)

type frameSourceFunc func(context.Context, report.Request, report.CoverageSelection, report.Budget, report.Budget) (report.Frame, error)

func (f frameSourceFunc) MaterializeTermReportFrame(ctx context.Context, r report.Request, s report.CoverageSelection, b, textBudget report.Budget) (report.Frame, error) {
	return f(ctx, r, s, b, textBudget)
}

func testRequest() report.Request {
	return report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "strict",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}}}}
}

func testMember() report.Member {
	return report.Member{
		Identity: report.Identity{NodeID: 1, VersionID: "v1", SHA256: strings.Repeat("a", 64)},
		Kind:     "other", RawMatches: []bool{true},
		Coverage: report.MemberCoverage{SearchState: "complete", DateEvidenceState: "complete", FamilyState: "complete"},
	}
}

func TestFinalizeUsesOnlyFrozenFrameAndKeepsReviewOutcome(t *testing.T) {
	member := testMember()
	member.Candidates = []report.DateCandidate{
		{ID: "a", Document: member.Identity, Role: "created", SourceClass: "native", Value: "2024-02-03",
			Precision: "date", Locator: report.Locator{EvidenceSHA256: strings.Repeat("b", 64)}},
		{ID: "b", Document: member.Identity, Role: "created", SourceClass: "native", Value: "2024-03-04",
			Precision: "date", Locator: report.Locator{EvidenceSHA256: strings.Repeat("b", 64)}},
	}
	reads := 0
	svc := &Service{Source: frameSourceFunc(func(_ context.Context, request report.Request, _ report.CoverageSelection, _, _ report.Budget) (report.Frame, error) {
		reads++
		return report.Frame{Request: request, Members: []report.Member{member}, ObservedAt: time.Now()}, nil
	}), Budget: report.NewBudget(1 << 20)}
	defer func() { _ = svc.Budget.Close() }()
	frame, err := svc.Prepare(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(frame.CoverageSelection.ConfigurationSHA256) != 64 {
		t.Fatalf("missing frozen coverage configuration digest: %+v", frame.CoverageSelection)
	}
	_, err = svc.Finalize(context.Background(), frame, nil)
	if !errors.Is(err, ErrReviewRequired) {
		t.Fatalf("expected review, got %v", err)
	}
	choice := report.DateChoice{Document: member.Identity, CandidateID: "b", EvidenceSHA256: strings.Repeat("b", 64),
		Reason: "Reviewed source", Action: "select"}
	got, err := svc.Finalize(context.Background(), frame, []report.DateChoice{choice})
	if err != nil || got.Counts[0].Hits != 1 || got.Frame.Members[0].Selection.Date != "2024-03-04" {
		t.Fatalf("result=%+v err=%v", got.Counts, err)
	}
	if reads != 1 || frame.Members[0].Selection.Date != "" {
		t.Fatalf("revisited or changed frozen frame: reads=%d selection=%+v", reads, frame.Members[0].Selection)
	}
}

func TestPrepareExtractsNativeTextThenDropsFullBytes(t *testing.T) {
	member := testMember()
	member.RawMatches[0] = false
	text := []byte("Document dated 2024-05-06")
	digest := sha256.Sum256(text)
	sha := hex.EncodeToString(digest[:])
	binding := report.TextBinding{Kind: "native", Document: member.Identity, Size: int64(len(text)),
		Native: &report.NativeText{Text: text, TextSHA256: sha, SearchableVersionID: "v1", Status: "ok"}}
	svc := &Service{Source: frameSourceFunc(func(_ context.Context, request report.Request, _ report.CoverageSelection, _, _ report.Budget) (report.Frame, error) {
		return report.Frame{Request: request, Members: []report.Member{member}, Texts: []report.TextBinding{binding}}, nil
	}), Budget: report.NewBudget(1 << 20)}
	defer func() { _ = svc.Budget.Close() }()
	frame, err := svc.Prepare(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(frame.Members[0].Candidates) != 1 || frame.Members[0].Candidates[0].Value != "2024-05-06" ||
		frame.Texts[0].Native == nil || frame.Texts[0].Native.Text != nil {
		t.Fatalf("incorrect native extraction or text retention: %+v", frame)
	}
	got, err := svc.Finalize(context.Background(), frame, nil)
	if err != nil || got.Frame.Members[0].Selection.Date != "2024-05-06" {
		t.Fatalf("finalize selection=%+v err=%v", got.Frame.Members[0].Selection, err)
	}
}

func TestStrictCoverageUsesDateEligiblePopulation(t *testing.T) {
	inside := testMember()
	inside.Candidates = []report.DateCandidate{{ID: "inside", Document: inside.Identity,
		Role: "created", SourceClass: "native", Value: "2024-05-06", Precision: "date"}}
	outside := testMember()
	outside.Identity.NodeID = 2
	outside.Candidates = []report.DateCandidate{{ID: "outside", Document: outside.Identity,
		Role: "created", SourceClass: "native", Value: "2023-05-06", Precision: "date"}}
	outside.Coverage.SearchState = "missing"
	frame := report.Frame{Request: testRequest(), Members: []report.Member{inside, outside}}
	svc := &Service{Budget: report.NewBudget(1 << 20)}
	defer func() { _ = svc.Budget.Close() }()
	got, err := svc.Finalize(context.Background(), frame, nil)
	if err != nil || got.Counts[0].Hits != 1 || got.Frame.RowCoverage[0].Scoped != 1 {
		t.Fatalf("result=%+v coverage=%+v err=%v", got.Counts, got.Frame.RowCoverage, err)
	}
	frame.Members[1].Candidates[0].Value = "2024-05-06"
	_, err = svc.Finalize(context.Background(), frame, nil)
	if !errors.Is(err, ErrIncompleteCoverage) {
		t.Fatalf("missing in-range text returned %v", err)
	}
}
