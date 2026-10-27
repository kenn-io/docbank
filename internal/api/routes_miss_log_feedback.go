package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/processing"
)

// SearchFeedbackRequest requires the separate capability issued by a search.
// Operator labels use the management route and need only the record ID.
type SearchFeedbackRequest struct {
	ID         string           `json:"id" minLength:"48" maxLength:"48"`
	Capability string           `json:"capability" maxLength:"64"`
	Feedback   misslog.Feedback `json:"feedback"`
}

// SearchCompletionRequest is the final merged CLI page, without excerpts.
type SearchCompletionRequest struct {
	Search     DocumentSearchRequest  `json:"search"`
	ActualMode string                 `json:"actual_mode" enum:"lexical,semantic,hybrid"`
	Coverage   DocumentSearchCoverage `json:"coverage"`
	Hits       []misslog.Hit          `json:"hits" maxItems:"10"`
	Truncated  bool                   `json:"truncated,omitempty"`
}

type SearchFeedbackResult struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
}

func missLogFeedbackError(err error) error {
	switch {
	case errors.Is(err, misslog.ErrUnavailable):
		return NewError(http.StatusNotFound, "not_found", "search receipt or feedback capability unavailable")
	case errors.Is(err, misslog.ErrInvalid):
		return NewError(http.StatusUnprocessableEntity, "validation", "invalid search feedback")
	default:
		return missLogFailure()
	}
}
func registerMissLogFeedbackRoutes(h huma.API, d Deps) {
	type completionOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         EmptySearchCompletion
	}
	huma.Register(h, huma.Operation{OperationID: "completeSearch", Method: http.MethodPost, Path: "/api/v1/search/complete", MaxBodyBytes: 1 << 20, Summary: "Record the final merged CLI search (operator only)"}, func(ctx context.Context, in *struct{ Body SearchCompletionRequest }) (*completionOutput, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		if !d.MissLog.Enabled() {
			return &completionOutput{CacheControl: "no-store", Body: EmptySearchCompletion{}}, nil
		}
		input := in.Body
		request := input.Search
		if request.Fence.VaultUID != d.Store.VaultID() {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "search scope belongs to another vault")
		}
		fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: request.Fence.VaultUID, ContentVersionIDs: request.Fence.ContentVersionIDs})
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "search scope is invalid")
		}
		if err := validateDocumentSearch(ctx, d.Processing, DocumentSearchValidationRequest{Query: request.Query, Mode: request.Mode, Limit: request.Limit, Profile: request.Profile, BindingID: request.BindingID}); err != nil {
			return nil, err
		}
		for _, id := range request.Fence.ContentVersionIDs {
			if _, err := d.Store.ContentVersionByID(ctx, id); err != nil {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", "search scope contains an unavailable version")
			}
		}
		for _, hit := range input.Hits {
			node, err := d.Store.NodeByID(ctx, hit.NodeID)
			if err != nil || node.TrashedAt != nil {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", "search hit is unavailable")
			}
			if node.Kind == "dir" {
				if hit.ContentVersionID != "" || hit.Score != nil || len(hit.EvidenceIDs) > 0 {
					return nil, NewError(http.StatusUnprocessableEntity, "validation", "folder hit has document evidence")
				}
				continue
			}
			if !slices.Contains(request.Fence.ContentVersionIDs, hit.ContentVersionID) {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", "search hit is outside its scope")
			}
			version, err := d.Store.ContentVersionByID(ctx, hit.ContentVersionID)
			if err != nil || version.NodeID != hit.NodeID {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", "search hit identity is invalid")
			}
		}
		scope := receiptScope(request)
		scope["folder_search"] = map[string]any{packageLimitParameter: request.Limit}
		coverage := receiptScope(input.Coverage)
		coverage["truncated"] = input.Truncated
		ticket := d.MissLog.Observe(ctx, misslog.Receipt{Source: "client_finalized", Query: request.Query, Profile: request.Profile, RequestedMode: request.Mode, ActualMode: input.ActualMode, Scope: scope, Coverage: coverage, FenceFingerprint: fingerprint, Hits: input.Hits})
		return &completionOutput{CacheControl: "no-store", Body: EmptySearchCompletion{MissLogID: ticket.ID, MissLogCapability: ticket.Capability}}, nil
	})

	type output struct{ Body SearchFeedbackResult }
	huma.Register(h, huma.Operation{OperationID: "recordSearchFeedback", Method: http.MethodPost, Path: "/api/v1/search/feedback", MaxBodyBytes: 64 << 10, Summary: "Record feedback using a search capability"}, func(ctx context.Context, in *struct{ Body SearchFeedbackRequest }) (*output, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		if err := d.MissLog.Feedback(ctx, in.Body.ID, in.Body.Capability, in.Body.Feedback, false); err != nil {
			return nil, missLogFeedbackError(err)
		}
		return &output{Body: SearchFeedbackResult{ID: in.Body.ID, Success: true}}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "labelMissLogRecord", Method: http.MethodPost, Path: "/api/v1/miss-log/{id}/feedback", MaxBodyBytes: 64 << 10, Summary: "Label one private search receipt (operator only)"}, func(ctx context.Context, in *struct {
		ID   string `path:"id" maxLength:"128"`
		Body misslog.Feedback
	}) (*output, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		if err := d.MissLog.Feedback(ctx, in.ID, "", in.Body, true); err != nil {
			return nil, missLogFeedbackError(err)
		}
		return &output{Body: SearchFeedbackResult{ID: in.ID, Success: true}}, nil
	})
}
