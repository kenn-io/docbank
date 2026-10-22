package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

type PhotoFace = store.PhotoFace

type CreatePhotoFaceRequest struct {
	FaceID     string            `json:"face_id,omitzero" format:"uuid"`
	Box        *document.FaceBox `json:"box,omitzero"`
	PersonID   string            `json:"person_id,omitzero" format:"uuid"`
	PersonName *string           `json:"person_name,omitzero" doc:"New person name for a whole-photo mark with a caller-chosen UUIDv4 person_id; reuse both fields for retries. Limited to 200 UTF-8 bytes"`
}

type PhotoPersonAssertion struct {
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

type PhotoFacePerson = store.PhotoFacePerson

type PhotoFacePage struct {
	People     []PhotoFacePerson      `json:"people" maxItems:"1000"`
	Faces      []PhotoFace            `json:"faces" maxItems:"1000"`
	Assertions []PhotoPersonAssertion `json:"assertions" maxItems:"1000"`
}

func registerPhotoFaceRoutes(api huma.API, d Deps, g *gate) {
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		panic(err)
	}
	huma.Register(api, huma.Operation{OperationID: "listPersonFacePreviews", Method: http.MethodGet, Path: "/api/v1/photos/people/{person_id}/face-previews", Summary: "Read up to three linked source-version photos in timeline order"}, func(ctx context.Context, in *struct {
		PersonID string `path:"person_id" format:"uuid"`
		Hidden   bool   `query:"hidden"`
	}) (*struct{ Body store.PersonFacePreviewPage }, error) {
		previews, err := d.Store.PersonFacePreviews(ctx, in.PersonID, fingerprints.GridRecipe, in.Hidden)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body store.PersonFacePreviewPage }{Body: previews}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "listPhotoFaces", Method: http.MethodGet, Path: "/api/v1/photos/versions/{content_version_id}/faces", Summary: "Read face boxes and people on one photo version"}, func(ctx context.Context, in *struct {
		ContentVersionID string `path:"content_version_id" format:"uuid"`
	}) (*struct{ Body PhotoFacePage }, error) {
		faces, assertions, people, err := d.Store.PhotoFacePersonNames(ctx, in.ContentVersionID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		page := PhotoFacePage{Faces: faces, People: people, Assertions: make([]PhotoPersonAssertion, 0, len(assertions))}
		for _, assertion := range assertions {
			page.Assertions = append(page.Assertions, photoAssertion(assertion))
		}
		return &struct{ Body PhotoFacePage }{Body: page}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "createPhotoFace", Method: http.MethodPost, Path: "/api/v1/photos/versions/{content_version_id}/faces", Summary: "Record a face box or a person in a whole photo", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct {
		ContentVersionID string `path:"content_version_id" format:"uuid"`
		Body             CreatePhotoFaceRequest
	}) (*struct{ Body PhotoFaceResult }, error) {
		var result PhotoFaceResult
		err := g.mutate(func() error {
			personName := ""
			if in.Body.PersonName != nil {
				personName = *in.Body.PersonName
				if in.Body.Box != nil || personName == "" {
					return FromStoreError(store.ErrInvalidPhotoFace)
				}
			}
			if in.Body.Box == nil {
				if in.Body.FaceID != "" {
					return FromStoreError(store.ErrInvalidPhotoFace)
				}
				assertion, err := d.Store.MarkPersonInPhoto(ctx, in.ContentVersionID, in.Body.PersonID, personName)
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
		// A browser withdraws a rejection to clear a merge conflict; rejecting
		// and choosing a key face stay with API keys and the CLI.
		if browserSessionRequest(ctx) && !slices.Contains([]string{"name", "retract", "dismiss", "withdraw_rejection"}, in.Body.Action) {
			return nil, NewError(http.StatusForbidden, "web_session_read_only", "Browser sessions can only name, clear, or remove faces, clear whole-photo marks, and withdraw rejections.")
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
