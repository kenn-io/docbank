package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/document"
)

// personPhotoJoins and personPhotoWhere link a person's depicted evidence to
// its live photos with the same current-source predicate as face previews,
// skipping the browse metadata joins that cards don't read.
const personPhotoJoins = ` CROSS JOIN content_versions cv ON cv.version_id=assertion.content_version_id
 CROSS JOIN nodes source_node ON source_node.id=cv.node_id
 CROSS JOIN photo_files member ON member.node_id=source_node.id
 CROSS JOIN photo_assets a ON a.asset_id=member.asset_id
 CROSS JOIN photo_files f ON f.file_id=a.display_file_id
 CROSS JOIN nodes n ON n.id=f.node_id`

func personPhotoWhere(hidden bool) string {
	return ` WHERE assertion.role='depicted' AND assertion.action='assert' AND n.current_version_id IS NOT NULL
 AND ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(hidden) + `
 AND ` + currentPhotoSourcePredicate("member", "source_node", "cv.version_id")
}

func personPhotoCountsSQL(hidden bool) string {
	return `SELECT assertion.person_id,COUNT(DISTINCT a.asset_id)
 FROM json_each(?) requested
 CROSS JOIN person_document_assertions assertion ON assertion.person_id=requested.value` + personPhotoJoins + personPhotoWhere(hidden) + `
 GROUP BY assertion.person_id`
}

// personPortraitSQL reads one boxed face: the key face, then the newest photo,
// preferring a ready grid preview and the largest box.
func personPortraitSQL(hidden bool) string {
	return `SELECT a.asset_id,cv.version_id,member.rotation,face.x,face.y,face.width,face.height,
 CASE WHEN g.state='ready' AND g.output_blob_hash IS NOT NULL THEN g.generation_id ELSE '' END
 FROM person_document_assertions assertion
 CROSS JOIN photo_faces face ON face.assertion_id=assertion.assertion_id AND face.content_version_id=assertion.content_version_id` + personPhotoJoins + `
 LEFT JOIN photo_timeline pl ON pl.file_id=f.file_id
 LEFT JOIN visual_preview_generations g ON g.content_version_id=cv.version_id AND g.recipe_fingerprint=?2` +
		personPhotoWhere(hidden) + ` AND assertion.person_id=?1
 ORDER BY face.face_id=(SELECT key_face_id FROM persons WHERE person_id=?1) DESC,COALESCE(pl.sort_key,'') DESC,
 (g.state='ready' AND g.output_blob_hash IS NOT NULL) DESC,face.width*face.height DESC,a.asset_id,face.face_id LIMIT 1`
}

// personPhotoSummaries adds Library counts, Hidden counts while unlocked, and
// one readable portrait per person: Library first, then unlocked Hidden.
func (s *Store) personPhotoSummaries(ctx context.Context, tx *sql.Tx, people []PersonSearchResult, recipe string, unlocked bool) error {
	ids := make([]string, len(people))
	for i := range people {
		ids[i] = people[i].PersonID
		people[i].PhotoSummary = &PersonPhotoSummary{}
		if unlocked {
			people[i].PhotoSummary.HiddenCount = new(int64(0))
		}
	}
	requested, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	counts := map[bool]map[string]int64{}
	for _, hidden := range []bool{false, true} {
		if hidden && !unlocked {
			continue
		}
		if counts[hidden], err = personPhotoCounts(ctx, tx, string(requested), hidden); err != nil {
			return err
		}
	}
	for i := range people {
		summary := people[i].PhotoSummary
		summary.LibraryCount = counts[false][people[i].PersonID]
		if unlocked {
			*summary.HiddenCount = counts[true][people[i].PersonID]
		}
		for _, hidden := range []bool{false, true} {
			if summary.Face != nil || counts[hidden][people[i].PersonID] == 0 {
				continue
			}
			// Cards show assigned faces; whole-photo evidence still counts.
			var preview PersonFacePreview
			var box document.FaceBox
			err := tx.QueryRowContext(ctx, personPortraitSQL(hidden), people[i].PersonID, recipe).Scan(
				&preview.AssetID, &preview.ContentVersionID, &preview.Rotation, &box.X, &box.Y, &box.Width, &box.Height, &preview.GenerationID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			preview.Box = &box
			summary.Face, summary.FaceHidden = &preview, hidden
		}
	}
	return nil
}

func personPhotoCounts(ctx context.Context, tx *sql.Tx, requested string, hidden bool) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, personPhotoCountsSQL(hidden), requested)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int64{}
	for rows.Next() {
		var id string
		var count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}
