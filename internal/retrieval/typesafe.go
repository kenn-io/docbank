package retrieval

import (
	"context"
	"errors"
	"reflect"

	wire "go.kenn.io/docbank/document/typesafe"
)

type TypesafeReranker interface {
	Rerank(ctx context.Context, request wire.RerankRequest) (wire.Result, error)
}

type TypesafeClient struct{ reranker TypesafeReranker }

type TypesafeExecution struct {
	Scores  []RerankScore
	Receipt wire.Receipt
}

var _ TypesafeReranker = (*wire.Client)(nil)
var _ RerankingProvider = (*TypesafeClient)(nil)

func NewTypesafeClient(reranker TypesafeReranker) (*TypesafeClient, error) {
	if reranker == nil || isNil(reranker) {
		return nil, errors.New("typesafe retrieval: reranker is required")
	}
	return &TypesafeClient{reranker: reranker}, nil
}

func (client *TypesafeClient) Rerank(ctx context.Context, request RerankingRequest) ([]RerankScore, error) {
	execution, err := client.RerankWithReceipt(ctx, request)
	return execution.Scores, err
}

func (client *TypesafeClient) RerankWithReceipt(ctx context.Context, request RerankingRequest) (TypesafeExecution, error) {
	if client == nil || client.reranker == nil || ctx == nil {
		return TypesafeExecution{}, errors.New("typesafe retrieval: client and context are required")
	}
	seen := make(map[DocumentIdentity]struct{}, len(request.Candidates))
	texts := make([]string, len(request.Candidates))
	for index, candidate := range request.Candidates {
		identity := candidate.Document
		if identity.VaultID == "" || identity.NodeID <= 0 || identity.ContentVersionID == "" {
			return TypesafeExecution{}, errors.New("typesafe retrieval: candidate identity is invalid")
		}
		if _, exists := seen[identity]; exists {
			return TypesafeExecution{}, errors.New("typesafe retrieval: candidate identity is duplicated")
		}
		seen[identity] = struct{}{}
		texts[index] = candidate.Excerpt
	}
	result, err := client.reranker.Rerank(ctx, wire.RerankRequest{Query: request.Query, Candidates: texts})
	if err != nil {
		return TypesafeExecution{}, err
	}
	if len(result.Scores) != len(request.Candidates) {
		return TypesafeExecution{}, &wire.ProviderError{Kind: wire.ErrPermanentResponse}
	}
	scores := make([]RerankScore, len(result.Scores))
	for index, score := range result.Scores {
		scores[index] = RerankScore{Document: request.Candidates[index].Document, Score: score}
	}
	return TypesafeExecution{Scores: scores, Receipt: result.Receipt}, nil
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
