package reporting

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/report"
)

func releaseFixture(t *testing.T, budget report.Budget, review bool) (*Cache, *Service) {
	t.Helper()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	cache := NewCache(func() time.Time { return now }, budget)
	t.Cleanup(func() { require.NoError(t, budget.Close()) })
	t.Cleanup(cache.InvalidateAll)
	reads := 0
	service := cacheFixture(now, &reads)
	source := service.Source
	service.Source = frameSourceFunc(func(ctx context.Context, request report.Request,
		selection report.CoverageSelection, frameBudget, textBudget report.Budget,
	) (report.Frame, error) {
		frame, err := source.MaterializeTermReportFrame(ctx, request, selection, frameBudget, textBudget)
		if err != nil {
			return frame, err
		}
		if review {
			candidate := frame.Members[0].Candidates[0]
			candidate.ID, candidate.Value = "other-date", "2024-06-07"
			frame.Members[0].Candidates = append(frame.Members[0].Candidates, candidate)
		}
		_, err = frameBudget.Reserve(ctx, 1024)
		return frame, err
	})
	return cache, service
}

func TestCacheReleaseCapacityAndOwnership(t *testing.T) {
	for _, state := range []string{"complete", "needs_review"} {
		t.Run(state, func(t *testing.T) {
			cache, service := releaseFixture(t, report.NewBudget(16<<20), state == "needs_review")
			var first report.Summary
			for index := range 8 {
				summary, err := cache.Create(t.Context(), "owner", service, testRequest())
				require.NoError(t, err)
				require.Equal(t, state, summary.State)
				if index == 0 {
					first = summary
				}
			}
			_, err := cache.Create(t.Context(), "owner", service, testRequest())
			require.ErrorIs(t, err, ErrCapacity)
			require.ErrorIs(t, cache.Release("other", first.ID), ErrUnavailable)
			require.ErrorIs(t, cache.Release("", first.ID), ErrUnavailable)
			require.ErrorIs(t, cache.Release("owner", strings.Repeat("a", 48)), ErrUnavailable)
			require.NoError(t, cache.Release("owner", first.ID))
			require.ErrorIs(t, cache.Release("owner", first.ID), ErrUnavailable)
			_, err = cache.Summary("owner", first.ID)
			require.ErrorIs(t, err, ErrUnavailable)
			last, err := cache.Create(t.Context(), "owner", service, testRequest())
			require.NoError(t, err)
			cache.now = func() time.Time { return first.ExpiresAt }
			require.ErrorIs(t, cache.Release("owner", last.ID), ErrUnavailable)
			require.Zero(t, cache.budget.Used())
		})
	}
}

func TestCacheReleasePreservesChild(t *testing.T) {
	budget := report.NewBudget(16 << 20)
	cache, service := releaseFixture(t, budget, false)
	baseline := budget.Used()
	parent, err := cache.Create(t.Context(), "owner", service, testRequest())
	require.NoError(t, err)
	child, err := cache.Revise(t.Context(), "owner", parent.ID, service, nil)
	require.NoError(t, err)
	page, err := cache.Dates(t.Context(), "owner", child.ID, report.DatePageRequest{})
	require.NoError(t, err)
	packet := readReleasePacket(t, cache, child.ID)
	require.NoError(t, cache.Release("owner", parent.ID))
	retained, err := cache.Summary("owner", child.ID)
	require.NoError(t, err)
	require.Equal(t, child, retained)
	require.Equal(t, parent.ExpiresAt, retained.ExpiresAt)
	retainedPage, err := cache.Dates(t.Context(), "owner", child.ID, report.DatePageRequest{})
	require.NoError(t, err)
	require.Equal(t, page, retainedPage)
	require.Equal(t, packet, readReleasePacket(t, cache, child.ID))
	verificationBudget := report.NewBudget(16 << 20)
	defer func() { require.NoError(t, verificationBudget.Close()) }()
	verified, err := report.VerifyBundle(t.Context(), verificationBudget,
		bytes.NewReader(packet), int64(len(packet)))
	require.NoError(t, err)
	require.True(t, verified.InternallyConsistent)
	require.Greater(t, budget.Used(), baseline)
	require.NoError(t, cache.Release("owner", child.ID))
	require.Equal(t, baseline, budget.Used())
}

func readReleasePacket(t *testing.T, cache *Cache, id string) []byte {
	t.Helper()
	reader, size, _, err := cache.Acquire(t.Context(), "owner", id, "bundle")
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Len(t, raw, int(size))
	return raw
}

