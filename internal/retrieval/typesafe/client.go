package typesafe

import (
	"context"
	"errors"
	"reflect"

	wire "go.kenn.io/docbank/document/typesafe"
	"go.kenn.io/docbank/internal/retrieval"
)

type Reranker interface {
	Rerank(ctx context.Context, request wire.RerankRequest) (wire.Result, error)
}

type Client struct{ reranker Reranker }

type Execution struct {
	Scores  []retrieval.RerankScore
	Receipt wire.Receipt
}

var _ Reranker = (*wire.Client)(nil)
var _ retrieval.RerankingProvider = (*Client)(nil)

func New(reranker Reranker) (*Client, error) {
	if reranker == nil || isNil(reranker) {
		return nil, errors.New("typesafe retrieval: reranker is required")
	}
	return &Client{reranker: reranker}, nil
}

func (client *Client) Rerank(ctx context.Context, request retrieval.RerankingRequest) ([]retrieval.RerankScore, error) {
	execution, err := client.RerankWithReceipt(ctx, request)
	return execution.Scores, err
}

func (client *Client) RerankWithReceipt(ctx context.Context, request retrieval.RerankingRequest) (Execution, error) {
	if client == nil || client.reranker == nil || ctx == nil {
		return Execution{}, errors.New("typesafe retrieval: client and context are required")
	}
	seen := make(map[retrieval.DocumentIdentity]struct{}, len(request.Candidates))
	texts := make([]string, len(request.Candidates))
	for index, candidate := range request.Candidates {
		identity := candidate.Document
		if identity.VaultID == "" || identity.NodeID <= 0 || identity.ContentVersionID == "" {
			return Execution{}, errors.New("typesafe retrieval: candidate identity is invalid")
		}
		if _, exists := seen[identity]; exists {
			return Execution{}, errors.New("typesafe retrieval: candidate identity is duplicated")
		}
		seen[identity] = struct{}{}
		texts[index] = candidate.Excerpt
	}
	result, err := client.reranker.Rerank(ctx, wire.RerankRequest{Query: request.Query, Candidates: texts})
	if err != nil {
		return Execution{}, err
	}
	if len(result.Scores) != len(request.Candidates) {
		return Execution{}, &wire.ProviderError{Kind: wire.ErrPermanentResponse}
	}
	scores := make([]retrieval.RerankScore, len(result.Scores))
	for index, score := range result.Scores {
		scores[index] = retrieval.RerankScore{Document: request.Candidates[index].Document, Score: score}
	}
	return Execution{Scores: scores, Receipt: result.Receipt}, nil
}

func isNil(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
