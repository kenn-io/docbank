package pgschema

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
)

func checkOwnerMigration(t *testing.T, owner, runtime *sql.DB, cfg Config) {
	t.Helper()
	ctx := t.Context()
	id, err := Validate(ctx, runtime, cfg)
	require.NoError(t, err)
	_, err = owner.ExecContext(ctx, `WITH allocated AS (
		UPDATE documents.node_allocation SET high_water=high_water+1 WHERE singleton=1 RETURNING high_water
	) INSERT INTO documents.nodes(id,parent_id,name,kind,created_at,modified_at)
		SELECT high_water,1,'keep me','dir',created_at,created_at FROM allocated CROSS JOIN documents.nodes WHERE parent_id IS NULL`)
	require.NoError(t, err)

	var originalVersion int
	var originalFingerprint string
	require.NoError(t, owner.QueryRowContext(ctx, `SELECT schema_version,schema_fingerprint
		FROM documents.vault_metadata WHERE singleton=1`).Scan(&originalVersion, &originalFingerprint))

	const update = `ALTER TABLE {{schema}}.nodes ADD COLUMN migration_probe text NOT NULL DEFAULT 'before upgrade';`
	const settle = `ALTER TABLE {{schema}}.nodes ALTER COLUMN migration_probe SET DEFAULT 'after upgrade';`
	next := appendTestMigrations(t, update, settle)
	// Fail only after observing the previous migration's DDL and history stamp.
	// This distinguishes rollback of the whole upgrade from rollback of one batch.
	fail := fmt.Sprintf(`DO $$
		BEGIN
			PERFORM migration_probe FROM {{schema}}.nodes WHERE id=2;
			IF NOT EXISTS(SELECT 1 FROM {{schema}}.vault_metadata
				WHERE singleton=1 AND schema_version=%d AND schema_fingerprint='%s') THEN
				RAISE EXCEPTION 'preceding migration history is missing';
			END IF;
			RAISE EXCEPTION 'second migration failed after first was recorded';
		END $$;`, originalVersion+1, next[originalVersion].fingerprint)
	broken := appendTestMigrations(t, update, fail)
	_, err = validate(ctx, runtime, cfg, next)
	require.ErrorContains(t, err, "run setup as the schema owner")
	_, err = setup(ctx, runtime, cfg, schemaSQL, next, true)
	require.ErrorContains(t, err, "requires the schema owner role")
	_, err = setup(ctx, owner, cfg, schemaSQL, next, false)
	require.ErrorContains(t, err, "run setup as the schema owner")
	_, err = setup(ctx, owner, cfg, schemaSQL, broken, true)
	require.ErrorContains(t, err, "second migration failed after first was recorded")
	var rolledBackVersion int
	var rolledBackFingerprint string
	require.NoError(t, owner.QueryRowContext(ctx, `SELECT schema_version,schema_fingerprint
		FROM documents.vault_metadata WHERE singleton=1`).Scan(&rolledBackVersion, &rolledBackFingerprint))
	require.Equal(t, originalVersion, rolledBackVersion)
	require.Equal(t, originalFingerprint, rolledBackFingerprint)
	kept, err := Validate(ctx, runtime, cfg)
	require.NoError(t, err)
	require.Equal(t, id, kept)
	var columnExists bool
	require.NoError(t, owner.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM information_schema.columns WHERE table_schema='documents'
		AND table_name='nodes' AND column_name='migration_probe')`).Scan(&columnExists))
	require.False(t, columnExists, "failed migration rolled back its preceding DDL")

	upgraded, err := setup(ctx, owner, cfg, schemaSQL, next, true)
	require.NoError(t, err)
	require.Equal(t, id, upgraded)
	again, err := setup(ctx, owner, cfg, schemaSQL, next, true)
	require.NoError(t, err)
	require.Equal(t, id, again)
	got, err := validate(ctx, runtime, cfg, next)
	require.NoError(t, err)
	require.Equal(t, id, got)
	var storedVersion int
	var evidence string
	require.NoError(t, owner.QueryRowContext(ctx, `SELECT v.schema_version,n.migration_probe
		FROM documents.vault_metadata v CROSS JOIN documents.nodes n
		WHERE n.id=2 AND n.name='keep me'`).Scan(&storedVersion, &evidence))
	require.Equal(t, len(next), storedVersion)
	require.Equal(t, "before upgrade", evidence)
	_, err = Validate(ctx, runtime, cfg)
	require.ErrorContains(t, err, "outside this build's supported range")
	_, err = Setup(ctx, owner, cfg)
	require.ErrorContains(t, err, "outside this build's supported range")

	// This test uses a synthetic next revision. Restore the fixture before the
	// parent test checks malformed version metadata.
	_, err = owner.ExecContext(ctx, `ALTER TABLE documents.nodes DROP COLUMN migration_probe`)
	require.NoError(t, err)
	_, err = owner.ExecContext(ctx, `UPDATE documents.vault_metadata
		SET schema_version=$1, schema_fingerprint=$2 WHERE singleton=1`, len(migrations), migrations[len(migrations)-1].fingerprint)
	require.NoError(t, err)
}

func appendTestMigrations(t *testing.T, statements ...string) []migration {
	t.Helper()
	files := fstest.MapFS{}
	for _, entry := range migrations {
		files["migrations/"+entry.name] = &fstest.MapFile{Data: []byte(entry.ddl)}
	}
	for i, ddl := range statements {
		files[fmt.Sprintf("migrations/%06d_test_upgrade.up.sql", len(migrations)+i+1)] = &fstest.MapFile{Data: []byte(ddl)}
	}
	chain, err := loadMigrations(files)
	require.NoError(t, err)
	return chain
}

func TestNumberedMigrationsMatchCanonicalSchema(t *testing.T) {
	t.Parallel()
	db, owner := pgtest.NewDatabase(t)
	ctx := t.Context()
	fresh := Config{Schema: "fresh_catalog", SchemaOwner: owner}
	numbered := Config{Schema: "numbered_catalog", SchemaOwner: owner}
	_, err := Setup(ctx, db, fresh)
	require.NoError(t, err)
	require.NotEmpty(t, migrations)
	_, err = setup(ctx, db, numbered, migrations[0].ddl, migrations[:1], false)
	require.NoError(t, err)
	_, err = Setup(ctx, db, numbered)
	require.NoError(t, err)
	require.Equal(t, installedCatalog(t, db, fresh.Schema), installedCatalog(t, db, numbered.Schema))
}

// Inspect installed objects so changes to SQL formatting and object creation
// order have no bearing on whether fresh and upgraded schemas agree.
func installedCatalog(t *testing.T, db *sql.DB, schema string) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
		WITH relations AS (
			SELECT c.* FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=$1
		), definitions(kind, name, definition) AS (
			SELECT 'relation', c.relname::text, concat_ws('|',c.relkind,c.relpersistence,c.relrowsecurity,c.relforcerowsecurity,c.reloptions)
			FROM relations c
			UNION ALL
			SELECT 'column', c.relname||'.'||a.attname, concat_ws('|',row_number() OVER (PARTITION BY c.oid ORDER BY a.attnum),format_type(a.atttypid,a.atttypmod),
				a.attnotnull,a.attidentity,a.attgenerated,a.attcollation,pg_get_expr(d.adbin,d.adrelid))
			FROM relations c JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid
			LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum
			WHERE a.attnum>0 AND NOT a.attisdropped AND c.relkind IN ('r','p','v','m')
			UNION ALL
			SELECT 'index', c.relname::text, pg_get_indexdef(c.oid) FROM relations c WHERE c.relkind IN ('i','I')
			UNION ALL
			SELECT 'constraint', c.relname||'.'||k.conname, pg_get_constraintdef(k.oid)
			FROM relations c JOIN pg_catalog.pg_constraint k ON k.conrelid=c.oid
			UNION ALL
			SELECT 'view', c.relname::text, pg_get_viewdef(c.oid) FROM relations c WHERE c.relkind IN ('v','m')
			UNION ALL
			SELECT 'function', p.proname||'('||pg_get_function_identity_arguments(p.oid)||')', pg_get_functiondef(p.oid)
			FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1
			UNION ALL
			SELECT 'trigger', c.relname||'.'||g.tgname, pg_get_triggerdef(g.oid)
			FROM relations c JOIN pg_catalog.pg_trigger g ON g.tgrelid=c.oid WHERE NOT g.tgisinternal
			UNION ALL
			SELECT 'sequence', c.relname::text,
				concat_ws('|',q.seqtypid,q.seqstart,q.seqincrement,q.seqmax,q.seqmin,q.seqcache,q.seqcycle)
			FROM relations c JOIN pg_catalog.pg_sequence q ON q.seqrelid=c.oid
		)
		SELECT kind, name, definition FROM definitions`, schema)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	result := map[string]string{}
	for rows.Next() {
		var kind, name, definition string
		require.NoError(t, rows.Scan(&kind, &name, &definition))
		key := kind + "/" + name
		require.NotContains(t, result, key)
		result[key] = strings.ReplaceAll(definition, schema, "vault_catalog")
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, result)
	return result
}
