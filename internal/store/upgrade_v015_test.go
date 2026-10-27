package store

import (
	"bytes"
	"database/sql"
	_ "embed"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"uuid"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	docsqlite "go.kenn.io/docbank/sqlite"
)

//go:embed testdata/schema-v0.15.0.sql
var schemaV0150SQL string

//go:embed testdata/schema-v0.15.1.sql
var schemaV0151SQL string

//go:embed testdata/schema-v0.16.0.sql
var schemaV0160SQL string

// releasedSchemaFixtures holds the exact schema.sql of every release from
// v0.15.0 on, keyed by storage schema version.
var releasedSchemaFixtures = map[int]string{28: schemaV0150SQL, 29: schemaV0151SQL, 41: schemaV0160SQL}

// firstFixtureStorageSchemaVersion is the oldest schema in
// releasedSchemaFixtures. Older releases have their own upgrade tests.
const firstFixtureStorageSchemaVersion = 28

const releasedFixtureVaultID = "10000000-0000-4000-8000-000000000028"

// newReleasedFixtureStore creates a vault with an exact released schema and
// returns a Store bound to it. Seeding through current code is limited to
// tables whose layout matches the released one.
func newReleasedFixtureStore(t *testing.T, path string, driver docsqlite.Driver, schema string, version int) (*sql.DB, *Store) {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(schema)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO vault_metadata(singleton,vault_uid,schema_version) VALUES(1,?,?)`, releasedFixtureVaultID, version)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO nodes(id,parent_id,name,kind,revision,created_at,modified_at)
		VALUES(1,NULL,'','dir',1,'2026-08-16T12:00:00.000000000Z','2026-08-16T12:00:00.000000000Z')`)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	primary, err := ensurePrimaryBlobStoreTx(tx)
	require.NoError(t, err)
	require.NoError(t, initializeDocumentPeopleState(t.Context(), tx))
	require.NoError(t, ensureProcessingIncarnationTx(tx, nil))
	require.NoError(t, tx.Commit())
	return db, &Store{db: db, writeDB: db, rootID: 1, vaultID: releasedFixtureVaultID, primaryStoreID: primary.ID, driver: driver}
}

func dumpReleasedOperationalTables(t *testing.T, q metadataQuerier) string {
	t.Helper()
	var out strings.Builder
	for _, table := range slices.Concat(releasedStorageOperationTables, releasedPendingDeletionTables) {
		dumpReleasedTable(t, q, table, &out)
	}
	return out.String()
}

func dumpReleasedTable(t *testing.T, q metadataQuerier, table string, out *strings.Builder) {
	t.Helper()
	columns, err := queryTableColumns(t.Context(), q, table)
	require.NoError(t, err)
	if table == "embedding_jobs" {
		columns = slices.DeleteFunc(columns, func(column string) bool { return column == "source_profile_fingerprint" })
	}
	rows, err := q.QueryContext(t.Context(), `SELECT `+strings.Join(columns, ",")+` FROM `+table+` ORDER BY rowid`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	values := make([]any, len(columns))
	pointers := make([]any, len(values))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		require.NoError(t, rows.Scan(pointers...))
		fmt.Fprintf(out, "%s %q\n", table, values)
	}
	require.NoError(t, rows.Err())
}

