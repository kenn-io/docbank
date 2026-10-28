package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"

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

func registerMissLogRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{OperationID: "recordMiss", Method: http.MethodPost, Path: "/api/v1/miss-log", Summary: "Record a submitted empty search", DefaultStatus: http.StatusNoContent, MaxBodyBytes: 128 << 10}, func(ctx context.Context, in *struct{ Body misslog.Request }) (*struct{}, error) {
		d.MissLog.Record(ctx, in.Body)
		return nil, nil
	})
	type pageOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         MissLogPage
	}
	huma.Register(api, huma.Operation{OperationID: "listMissLog", Method: http.MethodGet, Path: "/api/v1/miss-log", Summary: "List private search misses (operator only)"}, func(ctx context.Context, in *struct {
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
	huma.Register(api, huma.Operation{OperationID: "deleteMissLogRecord", Method: http.MethodDelete, Path: "/api/v1/miss-log/{id}", Summary: "Delete one private search miss (operator only)"}, func(ctx context.Context, in *struct {
		ID string `path:"id" maxLength:"128"`
	}) (*struct{}, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		if err := g.mutate(ctx, func() error {
			if err := d.Store.DeleteMissLogRecord(ctx, in.ID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return NewError(http.StatusNotFound, "not_found", "miss log record not found")
				}
				return missLogFailure()
			}
			return nil
		}); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "deleteMissLog", Method: http.MethodDelete, Path: "/api/v1/miss-log", Summary: "Purge private search misses (operator only)"}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		if err := g.mutate(ctx, func() error {
			if err := d.Store.PurgeMissLogRecords(ctx); err != nil {
				return missLogFailure()
			}
			return nil
		}); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})
}

// DocumentSearchMiss keeps the compact outcome without source ID lists.
func DocumentSearchMiss(query, profile string, request DocumentSearchRequest, report DocumentSearchReport) misslog.Request {
	receipt := misslog.Request{Query: query, Profile: profile, BindingID: request.BindingID,
		RequestedMode: report.RequestedMode, ActualMode: report.ActualMode,
		DocumentCount: new(report.Coverage.ScopedDocuments), Truncated: report.Truncated, Degradations: report.Degradations,
		Coverage: map[string]any{"binding_required": report.Coverage.BindingRequired, "scoped_documents": report.Coverage.ScopedDocuments,
			"complete_documents": report.Coverage.CompleteDocuments, "state": report.Coverage.State}}
	if request.Scope != nil {
		receipt.Filters = misslog.ReceiptScope(request.Scope.Filters)
	} else if len(request.Fence.ContentVersionIDs) > 0 {
		fingerprint, _ := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: request.Fence.VaultUID, ContentVersionIDs: request.Fence.ContentVersionIDs})
		receipt.FenceFingerprint = fingerprint
		receipt.DocumentCount = new(len(request.Fence.ContentVersionIDs))
	}
	if report.Reranking != nil {
		receipt.Reranking = map[string]any{"outcome": report.Reranking.Outcome, "cause": report.Reranking.Cause, "candidate_count": report.Reranking.CandidateCount}
	}
	return receipt
}
