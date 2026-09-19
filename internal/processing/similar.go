package processing

import (
	"context"
	"slices"
	"time"

	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

type SimilarRequest struct {
	Selector  Selector
	BindingID string
	Limit     int
	Fence     SourceFence
}

func (service *Service) FindSimilar(ctx context.Context, request SimilarRequest) (retrieval.SimilarReport, error) {
	profile, ids, err := service.profileFence(request.Selector.Profile, request.Fence)
	if err != nil {
		return retrieval.SimilarReport{}, err
	}
	if !slices.Contains(ids, request.Selector.ContentVersionID) {
		return retrieval.SimilarReport{}, store.ErrInvalidProcessingSourceFence
	}
	node, version, _, err := service.resolve(ctx, request.Selector)
	if err != nil {
		return retrieval.SimilarReport{}, err
	}
	binding, err := selectEmbeddingBinding(profile.portable, request.BindingID)
	if err != nil {
		return retrieval.SimilarReport{}, err
	}
	if request.Limit == 0 {
		request.Limit = DefaultSearchLimit
	}
	if request.Limit < 1 || request.Limit > MaxSearchLimit {
		return retrieval.SimilarReport{}, store.ErrInvalidProcessingSourceFence
	}
	searcher, err := retrieval.NewSearcher(retrieval.SearcherConfig{Backend: service.catalog, Owner: "embedded-document-similarity", LeaseDuration: 5 * time.Minute})
	if err != nil {
		return retrieval.SimilarReport{}, err
	}
	return searcher.Similar(ctx, retrieval.SimilarQuery{Source: retrieval.DocumentIdentity{VaultID: service.catalog.VaultID(), NodeID: node.ID, ContentVersionID: version.ID},
		Limit: request.Limit, Scope: store.SearchOptions{ContentVersionIDs: ids}, ProcessingProfileFingerprint: profile.record.Fingerprint, BindingID: binding.Name})
}
