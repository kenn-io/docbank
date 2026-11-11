package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkMetadataImportCatalogsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	seeds := retainedMetadataCatalogSeeds()
	seeds["audit and moved parents"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		seedMetadataRoundTrip(t, s)
		parent, err := s.Mkdir(t.Context(), s.RootID(), "newer-parent")
		require.NoError(t, err)
		old, err := s.NodeByPath(t.Context(), "/Projects")
		require.NoError(t, err)
		_, _, err = s.Move(t.Context(), old.ID, parent.ID, "Projects", old.Revision)
		require.NoError(t, err)
		deleted, err := s.Mkdir(t.Context(), s.RootID(), "deleted-highest")
		require.NoError(t, err)
		_, _, err = s.Trash(t.Context(), deleted.ID, deleted.Revision)
		require.NoError(t, err)
		_, err = s.TrashEmptyBounded(t.Context(), 0, 10, true)
		require.NoError(t, err)
		plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "cli", nil)
		require.NoError(t, err)
		_, err = s.EnableInitialAudit(t.Context(), plan)
		require.NoError(t, err)
		return s
	}
	seeds["hidden photo lockout"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		asset := albumAsset(t, s, "hidden.jpg")
		require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
		_, err := s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
		require.NoError(t, err)
		for range 4 {
			_, _, err := s.UnlockPhotoHidden(t.Context(), "wrong-passcode")
			require.Error(t, err)
		}
		return s
	}
	seeds["processing jobs"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		versions := seedRenditionCatalogVersions(t, s)
		request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
		grantRenditionJobConsent(t, s, request)
		_, _, err := s.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		return s
	}
	seeds["candidate faces"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, face := faceForDecisionTest(t, s, "")
		buildFaceMatchCandidateForTest(t, s, node.CurrentVersionID, face.FaceID, 1, 0)
		require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
		return s
	}
	seeds["photo projection pages"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		for i := range 101 {
			albumAsset(t, s, fmt.Sprintf("photo-%03d.jpg", i))
		}
		return s
	}
	for name, seed := range seeds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			source := seed(t)
			var input bytes.Buffer
			require.NoError(t, source.ExportMetadata(ctx, &input))
			setup, owner := pgtest.NewDatabase(t)
			cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
			require.NoError(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())))
			runtime := newPostgresSchemaStore(t, setup, cfg)
			var output bytes.Buffer
			require.NoError(t, runtime.ExportMetadata(ctx, &output))
			require.Equal(t, input.String(), output.String())
			require.NoError(t, runtime.ValidateMetadata(ctx))
			recovered := newTestStore(t)
			require.NoError(t, recovered.ImportMetadata(ctx, bytes.NewReader(output.Bytes())))
			var roundTrip bytes.Buffer
			require.NoError(t, recovered.ExportMetadata(ctx, &roundTrip))
			require.Equal(t, input.String(), roundTrip.String())
			projections := func(store *Store) []string {
				var result []string
				require.NoError(t, queryRows(ctx, store.db, `SELECT file_id,sort_key,day,COALESCE(modified_time,''),COALESCE(folder_key,'') FROM photo_timeline ORDER BY file_id`, nil, func(rows *sql.Rows) error {
					var file, sortKey, day, modified, folder string
					if err := rows.Scan(&file, &sortKey, &day, &modified, &folder); err != nil {
						return err
					}
					result = append(result, file, sortKey, day, modified, folder)
					return nil
				}))
				require.NoError(t, queryRows(ctx, store.db, `SELECT asset_id FROM photo_xmp_unsettled ORDER BY asset_id`, nil, func(rows *sql.Rows) error {
					var asset string
					if err := rows.Scan(&asset); err != nil {
						return err
					}
					result = append(result, asset)
					return nil
				}))
				return result
			}
			require.Equal(t, projections(recovered), projections(runtime))

			before, err := source.VerifyAudit(ctx, nil)
			require.NoError(t, err)
			after, err := runtime.VerifyAudit(ctx, nil)
			require.NoError(t, err)
			require.Equal(t, before, after)
			if name == "hidden photo lockout" {
				_, _, err := runtime.UnlockPhotoHidden(ctx, "wrong-passcode")
				require.Error(t, err)
				var count, lastID int64
				require.NoError(t, runtime.db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(failure_id) FROM photo_hidden_failures`).Scan(&count, &lastID))
				require.Equal(t, int64(5), count)
				require.Equal(t, int64(5), lastID)
			}
			if name == "candidate faces" {
				visible, _, err := runtime.PersonCandidates(ctx, "open", 10, 0)
				require.NoError(t, err)
				require.Len(t, visible, 1)
				node, err := runtime.NodeByPath(ctx, "/portrait.jpg")
				require.NoError(t, err)
				asset, err := runtime.PhotoAssetForNode(ctx, node.ID)
				require.NoError(t, err)
				_, err = runtime.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
				visible, _, err = runtime.PersonCandidates(ctx, "open", 10, 0)
				require.NoError(t, err)
				require.Empty(t, visible, "restored candidate face membership must hide evidence from hidden photos")
			}
			if name == "audit and moved parents" {
				sequence, err := source.auditNodeSequence(ctx, source.db)
				require.NoError(t, err)
				node, err := runtime.Mkdir(ctx, runtime.RootID(), "after-transfer")
				require.NoError(t, err)
				require.Equal(t, sequence+1, node.ID)
				_, err = runtime.VerifyAudit(ctx, &before.Evidence)
				require.NoError(t, err)
			}
		})
	}
}

type failedMetadataReader struct{ err error }

func (r failedMetadataReader) Read([]byte) (int, error) { return 0, r.err }

func checkMetadataImportRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	_, err := source.CreateFile(t.Context(), source.RootID(), "retained.txt", fakeHash("a1"), 9, "text/plain")
	require.NoError(t, err)
	var input bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &input))
	for name, late := range map[string]bool{"interrupted stream": false, "late allocation write": true} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			setup, owner := pgtest.NewDatabase(t)
			cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
			before, err := pgschema.Setup(ctx, setup, cfg)
			require.NoError(t, err)
			var broken = io.MultiReader(bytes.NewReader(input.Bytes()), failedMetadataReader{errors.New("source interrupted")})
			writer := setup
			injected := false
			var injectionErr error
			if late {
				conn, err := setup.Conn(ctx)
				require.NoError(t, err)
				var config *pgx.ConnConfig
				require.NoError(t, conn.Raw(func(raw any) error {
					native, ok := raw.(*stdlib.Conn)
					require.True(t, ok)
					config = native.Conn().Config().Copy()
					return nil
				}))
				require.NoError(t, conn.Close())
				config.Tracer = renditionQueryStart(func(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) {
					if !injected && strings.HasPrefix(data.SQL, "UPDATE node_allocation SET high_water=") {
						injectionErr = setup.QueryRowContext(ctx, `SELECT pg_terminate_backend($1,5000)`, conn.PgConn().PID()).Scan(&injected)
					}
				})
				writer = stdlib.OpenDB(*config)
				t.Cleanup(func() { require.NoError(t, writer.Close()) })
				broken = bytes.NewReader(input.Bytes())
			}
			require.Error(t, ImportPostgresMetadata(ctx, writer, cfg, broken))
			if late {
				require.NoError(t, injectionErr)
				require.True(t, injected)
			}

			after, err := pgschema.Setup(ctx, setup, cfg)
			require.NoError(t, err)
			require.Equal(t, before, after)
			var nodes, blobs, highWater int64
			require.NoError(t, setup.QueryRowContext(ctx, `SELECT
				(SELECT COUNT(*) FROM documents.nodes), (SELECT COUNT(*) FROM documents.blobs),
				(SELECT high_water FROM documents.node_allocation WHERE singleton=1)`).Scan(&nodes, &blobs, &highWater))
			require.Equal(t, int64(1), nodes)
			require.Zero(t, blobs)
			require.Equal(t, int64(1), highWater)

			require.NoError(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())))
		})
	}
}

func checkMetadataImportAdmissionPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	var input bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &input))
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	setup.SetMaxOpenConns(1)
	var originalPath string
	require.NoError(t, setup.QueryRowContext(ctx, `SHOW search_path`).Scan(&originalPath))
	runtime := newPostgresSchemaStore(t, setup, cfg)
	require.ErrorContains(t, ImportPostgresMetadata(ctx, runtime.db, cfg, bytes.NewReader(input.Bytes())), "schema owner role")
	_, err := runtime.Mkdir(ctx, runtime.RootID(), "keep-this")
	require.NoError(t, err)
	var before, after bytes.Buffer
	require.NoError(t, runtime.ExportMetadata(ctx, &before))
	require.ErrorContains(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())), "not pristine")
	require.NoError(t, runtime.ExportMetadata(ctx, &after))
	require.Equal(t, before.String(), after.String())
	var path string
	require.NoError(t, setup.QueryRowContext(ctx, `SHOW search_path`).Scan(&path))
	require.Equal(t, originalPath, path, "the caller's pool search path stays unchanged")
	require.Zero(t, setup.Stats().InUse)
}

func checkMetadataImportKeepsFreshConsentPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	request := testProviderAuthorizationRequest()
	_, err := source.GrantConsent(ctx, grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	_, err = source.AuthorizeProviderOperation(ctx, request)
	require.NoError(t, err)
	var input bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &input))
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	require.NoError(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())))
	runtime := newPostgresSchemaStore(t, setup, cfg)
	_, err = runtime.AuthorizeProviderOperation(ctx, request)
	require.ErrorIs(t, err, ErrProcessingConsentRequired)
	_, err = runtime.GrantConsent(ctx, grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	_, err = runtime.AuthorizeProviderOperation(ctx, request)
	require.NoError(t, err)
}
