package store

import (
	"context"
	"fmt"

	"go.kenn.io/docbank/document"
)

type metadataPhotoFace struct {
	Type             string  `json:"type"`
	FaceID           string  `json:"face_id" db:"face_id"`
	ContentVersionID string  `json:"content_version_id" db:"content_version_id"`
	X                float64 `json:"x" db:"x"`
	Y                float64 `json:"y" db:"y"`
	Width            float64 `json:"width" db:"width"`
	Height           float64 `json:"height" db:"height"`
	Revision         int64   `json:"revision" db:"revision"`
	AssertionID      *string `json:"assertion_id" db:"assertion_id"`
	CreatedAt        string  `json:"created_at" db:"created_at"`
}

var photoFaceMetadata = newMetadataTable(metadataTable[metadataPhotoFace]{record: metadataPhotoFace{Type: "photo_face"}, table: "photo_faces", suffix: "ORDER BY face_id", validate: func(v metadataPhotoFace) error {
	if v.Type != "photo_face" {
		return ErrInvalidPhotoFace
	}
	face := PhotoFace{FaceID: v.FaceID, ContentVersionID: v.ContentVersionID, Box: document.FaceBox{X: v.X, Y: v.Y, Width: v.Width, Height: v.Height}, Revision: v.Revision, CreatedAt: v.CreatedAt}
	if v.AssertionID != nil {
		face.AssertionID = *v.AssertionID
	}
	return validatePhotoFace(face)
}, checkExport: true})

func validatePhotoFaceMetadataState(ctx context.Context, q metadataQuerier) error {
	if err := photoFaceMetadata.validateRows(ctx, q); err != nil {
		return fmt.Errorf("validating photo face metadata records: %w", err)
	}
	return validatePhotoFaceReferences(ctx, q)
}
