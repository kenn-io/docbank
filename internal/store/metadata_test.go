package store

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	metadataHashCurrent    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	metadataHashTrashed    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	metadataHashVersion    = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	metadataVersionCurrent = "11111111-1111-4111-8111-111111111111"
	metadataVersionOld     = "22222222-2222-4222-8222-222222222222"
	metadataVersionTrashed = "33333333-3333-4333-8333-333333333333"
)

func TestMetadataJSONLRoundTripPreservesLogicalState(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	filesystemMTime := time.Date(2026, time.February, 3, 4, 5, 6, 120_000_000, time.UTC).
		Format(time.RFC3339Nano)
	ingestID, err := source.BeginIngest(ctx, "cli", "actual filesystem timestamp")
	require.NoError(t, err)
	_, added, err := source.IngestFile(ctx, ingestID, source.RootID(), "filesystem-time.txt",
		"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", 4,
		"text/plain", "/source/filesystem-time.txt", filesystemMTime)
	require.NoError(t, err)
	require.True(t, added)
	lostParent, err := source.Mkdir(ctx, source.RootID(), "deleted-parent")
	require.NoError(t, err)
	lostFile, err := source.CreateFile(ctx, lostParent.ID, "lost.txt",
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 6, "text/plain")
	require.NoError(t, err)
	_, _, err = source.Trash(ctx, lostFile.ID, UnconditionalRev)
	require.NoError(t, err)
	_, _, err = source.Trash(ctx, lostParent.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = source.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, lostParent.ID)
	require.NoError(t, err)
	require.NoError(t, source.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes(id,parent_id,name,kind,created_at,modified_at)
			VALUES(100,1,'later-deleted','dir','2026-02-04T00:00:00.000000000Z','2026-02-04T00:00:00.000000000Z')`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=100`)
		return err
	}))

	var first, second bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &first))
	require.NoError(t, source.ExportMetadata(ctx, &second))
	assert.Equal(t, first.Bytes(), second.Bytes(), "unchanged metadata must export byte-identically")
	assert.Contains(t, first.String(), `{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":100}`)
	assert.Contains(t, first.String(), `"original_mtime":"2026-02-03T04:05:06.12Z"`)
	assert.Contains(t, first.String(), `{"type":"node","id":7,"parent_id":1,"name":"Projects","kind":"dir"`)
	assert.NotContains(t, first.String(), "blob_pack_index")
	assert.NotContains(t, first.String(), "metadata-pack")

	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(first.Bytes())))

	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	assert.Equal(t, first.Bytes(), restored.Bytes())
	assert.Equal(t, int64(1), target.RootID())
	var importedTrashParent sql.NullInt64
	var importedTrashName sql.NullString
	require.NoError(t, target.db.QueryRowContext(ctx,
		`SELECT trash_parent, trash_name FROM nodes WHERE id = ?`, lostFile.ID).
		Scan(&importedTrashParent, &importedTrashName))
	assert.False(t, importedTrashParent.Valid)
	require.True(t, importedTrashName.Valid)
	assert.Equal(t, "lost.txt", importedTrashName.String)

	node, err := target.NodeByPath(ctx, "/Projects/report.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(10), node.ID)
	assert.Equal(t, metadataHashCurrent, node.BlobHash)
	assert.Equal(t, metadataVersionCurrent, node.CurrentVersionID)
	versions, total, err := target.ContentVersions(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, versions, 2)
	assert.Equal(t, metadataVersionCurrent, versions[0].ID)
	assert.Equal(t, metadataVersionOld, versions[1].ID)
	_, err = target.NodeByPath(ctx, "/Empty")
	require.NoError(t, err, "empty directories are logical backup records")

	results, truncated, err := target.SearchPage(ctx, "report", 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, results, 1, "FTS must be rebuilt by logical node import")
	assert.Equal(t, node.ID, results[0].Node.ID)

	var packRows int64
	require.NoError(t, target.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM blob_packs) + (SELECT COUNT(*) FROM blob_pack_index)`).Scan(&packRows))
	assert.Zero(t, packRows, "physical pack authority is reconstructed separately")
	restoredLostFile, err := target.Restore(ctx, lostFile.ID, UnconditionalRev)
	require.NoError(t, err)
	restoredLostPath, err := target.Path(ctx, restoredLostFile.ID)
	require.NoError(t, err)
	assert.Equal(t, "/lost.txt", restoredLostPath)

	created, err := target.Mkdir(ctx, target.RootID(), "after-restore")
	require.NoError(t, err)
	assert.Greater(t, created.ID, int64(100), "AUTOINCREMENT must not reuse any historically allocated ID")
}

func TestImportMetadataRejectsDanglingContentAndRollsBack(t *testing.T) {
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })

	input := strings.Join([]string{
		`{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":2}`,
		`{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
		`{"type":"node","id":2,"parent_id":1,"name":"missing.bin","kind":"file","current_version_id":"44444444-4444-4444-8444-444444444444","revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
	}, "\n") + "\n"
	err = target.ImportMetadata(context.Background(), strings.NewReader(input))
	require.ErrorContains(t, err, "current version does not belong")
	assert.Equal(t, int64(1), target.RootID())
	var nodes int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	assert.Equal(t, int64(1), nodes, "failed import must leave the pristine target intact")
}

