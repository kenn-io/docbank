package backupapp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"
)

func TestPhysicalRestoreIntoPostgres(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*testing.T) *archiveFixture
		pack bool
	}{
		{"SQLite packed source", newArchiveFixture, true},
		{"PostgreSQL loose source", postgresArchiveFixture, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup, owner := pgtest.NewDatabase(t)
			ctx := t.Context()
			source := tc.open(t)
			var looseHash string
			if !tc.pack {
				size := blob.MaxPackedBlobBytes + 1
				var err error
				looseHash, _, err = source.blobs.WriteContext(ctx, io.LimitReader(zeroReader{}, size))
				require.NoError(t, err)
				_, err = source.metadata.CreateFile(ctx, source.metadata.RootID(), "large.bin", looseHash, size, "application/octet-stream")
				require.NoError(t, err)
			}
			node, err := source.metadata.NodeByPath(ctx, "/alpha.txt")
			require.NoError(t, err)
			auditPlan, err := source.metadata.PreviewInitialAudit(ctx, source.metadata.RootID(), "cli", nil)
			require.NoError(t, err)
			_, err = source.metadata.EnableInitialAudit(ctx, auditPlan)
			require.NoError(t, err)
			audit, err := source.metadata.AuditStatus(ctx, nil)
			require.NoError(t, err)
			incarnation, err := source.metadata.CurrentProcessingIncarnation(ctx)
			require.NoError(t, err)
			sourceOwnership := store.NewPackCatalog(source.metadata).PrimaryOwnership()
			if tc.pack {
				packed, err := source.blobs.Maintainer().Pack(ctx, packstore.PackOptions{})
				require.NoError(t, err)
				require.Equal(t, 2, packed.BlobsPacked)
			}
			repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
			require.NoError(t, err)
			snapshot, err := backupapp.Create(ctx, repo, "test-version", source.metadata, source.blobs, backup.CreateOptions{Jobs: 2})
			require.NoError(t, err)
			source.discard()

			cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
			runtime := pgstoretest.RuntimeConfig(t, setup, cfg)
			target := filepath.Join(t.TempDir(), "restored")
			restored, err := backupapp.RestorePostgres(ctx, repo, "test-version", store.DefaultSQLiteDriver(), backupapp.PostgresRestoreTarget{Owner: setup, Runtime: runtime, Schema: cfg},
				backup.RestoreOptions{TargetDir: target, SnapshotID: snapshot.SnapshotID, Jobs: 2})
			require.NoError(t, err)
			require.Equal(t, snapshot.SnapshotID, restored.SnapshotID)
			require.Equal(t, sourceOwnership.Vault, restored.VaultID)
			require.NoFileExists(t, filepath.Join(target, "docbank.db"))

			metadata, err := store.OpenPostgres(ctx, runtime, cfg, target)
			require.NoError(t, err)
			defer func() { require.NoError(t, metadata.Close()) }()
			require.NoError(t, metadata.ValidateMetadata(ctx))
			actualAudit, err := metadata.AuditStatus(ctx, nil)
			require.NoError(t, err)
			require.Equal(t, audit, actualAudit)
			newIncarnation, err := metadata.CurrentProcessingIncarnation(ctx)
			require.NoError(t, err)
			require.NotEqual(t, incarnation, newIncarnation)
			got, err := metadata.NodeByPath(ctx, "/alpha.txt")
			require.NoError(t, err)
			require.Equal(t, node.ID, got.ID)
			require.Equal(t, node.CurrentVersionID, got.CurrentVersionID)
			catalog := store.NewPackCatalog(metadata)
			require.NotEqual(t, sourceOwnership.Store, catalog.PrimaryStoreID())
			physical, err := blob.NewExistingReader(catalog, filepath.Join(target, "blobs"), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, physical.Close()) }()
			entries, err := catalog.ListIndexed(ctx)
			require.NoError(t, err)
			require.Len(t, entries, 2)
			if looseHash != "" {
				content, err := metadata.PhysicalContent(ctx, looseHash)
				require.NoError(t, err)
				require.Equal(t, "loose", content.Kind)
				require.Equal(t, "zstd", content.Encoding)
				reader, err := physical.OpenContext(ctx, looseHash)
				require.NoError(t, err)
				hash := sha256.New()
				size, err := io.Copy(hash, reader)
				require.NoError(t, reader.Close())
				require.NoError(t, err)
				require.Equal(t, blob.MaxPackedBlobBytes+1, size)
				require.Equal(t, looseHash, hex.EncodeToString(hash.Sum(nil)))
			}
			for name, want := range source.content {
				n, err := metadata.NodeByPath(ctx, "/"+name)
				require.NoError(t, err)
				reader, err := physical.OpenContext(ctx, n.BlobHash)
				require.NoError(t, err)
				data, err := io.ReadAll(reader)
				require.NoError(t, reader.Close())
				require.NoError(t, err)
				require.Equal(t, want, string(data))
			}
		})
	}
}

