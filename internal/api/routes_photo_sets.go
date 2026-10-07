package api

import (
	"context"
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"net/http"
)

func registerPhotoSetRoutes(api huma.API, d Deps, g *gate) {
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	if err != nil {
		panic(err)
	}
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
	if err != nil {
		panic(err)
	}
	huma.Register(api, huma.Operation{OperationID: "listPhotoAlbums", Method: http.MethodGet, Path: "/api/v1/photos/albums", Summary: "List albums with included counts and ready covers"}, func(ctx context.Context, _ *struct{}) (*struct{ Body []PhotoAlbumSummary }, error) {
		sets, err := d.Store.ListPhotoSets(ctx, fingerprint)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := make([]PhotoAlbumSummary, len(sets))
		for i, set := range sets {
			out[i] = fromStorePhotoAlbumSummary(set)
		}
		return &struct{ Body []PhotoAlbumSummary }{Body: out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getPhotoAlbum", Method: http.MethodGet, Path: "/api/v1/photos/albums/{set_id}", Summary: "Inspect one album"}, func(ctx context.Context, in *struct {
		SetID string `path:"set_id"`
	}) (*struct {
		ETag string `header:"ETag"`
		Body PhotoAlbumSummary
	}, error) {
		set, err := d.Store.PhotoSet(ctx, in.SetID, fingerprint)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct {
			ETag string `header:"ETag"`
			Body PhotoAlbumSummary
		}{ETag: revisionETag(set.Revision), Body: fromStorePhotoAlbumSummary(set)}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "createPhotoAlbum", Method: http.MethodPost, Path: "/api/v1/photos/albums", Summary: "Create an empty album", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct{ Body PhotoAlbumNameRequest }) (*photoAlbumOutput, error) {
		var out *photoAlbumOutput
		err := g.mutate(func() error {
			set, err := d.Store.CreatePhotoSet(ctx, in.Body.Name)
			if err != nil {
				return FromStoreError(err)
			}
			out = &photoAlbumOutput{ETag: revisionETag(set.Revision), Body: fromStorePhotoAlbum(set)}
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "updatePhotoAlbum", Method: http.MethodPut, Path: "/api/v1/photos/albums/{set_id}", Summary: "Rename or star an album"}, func(ctx context.Context, in *struct {
		SetID   string `path:"set_id"`
		IfMatch string `header:"If-Match"`
		Body    UpdatePhotoAlbumRequest
	}) (*photoAlbumOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoAlbumOutput
		err = g.mutate(func() error {
			set, err := d.Store.UpdatePhotoSet(ctx, in.SetID, revision, in.Body.Name, in.Body.Starred, nil)
			if err != nil {
				return FromStoreError(err)
			}
			out = &photoAlbumOutput{ETag: revisionETag(set.Revision), Body: fromStorePhotoAlbum(set)}
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "setPhotoAlbumCover", Method: http.MethodPut, Path: "/api/v1/photos/albums/{set_id}/cover", Summary: "Choose or reset an album cover"}, func(ctx context.Context, in *struct {
		SetID   string `path:"set_id"`
		IfMatch string `header:"If-Match"`
		Body    PhotoAlbumCoverRequest
	}) (*photoAlbumOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoAlbumOutput
		err = g.mutate(func() error {
			set, err := d.Store.UpdatePhotoSet(ctx, in.SetID, revision, nil, nil, &in.Body.AssetID)
			if err != nil {
				return FromStoreError(err)
			}
			out = &photoAlbumOutput{ETag: revisionETag(set.Revision), Body: fromStorePhotoAlbum(set)}
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "duplicatePhotoAlbum", Method: http.MethodPost, Path: "/api/v1/photos/albums/{set_id}/duplicate", Summary: "Duplicate an album and its member order"}, func(ctx context.Context, in *struct {
		SetID   string `path:"set_id"`
		IfMatch string `header:"If-Match"`
		Body    PhotoAlbumNameRequest
	}) (*photoAlbumOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoAlbumOutput
		err = g.mutate(func() error {
			set, err := d.Store.DuplicatePhotoSet(ctx, in.SetID, revision, in.Body.Name)
			if err != nil {
				return FromStoreError(err)
			}
			out = &photoAlbumOutput{ETag: revisionETag(set.Revision), Body: fromStorePhotoAlbum(set)}
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "deletePhotoAlbum", Method: http.MethodDelete, Path: "/api/v1/photos/albums/{set_id}", Summary: "Delete an album and keep its photos"}, func(ctx context.Context, in *struct {
		SetID   string `path:"set_id"`
		IfMatch string `header:"If-Match"`
	}) (*photoAlbumOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoAlbumOutput
		err = g.mutate(func() error {
			set, err := d.Store.DeletePhotoSet(ctx, in.SetID, revision)
			if err != nil {
				return FromStoreError(err)
			}
			out = &photoAlbumOutput{ETag: revisionETag(set.Revision), Body: fromStorePhotoAlbum(set)}
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "addPhotoAlbumMembers", Method: http.MethodPost, Path: "/api/v1/photos/albums/{set_id}/members/add", Summary: "Add explicit photos or complete search results"}, func(ctx context.Context, in *struct {
		SetID   string `path:"set_id"`
		IfMatch string `header:"If-Match"`
		Body    PhotoAlbumMembersRequest
	}) (*photoAlbumOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		selection := store.PhotoSetSelection{AssetIDs: in.Body.AssetIDs, Coverage: store.CoverageSelection{Configuration: in.Body.Coverage.Configuration, ProfileFingerprint: in.Body.Coverage.ProfileFingerprint}}
		if in.Body.Query != nil {
			value, err := parseWorkspaceQuery(*in.Body.Query)
			if err != nil {
				return nil, err
			}
			selection.Query = &value
		}
		var out *photoAlbumOutput
		err = g.mutate(func() error {
			set, err := d.Store.ChangePhotoSetMembers(ctx, in.SetID, revision, true, selection)
			if err != nil {
				return workspaceQueryError(err)
			}
			out = &photoAlbumOutput{ETag: revisionETag(set.Revision), Body: fromStorePhotoAlbum(set)}
			return nil
		})
		return out, err
	})
	huma.Register(api, huma.Operation{OperationID: "removePhotoAlbumMembers", Method: http.MethodPost, Path: "/api/v1/photos/albums/{set_id}/members/remove", Summary: "Remove explicit photos or complete search results"}, func(ctx context.Context, in *struct {
		SetID   string `path:"set_id"`
		IfMatch string `header:"If-Match"`
		Body    PhotoAlbumMembersRequest
	}) (*photoAlbumOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		selection := store.PhotoSetSelection{AssetIDs: in.Body.AssetIDs, Coverage: store.CoverageSelection{Configuration: in.Body.Coverage.Configuration, ProfileFingerprint: in.Body.Coverage.ProfileFingerprint}}
		if in.Body.Query != nil {
			value, err := parseWorkspaceQuery(*in.Body.Query)
			if err != nil {
				return nil, err
			}
			selection.Query = &value
		}
		var out *photoAlbumOutput
		err = g.mutate(func() error {
			set, err := d.Store.ChangePhotoSetMembers(ctx, in.SetID, revision, false, selection)
			if err != nil {
				return workspaceQueryError(err)
			}
			out = &photoAlbumOutput{ETag: revisionETag(set.Revision), Body: fromStorePhotoAlbum(set)}
			return nil
		})
		return out, err
	})
}
