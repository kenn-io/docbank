package store

import (
	"encoding/json/jsontext"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/sqlite"
)

type metadataCodecScratchKind string

type metadataCodecScratch struct {
	Type      string                   `json:"type"`
	Name      string                   `json:"name" db:"name"`
	Note      *string                  `json:"note" db:"note"`
	Count     *int64                   `json:"count" db:"count"`
	Kind      metadataCodecScratchKind `json:"kind" db:"kind"`
	Enabled   bool                     `json:"enabled" db:"enabled"`
	Tags      []string                 `json:"tags" db:"tags_json,json"`
	Raw       jsontext.Value           `json:"raw" db:"raw_json,json"`
	Blob      jsontext.Value           `json:"blob" db:"blob"`
	State     string                   `json:"state"`
	Singleton int                      `json:"-" db:"singleton"`
	Optional  *string                  `json:"optional,omitempty" db:"optional"`
}

func TestMetadataTableCodec(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := defaultSQLiteDriver().Open(filepath.Join(t.TempDir(), "codec.db"),
		docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(ctx, `CREATE TABLE scratch(name TEXT NOT NULL, note TEXT, count INTEGER,
		kind TEXT NOT NULL, enabled INTEGER NOT NULL, tags_json TEXT NOT NULL, raw_json TEXT NOT NULL,
		blob BLOB NOT NULL, singleton INTEGER NOT NULL, optional TEXT)`)
	require.NoError(t, err)
	table := newMetadataTable(metadataTable[metadataCodecScratch]{
		record: metadataCodecScratch{Type: "scratch_record", State: "template", Singleton: 1},
		table:  "scratch", suffix: "ORDER BY name", checkExport: true,
		validate: func(v metadataCodecScratch) error {
			if v.Name == "rejected" {
				return errors.New("rejected scratch")
			}
			return nil
		}})

	required, nullable := table.fields()
	assert.Equal(t, strings.Fields("type name note count kind enabled tags raw blob state"), required)
	assert.Equal(t, map[string]bool{"note": true, "count": true, "optional": true}, nullable)
	assert.Equal(t, "scratch_record", table.kind())
	assert.Equal(t, "name,note,count,kind,enabled,tags_json,raw_json,blob,singleton,optional", table.plan.list)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, line := range []string{
		`{"type":"scratch_record","name":"b","note":null,"count":null,"kind":"k","enabled":false,"tags":[ "z", "a" ],"raw":{"b":1, "a":2},"blob":"x","state":"template"}`,
		`{"type":"scratch_record","name":"a","note":"n","count":7,"kind":"k","enabled":true,"tags":[],"raw":[1],"blob":{"k":1},"state":"template"}`,
	} {
		require.NoError(t, table.importRecord(ctx, tx, jsontext.Value(line)))
	}
	require.EqualError(t, table.importRecord(ctx, tx, jsontext.Value(
		`{"type":"scratch_record","name":"rejected","note":null,"count":null,"kind":"k","enabled":false,"tags":[],"raw":1,"blob":1,"state":"template"}`)),
		"rejected scratch")
	require.NoError(t, tx.Commit())

	var stored []string
	rows, err := db.QueryContext(ctx, `SELECT name||'|'||tags_json||'|'||raw_json||'|'||typeof(blob)||'|'||singleton||'|'||typeof(note) FROM scratch ORDER BY name`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var row string
		require.NoError(t, rows.Scan(&row))
		stored = append(stored, row)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{`a|[]|[1]|blob|1|text`, `b|["z","a"]|{"b":1, "a":2}|blob|1|null`}, stored)

	var exported []metadataCodecScratch
	write := func(value any) error {
		record, ok := value.(metadataCodecScratch)
		require.True(t, ok)
		exported = append(exported, record)
		return nil
	}
	require.NoError(t, table.export(ctx, db, write))
	require.Len(t, exported, 2)
	assert.Equal(t, metadataCodecScratch{Type: "scratch_record", Name: "a", Note: new("n"), Count: new(int64(7)),
		Kind: "k", Enabled: true, Tags: []string{}, Raw: jsontext.Value(`[1]`), Blob: jsontext.Value(`{"k":1}`),
		State: "template", Singleton: 1}, exported[0])
	assert.Nil(t, exported[1].Note)
	assert.Nil(t, exported[1].Count)
	assert.Equal(t, jsontext.Value(`{"b":1, "a":2}`), exported[1].Raw)
	require.NoError(t, table.validateRows(ctx, db))

	_, err = db.ExecContext(ctx, `INSERT INTO scratch VALUES('rejected',NULL,NULL,'k',0,'[]','1',X'31',1,NULL)`)
	require.NoError(t, err)
	exported = nil
	require.EqualError(t, table.export(ctx, db, write),
		"validating scratch record metadata for export: rejected scratch")
	assert.Len(t, exported, 2)
	require.EqualError(t, table.validateRows(ctx, db),
		"validating scratch record metadata for export: rejected scratch")

	type duplicate struct {
		Type string `json:"type"`
		A    string `json:"a" db:"x"`
		B    string `json:"b" db:"x"`
	}
	type badOption struct {
		Type string `json:"type"`
		A    string `json:"a" db:"x,text"`
	}
	assert.Panics(t, func() { newMetadataTable(metadataTable[metadataCodecScratch]{table: "scratch"}) })
	assert.Panics(t, func() { newMetadataTable(metadataTable[duplicate]{record: duplicate{Type: "d"}, table: "d"}) })
	assert.Panics(t, func() { newMetadataTable(metadataTable[badOption]{record: badOption{Type: "b"}, table: "b"}) })
	assert.Panics(t, func() {
		newMetadataTable(metadataTable[metadataCodecScratch]{record: metadataCodecScratch{Type: "scratch_record"}})
	})
}

func TestMetadataCodecEmbeddingSchemaParity(t *testing.T) {
	t.Parallel()
	for _, codec := range embeddingMetadataTables {
		list := reflect.ValueOf(codec).Elem().FieldByName("plan").Elem().FieldByName("list").String()
		if list == "" {
			continue
		}
		index := slices.IndexFunc(embeddingCatalogSchema, func(schema embeddingCatalogTableSchema) bool {
			return schema.name == codec.sqlTable()
		})
		require.GreaterOrEqual(t, index, 0, codec.kind())
		assert.ElementsMatch(t, embeddingCatalogSchema[index].columns, strings.Split(list, ","), codec.kind())
	}
}
