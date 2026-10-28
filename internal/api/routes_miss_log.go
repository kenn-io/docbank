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

const missLogCacheControl = "no-store"

// MissLogPage contains sensitive receipts and is available only to the operator.
type MissLogPage struct {
	Items  []misslog.Receipt `json:"items"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

func missLogFailure() error {
	return NewError(http.StatusInternalServerError, "miss_log_unavailable", "miss log operation failed")
}

func registerMissLogRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{OperationID: "recordMiss", Method: http.MethodPost, Path: "/api/v1/miss-log", Summary: "Record a submitted empty search", DefaultStatus: http.StatusNoContent}, func(ctx context.Context, in *struct{ Body misslog.Request }) (*struct{}, error) {
		d.MissLog.Record(ctx, in.Body)
		return nil, nil
	})
	type recordOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         misslog.Receipt
	}
	huma.Register(api, huma.Operation{OperationID: "getMissLogRecord", Method: http.MethodGet, Path: "/api/v1/miss-log/{id}", Summary: "Read one private search miss (operator only)"}, func(ctx context.Context, in *struct {
		ID string `path:"id" maxLength:"128"`
	}) (*recordOutput, error) {
		if browserSessionRequest(ctx) {
			return nil, NewError(http.StatusForbidden, "forbidden", "miss log management requires the master API credential")
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
		return &recordOutput{CacheControl: missLogCacheControl, Body: receipt}, nil
	})
	type pageOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         MissLogPage
	}
	huma.Register(api, huma.Operation{OperationID: "listMissLog", Method: http.MethodGet, Path: "/api/v1/miss-log", Summary: "List private search misses (operator only)"}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" default:"20" minimum:"1" maximum:"100"`
		Offset int `query:"offset" default:"0" minimum:"0"`
	}) (*pageOutput, error) {
		if browserSessionRequest(ctx) {
			return nil, NewError(http.StatusForbidden, "forbidden", "miss log management requires the master API credential")
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
		return &pageOutput{CacheControl: missLogCacheControl, Body: page}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "deleteMissLogRecord", Method: http.MethodDelete, Path: "/api/v1/miss-log/{id}", Summary: "Delete one private search miss (operator only)"}, func(ctx context.Context, in *struct {
		ID string `path:"id" maxLength:"128"`
	}) (*struct{}, error) {
		if browserSessionRequest(ctx) {
			return nil, NewError(http.StatusForbidden, "forbidden", "miss log management requires the master API credential")
		}
		if err := g.mutate(func() error {
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
		if browserSessionRequest(ctx) {
			return nil, NewError(http.StatusForbidden, "forbidden", "miss log management requires the master API credential")
		}
		if err := g.mutate(func() error {
			err := d.Store.PurgeMissLogRecords(ctx)
			if err != nil {
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
func DocumentSearchMiss(query, profile string, fence DocumentSourceFence, report DocumentSearchReport) misslog.Request {
	fingerprint, _ := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: fence.VaultUID, ContentVersionIDs: fence.ContentVersionIDs})
	receipt := misslog.Request{Query: query, Profile: profile, RequestedMode: report.RequestedMode, ActualMode: report.ActualMode,
		FenceFingerprint: fingerprint, DocumentCount: new(len(fence.ContentVersionIDs)), Truncated: report.Truncated, Degradations: report.Degradations,
		Coverage: map[string]any{"binding_required": report.Coverage.BindingRequired, "scoped_documents": report.Coverage.ScopedDocuments,
			"complete_documents": report.Coverage.CompleteDocuments, "state": report.Coverage.State}}
	if report.Reranking != nil {
		receipt.Reranking = map[string]any{"outcome": report.Reranking.Outcome, "cause": report.Reranking.Cause, "candidate_count": report.Reranking.CandidateCount}
	}
	return receipt
}
