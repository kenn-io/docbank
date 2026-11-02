package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoFaceDetectionReconciliation(t *testing.T) {
	t.Parallel()
	fixture := newFaceDetectionTestFixture(t)
	s := fixture.s
	scan, read := fixture.scan, fixture.read
	ctx := t.Context()
	first := document.FaceObservation{Box: document.FaceBox{X: .1, Y: .1, Width: .2, Height: .2}, Roll: new(0.0), Facing: new(.9), EyesOpen: new(true), Smile: new(false)}
	other := document.FaceObservation{Box: document.FaceBox{X: .7, Y: .7, Width: .2, Height: .2}}
	require.NoError(t, scan("b1", first, other))
	faces := read()
	require.Len(t, faces, 2)
	var original PhotoFace
	for _, f := range faces {
		if f.Box == first.Box {
			original = f
		}
	}
	require.NotEmpty(t, original.FaceID)
	next := first
	next.Box.X = .11
	next.Facing = new(.5)
	require.NoError(t, scan("b2", next))
	faces = read()
	require.Len(t, faces, 1)
	require.Equal(t, original.FaceID, faces[0].FaceID)
	require.Equal(t, next.Box, faces[0].Box)
	require.InDelta(t, .5, *faces[0].Facing, 1e-12)
	require.Equal(t, int64(2), faces[0].Revision)
	next.Roll = new(.3)
	require.NoError(t, scan("b3", next))
	faces = read()
	require.Len(t, faces, 1)
	require.Equal(t, original.FaceID, faces[0].FaceID)
	require.InDelta(t, .3, *faces[0].Roll, 1e-12)
	require.Equal(t, int64(3), faces[0].Revision)
	// Restore drops completion markers. An identical detector reply keeps revisions,
	// while unavailable measurements remain distinct from observed zero or false.
	for _, tc := range []struct {
		roll     *float64
		eyes     *bool
		revision int64
	}{
		{new(.3), new(true), 3},
		{nil, new(true), 4},
		{new(0.0), new(true), 5},
		{new(0.0), nil, 6},
		{new(0.0), new(false), 7},
	} {
		var exported bytes.Buffer
		require.NoError(t, s.ExportMetadata(ctx, &exported))
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
		s = restored
		fixture.s = restored
		_, checkpoint, err := s.VaultChanges(ctx, "", 500)
		require.NoError(t, err)
		next.Roll, next.EyesOpen = tc.roll, tc.eyes
		require.NoError(t, scan("b3", next))
		changes, _, err := s.VaultChanges(ctx, checkpoint, 500)
		require.NoError(t, err)
		if tc.revision == 3 {
			require.Empty(t, changes)
		} else {
			require.Len(t, changes, 1)
			require.Equal(t, "processed", changes[0].Kind)
		}
		faces = read()
		require.Len(t, faces, 1)
		require.Equal(t, original.FaceID, faces[0].FaceID)
		require.Equal(t, tc.revision, faces[0].Revision)
		require.Equal(t, tc.roll, faces[0].Roll)
		require.Equal(t, tc.eyes, faces[0].EyesOpen)
	}
	// Completion is keyed to the detector, including empty results.
	require.NoError(t, scan("b3"))
	require.Len(t, read(), 1)
	require.NoError(t, scan("b4"))
	require.Empty(t, read())
}

