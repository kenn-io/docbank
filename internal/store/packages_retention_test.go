package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSealedSnapshotRetainsSourceVersionDuringPrune(t *testing.T) {
	s := newTestStore(t)
	body := []byte("Original package source")
	hash := sha256.Sum256(body)
	node, err := s.CreateFile(t.Context(), s.RootID(), "original.txt", hex.EncodeToString(hash[:]), int64(len(body)), "text/plain")
	require.NoError(t, err)
	oldVersion := node.CurrentVersionID
	snapshotID, err := newUUIDv4()
	require.NoError(t, err)
	occurrence := strings.Repeat("e", 32)
	_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{
		SnapshotID: snapshotID,
		Members: []CollectionSnapshotMember{{
			Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID,
			ContentVersionID: oldVersion, BlobSHA256: hex.EncodeToString(hash[:]), Size: int64(len(body)),
			FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name,
			FrozenFieldsJSON: "{}", DocumentKind: "other",
		}},
	})
	require.NoError(t, err)
	replacement := []byte("New current version")
	replacementHash := sha256.Sum256(replacement)
	updated, _, err := s.ReplaceContent(t.Context(), node.ID, node.Revision,
		hex.EncodeToString(replacementHash[:]), int64(len(replacement)), "text/plain")
	require.NoError(t, err)
	selector := VersionPruneSelector{VersionIDs: []string{oldVersion}}
	for _, run := range []bool{false, true} {
		_, err = s.PruneContentVersions(t.Context(), node.ID, updated.Revision, selector, run)
		require.ErrorIs(t, err, ErrPackageRetained)
	}
	_, err = s.ContentVersionByID(t.Context(), oldVersion)
	require.NoError(t, err)
}

func TestTrashEmptySkipsSealedSnapshotSourceAndDeletesOtherTrash(t *testing.T) {
	s := newTestStore(t)
	protected, err := s.CreateFile(t.Context(), s.RootID(), "snapshot-source.txt", fakeHash("c1"), 10, "text/plain")
	require.NoError(t, err)
	unrelated, err := s.CreateFile(t.Context(), s.RootID(), "unrelated.txt", fakeHash("c2"), 10, "text/plain")
	require.NoError(t, err)
	snapshotID, err := newUUIDv4()
	require.NoError(t, err)
	occurrence := strings.Repeat("a", 32)
	_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: snapshotID,
		Members: []CollectionSnapshotMember{{Ordinal: 1, OccurrenceID: occurrence,
			NodeID: protected.ID, ContentVersionID: protected.CurrentVersionID,
			BlobSHA256: protected.BlobHash, Size: 10, FamilyID: occurrence, FamilyOrder: 1,
			DisplayName: protected.Name, FrozenFieldsJSON: "{}", DocumentKind: "other"}}})
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), protected.ID, protected.Revision)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), unrelated.ID, unrelated.Revision)
	require.NoError(t, err)
	preview, err := s.TrashEmptyBounded(t.Context(), 0, 1, false)
	require.NoError(t, err)
	require.EqualValues(t, 1, preview.Candidates)
	require.False(t, preview.More)
	run, err := s.TrashEmptyBounded(t.Context(), 0, 1, true)
	require.NoError(t, err)
	require.EqualValues(t, 1, run.Deleted)
	_, err = s.NodeByID(t.Context(), protected.ID)
	require.NoError(t, err)
	_, err = s.NodeByID(t.Context(), unrelated.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPackageLabelAndReceiptRetainHistoricalVersionsDuringPrune(t *testing.T) {
	for _, authority := range []string{"label", "receipt"} {
		t.Run(authority, func(t *testing.T) {
			s := newTestStore(t)
			pkg, node := seedReceivedPackage(t, s, authority)
			key, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
			require.NoError(t, err)
			occurrence := PackageOccurrenceID(pkg.PackageID, key)
			switch authority {
			case "label":
				_, err = s.db.ExecContext(t.Context(), `INSERT INTO package_labels(
					package_id,provenance,label_set,label,label_sort_key,occurrence_id,content_version_id,
					page_state,endpoint) VALUES(?,?,?,?,?,?,?,?,?)`, pkg.PackageID, "received", "EXT",
					"EXT000001", "EXT000001", occurrence, node.CurrentVersionID, "unknown", "begin")
			case "receipt":
				receiptID, idErr := newUUIDv4()
				require.NoError(t, idErr)
				_, err = s.db.ExecContext(t.Context(), `INSERT INTO package_import_receipts(
					receipt_id,package_id,record_key,occurrence_id,content_version_id,state,receipt_json,recorded_at
				) VALUES(?,?,?,?,?,?,?,?)`, receiptID, pkg.PackageID, key, occurrence, node.CurrentVersionID,
					"committed", []byte(`{}`), nowRFC3339())
				require.NoError(t, err)
				_, err = s.db.ExecContext(t.Context(), `INSERT INTO package_import_heads(package_id,record_key,receipt_id)
					VALUES(?,?,?)`, pkg.PackageID, key, receiptID)
			}
			require.NoError(t, err)
			updated, _, err := s.ReplaceContent(t.Context(), node.ID, node.Revision,
				fakeHash("d1"), 12, "text/plain")
			require.NoError(t, err)
			for _, run := range []bool{false, true} {
				_, err = s.PruneContentVersions(t.Context(), node.ID, updated.Revision,
					VersionPruneSelector{VersionIDs: []string{node.CurrentVersionID}}, run)
				require.ErrorIs(t, err, ErrPackageRetained)
			}
		})
	}
}

func TestPackageManifestAndSelectedRepresentationRemainBlobRoots(t *testing.T) {
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
