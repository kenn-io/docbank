package metadata

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testVaultID     = "11111111-1111-4111-8111-111111111111"
	testCreateID    = "22222222-2222-4222-8222-222222222222"
	testFirstEditID = "33333333-3333-4333-8333-333333333333"
	testReplaceID   = "44444444-4444-4444-8444-444444444444"
	testRevertID    = "55555555-5555-4555-8555-555555555555"
	testReplaceOpID = "66666666-6666-4666-8666-666666666666"
	testRevertOpID  = "77777777-7777-4777-8777-777777777777"
	testIngestID    = "88888888-8888-4888-8888-888888888888"
	testTagID       = "99999999-9999-4999-8999-999999999999"
	testCreateOpID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	testFirstEditOp = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	testTime        = "2026-07-14T12:00:00.000000000Z"
	testEarlier     = "2026-07-13T12:00:00.000000000Z"
	testHashA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testHashB       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestZeroScopeRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := openTestDB(t)
	seedZeroScopeFixture(t, source)
	require.NoError(t, Validate(ctx, source))

	var first, second bytes.Buffer
	require.NoError(t, Export(ctx, source, &first))
	require.NoError(t, Export(ctx, source, &second))
	assert.Equal(t, first.String(), second.String())
	assert.Contains(t, first.String(), `"version":1`)
	assert.Contains(t, first.String(), `"node_sequence":10`)
	assert.Contains(t, first.String(), `"name":"_3g"`)
	assert.Contains(t, first.String(), `"source_desc":"_2FnZW50"`)

	target := openTestDB(t)
	require.NoError(t, Import(ctx, target, bytes.NewReader(first.Bytes())))
	require.NoError(t, Validate(ctx, target))
	var roundTrip bytes.Buffer
	require.NoError(t, Export(ctx, target, &roundTrip))
	assert.Equal(t, first.String(), roundTrip.String())
}

func TestImportRejectsMalformedAuthorityTransactionally(t *testing.T) {
	ctx := context.Background()
	source := openTestDB(t)
	seedZeroScopeFixture(t, source)
	var exported bytes.Buffer
	require.NoError(t, Export(ctx, source, &exported))
	valid := exported.String()

	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name: "padded opaque bytes",
			mutate: func(input string) string {
				return strings.Replace(input, `"name":"_3g"`, `"name":"_3g="`, 1)
			},
			wantErr: "base64",
		},
		{
			name: "nonzero base64 trailing bits",
			mutate: func(input string) string {
				return strings.Replace(input, `"name":"_3g"`, `"name":"_3h"`, 1)
			},
			wantErr: "canonical",
		},
		{
			name: "noncanonical UUID",
			mutate: func(input string) string {
				return strings.Replace(input, testVaultID,
					"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", 1)
			},
			wantErr: "canonical lowercase UUID",
		},
		{
			name: "lowered node high water",
			mutate: func(input string) string {
				return strings.Replace(input, `"node_sequence":10`, `"node_sequence":1`, 1)
			},
			wantErr: "below maximum node ID",
		},
		{
			name: "older current pointer",
			mutate: func(input string) string {
				return strings.Replace(input,
					`"current_version_id":"`+testRevertID+`"`,
					`"current_version_id":"`+testReplaceID+`"`, 1)
			},
			wantErr: "current version is older",
		},
		{
			name: "duplicate known revision",
			mutate: func(input string) string {
				return strings.Replace(input,
					`"node_revision":3`, `"node_revision":2`, 1)
			},
			wantErr: "UNIQUE constraint failed",
		},
		{
			name: "directory content version",
			mutate: func(input string) string {
				return strings.Replace(input, `"node_id":2`, `"node_id":1`, 1)
			},
			wantErr: "directory retains a content version",
		},
		{
			name: "forged provenance identity",
			mutate: func(input string) string {
				marker := `"type":"provenance","identity":"`
				index := strings.Index(input, marker)
				require.NotEqual(t, -1, index)
				index += len(marker)
				replacement := "0"
				if input[index] == '0' {
					replacement = "1"
				}
				return input[:index] + replacement + input[index+1:]
			},
			wantErr: "identity does not match",
		},
		{
			name: "unknown field",
			mutate: func(input string) string {
				return strings.Replace(input, `"node_sequence":10}`, `"node_sequence":10,"surprise":true}`, 1)
			},
			wantErr: "unknown field",
		},
		{
			name: "duplicate field",
			mutate: func(input string) string {
				return strings.Replace(input, `"size":4`, `"size":4,"size":4`, 1)
			},
			wantErr: "duplicate field",
		},
		{
			name: "case aliased field",
			mutate: func(input string) string {
				return strings.Replace(input, `"size":4`, `"Size":4`, 1)
			},
			wantErr: "case alias",
		},
		{
			name: "null scalar field",
			mutate: func(input string) string {
				return strings.Replace(input, `"size":4`, `"size":null`, 1)
			},
			wantErr: "cannot be null",
		},
		{
			name: "ambiguous provenance successors",
			mutate: func(input string) string {
				first, _ := fixtureProvenance(t)
				path := OpaqueBytes("alternate")
				third := Provenance{
					Type: provenanceRecordType, NodeID: 2, IngestID: testIngestID,
					OriginalPath: &path, Supersedes: &first.Identity,
				}
				var err error
				third.Identity, err = ProvenanceIdentity(third)
				require.NoError(t, err)
				encoded, err := json.Marshal(third)
				require.NoError(t, err)
				return strings.Replace(input, `{"type":"tag"`, string(encoded)+"\n"+`{"type":"tag"`, 1)
			},
			wantErr: "UNIQUE constraint failed: provenance.supersedes",
		},
		{
			name: "cross-node provenance successor",
			mutate: func(input string) string {
				_, original := fixtureProvenance(t)
				changed := original
				changed.NodeID = 1
				var err error
				changed.Identity, err = ProvenanceIdentity(changed)
				require.NoError(t, err)
				oldJSON, err := json.Marshal(original)
				require.NoError(t, err)
				newJSON, err := json.Marshal(changed)
				require.NoError(t, err)
				return strings.Replace(input, string(oldJSON), string(newJSON), 1)
			},
			wantErr: "provenance supersedes a fact on another node",
		},
		{
			name: "invalid UTF-8 text",
			mutate: func(input string) string {
				return strings.Replace(input, `"name":"archive"`,
					`"name":"a`+string([]byte{0xff})+`"`, 1)
			},
			wantErr: "invalid UTF-8",
		},
		{
			name: "unpaired surrogate",
			mutate: func(input string) string {
				return strings.Replace(input, `"name":"archive"`, `"name":"\ud800"`, 1)
			},
			wantErr: "unpaired high surrogate",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := openTestDB(t)
			err := Import(ctx, target, strings.NewReader(test.mutate(valid)))
			require.ErrorContains(t, err, test.wantErr)
			assertPristine(t, target)
		})
	}
}

