package store

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionPruneBlobStatsBatchesCandidateHashes(t *testing.T) {
	s := newTestStore(t)
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	hashes := make([]string, 501)
	for index := range hashes {
		hashes[index] = fmt.Sprintf("%064x", index+1)
	}
	stats, err := versionPruneBlobStatsTx(tx, hashes)
	require.NoError(t, err)
	assert.Empty(t, stats)
}

func TestPruneContentVersionsPreviewRunAndMetadataRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "history.txt", fakeHash("a1"), 10, "text/plain")
	require.NoError(t, err)
	replaced, second, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("b2"), 20, "text/plain",
	)
	require.NoError(t, err)
	replaced, current, err := s.ReplaceContent(
		ctx, created.ID, replaced.Revision, fakeHash("c3"), 30, "text/plain",
	)
	require.NoError(t, err)

	preview, err := s.PruneContentVersions(ctx, created.ID, replaced.Revision,
		VersionPruneSelector{KeepNewest: 2}, false)
	require.NoError(t, err)
	assert.False(t, preview.Run)
	assert.False(t, preview.Changed)
	assert.Equal(t, int64(10), preview.LogicalBytes)
	assert.Equal(t, 1, preview.UniqueBlobs)
	assert.Equal(t, 1, preview.ReleasableBlobs)
	assert.Equal(t, int64(10), preview.ReleasableBytes)
	assert.Equal(t, 1, preview.LooseBlobsPendingGC)
	require.Len(t, preview.Candidates, 1)
	assert.Equal(t, created.CurrentVersionID, preview.Candidates[0].ID)

	_, err = s.PruneContentVersions(ctx, created.ID, created.Revision,
		VersionPruneSelector{KeepNewest: 2}, true)
	require.ErrorIs(t, err, ErrStaleRevision)

	receipt, err := s.PruneContentVersions(ctx, created.ID, replaced.Revision,
		VersionPruneSelector{KeepNewest: 2}, true)
	require.NoError(t, err)
	assert.True(t, receipt.Run)
	assert.True(t, receipt.Changed)
	assert.Equal(t, 1, receipt.DeletedVersions)
	assert.Equal(t, replaced.Revision+1, receipt.Node.Revision)
	assert.Equal(t, current.ID, receipt.Node.CurrentVersionID)
	_, err = s.ContentVersionByID(ctx, created.CurrentVersionID)
	require.ErrorIs(t, err, ErrNotFound)
	versions, total, err := s.ContentVersions(ctx, created.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, versions, 2)
	assert.Equal(t, current.ID, versions[0].ID)
	assert.Equal(t, second.ID, versions[1].ID)

	unreachable, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	require.Len(t, unreachable, 1)
	assert.Equal(t, fakeHash("a1"), unreachable[0].Hash)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	restoredNode, err := restored.NodeByPath(ctx, "/history.txt")
	require.NoError(t, err)
	assert.Equal(t, receipt.Node.Revision, restoredNode.Revision)
	restoredVersions, restoredTotal, err := restored.ContentVersions(ctx, restoredNode.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, restoredTotal)
	assert.Equal(t, []string{current.ID, second.ID},
		[]string{restoredVersions[0].ID, restoredVersions[1].ID})
}

