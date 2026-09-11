package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func registerWorkspaceQueryRoutes(api huma.API, d Deps, service *store.QuerySnapshotService) {
	huma.Register(api, huma.Operation{
		OperationID: "createWorkspaceQuery", Method: http.MethodPost,
		Path: "/api/v1/workspace/queries", Summary: "Create an exact bounded query snapshot",
		MaxBodyBytes: maxWorkspaceQueryRequestBytes,
	}, func(ctx context.Context, in *struct{ Body WorkspaceQueryCreateRequest }) (*struct{ Body WorkspaceQueryResponse }, error) {
		owner, ok := workspaceSnapshotOwner(ctx)
		if !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated snapshot owner is missing")
		}
		value, err := parseWorkspaceQuery(in.Body.Query)
		if err != nil {
			return nil, err
		}
		selection, err := selectCollectionProfile(d.Cfg, in.Body.Profile)
		if err != nil {
			return nil, err
		}
		if service == nil {
			return nil, NewError(http.StatusServiceUnavailable, "workspace_unavailable", "workspace query snapshots are unavailable")
		}
		page, err := service.Create(ctx, owner, store.SnapshotRequest{
			Query: value, Coverage: selection.Coverage, PageSize: in.Body.PageSize, Facets: in.Body.Facets,
		})
		if err != nil {
			return nil, workspaceQueryError(err)
		}
		response, err := fromStoreWorkspacePage(page)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", "could not encode workspace query")
		}
		return &struct{ Body WorkspaceQueryResponse }{Body: response}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "readWorkspaceQueryPage", Method: http.MethodPost,
		Path: "/api/v1/workspace/queries/{id}/pages", Summary: "Read one exact snapshot page",
		MaxBodyBytes: 4 << 10,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body WorkspaceQueryPageRequest
	}) (*struct{ Body WorkspaceQueryResponse }, error) {
		owner, ok := workspaceSnapshotOwner(ctx)
		if !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated snapshot owner is missing")
		}
		if service == nil {
			return nil, NewError(http.StatusServiceUnavailable, "workspace_unavailable", "workspace query snapshots are unavailable")
		}
		page, err := service.Page(ctx, owner, in.ID, in.Body.Cursor)
		if err != nil {
			return nil, workspaceQueryError(err)
		}
		response, err := fromStoreWorkspacePage(page)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", "could not encode workspace query page")
		}
		return &struct{ Body WorkspaceQueryResponse }{Body: response}, nil
	})
}

func parseWorkspaceQuery(raw QueryPayload) (query.Query, error) {
	value, err := query.Parse(raw)
	if err == nil {
		return value, nil
	}
	problem := NewError(http.StatusUnprocessableEntity, "invalid_query", err.Error())
	problem.Position = &ErrorPosition{}
	return query.Query{}, problem
}

func workspaceQueryError(err error) error {
	if positioned, ok := errors.AsType[*query.ExpressionError](err); ok {
		problem := NewError(http.StatusUnprocessableEntity, "invalid_query", positioned.Message)
		problem.Position = &ErrorPosition{Offset: positioned.Offset, End: positioned.End}
		return problem
	}
	switch {
	case errors.Is(err, store.ErrSnapshotGone):
		return NewError(http.StatusGone, "snapshot_gone", "query snapshot is missing, expired, or revoked")
	case errors.Is(err, store.ErrSnapshotCursor):
		return NewError(http.StatusBadRequest, "invalid_cursor", "query snapshot cursor is invalid")
	case errors.Is(err, store.ErrSnapshotAdmission):
		return NewError(http.StatusTooManyRequests, "snapshot_capacity", "query snapshot cache capacity is unavailable")
	case errors.Is(err, store.ErrSnapshotBusy):
		return NewError(http.StatusTooManyRequests, "snapshot_busy", "query snapshot builders are busy")
	case errors.Is(err, store.ErrQuerySnapshotTooLarge):
		return NewError(http.StatusRequestEntityTooLarge, "snapshot_too_large", "query snapshot exceeds its bounded size")
	case errors.Is(err, store.ErrInvalidCoverageSelection):
		return NewError(http.StatusUnprocessableEntity, "invalid_profile", err.Error())
	case errors.Is(err, store.ErrInvalidSavedQueryRun):
		return NewError(http.StatusUnprocessableEntity, "invalid_saved_query_run", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(http.StatusServiceUnavailable, "snapshot_unavailable", "query snapshot did not finish within its resource budget")
	default:
		return FromStoreError(err)
	}
}
