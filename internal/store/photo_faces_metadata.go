package store

import (
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/document"
)

type metadataPhotoFace struct {
	Type                string  `json:"type"`
	FaceID              string  `json:"face_id" db:"face_id"`
	ContentVersionID    string  `json:"content_version_id" db:"content_version_id"`
	DetectorFingerprint string  `json:"detector_fingerprint" db:"detector_fingerprint"`
	X                   float64 `json:"x" db:"x"`
	Y                   float64 `json:"y" db:"y"`
	Width               float64 `json:"width" db:"width"`
	Height              float64 `json:"height" db:"height"`
	Revision            int64   `json:"revision" db:"revision"`
	AssertionID         *string `json:"assertion_id" db:"assertion_id"`
	CreatedAt           string  `json:"created_at" db:"created_at"`
}

var photoFaceMetadata = newMetadataTable(metadataTable[metadataPhotoFace]{record: metadataPhotoFace{Type: "photo_face"}, table: "photo_faces", suffix: "ORDER BY face_id", validate: func(v metadataPhotoFace) error {
	if v.Type != "photo_face" {
		return ErrInvalidPhotoFace
	}
	face := PhotoFace{FaceID: v.FaceID, ContentVersionID: v.ContentVersionID, DetectorFingerprint: v.DetectorFingerprint, Box: document.FaceBox{X: v.X, Y: v.Y, Width: v.Width, Height: v.Height}, Revision: v.Revision, CreatedAt: v.CreatedAt}
	if v.AssertionID != nil {
		face.AssertionID = *v.AssertionID
	}
	return validatePhotoFace(face)
}, checkExport: true})

func validatePhotoFaceReceipt(v metadataPhotoReceipt) error {
	if v.AssetID == nil || validateUUIDv4(*v.AssetID) != nil || v.SettingsKey != nil || v.SetID != nil || v.BeforeRevision != 0 || v.AfterRevision != 1 {
		return ErrInvalidPhotoFace
	}
	var before, after photoFaceReceipt
	if err := json.Unmarshal([]byte(v.BeforeJSON), &before, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(v.AfterJSON), &after, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	if before.Face != nil || before.Assertion != nil {
		return ErrInvalidPhotoFace
	}
	if after.Face != nil {
		if err := validatePhotoFace(*after.Face); err != nil {
			return err
		}
		if after.Face.Revision != 1 {
			return ErrInvalidPhotoFace
		}
	}
	if after.Assertion != nil {
		assertion := *after.Assertion
		if validateUUIDv4(assertion.AssertionID) != nil || assertion.Role != "depicted" || assertion.Action != "assert" || assertion.Revision != 1 {
			return ErrInvalidPhotoFace
		}
		if err := validatePersonAssertion(assertion); err != nil {
			return err
		}
		if err := validateMetadataTime("face assertion recorded_at", assertion.RecordedAt); err != nil {
			return err
		}
		if after.Face != nil && (assertion.AssertionID != after.Face.AssertionID || assertion.ContentVersionID != after.Face.ContentVersionID) {
			return ErrInvalidPhotoFace
		}
	}
	switch v.Operation {
	case "face_create":
		if after.Face == nil {
			return ErrInvalidPhotoFace
		}
	case "face_mark":
		if after.Face != nil || after.Assertion == nil {
			return ErrInvalidPhotoFace
		}
	default:
		return errors.New("invalid face receipt operation")
	}
	return validateMetadataTime("face receipt created_at", v.CreatedAt)
}
