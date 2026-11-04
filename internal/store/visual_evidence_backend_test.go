package store

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkPreviewBatchRecipes(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	var versions []string
	recipes := make(map[string]string)
	expected := make(map[string]map[string]VisualPreviewGeneration)
	for i := range 2 {
		node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("photo-%d.jpg", i), fakeHash(fmt.Sprintf("%02x", 0x41+i)), 12, "image/jpeg")
		require.NoError(t, err)
		versions = append(versions, node.CurrentVersionID)
		expected[node.CurrentVersionID] = make(map[string]VisualPreviewGeneration)
		for j, edge := range []int{512, 2560, 4096} {
			recipe := visualPreviewRecipe()
			recipe.MaxEdgePixels = edge
			canonical := readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("43"), 9, recipe)
			generation, err := s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, canonical, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
			require.NoError(t, err)
			if j < 2 {
				recipes[strconv.Itoa(edge)] = generation.RecipeFingerprint
				expected[node.CurrentVersionID][generation.RecipeFingerprint] = generation
			}
		}
	}
	// Repeated and absent versions must not add results; unrequested recipes stay out.
	versions = append(versions, versions[0], "d7adc0ca-cfdc-4c33-a6d8-06a2b13c50bc")
	got, err := photoPreviewGenerations(ctx, s.db, versions, recipes)
	require.NoError(t, err)
	require.Equal(t, expected, got)
}

func checkVisualPreviewPublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("61"), 12, "image/jpeg")
	require.NoError(t, err)
	physical := &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9}
	first, err := s.PublishVisualPreview(ctx, node.CurrentVersionID, readyVisualPreview(t, node.BlobHash, fakeHash("62"), 9), physical)
	require.NoError(t, err)
	recipe := visualPreviewRecipe()
	recipe.MaxEdgePixels = 512
	next := readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("63"), 9, recipe)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	// Fail the head write after the output blob and generation have been inserted.
	_, err = setup.ExecContext(ctx, "REVOKE UPDATE(generation_id,published_at) ON documents.visual_preview_heads FROM "+role)
	require.NoError(t, err)
	_, err = s.PublishVisualPreview(ctx, node.CurrentVersionID, next, physical)
	require.ErrorContains(t, err, "permission denied")
	view, err := s.ContentVersionVisualPreview(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, first.GenerationID, view.Generation.GenerationID)
	_, err = s.BlobInfo(ctx, fakeHash("63"))
	require.ErrorIs(t, err, ErrNotFound)
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM visual_preview_generations`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = setup.ExecContext(ctx, "GRANT UPDATE(generation_id,published_at) ON documents.visual_preview_heads TO "+role)
	require.NoError(t, err)
	second, err := s.PublishVisualPreview(ctx, node.CurrentVersionID, next, physical)
	require.NoError(t, err)
	view, err = s.ContentVersionVisualPreview(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, second.GenerationID, view.Generation.GenerationID)
	checkRetentionMetadata(t, s)
}

func checkVisualEvidenceSnapshotsPostgres(t *testing.T) {
	t.Helper()
	for _, operation := range []string{"preview-head", "preview-recipe", "pdf-receipt", "pdf-receipts"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			var read func(*Store) (any, error)
			var mutate func() error
			var versionID string
			if operation == "preview-head" || operation == "preview-recipe" {
				node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("71"), 12, "image/jpeg")
				require.NoError(t, err)
				versionID = node.CurrentVersionID
				mutate = func() error {
					updated, _, err := s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("73"), 12, "image/jpeg")
					if err != nil {
						return err
					}
					_, err = s.PruneContentVersions(ctx, node.ID, updated.Revision, VersionPruneSelector{VersionIDs: []string{versionID}}, true)
					return err
				}
				generation, err := s.PublishVisualPreview(ctx, versionID, readyVisualPreview(t, node.BlobHash, fakeHash("72"), 9), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
				require.NoError(t, err)
				read = func(reader *Store) (any, error) {
					if operation == "preview-head" {
						return reader.ContentVersionVisualPreview(ctx, versionID)
					}
					return reader.ContentVersionVisualPreviewByRecipe(ctx, versionID, generation.RecipeFingerprint)
				}
			} else {
				seedRenditionCatalogVersions(t, s)
				attachment := stageEmailPDFFixture(t, s)
				require.NoError(t, publishAttachmentForTest(t, s, attachment))
				versionID = attachment.ContentVersionID
				mutate = func() error {
					_, err := s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{versionID}})
					return err
				}
				read = func(reader *Store) (any, error) {
					if operation == "pdf-receipt" {
						return reader.EmailPDFReceipt(ctx, versionID, attachment.Profile.Fingerprint)
					}
					return reader.EmailPDFReceipts(ctx, versionID)
				}
			}
			before, err := read(s)
			require.NoError(t, err)
			conn, err := s.db.Conn(ctx)
			require.NoError(t, err)
			var cfg *pgx.ConnConfig
			err = conn.Raw(func(raw any) error {
				pg, ok := raw.(*stdlib.Conn)
				if !ok {
					return fmt.Errorf("unexpected PostgreSQL connection %T", raw)
				}
				cfg = pg.Conn().Config().Copy()
				return nil
			})
			require.NoError(t, conn.Close())
			require.NoError(t, err)
			var changed bool
			var mutationErr error
			cfg.Tracer = &afterFirstSelect{run: func() {
				mutationErr = mutate()
				changed = true
			}}
			db := stdlib.OpenDB(*cfg)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			reader := &Store{db: db, rootID: s.rootID, vaultID: s.vaultID, postgresSchema: s.postgresSchema}
			during, err := read(reader)
			require.NoError(t, mutationErr)
			require.True(t, changed)
			require.NoError(t, err)
			require.Equal(t, before, during)
			after, err := read(s)
			if operation == "pdf-receipts" {
				require.NoError(t, err)
				require.Empty(t, after)
			} else {
				require.ErrorIs(t, err, ErrNotFound)
			}
		})
	}
}
