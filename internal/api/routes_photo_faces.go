package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

type PhotoFace = store.PhotoFace

type CreatePhotoFaceRequest struct {
	Box      *document.FaceBox `json:"box,omitzero"`
	PersonID string            `json:"person_id,omitzero" format:"uuid"`
}

type PhotoFaceResult struct {
	Face        *PhotoFace `json:"face,omitzero"`
	AssertionID string     `json:"assertion_id,omitzero" format:"uuid"`
}

type PhotoFacePage struct {
	Faces []PhotoFace `json:"faces" maxItems:"1000"`
}

func registerPhotoFaceRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "listPhotoFaces", Method: http.MethodGet, Path: "/api/v1/photos/versions/{content_version_id}/faces", Summary: "Read face boxes on one photo version"}, func(ctx context.Context, in *struct {
		ContentVersionID string `path:"content_version_id" format:"uuid"`
	}) (*struct{ Body PhotoFacePage }, error) {
		faces, err := d.Store.PhotoFaces(ctx, in.ContentVersionID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body PhotoFacePage }{Body: PhotoFacePage{Faces: faces}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "createPhotoFace", Method: http.MethodPost, Path: "/api/v1/photos/versions/{content_version_id}/faces", Summary: "Record a face box or a person in a whole photo", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct {
		ContentVersionID string `path:"content_version_id" format:"uuid"`
		Body             CreatePhotoFaceRequest
	}) (*struct{ Body PhotoFaceResult }, error) {
		var result PhotoFaceResult
		err := g.mutate(func() error {
			if in.Body.Box == nil {
				if in.Body.PersonID == "" {
					return FromStoreError(store.ErrInvalidPhotoFace)
				}
				assertion, err := d.Store.MarkPersonInPhoto(ctx, in.ContentVersionID, in.Body.PersonID)
				if err != nil {
					return FromStoreError(err)
				}
				result.AssertionID = assertion.AssertionID
			} else {
				change, err := d.Store.CreatePhotoFace(ctx, in.ContentVersionID, *in.Body.Box, in.Body.PersonID)
				if err != nil {
					return FromStoreError(err)
				}
				result = PhotoFaceResult{Face: &change, AssertionID: change.AssertionID}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return &struct{ Body PhotoFaceResult }{Body: result}, nil
	})
}
