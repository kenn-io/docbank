package reporting

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/report"
)

func TestCacheDatesDefaultBudgetContinues(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	members := make([]report.Member, 4)
	prefix := report.DatePage{Members: make([]report.DateReviewMember, 3)}
	var expected []report.DateCandidate
	for i := range members {
		members[i] = testMember()
		members[i].Identity.NodeID = int64(i + 1)
		members[i].Identity.VersionID = "20000000-0000-4000-8000-000000000001"
		count := 256
		if i == 3 {
			count = 2
		}
		for j := range count {
			candidate := report.DateCandidate{ID: fmt.Sprintf("%064x", i*256+j),
				Document: members[i].Identity, Role: "created", SourceClass: "native",
				Raw: "source date", Value: fmt.Sprintf("2024-05-%02d", j%2+1), Precision: "date",
				Locator: report.Locator{EvidenceSHA256: strings.Repeat("b", 64), Quote: "x"}}
			members[i].Candidates = append(members[i].Candidates, candidate)
		}
		if i < 3 {
			prefix.Members[i] = report.DateReviewMember{Document: members[i].Identity,
				Candidates: members[i].Candidates, CandidatesComplete: true}
		}
	}
	// Fill the earlier members to within 512 bytes of the default ceiling.
	// The next member's first complete evidence item needs more room than that.
	raw, err := json.Marshal(prefix)
	require.NoError(t, err)
	padding := (1 << 20) - 512 - len(raw)
	for i := range 3 {
		for j := range members[i].Candidates {
			n := padding / (768 - i*256 - j)
			members[i].Candidates[j].Locator.Quote += strings.Repeat("q", n)
			padding -= n
		}
	}
	for j := range members[3].Candidates {
		members[3].Candidates[j].Locator.Quote = strings.Repeat("z", 1024)
	}
	for _, member := range members {
		expected = append(expected, member.Candidates...)
	}
	cache, summary := cacheForDateMembers(t, now, members)
	page, err := cache.Dates(t.Context(), "owner", summary.ID, report.DatePageRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)
	var got []report.DateCandidate
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10, "pager failed to advance")
		require.NotEmpty(t, page.Members)
		encoded, err := json.Marshal(page)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), 1<<20)
		for _, member := range page.Members {
			got = append(got, member.Candidates...)
		}
		if page.NextCursor == "" {
			break
		}
		page, err = cache.Dates(t.Context(), "owner", summary.ID, report.DatePageRequest{Cursor: page.NextCursor})
		require.NoError(t, err)
	}
	require.Equal(t, expected, got)
}

func cacheForDateMembers(t *testing.T, now time.Time, members []report.Member) (*Cache, report.Summary) {
	t.Helper()
	budget := report.NewBudget(16 << 20)
	t.Cleanup(func() { _ = budget.Close() })
	cache := NewCache(func() time.Time { return now }, budget)
	t.Cleanup(cache.InvalidateAll)
	service := &Service{Source: frameSourceFunc(func(_ context.Context, request report.Request,
		selection report.CoverageSelection, _, _ report.Budget) (report.Frame, error) {
		return report.Frame{VaultID: "synthetic-vault", GenerationKind: "native", ObservedAt: now,
			Request: request, CoverageSelection: selection, Members: members}, nil
	})}
	summary, err := cache.Create(t.Context(), "owner", service, testRequest())
	require.NoError(t, err)
	require.Equal(t, "needs_review", summary.State)

	return cache, summary
}

func TestCacheDatesByteBudget(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	member := testMember()
	for j := range 256 {
		member.Candidates = append(member.Candidates, report.DateCandidate{ID: fmt.Sprintf("%064x", j),
			Document: member.Identity, Role: "created", SourceClass: "native", Raw: "2024-05-01",
			Value: fmt.Sprintf("2024-05-%02d", j%2+1), Precision: "date",
			Locator: report.Locator{Quote: strings.Repeat("\"\n\t", 340), EvidenceSHA256: strings.Repeat("b", 64)}})
	}
	empty := testMember()
	empty.Identity.NodeID = 2
	cache, summary := cacheForDateMembers(t, now, []report.Member{member, empty})
	for _, limit := range []int{0, 64 << 10, 256 << 10, 1 << 20} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			request := report.DatePageRequest{MaxBytes: limit}
			ceiling := limit
			if ceiling == 0 {
				ceiling = 1 << 20
			}
			var got []report.DateCandidate
			emptySeen := 0
			for pages := 0; ; pages++ {
				require.Less(t, pages, 100)
				page, err := cache.Dates(t.Context(), "owner", summary.ID, request)
				require.NoError(t, err)
				raw, err := json.Marshal(page)
				require.NoError(t, err)
				require.LessOrEqual(t, len(raw), ceiling)
				require.NotEmpty(t, page.Members)
				for _, item := range page.Members {
					if item.Document.NodeID == 2 {
						emptySeen++
						require.True(t, item.CandidatesComplete)
					}
					got = append(got, item.Candidates...)
					if item.Document.NodeID == 1 {
						require.Equal(t, len(got) == 256, item.CandidatesComplete)
					}
				}
				if page.NextCursor == "" {
					break
				}
				require.NotEqual(t, request.Cursor, page.NextCursor)
				request.Cursor = page.NextCursor
			}
			require.Equal(t, member.Candidates, got)
			require.Equal(t, 1, emptySeen)
		})
	}
	member.Candidates[0].Locator.Quote = strings.Repeat("x", 1<<20)
	large, largeSummary := cacheForDateMembers(t, now, []report.Member{member})
	_, err := large.Dates(t.Context(), "owner", largeSummary.ID, report.DatePageRequest{MaxBytes: 64 << 10})
	require.ErrorIs(t, err, report.ErrReportLimit)
}
