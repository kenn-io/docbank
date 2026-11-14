package store

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkPhotoSetReceiptsAndOrdering(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	var ids []string
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for i := range 258 {
			name := fmt.Sprintf("photo-%03d.jpg", i)
			node, _, err := s.createFileTx(ctx, tx, s.RootID(), name, browseHash(name), 20, "image/jpeg")
			if err != nil {
				return err
			}
			asset, err := s.insertPhotoAssetTx(ctx, tx, node.ID, PhotoRoleImage, PhotoKindPhoto)
			if err != nil {
				return err
			}
			ids = append(ids, asset.ID)
		}
		return nil
	}))
	set, err := s.CreatePhotoSet(ctx, "a")
	require.NoError(t, err)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: append(slices.Clone(ids), ids[0])})
	require.NoError(t, err)
	rows, err := s.db.QueryContext(ctx, `SELECT before_revision,after_revision,after_json FROM photo_change_receipts WHERE set_id=$1 AND operation='set_add'`, set.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var recorded []string
	var chunks []int
	for rows.Next() {
		var before, after int64
		var raw string
		require.NoError(t, rows.Scan(&before, &after, &raw))
		var receipt struct {
			Set PhotoSet `json:"set"`
			IDs []string `json:"asset_ids"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &receipt))
		require.EqualValues(t, 1, before)
		require.Equal(t, set.Revision, after)
		require.Equal(t, set, receipt.Set)
		require.Contains(t, [][]string{ids[:256], ids[256:]}, receipt.IDs)
		chunks = append(chunks, len(receipt.IDs))
		recorded = append(recorded, receipt.IDs...)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.ElementsMatch(t, []int{256, 2}, chunks)
	require.ElementsMatch(t, ids, recorded)
	noop, err := s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	require.Equal(t, set, noop)
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts WHERE set_id=$1 AND operation='set_add'`, set.ID).Scan(&count))
	require.Equal(t, 2, count)
	for _, name := range []string{"_x", "Z", "B", "b", "star"} {
		album, err := s.CreatePhotoSet(ctx, name)
		require.NoError(t, err)
		if name == "star" {
			_, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, nil, new(true), nil)
			require.NoError(t, err)
		}
	}
	albums, err := s.ListPhotoSets(ctx, "")
	require.NoError(t, err)
	var names []string
	for _, album := range albums {
		names = append(names, album.Name)
	}
	require.Equal(t, []string{"star", "B", "Z", "_x", "a", "b"}, names)
	require.EqualValues(t, 258, albums[4].MemberCount)
	checkRetentionMetadata(t, s)
}

func checkPhotoSetHiddenSummaries(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	hidden := albumAsset(t, s, "hidden-cover.jpg")
	visible := albumAsset(t, s, "visible.jpg")
	album, err := s.CreatePhotoSet(ctx, "Hidden cover")
	require.NoError(t, err)
	album, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{hidden.ID, visible.ID}})
	require.NoError(t, err)
	cover := &hidden.ID
	album, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, nil, nil, &cover)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, hidden.ID, hidden.Revision, true)
	require.NoError(t, err)
	summary, err := s.PhotoSet(ctx, album.ID, "")
	require.NoError(t, err)
	require.Nil(t, summary.CoverAssetID)
	require.EqualValues(t, 2, summary.MemberCount)
	require.EqualValues(t, 1, summary.IncludedCount)
	copied, err := s.DuplicatePhotoSet(ctx, album.ID, album.Revision, "Copy")
	require.NoError(t, err)
	require.Nil(t, copied.CoverAssetID)
	_, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, false, PhotoSetSelection{AssetIDs: []string{hidden.ID}})
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	summary, err = s.PhotoSet(unlocked, copied.ID, "")
	require.NoError(t, err)
	require.Equal(t, &hidden.ID, summary.CoverAssetID)
	require.EqualValues(t, 1, summary.IncludedCount, "album summaries always use visible covers")
	_, err = s.ChangePhotoSetMembers(unlocked, album.ID, album.Revision, false, PhotoSetSelection{AssetIDs: []string{hidden.ID}})
	require.NoError(t, err)
	summary, err = s.PhotoSet(unlocked, album.ID, "")
	require.NoError(t, err)
	require.Nil(t, summary.CoverAssetID)
	require.EqualValues(t, 1, summary.MemberCount)
}

func checkPhotoSetSnapshotsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, list := range []bool{false, true} {
		t.Run(fmt.Sprintf("list=%t", list), func(t *testing.T) {
			t.Parallel()
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			asset := albumAsset(t, s, "snapshot.jpg")
			album, err := s.CreatePhotoSet(ctx, "Before")
			require.NoError(t, err)
			album, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{asset.ID}})
			require.NoError(t, err)
			before, err := s.PhotoSet(ctx, album.ID, "")
			require.NoError(t, err)
			reader := postgresSnapshotReader(t, s, func() {
				_, err := s.DeletePhotoSet(ctx, album.ID, album.Revision)
				require.NoError(t, err)
			})
			if list {
				during, err := reader.ListPhotoSets(ctx, "")
				require.NoError(t, err)
				require.Equal(t, []PhotoSetSummary{before}, during)
			} else {
				during, err := reader.PhotoSet(ctx, album.ID, "")
				require.NoError(t, err)
				require.Equal(t, before, during)
			}
			_, err = s.PhotoSet(ctx, album.ID, "")
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

func checkPhotoSetRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, operation := range []string{"create", "update", "add", "remove", "duplicate", "delete"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			setup, owner := pgtest.NewDatabase(t)
			s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			ctx := t.Context()
			first := albumAsset(t, s, "first.jpg")
			second := albumAsset(t, s, "second.jpg")
			album, err := s.CreatePhotoSet(ctx, "Kept")
			require.NoError(t, err)
			album, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{first.ID}})
			require.NoError(t, err)
			before, err := s.PhotoSet(ctx, album.ID, "")
			require.NoError(t, err)
			var role string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
			_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.photo_change_receipts FROM "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			var receipts int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts`).Scan(&receipts))
			run := func() error {
				var err error
				switch operation {
				case "create":
					_, err = s.CreatePhotoSet(ctx, "Rolled back")
				case "update":
					_, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, new("Rolled back"), nil, nil)
				case "add":
					_, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{second.ID}})
				case "remove":
					_, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, false, PhotoSetSelection{AssetIDs: []string{first.ID}})
				case "duplicate":
					_, err = s.DuplicatePhotoSet(ctx, album.ID, album.Revision, "Rolled back")
				case "delete":
					_, err = s.DeletePhotoSet(ctx, album.ID, album.Revision)
				}
				return err
			}
			require.ErrorContains(t, run(), "permission denied")
			albums, err := s.ListPhotoSets(ctx, "")
			require.NoError(t, err)
			require.Equal(t, []PhotoSetSummary{before}, albums)
			ids, err := photoSetMemberIDs(ctx, s.db, album.ID)
			require.NoError(t, err)
			require.Equal(t, []string{first.ID}, ids)
			var after int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts`).Scan(&after))
			require.Equal(t, receipts, after)
		})
	}
}
