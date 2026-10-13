package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
)

const ManualFaceFingerprint = "manual"
const maxPhotoFaces = 1000
const maxPhotoAssertions = 1000

var ErrInvalidPhotoFace = errors.New("invalid photo face")

type PhotoFace struct {
	FaceID              string           `json:"face_id"`
	ContentVersionID    string           `json:"content_version_id"`
	DetectorFingerprint string           `json:"detector_fingerprint"`
	Box                 document.FaceBox `json:"box"`
	Revision            int64            `json:"revision"`
	AssertionID         string           `json:"assertion_id,omitzero"`
	CreatedAt           string           `json:"created_at"`
}

func validatePhotoFace(face PhotoFace) error {
	if validateUUIDv4(face.FaceID) != nil || validateUUIDv4(face.ContentVersionID) != nil || face.Revision < 1 ||
		face.DetectorFingerprint == "" || len(face.DetectorFingerprint) > 256 || !document.ValidPersonIdentityText(face.DetectorFingerprint) ||
		(face.AssertionID != "" && validateUUIDv4(face.AssertionID) != nil) || face.Box.Validate() != nil {
		return ErrInvalidPhotoFace
	}
	return validateMetadataTime("face created_at", face.CreatedAt)
}

const photoFaceColumns = `face_id,content_version_id,detector_fingerprint,x,y,width,height,revision,COALESCE(assertion_id,''),created_at`

func scanPhotoFace(row scanner) (PhotoFace, error) {
	var face PhotoFace
	err := row.Scan(&face.FaceID, &face.ContentVersionID, &face.DetectorFingerprint, &face.Box.X, &face.Box.Y, &face.Box.Width, &face.Box.Height, &face.Revision, &face.AssertionID, &face.CreatedAt)
	return face, err
}

func photoFacesQuery(ctx context.Context, q metadataQuerier, versionID string) ([]PhotoFace, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE content_version_id=? ORDER BY face_id LIMIT 1001`, versionID)
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
		if len(faces) == maxPhotoFaces {
			return nil, ErrInvalidPhotoFace
		}
		faces = append(faces, face)
	}
	return faces, rows.Err()
}

// facePhotoTargetQuery checks the addressed observation member without user authorization.
func facePhotoTargetQuery(ctx context.Context, q metadataQuerier, versionID string) (string, error) {
	if validateUUIDv4(versionID) != nil {
		return "", ErrInvalidPhotoFace
	}
	var assetID, role string
	var node Node
	err := q.QueryRowContext(ctx, `SELECT member.asset_id,member.role,n.id,n.name,COALESCE(cv.mime_type,'') FROM content_versions cv
 JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
 JOIN photo_files member ON member.node_id=cv.node_id
 JOIN photo_assets a ON a.asset_id=member.asset_id
 WHERE cv.version_id=? AND a.kind='photo' AND n.kind='file' AND member.role IN ('image','raw') AND `+liveIncludedDisplayPredicate, versionID).Scan(&assetID, &role, &node.ID, &node.Name, &node.MimeType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	facts := photoNodeFacts(node)
	if validatePhotoFileForAsset(PhotoKindPhoto, role, facts) != nil ||
		(role == PhotoRoleRAW && facts.MediaType != "" && facts.MediaType != "application/octet-stream" && !isPhotoCameraRawMedia(facts.MediaType)) {
		return "", ErrNotFound
	}
	return assetID, nil
}

// facePhotoQuery adds the shared Hidden gate to structural observation eligibility.
func (s *Store) facePhotoQuery(ctx context.Context, q metadataQuerier, versionID string) error {
	assetID, err := facePhotoTargetQuery(ctx, q, versionID)
	if err != nil {
		return err
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
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision FROM person_document_assertions WHERE content_version_id=? AND role='depicted' AND (action='assert' OR EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=person_document_assertions.assertion_id)) ORDER BY person_id,assertion_id LIMIT 1001`, versionID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			assertion, err := scanPersonAssertion(rows)
			if err != nil {
				return err
			}
			if len(assertions) == maxPhotoAssertions {
				return ErrInvalidPhotoFace
			}
			assertions = append(assertions, assertion)
		}
		return rows.Err()
	})
	return faces, assertions, err
}

