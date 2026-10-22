package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"

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
	return `WITH photos AS MATERIALIZED (
 SELECT a.asset_id,cv.version_id
 FROM photo_files member
 JOIN nodes source_node ON source_node.id=member.node_id
 JOIN content_versions cv ON cv.version_id=source_node.current_version_id
 JOIN photo_assets a ON a.asset_id=member.asset_id
 JOIN photo_files f ON f.file_id=a.display_file_id
 JOIN nodes n ON n.id=f.node_id
 WHERE ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(hidden) + `
 AND ` + currentPhotoSourcePredicate("member", "source_node", "cv.version_id") + `)
 SELECT assertion.person_id,COUNT(DISTINCT photos.asset_id)
 FROM photos
 CROSS JOIN person_document_assertions assertion ON assertion.content_version_id=photos.version_id
 JOIN json_each(?) requested ON requested.value=assertion.person_id
 WHERE assertion.role='depicted' AND assertion.action='assert'
 GROUP BY assertion.person_id`
}

// personPortraitSQL ranks boxed faces for the whole page. A readable key face
// wins across both scopes; fallback portraits prefer Library photos.
func personPortraitSQL(unlocked bool) string {
	where := personPhotoWhere(false)
	if unlocked {
		where = strings.Replace(where, photoVisibilityPredicate(false), "("+photoVisibilityPredicate(false)+" OR "+photoVisibilityPredicate(true)+")", 1)
	}
	return `WITH portraits AS (
 SELECT person.person_id,a.asset_id,cv.version_id,member.rotation,face.x,face.y,face.width,face.height,
 CASE WHEN g.state='ready' AND g.output_blob_hash IS NOT NULL THEN g.generation_id ELSE '' END generation_id,
 a.hidden_at IS NOT NULL hidden,
 ROW_NUMBER() OVER (PARTITION BY person.person_id ORDER BY face.face_id=person.key_face_id DESC,
 a.hidden_at IS NULL DESC,COALESCE(pl.sort_key,'') DESC,
 (g.state='ready' AND g.output_blob_hash IS NOT NULL) DESC,face.width*face.height DESC,a.asset_id,face.face_id) rank
 FROM photo_faces face
 CROSS JOIN person_document_assertions assertion ON assertion.assertion_id=face.assertion_id AND assertion.content_version_id=face.content_version_id
 JOIN persons person ON person.person_id=assertion.person_id
 JOIN json_each(?1) requested ON requested.value=person.person_id` + personPhotoJoins + `
 LEFT JOIN photo_timeline pl ON pl.file_id=f.file_id
 LEFT JOIN visual_preview_generations g ON g.content_version_id=cv.version_id AND g.recipe_fingerprint=?2` + where + `)
 SELECT person_id,asset_id,version_id,rotation,x,y,width,height,generation_id,hidden FROM portraits WHERE rank=1`
}

// personPhotoSummaries adds Library counts, Hidden counts while unlocked, and
// one readable portrait per person, preferring its visible key face.
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
	}
	rows, err := tx.QueryContext(ctx, personPortraitSQL(unlocked), string(requested), recipe)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	summaries := make(map[string]*PersonPhotoSummary, len(people))
	for i := range people {
		summaries[people[i].PersonID] = people[i].PhotoSummary
	}
	for rows.Next() {
		var id string
		var preview PersonFacePreview
		var box document.FaceBox
		var hidden bool
		if err := rows.Scan(&id, &preview.AssetID, &preview.ContentVersionID, &preview.Rotation, &box.X, &box.Y, &box.Width, &box.Height, &preview.GenerationID, &hidden); err != nil {
			return err
		}
		preview.Box = &box
		summaries[id].Face, summaries[id].FaceHidden = &preview, hidden
	}
	return rows.Err()
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
