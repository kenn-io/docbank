package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

type collectionOutput struct{ Body Collection }
type collectionPageOutput struct{ Body CollectionPage }
type collectionMemberPageOutput struct{ Body CollectionMemberPage }
type collectionLabelOutput struct {
	ETag string `header:"ETag"`
	Body CollectionLabel
}

func registerCollectionRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "listCollections", Method: http.MethodGet, Path: "/api/v1/collections",
		Summary:     "List document-bearing ingest runs, newest first",
		Description: "Counts and bytes describe current live members. Empty runs remain directly accessible only when they retain label authority.",
	}, func(ctx context.Context, in *struct {
		Limit   int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset  int    `query:"offset" default:"0" minimum:"0"`
		Profile string `query:"profile" maxLength:"128"`
	}) (*collectionPageOutput, error) {
		selection, err := selectCollectionProfile(d.Cfg, in.Profile)
		if err != nil {
			return nil, err
		}
		collections, total, err := d.Store.Collections(ctx, in.Limit, in.Offset, selection.Coverage)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &collectionPageOutput{Body: CollectionPage{
			Items: []Collection{}, Total: total, Limit: in.Limit, Offset: in.Offset,
		}}
		for _, collection := range collections {
			out.Body.Items = append(out.Body.Items, fromStoreCollection(collection, selection))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getCollection", Method: http.MethodGet, Path: "/api/v1/collections/{id}",
		Summary: "Inspect one ingest run and its current live summary",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		Profile string `query:"profile" maxLength:"128"`
	}) (*collectionOutput, error) {
		selection, err := selectCollectionProfile(d.Cfg, in.Profile)
		if err != nil {
			return nil, err
		}
		collection, err := d.Store.CollectionByID(ctx, in.ID, selection.Coverage)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &collectionOutput{Body: fromStoreCollection(collection, selection)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listCollectionMembers", Method: http.MethodGet,
		Path:    "/api/v1/collections/{id}/members",
		Summary: "List a collection's current live file members",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		Limit   int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset  int    `query:"offset" default:"0" minimum:"0"`
		Profile string `query:"profile" maxLength:"128"`
	}) (*collectionMemberPageOutput, error) {
		selection, err := selectCollectionProfile(d.Cfg, in.Profile)
		if err != nil {
			return nil, err
		}
		page, err := d.Store.CollectionMembers(ctx, in.ID, in.Limit, in.Offset, selection.Coverage)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &collectionMemberPageOutput{Body: CollectionMemberPage{
			Collection: fromStoreCollection(page.Collection, selection), Items: []Node{},
			Total: page.Total, Limit: in.Limit, Offset: in.Offset,
		}}
		for _, member := range page.Items {
			node := fromStoreNode(member.Node)
			node.Path = member.Path
			out.Body.Items = append(out.Body.Items, node)
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getCollectionLabel", Method: http.MethodGet,
		Path:    "/api/v1/collections/{id}/label",
		Summary: "Read a collection's independently revisioned label",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*collectionLabelOutput, error) {
		label, err := d.Store.CollectionLabel(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return collectionLabelResponse(label), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "setCollectionLabel", Method: http.MethodPut,
		Path:        "/api/v1/collections/{id}/label",
		Summary:     "Set or clear a collection label under its label revision",
		Description: "Requires If-Match from the dedicated label resource. Label changes are unavailable after permanent audit authority is enabled.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		IfMatch string `header:"If-Match"`
		Body    struct {
			Label *string `json:"label" required:"true"`
		}
	}) (*collectionLabelOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *collectionLabelOutput
		err = g.mutate(func() error {
			label, setErr := d.Store.SetCollectionLabel(ctx, in.ID, revision, in.Body.Label)
			if setErr != nil {
				return FromStoreError(setErr)
			}
			out = collectionLabelResponse(label)
			return nil
		})
		return out, err
	})
}

func collectionLabelResponse(label store.CollectionLabel) *collectionLabelOutput {
	return &collectionLabelOutput{
		ETag: collectionLabelETag(label.Revision), Body: fromStoreCollectionLabel(label),
	}
}

func collectionLabelETag(revision int64) string {
	return fmt.Sprintf("%q", strconv.FormatInt(revision, 10))
}
