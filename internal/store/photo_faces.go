package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

var (
	ErrPhotoFaceRejectionLimit = errors.New("face already has 1,000 rejected people")
	ErrInvalidPhotoFace        = errors.New("invalid photo face")
	ErrPhotoFaceLimit          = errors.New("photo version already has 1,000 faces or 1,000 depicted people")
)

type photoFaceExistsError struct{}

func (photoFaceExistsError) Error() string {
	return "face ID is already in use with different face details"
}
func (photoFaceExistsError) Is(target error) bool { return target == ErrExists }

type PhotoFace struct {
	FaceID            string           `json:"face_id"`
	ContentVersionID  string           `json:"content_version_id"`
	Box               document.FaceBox `json:"box"`
	Revision          int64            `json:"revision"`
	AssertionID       string           `json:"assertion_id,omitzero"`
	CreatedAt         string           `json:"created_at"`
	RejectedPersonIDs []string         `json:"rejected_person_ids,omitzero" maxItems:"1000"`
	Dismissed         bool             `json:"dismissed"`
}

func validatePhotoFace(face PhotoFace) error {
	if validateUUIDv4(face.FaceID) != nil || validateUUIDv4(face.ContentVersionID) != nil || face.Revision < 1 ||
		(face.AssertionID != "" && validateUUIDv4(face.AssertionID) != nil) || face.Box.Validate() != nil {
		return ErrInvalidPhotoFace
	}
	return validateMetadataTime("face created_at", face.CreatedAt)
}

const photoFaceColumns = `face_id,content_version_id,x,y,width,height,revision,COALESCE(assertion_id,''),created_at`

func scanPhotoFace(row scanner) (PhotoFace, error) {
	var face PhotoFace
	err := row.Scan(&face.FaceID, &face.ContentVersionID, &face.Box.X, &face.Box.Y, &face.Box.Width, &face.Box.Height, &face.Revision, &face.AssertionID, &face.CreatedAt)
	return face, err
}

func photoFacesQuery(ctx context.Context, q metadataQuerier, versionID string) ([]PhotoFace, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE content_version_id=? ORDER BY face_id LIMIT ?`, versionID, document.MaxPhotoRecordsPerVersion+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	faces := make([]PhotoFace, 0)
	for rows.Next() {
		face, err := scanPhotoFace(rows)
		if err != nil {
			return nil, err
		}
		if len(faces) == document.MaxPhotoRecordsPerVersion {
			return nil, ErrInvalidPhotoFace
		}
		faces = append(faces, face)
	}
	return faces, rows.Err()
}

// facePhotoQuery authorizes retained image or RAW versions through their live photo member.
func (s *Store) facePhotoQuery(ctx context.Context, q metadataQuerier, versionID string) error {
	if validateUUIDv4(versionID) != nil {
		return ErrInvalidPhotoFace
	}
	var assetID, role string
	var node Node
	err := q.QueryRowContext(ctx, `SELECT member.asset_id,member.role,n.id,n.name,COALESCE(cv.mime_type,'') FROM content_versions cv
 JOIN nodes n ON n.id=cv.node_id
 JOIN photo_files member ON member.node_id=cv.node_id
 JOIN photo_assets a ON a.asset_id=member.asset_id
 WHERE cv.version_id=? AND a.kind='photo' AND n.kind='file' AND member.role IN ('image','raw') AND `+liveIncludedDisplayPredicate, versionID).Scan(&assetID, &role, &node.ID, &node.Name, &node.MimeType)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !validFacePhotoNode(role, node) {
		return ErrNotFound
	}
	_, err = s.photoAssetReadQuery(ctx, q, assetID)
	return err
}

type PhotoFacePerson struct {
	PersonID    string `json:"person_id" format:"uuid"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"`
}

type PersonFacePreview struct {
	AssetID          string            `json:"asset_id" format:"uuid"`
	ContentVersionID string            `json:"content_version_id" format:"uuid"`
	GenerationID     string            `json:"generation_id"`
	Rotation         int               `json:"rotation"`
	Box              *document.FaceBox `json:"box,omitzero"`
}

