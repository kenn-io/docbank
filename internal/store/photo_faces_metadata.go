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
	NamedAt          *string `json:"named_at,omitempty" db:"named_at"`
}

var photoFaceMetadata = newMetadataTable(metadataTable[metadataPhotoFace]{record: metadataPhotoFace{Type: "photo_face"}, table: "photo_faces", suffix: "ORDER BY face_id", validate: func(v metadataPhotoFace) error {
	if v.Type != "photo_face" {
		return ErrInvalidPhotoFace
	}
	face := PhotoFace{FaceID: v.FaceID, ContentVersionID: v.ContentVersionID, Box: document.FaceBox{X: v.X, Y: v.Y, Width: v.Width, Height: v.Height}, Revision: v.Revision, CreatedAt: v.CreatedAt}
	if v.AssertionID != nil {
		face.AssertionID = *v.AssertionID
	}
	if v.NamedAt != nil {
		if v.AssertionID == nil {
			return ErrInvalidPhotoFace
		}
		if err := validateMetadataTime("photo face named_at", *v.NamedAt); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidPhotoFace, err)
		}
	}
	return validatePhotoFace(face)
}, checkExport: true})

func validatePhotoFaceMetadataState(ctx context.Context, q metadataQuerier) error {
	if err := photoFaceMetadata.validateRows(ctx, q); err != nil {
		return fmt.Errorf("validating photo face metadata records: %w", err)
	}
	return validatePhotoFaceReferences(ctx, q)
}

func validatePhotoFaceDecisionReferences(ctx context.Context, q metadataQuerier) error {
	if err := requirePhotoFaceRejectionBound(ctx, q, "", "", ""); err != nil {
		return fmt.Errorf("%w: validating face rejection bounds: %w", ErrInvalidPhotoFace, err)
	}
	return validatePersonKeyFaces(ctx, q, "", ErrInvalidPhotoFace)
}

func validatePersonKeyFaces(ctx context.Context, q metadataQuerier, personID string, invalidError error) error {
	var invalid bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons p LEFT JOIN photo_faces f ON f.face_id=p.key_face_id LEFT JOIN person_document_assertions a ON a.assertion_id=f.assertion_id WHERE (?='' OR p.person_id=?) AND p.key_face_id IS NOT NULL AND (a.person_id IS NULL OR a.person_id<>p.person_id OR a.action<>'assert'))`, personID, personID).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return invalidError
	}
	return nil
}
