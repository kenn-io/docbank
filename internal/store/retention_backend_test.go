package store

import (
	"database/sql"
	"github.com/jackc/pgx/v5"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func checkRetentionCleanupPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	file, err := s.CreateFile(ctx, s.RootID(), "history.txt", fakeHash("a1"), 10, "text/plain")
	require.NoError(t, err)
	old := file.CurrentVersionID
	file, _, err = s.ReplaceContent(ctx, file.ID, file.Revision, fakeHash("b2"), 20, "text/plain")
	require.NoError(t, err)
	preview, err := s.PruneContentVersions(ctx, file.ID, file.Revision, VersionPruneSelector{KeepNewest: 1}, false)
	require.NoError(t, err)
	require.Len(t, preview.Candidates, 1)
	require.Equal(t, old, preview.Candidates[0].ID)
	require.Equal(t, int64(10), preview.ReleasableBytes)
	result, err := s.PruneContentVersions(ctx, file.ID, file.Revision, VersionPruneSelector{KeepNewest: 1}, true)
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedVersions)
	_, err = s.ContentVersionByID(ctx, old)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = s.Trash(ctx, file.ID, result.Node.Revision)
	require.NoError(t, err)
	empty, err := s.TrashEmpty(ctx, 0, false)
	require.NoError(t, err)
	require.Equal(t, int64(1), empty.Candidates)
	empty, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	require.Equal(t, int64(1), empty.Deleted)
	_, err = s.NodeByID(ctx, file.ID)
	require.ErrorIs(t, err, ErrNotFound)
	empty, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	require.Zero(t, empty.Candidates)
	require.Zero(t, empty.Deleted)
}

// Copy native SQLite fixtures only after their producing operations have run.
// PostgreSQL executes the cleanup through its runtime role and canonical DDL.
func copyRetentionStore(t *testing.T, source *Store) (*Store, *sql.DB) {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	target := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	_, err := setup.ExecContext(t.Context(), `UPDATE documents.vault_metadata SET vault_uid=$1`, source.vaultID)
	require.NoError(t, err)
	target.vaultID = source.vaultID
	tables := append(append([]string{}, postgresEnrollmentCatalogs...), "blob_locations", "blob_packs", "blob_pack_entries", "photo_source_claims", "photo_timeline", "document_event_state", "document_event_generations", "document_event_heads", "document_events", "document_event_actors", "document_event_primaries", "document_event_dirty", "text_searchable_versions", "text_extraction_queue")
	copyStoreCatalogFixture(t, source, setup, tables)
	target.primaryStoreID = source.primaryStoreID
	return target, setup
}

func checkRetainedCleanupFixturesPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	t.Run("PruneContentVersionsRetainsDependenciesAndCheckpointsAllPrior", func(t *testing.T) {
		t.Parallel()
		checkPruneContentVersionsRetainsDependenciesAndCheckpointsAllPrior(t, true)
	})
	t.Run("PruneContentVersionsAccountsForVisualPreviewOutputs", func(t *testing.T) { t.Parallel(); checkPruneContentVersionsAccountsForVisualPreviewOutputs(t, true) })
	t.Run("PruneContentVersionsAccountsForPageImages", func(t *testing.T) { t.Parallel(); checkPruneContentVersionsAccountsForPageImages(t, true) })
	t.Run("PruneContentVersionsReportsMixedLocationsAcrossStores", func(t *testing.T) { t.Parallel(); checkPruneContentVersionsReportsMixedLocationsAcrossStores(t, true) })
	t.Run("PruneContentVersionsCancelsOrphanedRenditionJob", func(t *testing.T) { t.Parallel(); checkPruneContentVersionsCancelsOrphanedRenditionJob(t, true) })
	t.Run("PruneContentVersionClearsDeletedAuthorityFromOperatorRequiredJob", func(t *testing.T) {
		t.Parallel()
		checkPruneContentVersionClearsDeletedAuthorityFromOperatorRequiredJob(t, true)
	})
	t.Run("PhotoTrashEmptyCompleteGroups", func(t *testing.T) { t.Parallel(); checkPhotoTrashEmptyCompleteGroups(t, true) })
	t.Run("SealedSnapshotRetainsSourceVersionDuringPrune", func(t *testing.T) { t.Parallel(); checkSealedSnapshotRetainsSourceVersionDuringPrune(t, true) })
	t.Run("TrashEmptySkipsSealedSnapshotSourceAndDeletesOtherTrash", func(t *testing.T) {
		t.Parallel()
		checkTrashEmptySkipsSealedSnapshotSourceAndDeletesOtherTrash(t, true)
	})
}

func checkRetentionMetadata(t *testing.T, s *Store) {
	t.Helper()
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		high, err := s.auditNodeSequence(t.Context(), tx)
		if err != nil {
			return err
		}
		return s.validateMetadataState(t.Context(), tx, high)
	}))
}

func checkCleanupSharedProcessingPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, stage := range []string{"provider", "build"} {
		t.Run(stage, func(t *testing.T) {
			source, versions := newRenditionCatalogFixture(t)
			ctx := t.Context()
			profile := catalogProcessingProfile(t, false)
			request := renditionJobTestRequest(versions[0], profile)
			other := renditionJobTestRequest(versions[1], profile)
			other.Authorization.Principal = "operator:remaining"
			grantRenditionJobConsent(t, source, request)
			grantRenditionJobConsent(t, source, other)
			job, waiter, err := source.EnqueueRenditionJob(ctx, request)
			require.NoError(t, err)
			_, remaining, err := source.EnqueueRenditionJob(ctx, other)
			require.NoError(t, err)
			now := time.Now().UTC()
			claim, err := source.ClaimRenditionJob(ctx, job.ID, "worker:cleanup", now, time.Minute)
			require.NoError(t, err)
			_, err = source.BeginRenditionProvider(ctx, claim, waiter.ID, now, renditionJobTestSnapshot(request))
			require.NoError(t, err)
			if stage == "provider" {
				require.NoError(t, source.CheckpointRenditionProvider(ctx, claim, "active-operation", now))
			} else {
				build := catalogRenditionBuild(source, profile)
				build.ID = job.ID
				require.NoError(t, source.StageRenditionJobBuild(ctx, claim, build, now))
			}
			node, err := source.NodeByPath(ctx, "/synthetic-source-a.pdf")
			require.NoError(t, err)
			node, _, err = source.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("d7"), 25, "application/pdf")
			require.NoError(t, err)
			target, _ := copyRetentionStore(t, source)
			for _, s := range []*Store{source, target} {
				_, err = s.PruneContentVersions(ctx, node.ID, node.Revision, VersionPruneSelector{VersionIDs: []string{versions[0]}}, true)
				require.NoError(t, err)
				current, err := s.RenditionJobByID(ctx, job.ID)
				require.NoError(t, err)
				require.Equal(t, 1, current.WaiterCount)
				var selected, owner, handle sql.NullString
				require.NoError(t, s.db.QueryRowContext(ctx, `SELECT selected_waiter_id,claim_owner,provider_resume_handle FROM rendition_jobs WHERE job_id=$1`, job.ID).Scan(&selected, &owner, &handle))
				if stage == "provider" {
					require.Equal(t, RenditionJobRunning, current.State)
					require.Equal(t, claim.Epoch, current.ClaimEpoch)
					require.Equal(t, remaining.ID, selected.String)
					require.Equal(t, claim.Owner, owner.String)
					require.Equal(t, "active-operation", handle.String)
				} else {
					require.Equal(t, RenditionJobQueued, current.State)
					require.Equal(t, claim.Epoch+1, current.ClaimEpoch)
					require.False(t, selected.Valid)
					require.False(t, owner.Valid)
					var epoch int64
					var active bool
					require.NoError(t, s.db.QueryRowContext(ctx, `SELECT fencing_token,active FROM current_rendition_roots WHERE root_id=$1`, renditionJobRootID("build", job.ID)).Scan(&epoch, &active))
					require.True(t, active)
					require.Equal(t, current.ClaimEpoch, epoch)
				}
				checkRetentionMetadata(t, s)
			}
		})
	}
}

func checkCleanupInvalidationAndRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, source, "note.txt", "a1", nil)
	person, err := source.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, err = source.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version.ID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	node, err := source.NodeByID(ctx, version.NodeID)
	require.NoError(t, err)
	node, _, err = source.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("d8"), 20, "image/jpeg")
	require.NoError(t, err)
	target, setup := copyRetentionStore(t, source)
	read := func(s *Store) [4]int64 {
		var out [4]int64
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT
   (SELECT binding_epoch FROM document_people_state),
   (SELECT publication_epoch FROM document_event_state),
   (SELECT count(*) FROM person_document_assertions),
   (SELECT count(*) FROM document_event_heads)`).Scan(&out[0], &out[1], &out[2], &out[3]))
		return out
	}
	before := read(target)
	require.Positive(t, before[2])
	require.Positive(t, before[3])
	var role string
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	// Deny the final version deletion after the revision and epochs have changed.
	_, err = setup.ExecContext(ctx, `REVOKE DELETE ON documents.content_versions FROM `+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	selector := VersionPruneSelector{VersionIDs: []string{version.ID}}
	_, err = target.PruneContentVersions(ctx, node.ID, node.Revision, selector, true)
	require.ErrorContains(t, err, "permission denied for table content_versions")
	require.Equal(t, before, read(target))
	retained, err := target.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	require.Equal(t, node, retained)
	_, err = setup.ExecContext(ctx, `GRANT DELETE ON documents.content_versions TO `+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	for _, s := range []*Store{source, target} {
		_, err = s.PruneContentVersions(ctx, node.ID, node.Revision, selector, true)
		require.NoError(t, err)
		require.Equal(t, [4]int64{before[0] + 1, before[1] + 1, 0, 0}, read(s))
		checkRetentionMetadata(t, s)
	}
}

func checkCleanupEmailRetentionPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	fixture := newEmailFixture(t, source, "source.eml")
	view, err := source.PublishEmailGeneration(ctx, fixture.publication)
	require.NoError(t, err)
	receipt, err := source.PublishEmailDocuments(ctx, attachmentRequest(t, source, view, "retained"))
	require.NoError(t, err)
	parent, _, err := source.ReplaceContent(ctx, view.Version.NodeID, UnconditionalRev, fakeHash("d1"), 8, "text/plain")
	require.NoError(t, err)
	child, _, err := source.ReplaceContent(ctx, receipt.Relations[0].Child.NodeID, UnconditionalRev, fakeHash("d2"), 9, "text/plain")
	require.NoError(t, err)
	target, _ := copyRetentionStore(t, source)
	for _, s := range []*Store{source, target} {
		for i, node := range []Node{parent, child} {
			version := []string{view.Version.ID, receipt.Relations[0].Child.VersionID}[i]
			_, err := s.PruneContentVersions(ctx, node.ID, node.Revision, VersionPruneSelector{VersionIDs: []string{version}}, true)
			require.ErrorIs(t, err, ErrEmailDocumentConflict)
			require.ErrorContains(t, err, receipt.OperationID)
			retained, err := s.NodeByID(ctx, node.ID)
			require.NoError(t, err)
			require.Equal(t, node, retained)
			_, _, err = s.Trash(ctx, node.ID, node.Revision)
			require.NoError(t, err)
		}
		other, err := s.CreateFile(ctx, s.RootID(), "unrelated.txt", fakeHash("d3"), 10, "text/plain")
		require.NoError(t, err)
		_, _, err = s.Trash(ctx, other.ID, other.Revision)
		require.NoError(t, err)
		empty, err := s.TrashEmpty(ctx, 0, true)
		require.NoError(t, err)
		require.Equal(t, int64(2), empty.Retained)
		require.Equal(t, int64(1), empty.Deleted)
		checkRetentionMetadata(t, s)
	}
}

func checkCleanupPhotoReceiptsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	asset, nodes := photoTrashFixture(t, source)
	raw := fileByRole(asset.Files, PhotoRoleRAW)
	importXMP(t, source, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating}, "retained keyword")
	current, err := source.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	_, err = source.TrashPhotoAsset(ctx, asset.ID, current.Revision)
	require.NoError(t, err)
	target, setup := copyRetentionStore(t, source)
	before := readTrashPhotoState(t, target, asset.ID)
	require.NotEmpty(t, before.Receipts)
	var role string
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	_, err = setup.ExecContext(ctx, `REVOKE DELETE ON documents.nodes FROM `+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	_, err = target.TrashEmpty(ctx, 0, true)
	require.ErrorContains(t, err, "permission denied for table nodes")
	require.Equal(t, before, readTrashPhotoState(t, target, asset.ID))
	_, err = setup.ExecContext(ctx, `GRANT DELETE ON documents.nodes TO `+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	for _, s := range []*Store{source, target} {
		result, err := s.TrashEmptyBounded(ctx, 0, 1, true)
		require.NoError(t, err)
		require.Equal(t, int64(len(nodes)), result.Deleted)
		checkRetentionMetadata(t, s)
	}
	require.Equal(t, readTrashPhotoState(t, source, asset.ID), readTrashPhotoState(t, target, asset.ID))
}

func checkCleanupEmbeddingAuthorityPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source, version, profile, _ := newEmbeddingCatalogFixture(t)
	ctx := t.Context()
	request := embeddingJobTestRequest(t, source, version, profile, "cleanup")
	job, err := source.EnqueueEmbeddingJob(ctx, request)
	require.NoError(t, err)
	_, _, found, err := source.ClaimNextEmbeddingWork(ctx, "worker:cleanup", time.Now().UTC(), time.Hour, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	old, err := source.ContentVersionByID(ctx, version)
	require.NoError(t, err)
	node, _, err := source.ReplaceContent(ctx, old.NodeID, UnconditionalRev, fakeHash("e3"), 24, "application/pdf")
	require.NoError(t, err)
	target, _ := copyRetentionStore(t, source)
	for _, s := range []*Store{source, target} {
		_, err := s.PruneContentVersions(ctx, node.ID, node.Revision, VersionPruneSelector{VersionIDs: []string{version}}, true)
		require.NoError(t, err)
		var jobs, roots, sets, attachments, generations int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT
   (SELECT count(*) FROM embedding_jobs WHERE job_id=$1),
   (SELECT count(*) FROM current_rendition_roots WHERE root_id=$1),
   (SELECT count(*) FROM embedding_sets WHERE content_version_id=$2),
   (SELECT count(*) FROM rendition_attachments WHERE content_version_id=$2),
   (SELECT count(*) FROM embedding_input_generations WHERE generation_id=$3)`, job.ID, version, request.InputGeneration.ID).Scan(&jobs, &roots, &sets, &attachments, &generations))
		require.Zero(t, jobs)
		require.Zero(t, roots)
		require.Zero(t, sets)
		require.Zero(t, attachments)
		require.Equal(t, 1, generations, "immutable generation authority remains for derivative collection")
		checkRetentionMetadata(t, s)
	}
}

func checkCleanupSidecarHistoryPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, source)
	raw := fileByRole(asset.Files, PhotoRoleRAW)
	evidence, err := source.BeginIngest(ctx, "Embedded:agent", "Sidecar evidence")
	require.NoError(t, err)
	sidecar, err := source.IngestFileExact(ctx, evidence, source.RootID(), "capture.xmp", fakeHash("c3"), 4, "application/rdf+xml", "/evidence/sidecar.xmp", "")
	require.NoError(t, err)
	_, err = source.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &raw.ID)
	require.NoError(t, err)
	importXMP(t, source, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
	facts, err := source.NodeProvenance(ctx, sidecar.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, facts.Items, 1)
	require.Equal(t, "/evidence/sidecar.xmp", facts.Items[0].OriginalPath)
	sidecar, err = source.NodeByID(ctx, sidecar.ID)
	require.NoError(t, err)
	current, _, err := source.ReplaceContent(ctx, sidecar.ID, sidecar.Revision, fakeHash("e5"), 50, "application/rdf+xml")
	require.NoError(t, err)
	target, _ := copyRetentionStore(t, source)
	for _, s := range []*Store{source, target} {
		outside, err := photoXMPSourceVersionsTx(ctx, s.db, sidecar.ID)
		require.NoError(t, err)
		for _, observation := range outside {
			require.NotEqual(t, "/evidence/sidecar.xmp", observation.Path)
		}
		result, err := s.PruneContentVersions(ctx, current.ID, current.Revision, VersionPruneSelector{KeepNewest: 1}, true)
		require.NoError(t, err)
		require.Empty(t, result.Candidates)
		require.Len(t, result.DependencyRetained, 1)
		require.Equal(t, sidecar.CurrentVersionID, result.DependencyRetained[0].ID)
		retained, err := s.NodeByID(ctx, current.ID)
		require.NoError(t, err)
		require.Equal(t, current, retained)
	}
}

func checkCleanupPhotoTimelinePostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	first, err := source.BeginIngest(ctx, "cli", "Original observation")
	require.NoError(t, err)
	node, err := source.IngestFileExact(ctx, first, source.RootID(), "photo.jpg", fakeHash("a1"), 8, "image/jpeg", "/first/photo.jpg", "2024-01-02T03:04:05Z")
	require.NoError(t, err)
	node, _, err = source.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("b2"), 9, "image/jpeg")
	require.NoError(t, err)
	last, err := source.BeginIngest(ctx, "cli", "Later observation")
	require.NoError(t, err)
	receipt, _, err := source.ReplaceContentForIngest(ctx, last, node.ID, node.Revision, fakeHash("a1"), 8, "image/jpeg", "/later/photo.jpg", "2024-03-04T05:06:07Z")
	require.NoError(t, err)
	target, _ := copyRetentionStore(t, source)
	read := func(s *Store) [3]string {
		var out [3]string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT c.original_path,c.original_mtime,t.day FROM photo_files f JOIN photo_source_claims c ON c.file_id=f.file_id JOIN photo_timeline t ON t.file_id=f.file_id WHERE f.node_id=$1`, node.ID).Scan(&out[0], &out[1], &out[2]))
		return out
	}
	require.Equal(t, [3]string{"/first/photo.jpg", "2024-01-02T03:04:05Z", "2024-01-02"}, read(target))
	for _, s := range []*Store{source, target} {
		result, err := s.PruneContentVersions(ctx, node.ID, receipt.Node.Revision, VersionPruneSelector{KeepNewest: 1}, true)
		require.NoError(t, err)
		require.Equal(t, 2, result.DeletedVersions)
		require.Equal(t, [3]string{"/later/photo.jpg", "2024-03-04T05:06:07Z", "2024-03-04"}, read(s))
		checkRetentionMetadata(t, s)
	}
}