// seedV0150PendingWork records a photo, consent with a retrying rendition
// job, a queued photo import, its cleanup record, and every pending deletion.
func seedV0150PendingWork(t *testing.T, db *sql.DB, legacy *Store) (photoNodeID int64, jobID, operationID string) {
	t.Helper()
	ctx := t.Context()
	node := createReleasedFixtureFile(t, legacy, "released-photo.raw", fakeHash("a1"), 4, "application/octet-stream")
	seedReleasedPhotoAsset(t, db, node.ID)
	const at = "2026-10-05T12:00:00.000000000Z"

	versions := seedRenditionCatalogVersions(t, legacy)
	request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
	grantRenditionJobConsent(t, legacy, request)
	job, waiter, err := legacy.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	now := time.Now().UTC()
	claim, err := legacy.ClaimRenditionJob(ctx, job.ID, "worker:released", now, time.Minute)
	require.NoError(t, err)
	_, err = legacy.BeginRenditionProvider(ctx, claim, waiter.ID, now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	require.NoError(t, legacy.CheckpointRenditionProvider(ctx, claim, "released-provider-handle", now.Add(2*time.Second)))
	require.NoError(t, legacy.MarkRenditionJobRetry(ctx, claim, RenditionFailureTransient, now.Add(3*time.Second), now.Add(4*time.Second)))

	// Current operation seeding needs checkout retention tables. Temporary
	// tables keep the released on-disk schema exact for the upgrade.
	_, err = db.Exec(`CREATE TEMP TABLE photo_checkouts (checkout_id TEXT, operation_id TEXT, retired_at TEXT);
		CREATE TEMP TABLE photo_checkout_entries (checkout_id TEXT, file_id TEXT, published INTEGER)`)
	require.NoError(t, err)
	operation, err := legacy.CreateLocalOperation(ctx, StorageOperationKindPhotoImport, `{}`)
	require.NoError(t, err)
	purged := fakeHash("b2")
	require.NoError(t, legacy.withStorageTx(ctx, func(tx *sql.Tx) error { return legacy.EnsureBlobTx(tx, purged, 9) }))
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO storage_operation_stores(operation_id,store_id,role) VALUES(?,?,'destination')`,
			[]any{operation.ID, legacy.primaryStoreID}},
		{`INSERT INTO storage_operation_cleanup(operation_id,store_id,loose_hash) VALUES(?,?,?)`,
			[]any{operation.ID, legacy.primaryStoreID, fakeHash("c3")}},
		{`INSERT INTO gc_loose_retirements(store_id,blob_hash,loose_encoding) VALUES(?,?,0)`,
			[]any{legacy.primaryStoreID, fakeHash("d4")}},
		{`INSERT INTO derivative_blob_purge_pending(blob_hash) VALUES(?)`, []any{purged}},
		{`INSERT INTO blob_packs(store_id,pack_id,entry_count,stored_bytes,created_at) VALUES(?,'purged-pack',0,0,?)`,
			[]any{legacy.primaryStoreID, at}},
		{`INSERT INTO derivative_pack_purge_pending(store_id,pack_id) VALUES(?,'purged-pack')`,
			[]any{legacy.primaryStoreID}},
	} {
		_, err := db.Exec(statement.query, statement.args...)
		require.NoError(t, err)
	}
	return node.ID, job.ID, operation.ID
}

func TestUpgradeReleasedV0150PreservesAuthorityAndPendingWork(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			photoNodeID, jobID, operationID := seedV0150PendingWork(t, db, legacy)
			incarnation, err := legacy.CurrentProcessingIncarnation(t.Context())
			require.NoError(t, err)
			snapshot, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			var released bytes.Buffer
			require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), snapshot, &released, 28))
			pending := dumpReleasedOperationalTables(t, snapshot)
			require.NoError(t, snapshot.Rollback())
			require.NoError(t, db.Close())
			require.Equal(t, len(releasedStorageOperationTables)+len(releasedPendingDeletionTables),
				strings.Count(pending, "\n"), "the fixture seeds one row in every copied table")

			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()

			var version int
			require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata`).Scan(&version))
			assert.Equal(t, currentStorageSchemaVersion, version)
			var upgraded bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &upgraded))
			assert.Equal(t, normalizeReleasedUpgradeSnapshot(t, released.Bytes()), normalizeReleasedUpgradeSnapshot(t, upgraded.Bytes()))
			assert.Equal(t, pending, dumpReleasedOperationalTables(t, s.db))

			current, err := s.CurrentProcessingIncarnation(t.Context())
			require.NoError(t, err)
			assert.Equal(t, incarnation, current, "processing consent survives the upgrade")
			resumedAt := time.Now().UTC().Add(10 * time.Second)
			claim, err := s.ClaimRenditionJob(t.Context(), jobID, "worker:upgraded", resumedAt, time.Minute)
			require.NoError(t, err)
			_, err = s.RenditionJobWorkByClaim(t.Context(), claim, resumedAt)
			require.NoError(t, err, "a pending rendition job keeps its authorization")

			resumable, err := s.ResumableStorageOperations(t.Context())
			require.NoError(t, err)
			require.Len(t, resumable, 1)
			assert.Equal(t, operationID, resumable[0].ID)

			asset, err := s.PhotoAssetForNode(t.Context(), photoNodeID)
			require.NoError(t, err)
			assert.Equal(t, "40000000-0000-4000-8000-000000000001", asset.ID)
			require.FileExists(t, path+v28BackupSuffix)
		})
	}
}

