package docbank

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/pgtest"
)

func TestEmbeddedVaultLifecyclePostgres(t *testing.T) {
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	ctx := t.Context()
	sourceRoot := filepath.Join(t.TempDir(), "source")
	source, err := New(ctx, Config{Root: sourceRoot})
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
	catalog := PostgresCatalog{Schema: "documents", SchemaOwner: owner}
	_, err = SetupPostgres(ctx, setup, catalog)
	require.NoError(t, err)
	runtime := pgstoretest.RuntimeConfig(t, setup, catalog.schemaConfig())
	target := filepath.Join(t.TempDir(), "restored")
	nested := filepath.Join(sourceRoot, "nested-restore")
	refused, err := repository.RestorePostgres(ctx, setup, catalog, runtime, BackupRestoreOptions{Target: nested, SnapshotID: snapshot.ID})
	require.ErrorIs(t, err, ErrBackupRestoreTargetActive)
	require.Empty(t, refused)
	require.NoDirExists(t, nested)
	require.NoError(t, source.Close())

	ownerRuntime, err := pgx.ParseConfig(os.Getenv("DOCBANK_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	ownerRuntime.Database = runtime.Database
	refused, err = repository.RestorePostgres(ctx, setup, catalog, ownerRuntime, BackupRestoreOptions{Target: target, SnapshotID: snapshot.ID})
	require.ErrorContains(t, err, "separate runtime role")
	require.Empty(t, refused)
	require.NoDirExists(t, target)

	otherSetup, _ := pgtest.NewDatabase(t)
	otherRuntime := pgstoretest.RuntimeConfig(t, otherSetup, catalog.schemaConfig())
	refused, err = repository.RestorePostgres(ctx, setup, catalog, otherRuntime, BackupRestoreOptions{Target: target, SnapshotID: snapshot.ID})
	require.ErrorContains(t, err, "must name the same vault catalog")
	require.Empty(t, refused)
	require.NoDirExists(t, target)

	report, err := repository.RestorePostgres(ctx, setup, catalog, runtime,
		BackupRestoreOptions{Target: target, SnapshotID: snapshot.ID, Jobs: 1})
	require.NoError(t, err)
	require.Equal(t, PostgresRestoreReport{
		CatalogCommitted: true, SnapshotID: snapshot.ID, VaultID: source.ID(), Target: target,
	}, report)
	unbound := home.Layout{Root: t.TempDir()}
	require.NoError(t, os.MkdirAll(unbound.BlobsDir(), 0o700))
	lock, err := unbound.TryLockExclusive()
	require.NoError(t, err)
	require.NoError(t, lock.Release())
	unboundVault, err := OpenPostgres(ctx, Config{Root: unbound.Root}, catalog, runtime)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Nil(t, unboundVault)
	physical, err := packstore.NewLayout(unbound.BlobsDir(), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(t, err)
	require.NoFileExists(t, physical.OwnershipPath())

	layout := home.Layout{Root: target}
	open := func(ctx context.Context) (*Vault, error) {
		return OpenPostgres(ctx, Config{Root: target}, catalog, runtime)
	}
	vault, err := open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	require.NoFileExists(t, layout.DBPath())
	require.Empty(t, vault.SQLiteDriver())
	contender, err := open(ctx)
	require.ErrorIs(t, err, ErrVaultLocked)
	require.Nil(t, contender)
	require.ErrorIs(t, RebuildPostgresRestore(ctx, target, catalog, runtime), ErrVaultLocked)

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
	verified, err := vault.Verify(ctx, VerifyOptions{})
	require.NoError(t, err)
	require.Empty(t, verified.Problems)
	require.Equal(t, 3, verified.OK)
	require.NoError(t, vault.Close())
	require.NoError(t, RebuildPostgresRestore(ctx, target, catalog, runtime))

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
	sqliteTarget := filepath.Join(t.TempDir(), "recovered")
	_, err = reopened.RestoreBackup(ctx, recoveryRepository, BackupRestoreOptions{SnapshotID: recoverySnapshot.ID, Target: sqliteTarget, Jobs: 1})
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
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