func TestCacheReleaseRetainsReaders(t *testing.T) {
	cache, service := releaseFixture(t, report.NewBudget(16<<20), false)
	summary, err := cache.Create(t.Context(), "owner", service, testRequest())
	require.NoError(t, err)
	for _, format := range []string{"csv", "bundle"} {
		reader, size, _, err := cache.Acquire(t.Context(), "owner", summary.ID, format)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		finished := make(chan error, 1)
		go func() { finished <- cache.Release("owner", summary.ID) }()
		require.ErrorIs(t, <-finished, ErrRetained)
		require.ErrorIs(t, cache.Release("other", summary.ID), ErrUnavailable)
		raw, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Len(t, raw, int(size))
		require.NoError(t, reader.Close())
	}
	require.NoError(t, cache.Release("owner", summary.ID))
	_, _, _, err = cache.Acquire(t.Context(), "owner", summary.ID, "bundle")
	require.ErrorIs(t, err, ErrUnavailable)
}

// These barriers delay real operations without replacing their lifetime accounting.
type releaseBarrier struct {
	armed   atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func newReleaseBarrier(t *testing.T) (*releaseBarrier, func()) {
	t.Helper()
	barrier := &releaseBarrier{entered: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(barrier.resume) })
	t.Cleanup(resume)
	return barrier, resume
}

func (b *releaseBarrier) wait() {
	if b.armed.CompareAndSwap(true, false) {
		close(b.entered)
		<-b.resume
	}
}

type releaseBudget struct {
	report.Budget
	barrier *releaseBarrier
}

func (b releaseBudget) Child() report.Budget {
	return releaseBudget{Budget: b.Budget.Child(), barrier: b.barrier}
}

func (b releaseBudget) Reserve(ctx context.Context, size int64) (func(), error) {
	b.barrier.wait()
	return b.Budget.Reserve(ctx, size)
}

func TestCacheReleaseDuringRevision(t *testing.T) {
	barrier, resume := newReleaseBarrier(t)
	budget := releaseBudget{Budget: report.NewBudget(16 << 20), barrier: barrier}
	cache, service := releaseFixture(t, budget, false)
	parent, err := cache.Create(t.Context(), "owner", service, testRequest())
	require.NoError(t, err)
	barrier.armed.Store(true)
	finished := make(chan error, 1)
	go func() {
		_, err := cache.Revise(t.Context(), "owner", parent.ID, service, nil)
		finished <- err
	}()
	<-barrier.entered
	require.NoError(t, cache.Release("owner", parent.ID))
	require.Positive(t, budget.Used(), "in-flight revision still owns the frame")
	resume()
	require.ErrorIs(t, <-finished, ErrUnavailable)
	require.Zero(t, budget.Used())
	for range 8 {
		_, err := cache.Create(t.Context(), "owner", service, testRequest())
		require.NoError(t, err, "revision must return build and owner capacity")
	}
}

type releaseContext struct {
	context.Context
	barrier *releaseBarrier
}

func (c releaseContext) Err() error {
	c.barrier.wait()
	return c.Context.Err()
}

func TestCacheReleaseDuringDates(t *testing.T) {
	budget := report.NewBudget(16 << 20)
	cache, service := releaseFixture(t, budget, false)
	summary, err := cache.Create(t.Context(), "owner", service, testRequest())
	require.NoError(t, err)
	before, err := cache.Dates(t.Context(), "owner", summary.ID, report.DatePageRequest{})
	require.NoError(t, err)
	barrier, resume := newReleaseBarrier(t)
	barrier.armed.Store(true)
	finished := make(chan struct{})
	var page report.DatePage
	var readErr error
	go func() {
		defer close(finished)
		page, readErr = cache.Dates(releaseContext{Context: t.Context(), barrier: barrier},
			"owner", summary.ID, report.DatePageRequest{})
	}()
	<-barrier.entered
	require.NoError(t, cache.Release("owner", summary.ID))
	require.Positive(t, budget.Used(), "date reader still owns the frame")
	resume()
	<-finished
	require.NoError(t, readErr)
	require.Equal(t, before, page)
	require.Zero(t, budget.Used())
	_, err = cache.Dates(t.Context(), "owner", summary.ID, report.DatePageRequest{})
	require.ErrorIs(t, err, ErrUnavailable)
}