func TestPhysicalRestorePublicationCollisionPostgres(t *testing.T) {
	setup, owner := pgtest.NewDatabase(t)
	ctx := t.Context()
	source := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(ctx, repo, "test-version", source.metadata, source.blobs, backup.CreateOptions{})
	require.NoError(t, err)

	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	runtime := pgstoretest.RuntimeConfig(t, setup, cfg)
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	var once sync.Once
	var collisionErr error
	result, err := backupapp.RestorePostgres(ctx, repo, "test-version", store.DefaultSQLiteDriver(), backupapp.PostgresRestoreTarget{Owner: setup, Runtime: runtime, Schema: cfg},
		backup.RestoreOptions{TargetDir: target, Progress: func(backup.ProgressEvent) {
			once.Do(func() {
				if collisionErr = os.Mkdir(target, 0o700); collisionErr == nil {
					collisionErr = os.WriteFile(filepath.Join(target, "keep"), []byte("existing data"), 0o600)
				}
			})
		}})
	require.NoError(t, collisionErr)
	require.ErrorContains(t, err, "installing restored vault directory")
	require.Nil(t, result)
	data, err := os.ReadFile(filepath.Join(target, "keep"))
	require.NoError(t, err)
	require.Equal(t, "existing data", string(data))

	retry := filepath.Join(parent, "retry")
	_, err = backupapp.RestorePostgres(ctx, repo, "test-version", store.DefaultSQLiteDriver(), backupapp.PostgresRestoreTarget{Owner: setup, Runtime: runtime, Schema: cfg}, backup.RestoreOptions{TargetDir: retry})
	require.NoError(t, err)
	restored, err := store.OpenPostgres(ctx, runtime, cfg, retry)
	require.NoError(t, err)
	defer func() { require.NoError(t, restored.Close()) }()
	require.Equal(t, source.metadata.VaultID(), restored.VaultID())
}

type abortInstalledRestoreCommit struct{ target string }

func (tr abortInstalledRestoreCommit) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(query.SQL, "commit") {
		if _, err := os.Stat(tr.target); err == nil {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			return cancelled
		}
	}
	return ctx
}

func (abortInstalledRestoreCommit) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPhysicalRestoreAbortedCommitPostgres(t *testing.T) {
	t.Run("same snapshot", func(t *testing.T) { checkPhysicalRestoreAbortedCommit(t, false) })
	t.Run("different snapshot", func(t *testing.T) { checkPhysicalRestoreAbortedCommit(t, true) })
}

