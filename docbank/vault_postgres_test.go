package docbank

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/docbank/internal/version"
)

func TestEmbeddedVaultLifecyclePostgres(t *testing.T) {
	setup, owner := pgtest.NewDatabase(t)
	ctx := t.Context()
	source, err := New(ctx, Config{Root: filepath.Join(t.TempDir(), "source")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })

	alpha, err := source.Create(ctx, "/alpha.txt", strings.NewReader("alpha"), CreateOptions{
		MediaType: "text/plain", Expected: contentIdentity([]byte("alpha")),
	})
	require.NoError(t, err)
	beta, err := source.Create(ctx, "/beta.txt", strings.NewReader("beta"), CreateOptions{
		MediaType: "text/plain", Expected: contentIdentity([]byte("beta")),
	})
	require.NoError(t, err)
	plan, err := source.metadata.PreviewInitialAudit(ctx, source.metadata.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = source.metadata.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	packed, err := source.Pack(ctx, PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, packed.BlobsPacked)

	repository, err := InitBackupRepository(filepath.Join(t.TempDir(), "source-backups"))
	require.NoError(t, err)
	snapshot, err := source.CreateBackup(ctx, repository, BackupOptions{Jobs: 1})
	require.NoError(t, err)
	require.NoError(t, source.Close())

	schema := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	runtime := pgstoretest.RuntimeConfig(t, setup, schema)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.RestorePostgres(ctx, repository.repo, version.Version, store.DefaultSQLiteDriver(),
		backupapp.PostgresRestoreTarget{Owner: setup, Runtime: runtime, Schema: schema},
		backup.RestoreOptions{TargetDir: target, SnapshotID: snapshot.ID, Jobs: 1})
	require.NoError(t, err)
	config, blobOptions, err := normalizeVaultConfig(ctx, Config{Root: target})
	require.NoError(t, err)
	layout := home.Layout{Root: config.Root}
	open := func(ctx context.Context) (*Vault, error) {
		return openVaultWithCatalog(ctx, config, blobOptions, layout, layout.OpenAndLockExclusive,
			func(ctx context.Context) (*store.Store, error) {
				return store.OpenPostgres(ctx, runtime, schema, layout.Root)
			})
	}
	vault, err := open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	require.NoFileExists(t, layout.DBPath())
	contender, err := open(ctx)
	require.ErrorIs(t, err, ErrVaultLocked)
	require.Nil(t, contender)

	moved, err := vault.BatchMove(ctx, []BatchMoveItem{
		{SourcePath: "/alpha.txt", DestinationPath: "/beta.txt"},
		{NodeID: beta.Node.ID, IfRevision: beta.Node.Revision, DestinationPath: "/alpha.txt"},
	})
	require.NoError(t, err)
	require.Len(t, moved, 2)
	require.Equal(t, alpha.Node.ID, moved[0].Node.ID)
	require.Equal(t, "/alpha.txt", moved[0].FromPath)
	require.Equal(t, "/beta.txt", moved[0].Path)
	require.Equal(t, beta.Node.ID, moved[1].Node.ID)
	require.Equal(t, "/beta.txt", moved[1].FromPath)
	require.Equal(t, "/alpha.txt", moved[1].Path)

	created, err := vault.Create(ctx, "/new.txt", strings.NewReader("new contents"), CreateOptions{
		MediaType: "text/plain",
		Expected:  contentIdentity([]byte("new contents")),
		Provenance: &ProvenanceSource{
			Kind: "archive", Description: "test archive", Reference: "objects/new.txt",
		},
	})
	require.NoError(t, err)
	require.NoError(t, vault.metadata.ValidateMetadata(ctx))
	checkEmbeddedVaultWalk(t, vault, moved, created.Node)
	report, err := vault.Verify(ctx, VerifyOptions{})
	require.NoError(t, err)
	require.Empty(t, report.Problems)
	require.Equal(t, 3, report.OK)
	require.NoError(t, vault.Close())

	reopened, err := open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	checkEmbeddedContent(t, reopened, "/alpha.txt", beta.Node.ID, "beta")
	checkEmbeddedContent(t, reopened, "/beta.txt", alpha.Node.ID, "alpha")
	checkEmbeddedContent(t, reopened, "/new.txt", created.Node.ID, "new contents")
	require.NoFileExists(t, layout.DBPath())

	recoveryRepository, err := InitBackupRepository(filepath.Join(t.TempDir(), "recovery-backups"))
	require.NoError(t, err)
	recoverySnapshot, err := reopened.CreateBackup(ctx, recoveryRepository, BackupOptions{Jobs: 1})
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
	sqliteTarget := filepath.Join(t.TempDir(), "recovered")
	_, err = recoveryRepository.Restore(ctx, BackupRestoreOptions{SnapshotID: recoverySnapshot.ID, Target: sqliteTarget, Jobs: 1})
	require.NoError(t, err)
	recovered, err := New(ctx, Config{Root: sqliteTarget})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, recovered.Close()) })
	require.NoError(t, recovered.metadata.ValidateMetadata(ctx))
	checkEmbeddedContent(t, recovered, "/alpha.txt", beta.Node.ID, "beta")
	checkEmbeddedContent(t, recovered, "/beta.txt", alpha.Node.ID, "alpha")
	checkEmbeddedContent(t, recovered, "/new.txt", created.Node.ID, "new contents")
}

func checkEmbeddedVaultWalk(t *testing.T, vault *Vault, moved []BatchMoveReceipt, created Node) {
	t.Helper()
	ctx := t.Context()
	root, err := vault.Stat(ctx, "/")
	require.NoError(t, err)
	walker, err := vault.Walk(ctx, "/", WalkOptions{PageSize: 1})
	require.NoError(t, err)
	defer func() { require.NoError(t, walker.Close()) }()
	for _, want := range []WalkEntry{
		{Path: "/", Node: root}, {Path: "/alpha.txt", Node: moved[1].Node},
		{Path: "/beta.txt", Node: moved[0].Node}, {Path: "/new.txt", Node: created},
	} {
		page, err := walker.Next(ctx)
		require.NoError(t, err)
		require.Equal(t, []WalkEntry{want}, page)
	}
	_, err = walker.Next(ctx)
	require.ErrorIs(t, err, io.EOF)
}

func checkEmbeddedContent(t *testing.T, vault *Vault, path string, nodeID int64, want string) {
	t.Helper()
	content, err := vault.OpenContent(t.Context(), path)
	require.NoError(t, err)
	defer func() { require.NoError(t, content.Reader.Close()) }()
	data, err := io.ReadAll(content.Reader)
	require.NoError(t, err)
	require.Equal(t, nodeID, content.Node.ID)
	require.Equal(t, want, string(data))
}
