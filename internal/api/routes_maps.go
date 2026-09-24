package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

const maxMapRequestBytes = (1 << 20) + (64 << 10)

type mapPlanRequest struct {
	Definition store.ContentMapDefinition `json:"definition"`
}

type mapWriteRequest struct {
	Definition       store.ContentMapDefinition `json:"definition"`
	DefinitionDigest string                     `json:"definition_digest"`
}

type mapRefreshRequest struct {
	PreviousSnapshotID string `json:"previous_snapshot_id"`
}

type mapOutput struct {
	ETag string `header:"ETag"`
	Body store.ContentMap
}

func mapResult(record store.ContentMap) *mapOutput {
	return &mapOutput{ETag: strconv.Quote(strconv.FormatInt(record.Revision, 10)), Body: record}
}

func mapAccess(ctx context.Context) (store.MapAccess, error) {
	// Browser sessions have a narrower route set and need a source-scoped
	// operation principal before being permitted to curate maps.
	if browserSessionRequest(ctx) {
		return store.MapAccess{}, NewError(http.StatusForbidden, "map_scope_unavailable", "Map access is unavailable to this session.")
	}
	return store.MapAccess{Owner: "local", AllSources: true}, nil
}

func mapRouteError(err error) error {
	switch {
	case errors.Is(err, store.ErrInvalidContentMap):
		return NewError(http.StatusUnprocessableEntity, "invalid_content_map", "The structured map definition is invalid.")
	case errors.Is(err, store.ErrProcessingSourceFenceScopeTooLarge), errors.Is(err, store.ErrQuerySnapshotTooLarge):
		return NewError(http.StatusUnprocessableEntity, "map_scope_too_large", "Narrow the map to at most 4,096 sources.")
	case errors.Is(err, store.ErrNotFound):
		return NewError(http.StatusNotFound, "map_unavailable", "The authorized map or source is unavailable.")
	default:
		return FromStoreError(err)
	}
}