// Coverage guard: each released schema still upgrades to the current one.
// When the current schema moves past a fixture's version, this test fails
// until that version has a cutover adapter.
func TestEveryReleasedSchemaFixtureUpgrades(t *testing.T) {
	t.Parallel()
	for version, schema := range releasedSchemaFixtures {
		for _, driver := range v090UpgradeDrivers() {
			t.Run(fmt.Sprintf("v%d/%s", version, driver.name), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "docbank.db")
				db, legacy := newReleasedFixtureStore(t, path, driver.driver, schema, version)
				node := createReleasedFixtureFile(t, legacy, "released.jpg", fakeHash("e5"), 8, "image/jpeg")
				seedReleasedPhotoAsset(t, db, node.ID)
				seedReleasedMtime(t, db, node.ID, "2024-03-05T10:00:00Z")
				definition, err := legacy.CreateSavedQuery(t.Context(), "Released", "", SavedQueryKindQuery, []byte(`{}`))
				require.NoError(t, err)
				first := uuid.New().String()
				second := uuid.New().String()
				at := nowRFC3339()
				_, err = db.Exec(`INSERT INTO saved_query_runs(run_id,saved_query_id,saved_query_revision,query_fingerprint,snapshot_id,member_hash,total,total_bytes,ran_at,expires_at) VALUES(?,?,1,?,?,?,0,0,?,?)`, first, definition.ID, definition.Fingerprint, strings.Repeat("a", 32), strings.Repeat("b", 64), at, at)
				require.NoError(t, err)
				_, err = db.Exec(`INSERT INTO saved_query_runs(run_id,saved_query_id,saved_query_revision,query_fingerprint,snapshot_id,member_hash,total,total_bytes,ran_at,expires_at,previous_run_id,previous_member_hash,previous_total,previous_query_fingerprint) VALUES(?,?,1,?,?,?,0,0,?,?,?,?,0,?)`, second, definition.ID, definition.Fingerprint, strings.Repeat("c", 32), strings.Repeat("b", 64), at, at, first, strings.Repeat("b", 64), definition.Fingerprint)
				require.NoError(t, err)
				require.NoError(t, db.Close())
				s, err := Open(path, driver.driver)
				require.NoError(t, err)
				defer func() { require.NoError(t, s.Close()) }()
				firstRun := persistedSavedQueryRun(t, s, first)
				secondRun := persistedSavedQueryRun(t, s, second)
				assert.Equal(t, first, secondRun.PreviousRunID)
				assert.Empty(t, firstRun.PreviousRunID)
				assert.False(t, firstRun.Photos)
				assert.False(t, secondRun.Photos)
				var current int
				require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata`).Scan(&current))
				assert.Equal(t, currentStorageSchemaVersion, current)
				asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
				require.NoError(t, err)
				require.Nil(t, asset.HiddenAt)
				require.Equal(t, node.ID, asset.Files[0].NodeID)

				person, err := s.CreatePerson(t.Context(), "Ada", "operator")
				require.NoError(t, err)
				currentNode, err := s.NodeByID(t.Context(), node.ID)
				require.NoError(t, err)
				face, _, err := s.CreatePhotoFace(t.Context(), currentNode.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
				require.NoError(t, err)
				faces, _, _, err := s.PhotoFacePersonNames(t.Context(), currentNode.CurrentVersionID)
				require.NoError(t, err)
				require.Equal(t, []PhotoFace{face}, faces)
				page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"desc"}}`)}, nil)
				require.NoError(t, err)
				require.Len(t, page.Items, 1)
				require.Equal(t, "2024-03-05T10:00:00Z", *page.Items[0].FileModifiedTime, "an EXIF-less photo is placed by its released mtime")
			})
		}
	}
}

