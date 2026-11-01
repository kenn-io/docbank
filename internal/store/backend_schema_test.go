package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type backendColumn struct {
	Kind            string
	Required        bool
	PrimaryPosition int
}

type backendIndex struct {
	Columns []string
	Unique  bool
	Partial bool
}

type backendTable struct {
	Columns     map[string]backendColumn
	ForeignKeys []string
	Indexes     map[string]backendIndex
}

// Inspect the installed schemas, not a second copy of their DDL. Types are
// compared by logical family; engine identity and search structures differ.
// Predicate/expression SQL and deferred-FK timing need operation tests.
func checkBackendSchemaPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	sqlite := newTestStore(t)
	postgres := newPostgresQueryStore(t)
	left := describeBackendSchema(t, sqlite)
	right := describeBackendSchema(t, postgres)
	// SQLite records its released layout; PostgreSQL is not released yet.
	require.Contains(t, left["vault_metadata"].Columns, "schema_version")
	assert.NotContains(t, right["vault_metadata"].Columns, "schema_version")
	delete(left["vault_metadata"].Columns, "schema_version")
	// PostgreSQL's transactional node counter implements SQLite's sqlite_sequence.
	require.Contains(t, right, "node_allocation")
	delete(right, "node_allocation")
	for table, reason := range sqliteOnlyBackendTables {
		assert.Contains(t, left, table, "stale table exception: %s", reason)
		assert.NotContains(t, right, table, "remove resolved table exception: %s", reason)
		delete(left, table)
	}
	for name, extra := range postgresOnlyBackendIndexes {
		require.Contains(t, right, extra.table)
		assert.NotContains(t, left[extra.table].Indexes, name, "remove resolved index exception")
		assert.Equal(t, backendIndex{Columns: []string{extra.column}}, right[extra.table].Indexes[name], "PostgreSQL cascade index %s", name)
		delete(right[extra.table].Indexes, name)
	}
	for table, expected := range left {
		actual, ok := right[table]
		if !assert.True(t, ok, "PostgreSQL missing table %s", table) {
			continue
		}
		assert.Equal(t, expected.Columns, actual.Columns, "columns of %s", table)
		assert.Equal(t, expected.ForeignKeys, actual.ForeignKeys, "foreign keys of %s", table)
		for name, index := range expected.Indexes {
			other, ok := actual.Indexes[name]
			if !ok {
				if _, deferred := deferredBackendIndexes[name]; deferred {
					continue
				}
				t.Errorf("PostgreSQL missing index %s on %s", name, table)
				continue
			}
			assert.Equal(t, index, other, "index %s on %s", name, table)
		}
		for name := range actual.Indexes {
			_, ok := expected.Indexes[name]
			assert.True(t, ok, "SQLite missing index %s on %s", name, table)
		}
	}
	for table := range right {
		_, ok := left[table]
		assert.True(t, ok, "SQLite missing table %s", table)
	}
	// An exception must disappear when its index is ported; it cannot mask a
	// later removal of an index that was already qualified.
	for name, table := range deferredBackendIndexes {
		require.Contains(t, left, table, "stale deferred index %s", name)
		require.Contains(t, right, table, "stale deferred index %s", name)
		assert.Contains(t, left[table].Indexes, name)
		assert.NotContains(t, right[table].Indexes, name, "remove resolved exception")
	}
}

