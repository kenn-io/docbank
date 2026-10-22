package store

import (
	"context"
	"database/sql"

	"go.kenn.io/docbank/document"
)

// personPhotoSummary uses the same snapshot and source predicates as people paging.
func (s *Store) personPhotoSummary(ctx context.Context, tx *sql.Tx, id, recipe string, unlocked bool) (PersonPhotoSummary, error) {
	out := PersonPhotoSummary{}
	for _, hidden := range []bool{false, true} {
		if hidden && !unlocked {
			continue
		}
		var count int64
		err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT a.asset_id)
 FROM person_document_assertions assertion
 CROSS JOIN content_versions cv ON cv.version_id=assertion.content_version_id
 CROSS JOIN nodes source_node ON source_node.id=cv.node_id
 CROSS JOIN photo_files member ON member.node_id=source_node.id
 CROSS JOIN `+photoBrowseDisplayFrom+`
 WHERE assertion.person_id=? AND assertion.role='depicted' AND assertion.action='assert'
 AND a.asset_id=member.asset_id AND `+photoBrowseLiveDisplay+` AND `+photoVisibilityPredicate(hidden)+`
 AND `+currentPhotoSourcePredicate("member", "source_node", "cv.version_id"), id).Scan(&count)
		if err != nil {
			return out, err
		}
		if hidden {
			out.HiddenCount = new(count)
		} else {
			out.LibraryCount = count
		}
		if count == 0 {
			continue
		}
		rows, err := tx.QueryContext(ctx, personFacePreviewsSQL(hidden, true), id, recipe)
		if err != nil {
			return out, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var preview PersonFacePreview
			var key bool
			var x, y, width, height sql.NullFloat64
			if err = rows.Scan(&preview.AssetID, &preview.ContentVersionID, &preview.Rotation, &x, &y, &width, &height, &preview.GenerationID, &key); err != nil {
				break
			}
			// Cards use assigned faces; whole-photo evidence still contributes to counts.
			if width.Valid && (out.Face == nil || key) {
				preview.Box = &document.FaceBox{X: x.Float64, Y: y.Float64, Width: width.Float64, Height: height.Float64}
				out.Face = &preview
				out.FaceHidden = hidden
			}
		}
		readErr := rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return out, err
		}
		if readErr != nil {
			return out, readErr
		}
		if closeErr != nil {
			return out, closeErr
		}
	}
	return out, nil
}