// Coverage guard: a released schema is frozen. Changing schema.sql after a
// release requires a new storage schema version and a cutover adapter.
func TestReleasedSchemaFixturesMatchVersions(t *testing.T) {
	t.Parallel()
	for _, version := range releasedStorageSchemaVersions {
		if version < firstFixtureStorageSchemaVersion {
			continue
		}
		fixture, ok := releasedSchemaFixtures[version]
		require.Truef(t, ok, "released storage schema %d needs an exact fixture in releasedSchemaFixtures", version)
		if version == currentStorageSchemaVersion {
			assert.Equal(t, fixture, schemaSQL,
				"schema.sql differs from released schema %d; bump currentStorageSchemaVersion and add a cutover adapter", version)
		}
	}
	for version := range releasedSchemaFixtures {
		assert.Contains(t, releasedStorageSchemaVersions, version)
	}
}

func TestReleasedStorageSchemasRequireAdapters(t *testing.T) { //nolint:paralleltest // swaps the package-level releasedStorageSchemaVersions
	require.NoError(t, validateReleasedStorageSchemas())
	original := releasedStorageSchemaVersions
	t.Cleanup(func() { releasedStorageSchemaVersions = original })

	releasedStorageSchemaVersions = append(append([]int(nil), original...), 27)
	require.ErrorContains(t, validateReleasedStorageSchemas(), "released storage schema version 27 adapter is missing")

	releasedStorageSchemaVersions = slices.DeleteFunc(slices.Clone(original), func(version int) bool { return version == 28 })
	require.ErrorContains(t, validateReleasedStorageSchemas(), "storage schema version 28 has an adapter but no release")
}

func TestOpenRejectsChangedReleasedV0150Layout(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, _ := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			_, err := db.Exec(`ALTER TABLE storage_operations ADD COLUMN unexpected TEXT`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			s, err := Open(path, driver.driver)
			if s != nil {
				require.NoError(t, s.Close())
			}
			require.ErrorContains(t, err, "not a released v0.15.0 database: unexpected storage_operations columns")
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after, "a refused upgrade leaves the vault unchanged")
		})
	}
}

func TestImportMetadataProcessingIncarnationAcceptsOnlyIdenticalExistingRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	current := metadataProcessingIncarnation{Type: metadataProcessingIncarnationType}
	require.NoError(t, s.db.QueryRow(`SELECT incarnation_id,created_at FROM processing_incarnations`).
		Scan(&current.ID, &current.CreatedAt))
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()

	require.NoError(t, importMetadataProcessingIncarnation(t.Context(), tx, current))
	changed := current
	changed.CreatedAt = "2026-01-01T00:00:00.000000000Z"
	require.ErrorContains(t, importMetadataProcessingIncarnation(t.Context(), tx, changed),
		"processing incarnation "+current.ID+" already exists with created_at")
	older := metadataProcessingIncarnation{Type: metadataProcessingIncarnationType,
		ID: "60000000-0000-4000-8000-000000000001", CreatedAt: "2026-01-01T00:00:00.000000000Z"}
	require.NoError(t, importMetadataProcessingIncarnation(t.Context(), tx, older))

	var count int
	require.NoError(t, tx.QueryRow(`SELECT COUNT(*) FROM processing_incarnations`).Scan(&count))
	assert.Equal(t, 2, count)
}