func TestNewUUIDIsCanonicalV4(t *testing.T) {
	first, err := NewUUID()
	require.NoError(t, err)
	second, err := NewUUID()
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
	assert.NoError(t, validateUUID("generated UUID", first))
	assert.NoError(t, validateUUID("generated UUID", second))
}

func TestSchemaRequiresForeignKeys(t *testing.T) {
	db, err := sql.Open("sqlite3", t.TempDir()+"/unsafe.db")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	err = CreateSchema(context.Background(), db)
	require.ErrorContains(t, err, "foreign keys are disabled")
}

func TestExportRejectsInvalidDirectState(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		mutate  string
		wantErr string
	}{
		{
			name:    "invalid text bytes",
			mutate:  `UPDATE tags SET name=CAST(X'FF' AS TEXT)`,
			wantErr: "invalid tag record",
		},
		{
			name:    "forged provenance identity",
			mutate:  `UPDATE provenance SET identity=printf('%064x',0) WHERE supersedes IS NOT NULL`,
			wantErr: "identity does not match",
		},
		{
			name:    "older current pointer",
			mutate:  `UPDATE nodes SET current_version_id='` + testReplaceID + `' WHERE id=2`,
			wantErr: "current version is older",
		},
		{
			name: "create after revision one",
			mutate: `UPDATE content_versions SET transition_kind='content_create' ` +
				`WHERE version_id='` + testReplaceID + `'`,
			wantErr: "content-create chronology is invalid",
		},
		{
			name: "revert source does not match result",
			mutate: `UPDATE content_versions SET source_version_id='` + testReplaceID + `' ` +
				`WHERE version_id='` + testRevertID + `'`,
			wantErr: "revert source relationship is invalid",
		},
		{
			name:    "directory retains content version",
			mutate:  `UPDATE content_versions SET node_id=1 WHERE version_id='` + testCreateID + `'`,
			wantErr: "directory retains a content version",
		},
		{
			name:    "cross-node provenance successor",
			mutate:  `UPDATE provenance SET node_id=1 WHERE supersedes IS NOT NULL`,
			wantErr: "provenance supersedes a fact on another node",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := openTestDB(t)
			seedZeroScopeFixture(t, db)
			_, err := db.ExecContext(ctx, test.mutate)
			require.NoError(t, err)
			var output bytes.Buffer
			err = Export(ctx, db, &output)
			require.ErrorContains(t, err, test.wantErr)
			assert.Empty(t, output.String(), "validation must precede publication")
		})
	}
}

