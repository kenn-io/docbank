package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/docbank/document"
)

// photoRotationTargetTx uses the content version as the transient proposal ID.
// Authored revision checks still bind a response to the displayed file.
func photoRotationTargetTx(ctx context.Context, tx *sql.Tx, version string) (photoSuggestionRow, int, bool, error) {
	var row photoSuggestionRow
	var angle int
	var confirmed bool
	err := tx.QueryRowContext(ctx, `SELECT v.version_id,f.file_id,f.revision,n.revision,a.asset_id,a.revision,a.hidden_at IS NOT NULL,f.rotation,(f.confirmed_fields & ?)<>0
 FROM content_versions v JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
 JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 WHERE v.version_id=?`, PhotoConfirmedRotation, version).Scan(&row.ContentVersionID, &row.Target, &row.SourceRevision, &row.NodeRevision, &row.AssetID, &row.AssetRevision, &row.hidden, &angle, &confirmed)
	row.ID = row.ContentVersionID
	row.Value.Tags = []string{}
	return row, angle, confirmed, err
}

// photoRotationSuggestionTx derives the correction from current face decisions.
// Any active face blocks provider fallback; missing rolls do not vote.
func (s *Store) photoRotationSuggestionTx(ctx context.Context, tx *sql.Tx, assetID string) (PhotoSuggestion, error) {
	var version string
	if err := tx.QueryRowContext(ctx, `SELECT n.current_version_id FROM photo_assets a JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id WHERE a.asset_id=?`, assetID).Scan(&version); err != nil {
		return PhotoSuggestion{}, err
	}
	row, _, confirmed, err := photoRotationTargetTx(ctx, tx, version)
	if err != nil || confirmed {
		return PhotoSuggestion{}, err
	}
	faces, err := s.photoQualityFacesTx(ctx, tx, version)
	if err != nil {
		return PhotoSuggestion{}, err
	}
	if len(faces) > 0 {
		rolls := make([]*float64, 0, len(faces))
		for _, face := range faces {
			rolls = append(rolls, face.Roll)
		}
		row.Rotation = document.PhotoRotationFromRolls(rolls)
		row.RotationSource = "faces"
	} else {
		err = tx.QueryRowContext(ctx, `SELECT rotation FROM photo_suggestions WHERE content_version_id=? AND NOT awaiting AND rotation IN (90,180,270) ORDER BY created_at DESC,id DESC LIMIT 1`, version).Scan(&row.Rotation)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return PhotoSuggestion{}, err
		}
		row.RotationSource = "description"
	}
	return row.PhotoSuggestion, nil
}

// respondPhotoRotationTx authors the angle the owner saw, even if new face or
// provider measurements have arrived since the inspector loaded it. A current
// nonzero proposal must still exist.
func (s *Store) respondPhotoRotationTx(ctx context.Context, tx *sql.Tx, version string, request PhotoSuggestionResponse) (PhotoSuggestion, error) {
	if request.Caption || len(request.Tags) > 0 || request.Rotation == nil || (*request.Rotation != 90 && *request.Rotation != 180 && *request.Rotation != 270) {
		return PhotoSuggestion{}, ErrInvalidPhotoSuggestionResponse
	}
	row, current, confirmed, err := photoRotationTargetTx(ctx, tx, version)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoSuggestion{}, ErrPhotoSuggestionConflict
	}
	if err != nil {
		return PhotoSuggestion{}, err
	}
	if confirmed {
		return PhotoSuggestion{}, ErrPhotoSuggestionConflict
	}
	live, err := s.photoRotationSuggestionTx(ctx, tx, row.AssetID)
	if err != nil {
		return PhotoSuggestion{}, err
	}
	if live.Rotation == nil {
		return PhotoSuggestion{}, ErrPhotoSuggestionConflict
	}
	if row.hidden {
		if _, err := s.hiddenSession(ctx, tx); err != nil {
			return PhotoSuggestion{}, err
		}
	}
	if request.Response == photoSuggestionDismissed {
		request.Rotation = new(current)
	}
	receipt, err := s.acceptPhotoSuggestionTx(ctx, tx, &row, request)
	if err != nil {
		return PhotoSuggestion{}, err
	}
	row, _, _, err = photoRotationTargetTx(ctx, tx, version)
	row.AuthoredReceipt = receipt
	return row.PhotoSuggestion, err
}
