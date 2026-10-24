package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// photoXMPPairs creates a synthetic JPEG+XMP folder without blob I/O.
func photoXMPPairs(tb testing.TB, count int) (*Store, Node) {
	tb.Helper()
	s, err := Open(filepath.Join(tb.TempDir(), "docbank.db"))
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, s.Close()) })
	folder, err := s.Mkdir(tb.Context(), s.RootID(), "Photos")
	require.NoError(tb, err)
	run, err := s.BeginIngest(tb.Context(), "photo-import", "/synthetic")
	require.NoError(tb, err)
	for i := range count {
		name := fmt.Sprintf("photo-%06d.jpg", i)
		_, err = s.IngestPhotoGroup(tb.Context(), run, PhotoImportGroup{DestinationID: folder.ID, Members: []PhotoImportMember{
			{Name: name, Role: PhotoRoleImage, BlobHash: fmt.Sprintf("%064x", i*2+1), Size: 128, MediaType: "image/jpeg", OriginalPath: "/synthetic/" + name},
			{Name: name + ".xmp", Role: PhotoRoleSidecar, BlobHash: fmt.Sprintf("%064x", i*2+2), Size: 128, MediaType: "application/rdf+xml", OriginalPath: "/synthetic/" + name + ".xmp"},
		}})
		require.NoError(tb, err)
	}
	return s, folder
}

// BenchmarkPhotoXMPAuthorityToggle isolates the committed authority write from enrollment.
func BenchmarkPhotoXMPAuthorityToggle(b *testing.B) {
	s, _ := photoXMPPairs(b, 10_000)
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(b.Context(), "INSERT INTO audit_records(digest,kind,record_json) VALUES(?,'allocation_genesis','{}')", digest)
		return err
	}))
	for _, on := range []bool{true, false} {
		b.Run(fmt.Sprintf("on=%t", on), func(b *testing.B) {
			b.ReportAllocs()
			b.StopTimer()
			for range b.N {
				if !on {
					require.NoError(b, photoXMPAuthority(b, s, digest, true))
				}
				b.StartTimer()
				err := photoXMPAuthority(b, s, digest, on)
				b.StopTimer()
				require.NoError(b, err)
				var queued int
				require.NoError(b, s.db.QueryRowContext(b.Context(), "SELECT count(*) FROM photo_xmp_unsettled").Scan(&queued))
				require.Equal(b, 10_000, queued)
				if on {
					require.NoError(b, photoXMPAuthority(b, s, digest, false))
				}
			}
		})
	}
}

func photoXMPAuthority(b *testing.B, s *Store, digest string, on bool) error {
	b.Helper()
	return s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
		if on {
			_, err := tx.ExecContext(b.Context(), "INSERT INTO audit_authority(singleton,lineage_id,operation_sequence_high_water,allocation_genesis_digest,allocation_entry_count,allocation_head) VALUES(1,'synthetic-benchmark',1,?,1,?)", digest, digest)
			return err
		}
		_, err := tx.ExecContext(b.Context(), "DELETE FROM audit_authority WHERE singleton=1")
		return err
	})
}

// BenchmarkPhotoXMPBulkNodeUpdate times a bulk revision bump over 100k non-photo nodes,
// as trash, tag renames, and tagged-node touches do.
func BenchmarkPhotoXMPBulkNodeUpdate(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(b.Context(), `WITH RECURSIVE i(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM i WHERE n<100000)
 INSERT INTO nodes(parent_id,name,kind,created_at,modified_at) SELECT ?,printf('doc-%06d.pdf',n),'dir','2024-01-01T00:00:00Z','2024-01-01T00:00:00Z' FROM i`, s.RootID())
		return err
	}))
	b.ReportAllocs()
	for b.Loop() {
		require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(b.Context(), `UPDATE nodes SET revision=revision+1 WHERE parent_id=? AND kind='dir'`, s.RootID())
			return err
		}))
	}
}

// BenchmarkPhotoXMPIdlePollUnextracted times one idle worker poll while every queued photo still waits on extraction.
func BenchmarkPhotoXMPIdlePollUnextracted(b *testing.B) {
	s, _ := photoXMPPairs(b, 20_000)
	b.ReportAllocs()
	for b.Loop() {
		targets, err := s.PhotoXMPTargetsAfter(b.Context(), "", 10)
		require.NoError(b, err)
		require.Empty(b, targets)
	}
}
