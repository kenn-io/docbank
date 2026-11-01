package store

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkTrashRestorePostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, audited := range []bool{false, true} {
		name := "ordinary"
		if audited {
			name = "audited"
		}
		t.Run(name, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			if audited {
				plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "cli", nil)
				require.NoError(t, err)
				_, err = s.EnableInitialAudit(ctx, plan)
				require.NoError(t, err)
			}
			parent, _, err := s.MkdirPath(ctx, "/Documents")
			require.NoError(t, err)
			folder, err := s.Mkdir(ctx, parent.ID, "Drafts")
			require.NoError(t, err)
			file, err := s.CreateFile(ctx, folder.ID, "note.txt", fakeHash("a1"), 4, "text/plain")
			require.NoError(t, err)
			separate, err := s.CreateFile(ctx, folder.ID, "old.txt", fakeHash("a2"), 4, "text/plain")
			require.NoError(t, err)
			separate, _, err = s.Trash(ctx, separate.ID, separate.Revision)
			require.NoError(t, err)
			folder, err = s.NodeByID(ctx, folder.ID)
			require.NoError(t, err)
			trashed, original, err := s.TrashPathRevision(ctx, "/Documents/Drafts", folder.Revision)
			require.NoError(t, err)
			require.Equal(t, "/Documents/Drafts", original)
			require.Equal(t, new(s.RootID()), trashed.ParentID)
			child, err := s.NodeByID(ctx, file.ID)
			require.NoError(t, err)
			require.Equal(t, trashed.TrashedAt, child.TrashedAt)
			require.Equal(t, file.Revision+1, child.Revision)
			_, err = s.NodeByPath(ctx, original)
			require.ErrorIs(t, err, ErrNotFound)
			roots, err := s.TrashedRoots(ctx)
			require.NoError(t, err)
			require.Len(t, roots, 2)
			page, total, err := s.TrashedRootsPage(ctx, 1, 0)
			require.NoError(t, err)
			require.Equal(t, 2, total)
			require.Len(t, page, 1)
			require.Equal(t, folder.ID, page[0].ID)
			restored, path, err := s.Restore(ctx, folder.ID, trashed.Revision)
			require.NoError(t, err)
			require.Equal(t, original, path)
			require.Nil(t, restored.TrashedAt)
			child, err = s.NodeByPath(ctx, path+"/note.txt")
			require.NoError(t, err)
			require.Equal(t, file.ID, child.ID)
			require.Equal(t, file.CurrentVersionID, child.CurrentVersionID)
			retained, err := s.NodeByID(ctx, separate.ID)
			require.NoError(t, err)
			require.Equal(t, separate, retained, "independently trashed files stay in trash")
			_, path, err = s.Restore(ctx, separate.ID, separate.Revision)
			require.NoError(t, err)
			require.Equal(t, "/Documents/Drafts/old.txt", path)
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				high, err := s.auditNodeSequence(ctx, tx)
				if err != nil {
					return err
				}
				return s.validateMetadataState(ctx, tx, high)
			}))
		})
	}
}

func checkTrashRestoreNamesPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	parent, err := s.Mkdir(ctx, s.RootID(), "Drafts")
	require.NoError(t, err)
	file, err := s.CreateFile(ctx, parent.ID, "report.txt", fakeHash("b2"), 4, "text/plain")
	require.NoError(t, err)
	file, _, err = s.Trash(ctx, file.ID, file.Revision)
	require.NoError(t, err)
	// An unavailable original parent falls back to the root. Fill the first
	// two names and a later suffix to exercise the recursive first-gap query.
	_, _, err = s.Trash(ctx, parent.ID, UnconditionalRev)
	require.NoError(t, err)
	for _, name := range []string{"report.txt", "report (2).txt", "report (4).txt"} {
		_, err := s.CreateFile(ctx, s.RootID(), name, file.BlobHash, file.Size, file.MimeType)
		require.NoError(t, err)
	}
	restored, path, err := s.Restore(ctx, file.ID, file.Revision)
	require.NoError(t, err)
	require.Equal(t, "/report (3).txt", path)
	require.Equal(t, new(s.RootID()), restored.ParentID)
	require.Equal(t, file.CurrentVersionID, restored.CurrentVersionID)
}

func checkTrashPageSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	node, err := s.Mkdir(ctx, s.RootID(), "recover")
	require.NoError(t, err)
	node, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	var config *pgx.ConnConfig
	require.NoError(t, conn.Raw(func(driverConn any) error {
		pgConn, ok := driverConn.(*stdlib.Conn)
		require.True(t, ok)
		config = pgConn.Conn().Config().Copy()
		return nil
	}))
	require.NoError(t, conn.Close())
	var writeErr error
	config.Tracer = &afterFirstSelect{run: func() {
		_, _, writeErr = s.Restore(ctx, node.ID, node.Revision)
	}}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	reader := &Store{db: db, postgresSchema: s.postgresSchema, rootID: s.RootID()}
	page, total, err := reader.TrashedRootsPage(ctx, 1, 0)
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, page, 1)
	require.Equal(t, node.ID, page[0].ID)
	page, total, err = s.TrashedRootsPage(ctx, 1, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, page)
}

func checkPhotoTrashRestorePostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, audited := range []bool{false, true} {
		t.Run(fmt.Sprintf("audited=%t", audited), func(t *testing.T) {
			ctx := t.Context()
			setup, owner := pgtest.NewDatabase(t)
			target := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			source := newTestStore(t)
			source.vaultID = target.vaultID
			_, err := source.writeDB.ExecContext(ctx, `UPDATE vault_metadata SET vault_uid=$1`, source.vaultID)
			require.NoError(t, err)
			asset, nodes := photoTrashFixture(t, source)
			folder, err := source.Mkdir(ctx, source.RootID(), "Photos")
			require.NoError(t, err)
			for _, node := range nodes {
				parent, err := source.NodeByID(ctx, *node.ParentID)
				require.NoError(t, err)
				_, _, err = source.Move(ctx, parent.ID, folder.ID, parent.Name, parent.Revision)
				require.NoError(t, err)
			}
			raw := fileByRole(asset.Files, PhotoRoleRAW)
			importXMP(t, source, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating}, "retained keyword")
			if audited {
				plan, err := source.PreviewInitialAudit(ctx, source.RootID(), "cli", nil)
				require.NoError(t, err)
				_, err = source.EnableInitialAudit(ctx, plan)
				require.NoError(t, err)
			}
			tables := append(append([]string{}, postgresEnrollmentCatalogs...), "blob_locations", "blob_packs", "blob_pack_entries", "photo_source_claims", "photo_timeline")
			copyStoreCatalogFixture(t, source, setup, tables)
			target.primaryStoreID = source.primaryStoreID
			// Compare real operations with SQLite's retained triggers, not a second
			// implementation of the queue or sidecar receipt rules.
			check := func() {
				require.Equal(t, readTrashPhotoState(t, source, asset.ID), readTrashPhotoState(t, target, asset.ID))
				for _, s := range []*Store{source, target} {
					require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
						high, err := s.auditNodeSequence(ctx, tx)
						if err != nil {
							return err
						}
						return s.validateMetadataState(ctx, tx, high)
					}))
				}
			}
			check()
			for _, action := range []string{"sidecar revision drift", "member restore", "sidecar trash", "sidecar restore", "partial group", "whole group", "member restore", "folder trash", "folder restore", "complete subtree trash", "complete subtree restore"} {
				for _, s := range []*Store{source, target} {
					switch action {
					case "sidecar revision drift":
						_, err = s.TrashPhotoAsset(ctx, asset.ID, asset.Revision-1)
					case "sidecar trash":
						_, _, err = s.Trash(ctx, nodes[2].ID, UnconditionalRev)
					case "sidecar restore":
						_, _, err = s.Restore(ctx, nodes[2].ID, UnconditionalRev)
					case "partial group":
						_, _, err = s.Trash(ctx, nodes[1].ID, UnconditionalRev)
					case "whole group":
						current, e := photoAssetByIDQuery(ctx, s.db, asset.ID)
						require.NoError(t, e)
						_, err = s.TrashPhotoAsset(ctx, asset.ID, current.Revision)
					case "member restore":
						_, _, err = s.Restore(ctx, raw.NodeID, UnconditionalRev)
					case "folder trash":
						_, _, err = s.Trash(ctx, *nodes[0].ParentID, UnconditionalRev)
					case "folder restore":
						_, _, err = s.Restore(ctx, raw.NodeID, UnconditionalRev)
					case "complete subtree trash":
						_, _, err = s.Trash(ctx, folder.ID, UnconditionalRev)
					case "complete subtree restore":
						_, _, err = s.Restore(ctx, folder.ID, UnconditionalRev)
					}
					require.NoError(t, err, action)
				}
				check()
				if action == "whole group" {
					page, total, err := target.TrashedRootsPage(ctx, 1, 0)
					require.NoError(t, err)
					require.Equal(t, 1, total)
					require.Len(t, page, 1)
					require.Equal(t, asset.ID, page[0].PhotoAssetID)
					require.Equal(t, 3, page[0].PhotoFileCount)
					display, ok := photoFileByID(asset.Files, *asset.DisplayFileID)
					require.True(t, ok)
					require.Equal(t, display.NodeID, page[0].ID)
				}
			}
			// The final receipt is written after node mutations and audit advancement.
			// Fail that write and verify the operation retains none of its earlier work.
			before := readTrashPhotoState(t, target, asset.ID)
			var records int
			require.NoError(t, target.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_records`).Scan(&records))
			var role string
			require.NoError(t, target.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
			_, err = setup.ExecContext(ctx, `REVOKE INSERT ON documents.photo_change_receipts FROM `+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			current, err := photoAssetByIDQuery(ctx, target.db, asset.ID)
			require.NoError(t, err)
			_, err = target.TrashPhotoAsset(ctx, asset.ID, current.Revision)
			require.ErrorContains(t, err, "permission denied for table photo_change_receipts")
			require.Equal(t, before, readTrashPhotoState(t, target, asset.ID))
			var after int
			require.NoError(t, target.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_records`).Scan(&after))
			require.Equal(t, records, after)
		})
	}
}

