package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

const missLogCacheControlNoStore = "no-store"

// MissLogPage contains sensitive receipts and is available only to the operator.
type MissLogPage struct {
	Items  []misslog.Receipt `json:"items"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

func missLogFailure() error {
	return NewError(http.StatusInternalServerError, "miss_log_unavailable", "miss log operation failed")
}

func registerMissLogRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{OperationID: "recordMiss", Method: http.MethodPost, Path: "/api/v1/miss-log", Summary: "Record a submitted empty search", DefaultStatus: http.StatusNoContent, MaxBodyBytes: 64 << 10}, func(ctx context.Context, in *struct{ Body misslog.Request }) (*struct{}, error) {
		_, hasTextCoverage := in.Body.Coverage["text"]
		tagID, _ := in.Body.Filters["tag_id"].(string)
		plainScope := in.Body.DocumentCount == nil && in.Body.FenceFingerprint == "" &&
			(len(in.Body.Filters) == 0 || len(in.Body.Filters) == 1 && tagID != "")
		if d.Cfg.MissLog.Enabled && in.Body.ActualMode == "lexical" && strings.TrimSpace(in.Body.Query) != "" && !hasTextCoverage && plainScope {
			// Web and TUI submissions lack the CLI's text coverage lookup.
			// Other scopes retain only the coverage supplied by their caller.
			// Keep enrichment best-effort, like receipt persistence itself.
			if coverage, err := d.Store.SearchTextCoverage(ctx, store.SearchOptions{TagID: tagID}); err == nil {
				if in.Body.Coverage == nil {
					in.Body.Coverage = make(map[string]any)
				}
				in.Body.Coverage["text"] = SearchTextCoverage(coverage)
			}
		}
		d.MissLog.Record(ctx, in.Body)
		return &struct{}{}, nil
	})
	type pageOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         MissLogPage
	}
	huma.Register(api, huma.Operation{OperationID: "listMissLog", Method: http.MethodGet, Path: "/api/v1/miss-log", Summary: "List private search misses (operator only)"}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" default:"20" minimum:"1" maximum:"100"`
		Offset int `query:"offset" default:"0" minimum:"0"`
	}) (*pageOutput, error) {
		items, err := d.MissLog.List(ctx, in.Limit, in.Offset)
		if err != nil {
			return nil, missLogFailure()
		}
		page := MissLogPage{Items: items, Limit: in.Limit, Offset: in.Offset}
		return &pageOutput{CacheControl: missLogCacheControlNoStore, Body: page}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "deleteMissLogRecord", Method: http.MethodDelete, Path: "/api/v1/miss-log/{id}", Summary: "Delete one private search miss (operator only)"}, func(ctx context.Context, in *struct {
		ID string `path:"id" maxLength:"128"`
	}) (*struct{}, error) {
		if err := d.MissLog.Delete(ctx, in.ID); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, NewError(http.StatusNotFound, "not_found", "miss log record not found")
			}
			return nil, missLogFailure()
		}
		return &struct{}{}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "deleteMissLog", Method: http.MethodDelete, Path: "/api/v1/miss-log", Summary: "Purge private search misses (operator only)"}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		if err := d.MissLog.Purge(ctx); err != nil {
			return nil, missLogFailure()
		}
		return &struct{}{}, nil
	})
}

// DocumentSearchMiss keeps the compact outcome without source ID lists.
func DocumentSearchMiss(query, profile string, request DocumentSearchRequest, report DocumentSearchReport) misslog.Request {
	receipt := misslog.Request{Query: query, Profile: profile, BindingID: request.BindingID,
		RequestedMode: report.RequestedMode, ActualMode: report.ActualMode,
		Truncated: report.Truncated, Degradations: report.Degradations,
		Coverage: map[string]any{"binding_required": report.Coverage.BindingRequired, "scoped_documents": report.Coverage.ScopedDocuments,
			"complete_documents": report.Coverage.CompleteDocuments, "state": report.Coverage.State}}
	if request.Scope != nil {
		receipt.Filters = misslog.ReceiptScope(request.Scope.Filters)
	}
	if request.Scope == nil && (request.Fence.VaultUID != "" || len(request.Fence.ContentVersionIDs) > 0) {
		receipt.DocumentCount = new(len(request.Fence.ContentVersionIDs))
	} else if report.Coverage.State != "" && report.Coverage.State != "unknown" {
		receipt.DocumentCount = new(report.Coverage.ScopedDocuments)
	}
	if request.Scope == nil && len(request.Fence.ContentVersionIDs) > 0 {
		fingerprint, _ := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: request.Fence.VaultUID, ContentVersionIDs: request.Fence.ContentVersionIDs})
		receipt.FenceFingerprint = fingerprint
	}
	if report.Reranking != nil {
		receipt.Reranking = map[string]any{"outcome": report.Reranking.Outcome, "cause": report.Reranking.Cause, "candidate_count": report.Reranking.CandidateCount}
	}
	return receipt
}