type PersonFacePreviewPage struct {
	Items []PersonFacePreview `json:"items" maxItems:"3"`
}

const rejectedFaceMatchPredicate = `c.reason='face_match' AND c.state='rejected'`

// personFaceCropOrder chooses a ready crop, then its area and stable IDs.
const personFaceCropOrder = `(g.state='ready' AND g.output_blob_hash IS NOT NULL) DESC,
 face.width*face.height DESC,cv.version_id,face.face_id`

func personFacePreviewsQuery(hidden bool) string {
	return `WITH linked AS NOT MATERIALIZED (
 SELECT member.asset_id,boxed_face.face_id
 FROM person_document_assertions assertion
 CROSS JOIN content_versions cv ON cv.version_id=assertion.content_version_id
 CROSS JOIN nodes source_node ON source_node.id=cv.node_id
 CROSS JOIN photo_files member ON member.node_id=source_node.id
  AND ` + currentPhotoSourcePredicate("member", "source_node", "cv.version_id") + `
 JOIN persons person ON person.person_id=assertion.person_id AND person.state<>'retired'
 LEFT JOIN photo_faces boxed_face ON boxed_face.assertion_id=assertion.assertion_id
  AND boxed_face.content_version_id=assertion.content_version_id
 WHERE assertion.person_id=? AND assertion.role='depicted' AND assertion.action='assert'
 ), linked_assets AS MATERIALIZED (
 SELECT asset_id,MAX(face_id IS NOT NULL) has_box FROM linked GROUP BY asset_id
 ), asset_page AS MATERIALIZED (
 SELECT a.asset_id,COALESCE(pl.sort_key,'') sort_key
 FROM linked_assets CROSS JOIN ` + photoBrowseDisplayFrom + `
 WHERE a.asset_id=linked_assets.asset_id AND ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(hidden) + `
 ORDER BY linked_assets.has_box DESC,COALESCE(pl.sort_key,'') DESC,a.asset_id ASC LIMIT 3
 ), candidates AS (
 SELECT asset_page.asset_id,cv.version_id,member.rotation,face.x,face.y,face.width,face.height,
 asset_page.sort_key,COALESCE(g.generation_id,'') generation_id,
 CASE WHEN g.state='ready' AND g.output_blob_hash IS NOT NULL THEN 1 ELSE 0 END ready,
 ROW_NUMBER() OVER (PARTITION BY asset_page.asset_id ORDER BY
 (face.width IS NOT NULL) DESC,` + personFaceCropOrder + `) crop_rank
 FROM asset_page CROSS JOIN photo_files member ON member.asset_id=asset_page.asset_id
 CROSS JOIN nodes source_node ON source_node.id=member.node_id
 CROSS JOIN content_versions cv ON cv.node_id=source_node.id AND ` + currentPhotoSourcePredicate("member", "source_node", "cv.version_id") + `
 CROSS JOIN person_document_assertions assertion ON assertion.content_version_id=cv.version_id
  AND assertion.person_id=?1 AND assertion.role='depicted' AND assertion.action='assert'
 LEFT JOIN photo_faces face ON face.assertion_id=assertion.assertion_id AND face.content_version_id=cv.version_id
 LEFT JOIN visual_preview_generations g ON g.content_version_id=cv.version_id AND g.recipe_fingerprint=?2
 )
 SELECT asset_id,version_id,rotation,x,y,width,height,CASE WHEN ready=1 THEN generation_id ELSE '' END
 FROM candidates WHERE crop_rank=1 ORDER BY sort_key DESC,asset_id ASC`
}

// PersonFacePreviews returns up to three exact source versions in timeline order.
func (s *Store) PersonFacePreviews(ctx context.Context, personID, gridRecipe string, hidden bool) (PersonFacePreviewPage, error) {
	page := PersonFacePreviewPage{Items: []PersonFacePreview{}}
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		if hidden {
			if _, err := s.hiddenSession(ctx, tx); err != nil {
				return err
			}
		}
		// Bound the asset page before expanding and ranking its crop candidates.
		rows, err := tx.QueryContext(ctx, personFacePreviewsQuery(hidden), personID, gridRecipe)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var preview PersonFacePreview
			var x, y, width, height sql.NullFloat64
			if err := rows.Scan(&preview.AssetID, &preview.ContentVersionID, &preview.Rotation, &x, &y, &width, &height, &preview.GenerationID); err != nil {
				return err
			}
			if width.Valid {
				preview.Box = &document.FaceBox{X: x.Float64, Y: y.Float64, Width: width.Float64, Height: height.Float64}
			}
			page.Items = append(page.Items, preview)
		}
		return rows.Err()
	})
	return page, err
}

// PhotoFacePersonNames reads faces, assertions, and their people in one snapshot.
func (s *Store) PhotoFacePersonNames(ctx context.Context, versionID string) ([]PhotoFace, []PersonDocumentAssertion, []PhotoFacePerson, error) {
	people := make([]PhotoFacePerson, 0)
	var faces []PhotoFace
	assertions := make([]PersonDocumentAssertion, 0)
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		if err := s.facePhotoQuery(ctx, tx, versionID); err != nil {
			return err
		}
		var err error
		faces, err = photoFacesQuery(ctx, tx, versionID)
		if err == nil {
			err = s.readFaceDecisionsTx(ctx, tx, faces)
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT a.assertion_id,a.content_version_id,a.person_id,a.role,a.action,a.note,a.recorded_at,a.revision,a.photo_mark,p.display_name,p.state FROM person_document_assertions a JOIN persons p ON p.person_id=a.person_id WHERE a.content_version_id=? AND a.role='depicted' AND (a.action='assert' OR EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=a.assertion_id)) ORDER BY a.person_id,a.assertion_id LIMIT ?`, versionID, document.MaxPhotoRecordsPerVersion+1)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var assertion PersonDocumentAssertion
			var person PhotoFacePerson
			if err := rows.Scan(&assertion.AssertionID, &assertion.ContentVersionID, &assertion.PersonID, &assertion.Role,
				&assertion.Action, &assertion.Note, &assertion.RecordedAt, &assertion.Revision, &assertion.PhotoMark,
				&person.DisplayName, &person.State); err != nil {
				return err
			}
			if len(assertions) == document.MaxPhotoRecordsPerVersion {
				return ErrInvalidPhotoFace
			}
			assertions = append(assertions, assertion)
			person.PersonID = assertion.PersonID
			if len(people) == 0 || people[len(people)-1].PersonID != person.PersonID {
				people = append(people, person)
			}
		}
		return rows.Err()
	})
	return faces, assertions, people, err
}