func TestProvenanceIdentityGolden(t *testing.T) {
	path := OpaqueBytes{0xff, '/', 'x'}
	mtime := testEarlier
	record := Provenance{
		Type:          "provenance",
		NodeID:        2,
		IngestID:      testIngestID,
		OriginalPath:  &path,
		OriginalMTime: &mtime,
	}
	identity, err := ProvenanceIdentity(record)
	require.NoError(t, err)
	assert.Equal(t, "16aaed129f1ef198ec1e52a5a670273152fd2e8502840f359564c92c23f83c2c", identity)
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := t.TempDir() + "/metadata-v1.db"
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_txlock=immediate")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, CreateSchema(context.Background(), db))
	return db
}

func seedZeroScopeFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO vault_metadata(singleton,format_version,vault_id) VALUES(1,1,?)`, testVaultID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO blobs(hash,size,created_at) VALUES
		  (?,4,?),
		  (?,5,?)`, testHashA, testEarlier, testHashB, testEarlier)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO nodes(
		  id,parent_id,name,kind,current_version_id,revision,created_at,modified_at
		) VALUES
		  (1,NULL,X'','dir',NULL,2,?,?),
		  (2,1,X'FF78','file',?,5,?,?)`,
		testEarlier, testTime, testRevertID, testEarlier, testTime)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO content_versions(
		  version_id,node_id,blob_hash,size,media_type,recorded_at,node_revision,
		  introduced_operation_id,transition_kind,source_version_id
		) VALUES
		  (?,2,?,5,'application/octet-stream',?,1,?,'content_create',NULL),
		  (?,2,?,4,'application/octet-stream',?,2,?,'content_replace',NULL),
		  (?,2,?,5,'application/octet-stream',?,3,?,'content_replace',NULL),
		  (?,2,?,4,'application/octet-stream',?,4,?,'content_revert',?)`,
		testCreateID, testHashB, testEarlier, testCreateOpID,
		testFirstEditID, testHashA, testEarlier, testFirstEditOp,
		testReplaceID, testHashB, testTime, testReplaceOpID,
		testRevertID, testHashA, testTime, testRevertOpID, testFirstEditID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO ingests(ingest_id,started_at,source_kind,source_desc)
		VALUES(?,?,'filesystem',X'FF6167656E74')`, testIngestID, testEarlier)
	require.NoError(t, err)
	firstProvenance, secondProvenance := fixtureProvenance(t)
	for _, provenance := range []Provenance{firstProvenance, secondProvenance} {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO provenance(
			  identity,node_id,ingest_id,original_path,original_mtime,supersedes
			) VALUES(?,?,?,?,?,?)`,
			provenance.Identity, provenance.NodeID, provenance.IngestID,
			opaqueArgument(provenance.OriginalPath), provenance.OriginalMTime,
			provenance.Supersedes)
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tags(tag_id,name) VALUES(?,'archive')`, testTagID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO node_tags(node_id,tag_id) VALUES(2,?)`, testTagID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO extracted_text(
		  blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
		) VALUES(?,'plain',1,'ok',NULL,1,'hello',?)`, testHashA, testTime)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = db.ExecContext(ctx, `UPDATE sqlite_sequence SET seq=10 WHERE name='nodes'`)
	require.NoError(t, err)
}

func fixtureProvenance(t *testing.T) (Provenance, Provenance) {
	t.Helper()
	path := OpaqueBytes{0xff, '/', 'x'}
	mtime := testEarlier
	first := Provenance{
		Type:          "provenance",
		NodeID:        2,
		IngestID:      testIngestID,
		OriginalPath:  &path,
		OriginalMTime: &mtime,
	}
	var err error
	first.Identity, err = ProvenanceIdentity(first)
	require.NoError(t, err)
	secondPath := OpaqueBytes("renamed/x")
	second := Provenance{
		Type:          "provenance",
		NodeID:        2,
		IngestID:      testIngestID,
		OriginalPath:  &secondPath,
		OriginalMTime: &mtime,
		Supersedes:    &first.Identity,
	}
	second.Identity, err = ProvenanceIdentity(second)
	require.NoError(t, err)
	return first, second
}

func assertPristine(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int64
	require.NoError(t, db.QueryRow(`
		SELECT
		  (SELECT COUNT(*) FROM vault_metadata) +
		  (SELECT COUNT(*) FROM blobs) +
		  (SELECT COUNT(*) FROM nodes) +
		  (SELECT COUNT(*) FROM content_versions) +
		  (SELECT COUNT(*) FROM ingests) +
		  (SELECT COUNT(*) FROM provenance) +
		  (SELECT COUNT(*) FROM tags) +
		  (SELECT COUNT(*) FROM node_tags) +
		  (SELECT COUNT(*) FROM extracted_text)
	`).Scan(&count))
	assert.Zero(t, count)
}
