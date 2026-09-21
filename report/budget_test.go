package report

import (
	"context"
	"math"
	"sync"
	"testing"
)

func TestBudgetSharesLimitAcrossChildrenAndReleasesExactlyOnce(t *testing.T) {
	root := NewBudget(100)
	first := root.Child()
	second := root.Child()
	releaseFirst, err := first.Reserve(context.Background(), 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Reserve(context.Background(), 41); err == nil {
		t.Fatal("children exceeded shared limit")
	}
	releaseSecond, err := second.Reserve(context.Background(), 40)
	if err != nil {
		t.Fatal(err)
	}
	if got := root.Used(); got != 100 {
		t.Fatalf("used=%d, want 100", got)
	}
	releaseFirst()
	releaseFirst()
	if got := root.Used(); got != 40 {
		t.Fatalf("double release changed used bytes: %d", got)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	releaseSecond()
	if got := root.Used(); got != 0 {
		t.Fatalf("close failed to release child reservation: %d", got)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetRejectsCanceledAndOverflowReservations(t *testing.T) {
	root := NewBudget(math.MaxInt64)
	defer func() { _ = root.Close() }()
	child := root.Child()
	defer func() { _ = child.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := child.Reserve(ctx, 1); err == nil {
		t.Fatal("accepted canceled reservation")
	}
	if _, err := child.Reserve(context.Background(), -1); err == nil {
		t.Fatal("accepted negative reservation")
	}
	release, err := child.Reserve(context.Background(), math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Reserve(context.Background(), 1); err == nil {
		t.Fatal("overflowed reservation counter")
	}
	release()
	if got := root.Used(); got != 0 {
		t.Fatalf("failed reservations leaked %d bytes", got)
	}
}

func TestBudgetConcurrentReservationsStayWithinLimit(t *testing.T) {
	root := NewBudget(100)
	defer func() { _ = root.Close() }()
	var wg sync.WaitGroup
	releases := make(chan func(), 50)
	for range 50 {
		wg.Go(func() {
			if release, err := root.Reserve(context.Background(), 3); err == nil {
				releases <- release
			}
		})
	}
	wg.Wait()
	close(releases)
	if root.Used() > 100 {
		t.Fatalf("used=%d exceeds 100", root.Used())
	}
	for release := range releases {
		release()
	}
	if got := root.Used(); got != 0 {
		t.Fatalf("concurrent reservations leaked %d bytes", got)
	}
}