func describeBackendSchema(t *testing.T, s *Store) map[string]backendTable {
	t.Helper()
	ctx := t.Context()
	query := `SELECT name FROM pragma_table_list WHERE schema='main' AND type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`
	if s.postgresSchema != "" {
		query = `SELECT relname FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relkind='r' ORDER BY relname`
	}
	var tables []string
	require.NoError(t, queryRows(ctx, s.db, query, nil, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		tables = append(tables, name)
		return nil
	}))
	result := make(map[string]backendTable, len(tables))
	for _, table := range tables {
		result[table] = backendTable{
			Columns:     describeBackendColumns(t, s, table),
			ForeignKeys: describeBackendForeignKeys(t, s, table),
			Indexes:     describeBackendIndexes(t, s, table),
		}
	}
	if s.postgresSchema != "" {
		var wrong []string
		require.NoError(t, queryRows(ctx, s.db, `SELECT r.relname||'.'||a.attname
   FROM pg_attribute a JOIN pg_class r ON r.oid=a.attrelid
   JOIN pg_collation c ON c.oid=a.attcollation
   WHERE r.relnamespace=current_schema()::regnamespace AND r.relkind='r'
    AND a.attnum>0 AND NOT a.attisdropped AND c.collname<>'C'`, nil, func(rows *sql.Rows) error {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			wrong = append(wrong, name)
			return nil
		}))
		assert.Empty(t, wrong, "PostgreSQL text columns must retain byte ordering")
	}
	return result
}

func describeBackendColumns(t *testing.T, s *Store, table string) map[string]backendColumn {
	t.Helper()
	query := `SELECT name,type,"notnull",pk FROM pragma_table_info($1)`
	if s.postgresSchema != "" {
		query = `SELECT a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,
   COALESCE(array_position(c.conkey,a.attnum),0)
   FROM pg_attribute a JOIN pg_class r ON r.oid=a.attrelid
   LEFT JOIN pg_constraint c ON c.conrelid=r.oid AND c.contype='p'
   WHERE r.relnamespace=current_schema()::regnamespace AND r.relname=$1
    AND a.attnum>0 AND NOT a.attisdropped`
	}
	columns := map[string]backendColumn{}
	require.NoError(t, queryRows(t.Context(), s.db, query, []any{table}, func(rows *sql.Rows) error {
		var name string
		var column backendColumn
		if err := rows.Scan(&name, &column.Kind, &column.Required, &column.PrimaryPosition); err != nil {
			return err
		}
		switch strings.ToLower(column.Kind) {
		case "integer", "bigint", "smallint":
			column.Kind = "integer"
		case "text", "uuid":
			column.Kind = "text"
		case "blob", "bytea":
			column.Kind = "bytes"
		case "real", "double precision":
			column.Kind = "real"
		default:
			return fmt.Errorf("unclassified column type %s.%s: %s", table, name, column.Kind)
		}
		// SQLite rowid-table TEXT primary keys report nullable; Go never writes a
		// null identity. WITHOUT ROWID and PostgreSQL enforce the logical rule.
		if column.PrimaryPosition != 0 {
			column.Required = true
		}
		columns[name] = column
		return nil
	}))
	return columns
}

func describeBackendForeignKeys(t *testing.T, s *Store, table string) []string {
	t.Helper()
	query := `SELECT CAST(id AS TEXT),"from","table","to",on_delete,on_update FROM pragma_foreign_key_list($1) ORDER BY id,seq`
	if s.postgresSchema != "" {
		query = `SELECT c.oid::text,a.attname,rr.relname,ra.attname,c.confdeltype::text,c.confupdtype::text
   FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid JOIN pg_class rr ON rr.oid=c.confrelid
   JOIN LATERAL unnest(c.conkey,c.confkey) WITH ORDINALITY AS k(local_id,remote_id,position) ON true
   JOIN pg_attribute a ON a.attrelid=r.oid AND a.attnum=k.local_id
   JOIN pg_attribute ra ON ra.attrelid=rr.oid AND ra.attnum=k.remote_id
   WHERE r.relnamespace=current_schema()::regnamespace AND r.relname=$1 AND c.contype='f'
   ORDER BY c.oid,k.position`
	}
	groups := map[string][]string{}
	require.NoError(t, queryRows(t.Context(), s.db, query, []any{table}, func(rows *sql.Rows) error {
		var id, from, target, to, onDelete, onUpdate string
		if err := rows.Scan(&id, &from, &target, &to, &onDelete, &onUpdate); err != nil {
			return err
		}
		action := func(value string) string {
			switch value {
			case "a":
				return "NO ACTION"
			case "r":
				return "RESTRICT"
			case "c":
				return "CASCADE"
			case "n":
				return "SET NULL"
			case "d":
				return "SET DEFAULT"
			}
			return value
		}
		groups[id] = append(groups[id], from+"->"+target+"."+to+" delete="+action(onDelete)+" update="+action(onUpdate))
		return nil
	}))
	var keys []string
	for _, group := range groups {
		keys = append(keys, strings.Join(group, ";"))
	}
	slices.Sort(keys)
	return keys
}