func TestImportMetadataRejectsOrphanedExtractionAndRollsBack(t *testing.T) {
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })

	input := strings.Join([]string{
		`{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":1}`,
		`{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
		`{"type":"extracted_text","blob_hash":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","extractor":"plain","extractor_version":1,"status":"ok","error":null,"attempts":1,"text":"orphan","extracted_at":"2026-01-01T00:00:00.000000000Z"}`,
	}, "\n") + "\n"
	err = target.ImportMetadata(context.Background(), strings.NewReader(input))
	require.ErrorContains(t, err, "extracted text references missing blob authority")
	var nodes, extracted int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM extracted_text`).Scan(&extracted))
	assert.Equal(t, int64(1), nodes)
	assert.Zero(t, extracted)
}

func TestImportMetadataRejectsDisconnectedCycle(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	cyclic := strings.Replace(exported.String(),
		`"id":7,"parent_id":1`, `"id":7,"parent_id":12`, 1)
	cyclic = strings.Replace(cyclic,
		`"id":12,"parent_id":1`, `"id":12,"parent_id":7`, 1)

	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	err = target.ImportMetadata(ctx, strings.NewReader(cyclic))
	require.ErrorContains(t, err, "unreachable")
	var nodes int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	assert.Equal(t, int64(1), nodes)
}

func TestImportMetadataRejectsUnsafeTrashTopology(t *testing.T) {
	stamp := "2026-01-01T00:00:00.000000000Z"
	otherStamp := "2026-01-02T00:00:00.000000000Z"
	root := `{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":null,"trash_parent":null,"trash_name":null}`
	tests := []struct {
		name    string
		records []string
		want    string
	}{
		{
			name: "restore parent inside subtree",
			records: []string{
				`{"type":"node","id":2,"parent_id":1,"name":"A","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":3,"trash_name":"A"}`,
				`{"type":"node","id":3,"parent_id":2,"name":"B","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":null,"trash_name":null}`,
			},
			want: "trash parent points inside its subtree",
		},
		{
			name: "trash root not detached",
			records: []string{
				`{"type":"node","id":3,"parent_id":1,"name":"container","kind":"dir","current_version_id":null,"revision":1,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":null,"trash_parent":null,"trash_name":null}`,
				`{"type":"node","id":2,"parent_id":3,"name":"A","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":1,"trash_name":"A"}`,
			},
			want: "trash root is not detached",
		},
		{
			name: "trashed node without trash root",
			records: []string{
				`{"type":"node","id":2,"parent_id":1,"name":"orphan","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":null,"trash_name":null}`,
			},
			want: "does not belong to exactly one trash root",
		},
		{
			name: "trash descendant timestamp differs",
			records: []string{
				`{"type":"node","id":2,"parent_id":1,"name":"A","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":1,"trash_name":"A"}`,
				`{"type":"node","id":3,"parent_id":2,"name":"B","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + otherStamp + `","trash_parent":null,"trash_name":null}`,
			},
			want: "live node or mismatched timestamp",
		},
		{
			name: "live descendant beneath trash root",
			records: []string{
				`{"type":"node","id":2,"parent_id":1,"name":"A","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":1,"trash_name":"A"}`,
				`{"type":"node","id":3,"parent_id":2,"name":"B","kind":"dir","current_version_id":null,"revision":1,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":null,"trash_parent":null,"trash_name":null}`,
			},
			want: "live node or mismatched timestamp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := Open(filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			lines := append([]string{
				`{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":3}`,
				root,
			}, tt.records...)
			err = target.ImportMetadata(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"))
			require.ErrorContains(t, err, tt.want)
			var nodes int64
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
			assert.Equal(t, int64(1), nodes)
		})
	}
}

func TestImportMetadataRejectsNodeSequenceBelowSurvivingIDs(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	badSequence := strings.Replace(exported.String(), `"node_sequence":50`, `"node_sequence":10`, 1)

	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	err = target.ImportMetadata(ctx, strings.NewReader(badSequence))
	require.ErrorContains(t, err, "below imported maximum")
	var nodes int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	assert.Equal(t, int64(1), nodes)
}

func TestImportMetadataRejectsNonPristineTarget(t *testing.T) {
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	_, err = target.Mkdir(context.Background(), target.RootID(), "existing")
	require.NoError(t, err)

	input := `{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":1}` + "\n"
	err = target.ImportMetadata(context.Background(), strings.NewReader(input))
	require.ErrorContains(t, err, "not pristine")
	_, err = target.NodeByPath(context.Background(), "/existing")
	require.NoError(t, err)
}

