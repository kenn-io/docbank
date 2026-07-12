package store

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	metadataHashCurrent = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	metadataHashTrashed = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	metadataHashVersion = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestMetadataJSONLRoundTripPreservesLogicalState(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)

	var first, second bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &first))
	require.NoError(t, source.ExportMetadata(ctx, &second))
	assert.Equal(t, first.Bytes(), second.Bytes(), "unchanged metadata must export byte-identically")
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

	node, err := target.NodeByPath(ctx, "/Projects/report.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(10), node.ID)
	assert.Equal(t, metadataHashCurrent, node.BlobHash)
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

	created, err := target.Mkdir(ctx, target.RootID(), "after-restore")
	require.NoError(t, err)
	assert.Greater(t, created.ID, int64(12), "AUTOINCREMENT must not reuse imported stable IDs")
}

func TestImportMetadataRejectsDanglingContentAndRollsBack(t *testing.T) {
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })

	input := strings.Join([]string{
		`{"type":"meta","format":"docbank-metadata","version":1}`,
		`{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","blob_hash":null,"size":null,"mime_type":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
		`{"type":"node","id":2,"parent_id":1,"name":"missing.bin","kind":"file","blob_hash":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","size":1,"mime_type":"application/octet-stream","revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
	}, "\n") + "\n"
	err = target.ImportMetadata(context.Background(), strings.NewReader(input))
	require.ErrorContains(t, err, "foreign key")
	assert.Equal(t, int64(1), target.RootID())
	var nodes int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	assert.Equal(t, int64(1), nodes, "failed import must leave the pristine target intact")
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

func TestImportMetadataRejectsNonPristineTarget(t *testing.T) {
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	_, err = target.Mkdir(context.Background(), target.RootID(), "existing")
	require.NoError(t, err)

	input := `{"type":"meta","format":"docbank-metadata","version":1}` + "\n"
	err = target.ImportMetadata(context.Background(), strings.NewReader(input))
	require.ErrorContains(t, err, "not pristine")
	_, err = target.NodeByPath(context.Background(), "/existing")
	require.NoError(t, err)
}

func TestImportMetadataRejectsUnknownVersionAndFields(t *testing.T) {
	for _, input := range []string{
		`{"type":"meta","format":"docbank-metadata","version":2}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"surprise":true}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1}` + "\n" +
			`{"type":"future_record","value":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1}` + "\n" +
			`{"type":"blob","hash":"` + metadataHashCurrent + `","size":12}` + "\n",
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
			`INSERT INTO nodes(id,parent_id,name,kind,blob_hash,size,mime_type,revision,created_at,modified_at)
			 VALUES(10,7,'report.txt','file','` + metadataHashCurrent + `',12,'text/plain',3,
			 '2026-01-08T00:00:00.000000000Z','2026-01-09T00:00:00.000000000Z')`,
			`INSERT INTO nodes(id,parent_id,name,kind,blob_hash,size,mime_type,revision,created_at,modified_at,trashed_at,trash_parent,trash_name)
			 VALUES(11,7,'old.bin','file','` + metadataHashTrashed + `',5,'application/octet-stream',2,
			 '2026-01-10T00:00:00.000000000Z','2026-01-11T00:00:00.000000000Z',
			 '2026-01-12T00:00:00.000000000Z',7,'old.bin')`,
			`INSERT INTO node_versions(node_id,blob_hash,size,replaced_at)
			 VALUES(10,'` + metadataHashVersion + `',9,'2026-01-09T00:00:00.000000000Z')`,
			`INSERT INTO ingests(id,started_at,source_kind,source_desc)
			 VALUES(4,'2026-01-08T00:00:00.000000000Z','filesystem','dropbox')`,
			`INSERT INTO provenance(node_id,ingest_id,original_path,original_mtime)
			 VALUES(10,4,'/source/report.txt','2025-12-31T23:00:00.000000000Z')`,
			`INSERT INTO tags(id,name) VALUES(8,'important')`,
			`INSERT INTO node_tags(node_id,tag_id) VALUES(10,8)`,
			`INSERT INTO extracted_text(blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at)
			 VALUES('` + metadataHashCurrent + `','plain',2,'ok',NULL,1,'line one\nline two','2026-01-13T00:00:00.000000000Z')`,
			`INSERT INTO blob_packs(pack_id,entry_count,stored_bytes,created_at)
			 VALUES('metadata-pack',1,12,'2026-01-14T00:00:00.000000000Z')`,
			`INSERT INTO blob_pack_index(blob_hash,pack_id,pack_offset,stored_len,raw_len,flags,crc32c)
			 VALUES('` + metadataHashCurrent + `','metadata-pack',16,12,12,0,42)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}))
}
