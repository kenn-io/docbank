package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimelineDirtyStateCascadesWithVersion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node, err := s.CreateFile(
		t.Context(), s.RootID(), "a.txt", fakeHash("a1"), 1, "text/plain",
	)
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return markDocumentEventDirtyTx(t.Context(), tx, node.CurrentVersionID, "test")
	}))
	_, err = s.db.Exec(`DELETE FROM nodes WHERE id=?`, node.ID)
	require.NoError(t, err)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_event_dirty`).Scan(&count))
	require.Zero(t, count)
}

func TestInvalidateDocumentEventsForVersionsRollsBackWithItsDirtyState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node, err := s.CreateFile(
		t.Context(), s.RootID(), "atomic.txt", fakeHash("a41"), 1, "text/plain",
	)
	require.NoError(t, err)
	generations := publishLifecycleDocumentEvents(t, s, node.CurrentVersionID)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER reject_timeline_dirty
			BEFORE INSERT ON document_event_dirty BEGIN
			SELECT RAISE(ABORT, 'forced dirty failure'); END`)
		return err
	}))

	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := invalidateDocumentEventsForVersionsTx(t.Context(), tx, []string{node.CurrentVersionID})
		return err
	})
	require.ErrorContains(t, err, "forced dirty failure")
	view, err := s.DocumentEventsForVersion(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, generations[node.CurrentVersionID], view.Generation.GenerationID,
		"head and generation deletion must roll back with dirty-state publication")
}

func TestChangedEvidenceRevokesPublishedDocumentEvents(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"metadata", "binding"} {
		t.Run(source, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			version, _ := ingestDocumentEventTarget(t, s, "observed.txt", "c81")
			copyNode, err := s.CreateFile(ctx, s.RootID(), "copy.txt", version.BlobHash, version.Size, version.MimeType)
			require.NoError(t, err)
			other, _ := ingestDocumentEventTarget(t, s, "other.txt", "c82")
			_, err = s.PublishSourceMetadata(ctx, version.BlobHash, fakeHash("c83"), mustSourceMetadata(t, "before"))
			require.NoError(t, err)
			target := requireDocumentEventTarget(t, s, version.ID)
			generations := publishLifecycleDocumentEvents(t, s, version.ID, copyNode.CurrentVersionID, other.ID)
			beforeDigest := requireDocumentEventInputsSHA256(t, s, target)
			beforeCoverage, err := s.DocumentEventCoverageReport(ctx)
			require.NoError(t, err)

			run, err := s.BeginIngest(ctx, "test", "Synthetic observation")
			require.NoError(t, err)
			changeEvidence := func() error {
				if source == "metadata" {
					_, err := s.PublishSourceMetadata(ctx, version.BlobHash, fakeHash("c84"), mustSourceMetadata(t, "after"))
					return err
				}
				observed, _, err := s.IngestFileWithMembership(ctx, run, s.RootID(), "observed.txt",
					version.BlobHash, version.Size, version.MimeType, "/synthetic/observed.txt", "2025-02-03T04:05:06Z")
				if err == nil {
					require.Equal(t, version.ID, observed.CurrentVersionID)
				}
				return err
			}

			_, err = s.db.Exec(`CREATE TRIGGER reject_timeline_dirty
				BEFORE INSERT ON document_event_dirty BEGIN
				SELECT RAISE(ABORT, 'forced dirty failure'); END`)
			require.NoError(t, err)
			require.ErrorContains(t, changeEvidence(), "forced dirty failure")
			require.Equal(t, beforeDigest, requireDocumentEventInputsSHA256(t, s, target),
				"evidence changes must roll back with invalidation")
			view, err := s.DocumentEventsForVersion(ctx, version.ID)
			require.NoError(t, err)
			require.Equal(t, generations[version.ID], view.Generation.GenerationID)
			_, err = s.db.Exec(`DROP TRIGGER reject_timeline_dirty`)
			require.NoError(t, err)

			require.NoError(t, changeEvidence())
			affected := []string{version.ID}
			unaffected := []string{other.ID}
			if source == "metadata" {
				affected = append(affected, copyNode.CurrentVersionID)
			} else {
				unaffected = append(unaffected, copyNode.CurrentVersionID)
			}
			for _, id := range affected {
				_, err := s.DocumentEventsForVersion(ctx, id)
				require.ErrorIs(t, err, ErrNotFound, "changed evidence must immediately revoke the old timeline")
				assertDocumentEventGenerationCount(t, s, generations[id], 0)
			}
			for _, id := range unaffected {
				view, err := s.DocumentEventsForVersion(ctx, id)
				require.NoError(t, err)
				require.Equal(t, generations[id], view.Generation.GenerationID)
			}
			afterCoverage, err := s.DocumentEventCoverageReport(ctx)
			require.NoError(t, err)
			require.Greater(t, afterCoverage.PublicationEpoch, beforeCoverage.PublicationEpoch)

			target = requireDocumentEventTarget(t, s, version.ID)
			digest := requireDocumentEventInputsSHA256(t, s, target)
			require.NotEqual(t, beforeDigest, digest)
			require.NoError(t, s.RecordDocumentEventAttempt(ctx, target, digest, "failed", []byte(`[]`)))
			_, err = s.DocumentEventsForVersion(ctx, version.ID)
			require.ErrorIs(t, err, ErrNotFound, "a failed rebuild must not expose the retired timeline")
			rebuilt := make(map[string]string, len(affected))
			for _, id := range affected {
				if id != version.ID {
					target = requireDocumentEventTarget(t, s, id)
				}
				record := coverageDocumentEventRecord(t, s.VaultID(), id, target.BlobHash, "created", false)
				generation, err := s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint,
					requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
				require.NoError(t, err)
				rebuilt[id] = generation.GenerationID
			}
			require.NoError(t, changeEvidence())
			for _, id := range affected {
				view, err := s.DocumentEventsForVersion(ctx, id)
				require.NoError(t, err)
				require.NotEqual(t, generations[id], view.Generation.GenerationID)
				require.Equal(t, rebuilt[id], view.Generation.GenerationID,
					"replaying unchanged evidence must preserve the rebuilt timeline")
			}
		})
	}
}

