package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func registerPhotoTimelineRoute(api huma.API, service *store.QuerySnapshotService) {
	huma.Register(api, huma.Operation{
		OperationID: "getPhotoTimeline", Method: http.MethodPost, Path: "/api/v1/photos/assets/timeline",
		Summary:      "Count matching photo assets by recorded capture day",
		MaxBodyBytes: query.MaxInputBytes + (32 << 10),
	}, func(ctx context.Context, in *struct{ Body PhotoTimelineRequest }) (*struct{ Body PhotoTimeline }, error) {
		owner, ok := workspaceSnapshotOwner(ctx)
		if !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated snapshot owner is missing")
		}
		value, err := parseWorkspaceQuery(in.Body.Query)
		if err != nil {
			return nil, err
		}
		if service == nil {
			return nil, NewError(http.StatusServiceUnavailable, "workspace_unavailable", "photo timeline counts are unavailable")
		}
		projection, err := service.CreateFacets(ctx, owner, store.SnapshotRequest{
			FacetsOnly: true, Query: value, Facets: []string{"capture_day"},
			Coverage: store.CoverageSelection{
				Configuration:      in.Body.Coverage.Configuration,
				ProfileFingerprint: in.Body.Coverage.ProfileFingerprint,
			},
		})
		if err != nil {
			return nil, workspaceQueryError(err)
		}
		timeline, err := fromStorePhotoTimeline(projection)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", "could not encode photo timeline")
		}
		return &struct{ Body PhotoTimeline }{Body: timeline}, nil
	})
}