func checkPhysicalRestoreAbortedCommit(t *testing.T, changeSnapshot bool) {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	ctx := t.Context()
	source := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	snapshot, err := backupapp.Create(ctx, repo, "test-version", source.metadata, source.blobs, backup.CreateOptions{})
	require.NoError(t, err)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	runtime := pgstoretest.RuntimeConfig(t, setup, cfg)
	target := filepath.Join(t.TempDir(), "restored")

	conn, err := setup.Conn(ctx)
	require.NoError(t, err)
	var ownerConfig *pgx.ConnConfig
	require.NoError(t, conn.Raw(func(raw any) error {
		pgxConn, ok := raw.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected PostgreSQL driver %T", raw)
		}
		ownerConfig = pgxConn.Conn().Config().Copy()
		return nil
	}))
	require.NoError(t, conn.Close())
	ownerConfig.Tracer = abortInstalledRestoreCommit{target: target}
	writer := stdlib.OpenDB(*ownerConfig)
	defer func() { require.NoError(t, writer.Close()) }()
	result, err := backupapp.RestorePostgres(ctx, repo, "test-version", store.DefaultSQLiteDriver(),
		backupapp.PostgresRestoreTarget{Owner: writer, Runtime: runtime, Schema: cfg}, backup.RestoreOptions{TargetDir: target, SnapshotID: snapshot.SnapshotID})
	require.ErrorContains(t, err, "restore directory was installed")
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, result)

	restored, err := store.OpenPostgres(ctx, runtime, cfg, target)
	require.NoError(t, err)
	defer func() { require.NoError(t, restored.Close()) }()
	require.NotEqual(t, result.VaultID, restored.VaultID())
	_, err = restored.NodeByPath(ctx, "/alpha.txt")
	require.ErrorIs(t, err, store.ErrNotFound)
	physical, err := blob.NewExistingReader(store.NewPackCatalog(restored), filepath.Join(target, "blobs"), nil)
	require.ErrorIs(t, err, packstore.ErrStoreFenced)
	require.Nil(t, physical)

	want := "alpha backup"
	if changeSnapshot {
		want = "changed after the first snapshot"
		node, err := source.metadata.NodeByPath(ctx, "/alpha.txt")
		require.NoError(t, err)
		hash, size, err := source.blobs.WriteContext(ctx, strings.NewReader(want))
		require.NoError(t, err)
		_, _, err = source.metadata.ReplaceContent(ctx, node.ID, node.Revision, hash, size, node.MimeType)
		require.NoError(t, err)
		retrySnapshot, err := backupapp.Create(ctx, repo, "test-version", source.metadata, source.blobs, backup.CreateOptions{})
		require.NoError(t, err)
		require.NotEqual(t, snapshot.SnapshotID, retrySnapshot.SnapshotID)
		snapshot = retrySnapshot
	}

	retry := filepath.Join(filepath.Dir(target), "retry")
	committed, err := backupapp.RestorePostgres(ctx, repo, "test-version", store.DefaultSQLiteDriver(),
		backupapp.PostgresRestoreTarget{Owner: setup, Runtime: runtime, Schema: cfg},
		backup.RestoreOptions{TargetDir: retry, SnapshotID: snapshot.SnapshotID})
	require.NoError(t, err)
	require.Equal(t, result.VaultID, committed.VaultID)
	require.Equal(t, snapshot.SnapshotID, committed.SnapshotID)
	live, err := store.OpenPostgres(ctx, runtime, cfg, retry)
	require.NoError(t, err)
	defer func() { require.NoError(t, live.Close()) }()
	catalog := store.NewPackCatalog(live)
	physical, err = blob.NewExistingReader(catalog, filepath.Join(target, "blobs"), nil)
	require.ErrorIs(t, err, packstore.ErrStoreFenced, "a committed retry must not authorize the interrupted directory")
	require.Nil(t, physical)
	physical, err = blob.NewExistingReader(catalog, filepath.Join(retry, "blobs"), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, physical.Close()) }()
	node, err := live.NodeByPath(ctx, "/alpha.txt")
	require.NoError(t, err)
	reader, err := physical.OpenContext(ctx, node.BlobHash)
	require.NoError(t, err)
	content, err := io.ReadAll(reader)
	require.NoError(t, reader.Close())
	require.NoError(t, err)
	require.Equal(t, want, string(content))
}

func TestPhysicalRestoreRetriesDerivedIndexesPostgres(t *testing.T) {
	setup, owner := pgtest.NewDatabase(t)
	ctx := t.Context()
	source := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(ctx, repo, "test-version", source.metadata, source.blobs, backup.CreateOptions{})
	require.NoError(t, err)

	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	runtime := pgstoretest.RuntimeConfig(t, setup, cfg)
	role := pgx.Identifier{runtime.User}.Sanitize()
	_, err = setup.ExecContext(ctx, `REVOKE INSERT ON documents.document_event_state FROM `+role)
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "restored")
	result, err := backupapp.RestorePostgres(ctx, repo, "test-version", store.DefaultSQLiteDriver(),
		backupapp.PostgresRestoreTarget{Owner: setup, Runtime: runtime, Schema: cfg}, backup.RestoreOptions{TargetDir: target})
	require.ErrorContains(t, err, "PostgreSQL restore committed; rebuilding projections")
	require.ErrorContains(t, err, "permission denied for table document_event_state")
	require.NotNil(t, result)
	require.Equal(t, source.metadata.VaultID(), result.VaultID)

	_, err = setup.ExecContext(ctx, `GRANT INSERT ON documents.document_event_state TO `+role)
	require.NoError(t, err)
	require.NoError(t, backupapp.RebuildPostgresRestoreProjections(ctx, runtime, cfg, target))
	restored, err := store.OpenPostgres(ctx, runtime, cfg, target)
	require.NoError(t, err)
	defer func() { require.NoError(t, restored.Close()) }()
	node, err := restored.NodeByPath(ctx, "/alpha.txt")
	require.NoError(t, err)
	_, head, err := restored.DocumentPeopleForVersion(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, "published", head.State)
}

func TestBackupRestoresRenditionBytesAndSearchPostgres(t *testing.T) {
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	runtime := pgstoretest.RuntimeConfig(t, setup, cfg)
	checkBackupRestoresRenditionBytesAndSearch(t, func(t *testing.T, repo *backup.Repo, target string) *store.Store {
		t.Helper()
		_, err := backupapp.RestorePostgres(t.Context(), repo, "test-version", store.DefaultSQLiteDriver(),
			backupapp.PostgresRestoreTarget{Owner: setup, Runtime: runtime, Schema: cfg},
			backup.RestoreOptions{TargetDir: target})
		require.NoError(t, err)
		restored, err := store.OpenPostgres(t.Context(), runtime, cfg, target)
		require.NoError(t, err)
		return restored
	})
}
