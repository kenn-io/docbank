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
	if v.AssetID == nil || validateUUIDv4(*v.AssetID) != nil || v.SettingsKey != nil || v.SetID != nil || v.AfterRevision != v.BeforeRevision+1 {
		return ErrInvalidPhotoFace
	}
	var before, after photoFaceReceipt
	if err := json.Unmarshal([]byte(v.BeforeJSON), &before, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(v.AfterJSON), &after, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	for _, state := range []photoFaceReceipt{before, after} {
		if state.Face != nil {
			if err := validatePhotoFace(*state.Face); err != nil {
				return err
			}
		}
		if state.Assertion != nil {
			if validateUUIDv4(state.Assertion.AssertionID) != nil {
				return ErrInvalidPhotoFace
			}
			if err := validateMetadataTime("face assertion recorded_at", state.Assertion.RecordedAt); err != nil {
				return err
			}
			if err := validatePersonAssertion(*state.Assertion); err != nil {
				return err
			}
			if state.Face == nil || state.Assertion.Role != "depicted" || state.Assertion.AssertionID != state.Face.AssertionID || state.Assertion.ContentVersionID != state.Face.ContentVersionID {
				return ErrInvalidPhotoFace
			}
		}
	}
	switch v.Operation {
	case "face_create":
		if before.Face != nil || after.Face == nil || after.Face.Revision != 1 || v.BeforeRevision != 0 || after.UndoOf != "" {
			return ErrInvalidPhotoFace
		}
	case "face_update":
		if before.Face == nil || after.Face == nil || before.Face.FaceID != after.Face.FaceID || before.Face.ContentVersionID != after.Face.ContentVersionID || before.Face.Revision != v.BeforeRevision || after.Face.Revision != v.AfterRevision || after.UndoOf != "" {
			return ErrInvalidPhotoFace
		}
	case "face_undo":
		if before.Face == nil || validateUUIDv4(after.UndoOf) != nil || before.Face.Revision != v.BeforeRevision || (after.Face != nil && (after.Face.FaceID != before.Face.FaceID || after.Face.Revision != v.AfterRevision)) {
			return ErrInvalidPhotoFace
		}
	default:
		return errors.New("invalid face receipt operation")
	}
	return validateMetadataTime("face receipt created_at", v.CreatedAt)
}
