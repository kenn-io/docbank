package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

type PhotoRejectsRequest struct {
	Query    QueryPayload           `json:"query"`
	Hidden   bool                   `json:"hidden,omitempty"`
	Coverage WorkspaceQueryCoverage `json:"coverage,omitzero"`
}

type MovePhotoRejectsRequest struct {
	Hidden  bool                      `json:"hidden,omitempty"`
	Targets []store.PhotoRejectTarget `json:"targets" maxItems:"1000"`
}

type PhotoRejectsPreflight = store.PhotoRejectsPreflight
type PhotoRejectsMoved = store.PhotoRejectsMoved

func registerPhotoRejectRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "preflightPhotoRejects", Method: http.MethodPost, Path: "/api/v1/photos/rejects/preflight", Summary: "Preview rejected photos in the current scope", MaxBodyBytes: query.MaxInputBytes + (64 << 10)}, func(ctx context.Context, in *struct{ Body PhotoRejectsRequest }) (*struct{ Body PhotoRejectsPreflight }, error) {
		value, err := query.Parse(in.Body.Query)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_query", err.Error())
		}
		result, err := d.Store.PreflightPhotoRejects(ctx, store.PhotoRejectsRequest{Query: value, Hidden: in.Body.Hidden, Coverage: store.CoverageSelection{Configuration: in.Body.Coverage.Configuration, ProfileFingerprint: in.Body.Coverage.ProfileFingerprint}})
		if err != nil {
			return nil, workspaceQueryError(err)
		}
		return &struct{ Body PhotoRejectsPreflight }{Body: result}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "movePhotoRejects", Method: http.MethodPost, Path: "/api/v1/photos/rejects/trash", Summary: "Move previewed rejected photos to trash", MaxBodyBytes: 256 << 10}, func(ctx context.Context, in *struct{ Body MovePhotoRejectsRequest }) (*struct{ Body PhotoRejectsMoved }, error) {
		var result PhotoRejectsMoved
		err := g.mutate(func() error {
			var err error
			result, err = d.Store.MovePhotoRejects(ctx, in.Body.Hidden, in.Body.Targets)
			return workspaceQueryError(err)
		})
		if err != nil {
			return nil, err
		}
		return &struct{ Body PhotoRejectsMoved }{Body: result}, nil
	})
}
