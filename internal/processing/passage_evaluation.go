//go:build rerankeval

package processing

import (
	"context"
	"errors"
	"go.kenn.io/docbank/internal/retrieval"
)

func (service *Service) PreparePassageEvaluation(ctx context.Context, request SearchRequest) (*retrieval.PassageEvaluation, error) {
	if request.AllLive != nil || request.MediaSources != nil || request.Rerank {
		return nil, errors.New("offline passage evaluation requires an explicit fence and ordinary unreranked search")
	}
	var evaluation *retrieval.PassageEvaluation
	_, err := service.searchWith(ctx, request, func(searcher *retrieval.Searcher, ctx context.Context, query retrieval.Query) (retrieval.Report, error) {
		var err error
		evaluation, err = searcher.PreparePassageEvaluation(ctx, query)
		if err != nil {
			return retrieval.Report{}, err
		}
		return evaluation.Report, nil
	})
	return evaluation, err
}