func TestUpgradeReleasedV0150KeepsExportState(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			node, err := legacy.CreateFile(ctx, legacy.RootID(), "exported.txt", fakeHash("f6"), 12, "text/plain")
			require.NoError(t, err)
			source, err := legacy.CreateExportSource(ctx, "master", bundle.SourceRequest{
				OperationID: uuid.New().String(), Kind: "explicit",
				Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}},
			}, nil)
			require.NoError(t, err)
			plan, err := legacy.CreateExportPlan(ctx, "master", bundle.PlanRequest{
				OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash,
				Roles: []bundle.RolePolicy{{Role: "original"}},
			})
			require.NoError(t, err)
			job := bundle.JobRequest{PlanID: plan.ID, Fingerprint: plan.Fingerprint}
			job.OperationID = uuid.New().String()
			completed, err := legacy.QueueExportJob(ctx, "master", job)
			require.NoError(t, err)
			_, err = db.Exec(`UPDATE export_jobs SET state='completed',archive_name=? WHERE id=?`,
				completed.ID+".zip", completed.ID)
			require.NoError(t, err)
			job.OperationID = uuid.New().String()
			interrupted, err := legacy.QueueExportJob(ctx, "master", job)
			require.NoError(t, err)
			_, err = legacy.ClaimExportJob(ctx)
			require.NoError(t, err)
			snapshot, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			var released strings.Builder
			for _, table := range releasedExportTables {
				dumpReleasedTable(t, snapshot, table, &released)
			}
			require.NoError(t, snapshot.Rollback())
			require.NoError(t, db.Close())

			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()

			var upgraded strings.Builder
			for _, table := range releasedExportTables {
				dumpReleasedTable(t, s.db, table, &upgraded)
			}
			assert.Equal(t, released.String(), upgraded.String())
			retained, err := s.ExportArchiveRetained(ctx, completed.ID)
			require.NoError(t, err)
			assert.True(t, retained, "exporter startup keeps the completed archive")
			_, err = s.ExportSource(ctx, "master", source.ID)
			require.NoError(t, err)
			_, err = s.ExportPlan(ctx, "master", plan.ID)
			require.NoError(t, err)
			_, err = s.ExportJob(ctx, "master", interrupted.ID)
			require.NoError(t, err)
			require.NoError(t, s.RequeueExportJobs(ctx))
			resumed, err := s.ClaimExportJob(ctx)
			require.NoError(t, err)
			assert.Equal(t, interrupted.ID, resumed.Job.ID)
			assert.Equal(t, "master", resumed.Owner)
		})
	}
}

func TestUpgradeReleasedV0150KeepsAdmittedMediaQueued(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			request := suppliedMediaPublicationFixture(t, legacy)
			consent := testProviderAuthorizationRequest()
			consent.Principal = request.Operation.Principal
			_, err := legacy.GrantConsent(ctx, grantRequestForAuthorization(consent, nil))
			require.NoError(t, err)
			authorization, err := legacy.AuthorizeProviderOperation(ctx, consent)
			require.NoError(t, err)
			consent.PriorAuthorization = &authorization
			request.ProcessingProfile, request.ProcessingPrincipal = "speech", consent.Principal
			request.ProcessingScope, request.ProcessingProfileFingerprint = consent.Scope, consent.ProfileFingerprint
			request.ProcessingAuthorization = consent
			admitted, err := legacy.RetainSuppliedMedia(ctx, request)
			require.NoError(t, err)
			require.Equal(t, "queued", admitted.OperationState)
			require.Empty(t, admitted.JobID, "admission committed before a rendition job was bound")
			require.NoError(t, db.Close())

			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()

			continuations, err := s.MediaProcessingContinuations(ctx, consent.Principal, "", 10)
			require.NoError(t, err)
			require.Len(t, continuations, 1, "startup finishes the admission after the upgrade")
			assert.Equal(t, admitted.OperationID, continuations[0].OperationID)
		})
	}
}