func TestPhotoFaceDetectionProtectsCandidatesAndUndoReceipts(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"name", "candidate"} {
		t.Run(map[string]string{"name": "undone name receipt", "candidate": "candidate"}[action], func(t *testing.T) {
			fixture := newFaceDetectionTestFixture(t)
			s, version := fixture.s, fixture.version
			scan, read := fixture.scan, fixture.read
			ctx := t.Context()
			observation := document.FaceObservation{Box: document.FaceBox{Width: .2, Height: .2}, Facing: new(.9)}
			require.NoError(t, scan("b1", observation))
			face := read()[0]
			person, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			var receipt PhotoFaceDecisionReceipt
			switch action {
			case "candidate":
				buildFaceMatchCandidateForTest(t, s, version, face.FaceID, 0, 0)
			default:
				decision := action
				request := PhotoFaceDecisionRequest{FaceID: face.FaceID, Revision: face.Revision, Action: decision}
				request.PersonID = person.PersonID
				receipt, err = s.DecidePhotoFace(ctx, version, request)
				require.NoError(t, err)
			}
			if action == "name" {
				_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
				require.NoError(t, err)
				require.Empty(t, read()[0].AssertionID)
			}
			before := read()
			_, checkpoint, err := s.VaultChanges(ctx, "", 500)
			require.NoError(t, err)
			observation.Box.X = .01
			observation.Facing = new(.1)
			require.NoError(t, scan("b2", observation))
			require.Equal(t, before, read())
			require.NoError(t, scan("b3"))
			require.Equal(t, before, read())
			changes, _, err := s.VaultChanges(ctx, checkpoint, 500)
			require.NoError(t, err)
			require.Empty(t, changes)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceDetectionCapacityRollback(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	version := node.CurrentVersionID
	automatic := PhotoFace{FaceID: uuid.New().String(), ContentVersionID: version, Box: document.FaceBox{X: .5, Y: .5, Width: .1, Height: .1}, Facing: new(.9), DetectorFingerprint: fakeHash("b0"), Revision: 1, CreatedAt: nowRFC3339()}
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		for range document.MaxPhotoRecordsPerVersion - 1 {
			if err := insertPhotoFaceTx(ctx, tx, PhotoFace{FaceID: uuid.New().String(), ContentVersionID: version, Box: document.FaceBox{Width: .1, Height: .1}, Revision: 1, CreatedAt: nowRFC3339()}); err != nil {
				return err
			}
		}
		return insertPhotoFaceTx(ctx, tx, automatic)
	}))
	scan := func() error {
		return s.withStorageTx(ctx, func(tx *sql.Tx) error {
			return publishFaceDetectionTx(ctx, tx, version, fakeHash("b1"), document.FaceDetection{Faces: []document.FaceObservation{{Box: automatic.Box, Facing: new(.4)}, {Box: document.FaceBox{X: .8, Y: .8, Width: .1, Height: .1}}}}, time.Now())
		})
	}
	require.ErrorIs(t, scan(), ErrPhotoFaceLimit)
	saved, err := photoFacesQuery(ctx, s.db, version)
	require.NoError(t, err)
	index := slices.IndexFunc(saved, func(face PhotoFace) bool { return face.FaceID == automatic.FaceID })
	require.NotEqual(t, -1, index, "the matched face remains after rollback")
	require.Equal(t, automatic, saved[index], "the matched update rolls back before the capacity error")
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_faces`).Scan(&count))
	require.Equal(t, 1000, count)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_face_scans`).Scan(&count))
	require.Zero(t, count)
}

func TestPhotoFaceDetectionReleasedFaces(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "vault.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0160SQL, 41)
			node := createReleasedFixtureFile(t, legacy, "portrait.jpg", fakeHash("a1"), 4, "image/jpeg")
			seedReleasedPhotoAsset(t, db, node.ID)
			faceID := uuid.New().String()
			_, err := db.ExecContext(ctx, `INSERT INTO photo_faces(face_id,content_version_id,x,y,width,height,revision,created_at) VALUES(?,?,0,0,0.2,0.3,1,?)`, faceID, node.CurrentVersionID, nowRFC3339())
			require.NoError(t, err)
			require.NoError(t, db.Close())
			upgraded, err := Open(t.Context(), path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, upgraded.Close()) }()
			faces, err := photoFacesQuery(ctx, upgraded.db, node.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, faces, 1)
			require.Equal(t, faceID, faces[0].FaceID)
			require.Nil(t, faces[0].Roll)
			require.Empty(t, faces[0].DetectorFingerprint)
			var exported bytes.Buffer
			require.NoError(t, upgraded.ExportMetadata(ctx, &exported))
			require.Contains(t, exported.String(), faceID)
			require.NotContains(t, exported.String(), `"detector_fingerprint":"`)
		})
	}
}