func insertPhotoFaceTx(ctx context.Context, tx *sql.Tx, face PhotoFace) error {
	if err := validateFaceAssertion(ctx, tx, face.ContentVersionID, face.AssertionID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_faces(face_id,content_version_id,detector_fingerprint,x,y,width,height,revision,assertion_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, face.FaceID, face.ContentVersionID, face.DetectorFingerprint, face.Box.X, face.Box.Y, face.Box.Width, face.Box.Height, face.Revision, nullableString(face.AssertionID), face.CreatedAt)
	return err
}

// assertedFacePersonTx creates only the shared depicted edge; an existing suppressed edge requires an explicit assertion edit.
func assertedFacePersonTx(ctx context.Context, tx *sql.Tx, versionID, personID string) (PersonDocumentAssertion, bool, error) {
	resolved, err := resolveActiveCandidatePersonTx(ctx, tx, personID)
	if err != nil {
		return PersonDocumentAssertion{}, false, err
	}
	return ensurePersonAssertionTx(ctx, tx, versionID, resolved, "depicted")
}

func (s *Store) CreatePhotoFace(ctx context.Context, versionID, faceID string, box document.FaceBox, personID string) (PhotoFace, *PersonDocumentAssertion, error) {
	if validateUUIDv4(faceID) != nil || box.Validate() != nil || (personID != "" && validateUUIDv4(personID) != nil) {
		return PhotoFace{}, nil, ErrInvalidPhotoFace
	}
	var face PhotoFace
	var assertion *PersonDocumentAssertion
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := s.facePhotoQuery(ctx, tx, versionID); err != nil {
			return err
		}
		resolved := ""
		if personID != "" {
			var err error
			resolved, err = resolveActiveCandidatePersonTx(ctx, tx, personID)
			if err != nil {
				return err
			}
		}
		existing, err := scanPhotoFace(tx.QueryRowContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE face_id=?`, faceID))
		if err == nil {
			if existing.ContentVersionID != versionID || existing.Box != box || existing.DetectorFingerprint != ManualFaceFingerprint {
				return ErrExists
			}
			existingPerson := ""
			if existing.AssertionID != "" {
				saved, err := personAssertionByIDTx(ctx, tx, existing.AssertionID)
				if err != nil {
					return err
				}
				assertion = &saved
				existingPerson = saved.PersonID
			}
			if resolved != "" && existingPerson != resolved {
				return ErrExists
			}
			face = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_faces WHERE content_version_id=?`, versionID).Scan(&count); err != nil {
			return err
		}
		if count >= maxPhotoFaces {
			return ErrInvalidPhotoFace
		}
		face = PhotoFace{FaceID: faceID, ContentVersionID: versionID, DetectorFingerprint: ManualFaceFingerprint, Box: box, Revision: 1, CreatedAt: nowRFC3339()}
		created := false
		if resolved != "" {
			saved, isNew, err := ensurePersonAssertionTx(ctx, tx, versionID, resolved, "depicted")
			if err != nil {
				return err
			}
			assertion = &saved
			face.AssertionID = saved.AssertionID
			created = isNew
		}
		if err := insertPhotoFaceTx(ctx, tx, face); err != nil {
			return err
		}
		if created {
			return advancePersonBindingEpochTx(ctx, tx)
		}
		return nil
	})
	return face, assertion, err
}

// MarkPersonInPhoto records a depicted edge without an invented face box.
func (s *Store) MarkPersonInPhoto(ctx context.Context, versionID, personID string) (PersonDocumentAssertion, error) {
	if validateUUIDv4(personID) != nil {
		return PersonDocumentAssertion{}, ErrInvalidPerson
	}
	var assertion PersonDocumentAssertion
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		err := s.facePhotoQuery(ctx, tx, versionID)
		if err != nil {
			return err
		}
		var created bool
		assertion, created, err = assertedFacePersonTx(ctx, tx, versionID, personID)
		if err != nil || !created {
			return err
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
	return assertion, err
}

// ReconcilePhotoFaces matches observations across detector versions by best overlap, with stable tie breaking.
func (s *Store) ReconcilePhotoFaces(ctx context.Context, versionID, fingerprint string, boxes []document.FaceBox) ([]PhotoFace, error) {
	if fingerprint == "" || fingerprint == ManualFaceFingerprint || len(fingerprint) > 256 || !document.ValidPersonIdentityText(fingerprint) || len(boxes) > maxPhotoFaces {
		return nil, ErrInvalidPhotoFace
	}
	for _, box := range boxes {
		if box.Validate() != nil {
			return nil, ErrInvalidPhotoFace
		}
	}
	boxes = slices.Clone(boxes)
	slices.SortFunc(boxes, compareFaceBoxes)
	var faces []PhotoFace
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if _, err := facePhotoTargetQuery(ctx, tx, versionID); err != nil {
			return err
		}
		old, err := photoFacesQuery(ctx, tx, versionID)
		if err != nil {
			return err
		}
		type match struct {
			old, new int
			overlap  float64
		}
		var matches []match
		for i, face := range old {
			for j, box := range boxes {
				if overlap := face.Box.Overlap(box); overlap >= 0.5 {
					matches = append(matches, match{i, j, overlap})
				}
			}
		}
		slices.SortFunc(matches, func(a, b match) int {
			if a.overlap > b.overlap {
				return -1
			}
			if a.overlap < b.overlap {
				return 1
			}
			if c := strings.Compare(old[a.old].FaceID, old[b.old].FaceID); c != 0 {
				return c
			}
			return a.new - b.new
		})
		usedOld := make(map[int]bool)
		usedNew := make(map[int]bool)
		for _, m := range matches {
			if usedOld[m.old] || usedNew[m.new] {
				continue
			}
			usedOld[m.old] = true
			usedNew[m.new] = true
			face := old[m.old]
			box := boxes[m.new]
			if face.DetectorFingerprint == ManualFaceFingerprint || (face.DetectorFingerprint == fingerprint && face.Box == box) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE photo_faces SET detector_fingerprint=?,x=?,y=?,width=?,height=?,revision=revision+1 WHERE face_id=?`, fingerprint, box.X, box.Y, box.Width, box.Height, face.FaceID); err != nil {
				return err
			}
		}
		protected, err := protectedCandidatePhotoFaceIDs(ctx, tx, versionID)
		if err != nil {
			return err
		}
		retained := len(old)
		for i, face := range old {
			if usedOld[i] || face.DetectorFingerprint == ManualFaceFingerprint || face.AssertionID != "" || protected[face.FaceID] {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM photo_faces WHERE face_id=?`, face.FaceID); err != nil {
				return err
			}
			retained--
		}
		added := 0
		for j, box := range boxes {
			if usedNew[j] {
				continue
			}
			if retained+added >= maxPhotoFaces {
				return ErrInvalidPhotoFace
			}
			id, err := newUUIDv4()
			if err != nil {
				return err
			}
			face := PhotoFace{FaceID: id, ContentVersionID: versionID, DetectorFingerprint: fingerprint, Box: box, Revision: 1, CreatedAt: nowRFC3339()}
			if err := insertPhotoFaceTx(ctx, tx, face); err != nil {
				return err
			}
			added++
		}
		faces, err = photoFacesQuery(ctx, tx, versionID)
		return err
	})
	return faces, err
}

