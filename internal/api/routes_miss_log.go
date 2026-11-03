package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// MissLogPage contains sensitive receipts and is available only to the operator.
type MissLogPage struct {
	Items  []misslog.Receipt `json:"items"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

func requireMissLogOwner(ctx context.Context) error {
	if auth, _ := ctx.Value(authenticationContextKey{}).(string); auth != "master" {
		return NewError(http.StatusForbidden, "forbidden", "miss log management requires the master API credential")
	}
	return nil
}

func missLogFailure() error {
	return NewError(http.StatusInternalServerError, "miss_log_unavailable", "miss log operation failed")
}

func registerMissLogRoutes(api huma.API, d Deps, _ *OperationGate) {
	registerMissLogFeedbackRoutes(api, d)

	type recordOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         misslog.Receipt
	}
	huma.Register(api, huma.Operation{OperationID: "getMissLogRecord", Method: http.MethodGet, Path: "/api/v1/miss-log/{id}", Summary: "Read one private search receipt (operator only)"}, func(ctx context.Context, in *struct {
		ID string `path:"id" maxLength:"128"`
	}) (*recordOutput, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		row, err := d.Store.MissLogRecordByID(ctx, in.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, NewError(http.StatusNotFound, "not_found", "miss log record not found")
		}
		if err != nil {
			return nil, missLogFailure()
		}
		var receipt misslog.Receipt
		if json.Unmarshal([]byte(row.Receipt), &receipt) != nil {
			return nil, missLogFailure()
		}
		return &recordOutput{CacheControl: "no-store", Body: receipt}, nil
	})
	type completionInput struct{ Body EmptySearchCompletionRequest }
	type completionOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         EmptySearchCompletion
	}
	huma.Register(api, huma.Operation{OperationID: "completeEmptySearch", Method: http.MethodPost, Path: "/api/v1/search/complete-empty", MaxBodyBytes: 1 << 20, Summary: "Record the final empty automatic CLI search (operator only)"}, func(ctx context.Context, in *completionInput) (*completionOutput, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		request := in.Body.Search
		if request.Fence.VaultUID != d.Store.VaultID() {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "search scope belongs to another vault")
		}
		if _, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: request.Fence.VaultUID, ContentVersionIDs: request.Fence.ContentVersionIDs}); err != nil {
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
		request.SkipMissLog = false
		report := DocumentSearchReport{RequestedMode: request.Mode, ActualMode: in.Body.ActualMode, Coverage: in.Body.Coverage}
		fingerprint, _ := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: request.Fence.VaultUID, ContentVersionIDs: request.Fence.ContentVersionIDs})
		scope := receiptScope(request)
		scope["folder_search"] = map[string]any{packageLimitParameter: request.Limit}
		ticket := d.MissLog.Observe(ctx, misslog.Receipt{Source: "client_finalized", Query: request.Query, Profile: request.Profile, RequestedMode: report.RequestedMode, ActualMode: report.ActualMode, Scope: scope, Coverage: receiptScope(report.Coverage), FenceFingerprint: fingerprint})
		return &completionOutput{CacheControl: "no-store", Body: EmptySearchCompletion{MissLogID: ticket.ID, MissLogCapability: ticket.Capability}}, nil
	})
	type pageOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         MissLogPage
	}
	huma.Register(api, huma.Operation{OperationID: "listMissLog", Method: http.MethodGet, Path: "/api/v1/miss-log", Summary: "List private search receipts (operator only)"}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" default:"20" minimum:"1" maximum:"100"`
		Offset int `query:"offset" default:"0" minimum:"0"`
	}) (*pageOutput, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		rows, err := d.Store.ListMissLogRecords(ctx, in.Limit, in.Offset)
		if err != nil {
			return nil, missLogFailure()
		}
		page := MissLogPage{Items: []misslog.Receipt{}, Limit: in.Limit, Offset: in.Offset}
		for _, row := range rows {
			var receipt misslog.Receipt
			if err := json.Unmarshal([]byte(row.Receipt), &receipt); err != nil {
				return nil, missLogFailure()
			}
			page.Items = append(page.Items, receipt)
		}
		return &pageOutput{CacheControl: "no-store", Body: page}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "deleteMissLogRecord", Method: http.MethodDelete, Path: "/api/v1/miss-log/{id}", Summary: "Delete one private search receipt (operator only)"}, func(ctx context.Context, in *struct {
		ID string `path:"id" maxLength:"128"`
	}) (*struct{}, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		if err := d.MissLog.Delete(ctx, in.ID, time.Time{}); err != nil {
			if apiError, ok := errors.AsType[*Error](err); ok {
				return nil, apiError
			}
			return nil, missLogFailure()
		}
		return &struct{}{}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "deleteMissLog", Method: http.MethodDelete, Path: "/api/v1/miss-log", Summary: "Purge private search misses or delete by date (operator only)"}, func(ctx context.Context, in *struct {
		Before string `query:"before" maxLength:"64"`
	}) (*struct{}, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		var cutoff time.Time
		if in.Before != "" {
			var err error
			cutoff, err = time.Parse(time.RFC3339, in.Before)
			if err != nil {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", "before must be an RFC 3339 timestamp")
			}
			if cutoff.IsZero() {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", "before must be a nonzero RFC 3339 timestamp")
			}
		}
		if err := d.MissLog.Delete(ctx, "", cutoff); err != nil {
			if apiError, ok := errors.AsType[*Error](err); ok {
				return nil, apiError
			}
			return nil, missLogFailure()
		}
		return &struct{}{}, nil
	})
}

