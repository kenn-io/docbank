package store

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"time"
	"uuid"

	"go.kenn.io/docbank/document"
)

// PhotoFaceDetectionTargets lists visible display versions missing this scan.
// Queue state keeps concurrent starts and terminal failures out of the pass.
func (s *Store) PhotoFaceDetectionTargets(ctx context.Context, profile, detector, after string, limit int) ([]ContentVersion, error) {
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return nil, err
	}
	result := []ContentVersion{}
	query := `SELECT ` + contentVersionCols + ` FROM content_versions WHERE version_id IN (
 SELECT v.version_id FROM visual_preview_generations g
 CROSS JOIN content_versions v ON v.version_id=g.content_version_id AND v.blob_hash=g.source_sha256
 CROSS JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id
 CROSS JOIN photo_files f ON f.node_id=n.id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 WHERE g.content_version_id>?2 AND g.recipe_fingerprint=?1 AND ` + photoDescriptionEligibility + ` AND g.state='ready'
 AND NOT EXISTS(SELECT 1 FROM photo_face_scans s WHERE s.content_version_id=v.version_id AND s.detector_fingerprint=?5)
 AND NOT EXISTS(SELECT 1 FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id WHERE w.content_version_id=v.version_id AND w.profile_fingerprint=?3
 AND (w.state='waiting' AND NOT (j.state='failed' AND j.failure_code IN ('consent','stale_authority')) OR w.state='rejected' AND w.failure_code IN ('terminal','ambiguous')))
 ORDER BY g.content_version_id LIMIT ?4) ORDER BY version_id`
	err = queryRows(ctx, s.db, query, []any{fingerprints.GridRecipe, after, profile, limit, detector}, func(rows *sql.Rows) error {
		version, err := scanContentVersion(rows)
		if err == nil {
			result = append(result, version)
		}
		return err
	})
	return result, err
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
 UNION SELECT key_face_id FROM persons WHERE key_face_id IS NOT NULL
 UNION SELECT j.atom FROM photo_change_receipts r,json_tree(r.before_json) j WHERE r.operation='face_decision' AND json_extract(r.before_json,'$.content_version_id')=? AND j.type='text'
 UNION SELECT j.atom FROM photo_change_receipts r,json_tree(r.after_json) j WHERE r.operation='face_decision' AND json_extract(r.before_json,'$.content_version_id')=? AND j.type='text'
 UNION SELECT j.atom FROM person_merges r,json_tree(CAST(r.moved_json AS TEXT)) j WHERE j.type='text'
 UNION SELECT j.atom FROM person_splits r,json_tree(CAST(r.receipt_json AS TEXT)) j WHERE j.type='text'`, version, version, version)
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

// publishFaceDetectionTx reconciles a scan and commits its completion together.
func publishFaceDetectionTx(ctx context.Context, tx *sql.Tx, version, detector string, result document.FaceDetection, at time.Time) error {
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return err
	}
	if active {
		return ErrAuditMutationUnsupported
	}
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
		for j, observation := range result.Faces {
			if overlap := faceBoxOverlap(face.Box, observation.Box); overlap >= 0.14 {
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
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_face_scans(content_version_id,detector_fingerprint,completed_at) VALUES(?,?,?)`, version, detector, at.UTC().Format(timestampLayout))
	return err
}