func TestUpgradeReleasedV0150KeepsFailedEmbeddingJobs(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			versions := seedRenditionCatalogVersions(t, legacy)
			profile := embeddingCatalogProfile(t)
			require.NoError(t, legacy.withStorageTx(ctx, func(tx *sql.Tx) error {
				return ensureProcessingProfileTx(ctx, tx, profile)
			}))
			request := embeddingJobTestRequest(t, legacy, versions[0], profile, "upgrade-failed")
			// Seed the released operational row in its original layout. Current
			// admission now records recovery provenance absent from this release.
			record := embeddingSetFixture(legacy, versions[0], profile.Fingerprint, document.EmbeddingInputOriginalFile, request.BindingID, "")
			record.InputGeneration = request.InputGeneration
			require.NoError(t, legacy.StageEmbeddingSet(ctx, record))
			authorization, err := legacy.AuthorizeProviderOperation(ctx, request.Authorization)
			require.NoError(t, err)
			jobID := embeddingJobID(legacy.VaultID(), versions[0], profile.Fingerprint, request.BindingID,
				document.EmbeddingInputOriginalFile, request.InputGeneration.ID, authorization.GrantID,
				authorization.ProcessingIncarnationID, authorization.RevocationFence)
			at := time.Now().UTC().Format(timestampLayout)
			for _, bindingID := range []string{"optional", "required"} {
				id := embeddingJobID(legacy.VaultID(), versions[0], profile.Fingerprint, bindingID, document.EmbeddingInputOriginalFile, request.InputGeneration.ID, authorization.GrantID, authorization.ProcessingIncarnationID, authorization.RevocationFence)
				_, err = db.Exec(`INSERT INTO embedding_jobs(job_id,vault_uid,content_version_id,profile_fingerprint,
				binding_id,input_kind,generation_id,vector_space_id,principal,scope,authorization_grant_id,
				authorization_incarnation_id,authorization_revocation_fence,state,failure_code,available_at,created_at,updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'failed','input_rejected',?,?,?)`,
					id, legacy.VaultID(), versions[0], profile.Fingerprint, bindingID, document.EmbeddingInputOriginalFile,
					request.InputGeneration.ID, record.VectorSpace.ID, request.Authorization.Principal, request.Authorization.Scope,
					authorization.GrantID, authorization.ProcessingIncarnationID, authorization.RevocationFence, at, at, at)
				require.NoError(t, err)
			}
			fingerprints := []string{request.Descriptor.Fingerprint}
			reconcile := func(s *Store) EmbeddingReconcileResult {
				result, err := s.ReconcileEmbeddingJobs(ctx, EmbeddingReconcileRequest{Mutate: embeddingTestMutation,
					At: time.Now().UTC(), Limit: 100, DescriptorFingerprints: fingerprints})
				require.NoError(t, err)
				return result
			}
			var released strings.Builder
			dumpReleasedTable(t, db, "embedding_jobs", &released)
			require.NoError(t, db.Close())

			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()

			var upgraded strings.Builder
			dumpReleasedTable(t, s.db, "embedding_jobs", &upgraded)
			assert.Equal(t, released.String(), upgraded.String())
			assert.Zero(t, reconcile(s).Enqueued, "reconciliation does not reopen released jobs")
			_, _, ok, err := s.ClaimEmbeddingWork(ctx, jobID, "worker:upgraded",
				time.Now().UTC().Add(time.Second), time.Minute, fingerprints)
			require.NoError(t, err)
			assert.False(t, ok, "a permanently failed job stays failed")
		})
	}
}

func TestUpgradeReleasedV0150KeepsPackagePreflight(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			pkg, run, _ := prepareReceivedPackage(t, legacy, "leased")
			request := packageImportJobRequest(t, legacy, pkg)
			released, err := legacy.PackagePreflight(ctx, request.Owner, request.PreflightID)
			require.NoError(t, err)
			require.NoError(t, db.Close())

			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()

			upgraded, err := s.PackagePreflight(ctx, request.Owner, request.PreflightID)
			require.NoError(t, err)
			assert.Equal(t, released, upgraded)
			job, err := s.AdmitPackageImport(ctx, run, pkg, request)
			require.NoError(t, err, "the preview admits its import without being rerun")
			assert.Equal(t, request.PreflightID, job.PreflightID)
		})
	}
}

func TestUpgradeReleasedV0150KeepsMailboxUpload(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			first, last := MailboxChunk{Index: 0, SHA256: fakeHash("ab0"), Size: MailboxChunkBytes},
				MailboxChunk{Index: 1, SHA256: fakeHash("ab1"), Size: 1}
			require.NoError(t, legacy.withStorageTx(ctx, func(tx *sql.Tx) error {
				if err := legacy.EnsureBlobTx(tx, first.SHA256, first.Size); err != nil {
					return err
				}
				return legacy.EnsureBlobTx(tx, last.SHA256, last.Size)
			}))
			upload := MailboxContainerRequest{ID: "upload", Owner: "one", SHA256: fakeHash("abc"),
				Size: MailboxChunkBytes + 1, Format: "mbox"}
			_, err := legacy.BeginMailboxContainer(ctx, upload)
			require.NoError(t, err)
			require.NoError(t, legacy.PutMailboxChunk(ctx, upload.Owner, upload.ID, first))
			released, err := legacy.MailboxContainer(ctx, upload.Owner, upload.ID)
			require.NoError(t, err)
			require.NoError(t, db.Close())

			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()

			upgraded, err := s.MailboxContainer(ctx, upload.Owner, upload.ID)
			require.NoError(t, err)
			assert.Equal(t, released, upgraded)
			require.NoError(t, s.PutMailboxChunk(ctx, upload.Owner, upload.ID, first), "accepted chunks stay accepted")
			require.NoError(t, s.PutMailboxChunk(ctx, upload.Owner, upload.ID, last), "the upload resumes")
		})
	}
}

