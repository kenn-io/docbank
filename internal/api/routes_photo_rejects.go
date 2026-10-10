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
	Coverage WorkspaceQueryCoverage `json:"coverage,omitzero"`
	Hidden   bool                   `json:"hidden,omitempty"`
	Digest   string                 `json:"digest,omitempty" maxLength:"64"`
}

type PhotoRejectsPreflight = store.PhotoRejectsPreflight

func registerPhotoRejectRoutes(api huma.API, d Deps, g *gate) {
	for _, confirm := range []bool{false, true} {
		id, path, summary := "preflightPhotoRejects", "/api/v1/photos/rejects/preflight", "Preview rejected photos in the current scope"
		if confirm {
			id, path, summary = "movePhotoRejects", "/api/v1/photos/rejects/trash", "Move confirmed rejects to recoverable trash"
		}
		huma.Register(api, huma.Operation{OperationID: id, Method: http.MethodPost, Path: path, Summary: summary, MaxBodyBytes: query.MaxInputBytes + (64 << 10)}, func(ctx context.Context, in *struct{ Body PhotoRejectsRequest }) (*struct{ Body PhotoRejectsPreflight }, error) {
			value, err := query.Parse(in.Body.Query)
			if err != nil {
				return nil, NewError(http.StatusUnprocessableEntity, "invalid_query", err.Error())
			}
			request := store.PhotoRejectsRequest{Query: value, Hidden: in.Body.Hidden, Coverage: store.CoverageSelection{Configuration: in.Body.Coverage.Configuration, ProfileFingerprint: in.Body.Coverage.ProfileFingerprint}}
			var result PhotoRejectsPreflight
			if confirm {
				err = g.mutate(func() error {
					var callErr error
					result, callErr = d.Store.MovePhotoRejects(ctx, request, in.Body.Digest)
					return FromStoreError(callErr)
				})
			} else {
				result, err = d.Store.PreflightPhotoRejects(ctx, request)
				err = workspaceQueryError(err)
			}
			if err != nil {
				return nil, err
			}
			return &struct{ Body PhotoRejectsPreflight }{Body: result}, nil
		})
	}
}
