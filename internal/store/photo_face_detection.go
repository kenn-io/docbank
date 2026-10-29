package store

import (
	"cmp"
	"context"
	"database/sql"
	"reflect"
	"slices"
	"time"
	"uuid"

	"go.kenn.io/docbank/document"
)

// RequireFaceDetectionSupported refuses face mutations while audit authority exists.
func (s *Store) RequireFaceDetectionSupported(ctx context.Context) error {
	return requireLogicalMutationSupported(ctx, s.db)
}

func faceScanSettledTx(ctx context.Context, tx *sql.Tx, version, detector string) (bool, error) {
	var settled bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_face_scans WHERE content_version_id=? AND detector_fingerprint=?)`, version, detector).Scan(&settled)
	return settled, err
}

// protectedDetectionFacesTx includes references retained for undo, even when a
// face no longer has an active assignment or candidate.
func protectedDetectionFacesTx(ctx context.Context, tx *sql.Tx, version string) (map[string]bool, error) {
	protected := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT face_id FROM person_candidate_faces WHERE content_version_id=?
 UNION SELECT json_extract(j.value,'$.face_id') FROM photo_change_receipts r INDEXED BY photo_change_receipts_face_version,json_each(r.before_json,'$.faces') j WHERE r.operation='face_decision' AND json_extract(r.before_json,'$.content_version_id')=?`, version, version)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		protected[id] = true
	}
	return protected, rows.Err()
}

func faceBoxOverlap(a, b document.FaceBox) float64 {
	width := max(0, min(a.X+a.Width, b.X+b.Width)-max(a.X, b.X))
	height := max(0, min(a.Y+a.Height, b.Y+b.Height)-max(a.Y, b.Y))
	intersection := width * height
	return intersection / (a.Width*a.Height + b.Width*b.Height - intersection)
}

// faceMatchScores scores each observation against a saved box by IoU, except
// that when no IoU reaches the match floor, the sole observation fully inside or
// around the box scores 1; multiple nested observations keep IoU scores.
func faceMatchScores(box document.FaceBox, observations []document.FaceObservation) []float64 {
	inside := func(o document.FaceObservation) bool {
		return o.Box.X >= box.X && o.Box.Y >= box.Y && o.Box.X+o.Box.Width <= box.X+box.Width && o.Box.Y+o.Box.Height <= box.Y+box.Height ||
			box.X >= o.Box.X && box.Y >= o.Box.Y && box.X+box.Width <= o.Box.X+o.Box.Width && box.Y+box.Height <= o.Box.Y+o.Box.Height
	}
	scores := make([]float64, len(observations))
	for j, o := range observations {
		scores[j] = faceBoxOverlap(box, o.Box)
	}
	if sole := slices.IndexFunc(observations, inside); sole >= 0 && !slices.ContainsFunc(observations[sole+1:], inside) && slices.Max(scores) < 0.14 {
		scores[sole] = 1
	}
	return scores
}

// publishFaceDetectionTx reconciles an authorized scan and commits its completion together.
func publishFaceDetectionTx(ctx context.Context, tx *sql.Tx, version, detector string, result document.FaceDetection, at time.Time) error {
	settled, err := faceScanSettledTx(ctx, tx, version, detector)
	if err != nil || settled {
		return err
	}
	if len(result.Faces) > document.MaxPhotoRecordsPerVersion {
		return ErrPhotoFaceLimit
	}
	for _, observation := range result.Faces {
		if observation.Validate() != nil {
			return ErrInvalidPhotoFace
		}
	}
	faces, err := photoFacesQuery(ctx, tx, version)
	if err != nil {
		return err
	}
	protected, err := protectedDetectionFacesTx(ctx, tx, version)
	if err != nil {
		return err
	}
	for _, face := range faces {
		if face.DetectorFingerprint == "" || face.AssertionID != "" {
			protected[face.FaceID] = true
		}
	}
	type pair struct {
		face, observation int
		overlap           float64
	}
	var pairs []pair
	for i, face := range faces {
		for j, overlap := range faceMatchScores(face.Box, result.Faces) {
			if overlap >= 0.14 {
				pairs = append(pairs, pair{i, j, overlap})
			}
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		if order := cmp.Compare(b.overlap, a.overlap); order != 0 {
			return order
		}
		if order := cmp.Compare(faces[a.face].FaceID, faces[b.face].FaceID); order != 0 {
			return order
		}
		return cmp.Compare(a.observation, b.observation)
	})
	matchedFaces := map[int]int{}
	matchedObservations := map[int]bool{}
	for _, p := range pairs {
		if _, ok := matchedFaces[p.face]; ok || matchedObservations[p.observation] {
			continue
		}
		matchedFaces[p.face] = p.observation
		matchedObservations[p.observation] = true
	}
	for i, face := range faces {
		if protected[face.FaceID] {
			continue
		}
		j, matched := matchedFaces[i]
		if !matched {
			if _, err := tx.ExecContext(ctx, `DELETE FROM photo_faces WHERE face_id=?`, face.FaceID); err != nil {
				return err
			}
			continue
		}
		o := result.Faces[j]
		stored := document.FaceObservation{Box: face.Box, Roll: face.Roll, Facing: face.Facing, EyesOpen: face.EyesOpen, Smile: face.Smile}
		if face.DetectorFingerprint == detector && reflect.DeepEqual(stored, o) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_faces SET x=?,y=?,width=?,height=?,roll=?,facing=?,eyes_open=?,smile=?,detector_fingerprint=?,revision=revision+1 WHERE face_id=?`, o.Box.X, o.Box.Y, o.Box.Width, o.Box.Height, o.Roll, o.Facing, o.EyesOpen, o.Smile, detector, face.FaceID); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_faces WHERE content_version_id=?`, version).Scan(&count); err != nil {
		return err
	}
	for j, o := range result.Faces {
		if matchedObservations[j] {
			continue
		}
		count++
		if count > document.MaxPhotoRecordsPerVersion {
			return ErrPhotoFaceLimit
		}
		face := PhotoFace{FaceID: uuid.New().String(), ContentVersionID: version, Box: o.Box, Roll: o.Roll, Facing: o.Facing, EyesOpen: o.EyesOpen, Smile: o.Smile, DetectorFingerprint: detector, Revision: 1, CreatedAt: at.UTC().Format(timestampLayout)}
		if err := insertPhotoFaceTx(ctx, tx, face); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_face_scans(content_version_id,detector_fingerprint) VALUES(?,?)`, version, detector)
	return err
}
