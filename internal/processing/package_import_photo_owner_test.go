package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

// enrollPhotoOwner enrolls a new operator person and returns its id.
func enrollPhotoOwner(t *testing.T, catalog *store.Store, name string) string {
	t.Helper()
	person, err := catalog.CreatePerson(t.Context(), name, "operator")
	require.NoError(t, err)
	_, err = catalog.EnrollPhotoOwner(t.Context(), person.PersonID, person.Revision)
	require.NoError(t, err)
	return person.PersonID
}

func TestPackageImportEnrollsPhotosUnderAdmittingOwner(t *testing.T) {
	var admitting, other string
	env := newPackageImportTestEnvForOwner(t, func(catalog *store.Store) string {
		admitting, other = enrollPhotoOwner(t, catalog, "Admitting"), enrollPhotoOwner(t, catalog, "Other")
		return admitting
	}, 1, false, true, false)
	worker, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.NoError(t, err)

	pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
	require.NoError(t, err)
	members, err := env.Catalog.SnapshotMembers(t.Context(), pkg.SnapshotID, 0, 10)
	require.NoError(t, err)
	require.Len(t, members, 1)
	var pageVersionID string
	for _, representation := range members[0].Representations {
		if representation.Role == "page_image" && representation.Status == "available" {
			pageVersionID = representation.ContentVersionID
		}
	}
	require.NotEmpty(t, pageVersionID)
	page, err := env.Catalog.ContentVersionByID(t.Context(), pageVersionID)
	require.NoError(t, err)
	asset, err := env.Catalog.PhotoAssetForNode(store.WithPhotoOwner(t.Context(), admitting), page.NodeID)
	require.NoError(t, err)
	require.Equal(t, admitting, *asset.OwnerID)
	_, err = env.Catalog.PhotoAssetForNode(store.WithPhotoOwner(t.Context(), other), page.NodeID)
	require.ErrorIs(t, err, store.ErrNotFound)
	pageImages := func(ownerID string) []string {
		members, err := env.Catalog.PackageMembers(store.WithPhotoOwner(t.Context(), ownerID), env.PackageID, 0, 10)
		require.NoError(t, err)
		require.Len(t, members, 1)
		var versions []string
		for _, representation := range members[0].Representations {
			if representation.Role == "page_image" {
				versions = append(versions, representation.ContentVersionID)
			}
		}
		return versions
	}
	require.Empty(t, pageImages(other), "another owner's browse must not expose the admitting owner's page image")
	require.Contains(t, pageImages(admitting), pageVersionID)
}