func protectedCandidatePhotoFaceIDs(ctx context.Context, q metadataQuerier, versionID string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT json_extract(e.value,'$.evidence_id')
 FROM person_match_candidates c,json_each(c.evidence_json) e
 WHERE c.state IN ('open','rejected') AND c.reason='face_match' AND json_extract(e.value,'$.evidence_kind')='face' AND json_extract(e.value,'$.content_version_id')=?`, versionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

func compareFaceBoxes(a, b document.FaceBox) int {
	for _, pair := range [][2]float64{{a.X, b.X}, {a.Y, b.Y}, {a.Width, b.Width}, {a.Height, b.Height}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

const faceAssertionMatchesSQL = `a.content_version_id=f.content_version_id AND a.role='depicted'`

func validatePhotoFaceReferences(ctx context.Context, q metadataQuerier) error {
	var invalid bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_faces f LEFT JOIN content_versions v ON v.version_id=f.content_version_id LEFT JOIN person_document_assertions a ON a.assertion_id=f.assertion_id WHERE v.version_id IS NULL OR (f.assertion_id IS NOT NULL AND (a.assertion_id IS NULL OR NOT (`+faceAssertionMatchesSQL+`))))
 OR EXISTS(SELECT 1 FROM person_document_assertions WHERE role='depicted' GROUP BY content_version_id HAVING COUNT(*)>1000)
 OR EXISTS(SELECT 1 FROM photo_faces GROUP BY content_version_id HAVING COUNT(*)>1000)
 OR EXISTS(SELECT 1 FROM person_match_candidates c, json_each(c.evidence_json) e LEFT JOIN photo_faces f ON f.face_id=json_extract(e.value,'$.evidence_id') WHERE c.state='open' AND json_extract(e.value,'$.evidence_kind')='face' AND (f.face_id IS NULL OR f.content_version_id<>json_extract(e.value,'$.content_version_id')))
 OR EXISTS(SELECT 1 FROM persons p LEFT JOIN photo_faces f ON f.face_id=p.key_face_id LEFT JOIN person_document_assertions a ON a.assertion_id=f.assertion_id WHERE p.key_face_id IS NOT NULL AND (f.face_id IS NULL OR a.person_id IS NULL OR a.person_id<>p.person_id OR a.action<>'assert'))`).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("checking face references: %w", err)
	}
	if invalid {
		return ErrInvalidPhotoFace
	}
	return nil
}

func validateFaceAssertion(ctx context.Context, q metadataQuerier, versionID, assertionID string) error {
	if assertionID == "" {
		return nil
	}
	var valid bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT ? AS content_version_id) f JOIN person_document_assertions a ON a.assertion_id=? WHERE `+faceAssertionMatchesSQL+`)`, versionID, assertionID).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrInvalidPhotoFace
	}
	return nil
}

func requirePhotoAssertionCapacity(ctx context.Context, q metadataQuerier, versionID, role string) error {
	if role != "depicted" {
		return nil
	}
	var count int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_document_assertions WHERE content_version_id=? AND role='depicted'`, versionID).Scan(&count); err != nil {
		return err
	}
	if count >= maxPhotoAssertions {
		return ErrInvalidPhotoFace
	}
	return nil
}