func TestPruneContentVersionsRemovesHistoricalRenditionAttachment(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-24T15:00:00.000000000Z",
	}
	require.NoError(t, s.AttachRenditionBuild(ctx, attachment))
	require.NoError(t, s.PublishRenditionHead(ctx, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: "2026-08-24T15:01:00.000000000Z",
	}))

	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(
		ctx, node.ID, node.Revision, fakeHash("prune-current"), 21, "application/pdf",
	)
	require.NoError(t, err)
	receipt, err := s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{versions[0]}}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, receipt.DeletedVersions)

	var attachments, heads, builds int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_attachments`).Scan(&attachments))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&heads))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&builds))
	assert.Equal(t, []int{0, 0, 1}, []int{attachments, heads, builds},
		"version deletion revokes version authority while shared immutable builds remain for GC")
}

func TestPruneContentVersionsCancelsOrphanedRenditionJob(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "worker:version-prune", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(ctx, claim, waiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(ctx, claim, build, now.Add(2*time.Second)))

	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(
		ctx, node.ID, node.Revision, fakeHash("pruned-job-current"), 24, "application/pdf",
	)
	require.NoError(t, err)
	receipt, err := s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{versions[0]}}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, receipt.DeletedVersions)

	_, err = s.RenditionJobByID(ctx, job.ID)
	require.ErrorIs(t, err, ErrNotFound)
	var waiters int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM rendition_job_waiters WHERE job_id=?`, job.ID,
	).Scan(&waiters))
	assert.Zero(t, waiters)
	var roots int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM current_rendition_roots
		WHERE root_id IN (?,?)`, renditionJobRootID("build", job.ID),
		renditionJobRootID("generation", job.ID)).Scan(&roots))
	assert.Zero(t, roots)
}

func TestPruneContentVersionQuarantinesAmbiguousSharedRenditionJob(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:remaining-waiter"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, firstWaiter, err := s.EnqueueRenditionJob(ctx, firstRequest)
	require.NoError(t, err)
	_, _, err = s.EnqueueRenditionJob(ctx, secondRequest)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "worker:ambiguous-prune", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(ctx, claim, firstWaiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(firstRequest))
	require.NoError(t, err)

	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(
		ctx, node.ID, node.Revision, fakeHash("ambiguous-prune-current"), 25, "application/pdf",
	)
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{versions[0]}}, true)
	require.NoError(t, err)

	current, err := s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobOperatorRequired, current.State)
	assert.Equal(t, RenditionFailureAmbiguous, current.FailureCode)
	assert.Equal(t, 1, current.WaiterCount)
	require.ErrorIs(t,
		s.CheckpointRenditionProvider(ctx, claim, "late-provider-handle", now.Add(3*time.Second)),
		ErrRenditionJobFenced,
	)
}

func TestPruneSoleWaiterKeepsAmbiguousJobSourceOutOfLooseAndPackedGC(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(
		ctx, s.RootID(), "single-waiter-source.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(created.CurrentVersionID, profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "worker:sole-ambiguous-prune", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(ctx, claim, waiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)

	node, _, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, testSHA256([]byte("sole-waiter-replacement")),
		21, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, true)
	require.NoError(t, err)
	current, err := s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobOperatorRequired, current.State)
	assert.Zero(t, current.WaiterCount)

	unreachable, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	for _, candidate := range unreachable {
		assert.NotEqual(t, catalogSourceHash, candidate.Hash,
			"an ambiguous tombstone retains exact source authority")
	}
	page, err := s.UnreachableBlobsPageFrom(ctx, nil, 100)
	require.NoError(t, err)
	for _, candidate := range page.Items {
		assert.NotEqual(t, catalogSourceHash, candidate.Hash,
			"paged GC eligibility is shared by loose and packed storage")
	}

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadataForRestore(ctx, bytes.NewReader(exported.Bytes())))
	restoredJob, err := restored.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobOperatorRequired, restoredJob.State)
	assert.Zero(t, restoredJob.WaiterCount)

	retryFile, err := restored.CreateFile(
		ctx, restored.RootID(), "same-source-retry.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	retryRequest := request
	retryRequest.ContentVersionID = retryFile.CurrentVersionID
	grantRenditionJobConsent(t, restored, retryRequest)
	_, _, err = restored.EnqueueRenditionJob(ctx, retryRequest)
	require.ErrorIs(t, err, ErrRenditionJobOperatorRequired,
		"metadata restore must not join or restart a zero-waiter ambiguity fence")
}

func TestPruneContentVersionsRetainsDependenciesAndCheckpointsAllPrior(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "reverted.txt", fakeHash("d4"), 40, "text/plain")
	require.NoError(t, err)
	replaced, replacement, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("e5"), 50, "text/markdown",
	)
	require.NoError(t, err)
	reverted, revertVersion, _, err := s.RevertContent(
		ctx, created.ID, replaced.Revision, created.CurrentVersionID,
	)
	require.NoError(t, err)

	protected, err := s.PruneContentVersions(ctx, created.ID, reverted.Revision,
		VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, false)
	require.NoError(t, err)
	assert.Empty(t, protected.Candidates)
	require.Len(t, protected.DependencyRetained, 1)
	assert.Equal(t, created.CurrentVersionID, protected.DependencyRetained[0].ID)
	unchanged, err := s.PruneContentVersions(ctx, created.ID, reverted.Revision,
		VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, true)
	require.NoError(t, err)
	assert.False(t, unchanged.Changed)
	assert.Equal(t, reverted.Revision, unchanged.Node.Revision)

	preview, err := s.PruneContentVersions(ctx, created.ID, reverted.Revision,
		VersionPruneSelector{AllPrior: true}, false)
	require.NoError(t, err)
	assert.True(t, preview.CheckpointRequired)
	assert.Len(t, preview.Candidates, 3)
	assert.Equal(t, int64(130), preview.LogicalBytes)
	assert.Equal(t, 2, preview.UniqueBlobs)
	assert.Equal(t, 1, preview.SharedBlobs,
		"the checkpoint keeps the current/reverted blob reachable")
	assert.Equal(t, 1, preview.ReleasableBlobs)
	assert.Equal(t, int64(50), preview.LooseBytesPendingGC)

	receipt, err := s.PruneContentVersions(ctx, created.ID, reverted.Revision,
		VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	assert.True(t, receipt.Changed)
	assert.Equal(t, 3, receipt.DeletedVersions)
	require.NotNil(t, receipt.Checkpoint)
	assert.Equal(t, "content_replace", receipt.Checkpoint.TransitionKind)
	assert.Nil(t, receipt.Checkpoint.SourceVersionID)
	assert.Equal(t, created.BlobHash, receipt.Checkpoint.BlobHash)
	assert.Equal(t, reverted.Revision+1, receipt.Node.Revision)
	assert.Equal(t, receipt.Checkpoint.ID, receipt.Node.CurrentVersionID)

	versions, total, err := s.ContentVersions(ctx, created.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, versions, 1)
	assert.Equal(t, receipt.Checkpoint.ID, versions[0].ID)
	for _, id := range []string{created.CurrentVersionID, replacement.ID, revertVersion.ID} {
		_, err = s.ContentVersionByID(ctx, id)
		require.ErrorIs(t, err, ErrNotFound)
	}
	unreachable, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	require.Len(t, unreachable, 1)
	assert.Equal(t, replacement.BlobHash, unreachable[0].Hash)
}

func TestPruneContentVersionsReportsPackedAndSharedConsequences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "packed.txt", fakeHash("f6"), 60, "text/plain")
	require.NoError(t, err)
	replaced, packedVersion, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("a7"), 70, "text/plain",
	)
	require.NoError(t, err)
	replaced, _, err = s.ReplaceContent(
		ctx, created.ID, replaced.Revision, fakeHash("b8"), 80, "text/plain",
	)
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "shared.txt", created.BlobHash, created.Size, "text/plain")
	require.NoError(t, err)
	addTestPack(t, s, "pack-test", 1, 17, nowRFC3339())
	addTestPackEntry(t, s, packedVersion.BlobHash, "pack-test", 0, 17, packedVersion.Size)
	_, err = s.db.Exec(`
		UPDATE blob_locations
		SET kind='packed',encoding=NULL,stored_size=17
		WHERE blob_hash=? AND store_id=?`, packedVersion.BlobHash, s.primaryStoreID)
	require.NoError(t, err)

	preview, err := s.PruneContentVersions(ctx, created.ID, replaced.Revision,
		VersionPruneSelector{KeepNewest: 1}, false)
	require.NoError(t, err)
	assert.Len(t, preview.Candidates, 2)
	assert.Equal(t, 2, preview.UniqueBlobs)
	assert.Equal(t, 1, preview.SharedBlobs)
	assert.Equal(t, 1, preview.ReleasableBlobs)
	assert.Equal(t, 1, preview.PackedBlobsPendingRepack)
	assert.Equal(t, int64(17), preview.PackedBytesPendingRepack)
	assert.Zero(t, preview.LooseBytesPendingGC)
}

func TestPruneContentVersionsReportsMixedLocationsAcrossStores(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "mixed.txt", fakeHash("d7"), 60, "text/plain")
	require.NoError(t, err)
	replaced, historical, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("e8"), 70, "text/plain",
	)
	require.NoError(t, err)
	current, _, err := s.ReplaceContent(
		ctx, created.ID, replaced.Revision, fakeHash("f9"), 80, "text/plain",
	)
	require.NoError(t, err)

	addTestPack(t, s, "mixed-pack", 1, 17, nowRFC3339())
	addTestPackEntry(t, s, historical.BlobHash, "mixed-pack", 0, 17, historical.Size)
	_, err = s.db.Exec(`
		UPDATE blob_locations
		SET kind='packed',encoding=NULL,stored_size=17
		WHERE blob_hash=? AND store_id=?`, historical.BlobHash, s.primaryStoreID)
	require.NoError(t, err)
	const secondaryID = "40000000-0000-4000-8000-000000000001"
	_, err = s.db.Exec(`
		INSERT INTO blob_stores(
			store_id,name,kind,role,lifecycle,binding,ownership_epoch,created_at
		) VALUES(?, 'archive', 'filesystem', 'secondary', 'active', 'archive', ?, ?)`,
		secondaryID, "40000000-0000-4000-8000-000000000002", nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.Exec(`
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?, ?, ?, 'loose', 'zstd', 11, 1)`, historical.BlobHash, secondaryID,
		"40000000-0000-4000-8000-000000000003")
	require.NoError(t, err)

	preview, err := s.PruneContentVersions(ctx, created.ID, current.Revision,
		VersionPruneSelector{VersionIDs: []string{historical.ID}}, false)
	require.NoError(t, err)
	assert.Equal(t, 1, preview.ReleasableBlobs)
	assert.Equal(t, 1, preview.LooseBlobsPendingGC)
	assert.Equal(t, int64(11), preview.LooseBytesPendingGC)
	assert.Equal(t, 1, preview.PackedBlobsPendingRepack)
	assert.Equal(t, int64(17), preview.PackedBytesPendingRepack)
	assert.Equal(t, 1, preview.MixedBlobsPendingMaintenance)
}

func TestPruneContentVersionsValidatesSelectorsAndTargets(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.txt", fakeHash("c9"), 90, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "second.txt", fakeHash("da"), 100, "text/plain")
	require.NoError(t, err)

	for name, selector := range map[string]VersionPruneSelector{
		"none":       {},
		"several":    {KeepNewest: 1, AllPrior: true},
		"zero age":   {OlderThan: 0},
		"negative":   {KeepNewest: -1},
		"too many":   {VersionIDs: make([]string, MaxVersionPruneIDs+1)},
		"duplicate":  {VersionIDs: []string{second.CurrentVersionID, second.CurrentVersionID}},
		"invalid id": {VersionIDs: []string{"not-a-uuid"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, pruneErr := s.PruneContentVersions(ctx, second.ID, second.Revision, selector, false)
			require.Error(t, pruneErr)
		})
	}
	_, err = s.PruneContentVersions(ctx, second.ID, second.Revision,
		VersionPruneSelector{VersionIDs: []string{second.CurrentVersionID}}, false)
	require.ErrorIs(t, err, ErrVersionAlreadyCurrent)
	_, err = s.PruneContentVersions(ctx, second.ID, second.Revision,
		VersionPruneSelector{VersionIDs: []string{first.CurrentVersionID}}, false)
	require.ErrorIs(t, err, ErrVersionNodeMismatch)
	_, err = s.PruneContentVersions(ctx, second.ID, second.Revision,
		VersionPruneSelector{OlderThan: time.Hour}, false)
	require.NoError(t, err)

	replaced, _, err := s.ReplaceContent(
		ctx, second.ID, second.Revision, fakeHash("eb"), 110, "text/plain",
	)
	require.NoError(t, err)
	old := time.Now().UTC().Add(-2 * time.Hour).Format(timestampLayout)
	_, err = s.db.Exec(`UPDATE content_versions SET recorded_at = ? WHERE version_id = ?`,
		old, second.CurrentVersionID)
	require.NoError(t, err)
	preview, err := s.PruneContentVersions(ctx, second.ID, replaced.Revision,
		VersionPruneSelector{OlderThan: time.Hour}, false)
	require.NoError(t, err)
	require.Len(t, preview.Candidates, 1)
	assert.Equal(t, second.CurrentVersionID, preview.Candidates[0].ID)
	assert.NotEmpty(t, preview.Cutoff)
}
