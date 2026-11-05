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

const faceMatchThreshold = 0.14

// RequireFaceDetectionSupported refuses face mutations while audit authority exists.
func (s *Store) RequireFaceDetectionSupported(ctx context.Context) error {
	return requireLogicalMutationSupported(ctx, s.db)
}

func faceScanSettledTx(ctx context.Context, tx *sql.Tx, version, detector string) (bool, error) {
	var settled bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_face_scans WHERE content_version_id=$1 AND detector_fingerprint=$2)`, version, detector).Scan(&settled)
	return settled, err
}

// protectedDetectionFacesTx includes references retained for undo, even when a
// face no longer has an active assignment or candidate.
func (s *Store) protectedDetectionFacesTx(ctx context.Context, tx *sql.Tx, version string) (map[string]bool, error) {
	protected := map[string]bool{}
	d := metadataJSONDialect{postgres: s.postgresSchema != ""}
	receipts := "photo_change_receipts r INDEXED BY photo_change_receipts_face_version"
	faces := "json_each(r.before_json,'$.faces')"
	versionKey := d.text("r.before_json", "content_version_id")
	if d.postgres {
		receipts = "photo_change_receipts r"
		faces = "jsonb_array_elements(r.before_json::jsonb -> 'faces')"
		versionKey = d.indexedText("r.before_json", "content_version_id")
	}
	rows, err := tx.QueryContext(ctx, `SELECT face_id FROM person_candidate_faces WHERE content_version_id=$1
 UNION SELECT `+d.text("j.value", "face_id")+` FROM `+receipts+`,`+faces+` j WHERE r.operation='face_decision' AND `+versionKey+`=$1`, version)
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
	if sole := slices.IndexFunc(observations, inside); sole >= 0 && !slices.ContainsFunc(observations[sole+1:], inside) && slices.Max(scores) < faceMatchThreshold {
		scores[sole] = 1
	}
	return scores
}

// publishFaceDetectionTx reconciles an authorized scan and commits its completion together.
func (s *Store) publishFaceDetectionTx(ctx context.Context, tx *sql.Tx, version, detector string, result document.FaceDetection, at time.Time) error {
	settled, err := faceScanSettledTx(ctx, tx, version, detector)
	if err != nil || settled {
		return err
	}
	faces, err := photoFacesQuery(ctx, tx, version)
	if err != nil {
		return err
	}
	protected, err := s.protectedDetectionFacesTx(ctx, tx, version)
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
			if overlap >= faceMatchThreshold {
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
	changed := false
	for i, face := range faces {
		if protected[face.FaceID] {
			continue
		}
		j, matched := matchedFaces[i]
		if !matched {
			if _, err := tx.ExecContext(ctx, `DELETE FROM photo_faces WHERE face_id=$1`, face.FaceID); err != nil {
				return err
			}
			changed = true
			continue
		}
		o := result.Faces[j]
		stored := document.FaceObservation{Box: face.Box, Roll: face.Roll, Facing: face.Facing, EyesOpen: face.EyesOpen, Smile: face.Smile}
		observationChanged := !reflect.DeepEqual(stored, o)
		if face.DetectorFingerprint == detector && !observationChanged {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_faces SET x=$1,y=$2,width=$3,height=$4,roll=$5,facing=$6,eyes_open=$7,smile=$8,detector_fingerprint=$9,revision=revision+1 WHERE face_id=$10`, o.Box.X, o.Box.Y, o.Box.Width, o.Box.Height, o.Roll, o.Facing, nullableBoolInt(o.EyesOpen), nullableBoolInt(o.Smile), detector, face.FaceID); err != nil {
			return err
		}
		changed = changed || observationChanged
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_faces WHERE content_version_id=$1`, version).Scan(&count); err != nil {
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
		changed = true
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_face_scans(content_version_id,detector_fingerprint) VALUES($1,$2)`, version, detector)
	if err != nil || !changed {
		return err
	}
	return recordProcessedTx(ctx, tx, version, "")
}
