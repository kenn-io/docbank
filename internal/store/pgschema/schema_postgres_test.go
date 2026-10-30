package pgschema

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
)

func TestSetupPreservesIdentity(t *testing.T) {
	t.Parallel()
	db, owner := pgtest.NewDatabase(t)
	cfg := Config{Schema: "documents", SchemaOwner: owner}
	id, err := Setup(t.Context(), db, cfg)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil(), id)
	var rootID, childID int64
	var rootCreated string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT id,created_at FROM documents.nodes WHERE parent_id IS NULL`).Scan(&rootID, &rootCreated))
	const childTime = "2026-01-01T00:00:00.000000000Z"
	require.NoError(t, db.QueryRowContext(t.Context(), `WITH allocated AS (UPDATE documents.node_allocation SET high_water=high_water+1 WHERE singleton=1 RETURNING high_water)
		INSERT INTO documents.nodes(id,parent_id,name,kind,created_at,modified_at)
		SELECT high_water,$1,'kept','dir',$2,$2 FROM allocated RETURNING id`, rootID, childTime).Scan(&childID))
	var primary, epoch string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT store_id,ownership_epoch FROM documents.blob_stores WHERE role='primary'`).Scan(&primary, &epoch))
	again, err := Setup(t.Context(), db, cfg)
	require.NoError(t, err)
	assert.Equal(t, id, again)
	var keptPrimary, keptEpoch string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT store_id,ownership_epoch FROM documents.blob_stores WHERE role='primary'`).Scan(&keptPrimary, &keptEpoch))
	assert.Equal(t, primary, keptPrimary)
	assert.Equal(t, epoch, keptEpoch)
	var keptRootID, keptParent int64
	var keptCreated, keptName string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT id,created_at FROM documents.nodes WHERE parent_id IS NULL`).Scan(&keptRootID, &keptCreated))
	assert.Equal(t, rootID, keptRootID)
	assert.Equal(t, rootCreated, keptCreated)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT parent_id,name,created_at FROM documents.nodes WHERE id=$1`, childID).Scan(&keptParent, &keptName, &keptCreated))
	assert.Equal(t, rootID, keptParent)
	assert.Equal(t, "kept", keptName)
	assert.Equal(t, childTime, keptCreated)
	var highWater int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT high_water FROM documents.node_allocation WHERE singleton=1`).Scan(&highWater))
	assert.Equal(t, childID, highWater)
	var stored uuid.UUID
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT vault_uid FROM documents.vault_metadata WHERE singleton=1`).Scan(&stored))
	assert.Equal(t, id, stored)

	// A separate schema is a separate vault, even within the same database.
	other, err := Setup(t.Context(), db, Config{Schema: "other_documents", SchemaOwner: owner})
	require.NoError(t, err)
	assert.NotEqual(t, id, other)
}

func TestRuntimeCannotChangeSchema(t *testing.T) {
	t.Parallel()
	admin, _ := pgtest.NewDatabase(t)
	idSuffix := uuid.New()
	owner := fmt.Sprintf("Owner \"setup\" %x", idSuffix[:])
	role := fmt.Sprintf("runtime_%x", idSuffix[:])
	quotedOwner, quotedRole := pgx.Identifier{owner}.Sanitize(), pgx.Identifier{role}.Sanitize()
	quotedAssumed := pgx.Identifier{role + "_assumed"}.Sanitize()
	quotedIndirect := pgx.Identifier{role + "_indirect"}.Sanitize()
	_, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quotedOwner+" LOGIN PASSWORD 'fixture-owner-only'; CREATE ROLE "+quotedRole+" LOGIN NOINHERIT PASSWORD 'fixture-runtime-only'; CREATE ROLE "+quotedAssumed+" NOLOGIN NOINHERIT; CREATE ROLE "+quotedIndirect+" NOLOGIN NOINHERIT; GRANT "+quotedAssumed+" TO "+quotedRole+"; GRANT "+quotedIndirect+" TO "+quotedAssumed)
	require.NoError(t, err)
	t.Cleanup(func() {
		roles := strings.Join([]string{quotedRole, quotedAssumed, quotedIndirect, quotedOwner}, ", ")
		_, err := admin.ExecContext(context.Background(), "DROP OWNED BY "+roles+" CASCADE; DROP ROLE "+roles)
		require.NoError(t, err)
	})
	var database string
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database))
	_, err = admin.ExecContext(t.Context(), "GRANT CREATE ON DATABASE "+pgx.Identifier{database}.Sanitize()+" TO "+quotedOwner)
	require.NoError(t, err)
	connect := func(user, password, activeRole string) *sql.DB {
		c, err := pgx.ParseConfig(os.Getenv("DOCBANK_TEST_POSTGRES_DSN"))
		require.NoError(t, err)
		c.Database, c.User, c.Password = database, user, password
		if activeRole != "" {
			c.RuntimeParams["role"] = activeRole
		}
		db := stdlib.OpenDB(*c)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		return db
	}
	setup := connect(owner, "fixture-owner-only", "")
	runtime := connect(role, "fixture-runtime-only", "")
	cfg := Config{Schema: "documents", SchemaOwner: owner}
	id, err := Setup(t.Context(), setup, cfg)
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), "GRANT USAGE ON SCHEMA documents TO "+quotedRole+"; GRANT SELECT ON documents.vault_metadata TO "+quotedRole)
	require.NoError(t, err)
	actual, err := Validate(t.Context(), runtime, cfg)
	require.NoError(t, err)
	assert.Equal(t, id, actual)
	_, err = Setup(t.Context(), runtime, cfg)
	require.Error(t, err)
	_, err = Validate(t.Context(), setup, cfg)
	require.Error(t, err, "the owner must not be accepted as a runtime credential")
	for _, tc := range []struct{ name, grant, revoke string }{
		{"schema create", "GRANT CREATE ON SCHEMA documents TO ", "REVOKE CREATE ON SCHEMA documents FROM "},
		{"identity write", "GRANT UPDATE ON documents.vault_metadata TO ", "REVOKE UPDATE ON documents.vault_metadata FROM "},
		{"identity column write", "GRANT UPDATE(vault_uid) ON documents.vault_metadata TO ", "REVOKE UPDATE(vault_uid) ON documents.vault_metadata FROM "},
		{"owner membership", "GRANT " + quotedOwner + " TO ", "REVOKE " + quotedOwner + " FROM "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, recipient := range []struct{ name, role string }{
				{"direct", quotedRole}, {"assumed", quotedAssumed}, {"indirect", quotedIndirect},
			} {
				t.Run(recipient.name, func(t *testing.T) {
					_, err := admin.ExecContext(t.Context(), tc.grant+recipient.role)
					require.NoError(t, err)
					t.Cleanup(func() {
						_, err := admin.ExecContext(context.Background(), tc.revoke+recipient.role)
						require.NoError(t, err)
					})
					_, err = Validate(t.Context(), runtime, cfg)
					require.Error(t, err)
					_, err = admin.ExecContext(t.Context(), tc.revoke+recipient.role)
					require.NoError(t, err)
					actual, err := Validate(t.Context(), runtime, cfg)
					require.NoError(t, err)
					assert.Equal(t, id, actual)
				})
			}
		})
	}
	t.Run("membership without SET or INHERIT", func(t *testing.T) {
		_, err := admin.ExecContext(t.Context(), "GRANT UPDATE ON documents.vault_metadata TO "+quotedIndirect+"; GRANT "+quotedIndirect+" TO "+quotedAssumed+" WITH SET FALSE")
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := admin.ExecContext(context.Background(), "REVOKE UPDATE ON documents.vault_metadata FROM "+quotedIndirect+"; GRANT "+quotedIndirect+" TO "+quotedAssumed+" WITH SET TRUE")
			require.NoError(t, err)
		})
		actual, err := Validate(t.Context(), runtime, cfg)
		require.NoError(t, err)
		assert.Equal(t, id, actual)
	})
	t.Run("owner login with runtime role selected", func(t *testing.T) {
		_, err := admin.ExecContext(t.Context(), "GRANT "+quotedRole+" TO "+quotedOwner+"; REVOKE CREATE ON SCHEMA documents FROM "+quotedOwner+"; REVOKE INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER ON documents.vault_metadata FROM "+quotedOwner)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := admin.ExecContext(context.Background(), "REVOKE "+quotedRole+" FROM "+quotedOwner+"; GRANT CREATE ON SCHEMA documents TO "+quotedOwner+"; GRANT INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER ON documents.vault_metadata TO "+quotedOwner)
			require.NoError(t, err)
		})
		selected := connect(owner, "fixture-owner-only", role)
		_, err = Validate(t.Context(), selected, cfg)
		require.ErrorContains(t, err, "requires a separate runtime role", "selecting a lesser role must not hide the login's privileges")
	})
}

func TestSetupRefusesUnownedOrInvalidState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, prepare string }{
		{"empty schema", `CREATE SCHEMA documents`},
		{"unrelated schema", `CREATE SCHEMA documents; CREATE TABLE documents.kept(value text); INSERT INTO documents.kept VALUES('keep me')`},
		{"wrong singleton", `UPDATE documents.vault_metadata SET singleton=2`},
		{"multiple identities", `INSERT INTO documents.vault_metadata VALUES (2,'11111111-1111-4111-8111-111111111111')`},
		{"missing identity", `DELETE FROM documents.vault_metadata`},
		{"nil identity", `UPDATE documents.vault_metadata SET vault_uid='00000000-0000-0000-0000-000000000000'`},
		{"wrong table shape", `ALTER TABLE documents.vault_metadata DROP CONSTRAINT vault_metadata_pkey`},
		{"wrong schema owner", `ALTER SCHEMA documents OWNER TO pg_database_owner`},
		{"wrong table owner", `ALTER TABLE documents.vault_metadata OWNER TO pg_database_owner`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, owner := pgtest.NewDatabase(t)
			cfg := Config{Schema: "documents", SchemaOwner: owner}
			if tc.name != "empty schema" && tc.name != "unrelated schema" {
				_, err := Setup(t.Context(), db, cfg)
				require.NoError(t, err)
			}
			_, err := db.ExecContext(t.Context(), tc.prepare)
			require.NoError(t, err)
			_, err = Setup(t.Context(), db, cfg)
			require.Error(t, err)
			if tc.name == "unrelated schema" {
				var kept string
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT value FROM documents.kept`).Scan(&kept))
				assert.Equal(t, "keep me", kept)
			}
		})
	}
}