func createReleasedFixtureFile(t *testing.T, legacy *Store, name, hash string, size int64, mime string) Node {
	t.Helper()
	var node Node
	require.NoError(t, legacy.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		node, _, err = legacy.createFileTx(t.Context(), tx, legacy.RootID(), name, hash, size, mime)
		return err
	}))
	return node
}

func normalizeReleasedUpgradeSnapshot(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		if record["type"] == "photo_file" {
			defaults := map[string]any{"revision": float64(1), "rating": float64(0), "flag": "", "label": "", "caption": "", "creator": "", "copyright": "", "rotation": float64(0), "confirmed_fields": float64(0), "flag_owner": ""}
			for key, value := range defaults {
				if _, ok := record[key]; !ok {
					record[key] = value
				}
			}
		}
		records = append(records, record)
	}
	return records
}

// A released photo with a sidecar is queued for XMP sync without a new mutation.
func TestUpgradeReleasedSchemasQueueSidecarPhotos(t *testing.T) {
	t.Parallel()
	for _, version := range []int{29, 41} {
		for _, driver := range v090UpgradeDrivers() {
			t.Run(fmt.Sprintf("v%d/%s", version, driver.name), func(t *testing.T) {
				ctx := t.Context()
				path := filepath.Join(t.TempDir(), "docbank.db")
				db, legacy := newReleasedFixtureStore(t, path, driver.driver, releasedSchemaFixtures[version], version)
				raw := createReleasedFixtureFile(t, legacy, "capture.ARW", fakeHash("a1"), 4, "application/octet-stream")
				jpeg := createReleasedFixtureFile(t, legacy, "capture.JPG", fakeHash("b2"), 4, "image/jpeg")
				sidecar := createReleasedFixtureFile(t, legacy, "capture.xmp", fakeHash("c3"), 4, "application/rdf+xml")
				seedReleasedPhotoAsset(t, db, jpeg.ID)
				const assetID = "40000000-0000-4000-8000-000000000001"
				rawID, sidecarID := uuid.New().String(), uuid.New().String()
				_, err := db.Exec(`INSERT INTO photo_files(file_id,asset_id,node_id,role,sidecar_of_file_id,created_at)
				VALUES(?,?,?,'raw',NULL,?), (?,?,?,'sidecar',?,?)`, rawID, assetID, raw.ID, raw.CreatedAt, sidecarID, assetID, sidecar.ID, rawID, sidecar.CreatedAt)
				require.NoError(t, err)
				_, err = db.Exec(`UPDATE photo_assets SET revision=2,display_file_id=? WHERE asset_id=?`, rawID, assetID)
				require.NoError(t, err)
				seedReleasedXMPOrientation(t, db, raw)
				require.NoError(t, db.Close())
				s, err := Open(path, driver.driver)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				targets, err := s.PhotoXMPTargetsAfter(ctx, "", 10)
				require.NoError(t, err)
				require.Contains(t, targets, PhotoSidecarTarget{FileID: rawID, NodeID: raw.ID, AssetID: assetID}, "released photos must be queued without a new mutation")
			})
		}
	}
}