// registerMapRoutes is called by server.go, owned by the integration parent.
func registerMapRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "proposeContentMap", Method: http.MethodPost,
		Path: "/api/v1/maps/proposals", Summary: "Propose a map from an existing tag or scoped query",
		MaxBodyBytes: maxMapRequestBytes}, func(ctx context.Context, in *struct {
		Body store.ContentMapProposalRequest
	}) (*struct{ Body store.ContentMapPlan }, error) {
		access, err := mapAccess(ctx)
		if err != nil {
			return nil, err
		}
		plan, err := d.Store.ProposeContentMap(ctx, access, in.Body)
		if err != nil {
			return nil, mapRouteError(err)
		}
		return &struct{ Body store.ContentMapPlan }{Body: plan}, nil
	})

	huma.Register(api, huma.Operation{OperationID: "previewContentMap", Method: http.MethodPost,
		Path: "/api/v1/maps/plans", Summary: "Preview a structured map definition",
		MaxBodyBytes: maxMapRequestBytes}, func(ctx context.Context, in *struct{ Body mapPlanRequest }) (*struct{ Body store.ContentMapPlan }, error) {
		access, err := mapAccess(ctx)
		if err != nil {
			return nil, err
		}
		plan, err := d.Store.PreviewContentMap(ctx, access, in.Body.Definition)
		if err != nil {
			return nil, mapRouteError(err)
		}
		return &struct{ Body store.ContentMapPlan }{Body: plan}, nil
	})

	huma.Register(api, huma.Operation{OperationID: "createContentMap", Method: http.MethodPost,
		Path: "/api/v1/maps", Summary: "Save an accepted structured map plan",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: maxMapRequestBytes}, func(ctx context.Context, in *struct{ Body mapWriteRequest }) (*mapOutput, error) {
		access, err := mapAccess(ctx)
		if err != nil {
			return nil, err
		}
		var result *mapOutput
		err = g.mutate(func() error {
			created, createErr := d.Store.CreateContentMap(ctx, access, in.Body.Definition, in.Body.DefinitionDigest)
			if createErr != nil {
				return mapRouteError(createErr)
			}
			result = mapResult(created)
			return nil
		})
		return result, err
	})

	huma.Register(api, huma.Operation{OperationID: "getContentMap", Method: http.MethodGet,
		Path: "/api/v1/maps/{map_id}", Summary: "Read a structured map definition"}, func(ctx context.Context, in *struct {
		MapID string `path:"map_id"`
	}) (*mapOutput, error) {
		access, err := mapAccess(ctx)
		if err != nil {
			return nil, err
		}
		result, err := d.Store.ContentMapByID(ctx, access, in.MapID)
		if err != nil {
			return nil, mapRouteError(err)
		}
		if result.ArchivedAt != "" {
			return nil, mapRouteError(store.ErrNotFound)
		}
		return mapResult(result), nil
	})

	huma.Register(api, huma.Operation{OperationID: "updateContentMap", Method: http.MethodPatch,
		Path: "/api/v1/maps/{map_id}", Summary: "Replace a map definition at an expected revision",
		MaxBodyBytes: maxMapRequestBytes}, func(ctx context.Context, in *struct {
		MapID   string `path:"map_id"`
		IfMatch string `header:"If-Match"`
		Body    mapWriteRequest
	}) (*mapOutput, error) {
		access, err := mapAccess(ctx)
		if err != nil {
			return nil, err
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var result *mapOutput
		err = g.mutate(func() error {
			updated, updateErr := d.Store.UpdateContentMap(ctx, access, in.MapID, revision,
				in.Body.Definition, in.Body.DefinitionDigest)
			if updateErr != nil {
				return mapRouteError(updateErr)
			}
			result = mapResult(updated)
			return nil
		})
		return result, err
	})

	huma.Register(api, huma.Operation{OperationID: "archiveContentMap", Method: http.MethodDelete,
		Path: "/api/v1/maps/{map_id}", Summary: "Archive a map while retaining snapshots"}, func(ctx context.Context, in *struct {
		MapID   string `path:"map_id"`
		IfMatch string `header:"If-Match"`
	}) (*mapOutput, error) {
		access, err := mapAccess(ctx)
		if err != nil {
			return nil, err
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var result *mapOutput
		err = g.mutate(func() error {
			archived, archiveErr := d.Store.ArchiveContentMap(ctx, access, in.MapID, revision)
			if archiveErr != nil {
				return mapRouteError(archiveErr)
			}
			result = mapResult(archived)
			return nil
		})
		return result, err
	})

	huma.Register(api, huma.Operation{OperationID: "createContentMapSnapshot", Method: http.MethodPost,
		Path: "/api/v1/maps/{map_id}/snapshots", Summary: "Freeze permitted exact map membership",
		DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct {
		MapID   string `path:"map_id"`
		IfMatch string `header:"If-Match"`
	}) (*struct{ Body store.ContentMapSnapshot }, error) {
		access, err := mapAccess(ctx)
		if err != nil {
			return nil, err
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var result *struct{ Body store.ContentMapSnapshot }
		err = g.mutate(func() error {
			snapshot, createErr := d.Store.CreateContentMapSnapshot(ctx, access, in.MapID, revision)
			if createErr != nil {
				return mapRouteError(createErr)
			}
			result = &struct{ Body store.ContentMapSnapshot }{Body: snapshot}
			return nil
		})
		return result, err
	})

	huma.Register(api, huma.Operation{OperationID: "refreshContentMap", Method: http.MethodPost,
		Path: "/api/v1/maps/{map_id}/refresh", Summary: "Freeze a refreshed map and compare it with the previous snapshot",
		DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct {
		MapID   string `path:"map_id"`
		IfMatch string `header:"If-Match" required:"true"`
		Body    mapRefreshRequest
	}) (*struct{ Body store.ContentMapRefresh }, error) {
		access, err := mapAccess(ctx)
		if err != nil {
			return nil, err
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var result *struct{ Body store.ContentMapRefresh }
		err = g.mutate(func() error {
			refresh, refreshErr := d.Store.RefreshContentMap(ctx, access, in.MapID, revision, in.Body.PreviousSnapshotID)
			if refreshErr != nil {
				return mapRouteError(refreshErr)
			}
			result = &struct{ Body store.ContentMapRefresh }{Body: refresh}
			return nil
		})
		return result, err
	})

	huma.Register(api, huma.Operation{OperationID: "getContentMapSnapshot", Method: http.MethodGet,
		Path: "/api/v1/map-snapshots/{snapshot_id}", Summary: "Read one immutable authorized map snapshot"}, func(ctx context.Context, in *struct {
		SnapshotID string `path:"snapshot_id"`
	}) (*struct{ Body store.ContentMapSnapshot }, error) {
		access, err := mapAccess(ctx)
		if err != nil {
			return nil, err
		}
		snapshot, err := d.Store.ContentMapSnapshotByID(ctx, access, in.SnapshotID)
		if err != nil {
			return nil, mapRouteError(err)
		}
		return &struct{ Body store.ContentMapSnapshot }{Body: snapshot}, nil
	})
}
