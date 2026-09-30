package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type photoOwnerFixture struct {
	s                       *Store
	first, second           Person
	firstNode, secondNode   Node
	firstAsset, secondAsset PhotoAsset
}

func enrollTestOwner(t *testing.T, s *Store, name string) Person {
	t.Helper()
	person, err := s.CreatePerson(t.Context(), name, "operator")
	require.NoError(t, err)
	_, err = s.EnrollPhotoOwner(t.Context(), person.PersonID, person.Revision)
	require.NoError(t, err)
	return person
}

func createTestFileAt(t *testing.T, s *Store, dir, name, hash string) Node {
	t.Helper()
	parent, err := s.MkdirAll(t.Context(), dir)
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), parent.ID, name, fakeHash(hash), 11, "image/jpeg")
	require.NoError(t, err)
	return node
}

func newPhotoOwnerFixture(t *testing.T) photoOwnerFixture {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()
	f := photoOwnerFixture{s: s, first: enrollTestOwner(t, s, "First"), second: enrollTestOwner(t, s, "Second")}
	f.firstNode = createTestFileAt(t, s, "/photos/"+f.first.PersonID, "first.jpg", "face01")
	f.secondNode = createTestFileAt(t, s, "/photos/"+f.second.PersonID, "second.jpg", "face02")
	var err error
	f.firstAsset, err = s.PhotoAssetForNode(ctx, f.firstNode.ID)
	require.NoError(t, err)
	f.secondAsset, err = s.PhotoAssetForNode(ctx, f.secondNode.ID)
	require.NoError(t, err)
	return f
}

func TestPhotoOwnerFolderRule(t *testing.T) {
	t.Parallel()
	f := newPhotoOwnerFixture(t)
	s, ctx := f.s, t.Context()
	public := createTestFileAt(t, s, "/taxes", "receipt.jpg", "face03")
	shared := createTestFileAt(t, s, "/photos/shared", "beach.jpg", "face04")
	stranger, err := s.CreatePerson(ctx, "Not enrolled", "operator")
	require.NoError(t, err)
	unenrolled := createTestFileAt(t, s, "/photos/"+stranger.PersonID, "x.jpg", "face05")
	nested := createTestFileAt(t, s, "/photos/"+f.second.PersonID+"/Trip/Day", "nested.jpg", "face06")
	trip, err := s.NodeByPath(ctx, "/photos/"+f.second.PersonID+"/Trip")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, trip.ID, trip.Revision)
	require.NoError(t, err)
	trashRoot := createTestFileAt(t, s, "/photos/"+f.second.PersonID, "gone.jpg", "face07")
	_, _, err = s.Trash(ctx, trashRoot.ID, trashRoot.Revision)
	require.NoError(t, err)

	firstCtx := WithPhotoOwner(ctx, f.first.PersonID)
	secondCtx := WithPhotoOwner(ctx, f.second.PersonID)
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		node    int64
		visible bool
	}{
		{"public file", firstCtx, public.ID, true},
		{"own folder", firstCtx, f.firstNode.ID, true},
		{"other owner's folder", firstCtx, f.secondNode.ID, false},
		{"other owner's folder itself", firstCtx, *f.secondNode.ParentID, false},
		{"nested trashed folder", firstCtx, nested.ID, false},
		{"nested trashed folder, own view", secondCtx, nested.ID, true},
		{"trash root from another owner's folder", firstCtx, trashRoot.ID, false},
		{"trash root, own view", secondCtx, trashRoot.ID, true},
		{"photos child named for nobody", firstCtx, shared.ID, true},
		{"photos child named for an unenrolled person", secondCtx, unenrolled.ID, true},
		{"top photos folder", firstCtx, *shared.ParentID, true},
		{"no selection", ctx, f.secondNode.ID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPhotoNodeVisibleTx(tc.ctx, s.db, tc.node)
			if tc.visible {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrNotFound)
			}
		})
	}
}

func TestPhotoOwnersEnrollment(t *testing.T) {
	t.Parallel()
	f := newPhotoOwnerFixture(t)
	ctx := t.Context()
	_, err := f.s.MergePersons(ctx, f.first.PersonID, f.second.PersonID, "00000000-0000-4000-8000-000000000001", f.first.Revision, f.second.Revision)
	require.ErrorIs(t, err, ErrPhotoOwnerReferenced)

	// Enrollment adopts an existing folder.
	fourth, err := f.s.CreatePerson(ctx, "Fourth", "operator")
	require.NoError(t, err)
	existing, err := f.s.MkdirAll(ctx, "/photos/"+fourth.PersonID)
	require.NoError(t, err)
	_, err = f.s.EnrollPhotoOwner(ctx, fourth.PersonID, fourth.Revision)
	require.NoError(t, err)
	adopted, err := f.s.NodeByPath(ctx, "/photos/"+fourth.PersonID)
	require.NoError(t, err)
	assert.Equal(t, existing.ID, adopted.ID)
}

func TestPhotoOwnersVaultWideMaintenance(t *testing.T) {
	t.Parallel()
	f := newPhotoOwnerFixture(t)
	preference := "image"
	settings, err := f.s.SetPhotoSettings(WithPhotoOwner(t.Context(), f.first.PersonID), 1, &preference)
	require.NoError(t, err)
	require.Equal(t, int64(2), settings.Revision)
}

func TestPhotoOwnersMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	f := newPhotoOwnerFixture(t)
	ctx := t.Context()
	var exported bytes.Buffer
	require.NoError(t, f.s.ExportMetadata(ctx, &exported))
	assert.Contains(t, exported.String(), `"type":"photo_owner"`)
	assert.NotContains(t, exported.String(), `"owner_id"`)

	clone := newTestStore(t)
	require.NoError(t, clone.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	owners, err := clone.PhotoOwners(ctx)
	require.NoError(t, err)
	want, err := f.s.PhotoOwners(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, owners)
	_, err = clone.PhotoAssetByID(WithPhotoOwner(ctx, f.second.PersonID), f.secondAsset.ID)
	require.NoError(t, err)
	_, err = clone.PhotoAssetByID(WithPhotoOwner(ctx, f.second.PersonID), f.firstAsset.ID)
	require.ErrorIs(t, err, ErrNotFound, "the restored vault hides the other owner's folder")

	without := func(kind, id string) []byte {
		var kept [][]byte
		for line := range bytes.Lines(exported.Bytes()) {
			if !bytes.Contains(line, []byte(`"type":"`+kind+`"`)) || !bytes.Contains(line, []byte(id)) {
				kept = append(kept, line)
			}
		}
		return bytes.Join(kept, nil)
	}
	require.Error(t, newTestStore(t).ImportMetadata(ctx, bytes.NewReader(without("person", f.second.PersonID))))
}

func TestPhotoOwnersAuditGateAndSchema(t *testing.T) {
	t.Parallel()
	require.Equal(t, 28, currentStorageSchemaVersion)
	path := filepath.Join(t.TempDir(), "docbank.db")
	s, err := Open(path)
	require.NoError(t, err)
	person, err := s.CreatePerson(t.Context(), "Audited", "operator")
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	_, err = s.EnrollPhotoOwner(t.Context(), person.PersonID, person.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.NodeByPath(t.Context(), "/photos")
	require.ErrorIs(t, err, ErrNotFound, "a refused enrollment creates no folder")

	_, err = s.db.Exec(`UPDATE vault_metadata SET schema_version=26 WHERE singleton=1`)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, err = Open(path)
	require.Error(t, err, "a v26 vault must not open with the owner schema")
}