// seedReleasedXMPOrientation writes evidence without today's technical projection,
// whose columns differ from the released layout.
func seedReleasedXMPOrientation(t *testing.T, db *sql.DB, raw Node) {
	t.Helper()
	canonical := photoCanonical(t, photoMetadataField("image.exif.orientation", "image.exif", "Orientation", photoInteger(1)))
	metadata, checksum, err := document.DecodeSourceMetadataV1(canonical)
	require.NoError(t, err)
	generationID := sourceMetadataGenerationID(raw.BlobHash, metadata.ContractVersion, fakeHash("ee"), checksum)
	_, err = db.Exec(`INSERT INTO source_metadata_generations(generation_id,source_sha256,contract_version,extractor_fingerprint,canonical_json,checksum,created_at) VALUES(?,?,?,?,?,?,?)`, generationID, raw.BlobHash, metadata.ContractVersion, fakeHash("ee"), canonical, checksum, raw.CreatedAt)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO source_metadata_heads(source_sha256,generation_id,published_at) VALUES(?,?,?)`, raw.BlobHash, generationID, raw.CreatedAt)
	require.NoError(t, err)
}

func seedReleasedPhotoAsset(t *testing.T, db *sql.DB, nodeID int64) {
	t.Helper()
	const assetID, fileID, receiptID = "40000000-0000-4000-8000-000000000001",
		"40000000-0000-4000-8000-000000000002", "40000000-0000-4000-8000-000000000003"
	const at = "2026-10-05T12:00:00.000000000Z"
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`PRAGMA defer_foreign_keys=ON`)
	require.NoError(t, err)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO photo_assets(asset_id,kind,revision,display_file_id,created_at,updated_at) VALUES(?,'photo',1,?,?,?)`,
			[]any{assetID, fileID, at, at}},
		{`INSERT INTO photo_files(file_id,asset_id,node_id,role,created_at) VALUES(?,?,?,'image',?)`,
			[]any{fileID, assetID, nodeID, at}},
		{`INSERT INTO photo_change_receipts(receipt_id,operation,asset_id,before_revision,after_revision,before_json,after_json,created_at)
			VALUES(?,'create',?,0,1,'{}','{}',?)`, []any{receiptID, assetID, at}},
	} {
		_, err := tx.Exec(statement.query, statement.args...)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

func TestJSONLOmittedPhotoMarkSurvivesFaceRetraction(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			source := newTestStoreWithDriver(t, driver.driver)
			ctx := t.Context()
			person, err := source.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			node, face := faceForDecisionTest(t, source, person.PersonID)
			_, err = source.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
			require.NoError(t, err)

			var backup bytes.Buffer
			require.NoError(t, source.ExportMetadata(ctx, &backup))
			lines := bytes.Split(backup.Bytes(), []byte{'\n'})
			omitted := 0
			for i, line := range lines {
				if len(line) == 0 {
					continue
				}
				var record map[string]any
				require.NoError(t, json.Unmarshal(line, &record))
				kind, ok := record["type"].(string)
				require.True(t, ok)
				if kind != metadataPersonAssertionType {
					continue
				}
				photoMark, ok := record["photo_mark"].(bool)
				require.True(t, ok)
				require.True(t, photoMark)
				delete(record, "photo_mark")
				lines[i], err = json.Marshal(record)
				require.NoError(t, err)
				omitted++
			}
			require.Equal(t, 1, omitted)

			restored := newTestStoreWithDriver(t, driver.driver)
			require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(bytes.Join(lines, []byte{'\n'}))))
			decideFaceForTest(t, restored, node, face.FaceID, "", "retract")
			assertions, err := restored.DocumentPersonAssertions(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, assertions, 1)
			require.True(t, assertions[0].PhotoMark)
			require.Equal(t, personAssertionActionAssert, assertions[0].Action)
			require.NoError(t, restored.ValidateMetadata(ctx))
		})
	}
}

// seedReleasedMtime records an operational ingest observation of the node's current bytes.
func seedReleasedMtime(t *testing.T, db *sql.DB, nodeID int64, mtime string) {
	t.Helper()
	const at = "2026-10-05T12:00:00.000000000Z"
	var versionID, ingestID string
	require.NoError(t, db.QueryRow("SELECT current_version_id FROM nodes WHERE id=?", nodeID).Scan(&versionID))
	ingestID = "40000000-0000-4000-8000-000000000010"
	fact := metadataProvenance{Type: metadataProvenanceType, NodeID: nodeID, IngestID: ingestID, OriginalPath: "/shots/released.jpg", OriginalMTime: &mtime}
	identity, err := provenanceIdentity(fact)
	require.NoError(t, err)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO ingests(id,started_at,source_kind,source_desc) VALUES(?,?,?,?)", []any{ingestID, at, "cli", "released"}},
		{"INSERT INTO provenance(identity,node_id,ingest_id,original_path,original_mtime) VALUES(?,?,?,?,?)", []any{identity, nodeID, ingestID, "/shots/released.jpg", mtime}},
		{"INSERT INTO provenance_version_bindings(provenance_identity,content_version_id,observed_at,basis_ref) VALUES(?,?,?,?)", []any{identity, versionID, at, provenanceVersionBindingBasis}},
	} {
		_, err := db.Exec(statement.query, statement.args...)
		require.NoError(t, err)
	}
}
