package processing

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

// SearchPhotos selects asset ranks for the existing owner-bound snapshot path.
func (service *Service) SearchPhotos(ctx context.Context, request store.PhotoBrowseRequest) (store.PhotoRanking, error) {
	result := store.PhotoRanking{RequestedMode: request.Query.Mode, ActualMode: "lexical"}
	fallback := func(reason string) (store.PhotoRanking, error) { result.FallbackReason = reason; return result, nil }
	if request.Query.Mode == "lexical" {
		return result, nil
	}
	semanticText, scope, supported := photoSemanticScope(request.Query)
	if !supported {
		return fallback("unsupported_query")
	}
	reason := "provider_unavailable"
	names := slices.Sorted(maps.Keys(service.profiles))
	for _, name := range names {
		profile := service.profiles[name]
		for _, binding := range profile.portable.Embeddings {
			provider := profile.embedders[binding.Name]
			if binding.InputKind != document.EmbeddingInputImage || provider == nil || !provider.Descriptor().SupportsTextQuery {
				continue
			}
			selected, err := service.searchPhotoBinding(ctx, request, semanticText, scope, profile, binding, provider)
			if err != nil || selected.FallbackReason == "" || selected.FallbackReason == "unsupported_query" {
				return selected, err
			}
			if reason == "provider_unavailable" {
				reason = selected.FallbackReason
			}
		}
	}
	return fallback(reason)
}

func (service *Service) searchPhotoBinding(ctx context.Context, request store.PhotoBrowseRequest, semanticText string, scope query.Query, profile configuredProfile, binding document.EmbeddingBindingV1, provider document.EmbeddingProvider) (store.PhotoRanking, error) {
	result := store.PhotoRanking{RequestedMode: request.Query.Mode, ActualMode: "lexical"}
	fallback := func(reason string) (store.PhotoRanking, error) { result.FallbackReason = reason; return result, nil }
	_, fence, err := service.catalog.BeginProviderEgress(ctx, store.ProviderOperationAuthorizationRequest{Principal: service.principal, Scope: service.scope, ProfileFingerprint: profile.record.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint, InputClasses: []string{"query_text"}, RetainedArtifactClasses: []string{}})
	if err != nil {
		if isEmbeddingConsentFailure(err) {
			return fallback("consent_required")
		}
		return result, err
	}
	descriptor := provider.Descriptor()
	authorization := document.EmbeddingAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 1, MaxInputBytes: binding.MaxInputBytes, MaxResponseBytes: binding.MaxResponseBytes}
	searcher, err := retrieval.NewSearcher(retrieval.SearcherConfig{Backend: service.catalog, Encoders: service.embeddings, Owner: "photo-search", LeaseDuration: time.Minute})
	if err != nil {
		fence.Close()
		return result, err
	}
	semanticRequest := request
	semanticRequest.Query = scope
	report, err := searcher.Search(ctx, retrieval.Query{Text: semanticText, Mode: retrieval.Mode(request.Query.Mode), Limit: 1000, LexicalLimit: 1000, VectorLimit: 1000, ProcessingProfileFingerprint: profile.record.Fingerprint, BindingID: binding.Name, Authorization: authorization, Scope: store.SearchOptions{PhotoScope: &semanticRequest, PhotoLexicalQuery: &request.Query}})
	fence.Close()
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrVectorIndexSourceStale) {
			return fallback("index_unavailable")
		}
		if _, ok := errors.AsType[*query.ExpressionError](err); ok {
			return fallback("unsupported_query")
		}
		slog.WarnContext(ctx, "photo search fell back to metadata", "profile", profile.record.Fingerprint, "binding", binding.Name, "error", err)
		return fallback("provider_unavailable")
	}
	result.ActualMode, result.Truncated = string(report.ActualMode), report.Truncated
	result.CoverageIncomplete = report.Coverage.State == retrieval.CoverageIncomplete
	nodeIDs := make([]int64, len(report.Results))
	for i, hit := range report.Results {
		nodeIDs[i] = hit.Document.NodeID
	}
	assetIDs, err := service.catalog.PhotoAssetIDsForNodes(ctx, nodeIDs)
	if err != nil {
		return result, err
	}
	for _, hit := range report.Results {
		assetID, found := assetIDs[hit.Document.NodeID]
		if !found {
			return result, store.ErrNotFound
		}
		result.Members = append(result.Members, store.SnapshotMember{AssetID: assetID, NodeID: hit.Document.NodeID, ContentVersionID: hit.Document.ContentVersionID})
	}
	return result, nil
}

func photoSemanticScope(value query.Query) (string, query.Query, bool) {
	expression, err := query.ParseExpression(value.Text, value.Syntax)
	if err != nil {
		return "", value, false
	}
	var text, filters []string
	var visit func(*query.Expression) bool
	visit = func(node *query.Expression) bool {
		switch {
		case node.Kind == query.ExpressionAll:
			return true
		case node.Kind == query.ExpressionAnd:
			for _, child := range node.Children {
				if !visit(child) {
					return false
				}
			}
			return true
		case node.Kind == query.ExpressionField || node.Kind == query.ExpressionTerm && node.Value == "pick" && !node.Prefix:
			filters = append(filters, value.Text[node.Start:node.End])
			return true
		case node.Kind == query.ExpressionTerm || node.Kind == query.ExpressionPhrase:
			text = append(text, node.Value)
			return true
		default:
			return false
		}
	}
	if !visit(expression) || len(text) == 0 {
		return "", value, false
	}
	value.Mode, value.Text, value.Syntax = "lexical", strings.Join(filters, " AND "), "advanced"
	value.Sort = query.Sort{Field: "name", Direction: "asc"}
	return strings.Join(text, " "), value, true
}