func insertPhotoFaceTx(ctx context.Context, tx *sql.Tx, face PhotoFace) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_faces(face_id,content_version_id,x,y,width,height,revision,assertion_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, face.FaceID, face.ContentVersionID, face.Box.X, face.Box.Y, face.Box.Width, face.Box.Height, face.Revision, nullableString(face.AssertionID), face.CreatedAt)
	return err
}

// assertDepictedPersonTx creates or reuses the shared depicted edge for an active person.
// An existing suppressed edge requires an explicit assertion edit.
func (s *Store) assertDepictedPersonTx(ctx context.Context, tx *sql.Tx, versionID, personID string) (PersonDocumentAssertion, error) {
	resolved, err := resolveActiveCandidatePersonTx(ctx, tx, personID)
	if err != nil {
		return PersonDocumentAssertion{}, err
	}
	if err := s.fenceVisiblePeopleTx(ctx, tx, resolved); err != nil {
		return PersonDocumentAssertion{}, err
	}
	assertion, changed, err := ensurePersonAssertionTx(ctx, tx, versionID, resolved, "depicted")
	if err == nil && changed {
		err = advancePersonBindingEpochForVersionTx(ctx, tx, versionID)
	}
	return assertion, err
}

func (s *Store) currentFacePhotoQuery(ctx context.Context, q metadataQuerier, versionID string) error {
	var current bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id WHERE v.version_id=? AND n.current_version_id=v.version_id)`, versionID).Scan(&current); err != nil {
		return err
	}
	if !current {
		return ErrNotFound
	}
	return s.facePhotoQuery(ctx, q, versionID)
}

// CreatePhotoFace records a manual face box, optionally naming its person.
// Repeating a face ID with the same version and box returns the saved face.
func (s *Store) CreatePhotoFace(ctx context.Context, versionID, faceID string, box document.FaceBox, personID string) (PhotoFace, *PersonDocumentAssertion, error) {
	if validateUUIDv4(faceID) != nil || box.Validate() != nil || (personID != "" && validateUUIDv4(personID) != nil) {
		return PhotoFace{}, nil, ErrInvalidPhotoFace
	}
	var face PhotoFace
	var assertion *PersonDocumentAssertion
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := s.currentFacePhotoQuery(ctx, tx, versionID); err != nil {
			return err
		}
		existing, err := scanPhotoFace(tx.QueryRowContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE face_id=?`, faceID))
		if err == nil {
			if existing.ContentVersionID != versionID || existing.Box != box {
				return photoFaceExistsError{}
			}
			face, assertion, err = replayPhotoFaceTx(ctx, tx, existing, personID)
			if err == nil && assertion != nil {
				err = s.fenceVisiblePeopleTx(ctx, tx, assertion.PersonID)
			}
			if err == nil {
				items := []PhotoFace{face}
				err = s.readFaceDecisionsTx(ctx, tx, items)
				face = items[0]
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_faces WHERE content_version_id=?`, versionID).Scan(&count); err != nil {
			return err
		}
		if count >= document.MaxPhotoRecordsPerVersion {
			return ErrPhotoFaceLimit
		}
		face = PhotoFace{FaceID: faceID, ContentVersionID: versionID, Box: box, Revision: 1, CreatedAt: nowRFC3339()}
		if personID != "" {
			saved, err := s.assertDepictedPersonTx(ctx, tx, versionID, personID)
			if err != nil {
				return err
			}
			assertion = &saved
			face.AssertionID = saved.AssertionID
		}
		return insertPhotoFaceTx(ctx, tx, face)
	})
	return face, assertion, err
}

// replayPhotoFaceTx returns an existing face without changing its decision.
// A named face replays without requiring its person to remain active. Merge
// receipts survive retirement and link an absorbed ID to the saved person.
func replayPhotoFaceTx(ctx context.Context, tx *sql.Tx, existing PhotoFace, personID string) (PhotoFace, *PersonDocumentAssertion, error) {
	if existing.AssertionID != "" {
		saved, err := personAssertionByIDTx(ctx, tx, existing.AssertionID)
		if err != nil {
			return PhotoFace{}, nil, err
		}
		if personID != "" && personID != saved.PersonID {
			var merged bool
			if err := tx.QueryRowContext(ctx, `WITH RECURSIVE chain(person_id) AS (
 SELECT ? UNION SELECT m.survivor_person_id FROM person_merges m JOIN chain c ON m.absorbed_person_id=c.person_id
) SELECT EXISTS(SELECT 1 FROM chain WHERE person_id=?)`, personID, saved.PersonID).Scan(&merged); err != nil {
				return PhotoFace{}, nil, err
			}
			if !merged {
				return PhotoFace{}, nil, photoFaceExistsError{}
			}
		}
		return existing, &saved, nil
	}
	if personID == "" {
		return existing, nil, nil
	}
	return PhotoFace{}, nil, photoFaceExistsError{}
}

// MarkPersonInPhoto records a depicted edge without an invented face box.
func (s *Store) MarkPersonInPhoto(ctx context.Context, versionID, personID, personName string) (PersonDocumentAssertion, error) {
	if personID == "" || (personName != "" && !validPersonName(personName)) {
		return PersonDocumentAssertion{}, ErrInvalidPhotoFace
	}
	if personID != "" && validateUUIDv4(personID) != nil {
		return PersonDocumentAssertion{}, ErrInvalidPerson
	}
	var assertion PersonDocumentAssertion
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := s.currentFacePhotoQuery(ctx, tx, versionID); err != nil {
			return err
		}
		if personName != "" {
			var name, state string
			err := tx.QueryRowContext(ctx, `SELECT display_name,state FROM persons WHERE person_id=?`, personID).Scan(&name, &state)
			if errors.Is(err, sql.ErrNoRows) {
				_, err = createPersonWithIDTx(ctx, tx, personID, personName, "operator")
			} else if err == nil {
				if err := s.fenceVisiblePeopleTx(ctx, tx, personID); err != nil {
					return err
				}
				if state == personStateRetired {
					return ErrNotFound
				}
				if name != personName {
					return ErrInvalidPhotoFace
				}
			}
			if err != nil {
				return err
			}
		}
		var err error
		assertion, err = s.assertDepictedPersonTx(ctx, tx, versionID, personID)
		if err == nil && !assertion.PhotoMark {
			_, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET photo_mark=1,revision=revision+1 WHERE assertion_id=?`, assertion.AssertionID)
			if err == nil {
				assertion.PhotoMark = true
				assertion.Revision++
				err = advancePersonBindingEpochForVersionTx(ctx, tx, versionID)
			}
		}
		return err
	})
	return assertion, err
}

