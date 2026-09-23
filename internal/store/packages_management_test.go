package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validPackageRequest(t *testing.T, s *Store) PackageRequest {
	t.Helper()
	body := []byte("Synthetic package document")
	hash := sha256.Sum256(body)
	node, err := s.CreateFile(t.Context(), s.RootID(), "package.txt", hex.EncodeToString(hash[:]), int64(len(body)), "text/plain")
	require.NoError(t, err)
	snapshotID, err := newUUIDv4()
	require.NoError(t, err)
	occurrence := strings.Repeat("f", 32)
	_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{
		SnapshotID: snapshotID,
		Members: []CollectionSnapshotMember{{
			Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID,
			ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: int64(len(body)),
			FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name,
			FrozenFieldsJSON: "{}", DocumentKind: "other",
		}},
	})
	require.NoError(t, err)
	manifestBlob := fakeHash("b2")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, manifestBlob, 12)
	}))
	profileHash := sha256.Sum256([]byte("{}"))
	packageID, err := newUUIDv4()
	require.NoError(t, err)
	return PackageRequest{
		PackageID: packageID, SnapshotID: snapshotID, Direction: "produced",
		PackageName: "export-001", PartyLabel: "Example recipient",
		ProfileJSON: "{}", ProfileSHA256: hex.EncodeToString(profileHash[:]),
		MappingJSON: "{}", MappingSHA256: hex.EncodeToString(profileHash[:]),
		ManifestSHA256: fakeHash("b1"), ManifestBlobSHA256: manifestBlob,
		State: "sealed", Volumes: []PackageVolume{{Ordinal: 1, VolumeName: "VOL001",
			DeclaredRoot: "VOL001", MappedRoot: "VOL001", ResolvedRootSHA256: fakeHash("b3")}},
	}
}

func TestPackageIdentityIsIdempotentAndImmutable(t *testing.T) {
	s := newTestStore(t)
	request := validPackageRequest(t, s)
	created, err := s.CreatePackage(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, request.PackageID, created.PackageID)
	require.Equal(t, request.SnapshotID, created.SnapshotID)
	require.Equal(t, request.Volumes, created.Volumes)
	require.Equal(t, 1, created.MemberCount)
	require.NotEmpty(t, created.CreatedAt)

	read, err := s.Package(t.Context(), request.PackageID)
	require.NoError(t, err)
	require.Equal(t, created, read)
	again, err := s.CreatePackage(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, created, again)

	request.PackageName = "changed-export"
	_, err = s.CreatePackage(t.Context(), request)
	require.ErrorIs(t, err, ErrPackageConflict)
	_, err = s.db.Exec(`UPDATE packages SET package_name='altered' WHERE package_id=?`, request.PackageID)
	require.ErrorContains(t, err, "package identity is immutable")
}

func TestPackageCreationRequiresDirectionSpecificAdmission(t *testing.T) {
	s := newTestStore(t)
	base := validPackageRequest(t, s)
	received := base
	received.Direction = "received"
	received.State = "importing"
	_, err := s.CreatePackage(t.Context(), received)
	require.ErrorIs(t, err, ErrPackageConflict, "received imports need their own ingest run")

	produced := base
	produced.State = "importing"
	_, err = s.CreatePackage(t.Context(), produced)
	require.ErrorIs(t, err, ErrPackageConflict, "produced packages begin sealed")
}

func TestPackagesListsStablePagesAndFiltersDirection(t *testing.T) {
	s := newTestStore(t)
	base := validPackageRequest(t, s)
	ids := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003",
	}
	for _, id := range ids {
		request := base
		request.PackageID = id
		_, err := s.CreatePackage(t.Context(), request)
		require.NoError(t, err)
	}
	first, err := s.Packages(t.Context(), "produced", "", 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, ids[0], first[0].PackageID)
	require.Equal(t, ids[1], first[1].PackageID)
	second, err := s.Packages(t.Context(), "produced", first[1].PackageID, 2)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, ids[2], second[0].PackageID)
	none, err := s.Packages(t.Context(), "received", "", 2)
	require.NoError(t, err)
	require.Empty(t, none)
	_, err = s.Packages(t.Context(), "invalid", "", 2)
	require.ErrorIs(t, err, ErrPackageConflict)
}
