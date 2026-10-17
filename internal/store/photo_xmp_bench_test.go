package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

// BenchmarkPhotoXMPStatus100k measures idle worker listing and 250-row
// xmp:pending and xmp:conflict pages over 100,000 photos with decisions, of
// which 1,000 are Pending and 1,000 conflict. Seeding runs outside the timer; run it with
// go test -tags fts5 -run '^$' -bench PhotoXMPStatus100k ./internal/store/.
func BenchmarkPhotoXMPStatus100k(b *testing.B) {
	const photos = 100_000
	s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	ctx := b.Context()
	started := time.Now()
	for batch := 0; batch < photos; batch += 10_000 {
		require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
			for i := batch; i < batch+10_000; i++ {
				node, _, err := s.createFileTx(ctx, tx, s.RootID(), fmt.Sprintf("photo-%06d.jpg", i), fmt.Sprintf("%064x", i+1), 128, "image/jpeg")
				if err != nil {
					return err
				}
				if _, err := s.insertPhotoAssetTx(ctx, tx, node.ID, PhotoRoleImage, PhotoKindPhoto); err != nil {
					return err
				}
			}
			return nil
		}))
	}
	// Every extracted photo has a decision and a settled fence; 1,000
	// conflicts are the latest outcome, and 1,000 photos changed since settling.
	for _, statement := range []string{
		`UPDATE photo_files SET confirmed_fields=1,rating=3`,
		`INSERT INTO source_metadata_generations(generation_id,source_sha256,contract_version,extractor_fingerprint,canonical_json,checksum,created_at)
 SELECT lower(hex(randomblob(16))),hash,'v1','` + fakeHash("ee") + `',x'7b7d','',created_at FROM blobs`,
		`INSERT INTO source_metadata_heads(source_sha256,generation_id,published_at) SELECT source_sha256,generation_id,created_at FROM source_metadata_generations`,
		`INSERT INTO photo_change_receipts(receipt_id,operation,before_revision,after_revision,before_json,after_json,created_at)
 SELECT lower(hex(randomblob(16))),'xmp_sync',0,1,'null',json_object('version',1,'file_id',file_id,'node_id',node_id,'state','conflict'),'2026-10-01T00:00:00Z'
 FROM photo_files WHERE node_id%100=1`,
	} {
		_, err = s.db.ExecContext(ctx, statement)
		require.NoError(b, err)
	}
	settle := func(b *testing.B, where string) {
		b.Helper()
		_, err := s.db.ExecContext(ctx, `INSERT INTO photo_sidecar_considered(sidecar_file_id,asset_id,asset_revision,member_revisions,version_id,extractor_fingerprint)
 SELECT f.file_id,f.asset_id,a.revision,`+photoXMPFenceTokenSQL+`,n.current_version_id,`+photoXMPExtractorSQL+`
 FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id WHERE `+where)
		require.NoError(b, err)
	}
	settle(b, `f.node_id%100<>0`)
	b.Logf("seeded %d photos in %s", photos, time.Since(started).Round(time.Millisecond))
	page := func(b *testing.B, text string) {
		b.Helper()
		result, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{PageSize: 250, Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: text, Sort: query.Sort{Field: "name", Direction: "asc"}}}, nil)
		require.NoError(b, err)
		require.Len(b, result.Items, 250)
	}
	b.Run("idle worker listing", func(b *testing.B) {
		targets, err := s.PhotoXMPTargetsAfter(ctx, "", 10)
		require.NoError(b, err)
		require.Len(b, targets, 10)
		settle(b, `f.node_id%100=0`)
		b.Cleanup(func() {
			_, err := s.db.ExecContext(ctx, `DELETE FROM photo_sidecar_considered WHERE sidecar_file_id IN (SELECT file_id FROM photo_files WHERE node_id%100=0)`)
			require.NoError(b, err)
		})
		for b.Loop() {
			targets, err := s.PhotoXMPTargetsAfter(ctx, "", 10)
			require.NoError(b, err)
			require.Empty(b, targets)
		}
	})
	b.Run("pending page", func(b *testing.B) {
		for b.Loop() {
			page(b, "xmp:pending")
		}
	})
	b.Run("conflict page", func(b *testing.B) {
		for b.Loop() {
			page(b, "xmp:conflict")
		}
	})
}