func validatePhotoFaceReferences(ctx context.Context, q metadataQuerier) error {
	var invalid bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_faces f LEFT JOIN content_versions v ON v.version_id=f.content_version_id LEFT JOIN person_document_assertions a ON a.assertion_id=f.assertion_id WHERE v.version_id IS NULL OR (f.assertion_id IS NOT NULL AND (a.assertion_id IS NULL OR NOT (a.content_version_id=f.content_version_id AND a.role='depicted'))))
 OR EXISTS(SELECT 1 FROM person_document_assertions a WHERE a.role='depicted' AND (a.action<>'withdrawn' OR EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=a.assertion_id)) GROUP BY a.content_version_id HAVING COUNT(*)>?)
 OR EXISTS(SELECT 1 FROM photo_faces GROUP BY content_version_id HAVING COUNT(*)>?)`, document.MaxPhotoRecordsPerVersion, document.MaxPhotoRecordsPerVersion).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("checking face references: %w", err)
	}
	if invalid {
		return ErrInvalidPhotoFace
	}
	return nil
}

func requirePhotoAssertionCapacity(ctx context.Context, q metadataQuerier, versionID, role, existingAssertionID string) error {
	if role != "depicted" {
		return nil
	}
	count, err := photoAssertionCount(ctx, q, versionID, existingAssertionID)
	if err != nil {
		return err
	}
	if count >= document.MaxPhotoRecordsPerVersion {
		return ErrPhotoFaceLimit
	}
	return nil
}

func validFacePhotoNode(role string, node Node) bool {
	facts := photoNodeFacts(node)
	return validatePhotoFileForAsset(PhotoKindPhoto, role, facts) == nil && (role != PhotoRoleRAW || facts.MediaType == "" || facts.MediaType == "application/octet-stream" || isPhotoCameraRawMedia(facts.MediaType))
}

func photoAssertionCount(ctx context.Context, q metadataQuerier, versionID, existingAssertionID string) (int, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_document_assertions a WHERE a.content_version_id=? AND a.role='depicted' AND a.assertion_id<>? AND (a.action<>'withdrawn' OR EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=a.assertion_id))`, versionID, existingAssertionID).Scan(&count)
	return count, err
}
