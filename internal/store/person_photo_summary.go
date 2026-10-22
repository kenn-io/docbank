package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/document"
)

// personEvidence starts from the requested people's indexed depicted
// assertions, so the cost follows their evidence rather than the size of the
// vault. It binds the requested person IDs as JSON.
const personEvidence = ` FROM json_each(?1) requested
 CROSS JOIN persons person ON person.person_id=requested.value
 CROSS JOIN person_document_assertions assertion ON assertion.person_id=person.person_id AND assertion.action='assert' AND assertion.role='depicted'`

// personPhotoSource reaches the live photo of version cv with the same
// current-source predicate as face previews. Library photos qualify, and
// Hidden photos too while unlocked.
func personPhotoSource(unlocked bool) (joins, where string) {
	visible := photoVisibilityPredicate(false)
	if unlocked {
		visible = "1"
	}
	return ` CROSS JOIN nodes source_node ON source_node.id=cv.node_id
 CROSS JOIN photo_files member ON member.node_id=source_node.id
 CROSS JOIN photo_assets a ON a.asset_id=member.asset_id
 CROSS JOIN photo_files f ON f.file_id=a.display_file_id
 CROSS JOIN nodes n ON n.id=f.node_id`, ` WHERE n.current_version_id IS NOT NULL AND ` + photoBrowseLiveDisplay + ` AND ` + visible + `
 AND ` + currentPhotoSourcePredicate("member", "source_node", "cv.version_id")
}

// personPhotoCountsSQL checks each evidence version once, even when many
// requested people share it, and counts distinct assets, so a RAW+JPEG pair
// or a person named on both files counts once.
func personPhotoCountsSQL(unlocked bool) string {
	joins, where := personPhotoSource(unlocked)
	return `WITH evidence AS MATERIALIZED (SELECT assertion.person_id,assertion.content_version_id version_id` + personEvidence + `),
 photos AS MATERIALIZED (SELECT cv.version_id,a.asset_id,a.hidden_at IS NOT NULL hidden
 FROM (SELECT DISTINCT version_id FROM evidence) requested_version
 CROSS JOIN content_versions cv ON cv.version_id=requested_version.version_id` + joins + where + `)
 SELECT evidence.person_id,COUNT(DISTINCT CASE WHEN NOT photos.hidden THEN photos.asset_id END),
 COUNT(DISTINCT CASE WHEN photos.hidden THEN photos.asset_id END)
 FROM evidence JOIN photos ON photos.version_id=evidence.version_id GROUP BY evidence.person_id`
}

// personPortraitSQL ranks boxed faces for the whole page. A readable key face
// wins across both scopes; fallback portraits prefer Library photos.
func personPortraitSQL(unlocked bool) string {
	joins, where := personPhotoSource(unlocked)
	return `WITH portraits AS (
 SELECT person.person_id,a.asset_id,cv.version_id,member.rotation,face.x,face.y,face.width,face.height,
 CASE WHEN g.state='ready' AND g.output_blob_hash IS NOT NULL THEN g.generation_id ELSE '' END generation_id,
 a.hidden_at IS NOT NULL hidden,
 ROW_NUMBER() OVER (PARTITION BY person.person_id ORDER BY face.face_id=person.key_face_id DESC,
 a.hidden_at IS NULL DESC,COALESCE(pl.sort_key,'') DESC,
 (g.state='ready' AND g.output_blob_hash IS NOT NULL) DESC,face.width*face.height DESC,a.asset_id,face.face_id) rank` + personEvidence + `
 CROSS JOIN photo_faces face ON face.assertion_id=assertion.assertion_id AND face.content_version_id=assertion.content_version_id
 CROSS JOIN content_versions cv ON cv.version_id=face.content_version_id` + joins + `
 LEFT JOIN photo_timeline pl ON pl.file_id=f.file_id
 LEFT JOIN visual_preview_generations g ON g.content_version_id=cv.version_id AND g.recipe_fingerprint=?2` + where + `)
 SELECT person_id,asset_id,version_id,rotation,x,y,width,height,generation_id,hidden FROM portraits WHERE rank=1`
}

// personPhotoSummaries adds Library counts, Hidden counts while unlocked, and
// one readable portrait per person, preferring its visible key face.
func (s *Store) personPhotoSummaries(ctx context.Context, tx *sql.Tx, people []PersonSearchResult, recipe string, unlocked bool) error {
	ids := make([]string, len(people))
	summaries := make(map[string]*PersonPhotoSummary, len(people))
	for i := range people {
		ids[i] = people[i].PersonID
		people[i].PhotoSummary = &PersonPhotoSummary{}
		if unlocked {
			people[i].PhotoSummary.HiddenCount = new(int64(0))
		}
		summaries[ids[i]] = people[i].PhotoSummary
	}
	requested, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	counts, err := tx.QueryContext(ctx, personPhotoCountsSQL(unlocked), string(requested))
	if err != nil {
		return err
	}
	defer func() { _ = counts.Close() }()
	for counts.Next() {
		var id string
		var library, hidden int64
		if err := counts.Scan(&id, &library, &hidden); err != nil {
			return err
		}
		summaries[id].LibraryCount = library
		if unlocked {
			*summaries[id].HiddenCount = hidden
		}
	}
	if err := errors.Join(counts.Err(), counts.Close()); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, personPortraitSQL(unlocked), string(requested), recipe)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
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
