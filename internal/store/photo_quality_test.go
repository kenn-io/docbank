package store

import (
	"bytes"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func qualityTarget(node Node) PhotoVisualPreviewTarget {
	return PhotoVisualPreviewTarget{VersionID: node.CurrentVersionID, SourceSHA256: node.BlobHash, Size: node.Size, MediaType: node.MimeType}
}

func TestPhotoQualityPendingFiltersAndRetention(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ready := browsePhotoNode(t, s, "ready.jpg", browseHash("ready-quality"), "image/jpeg")
	pending := browsePhotoNode(t, s, "pending.jpg", browseHash("pending-quality"), "image/jpeg")
	browsePhotoNode(t, s, "ordinary.txt", browseHash("quality-document"), "text/plain")
	browsePhotoNode(t, s, "video.mp4", browseHash("quality-video"), "video/mp4")
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(ready), document.PhotoQualitySignals{Blur: 1, Framing: 0.5}))
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(ready), document.PhotoQualitySignals{Focus: 1}))
	for _, raw := range []string{`{"filters":{"focus_max":"0"}}`, `{"syntax":"advanced","text":"focus_max:0 AND blur_min:1"}`} {
		page := browsePhotoPage(t, s, raw)
		require.Len(t, page.Items, 1)
		require.Equal(t, ready.ID, page.Items[0].NodeID)
		require.NotNil(t, page.Items[0].Quality)
		require.Zero(t, page.Items[0].Quality.Focus)
	}
	page := browsePhotoPage(t, s, `{"filters":{"unevaluated":true}}`)
	require.Len(t, page.Items, 1)
	require.Equal(t, pending.ID, page.Items[0].NodeID)
	require.Nil(t, page.Items[0].Quality)
	snapshot, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, `{"filters":{"unevaluated":true}}`)})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 1)
	require.Equal(t, pending.ID, snapshot.Rows[0].NodeID)

	_, err = s.CreateSavedQuery(ctx, "Zero focus", "", SavedQueryKindQuery, []byte(`{"filters":{"focus_max":"0"}}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Nested quality", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Zero focus\""}`))
	require.NoError(t, err)
	require.Len(t, browsePhotoPage(t, s, `{"syntax":"advanced","text":"saved:\"Nested quality\" AND blur_min:1"}`).Items, 1)
	for _, text := range []string{"focus_min:0.5*", "unevaluated:yes", "blur_max:NaN"} {
		_, err := s.CompileQuery(ctx, snapshotTestQuery(t, `{"syntax":"advanced","text":"`+text+`"}`))
		require.Error(t, err)
	}
	var backup bytes.Buffer
	require.NoError(t, exportBackupMetadataSnapshot(ctx, s.db, &backup))
	require.NotContains(t, backup.String(), "photo_quality_signals")
	restored, err := OpenForRestore(filepath.Join(t.TempDir(), "restore.db"), s.driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	require.Len(t, browsePhotoPage(t, restored, `{"filters":{"unevaluated":true}}`).Items, 2)
	_, err = s.db.Exec(`UPDATE photo_quality_signals SET evaluator_fingerprint=?`, fakeHash("af"))
	require.NoError(t, err)
	require.Len(t, browsePhotoPage(t, s, `{"filters":{"unevaluated":true}}`).Items, 2)
	require.Empty(t, browsePhotoPage(t, s, `{"filters":{"focus_max":"0"}}`).Items)
	_, err = s.db.Exec(`UPDATE photo_quality_signals SET evaluator_fingerprint=?`, document.PhotoQualityEvaluatorFingerprint())
	require.NoError(t, err)
	old := qualityTarget(ready)
	ready, _, err = s.ReplaceContent(ctx, ready.ID, ready.Revision, browseHash("replacement-quality"), 20, "image/jpeg")
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, old, document.PhotoQualitySignals{Focus: 1}))
	require.Len(t, browsePhotoPage(t, s, `{"filters":{"unevaluated":true}}`).Items, 2)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_quality_signals WHERE content_version_id=?`, old.VersionID).Scan(&count))
	require.Equal(t, 1, count)
	require.Error(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(ready), document.PhotoQualitySignals{Focus: math.NaN()}))
}

func TestPhotoQualityDisplayAndPublicationFence(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"exclude", "trash", "replace", "display"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			node := browsePhotoNode(t, s, "fenced.jpg", browseHash("fenced-"+state), "image/jpeg")
			target := qualityTarget(node)
			recipe, _ := document.BuiltInVisualPreviewRecipe("grid")
			canonical := readyVisualPreviewWithRecipe(t, node.BlobHash, browseHash("preview-"+state), 20, recipe)
			_, err := s.PublishVisualPreview(ctx, node.CurrentVersionID, canonical, &BlobPhysical{Encoding: "raw", StoredBytes: 20})
			require.NoError(t, err)
			targets, err := s.MissingPhotoQualityTargetsAfter(ctx, "", 10)
			require.NoError(t, err)
			require.Equal(t, []PhotoVisualPreviewTarget{target}, targets)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			switch state {
			case "exclude":
				_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
			case "trash":
				_, _, err = s.Trash(ctx, node.ID, node.Revision)
			case "replace":
				_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, browseHash("fenced-new"), 20, "image/jpeg")
			case "display":
				other := browsePhotoNode(t, s, "second.jpg", browseHash("second-display"), "image/jpeg")
				second, e := s.PhotoAssetForNode(ctx, other.ID)
				require.NoError(t, e)
				_, e = s.DetachPhotoFile(ctx, second.ID, second.Revision, second.Files[0].ID, PhotoDetachOptions{})
				require.NoError(t, e)
				asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, other.ID, PhotoRoleImage, nil)
				require.NoError(t, err)
				for _, file := range asset.Files {
					if file.NodeID == other.ID {
						_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, new(file.ID))
						break
					}
				}
			}
			require.NoError(t, err)
			require.NoError(t, s.PublishPhotoQualitySignals(ctx, target, document.PhotoQualitySignals{Focus: 0.7}))
			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_quality_signals`).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestOpenCutsOverReleasedV28PhotoAuthority(t *testing.T) {
	t.Parallel()
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, err := test.driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			_, err = db.Exec(schemaV0150SQL)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			s, err := openCurrentStore(path, test.driver)
			require.NoError(t, err)
			node := browsePhotoNode(t, s, "released.jpg", browseHash("released-photo"), "image/jpeg")
			require.NoError(t, s.PublishPhotoQualitySignals(t.Context(), qualityTarget(node), document.PhotoQualitySignals{Focus: 0.7}))
			packed := browsePhotoNode(t, s, "packed.jpg", browseHash("released-packed"), "image/jpeg")
			packID := "0123456789abcdef0123456789abcdef"
			_, err = s.db.Exec(`INSERT INTO blob_packs(store_id,pack_id,entry_count,stored_bytes,created_at) VALUES(?,?,1,100,?)`, s.primaryStoreID, packID, nowRFC3339())
			require.NoError(t, err)
			_, err = s.db.Exec(`UPDATE blob_locations SET kind='packed',encoding=NULL,stored_size=20 WHERE blob_hash=?`, packed.BlobHash)
			require.NoError(t, err)
			_, err = s.db.Exec(`INSERT INTO blob_pack_entries(blob_hash,store_id,pack_id,pack_offset,stored_len,raw_len,flags,crc32c) VALUES(?,?,?,64,20,20,0,0)`, packed.BlobHash, s.primaryStoreID, packID)
			require.NoError(t, err)
			_, err = s.db.Exec(`DROP TABLE photo_quality_signals; UPDATE vault_metadata SET schema_version=28`)
			require.NoError(t, err)
			var before bytes.Buffer
			require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), s.db, &before, 28))
			require.NoError(t, s.Close())
			upgraded, err := Open(path, test.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, upgraded.Close()) }()
			var version, count int
			require.NoError(t, upgraded.db.QueryRow(`SELECT schema_version FROM vault_metadata`).Scan(&version))
			require.Equal(t, 29, version)
			require.NoError(t, upgraded.db.QueryRow(`SELECT COUNT(*) FROM photo_quality_signals`).Scan(&count))
			require.Zero(t, count)
			page := browsePhotoPage(t, upgraded, `{"filters":{"unevaluated":true}}`)
			require.Len(t, page.Items, 2)
			require.ElementsMatch(t, []string{node.CurrentVersionID, packed.CurrentVersionID}, []string{page.Items[0].ContentVersionID, page.Items[1].ContentVersionID})
			physical, err := upgraded.PhysicalContent(t.Context(), packed.BlobHash)
			require.NoError(t, err)
			require.Equal(t, "packed", physical.Kind)
			physical, err = upgraded.PhysicalContent(t.Context(), node.BlobHash)
			require.NoError(t, err)
			require.Equal(t, "loose", physical.Kind)
			var after bytes.Buffer
			require.NoError(t, upgraded.ExportMetadata(t.Context(), &after))
			require.Equal(t, before.String(), after.String())
			recovery, err := test.driver.Open(path+".schema-v28.bak", docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred})
			require.NoError(t, err)
			require.NoError(t, recovery.QueryRow(`SELECT schema_version FROM vault_metadata`).Scan(&version))
			require.Equal(t, 28, version)
			require.NoError(t, recovery.Close())
		})
	}
}

func TestPhotoQualityPurgedVersionCascade(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "purged.jpg", browseHash("purged-quality"), "image/jpeg")
	require.NoError(t, s.PublishPhotoQualitySignals(t.Context(), qualityTarget(node), document.PhotoQualitySignals{}))
	_, _, err := s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_quality_signals`).Scan(&count))
	require.Zero(t, count)
}