func TestPhotoFaceDetectionStableOverlap(t *testing.T) {
	t.Parallel()
	for _, shift := range []float64{.15, .16} {
		t.Run(fmt.Sprint(shift), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
			original := PhotoFace{FaceID: "10000000-0000-4000-8000-000000000001", ContentVersionID: node.CurrentVersionID, Box: document.FaceBox{Width: .2, Height: .2}, Revision: 1, CreatedAt: nowRFC3339(), DetectorFingerprint: fakeHash("b1")}
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error { return insertPhotoFaceTx(ctx, tx, original) }))
			observation := document.FaceObservation{Box: document.FaceBox{X: shift, Width: .2, Height: .2}, Facing: new(.3)}
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				return publishFaceDetectionTx(ctx, tx, node.CurrentVersionID, fakeHash("b2"), document.FaceDetection{Faces: []document.FaceObservation{observation}}, time.Now())
			}))
			faces, _, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, faces, 1)
			if shift == .15 {
				require.Equal(t, original.FaceID, faces[0].FaceID)
			} else {
				require.NotEqual(t, original.FaceID, faces[0].FaceID)
			}
		})
	}
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	ids := []string{"10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000002"}
	box := document.FaceBox{Width: .2, Height: .2}
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if err := insertPhotoFaceTx(ctx, tx, PhotoFace{FaceID: id, ContentVersionID: node.CurrentVersionID, Box: box, Revision: 1, CreatedAt: nowRFC3339(), DetectorFingerprint: fakeHash("b1")}); err != nil {
				return err
			}
		}
		return publishFaceDetectionTx(ctx, tx, node.CurrentVersionID, fakeHash("b2"), document.FaceDetection{Faces: []document.FaceObservation{{Box: box, Facing: new(.3)}, {Box: box, Facing: new(.7)}}}, time.Now())
	}))
	faces, _, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 2)
	require.Equal(t, ids[0], faces[0].FaceID)
	require.InDelta(t, .3, *faces[0].Facing, 1e-12)
	require.Equal(t, ids[1], faces[1].FaceID)
	require.InDelta(t, .7, *faces[1].Facing, 1e-12)
}

func TestPhotoFaceDetectionLooseManualBox(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		saved document.FaceBox
		boxes []document.FaceBox
		want  int
		named bool
	}{
		{"one face inside", document.FaceBox{X: .425, Y: .425, Width: .15, Height: .15}, []document.FaceBox{{X: .475, Y: .475, Width: .05, Height: .05}}, 1, false},
		{"two faces inside", document.FaceBox{X: .425, Y: .425, Width: .15, Height: .15}, []document.FaceBox{{X: .43, Y: .43, Width: .05, Height: .05}, {X: .52, Y: .52, Width: .05, Height: .05}}, 3, false},
		{"tight named box inside observation", document.FaceBox{X: .4, Y: .4, Width: .1, Height: .1}, []document.FaceBox{{X: .35, Y: .35, Width: .3, Height: .3}}, 1, true},
		{"overlap wins over nested observation", document.FaceBox{X: .3, Y: .3, Width: .2, Height: .2}, []document.FaceBox{{X: .31, Y: .3, Width: .2, Height: .2}, {X: .35, Y: .35, Width: .05, Height: .05}}, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
			personID := ""
			if tc.named {
				person, err := s.CreatePerson(ctx, "Ada", "operator")
				require.NoError(t, err)
				personID = person.PersonID
			}
			manual, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), tc.saved, personID)
			require.NoError(t, err)
			observations := make([]document.FaceObservation, len(tc.boxes))
			for i, box := range tc.boxes {
				observations[i].Box = box
			}
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				return publishFaceDetectionTx(ctx, tx, node.CurrentVersionID, fakeHash("b1"), document.FaceDetection{Faces: observations}, time.Now())
			}))
			faces, _, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, faces, tc.want)
			require.Contains(t, faces, manual, "the manual face remains unchanged")
			if tc.name == "overlap wins over nested observation" {
				for _, face := range faces {
					if face.FaceID != manual.FaceID {
						require.Equal(t, tc.boxes[1], face.Box, "the small observation stays separate from the named face")
					}
				}
			}
		})
	}
}

type faceDetectionTestFixture struct {
	t       *testing.T
	s       *Store
	version string
}

func newFaceDetectionTestFixture(t *testing.T) *faceDetectionTestFixture {
	t.Helper()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	return &faceDetectionTestFixture{t: t, s: s, version: node.CurrentVersionID}
}

func (f *faceDetectionTestFixture) scan(detector string, observations ...document.FaceObservation) error {
	return f.s.withStorageTx(f.t.Context(), func(tx *sql.Tx) error {
		return publishFaceDetectionTx(f.t.Context(), tx, f.version, fakeHash(detector), document.FaceDetection{Faces: observations}, time.Now())
	})
}

func (f *faceDetectionTestFixture) read() []PhotoFace {
	f.t.Helper()
	faces, _, _, err := f.s.PhotoFacePersonNames(f.t.Context(), f.version)
	require.NoError(f.t, err)
	return faces
}