func TestSetupRollsBackFailedCommit(t *testing.T) {
	t.Parallel()
	db, owner := pgtest.NewDatabase(t)
	// A deferred trigger fails after setup has created and validated the identity.
	// This exercises the commit boundary, not just an early syntax failure.
	_, err := db.ExecContext(t.Context(), `
		CREATE FUNCTION public.reject_identity_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test deferred commit failure'; END $$;
		CREATE FUNCTION public.install_commit_failure() RETURNS event_trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF EXISTS (SELECT 1 FROM pg_event_trigger_ddl_commands()
		      WHERE object_identity='documents.vault_metadata' AND command_tag='CREATE TABLE') THEN
		    CREATE CONSTRAINT TRIGGER reject_commit AFTER INSERT ON documents.vault_metadata
		    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.reject_identity_commit();
		  END IF;
		END $$;
		CREATE EVENT TRIGGER install_failure ON ddl_command_end WHEN TAG IN ('CREATE TABLE')
		EXECUTE FUNCTION public.install_commit_failure();`)
	require.NoError(t, err)
	_, err = Setup(t.Context(), db, Config{Schema: "documents", SchemaOwner: owner})
	require.ErrorContains(t, err, "test deferred commit failure")
	var exists bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='documents')`).Scan(&exists))
	assert.False(t, exists, "failed commit must not publish a schema or identity")
	_, err = db.ExecContext(t.Context(), `DROP EVENT TRIGGER install_failure`)
	require.NoError(t, err)
	_, err = Setup(t.Context(), db, Config{Schema: "documents", SchemaOwner: owner})
	require.NoError(t, err)
}

func TestConcurrentSetup(t *testing.T) {
	t.Parallel()
	db, owner := pgtest.NewDatabase(t)
	cfg := Config{Schema: "documents", SchemaOwner: owner}
	const count = 6
	ids := make([]uuid.UUID, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() { ids[i], errs[i] = Setup(t.Context(), db, cfg) })
	}
	wg.Wait()
	for i := range count {
		require.NoError(t, errs[i])
		assert.Equal(t, ids[0], ids[i])
	}
}

func TestSetupCancellationDoesNotPublish(t *testing.T) {
	t.Parallel()
	db, owner := pgtest.NewDatabase(t)
	cfg := Config{Schema: "documents", SchemaOwner: owner}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('docbank:metadata:' || $1, 0))`, cfg.Schema)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Setup(ctx, db, cfg); done <- err }()
	// Cancel only after PostgreSQL reports that setup is waiting for our lock.
	// The timeout bounds a broken test; it does not decide when to cancel.
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event='advisory')`).Scan(&waiting)
		return err == nil && waiting
	}, 10*time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	var exists bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='documents')`).Scan(&exists))
	assert.False(t, exists)
	require.NoError(t, tx.Rollback())
	_, err = Setup(t.Context(), db, cfg)
	require.NoError(t, err)
}
