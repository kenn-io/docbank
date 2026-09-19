// Package typesafetest provides a synthetic TypeSafe client for application tests.
// It never performs egress, and its scores are not Jev observations.
package typesafetest

import (
	"context"
	"errors"
	"slices"
	"sync"

	"go.kenn.io/docbank/document/typesafe"
)

type ScoreFunc func(query, candidate string) (float64, error)

type Fake struct {
	profile  typesafe.Profile
	score    ScoreFunc
	mu       sync.Mutex
	requests []typesafe.RerankRequest
}

func New(profile typesafe.Profile, score ScoreFunc) *Fake {
	return &Fake{profile: profile, score: score}
}

func (fake *Fake) Rerank(_ context.Context, request typesafe.RerankRequest) (typesafe.Result, error) {
	if fake == nil || fake.score == nil {
		return typesafe.Result{}, errors.New("typesafetest: score function is required")
	}
	if err := typesafe.CheckRequest(fake.profile, request); err != nil {
		return typesafe.Result{}, err
	}
	recorded := typesafe.RerankRequest{Query: request.Query, Candidates: slices.Clone(request.Candidates)}
	fake.mu.Lock()
	fake.requests = append(fake.requests, recorded)
	fake.mu.Unlock()
	scores := make([]float64, len(request.Candidates))
	for index, candidate := range request.Candidates {
		score, err := fake.score(request.Query, candidate)
		if err != nil {
			return typesafe.Result{}, err
		}
		scores[index] = score
	}
	return typesafe.Result{Scores: scores, Receipt: typesafe.Receipt{
		Model: typesafe.ModelJev113, RequestShape: fake.profile.RequestShape,
		CandidateCount: len(scores),
	}}, nil
}

func (fake *Fake) Requests() []typesafe.RerankRequest {
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