func receiptScope(input any) map[string]any {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var scope map[string]any
	if json.Unmarshal(encoded, &scope) != nil {
		return nil
	}
	delete(scope, "query")
	delete(scope, "skip_miss_log")
	return scope
}

func recordDocumentSearch(ctx context.Context, log *misslog.Service, input DocumentSearchRequest, report DocumentSearchReport) misslog.Ticket {
	if input.SkipMissLog {
		return misslog.Ticket{}
	}
	fingerprint, _ := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: input.Fence.VaultUID, ContentVersionIDs: input.Fence.ContentVersionIDs})
	return log.Observe(ctx, misslog.Receipt{Query: input.Query, Profile: input.Profile, RequestedMode: report.RequestedMode, ActualMode: report.ActualMode, Scope: receiptScope(input), Coverage: receiptScope(report.Coverage), FenceFingerprint: fingerprint, Hits: MissLogDocumentHits(report.Results)})
}

// MissLogDocumentHits strips excerpts and bounds the retained search evidence.
func MissLogDocumentHits(results []DocumentSearchResult) []misslog.Hit {
	hits := make([]misslog.Hit, 0, min(10, len(results)))
	for _, r := range results[:min(10, len(results))] {
		hit := misslog.Hit{Rank: r.Rank, NodeID: r.NodeID, ContentVersionID: r.ContentVersionID, Score: new(r.Score), Path: r.Path}
		for _, e := range r.Evidence[:min(64, len(r.Evidence))] {
			encoded, err := json.Marshal(e, json.Deterministic(true))
			if err != nil || len(encoded) > 1024 {
				continue
			}
			hit.EvidenceIDs = append(hit.EvidenceIDs, string(encoded))
		}
		hits = append(hits, hit)
	}
	return hits
}

func missLogLiveHits(results []SearchHit) []misslog.Hit {
	hits := make([]misslog.Hit, 0, min(10, len(results)))
	for i, r := range results[:min(10, len(results))] {
		hits = append(hits, misslog.Hit{Rank: i + 1, NodeID: r.Node.ID, ContentVersionID: r.Node.CurrentVersionID, Path: r.Path})
	}
	return hits
}

// EmptySearchCompletionRequest describes the final CLI page after adding folders.
// It accepts no excerpts, arbitrary metadata, or caller-selected record identity.
type EmptySearchCompletionRequest struct {
	Search     DocumentSearchRequest  `json:"search"`
	ActualMode string                 `json:"actual_mode" enum:"lexical,semantic,hybrid"`
	Coverage   DocumentSearchCoverage `json:"coverage"`
}
type EmptySearchCompletion struct {
	MissLogCapability string `json:"miss_log_capability,omitzero"`
	MissLogID         string `json:"miss_log_id,omitzero"`
}
