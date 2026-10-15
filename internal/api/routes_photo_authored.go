package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

type PhotoAuthoredPrepareRequest struct {
	PhotoSelectionRequest

	Hidden          bool `json:"hidden,omitempty"`
	SkipUnavailable bool `json:"skip_unavailable,omitempty"`
}

type PhotoAuthoredEditRequest struct {
	Targets       []store.PhotoAuthoredEditTarget `json:"targets" minItems:"1" maxItems:"1000"`
	Patch         store.PhotoAuthoredPatch        `json:"patch"`
	RotationDelta *int                            `json:"rotation_delta,omitempty" enum:"90" doc:"Rotate each target clockwise by 90 degrees."`
	Keywords      []store.PhotoKeywordEdit        `json:"keywords,omitempty" maxItems:"100"`
}

type PhotoAuthoredPreparation struct {
	Assets  []PhotoAsset                `json:"assets"`
	Targets []store.PhotoPreparedTarget `json:"targets"`
}

func registerPhotoAuthoredRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "preparePhotoAuthored", Method: http.MethodPost, Path: "/api/v1/photos/authored/prepare", Summary: "Resolve a revision-bound metadata editing session"}, func(ctx context.Context, in *struct{ Body PhotoAuthoredPrepareRequest }) (*struct {
		Body PhotoAuthoredPreparation
	}, error) {
		selection := store.PhotoSetSelection{AssetIDs: in.Body.AssetIDs, Hidden: in.Body.Hidden, SkipUnavailable: in.Body.SkipUnavailable, Coverage: store.CoverageSelection{Configuration: in.Body.Coverage.Configuration, ProfileFingerprint: in.Body.Coverage.ProfileFingerprint}}
		if in.Body.Query != nil {
			value, err := parseWorkspaceQuery(*in.Body.Query)
			if err != nil {
				return nil, err
			}
			selection.Query = &value
		}
		result, err := d.Store.PreparePhotoAuthored(ctx, selection)
		body := PhotoAuthoredPreparation{Assets: []PhotoAsset{}, Targets: result.Targets}
		for _, asset := range result.Assets {
			body.Assets = append(body.Assets, fromStorePhotoAsset(asset))
		}
		return &struct {
			Body PhotoAuthoredPreparation
		}{body}, workspaceQueryError(err)
	})

	huma.Register(api, huma.Operation{OperationID: "editPhotoAuthored", Method: http.MethodPost, Path: "/api/v1/photos/authored/edit", Summary: "Edit photo fields and keywords atomically", Description: "A no-op returns an empty receipt_id and empty before and after arrays. It creates no receipt and leaves the last real edit undoable."}, func(ctx context.Context, in *struct{ Body PhotoAuthoredEditRequest }) (*struct{ Body store.PhotoAuthoredReceipt }, error) {
		request := store.PhotoAuthoredBatch{Targets: in.Body.Targets, Patch: in.Body.Patch, RotationDelta: in.Body.RotationDelta, Keywords: in.Body.Keywords}
		var result store.PhotoAuthoredReceipt
		err := g.mutate(func() error {
			var err error
			result, err = d.Store.EditPhotoAuthoredBatch(ctx, request)
			if errors.Is(err, store.ErrPhotoEditReceiptLimit) {
				err = fmt.Errorf("%w. Select fewer photos or apply fewer changes at once", err)
			}
			return workspaceQueryError(err)
		})
		return &struct{ Body store.PhotoAuthoredReceipt }{result}, err
	})
	huma.Register(api, huma.Operation{OperationID: "undoPhotoAuthored", Method: http.MethodPost, Path: "/api/v1/photos/authored/{receipt_id}/undo", Summary: "Undo photo fields and keywords if their revisions still match"}, func(ctx context.Context, in *struct {
		ReceiptID string `path:"receipt_id" format:"uuid"`
	}) (*struct{ Body store.PhotoAuthoredReceipt }, error) {
		var result store.PhotoAuthoredReceipt
		err := g.mutate(func() error {
			var err error
			result, err = d.Store.UndoPhotoAuthored(ctx, in.ReceiptID)
			return FromStoreError(err)
		})
		return &struct{ Body store.PhotoAuthoredReceipt }{result}, err
	})
}

func photoAuthoredBrowserRequestAllowed(r *http.Request) bool {
	if r.URL.RawQuery == "" {
		if r.URL.Path == "/api/v1/photos/authored/edit" || r.URL.Path == "/api/v1/photos/authored/prepare" {
			return r.Method == http.MethodPost
		}
		if after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/photos/authored/"); ok {
			parts := strings.Split(after, "/")
			return len(parts) == 2 && parts[1] == "undo" && query.ValidateTextOperand("asset", parts[0]) == nil && r.Method == http.MethodPost
		}
	}
	return false
}
