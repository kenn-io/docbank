package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/kit/embedclient"
)

type FaceSimilarityRequest struct {
	FaceID                    string                     `json:"face_id" format:"uuid"`
	AllowLocalFaceRecognition bool                       `json:"allow_local_face_recognition"`
	References                []processing.FaceReference `json:"references" minItems:"1" maxItems:"16"`
}

func registerFaceSimilarityRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "comparePhotoFace", Method: http.MethodPost,
		Path:        "/api/v1/photos/versions/{content_version_id}/face-similarity",
		Summary:     "Compare one face with reviewed reference faces without assigning an identity",
		Middlewares: huma.Middlewares{noStoreMediaReceipt}, BodyReadTimeout: requestTimeout,
	}, func(ctx context.Context, in *struct {
		ContentVersionID string `path:"content_version_id" format:"uuid"`
		Body             FaceSimilarityRequest
	}) (*struct {
		Body processing.FaceSimilarityResult
	}, error) {
		if !in.Body.AllowLocalFaceRecognition {
			return nil, NewError(http.StatusUnprocessableEntity, "local_face_consent_required", "Set allow_local_face_recognition to compare these faces on this computer.")
		}
		ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
		defer cancel()
		load := d.FaceClient
		if load == nil {
			load = processing.LocalFaceClient
		}
		result, err := processing.CompareFaces(ctx, d.Store, d.Blobs, load,
			processing.FaceReference{ContentVersionID: in.ContentVersionID, FaceID: in.Body.FaceID}, in.Body.References)
		if err != nil {
			if errors.Is(err, processing.ErrSourceIneligible) {
				return nil, NewError(http.StatusConflict, "face_preview_unavailable", "Prepare a grid preview for a current, included Library photo before comparing faces.")
			}
			modelMissing := errors.Is(err, processing.ErrFaceModelUnavailable)
			if remote, ok := errors.AsType[*embedclient.APIError](err); ok && remote.StatusCode == http.StatusFailedDependency {
				modelMissing = true
			}
			if modelMissing {
				return nil, NewError(http.StatusServiceUnavailable, "face_model_unavailable", "Install Kit's face model and start a face-capable local server before comparing faces. This request does not install or restart it.")
			}
			return nil, FromStoreError(err)
		}
		return &struct {
			Body processing.FaceSimilarityResult
		}{Body: result}, nil
	})
}
