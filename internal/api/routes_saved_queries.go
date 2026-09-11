package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

type savedQueryOutput struct {
	ETag string `header:"ETag"`
	Body SavedQuery
}

type savedQueryPageOutput struct{ Body SavedQueryPage }

type optionalSavedQueryString struct {
	Set   bool
	Value string
}

func (o *optionalSavedQueryString) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return errors.New("saved query patch fields cannot be null")
	}
	o.Set = true
	return json.Unmarshal(raw, &o.Value)
}

func (optionalSavedQueryString) Schema(_ huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeString}
}

type optionalSavedQueryPayload struct {
	Set   bool
	Value SavedQueryPayload
}

func (o *optionalSavedQueryPayload) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return errors.New("saved query payload cannot be null")
	}
	var value SavedQueryPayload
	if err := value.UnmarshalJSON(raw); err != nil {
		return err
	}
	o.Set = true
	o.Value = value
	return nil
}

func (optionalSavedQueryPayload) Schema(r huma.Registry) *huma.Schema {
	return SavedQueryPayload{}.Schema(r)
}

func registerSavedQueryRoutes(api huma.API, d Deps, g *gate, snapshots *store.QuerySnapshotService) {
	huma.Register(api, huma.Operation{
		OperationID: "listSavedQueries", Method: http.MethodGet, Path: "/api/v1/saved-queries",
		Summary: "List saved query and highlight definitions by name",
	}, func(ctx context.Context, in *struct {
		Kind   string `query:"kind" enum:"query,highlight_set"`
		Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset int    `query:"offset" default:"0" minimum:"0"`
	}) (*savedQueryPageOutput, error) {
		items, total, err := d.Store.SavedQueries(ctx, in.Kind, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &savedQueryPageOutput{Body: SavedQueryPage{
			Items: []SavedQuery{}, Total: total, Limit: in.Limit, Offset: in.Offset,
		}}
		for _, item := range items {
			out.Body.Items = append(out.Body.Items, fromStoreSavedQuery(item))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "createSavedQuery", Method: http.MethodPost, Path: "/api/v1/saved-queries",
		Summary:       "Save one complete query or literal highlight set",
		Description:   "Stores intent only. This endpoint does not execute searches or highlight documents.",
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  maxSavedQueryRequestBytes,
	}, func(ctx context.Context, in *struct {
		Body SavedQueryCreateRequest
	}) (*savedQueryOutput, error) {
		var out *savedQueryOutput
		err := g.mutate(func() error {
			saved, err := d.Store.CreateSavedQuery(
				ctx, in.Body.Name, in.Body.Description, in.Body.Kind, in.Body.Payload,
			)
			if err != nil {
				return FromStoreError(err)
			}
			out = savedQueryResult(saved)
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "getSavedQuery", Method: http.MethodGet,
		Path:    "/api/v1/saved-queries/{saved_query_id}",
		Summary: "Inspect one saved definition by stable ID",
	}, func(ctx context.Context, in *struct {
		SavedQueryID string `path:"saved_query_id"`
	}) (*savedQueryOutput, error) {
		saved, err := d.Store.SavedQueryByID(ctx, in.SavedQueryID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return savedQueryResult(saved), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "updateSavedQuery", Method: http.MethodPatch,
		Path:         "/api/v1/saved-queries/{saved_query_id}",
		Summary:      "Edit a saved definition under its current revision",
		MaxBodyBytes: maxSavedQueryRequestBytes,
	}, func(ctx context.Context, in *struct {
		SavedQueryID string `path:"saved_query_id"`
		IfMatch      string `header:"If-Match"`
		Body         struct {
			Name        optionalSavedQueryString  `json:"name,omitzero" doc:"Normalized to NFC; must contain 1 to 256 UTF-8 bytes after NFC normalization."`
			Description optionalSavedQueryString  `json:"description,omitzero"`
			Payload     optionalSavedQueryPayload `json:"payload,omitzero"`
		}
	}) (*savedQueryOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		if !in.Body.Name.Set && !in.Body.Description.Set && !in.Body.Payload.Set {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_saved_query",
				"saved query patch must set name, description, and/or payload")
		}
		patch := store.SavedQueryPatch{}
		if in.Body.Name.Set {
			patch.Name = &in.Body.Name.Value
		}
		if in.Body.Description.Set {
			patch.Description = &in.Body.Description.Value
		}
		if in.Body.Payload.Set {
			payload := []byte(in.Body.Payload.Value)
			patch.Payload = &payload
		}
		var out *savedQueryOutput
		err = g.mutate(func() error {
			saved, err := d.Store.UpdateSavedQuery(ctx, in.SavedQueryID, revision, patch)
			if err != nil {
				return FromStoreError(err)
			}
			out = savedQueryResult(saved)
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "deleteSavedQuery", Method: http.MethodDelete,
		Path:    "/api/v1/saved-queries/{saved_query_id}",
		Summary: "Delete a saved definition under its current revision",
	}, func(ctx context.Context, in *struct {
		SavedQueryID string `path:"saved_query_id"`
		IfMatch      string `header:"If-Match"`
	}) (*savedQueryOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *savedQueryOutput
		err = g.mutate(func() error {
			saved, err := d.Store.DeleteSavedQuery(ctx, in.SavedQueryID, revision)
			if err != nil {
				return FromStoreError(err)
			}
			out = savedQueryResult(saved)
			return nil
		})
		return out, err
	})

	registerSavedQueryRunRoute(api, d, g, snapshots)
}

func registerSavedQueryRunRoute(
	api huma.API, d Deps, g *gate, snapshots *store.QuerySnapshotService,
) {
	huma.Register(api, huma.Operation{
		OperationID: "runSavedQuery", Method: http.MethodPost,
		Path:         "/api/v1/saved-queries/{saved_query_id}/runs",
		Summary:      "Run one revision-fenced saved query and retain its receipt",
		MaxBodyBytes: 32 << 10,
	}, func(ctx context.Context, in *struct {
		SavedQueryID string `path:"saved_query_id"`
		IfMatch      string `header:"If-Match"`
		Body         SavedQueryRunRequest
	}) (*struct{ Body SavedQueryRunResult }, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		owner, ok := workspaceSnapshotOwner(ctx)
		if !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated snapshot owner is missing")
		}
		selection, err := selectCollectionProfile(d.Cfg, in.Body.Profile)
		if err != nil {
			return nil, err
		}
		if snapshots == nil {
			return nil, NewError(http.StatusServiceUnavailable, "workspace_unavailable", "workspace query snapshots are unavailable")
		}
		var result SavedQueryRunResult
		err = g.mutate(func() error {
			run, page, runErr := snapshots.RunSaved(ctx, owner, in.SavedQueryID, revision, store.SnapshotRequest{
				Coverage: selection.Coverage, PageSize: in.Body.PageSize, Facets: in.Body.Facets,
			})
			if runErr != nil {
				return workspaceQueryError(runErr)
			}
			response, encodeErr := fromStoreWorkspacePage(page)
			if encodeErr != nil {
				return NewError(http.StatusInternalServerError, "internal", "could not encode saved query run")
			}
			result = SavedQueryRunResult{Run: fromStoreSavedQueryRun(run), Snapshot: response}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return &struct{ Body SavedQueryRunResult }{Body: result}, nil
	})
}

func savedQueryResult(saved store.SavedQuery) *savedQueryOutput {
	return &savedQueryOutput{
		ETag: strconv.Quote(strconv.FormatInt(saved.Revision, 10)),
		Body: fromStoreSavedQuery(saved),
	}
}

func fromStoreSavedQuery(saved store.SavedQuery) SavedQuery {
	return SavedQuery{
		ID: saved.ID, Name: saved.Name, Description: saved.Description, Kind: saved.Kind,
		Payload: SavedQueryPayload(bytes.Clone(saved.Payload)), Fingerprint: saved.Fingerprint,
		Revision: saved.Revision, CreatedAt: saved.CreatedAt, UpdatedAt: saved.UpdatedAt,
	}
}
