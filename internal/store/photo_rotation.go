package store

import (
	"context"
	"database/sql"
	"time"
	"uuid"

	"go.kenn.io/docbank/document"
)

// publishPhotoRotationTx keeps one rebuildable rotation proposal per version.
// Face measurements take precedence, including abstention when they disagree.
func publishPhotoRotationTx(ctx context.Context, tx *sql.Tx, version string, fallback *int, producer string, at time.Time) error {
	if eligible, err := photoDescriptionEligibleTx(ctx, tx, version); err != nil || !eligible {
		return err
	}
	faces, err := photoQualityFacesTx(ctx, tx, version)
	if err != nil {
		return err
	}
	var rolls []*float64
	for _, face := range faces {
		if face.Roll != nil {
			rolls = append(rolls, face.Roll)
		}
	}
	angle, source := fallback, "description"
	if len(rolls) > 0 {
		angle, source = document.PhotoRotationFromRolls(rolls), "faces"
	}
	var confirmed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id WHERE v.version_id=? AND (f.confirmed_fields & ?)<>0)`, version, PhotoConfirmedRotation).Scan(&confirmed); err != nil {
		return err
	}
	if confirmed {
		_, err = tx.ExecContext(ctx, `UPDATE photo_suggestions SET rotation=NULL,awaiting=0 WHERE content_version_id=? AND kind='rotation' AND NOT dismissed AND NOT caption_resolved`, version)
		return err
	}
	if angle == nil || *angle == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM photo_suggestions WHERE content_version_id=? AND kind='rotation' AND NOT dismissed AND NOT caption_resolved AND (? OR rotation_source='faces' OR awaiting AND ?)`, version, len(rolls) > 0, producer != "")
		return err
	}
	if *angle != 90 && *angle != 180 && *angle != 270 {
		return nil
	}
	// A constant producer identity gives face-only proposals the same ownership
	// and backup validation as descriptions, without claiming provider work.
	if producer == "" {
		producer = photoRotationProducer
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_suggestions(suggestion_id,content_version_id,producer_fingerprint,kind,rotation,rotation_source,created_at)
 SELECT ?,?,?,'rotation',?,?,? WHERE NOT EXISTS(SELECT 1 FROM photo_suggestions WHERE content_version_id=? AND kind='rotation')`, uuid.New().String(), version, producer, *angle, source, at.UTC().Format(timestampLayout), version)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE photo_suggestions SET rotation=?,rotation_source=?,awaiting=0 WHERE content_version_id=? AND kind='rotation' AND NOT dismissed AND NOT caption_resolved`, *angle, source, version)
	return err
}

const photoRotationProducer = "2d4971f7284c30b9ed5c650b7a7d995d30ec038c5e274ebc2e5ac4c2edabb3a1"
