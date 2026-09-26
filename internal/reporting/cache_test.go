package reporting

import (
	"context"
	"errors"
	"io"
	"strconv"
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
		selection report.CoverageSelection, _, _ report.Budget) (report.Frame, error) {
		*reads++
		return report.Frame{VaultID: "synthetic-vault", GenerationKind: "native", ObservedAt: now,
			Request: request, CoverageSelection: selection, Members: []report.Member{member}}, nil
	})}
}

func TestCacheReleasesCalculationBudget(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	defer cache.InvalidateAll()
	reads := 0
	service := cacheFixture(now, &reads)
	summary, err := cache.Create(context.Background(), "owner", service, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	retained := summary.CSVBytes + summary.BundleBytes
	if used := budget.Used(); used != retained {
		t.Fatalf("completed export retains %d bytes; artifacts need %d", used, retained)
	}
	revision, err := cache.Revise(context.Background(), "owner", summary.ID, service, nil)
	if err != nil {
		t.Fatal(err)
	}
	retained += revision.CSVBytes + revision.BundleBytes
	if used := budget.Used(); used != retained {
		t.Fatalf("completed revision retains %d bytes; artifacts need %d", used, retained)
	}
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

func TestReportFrozenWithdrawalAfterCompletionWithholdsCountsAndDownload(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	defer cache.InvalidateAll()
	reads, withdrawn := 0, false
	svc := cacheFixture(now, &reads)
	svc.Visibility = func(_ context.Context, _ report.Frame) error {
		if withdrawn {
			return ErrVisibilityChanged
		}
		return nil
	}
	summary, err := cache.Create(t.Context(), "owner", svc, testRequest())
	if err != nil || summary.Counts[0].Hits != 1 {
		t.Fatalf("initial report: %+v %v", summary, err)
	}
	withdrawn = true
	if got, err := cache.Summary("owner", summary.ID); !errors.Is(err, ErrVisibilityChanged) || len(got.Counts) != 0 {
		t.Fatalf("withdrawn counts disclosed: %+v %v", got, err)
	}
	if reader, _, _, err := cache.Acquire(t.Context(), "owner", summary.ID, "bundle"); !errors.Is(err, ErrVisibilityChanged) || reader != nil {
		t.Fatalf("withdrawn artifact disclosed: %v", err)
	}
}

func TestReportFrozenHistoryReceiptIncludesUnselectedFamilyDocument(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	defer cache.InvalidateAll()
	reads := 0
	svc := cacheFixture(now, &reads)
	source := svc.Source
	parent := testMember().Identity
	child := report.Identity{NodeID: 2, VersionID: "v2", SHA256: strings.Repeat("c", 64)}
	svc.Source = frameSourceFunc(func(ctx context.Context, request report.Request,
		selection report.CoverageSelection, scope, textScope report.Budget) (report.Frame, error) {
		frame, err := source.MaterializeTermReportFrame(ctx, request, selection, scope, textScope)
		if err != nil {
			return report.Frame{}, err
		}
		frame.Members[0].FamilyID = parent.VersionID
		frame.Relations = []report.Relation{{Parent: parent, Child: child,
			EvidenceID: "synthetic-family", EvidenceSHA256: strings.Repeat("d", 64)}}
		return frame, nil
	})
	request := testRequest()
	request.Version, request.AllDocuments = 2, false
	request.SelectedDocuments = []report.Identity{parent}
	summary, err := cache.Create(t.Context(), "owner", svc, request)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := cache.MemberIdentities(t.Context(), "owner", summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 2 || identities[0] != parent || identities[1] != child {
		t.Fatalf("durable visibility dependencies: %+v", identities)
	}
}

func TestReportFrozenDependencyLimitRejectsBeforePublication(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	defer cache.InvalidateAll()
	member := testMember()
	member.Candidates = []report.DateCandidate{
		{ID: "first", Document: member.Identity, Role: "created", SourceClass: "native",
			Value: "2024-05-06", Precision: "date", Locator: report.Locator{EvidenceSHA256: strings.Repeat("b", 64)}},
		{ID: "second", Document: member.Identity, Role: "created", SourceClass: "native",
			Value: "2024-06-07", Precision: "date", Locator: report.Locator{EvidenceSHA256: strings.Repeat("c", 64)}},
	}
	relations := make([]report.Relation, 50000)
	for i := range relations {
		relations[i] = report.Relation{Parent: member.Identity,
			Child: report.Identity{NodeID: int64(i + 2), VersionID: "child-" + strconv.Itoa(i), SHA256: strings.Repeat("d", 64)}}
	}
	svc := &Service{Source: frameSourceFunc(func(_ context.Context, request report.Request,
		selection report.CoverageSelection, _, _ report.Budget) (report.Frame, error) {
		return report.Frame{VaultID: "synthetic-vault", GenerationKind: "native", ObservedAt: now,
			Request: request, CoverageSelection: selection, Members: []report.Member{member}, Relations: relations}, nil
	}), Visibility: func(_ context.Context, frame report.Frame) error {
		_, err := report.VisibilityIdentities(frame)
		return err
	}}
	_, err := cache.Create(t.Context(), "owner", svc, testRequest())
	if !errors.Is(err, report.ErrReportLimit) {
		t.Fatalf("dependency limit: %v", err)
	}
	if len(cache.entries) != 0 || budget.Used() != 0 {
		t.Fatalf("over-limit observation published or retained resources: entries=%d bytes=%d", len(cache.entries), budget.Used())
	}
}

func TestReportFrozenWithdrawalAfterPreviewWithholdsReview(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	defer cache.InvalidateAll()
	member := testMember()
	for i, value := range []string{"2024-05-06", "2024-06-07"} {
		member.Candidates = append(member.Candidates, report.DateCandidate{ID: string(rune('a' + i)), Document: member.Identity,
			Role: "created", SourceClass: "native", Value: value, Precision: "date",
			Locator: report.Locator{EvidenceSHA256: strings.Repeat("b", 64)}})
	}
	withdrawn := false
	svc := &Service{Source: frameSourceFunc(func(_ context.Context, request report.Request,
		selection report.CoverageSelection, _, _ report.Budget) (report.Frame, error) {
		return report.Frame{VaultID: "synthetic-vault", GenerationKind: "native", ObservedAt: now,
			Request: request, CoverageSelection: selection, Members: []report.Member{member}}, nil
	}), Visibility: func(_ context.Context, _ report.Frame) error {
		if withdrawn {
			return ErrVisibilityChanged
		}
		return nil
	}}
	summary, err := cache.Create(t.Context(), "owner", svc, testRequest())
	if err != nil || summary.State != "needs_review" {
		t.Fatalf("preview: %+v %v", summary, err)
	}
	withdrawn = true
	if page, err := cache.Dates(t.Context(), "owner", summary.ID, report.DatePageRequest{}); !errors.Is(err, ErrVisibilityChanged) || len(page.Members) != 0 {
		t.Fatalf("withdrawn preview disclosed: %+v %v", page, err)
	}
	if _, err := cache.Revise(t.Context(), "owner", summary.ID, svc, nil); !errors.Is(err, ErrVisibilityChanged) {
		t.Fatalf("withdrawn revision: %v", err)
	}
}

func TestReportFrozenWithdrawalDuringPublicationRejectsReport(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	defer cache.InvalidateAll()
	reads := 0
	svc := cacheFixture(now, &reads)
	checking, release := make(chan struct{}), make(chan struct{})
	withdrawn := false
	svc.Visibility = func(_ context.Context, _ report.Frame) error {
		close(checking)
		<-release
		if withdrawn {
			return ErrVisibilityChanged
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := cache.Create(t.Context(), "owner", svc, testRequest()); done <- err }()
	<-checking
	withdrawn = true
	close(release)
	if err := <-done; !errors.Is(err, ErrVisibilityChanged) {
		t.Fatalf("publication after withdrawal: %v", err)
	}
}

func TestReportFrozenPublicationHonorsRequestCancellation(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(16 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	defer cache.InvalidateAll()
	reads := 0
	svc := cacheFixture(now, &reads)
	checking, release := make(chan struct{}), make(chan struct{})
	svc.Visibility = func(ctx context.Context, _ report.Frame) error {
		close(checking)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := cache.Create(ctx, "owner", svc, testRequest()); done <- err }()
	<-checking
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled publication: %v", err)
		}
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("publication visibility check ignored request cancellation")
	}
}

func TestReportFrozenBlockedVisibilityDoesNotStallAnotherOwner(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(32 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	defer cache.InvalidateAll()
	reads := 0
	firstService := cacheFixture(now, &reads)
	block := false
	entered, release := make(chan struct{}), make(chan struct{})
	firstService.Visibility = func(_ context.Context, _ report.Frame) error {
		if block {
			close(entered)
			<-release
		}
		return nil
	}
	first, err := cache.Create(t.Context(), "owner-a", firstService, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	secondService := cacheFixture(now, &reads)
	second, err := cache.Create(t.Context(), "owner-b", secondService, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	block = true
	firstDone := make(chan error, 1)
	go func() { _, err := cache.Summary("owner-a", first.ID); firstDone <- err }()
	<-entered
	secondDone := make(chan error, 1)
	go func() { _, err := cache.Summary("owner-b", second.ID); secondDone <- err }()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("other owner's read: %v", err)
		}
	case <-time.After(time.Second):
		close(release)
		<-firstDone
		<-secondDone
		t.Fatal("blocked source visibility held the global cache mutex")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestReportFrozenRevokedDuringVisibilityCheckWithholdsSummary(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	budget := report.NewBudget(32 << 20)
	defer func() { _ = budget.Close() }()
	cache := NewCache(func() time.Time { return now }, budget)
	defer cache.InvalidateAll()
	reads := 0
	svc := cacheFixture(now, &reads)
	block := false
	entered, release := make(chan struct{}), make(chan struct{})
	svc.Visibility = func(_ context.Context, _ report.Frame) error {
		if block {
			close(entered)
			<-release
		}
		return nil
	}
	summary, err := cache.Create(t.Context(), "owner", svc, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	block = true
	readDone := make(chan error, 1)
	go func() { _, err := cache.Summary("owner", summary.ID); readDone <- err }()
	<-entered
	revokeDone := make(chan struct{})
	go func() { cache.Revoke("owner"); close(revokeDone) }()
	select {
	case <-revokeDone:
	case <-time.After(time.Second):
		close(release)
		<-readDone
		t.Fatal("revoke waited for source visibility I/O")
	}
	close(release)
	if err := <-readDone; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("revoked summary: %v", err)
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
		selection report.CoverageSelection, _, _ report.Budget) (report.Frame, error) {
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
		selection report.CoverageSelection, _, _ report.Budget) (report.Frame, error) {
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