func TestImportMetadataRejectsUnknownVersionAndFields(t *testing.T) {
	for _, input := range []string{
		`{"type":"meta","format":"docbank-metadata","version":2,"node_sequence":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"version":1,"node_sequence":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":1,"surprise":true}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":1}` + "\n" +
			`{"type":"future_record","value":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":1}` + "\n" +
			`{"type":"blob","hash":"` + metadataHashCurrent + `","size":12}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":1}` + "\n" +
			`{"type":"blob","hash":"` + metadataHashCurrent + `","size":null,"created_at":"2026-01-01T00:00:00.000000000Z"}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":1}` + "\n" +
			`{"type":"blob","hash":"` + metadataHashCurrent + `","Size":12,"created_at":"2026-01-01T00:00:00.000000000Z"}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"node_sequence":1}` + "\n" +
			`{"type":"blob","hash":"` + metadataHashCurrent + `","size":12,"created_at":"2026-01-01T00:00:00.000000000+00:00"}` + "\n",
	} {
		t.Run(input, func(t *testing.T) {
			target, err := Open(filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			require.Error(t, target.ImportMetadata(context.Background(), strings.NewReader(input)))
		})
	}
}

func seedMetadataRoundTrip(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.withTx(ctx, func(tx *sql.Tx) error {
		statements := []string{
			`UPDATE nodes SET created_at='2026-01-01T00:00:00.000000000Z', modified_at='2026-01-02T00:00:00.000000000Z' WHERE id=1`,
			`INSERT INTO blobs(hash,size,created_at) VALUES
			 ('` + metadataHashCurrent + `',12,'2026-01-03T00:00:00.000000000Z'),
			 ('` + metadataHashTrashed + `',5,'2026-01-04T00:00:00.000000000Z'),
			 ('` + metadataHashVersion + `',9,'2026-01-05T00:00:00.000000000Z')`,
			`INSERT INTO nodes(id,parent_id,name,kind,created_at,modified_at) VALUES
			 (7,1,'Projects','dir','2026-01-06T00:00:00.000000000Z','2026-01-07T00:00:00.000000000Z')`,
			`INSERT INTO nodes(id,parent_id,name,kind,created_at,modified_at) VALUES
			 (12,1,'Empty','dir','2026-01-06T00:00:00.000000000Z','2026-01-07T00:00:00.000000000Z')`,
			`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at)
			 VALUES(10,7,'report.txt','file','` + metadataVersionCurrent + `',3,
			 '2026-01-08T00:00:00.000000000Z','2026-01-09T00:00:00.000000000Z')`,
			`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at,trashed_at,trash_parent,trash_name)
			 VALUES(11,1,'old.bin','file','` + metadataVersionTrashed + `',2,
			 '2026-01-10T00:00:00.000000000Z','2026-01-11T00:00:00.000000000Z',
			 '2026-01-12T00:00:00.000000000Z',7,'old.bin')`,
			`INSERT INTO content_versions(
				version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
				introduced_operation_id,transition_kind,source_version_id
			) VALUES
			 ('` + metadataVersionOld + `',10,'` + metadataHashVersion + `',9,'text/plain',
			  '2026-01-08T00:00:00.000000000Z',1,'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','content_create',NULL),
			 ('` + metadataVersionCurrent + `',10,'` + metadataHashCurrent + `',12,'text/plain',
			  '2026-01-09T00:00:00.000000000Z',2,'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','content_replace',NULL),
			 ('` + metadataVersionTrashed + `',11,'` + metadataHashTrashed + `',5,'application/octet-stream',
			  '2026-01-10T00:00:00.000000000Z',1,'cccccccc-cccc-4ccc-8ccc-cccccccccccc','content_create',NULL)`,
			`INSERT INTO ingests(id,started_at,source_kind,source_desc)
			 VALUES(4,'2026-01-08T00:00:00.000000000Z','filesystem','dropbox')`,
			`INSERT INTO provenance(node_id,ingest_id,original_path,original_mtime)
			 VALUES(10,4,'/source/report.txt','2025-12-31T23:00:00.12Z')`,
			`INSERT INTO tags(id,name) VALUES(8,'important')`,
			`INSERT INTO node_tags(node_id,tag_id) VALUES(10,8)`,
			`INSERT INTO extracted_text(blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at)
			 VALUES('` + metadataHashCurrent + `','plain',2,'ok',NULL,1,'line one\nline two','2026-01-13T00:00:00.000000000Z')`,
			`INSERT INTO blob_packs(pack_id,entry_count,stored_bytes,created_at)
			 VALUES('metadata-pack',1,12,'2026-01-14T00:00:00.000000000Z')`,
			`INSERT INTO blob_pack_index(blob_hash,pack_id,pack_offset,stored_len,raw_len,flags,crc32c)
			 VALUES('` + metadataHashCurrent + `','metadata-pack',16,12,12,0,42)`,
			`INSERT INTO nodes(id,parent_id,name,kind,created_at,modified_at)
			 VALUES(50,1,'historical-high-water','dir','2026-01-15T00:00:00.000000000Z','2026-01-15T00:00:00.000000000Z')`,
			`DELETE FROM nodes WHERE id=50`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}))
}