func TestLifecycleKeepsTimelineCoverageTruthful(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	first, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	second, err := s.NodeByPath(t.Context(), "/synthetic-source-b.pdf")
	require.NoError(t, err)
	historicalVersionID := versions[0]
	trashedVersionID := versions[1]
	first, current, err := s.ReplaceContent(
		t.Context(), first.ID, first.Revision, fakeHash("c51"), 21, "application/pdf",
	)
	require.NoError(t, err)

	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: trashedVersionID, BuildID: build.ID,
		Profile: profile, AttachedAt: "2026-09-12T09:00:00.000000000Z",
	}
	require.NoError(t, publishRenditionForTest(
		t, s, attachment, "2026-09-12T09:01:00.000000000Z", fakeHash("c52"),
	))

	generations := publishLifecycleDocumentEvents(
		t, s, historicalVersionID, current.ID, trashedVersionID,
	)
	coverage, err := s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(2), coverage.Selected)
	assert.Equal(t, int64(2), coverage.Indexed)
	assert.Zero(t, coverage.Pending)

	trashed, _, err := s.Trash(t.Context(), second.ID, second.Revision)
	require.NoError(t, err)
	_, err = s.DocumentEventsForVersion(t.Context(), trashedVersionID)
	require.NoError(t, err, "trash retains exact-version timeline rows")
	coverage, err = s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(2), coverage.Selected,
		"current retained file coverage includes trash")
	assert.Equal(t, int64(2), coverage.Indexed)

	beforePrune, err := s.DocumentEventStateRow(t.Context(), DocumentEventsDeriverFingerprint)
	require.NoError(t, err)
	receipt, err := s.PruneContentVersions(
		t.Context(), first.ID, first.Revision,
		VersionPruneSelector{VersionIDs: []string{historicalVersionID}}, true,
	)
	require.NoError(t, err)
	require.Equal(t, 1, receipt.DeletedVersions)
	afterPrune, err := s.DocumentEventStateRow(t.Context(), DocumentEventsDeriverFingerprint)
	require.NoError(t, err)
	assert.Greater(t, afterPrune.PublicationEpoch, beforePrune.PublicationEpoch)
	assertDocumentEventGenerationCount(t, s, generations[historicalVersionID], 0)
	_, err = s.DocumentEventsForVersion(t.Context(), historicalVersionID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.DocumentEventsForVersion(t.Context(), current.ID)
	require.NoError(t, err, "pruning one version must retain another version's head")

	purge, err := s.PurgeDerivatives(
		t.Context(), PurgeRequest{ContentVersionIDs: []string{trashedVersionID}},
	)
	require.NoError(t, err)
	require.Equal(t, 1, purge.RemovedHeads)
	_, err = s.ActiveRendition(t.Context(), trashedVersionID, profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.DocumentEventsForVersion(t.Context(), trashedVersionID)
	require.NoError(t, err, "rendition purge does not remove original-source timeline evidence")

	_, err = s.BumpDocumentEventInputEpoch(t.Context(), DocumentEventsDeriverFingerprint)
	require.NoError(t, err)
	drainLifecycleDocumentEventsOnce(t, s)
	_, err = s.ActiveRendition(t.Context(), trashedVersionID, profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound,
		"rebuilding the timeline must not recreate a purged rendition")

	invalidatedGeneration, err := s.DocumentEventsForVersion(t.Context(), trashedVersionID)
	require.NoError(t, err)
	var removed int64
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		removed, err = invalidateDocumentEventsForVersionsTx(t.Context(), tx, []string{trashedVersionID})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), removed)
	_, err = s.DocumentEventsForVersion(t.Context(), trashedVersionID)
	require.ErrorIs(t, err, ErrNotFound, "removed consumed evidence cannot serve an old head")
	assertDocumentEventGenerationCount(t, s, invalidatedGeneration.Generation.GenerationID, 0)
	var dirtyRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT revision FROM document_event_dirty
		WHERE content_version_id=?`, trashedVersionID).Scan(&dirtyRevision))
	require.Positive(t, dirtyRevision)
	coverage, err = s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), coverage.Indexed)
	assert.Equal(t, int64(1), coverage.Pending)

	restored, _, err := s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	require.Equal(t, trashedVersionID, restored.CurrentVersionID)
	coverage, err = s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), coverage.Indexed)
	assert.Equal(t, int64(1), coverage.Pending)
	drainLifecycleDocumentEventsOnce(t, s)
	coverage, err = s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(2), coverage.Indexed)
	assert.Zero(t, coverage.Pending)
	_, err = s.DocumentEventsForVersion(t.Context(), trashedVersionID)
	require.NoError(t, err)
}

func publishLifecycleDocumentEvents(
	t *testing.T, s *Store, versionIDs ...string,
) map[string]string {
	t.Helper()
	generations := make(map[string]string, len(versionIDs))
	for _, versionID := range versionIDs {
		target := requireDocumentEventTarget(t, s, versionID)
		record := coverageDocumentEventRecord(
			t, s.VaultID(), versionID, target.BlobHash, "created", false,
		)
		generation, err := s.PublishDocumentEvents(
			t.Context(), target, DocumentEventsDeriverFingerprint,
			requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record),
		)
		require.NoError(t, err)
		generations[versionID] = generation.GenerationID
	}
	return generations
}

func drainLifecycleDocumentEventsOnce(t *testing.T, s *Store) {
	t.Helper()
	targets, err := s.MissingDocumentEventTargetsAfter(
		t.Context(), DocumentEventsDeriverFingerprint, "", 100,
	)
	require.NoError(t, err)
	versionIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		versionIDs = append(versionIDs, target.ContentVersionID)
	}
	publishLifecycleDocumentEvents(t, s, versionIDs...)
	remaining, err := s.MissingDocumentEventTargetsAfter(
		t.Context(), DocumentEventsDeriverFingerprint, "", 100,
	)
	require.NoError(t, err)
	require.Empty(t, remaining, "one drain must publish every retained target")
}

func assertDocumentEventGenerationCount(t *testing.T, s *Store, generationID string, want int) {
	t.Helper()
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_event_generations
		WHERE generation_id=?`, generationID).Scan(&count))
	assert.Equal(t, want, count)
}
