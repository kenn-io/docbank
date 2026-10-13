package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackageManifestAndSelectedRepresentationRemainBlobRoots(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	request := validPackageRequest(t, s)
	manifest := request.ManifestBlobSHA256
	representation := fakeHash("c4")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, representation, 23)
	}))
	members, err := s.SnapshotMembers(t.Context(), request.SnapshotID, 0, 1)
	require.NoError(t, err)
	members[0].Representations = []CollectionSnapshotRepresentation{{
		OccurrenceID: members[0].OccurrenceID, Role: "supplied_text", Ordinal: 0,
		Status: "available", TextAuthority: "supplied", BlobSHA256: representation,
		Size: 23, MediaType: "text/plain",
	}}
	withSidecar, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{
		SnapshotID: withSidecar, Members: members,
	})
	require.NoError(t, err)
	request.SnapshotID = withSidecar
	_, err = s.CreatePackage(t.Context(), request)
	require.NoError(t, err)
	var captured []string
	rows, err := s.db.QueryContext(t.Context(), BackupBlobAuthorityCTE()+
		`SELECT hash FROM backup_authorized_blobs WHERE hash IN (?,?) ORDER BY hash`, manifest, representation)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var hash string
		require.NoError(t, rows.Scan(&hash))
		captured = append(captured, hash)
	}
	require.NoError(t, rows.Err())
	require.ElementsMatch(t, []string{manifest, representation}, captured)
	unreachable, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	require.NotContains(t, blobHashes(unreachable), manifest)
	require.NotContains(t, blobHashes(unreachable), representation)
}
