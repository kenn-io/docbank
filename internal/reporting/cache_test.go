package reporting

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.kenn.io/docbank/report"
)

func cacheFixture(now time.Time, reads *int) *Service {
	member := testMember()
	member.Candidates = []report.DateCandidate{{
		ID: "date", Document: member.Identity, Role: "imported", SourceClass: "vault_addition",
		Value: "2024-05-06", Precision: "date",
		Locator: report.Locator{EvidenceSHA256: strings.Repeat("b", 64)},
	}}
	return &Service{Source: frameSourceFunc(func(_ context.Context, request report.Request,
		selection report.CoverageSelection, _ report.Budget) (report.Frame, error) {
		*reads++
		return report.Frame{VaultID: "synthetic-vault", GenerationKind: "native", ObservedAt: now,
			Request: request, CoverageSelection: selection, Members: []report.Member{member}}, nil
	})}
}

func TestCacheOwnerBoundImmutableRevisionAndExpiry(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	clock := now
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return clock }, budget)
	reads := 0
	svc := cacheFixture(now, &reads)
	summary, err := cache.Create(context.Background(), "owner-a", svc, testRequest())
	if err != nil || summary.State != "complete" || len(summary.Counts) != 1 || summary.Counts[0].Hits != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if _, err := cache.Summary("owner-b", summary.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cross-owner lookup: %v", err)
	}
	page, err := cache.Dates(context.Background(), "owner-a", summary.ID, report.DatePageRequest{Limit: 10})
	if err != nil || len(page.Members) != 1 || len(page.Members[0].Candidates) != 1 {
		t.Fatalf("dates=%+v err=%v", page, err)
	}
	stream, size, digest, err := cache.Acquire(context.Background(), "owner-a", summary.ID, "bundle")
	if err != nil {
		t.Fatal(err)
	}
	packet, err := io.ReadAll(stream)
	if err != nil || int64(len(packet)) != size || digest != summary.BundleSHA256 {
		t.Fatalf("packet=%d size=%d digest=%s err=%v", len(packet), size, digest, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	choice := report.DateChoice{Document: page.Members[0].Document, CandidateID: "date",
		EvidenceSHA256: strings.Repeat("b", 64), Reason: "Reviewed source", Action: "select"}
	revision, err := cache.Revise(context.Background(), "owner-a", summary.ID, svc, []report.DateChoice{choice})
	if err != nil || revision.ParentID != summary.ID || revision.ID == summary.ID ||
		!revision.ExpiresAt.Equal(summary.ExpiresAt) || reads != 1 {
		t.Fatalf("revision=%+v reads=%d err=%v", revision, reads, err)
	}
	revisedDates, err := cache.Dates(context.Background(), "owner-a", revision.ID, report.DatePageRequest{Limit: 10})
	if err != nil || revisedDates.Members[0].Selection.Mode != "override" ||
		revisedDates.Members[0].Choice == nil || revisedDates.Members[0].Choice.Reason != "Reviewed source" {
		t.Fatalf("revision date evidence=%+v err=%v", revisedDates, err)
	}
	parent, err := cache.Summary("owner-a", summary.ID)
	if err != nil || parent.ParentID != "" {
		t.Fatalf("parent changed: %+v err=%v", parent, err)
	}
	clock = now.Add(31 * time.Minute)
	if _, err := cache.Summary("owner-a", summary.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expired parent: %v", err)
	}
	if _, err := cache.Summary("owner-a", revision.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expired revision: %v", err)
	}
	cache.InvalidateAll()
	if used := budget.Used(); used != 0 {
		t.Fatalf("retained %d bytes after invalidation", used)
	}
}

func TestCacheNeedsReviewKeepsDatesWithoutDownloads(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	member := testMember()
	for i, value := range []string{"2024-05-06", "2024-06-07"} {
		member.Candidates = append(member.Candidates, report.DateCandidate{ID: string(rune('a' + i)),
			Document: member.Identity, Role: "created", SourceClass: "native", Value: value,
			Precision: "date", Locator: report.Locator{EvidenceSHA256: strings.Repeat("b", 64)}})
	}
	svc := &Service{Source: frameSourceFunc(func(_ context.Context, request report.Request,
		selection report.CoverageSelection, _ report.Budget) (report.Frame, error) {
		return report.Frame{VaultID: "synthetic-vault", GenerationKind: "native", ObservedAt: now,
			Request: request, CoverageSelection: selection, Members: []report.Member{member}}, nil
	})}
	summary, err := cache.Create(context.Background(), "owner", svc, testRequest())
	if err != nil || summary.State != "needs_review" || len(summary.Counts) != 0 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if _, _, _, err := cache.Acquire(context.Background(), "owner", summary.ID, "csv"); !errors.Is(err, ErrReviewRequired) {
		t.Fatalf("review-only download: %v", err)
	}
	page, err := cache.Dates(context.Background(), "owner", summary.ID, report.DatePageRequest{Limit: 10})
	if err != nil || len(page.Members[0].Candidates) != 2 {
		t.Fatalf("review dates=%+v err=%v", page, err)
	}
	cache.InvalidateAll()
}

func TestCacheRevisionKeepsEarlierDateChoices(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	members := make([]report.Member, 2)
	for index := range members {
		members[index] = testMember()
		members[index].Identity.NodeID = int64(index + 1)
		for candidateIndex, value := range []string{"2024-05-06", "2024-06-07"} {
			members[index].Candidates = append(members[index].Candidates, report.DateCandidate{
				ID: string(rune('a' + candidateIndex)), Document: members[index].Identity,
				Role: "imported", SourceClass: "vault_addition", Value: value, Precision: "date",
				Locator: report.Locator{EvidenceSHA256: strings.Repeat("b", 64)},
			})
		}
	}
	svc := &Service{Source: frameSourceFunc(func(_ context.Context, request report.Request,
		selection report.CoverageSelection, _ report.Budget) (report.Frame, error) {
		return report.Frame{VaultID: "synthetic-vault", GenerationKind: "native", ObservedAt: now,
			Request: request, CoverageSelection: selection, Members: members}, nil
	})}
	first, err := cache.Create(context.Background(), "owner", svc, testRequest())
	if err != nil || first.State != "needs_review" || first.UnresolvedDates != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	choice := func(index int) report.DateChoice {
		return report.DateChoice{Document: members[index].Identity, CandidateID: "a",
			EvidenceSHA256: strings.Repeat("b", 64), Reason: "Reviewed synthetic source", Action: "select"}
	}
	second, err := cache.Revise(context.Background(), "owner", first.ID, svc, []report.DateChoice{choice(0)})
	if err != nil || second.State != "needs_review" || second.UnresolvedDates != 1 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	third, err := cache.Revise(context.Background(), "owner", second.ID, svc, []report.DateChoice{choice(1)})
	if err != nil || third.State != "complete" || len(third.Counts) != 1 || third.Counts[0].Hits != 2 {
		t.Fatalf("third=%+v err=%v", third, err)
	}
	page, err := cache.Dates(context.Background(), "owner", third.ID, report.DatePageRequest{Limit: 10})
	if err != nil || len(page.Members) != 2 || page.Members[0].Choice == nil || page.Members[1].Choice == nil {
		t.Fatalf("review page=%+v err=%v", page, err)
	}
	cache.InvalidateAll()
}
