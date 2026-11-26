package retrieval

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"

	"go.kenn.io/docbank/document/typesafe"
)

type ScoreFunc func(query, candidate string) (float64, error)

type typesafeFake struct {
	profile  typesafe.Profile
	score    ScoreFunc
	mu       sync.Mutex
	requests []typesafe.RerankRequest
}

func newTypesafeFake(profile typesafe.Profile, score ScoreFunc) *typesafeFake {
	return &typesafeFake{profile: profile, score: score}
}

func (fake *typesafeFake) Rerank(ctx context.Context, request typesafe.RerankRequest) (typesafe.Result, error) {
	if fake == nil || fake.score == nil {
		return typesafe.Result{}, errors.New("typesafetest: score function is required")
	}
	if err := typesafe.CheckRequest(fake.profile, request); err != nil {
		return typesafe.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return typesafe.Result{}, err
	}
	fingerprint, err := typesafe.PolicyFingerprint(fake.profile)
	if err != nil {
		return typesafe.Result{}, err
	}
	recorded := typesafe.RerankRequest{Query: request.Query, Candidates: slices.Clone(request.Candidates)}
	fake.mu.Lock()
	fake.requests = append(fake.requests, recorded)
	fake.mu.Unlock()
	scores := make([]float64, len(request.Candidates))
	for index, candidate := range request.Candidates {
		if err := ctx.Err(); err != nil {
			return typesafe.Result{}, err
		}
		score, err := fake.score(request.Query, candidate)
		if err := ctx.Err(); err != nil {
			return typesafe.Result{}, err
		}
		if err != nil {
			return typesafe.Result{}, err
		}
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return typesafe.Result{}, &typesafe.ProviderError{Kind: typesafe.ErrPermanentResponse}
		}
		scores[index] = score
	}
	return typesafe.Result{Scores: scores, Receipt: typesafe.Receipt{
		PolicyFingerprint: fingerprint, Model: typesafe.ModelJev113,
		RequestShape:   effectiveRequestShape(fake.profile),
		CandidateCount: len(scores),
	}}, nil
}

func effectiveRequestShape(profile typesafe.Profile) typesafe.RequestShape {
	if profile.RequestShape == "" {
		return typesafe.RequestShapePerCandidate
	}
	return profile.RequestShape
}

func (fake *typesafeFake) Requests() []typesafe.RerankRequest {
	if fake == nil {
		return nil
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	requests := make([]typesafe.RerankRequest, len(fake.requests))
	for index, request := range fake.requests {
		requests[index] = typesafe.RerankRequest{Query: request.Query, Candidates: slices.Clone(request.Candidates)}
	}
	return requests
}
