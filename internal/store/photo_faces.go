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

func (s *Store) PhotoFaces(ctx context.Context, versionID string) ([]PhotoFace, []PersonDocumentAssertion, error) {
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
		rows, err := tx.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision,photo_mark FROM person_document_assertions WHERE content_version_id=? AND role='depicted' AND (action='assert' OR EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=person_document_assertions.assertion_id)) ORDER BY person_id,assertion_id LIMIT ?`, versionID, document.MaxPhotoRecordsPerVersion+1)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			assertion, err := scanMarkedPersonAssertion(rows)
			if err != nil {
				return err
			}
			if len(assertions) == document.MaxPhotoRecordsPerVersion {
				return ErrInvalidPhotoFace
			}
			assertions = append(assertions, assertion)
		}
		return rows.Err()
	})
	return faces, assertions, err
}

func insertPhotoFaceTx(ctx context.Context, tx *sql.Tx, face PhotoFace) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_faces(face_id,content_version_id,x,y,width,height,revision,assertion_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, face.FaceID, face.ContentVersionID, face.Box.X, face.Box.Y, face.Box.Width, face.Box.Height, face.Revision, nullableString(face.AssertionID), face.CreatedAt)
	return err
}

// assertDepictedPersonTx creates or reuses the shared depicted edge for an active person.
// An existing suppressed edge requires an explicit assertion edit.
func assertDepictedPersonTx(ctx context.Context, tx *sql.Tx, versionID, personID string) (PersonDocumentAssertion, error) {
	resolved, err := resolveActiveCandidatePersonTx(ctx, tx, personID)
	if err != nil {
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
			saved, err := assertDepictedPersonTx(ctx, tx, versionID, personID)
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
func (s *Store) MarkPersonInPhoto(ctx context.Context, versionID, personID string) (PersonDocumentAssertion, error) {
	if validateUUIDv4(personID) != nil {
		return PersonDocumentAssertion{}, ErrInvalidPerson
	}
	var assertion PersonDocumentAssertion
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := s.currentFacePhotoQuery(ctx, tx, versionID); err != nil {
			return err
		}
		var err error
		assertion, err = assertDepictedPersonTx(ctx, tx, versionID, personID)
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
 OR EXISTS(SELECT 1 FROM person_document_assertions WHERE role='depicted' AND action<>'withdrawn' GROUP BY content_version_id HAVING COUNT(*)>?)
 OR EXISTS(SELECT 1 FROM photo_faces GROUP BY content_version_id HAVING COUNT(*)>?)`, document.MaxPhotoRecordsPerVersion, document.MaxPhotoRecordsPerVersion).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("checking face references: %w", err)
	}
	if invalid {
		return ErrInvalidPhotoFace
	}
	return nil
}

func requirePhotoAssertionCapacity(ctx context.Context, q metadataQuerier, versionID, role string) error {
	if role != "depicted" {
		return nil
	}
	var count int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_document_assertions WHERE content_version_id=? AND role='depicted' AND action<>'withdrawn'`, versionID).Scan(&count); err != nil {
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
