package typesafetest

import (
	"context"
	"errors"
	"math"
	"net/netip"
	"sync"
	"testing"

	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/typesafe"
)

func TestFakeRecordsSyntheticRequests(t *testing.T) {
	fake := New(typesafe.Profile{SecretBinding: "test", EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.typesafe.ai", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}, func(_, candidate string) (float64, error) {
		return float64(len(candidate)) / 10, nil
	})
	result, err := fake.Rerank(context.Background(), typesafe.RerankRequest{Query: "q", Candidates: []string{"candidate"}})
	if err != nil || result.Scores[0] != 0.9 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	requests := fake.Requests()
	if len(requests) != 1 || requests[0].Query != "q" || requests[0].Candidates[0] != "candidate" {
		t.Fatalf("requests=%+v", requests)
	}
}

func TestFakeEnforcesClientBounds(t *testing.T) {
	profile := typesafe.Profile{SecretBinding: "test", MaxQueryBytes: 1, EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.typesafe.ai", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}
	fake := New(profile, func(_, _ string) (float64, error) { t.Fatal("score function called"); return 0, nil })
	if _, err := fake.Rerank(context.Background(), typesafe.RerankRequest{Query: "too long", Candidates: []string{"x"}}); !errors.Is(err, typesafe.ErrCapacityResponse) {
		t.Fatalf("got %v", err)
	}
	if len(fake.Requests()) != 0 {
		t.Fatal("invalid request was recorded")
	}
}

func TestFakeHonorsCanceledContextBeforeRecording(t *testing.T) {
	profile := typesafe.Profile{SecretBinding: "test", EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.typesafe.ai", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}
	fake := New(profile, func(_, _ string) (float64, error) {
		t.Fatal("score function called")
		return 0, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fake.Rerank(ctx, typesafe.RerankRequest{Query: "q", Candidates: []string{"x"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if len(fake.Requests()) != 0 {
		t.Fatal("canceled request was recorded")
	}
}

func TestFakeStopsScoringAfterCancellation(t *testing.T) {
	var calls int
	ctx, cancel := context.WithCancel(context.Background())
	profile := typesafe.Profile{SecretBinding: "test", EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.typesafe.ai", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}
	fake := New(profile, func(_, _ string) (float64, error) {
		calls++
		cancel()
		return 0.5, nil
	})

	_, err := fake.Rerank(ctx, typesafe.RerankRequest{Query: "q", Candidates: []string{"first", "second"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if calls != 1 {
		t.Fatalf("score calls = %d, want 1", calls)
	}
}

func TestFakeMatchesClientReceiptAndRejectsInvalidScores(t *testing.T) {
	profile := typesafe.Profile{SecretBinding: "test", EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.typesafe.ai", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}
	fake := New(profile, func(_, _ string) (float64, error) { return 1.1, nil })
	result, err := fake.Rerank(context.Background(), typesafe.RerankRequest{Query: "q", Candidates: []string{"x"}})
	if !errors.Is(err, typesafe.ErrPermanentResponse) || len(result.Scores) != 0 {
		t.Fatalf("invalid score: result=%+v err=%v", result, err)
	}
	for _, score := range []float64{math.NaN(), math.Inf(1), -0.1, 1.1} {
		fake = New(profile, func(_, _ string) (float64, error) { return score, nil })
		if _, err := fake.Rerank(context.Background(), typesafe.RerankRequest{Query: "q", Candidates: []string{"x"}}); !errors.Is(err, typesafe.ErrPermanentResponse) {
			t.Errorf("score %v: got %v", score, err)
		}
	}

	fake = New(profile, func(_, _ string) (float64, error) { return 0.5, nil })
	result, err = fake.Rerank(context.Background(), typesafe.RerankRequest{Query: "q", Candidates: []string{"x"}})
	if err != nil || result.Receipt.PolicyFingerprint == "" || result.Receipt.RequestShape != typesafe.RequestShapePerCandidate {
		t.Fatalf("receipt=%+v err=%v", result.Receipt, err)
	}
}

func TestFakeIsSafeForConcurrentUse(t *testing.T) {
	fake := New(typesafe.Profile{SecretBinding: "test", EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.typesafe.ai", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}, func(_, _ string) (float64, error) { return 0.5, nil })
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, err := fake.Rerank(context.Background(), typesafe.RerankRequest{Query: "q", Candidates: []string{"x"}}); err != nil {
				t.Errorf("rerank: %v", err)
			}
			_ = fake.Requests()
		})
	}
	group.Wait()
	if len(fake.Requests()) != 8 {
		t.Fatalf("requests = %d", len(fake.Requests()))
	}
}