func describeBackendIndexes(t *testing.T, s *Store, table string) map[string]backendIndex {
	t.Helper()
	if s.postgresSchema != "" {
		return describePostgresIndexes(t, s, table)
	}
	type indexHeader struct {
		name    string
		unique  bool
		origin  string
		partial bool
	}
	var headers []indexHeader
	require.NoError(t, queryRows(t.Context(), s.db,
		`SELECT name,"unique",origin,partial FROM pragma_index_list($1) WHERE origin<>'pk'`, []any{table}, func(rows *sql.Rows) error {
			var h indexHeader
			if err := rows.Scan(&h.name, &h.unique, &h.origin, &h.partial); err != nil {
				return err
			}
			headers = append(headers, h)
			return nil
		}))
	indexes := map[string]backendIndex{}
	for _, h := range headers {
		index := backendIndex{Unique: h.unique, Partial: h.partial}
		require.NoError(t, queryRows(t.Context(), s.db,
			`SELECT COALESCE(name,'<expression>'),"desc" FROM pragma_index_xinfo($1) WHERE "key"=1 ORDER BY seqno`, []any{h.name}, func(rows *sql.Rows) error {
				var name string
				var descending bool
				if err := rows.Scan(&name, &descending); err != nil {
					return err
				}
				if descending {
					name += " DESC"
				}
				index.Columns = append(index.Columns, name)
				return nil
			}))
		name := h.name
		if h.origin == "u" {
			name = "unique:" + strings.Join(index.Columns, ",")
		}
		indexes[name] = index
	}
	return indexes
}

func describePostgresIndexes(t *testing.T, s *Store, table string) map[string]backendIndex {
	t.Helper()
	indexes := map[string]backendIndex{}
	require.NoError(t, queryRows(t.Context(), s.db, `
  SELECT ir.relname, i.indisunique, i.indpred IS NOT NULL, c.contype IS NOT NULL,
   COALESCE(a.attname,'<expression>'), (i.indoption[k.position-1] & 1)=1
  FROM pg_index i JOIN pg_class r ON r.oid=i.indrelid JOIN pg_class ir ON ir.oid=i.indexrelid
  LEFT JOIN pg_constraint c ON c.conindid=i.indexrelid AND c.contype='u'
  JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum,position) ON k.position<=i.indnkeyatts
  LEFT JOIN pg_attribute a ON a.attrelid=r.oid AND a.attnum=k.attnum
  WHERE r.relnamespace=current_schema()::regnamespace AND r.relname=$1 AND NOT i.indisprimary
  ORDER BY ir.relname,k.position`, []any{table}, func(rows *sql.Rows) error {
		var name, column string
		var unique, partial, constraint, descending bool
		if err := rows.Scan(&name, &unique, &partial, &constraint, &column, &descending); err != nil {
			return err
		}
		// Prefix now, replace with ordered columns after all rows are collected.
		if constraint {
			name = "constraint:" + name
		}
		index := indexes[name]
		index.Unique = unique
		index.Partial = partial
		if descending {
			column += " DESC"
		}
		index.Columns = append(index.Columns, column)
		indexes[name] = index
		return nil
	}))
	for name, index := range indexes {
		if strings.HasPrefix(name, "constraint:") {
			delete(indexes, name)
			indexes["unique:"+strings.Join(index.Columns, ",")] = index
		}
	}
	return indexes
}