type trashPhotoState struct {
	Nodes    []Node
	Asset    any
	Receipts []string
	Queue    []string
	Inputs   []string
}

func readTrashPhotoState(t *testing.T, s *Store, assetID string) trashPhotoState {
	t.Helper()
	ctx := t.Context()
	var out trashPhotoState
	require.NoError(t, queryRows(ctx, s.db, `SELECT `+nodeCols+` FROM `+nodeFrom+` ORDER BY n.id`, nil, func(rows *sql.Rows) error {
		node, err := scanNode(rows)
		if err != nil {
			return err
		}
		// Separate executions have different clocks; identity, revisions, bytes,
		// topology and trash presence must still be equal.
		node.ModifiedAt = ""
		if node.TrashedAt != nil {
			node.TrashedAt = new("trashed")
		}
		out.Nodes = append(out.Nodes, node)
		return nil
	}))
	asset, err := photoAssetByIDQuery(ctx, s.db, assetID)
	require.NoError(t, err)
	out.Asset = photoAssetState(asset, nil)
	for _, q := range []struct {
		sql  string
		dest *[]string
	}{
		{`SELECT operation||':'||before_json||':'||after_json FROM photo_change_receipts ORDER BY operation,before_revision,after_revision,before_json,after_json`, &out.Receipts},
		{`SELECT asset_id FROM photo_xmp_unsettled ORDER BY asset_id`, &out.Queue},
		{`SELECT file_id||':'||token FROM photo_xmp_inputs ORDER BY file_id`, &out.Inputs},
	} {
		require.NoError(t, queryRows(ctx, s.db, q.sql, nil, func(rows *sql.Rows) error {
			var value string
			if err := rows.Scan(&value); err != nil {
				return err
			}
			*q.dest = append(*q.dest, value)
			return nil
		}))
	}
	return out
}
