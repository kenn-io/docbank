package store

import (
	"bytes"
	"path/filepath"
	"sync"
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

func newPhotoOwnerFixture(t *testing.T) photoOwnerFixture {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()
	f := photoOwnerFixture{s: s, first: enrollTestOwner(t, s, "First"), second: enrollTestOwner(t, s, "Second")}
	var err error
	f.firstNode, err = s.CreateFile(WithPhotoOwner(ctx, f.first.PersonID), s.RootID(), "first.jpg", fakeHash("face01"), 11, "image/jpeg")
	require.NoError(t, err)
	f.secondNode, err = s.CreateFile(WithPhotoOwner(ctx, f.second.PersonID), s.RootID(), "second.jpg", fakeHash("face02"), 12, "image/jpeg")
	require.NoError(t, err)
	f.firstAsset, err = s.PhotoAssetForNode(ctx, f.firstNode.ID)
	require.NoError(t, err)
	f.secondAsset, err = s.PhotoAssetForNode(ctx, f.secondNode.ID)
	require.NoError(t, err)
	return f
}

func TestPhotoOwnersDefaultLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	results := make(chan PhotoOwner, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			owner, err := s.EnsureDefaultPhotoOwner(ctx)
			assert.NoError(t, err)
			results <- owner
		})
	}
	group.Wait()
	close(results)
	first := <-results
	for owner := range results {
		require.Equal(t, first.ID, owner.ID)
	}
	person, _, err := s.PersonByID(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, "Default", person.DisplayName)
	assert.Equal(t, "operator", person.Origin)

	// Removing the unused default hands the role to the next enrollment.
	named := enrollTestOwner(t, s, "Named")
	require.NoError(t, s.RemovePhotoOwner(ctx, first.ID, person.Revision))
	next, err := s.EnsureDefaultPhotoOwner(ctx)
	require.NoError(t, err)
	assert.Equal(t, named.PersonID, next.ID)
	node, err := s.CreateFile(WithPhotoOwner(ctx, ""), s.RootID(), "default.jpg", fakeHash("d1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	assert.Equal(t, named.PersonID, *asset.OwnerID)

	// Trashed members still leave the asset referencing its owner.
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	require.ErrorIs(t, s.RemovePhotoOwner(ctx, named.PersonID, named.Revision), ErrPhotoOwnerReferenced)
}

func TestPhotoOwnersEnrollment(t *testing.T) {
	t.Parallel()
	f := newPhotoOwnerFixture(t)
	ctx := t.Context()
	owners, err := f.s.PhotoOwners(ctx)
	require.NoError(t, err)
	require.Len(t, owners, 2)
	assert.Equal(t, f.first.PersonID, owners[0].ID)

	// The owner's name is the person's name.
	renamed, err := f.s.UpdatePerson(ctx, f.first.PersonID, f.first.Revision, "First Renamed")
	require.NoError(t, err)
	owner, err := f.s.PhotoOwner(ctx, f.first.PersonID)
	require.NoError(t, err)
	assert.Equal(t, "First Renamed", owner.Name)

	_, err = f.s.EnrollPhotoOwner(ctx, f.first.PersonID, renamed.Revision)
	require.ErrorIs(t, err, ErrPhotoOwnerEnrolled)
	_, err = f.s.EnrollPhotoOwner(ctx, "00000000-0000-4000-8000-000000000000", 1)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, f.s.RemovePhotoOwner(ctx, f.first.PersonID, f.first.Revision), ErrStaleRevision)
	require.ErrorIs(t, f.s.RemovePhotoOwner(ctx, f.first.PersonID, renamed.Revision), ErrPhotoOwnerReferenced)

	// Excluded assets still reference their owner, and an enrolled person
	// can be neither retired nor absorbed.
	_, err = f.s.SetPhotoAssetExcluded(WithPhotoOwner(ctx, f.second.PersonID), f.secondAsset.ID, f.secondAsset.Revision, true)
	require.NoError(t, err)
	require.ErrorIs(t, f.s.RemovePhotoOwner(ctx, f.second.PersonID, f.second.Revision), ErrPhotoOwnerReferenced)
	_, err = f.s.RetirePerson(ctx, f.second.PersonID, f.second.Revision)
	require.ErrorIs(t, err, ErrPhotoOwnerReferenced)
	_, err = f.s.MergePersons(ctx, f.first.PersonID, f.second.PersonID, "00000000-0000-4000-8000-000000000001", renamed.Revision, f.second.Revision)
	require.ErrorIs(t, err, ErrPhotoOwnerReferenced)

	_, err = f.s.PhotoAssetByID(WithPhotoOwner(ctx, f.second.PersonID), f.firstAsset.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.s.NodeByID(WithPhotoOwner(ctx, f.second.PersonID), f.firstNode.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.s.NodeByID(ctx, f.firstNode.ID)
	require.NoError(t, err, "in-process callers see every photo")
}

func TestPhotoOwnersVaultWideMaintenance(t *testing.T) {
	t.Parallel()
	f := newPhotoOwnerFixture(t)
	ctx := WithPhotoOwner(t.Context(), f.first.PersonID)
	preference := "image"
	settings, err := f.s.SetPhotoSettings(ctx, 1, &preference)
	require.NoError(t, err)
	require.Equal(t, int64(2), settings.Revision)
	for _, node := range []Node{f.firstNode, f.secondNode} {
		_, _, err = f.s.Trash(t.Context(), node.ID, node.Revision)
		require.NoError(t, err)
	}
	report, err := f.s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	require.Equal(t, int64(2), report.Deleted)
}

func TestPhotoOwnersMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	f := newPhotoOwnerFixture(t)
	ctx := t.Context()
	var exported bytes.Buffer
	require.NoError(t, f.s.ExportMetadata(ctx, &exported))
	assert.Contains(t, exported.String(), `"type":"photo_owner"`)
	assert.Contains(t, exported.String(), `"hidden_at":null`)
	assert.NotContains(t, exported.String(), "default_owner_id")

	clone := newTestStore(t)
	require.NoError(t, clone.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	owners, err := clone.PhotoOwners(ctx)
	require.NoError(t, err)
	want, err := f.s.PhotoOwners(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, owners)
	asset, err := clone.PhotoAssetByID(WithPhotoOwner(ctx, f.second.PersonID), f.secondAsset.ID)
	require.NoError(t, err)
	assert.Equal(t, f.second.PersonID, *asset.OwnerID)

	without := func(kind, id string) []byte {
		var kept [][]byte
		for line := range bytes.Lines(exported.Bytes()) {
			if !bytes.Contains(line, []byte(`"type":"`+kind+`"`)) || !bytes.Contains(line, []byte(id)) {
				kept = append(kept, line)
			}
		}
		return bytes.Join(kept, nil)
	}
	for _, stream := range [][]byte{without("photo_owner", f.second.PersonID), without("person", f.second.PersonID)} {
		require.Error(t, newTestStore(t).ImportMetadata(ctx, bytes.NewReader(stream)))
	}
}

func TestPhotoOwnersAuditGateAndSchema(t *testing.T) {
	t.Parallel()
	require.Equal(t, 28, currentStorageSchemaVersion)
	path := filepath.Join(t.TempDir(), "docbank.db")
	s, err := Open(path)
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	_, err = s.EnsureDefaultPhotoOwner(t.Context())
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	ownerID, err := s.PhotoOwnerForWrite(t.Context())
	require.NoError(t, err)
	assert.Empty(t, ownerID)

	_, err = s.db.Exec(`UPDATE vault_metadata SET schema_version=26 WHERE singleton=1`)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, err = Open(path)
	require.Error(t, err, "a v26 vault must not open with the owner schema")
}
