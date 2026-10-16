package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type PhotoFace = store.PhotoFace

type CreatePhotoFaceRequest struct {
	FaceID   string            `json:"face_id,omitzero" format:"uuid"`
	Box      *document.FaceBox `json:"box,omitzero"`
	PersonID string            `json:"person_id,omitzero" format:"uuid"`
}

type PhotoPersonAssertion struct {
	DisplayName      string `json:"display_name"`
	PhotoMark        bool   `json:"photo_mark,omitzero"`
	AssertionID      string `json:"assertion_id" format:"uuid"`
	ContentVersionID string `json:"content_version_id" format:"uuid"`
	PersonID         string `json:"person_id" format:"uuid"`
	Role             string `json:"role"`
	Action           string `json:"action"`
	Note             string `json:"note"`
	RecordedAt       string `json:"recorded_at"`
	Revision         int64  `json:"revision"`
}

func photoAssertion(assertion store.PersonDocumentAssertion) PhotoPersonAssertion {
	return PhotoPersonAssertion{PhotoMark: assertion.PhotoMark, AssertionID: assertion.AssertionID, ContentVersionID: assertion.ContentVersionID, PersonID: assertion.PersonID, Role: assertion.Role, Action: assertion.Action, Note: assertion.Note, RecordedAt: assertion.RecordedAt, Revision: assertion.Revision}
}

type PhotoFaceResult struct {
	Face      *PhotoFace            `json:"face,omitzero"`
	Assertion *PhotoPersonAssertion `json:"assertion,omitzero"`
}

type PhotoFacePage struct {
	Faces      []PhotoFace            `json:"faces" maxItems:"1000"`
	Assertions []PhotoPersonAssertion `json:"assertions" maxItems:"1000"`
}

func registerPhotoFaceRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "readPhotoFaceCrop", Method: http.MethodGet,
		Path:      "/api/v1/photos/assets/{asset_id}/face-crop",
		Summary:   "Read a face closeup from the displayed fit preview",
		Responses: map[string]*huma.Response{"200": {Description: "Verified JPEG face crop or whole fit preview", Content: map[string]*huma.MediaType{"image/jpeg": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}}},
	}, func(ctx context.Context, in *struct {
		AssetID          string `path:"asset_id"`
		ContentVersionID string `query:"content_version_id" format:"uuid" required:"true"`
		GenerationID     string `query:"generation_id" required:"true"`
		FaceID           string `query:"face_id" format:"uuid"`
		AssertionID      string `query:"assertion_id" format:"uuid"`
	}) (*huma.StreamResponse, error) {
		recipe, err := processing.VisualPreviewRecipeForSize("fit")
		if err != nil {
			return nil, err
		}
		_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
		if err != nil {
			return nil, err
		}
		view, box, err := d.Store.PhotoFaceCrop(ctx, in.AssetID, in.ContentVersionID, in.GenerationID, fingerprint, in.FaceID, in.AssertionID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		body, err := verifiedPhotoPreviewBytes(ctx, d, view.Generation.Preview.Output)
		if err != nil {
			return nil, err
		}
		if box != nil {
			body, err = processing.CropPhotoFace(body, *box)
			if err != nil {
				return nil, NewError(http.StatusInternalServerError, "photo_preview_corrupt", "The face preview could not be decoded.")
			}
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "image/jpeg")
			hctx.SetHeader("Content-Length", strconv.Itoa(len(body)))
			hctx.SetHeader("Cache-Control", "no-store")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			_, _ = hctx.BodyWriter().Write(body)
		}}, nil
	})

	huma.Register(api, huma.Operation{OperationID: "listPhotoFaces", Method: http.MethodGet, Path: "/api/v1/photos/versions/{content_version_id}/faces", Summary: "Read face boxes and people on one photo version"}, func(ctx context.Context, in *struct {
		ContentVersionID string `path:"content_version_id" format:"uuid"`
	}) (*struct{ Body PhotoFacePage }, error) {
		faces, assertions, err := d.Store.PhotoFaces(ctx, in.ContentVersionID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		names, err := d.Store.PhotoFacePersonNames(ctx, assertions)
		if err != nil {
			return nil, FromStoreError(err)
		}
		page := PhotoFacePage{Faces: faces, Assertions: make([]PhotoPersonAssertion, 0, len(assertions))}
		for _, assertion := range assertions {
			row := photoAssertion(assertion)
			row.DisplayName = names[assertion.PersonID]
			page.Assertions = append(page.Assertions, row)
		}
		return &struct{ Body PhotoFacePage }{Body: page}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "createPhotoFace", Method: http.MethodPost, Path: "/api/v1/photos/versions/{content_version_id}/faces", Summary: "Record a face box or a person in a whole photo", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct {
		ContentVersionID string `path:"content_version_id" format:"uuid"`
		Body             CreatePhotoFaceRequest
	}) (*struct{ Body PhotoFaceResult }, error) {
		var result PhotoFaceResult
		err := g.mutate(func() error {
			if in.Body.Box == nil {
				if in.Body.PersonID == "" || in.Body.FaceID != "" {
					return FromStoreError(store.ErrInvalidPhotoFace)
				}
				assertion, err := d.Store.MarkPersonInPhoto(ctx, in.ContentVersionID, in.Body.PersonID)
				if err != nil {
					return FromStoreError(err)
				}
				saved := photoAssertion(assertion)
				result.Assertion = &saved
			} else {
				face, assertion, err := d.Store.CreatePhotoFace(ctx, in.ContentVersionID, in.Body.FaceID, *in.Body.Box, in.Body.PersonID)
				if err != nil {
					return FromStoreError(err)
				}
				result.Face = &face
				if assertion != nil {
					saved := photoAssertion(*assertion)
					result.Assertion = &saved
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if result.Assertion != nil {
			names, err := d.Store.PhotoFacePersonNames(ctx, []store.PersonDocumentAssertion{{PersonID: result.Assertion.PersonID}})
			if err != nil {
				return nil, FromStoreError(err)
			}
			result.Assertion.DisplayName = names[result.Assertion.PersonID]
		}
		return &struct{ Body PhotoFaceResult }{Body: result}, nil
	})
}

type PhotoFaceDecisionRequest store.PhotoFaceDecisionRequest

type PhotoFaceDecisionResult struct {
	ReceiptID string `json:"receipt_id" format:"uuid"`
}

func registerPhotoFaceDecisionRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "decidePhotoFace", Method: http.MethodPost, Path: "/api/v1/photos/versions/{content_version_id}/face-decisions", Summary: "Name, retract, reject, dismiss, or choose a key face"}, func(ctx context.Context, in *struct {
		ContentVersionID string `path:"content_version_id" format:"uuid"`
		Body             PhotoFaceDecisionRequest
	}) (*struct{ Body PhotoFaceDecisionResult }, error) {
		if browserSessionRequest(ctx) && in.Body.Action != "name" {
			return nil, NewError(http.StatusForbidden, "web_session_read_only", "Browser sessions can only name faces.")
		}
		var receipt store.PhotoFaceDecisionReceipt
		err := g.mutate(func() error {
			var err error
			receipt, err = d.Store.DecidePhotoFace(ctx, in.ContentVersionID, store.PhotoFaceDecisionRequest(in.Body))
			if err != nil {
				return FromStoreError(err)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return &struct{ Body PhotoFaceDecisionResult }{Body: PhotoFaceDecisionResult{ReceiptID: receipt.ReceiptID}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "undoPhotoFaceDecision", Method: http.MethodPost, Path: "/api/v1/photos/face-decisions/{receipt_id}/undo", Summary: "Undo a face decision if its records are unchanged"}, func(ctx context.Context, in *struct {
		ReceiptID   string `path:"receipt_id" format:"uuid"`
		OperationID string `query:"operation_id" format:"uuid"`
	}) (*struct{ Body PhotoFaceDecisionResult }, error) {
		var receipt store.PhotoFaceDecisionReceipt
		err := g.mutate(func() error {
			var err error
			receipt, err = d.Store.UndoPhotoFaceDecisionWithOperationID(ctx, in.ReceiptID, in.OperationID)
			if err != nil {
				return FromStoreError(err)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return &struct{ Body PhotoFaceDecisionResult }{Body: PhotoFaceDecisionResult{ReceiptID: receipt.ReceiptID}}, nil
	})
}
