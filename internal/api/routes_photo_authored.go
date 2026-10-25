package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

type PhotoAuthoredPrepareRequest struct {
	PhotoSelectionRequest

	Hidden          bool `json:"hidden,omitempty"`
	SkipUnavailable bool `json:"skip_unavailable,omitempty"`
	Stack           bool `json:"stack,omitempty" doc:"Prepare up to 2,000 live files for a complete stack decision."`
}

type PhotoAuthoredEditRequest struct {
	Targets       []store.PhotoAuthoredEditTarget `json:"targets" minItems:"1" maxItems:"1000"`
	Patch         store.PhotoAuthoredPatch        `json:"patch"`
	RotationDelta *int                            `json:"rotation_delta,omitempty" enum:"90" doc:"Rotate each target clockwise by 90 degrees."`
	Keywords      []store.PhotoKeywordEdit        `json:"keywords,omitempty" maxItems:"100"`
}

type PhotoAuthoredPreparation struct {
	Suggestions []store.PhotoSuggestion     `json:"suggestions,omitempty"`
	Assets      []PhotoAsset                `json:"assets"`
	Targets     []store.PhotoPreparedTarget `json:"targets"`
}

type PhotoXMPResolveRequest struct {
	ConflictID string `json:"conflict_id" format:"uuid"`
	Choice     string `json:"choice" enum:"catalog,sidecar"`
}

type photoXMPResolveOutput struct {
	Status int
	Body   *store.PhotoAuthoredReceipt
}

// resolvePhotoXMPOperation returns a receipt whenever resolution imports metadata.
func resolvePhotoXMPOperation(api huma.API) huma.Operation {
	const defaultResponse = "default"
	errorSchema := api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[Error](), true, "")
	return huma.Operation{OperationID: "resolvePhotoXMP", Method: http.MethodPost, Path: "/api/v1/photos/authored/{file_id}/xmp", Summary: "Choose the catalog or sidecar copy of conflicting photo metadata", Responses: map[string]*huma.Response{
		"204":           {Description: "Resolution imported no catalog values"},
		defaultResponse: {Description: "Error", Content: map[string]*huma.MediaType{new(Error).ContentType("application/json"): {Schema: errorSchema}}},
	}}
}

// resolvePhotoXMP returns the import either choice created so every view
// showing the photo can apply it.
func resolvePhotoXMP(ctx context.Context, d Deps, g *gate, fileID string, request PhotoXMPResolveRequest) (*photoXMPResolveOutput, error) {
	out := &photoXMPResolveOutput{Status: http.StatusNoContent}
	err := g.mutate(func() error {
		var err error
		out.Body, err = processing.ResolvePhotoXMP(ctx, &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}, fileID, request.ConflictID, request.Choice)
		return FromStoreError(err)
	})
	if out.Body != nil {
		out.Status = http.StatusOK
	}
	return out, err
}

type PhotoStackDecisionRequest struct {
	BurstGapSeconds int                             `json:"burst_gap_seconds" minimum:"1" maximum:"30"`
	Hidden          bool                            `json:"hidden,omitempty"`
	AssetIDs        []string                        `json:"asset_ids" minItems:"2" maxItems:"500" uniqueItems:"true" format:"uuid"`
	Targets         []store.PhotoAuthoredEditTarget `json:"targets" minItems:"1" maxItems:"2000"`
	Decision        string                          `json:"decision" enum:"current,all,selected,representative"`
	CurrentID       string                          `json:"current_id" format:"uuid"`
	SelectedIDs     []string                        `json:"selected_ids,omitempty" maxItems:"500" uniqueItems:"true"`
}

func registerPhotoAuthoredRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, resolvePhotoXMPOperation(api), func(ctx context.Context, in *struct {
		FileID string `path:"file_id" format:"uuid"`
		Body   PhotoXMPResolveRequest
	}) (*photoXMPResolveOutput, error) {
		return resolvePhotoXMP(ctx, d, g, in.FileID, in.Body)
	})
	// Bound 500 observed frames and 2,000 file revisions.
	huma.Register(api, huma.Operation{OperationID: "decidePhotoStack", Method: http.MethodPost, Path: "/api/v1/photos/authored/stack", Summary: "Decide one observed stack atomically", MaxBodyBytes: 576 << 10}, func(ctx context.Context, in *struct{ Body PhotoStackDecisionRequest }) (*struct{ Body store.PhotoAuthoredReceipt }, error) {
		body := in.Body
		request := store.PhotoStackDecision{BurstGapSeconds: body.BurstGapSeconds, Hidden: body.Hidden, AssetIDs: body.AssetIDs, Targets: body.Targets, Decision: body.Decision, CurrentID: body.CurrentID, SelectedIDs: body.SelectedIDs, SourceMetadataFingerprint: processing.SourceMetadataExtractorFingerprint}
		var result store.PhotoAuthoredReceipt
		err := g.mutate(func() error {
			var callErr error
			result, callErr = d.Store.DecidePhotoStack(ctx, request)
			if errors.Is(callErr, store.ErrStalePhotoStack) {
				return NewError(http.StatusConflict, "photo_stack_changed", callErr.Error())
			}
			return workspaceQueryError(callErr)
		})
		return &struct{ Body store.PhotoAuthoredReceipt }{result}, err
	})
	huma.Register(api, huma.Operation{OperationID: "preparePhotoAuthored", Method: http.MethodPost, Path: "/api/v1/photos/authored/prepare", Summary: "Resolve a revision-bound metadata editing session"}, func(ctx context.Context, in *struct{ Body PhotoAuthoredPrepareRequest }) (*struct {
		Body PhotoAuthoredPreparation
	}, error) {
		selection := store.PhotoSetSelection{AssetIDs: in.Body.AssetIDs, Hidden: in.Body.Hidden, SkipUnavailable: in.Body.SkipUnavailable, Coverage: store.CoverageSelection{Configuration: in.Body.Coverage.Configuration, ProfileFingerprint: in.Body.Coverage.ProfileFingerprint}}
		if in.Body.Stack {
			selection.Limit = store.MaxPhotoStackTargets
		}
		if in.Body.Query != nil {
			value, err := parseWorkspaceQuery(*in.Body.Query)
			if err != nil {
				return nil, err
			}
			selection.Query = &value
		}
		result, err := d.Store.PreparePhotoAuthored(ctx, selection)
		body := PhotoAuthoredPreparation{Assets: []PhotoAsset{}, Targets: result.Targets, Suggestions: result.Suggestions}
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
		if after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/photos/suggestions/"); ok {
			parts := strings.Split(after, "/")
			return len(parts) == 2 && parts[1] == "response" && r.Method == http.MethodPost
		}
		if r.URL.Path == "/api/v1/photos/authored/stack" || r.URL.Path == "/api/v1/photos/authored/edit" || r.URL.Path == "/api/v1/photos/authored/prepare" {
			return r.Method == http.MethodPost
		}
		if after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/photos/authored/"); ok {
			parts := strings.Split(after, "/")
			if len(parts) == 2 && parts[1] == "xmp" && query.ValidateTextOperand("asset", parts[0]) == nil {
				return r.Method == http.MethodPost
			}
			return len(parts) == 2 && parts[1] == "undo" && query.ValidateTextOperand("asset", parts[0]) == nil && r.Method == http.MethodPost
		}
	}
	return false
}
