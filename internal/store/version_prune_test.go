package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionPruneBlobStatsReturnsCandidateStats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	hashes := make([]string, 501)
	for index := range hashes {
		hashes[index] = fmt.Sprintf("%064x", index+1)
	}
	expected := make(map[string]pruneBlobStats)
	for _, index := range []int{499, 500} {
		size := int64(index + 1)
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("batch-%d.txt", index), hashes[index], size, "text/plain")
		require.NoError(t, err)
		expected[hashes[index]] = pruneBlobStats{refs: 1, size: size, looseLocations: 1, looseStored: size}
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	// Finish before t.Context cancellation can start an asynchronous rollback.
	defer func() { require.NoError(t, tx.Rollback()) }()
	stats, err := versionPruneBlobStatsTx(tx, hashes)
	require.NoError(t, err)
	assert.Equal(t, expected, stats)
}

func TestPruneContentVersionsPreviewRunAndMetadataRoundTrip(t *testing.T) {
	t.Parallel()
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

func TestPruneContentVersionsCancelsOrphanedRenditionJob(t *testing.T) {
	t.Parallel()
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

func TestPruneContentVersionKeepsCheckpointedProviderClaimWhileReselectingWaiter(t *testing.T) {
	t.Parallel()
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
	_, secondWaiter, err := s.EnqueueRenditionJob(ctx, secondRequest)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "worker:ambiguous-prune", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(ctx, claim, firstWaiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(firstRequest))
	require.NoError(t, err)
	require.NoError(t, s.CheckpointRenditionProvider(
		ctx, claim, "active-provider-handle", now.Add(2*time.Second)))

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
	assert.Equal(t, RenditionJobRunning, current.State)
	assert.Equal(t, claim.Epoch, current.ClaimEpoch)
	assert.Empty(t, current.FailureCode)
	assert.Equal(t, 1, current.WaiterCount)
	var selectedWaiter string
	require.NoError(t, s.db.QueryRow(
		`SELECT selected_waiter_id FROM rendition_jobs WHERE job_id=?`, job.ID,
	).Scan(&selectedWaiter))
	assert.Equal(t, secondWaiter.ID, selectedWaiter)
	_, err = s.ClaimRenditionJob(
		ctx, job.ID, "worker:concurrent-resume", now.Add(3*time.Second), time.Minute)
	require.ErrorIs(t, err, ErrRenditionJobLeaseHeld)
	require.NoError(t,
		s.CheckpointRenditionProvider(ctx, claim, "late-provider-handle", now.Add(4*time.Second)))
}

func TestPruneContentVersionClearsDeletedAuthorityFromOperatorRequiredJob(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(
		ctx, job.ID, "worker:operator-required-prune", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(
		ctx, claim, waiter.ID, now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	require.NoError(t, s.MarkRenditionJobOperatorRequired(
		ctx, claim, now.Add(2*time.Second)))

	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(
		ctx, node.ID, node.Revision, testSHA256([]byte("operator-required-prune-current")),
		27, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{versions[0]}}, true)
	require.NoError(t, err)

	current, err := s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobOperatorRequired, current.State)
	assert.Equal(t, RenditionFailureAmbiguous, current.FailureCode)
	var selected, grant, incarnation sql.NullString
	var revocationFence sql.NullInt64
	require.NoError(t, s.db.QueryRow(`SELECT selected_waiter_id,authorization_grant_id,
		authorization_incarnation_id,authorization_revocation_fence
		FROM rendition_jobs WHERE job_id=?`, job.ID).Scan(
		&selected, &grant, &incarnation, &revocationFence))
	assert.False(t, selected.Valid)
	assert.False(t, grant.Valid)
	assert.False(t, incarnation.Valid)
	assert.False(t, revocationFence.Valid)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
}

func TestPruneContentVersionRequeuesSharedStagedJobAndRetainsRoot(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:remaining-staged-waiter"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, firstWaiter, err := s.EnqueueRenditionJob(ctx, firstRequest)
	require.NoError(t, err)
	_, secondWaiter, err := s.EnqueueRenditionJob(ctx, secondRequest)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "worker:staged-prune", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(ctx, claim, firstWaiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(firstRequest))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(ctx, claim, build, now.Add(2*time.Second)))

	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(
		ctx, node.ID, node.Revision, testSHA256([]byte("shared staged prune current")),
		26, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{versions[0]}}, true)
	require.NoError(t, err)

	current, err := s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobQueued, current.State)
	assert.Equal(t, RenditionPhaseBuildStaged, current.Phase)
	assert.Equal(t, 1, current.WaiterCount)
	var active bool
	var rootEpoch int64
	require.NoError(t, s.db.QueryRow(`SELECT active FROM current_rendition_roots
		WHERE root_id=?`, renditionJobRootID("build", job.ID)).Scan(&active))
	assert.True(t, active)
	require.NoError(t, s.db.QueryRow(`SELECT fencing_token FROM current_rendition_roots
		WHERE root_id=?`, renditionJobRootID("build", job.ID)).Scan(&rootEpoch))
	assert.Equal(t, current.ClaimEpoch, rootEpoch)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported),
		"requeue must preserve backup and audit root invariants")
	require.ErrorIs(t,
		s.MarkRenditionJobFailed(ctx, claim, RenditionFailureTerminal, now.Add(3*time.Second)),
		ErrRenditionJobFenced)

	reclaimed, err := s.ClaimRenditionJob(
		ctx, job.ID, "worker:remaining-staged-prune", now.Add(4*time.Second), time.Minute)
	require.NoError(t, err)
	work, err := s.RenditionJobWorkByClaim(ctx, reclaimed, now.Add(4*time.Second))
	require.NoError(t, err)
	assert.Equal(t, secondWaiter.ID, work.Waiter.ID)
}

func TestPruneContentVersionRequeuesFailedSharedStagedJob(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:remaining-failed-waiter"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, firstWaiter, err := s.EnqueueRenditionJob(ctx, firstRequest)
	require.NoError(t, err)
	_, secondWaiter, err := s.EnqueueRenditionJob(ctx, secondRequest)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(
		ctx, job.ID, "worker:failed-staged-prune", now, time.Minute)
	require.NoError(t, err)
	work, err := s.RenditionJobWorkByClaim(ctx, claim, now.Add(time.Second))
	require.NoError(t, err)
	selectedRequest := firstRequest
	selectedPath := "/synthetic-source-a.pdf"
	remainingWaiter := secondWaiter
	if work.Waiter.ID == secondWaiter.ID {
		selectedRequest = secondRequest
		selectedPath = "/synthetic-source-b.pdf"
		remainingWaiter = firstWaiter
	}
	_, err = s.BeginRenditionProvider(
		ctx, claim, work.Waiter.ID, now.Add(time.Second),
		renditionJobTestSnapshot(selectedRequest))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(
		ctx, claim, build, now.Add(2*time.Second)))
	require.NoError(t, s.MarkRenditionJobFailed(
		ctx, claim, RenditionFailureConsent, now.Add(3*time.Second)))

	node, err := s.NodeByPath(ctx, selectedPath)
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(
		ctx, node.ID, node.Revision, testSHA256([]byte("failed staged prune current")),
		28, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{selectedRequest.ContentVersionID}}, true)
	require.NoError(t, err)

	current, err := s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobQueued, current.State)
	assert.Equal(t, RenditionPhaseBuildStaged, current.Phase)
	assert.Empty(t, current.FailureCode)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	reclaimed, err := s.ClaimRenditionJob(
		ctx, job.ID, "worker:remaining-failed-staged", now.Add(4*time.Second), time.Minute)
	require.NoError(t, err)
	reclaimedWork, err := s.RenditionJobWorkByClaim(
		ctx, reclaimed, now.Add(4*time.Second))
	require.NoError(t, err)
	assert.Equal(t, remainingWaiter.ID, reclaimedWork.Waiter.ID)
}

func TestPruneContentVersionClearsDeletedAuthorityFromTerminalFailedJob(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:remaining-terminal-waiter"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, _, err := s.EnqueueRenditionJob(ctx, firstRequest)
	require.NoError(t, err)
	_, _, err = s.EnqueueRenditionJob(ctx, secondRequest)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(
		ctx, job.ID, "worker:terminal-prune", now, time.Minute)
	require.NoError(t, err)
	work, err := s.RenditionJobWorkByClaim(ctx, claim, now.Add(time.Second))
	require.NoError(t, err)
	selectedRequest := firstRequest
	selectedPath := "/synthetic-source-a.pdf"
	if work.Waiter.ContentVersionID == secondRequest.ContentVersionID {
		selectedRequest = secondRequest
		selectedPath = "/synthetic-source-b.pdf"
	}
	require.NoError(t, s.MarkRenditionJobFailed(
		ctx, claim, RenditionFailureTerminal, now.Add(2*time.Second)))

	node, err := s.NodeByPath(ctx, selectedPath)
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(
		ctx, node.ID, node.Revision, testSHA256([]byte("terminal failed prune current")),
		29, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{selectedRequest.ContentVersionID}}, true)
	require.NoError(t, err)

	current, err := s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobFailed, current.State)
	assert.Equal(t, RenditionPhaseQueued, current.Phase)
	assert.Equal(t, RenditionFailureTerminal, current.FailureCode)
	var selected, grant, incarnation sql.NullString
	var revocationFence sql.NullInt64
	require.NoError(t, s.db.QueryRow(`SELECT selected_waiter_id,authorization_grant_id,
		authorization_incarnation_id,authorization_revocation_fence
		FROM rendition_jobs WHERE job_id=?`, job.ID).Scan(
		&selected, &grant, &incarnation, &revocationFence))
	assert.False(t, selected.Valid)
	assert.False(t, grant.Valid)
	assert.False(t, incarnation.Valid)
	assert.False(t, revocationFence.Valid)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
}

func TestPruneSelectedWaiterReleasesRootsWhenOnlyRejectedWaitersRemain(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	firstRequest.Authorization.Principal = "operator:first-prune-candidate"
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:second-prune-candidate"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, firstWaiter, err := s.EnqueueRenditionJob(ctx, firstRequest)
	require.NoError(t, err)
	_, secondWaiter, err := s.EnqueueRenditionJob(ctx, secondRequest)
	require.NoError(t, err)
	rejectedRequest, selectedRequest := firstRequest, secondRequest
	selectedWaiter := secondWaiter
	selectedPath := "/synthetic-source-b.pdf"
	if secondWaiter.ID < firstWaiter.ID {
		rejectedRequest, selectedRequest = secondRequest, firstRequest
		selectedWaiter = firstWaiter
		selectedPath = "/synthetic-source-a.pdf"
	}
	_, err = s.RevokeConsent(ctx, ProcessingConsentRevocationRequest{
		Principal: rejectedRequest.Authorization.Principal,
		Scope:     rejectedRequest.Authorization.Scope,
	})
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "worker:rejected-sibling-prune", now, time.Minute)
	require.NoError(t, err)
	work, err := s.RenditionJobWorkByClaim(ctx, claim, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, selectedWaiter.ID, work.Waiter.ID)
	_, err = s.BeginRenditionProvider(ctx, claim, selectedWaiter.ID,
		now.Add(2*time.Second), renditionJobTestSnapshot(selectedRequest))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(ctx, claim, build, now.Add(3*time.Second)))

	node, err := s.NodeByPath(ctx, selectedPath)
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(
		ctx, node.ID, node.Revision, testSHA256([]byte("rejected sibling prune current")),
		27, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{selectedRequest.ContentVersionID}}, true)
	require.NoError(t, err)

	current, err := s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobFailed, current.State)
	assert.Equal(t, RenditionPhaseQueued, current.Phase)
	var activeRoots int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM current_rendition_roots
		WHERE root_id IN (?,?) AND active=1`, renditionJobRootID("build", job.ID),
		renditionJobRootID("generation", job.ID)).Scan(&activeRoots))
	assert.Zero(t, activeRoots)
	_, err = s.PurgeDerivatives(ctx, PurgeRequest{})
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
}

func TestPruneSoleWaiterKeepsAmbiguousJobSourceOutOfGC(t *testing.T) {
	t.Parallel()
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
	receipt, err := s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, receipt.SharedBlobs,
		"the job still pins the pruned version's bytes, so prune must not call them releasable")
	assert.Zero(t, receipt.ReleasableBlobs)
	assert.Zero(t, receipt.ReleasableBytes)
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
	_, err = s.db.Exec(
		`INSERT INTO derivative_blob_purge_pending(blob_hash) VALUES(?)`, catalogSourceHash)
	require.NoError(t, err)
	exactPurge, err := s.UnreachableDerivativePurgeBlobs(ctx)
	require.NoError(t, err)
	assert.Empty(t, exactPurge,
		"exact derivative GC must retain source authority for an ambiguous job")

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
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

func TestPruneSoleWaiterKeepsRetriedProviderResumeFence(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(
		ctx, job.ID, "worker:retried-provider-prune", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(
		ctx, claim, waiter.ID, now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	require.NoError(t, s.CheckpointRenditionProvider(
		ctx, claim, "retained-provider-handle", now.Add(2*time.Second)))
	require.NoError(t, s.MarkRenditionJobRetry(
		ctx, claim, RenditionFailureTransient,
		now.Add(3*time.Second), now.Add(4*time.Second)))

	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(
		ctx, node.ID, node.Revision, testSHA256([]byte("retried-provider-prune-current")),
		21, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{versions[0]}}, true)
	require.NoError(t, err)

	current, err := s.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobOperatorRequired, current.State)
	assert.Equal(t, RenditionPhaseProvider, current.Phase)
	assert.Equal(t, RenditionFailureAmbiguous, current.FailureCode)
	var resumeHandle string
	require.NoError(t, s.db.QueryRow(
		`SELECT provider_resume_handle FROM rendition_jobs WHERE job_id=?`, job.ID,
	).Scan(&resumeHandle))
	assert.Equal(t, "retained-provider-handle", resumeHandle)

	retryRequest := renditionJobTestRequest(versions[1], profile)
	retryRequest.Authorization.Principal = "operator:retry-after-prune"
	grantRenditionJobConsent(t, s, retryRequest)
	_, _, err = s.EnqueueRenditionJob(ctx, retryRequest)
	require.ErrorIs(t, err, ErrRenditionJobOperatorRequired,
		"fresh consent must not resubmit a provider operation with a durable handle")
}

func TestPruneContentVersionsRetainsAuthoritiesAndCheckpointsAllPrior(t *testing.T) {
	t.Parallel()
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
	for _, authority := range []string{"media source", "input artifact", "checkout entry"} {
		t.Run(authority, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			name, mediaType := "recording.mp3", "audio/mpeg"
			if authority == "checkout entry" {
				name, mediaType = "photo.jpg", "image/jpeg"
			}
			created, err := s.CreateFile(ctx, s.RootID(), name, fakeHash("a1"), 10, mediaType)
			require.NoError(t, err)
			if authority == "checkout entry" {
				checkout, operation, createErr := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{All: true}, "checkout", 10)
				require.NoError(t, createErr)
				require.NoError(t, s.FinishPhotoCheckout(ctx, checkout.ID, operation.ID, StorageOperationFailed, `{}`, "checkout failed", time.Time{}))
			}
			updated, _, err := s.ReplaceContent(ctx, created.ID, created.Revision, fakeHash("b2"), 20, mediaType)
			require.NoError(t, err)
			if authority != "checkout entry" {
				stamp := nowRFC3339()
				_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
					strings.Repeat("c", 64), stamp)
				require.NoError(t, err)
				if authority == "media source" {
					_, err = s.db.Exec(`INSERT INTO media_source_versions VALUES('source-version','source',1,?,?,?)`,
						created.CurrentVersionID, `{}`, stamp)
					require.NoError(t, err)
				} else {
					_, err = s.db.Exec(`INSERT INTO media_occurrences VALUES(
		'occurrence','source',NULL,'operator','ref','1','','','','{}',1,?,NULL)`, stamp)
					require.NoError(t, err)
					_, err = s.db.Exec(`INSERT INTO media_input_artifacts VALUES(
		'input','occurrence','source',NULL,?,'recording','supplied','','',?,?)`,
						created.CurrentVersionID, fakeHash("a1"), stamp)
					require.NoError(t, err)
				}
			}

			preview, err := s.PruneContentVersions(ctx, created.ID, updated.Revision,
				VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, false)
			require.NoError(t, err)
			require.Empty(t, preview.Candidates)
			require.Len(t, preview.DependencyRetained, 1)
			require.Equal(t, created.CurrentVersionID, preview.DependencyRetained[0].ID)

			run, err := s.PruneContentVersions(ctx, created.ID, updated.Revision,
				VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, true)
			require.NoError(t, err)
			require.False(t, run.Changed)
			require.Zero(t, run.DeletedVersions)
			_, err = s.ContentVersionByID(ctx, created.CurrentVersionID)
			require.NoError(t, err)
		})
	}
	for _, allPrior := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit selection", true: "all prior"}[allPrior], func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			created, err := s.CreateFile(ctx, s.RootID(), "recording.mp3", fakeHash("a1"), 10, "audio/mpeg")
			require.NoError(t, err)
			replaced, replacement, err := s.ReplaceContent(
				ctx, created.ID, created.Revision, fakeHash("b2"), 20, "audio/mpeg")
			require.NoError(t, err)
			reverted, revertVersion, _, err := s.RevertContent(
				ctx, created.ID, replaced.Revision, created.CurrentVersionID)
			require.NoError(t, err)
			_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
				strings.Repeat("c", 64), nowRFC3339())
			require.NoError(t, err)
			require.NoError(t, s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
				ID: "source-version", SourceID: "source", Revision: 1,
				ContentVersionID: revertVersion.ID, CaptureJSON: "{}",
			}))

			selector := VersionPruneSelector{AllPrior: true}
			node := reverted
			selected := []string{created.CurrentVersionID, replacement.ID, revertVersion.ID}
			if !allPrior {
				node, _, err = s.ReplaceContent(
					ctx, created.ID, reverted.Revision, fakeHash("d4"), 40, "audio/mpeg")
				require.NoError(t, err)
				selector = VersionPruneSelector{VersionIDs: selected}
			}

			preview, err := s.PruneContentVersions(ctx, created.ID, node.Revision, selector, false)
			require.NoError(t, err)
			require.Equal(t, []string{replacement.ID}, contentVersionIDs(preview.Candidates))
			require.ElementsMatch(t,
				[]string{created.CurrentVersionID, revertVersion.ID},
				contentVersionIDs(preview.DependencyRetained))
			require.False(t, preview.CheckpointRequired)

			run, err := s.PruneContentVersions(ctx, created.ID, node.Revision, selector, true)
			require.NoError(t, err)
			require.Equal(t, preview.Candidates, run.Candidates)
			require.Equal(t, preview.DependencyRetained, run.DependencyRetained)
			require.Equal(t, 1, run.DeletedVersions)
			require.Nil(t, run.Checkpoint)
			for _, id := range []string{created.CurrentVersionID, revertVersion.ID} {
				_, err = s.ContentVersionByID(ctx, id)
				require.NoError(t, err)
			}
			_, err = s.ContentVersionByID(ctx, replacement.ID)
			require.ErrorIs(t, err, ErrNotFound)
		})
	}

	t.Run("all prior with historical pinned revert", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		created, err := s.CreateFile(ctx, s.RootID(), "recording.mp3", fakeHash("a1"), 10, "audio/mpeg")
		require.NoError(t, err)
		replaced, replacement, err := s.ReplaceContent(
			ctx, created.ID, created.Revision, fakeHash("b2"), 20, "audio/mpeg")
		require.NoError(t, err)
		reverted, pinnedRevert, _, err := s.RevertContent(
			ctx, created.ID, replaced.Revision, created.CurrentVersionID)
		require.NoError(t, err)
		_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
			strings.Repeat("c", 64), nowRFC3339())
		require.NoError(t, err)
		require.NoError(t, s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
			ID: "source-version", SourceID: "source", Revision: 1,
			ContentVersionID: pinnedRevert.ID, CaptureJSON: "{}",
		}))
		updated, replacementAfterRevert, err := s.ReplaceContent(
			ctx, created.ID, reverted.Revision, fakeHash("d4"), 40, "audio/mpeg")
		require.NoError(t, err)
		current, currentRevert, _, err := s.RevertContent(
			ctx, created.ID, updated.Revision, replacement.ID)
		require.NoError(t, err)

		selector := VersionPruneSelector{AllPrior: true}
		preview, err := s.PruneContentVersions(ctx, created.ID, current.Revision, selector, false)
		require.NoError(t, err)
		require.ElementsMatch(t,
			[]string{currentRevert.ID, replacementAfterRevert.ID, replacement.ID},
			contentVersionIDs(preview.Candidates))
		require.ElementsMatch(t,
			[]string{pinnedRevert.ID, created.CurrentVersionID},
			contentVersionIDs(preview.DependencyRetained))
		require.True(t, preview.CheckpointRequired)

		run, err := s.PruneContentVersions(ctx, created.ID, current.Revision, selector, true)
		require.NoError(t, err)
		require.Equal(t, preview.Candidates, run.Candidates)
		require.Equal(t, preview.DependencyRetained, run.DependencyRetained)
		require.Equal(t, 3, run.DeletedVersions)
		require.NotNil(t, run.Checkpoint)
		require.Equal(t, replacement.BlobHash, run.Checkpoint.BlobHash)
		for _, id := range []string{pinnedRevert.ID, created.CurrentVersionID} {
			_, err = s.ContentVersionByID(ctx, id)
			require.NoError(t, err)
		}
		for _, id := range []string{currentRevert.ID, replacementAfterRevert.ID, replacement.ID} {
			_, err = s.ContentVersionByID(ctx, id)
			require.ErrorIs(t, err, ErrNotFound)
		}
	})
	t.Run("sealed snapshot", func(t *testing.T) {
		s := newTestStore(t)
		body := []byte("Original package source")
		hash := sha256.Sum256(body)
		node, err := s.CreateFile(t.Context(), s.RootID(), "original.txt", hex.EncodeToString(hash[:]), int64(len(body)), "text/plain")
		require.NoError(t, err)
		oldVersion := node.CurrentVersionID
		snapshotID, err := newUUIDv4()
		require.NoError(t, err)
		occurrence := strings.Repeat("e", 32)
		_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{
			SnapshotID: snapshotID,
			Members: []CollectionSnapshotMember{{
				Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID,
				ContentVersionID: oldVersion, BlobSHA256: hex.EncodeToString(hash[:]), Size: int64(len(body)),
				FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name,
				FrozenFieldsJSON: "{}", DocumentKind: "other",
			}},
		})
		require.NoError(t, err)
		replacement := []byte("New current version")
		replacementHash := sha256.Sum256(replacement)
		updated, _, err := s.ReplaceContent(t.Context(), node.ID, node.Revision,
			hex.EncodeToString(replacementHash[:]), int64(len(replacement)), "text/plain")
		require.NoError(t, err)
		selector := VersionPruneSelector{VersionIDs: []string{oldVersion}}
		for _, run := range []bool{false, true} {
			_, err = s.PruneContentVersions(t.Context(), node.ID, updated.Revision, selector, run)
			require.ErrorIs(t, err, ErrPackageRetained)
		}
		_, err = s.ContentVersionByID(t.Context(), oldVersion)
		require.NoError(t, err)
	})
	for _, authority := range []string{"label", "receipt"} {
		t.Run(authority, func(t *testing.T) {
			s := newTestStore(t)
			pkg, node := seedReceivedPackage(t, s, authority)
			key, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
			require.NoError(t, err)
			occurrence := PackageOccurrenceID(pkg.PackageID, key)
			switch authority {
			case "label":
				_, err = s.db.ExecContext(t.Context(), `INSERT INTO package_labels(
					package_id,provenance,label_set,label,occurrence_id,content_version_id,
					page_state,endpoint) VALUES(?,?,?,?,?,?,?,?)`, pkg.PackageID, "received", "EXT",
					"EXT000001", occurrence, node.CurrentVersionID, "unknown", "begin")
			case "receipt":
				receiptID, idErr := newUUIDv4()
				require.NoError(t, idErr)
				_, err = s.db.ExecContext(t.Context(), `INSERT INTO package_import_receipts(
					receipt_id,package_id,record_key,occurrence_id,content_version_id,state,receipt_json,recorded_at
				) VALUES(?,?,?,?,?,?,?,?)`, receiptID, pkg.PackageID, key, occurrence, node.CurrentVersionID,
					"committed", []byte(`{}`), nowRFC3339())
				require.NoError(t, err)
			}
			require.NoError(t, err)
			updated, _, err := s.ReplaceContent(t.Context(), node.ID, node.Revision,
				fakeHash("d1"), 12, "text/plain")
			require.NoError(t, err)
			for _, run := range []bool{false, true} {
				_, err = s.PruneContentVersions(t.Context(), node.ID, updated.Revision,
					VersionPruneSelector{VersionIDs: []string{node.CurrentVersionID}}, run)
				require.ErrorIs(t, err, ErrPackageRetained)
			}
		})
	}
}

func TestPruneContentVersionsRemovesRenditionAttachment(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	versions, profiles, _ := seedProcessingMetadataCatalog(t, s)
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	replaced, _, err := s.ReplaceContent(
		t.Context(), node.ID, node.Revision, fakeHash("f7"), 21, "application/pdf",
	)
	require.NoError(t, err)

	receipt, err := s.PruneContentVersions(
		t.Context(), node.ID, replaced.Revision,
		VersionPruneSelector{VersionIDs: []string{versions[0]}}, true,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, receipt.DeletedVersions)
	_, err = s.ActiveRendition(t.Context(), versions[0], profiles[0].Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPruneContentVersionsAccountsForVisualPreviewOutputs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("91"), 10, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PublishVisualPreview(ctx, created.CurrentVersionID,
		readyVisualPreview(t, created.BlobHash, fakeHash("92"), 8),
		&BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 8})
	require.NoError(t, err)

	replaced, historical, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("93"), 20, "image/jpeg",
	)
	require.NoError(t, err)
	_, err = s.PublishVisualPreview(ctx, historical.ID,
		readyVisualPreview(t, historical.BlobHash, fakeHash("94"), 9),
		&BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)

	current, currentVersion, err := s.ReplaceContent(
		ctx, created.ID, replaced.Revision, fakeHash("95"), 30, "image/jpeg",
	)
	require.NoError(t, err)
	_, err = s.PublishVisualPreview(ctx, currentVersion.ID,
		readyVisualPreview(t, currentVersion.BlobHash, fakeHash("94"), 9),
		&BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)

	preview, err := s.PruneContentVersions(ctx, created.ID, current.Revision,
		VersionPruneSelector{KeepNewest: 1}, false)
	require.NoError(t, err)
	assert.Equal(t, 4, preview.UniqueBlobs)
	assert.Equal(t, 1, preview.SharedBlobs)
	assert.Equal(t, 3, preview.ReleasableBlobs)
	assert.Equal(t, int64(38), preview.ReleasableBytes)
	assert.Equal(t, 3, preview.LooseBlobsPendingGC)
	assert.Equal(t, int64(38), preview.LooseBytesPendingGC)

	receipt, err := s.PruneContentVersions(ctx, created.ID, current.Revision,
		VersionPruneSelector{KeepNewest: 1}, true)
	require.NoError(t, err)
	assert.Equal(t, preview.UniqueBlobs, receipt.UniqueBlobs)
	assert.Equal(t, preview.SharedBlobs, receipt.SharedBlobs)
	assert.Equal(t, preview.ReleasableBlobs, receipt.ReleasableBlobs)
	assert.Equal(t, preview.ReleasableBytes, receipt.ReleasableBytes)

	unreachable, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Contains(t, unreachable, BlobInfo{Hash: fakeHash("92"), Size: 8})
	assert.NotContains(t, unreachable, BlobInfo{Hash: fakeHash("94"), Size: 9})
}

func TestPruneContentVersionsAccountsForPageImages(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	historical := pageStoreRequest(t, s)
	current, _, err := s.ReplaceContent(ctx, historical.NodeID, historical.Revision,
		fakeHash("d1"), 20, "application/pdf")
	require.NoError(t, err)
	historical.Revision = current.Revision
	currentRequest := historical
	currentRequest.Source.VersionID = current.CurrentVersionID
	currentRequest.Source.SHA256 = current.BlobHash
	currentRequest.Source.Size = current.Size
	recipe := pageStoreRecipe()
	for index, request := range []PageJobRequest{historical, currentRequest} {
		_, err := s.QueuePageJob(ctx, fmt.Sprintf("00000000-0000-4000-8000-%012d", index+1), request)
		require.NoError(t, err)
		claim, err := s.ClaimPageJob(ctx)
		require.NoError(t, err)
		frames := pageStoreFrames(t, request)
		require.NoError(t, s.PublishPageFrames(ctx, claim, frames))
		for _, frame := range frames {
			image := pageStoreImage(t, frame, recipe)
			if index == 0 && frame.Page == 1 {
				image.SHA256 = fakeHash("c2")
			}
			require.NoError(t, s.PublishPageImage(ctx, claim, image, recipe,
				&BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: image.Size}))
		}
		require.NoError(t, s.FinishPageJob(ctx, claim, PageJobCompleted, ""))
	}
	addTestPack(t, s, "page-image-pack", 1, 6, nowRFC3339())
	addTestPackEntry(t, s, fakeHash("c2"), "page-image-pack", 0, 6, 10)
	_, err = s.db.Exec(`UPDATE blob_locations SET kind='packed',encoding=NULL,stored_size=6
		WHERE blob_hash=? AND store_id=?`, fakeHash("c2"), s.primaryStoreID)
	require.NoError(t, err)

	for _, run := range []bool{false, true} {
		result, err := s.PruneContentVersions(ctx, current.ID, current.Revision,
			VersionPruneSelector{KeepNewest: 1}, run)
		require.NoError(t, err)
		assert.Equal(t, 3, result.UniqueBlobs)
		assert.Equal(t, 1, result.SharedBlobs)
		assert.Equal(t, 2, result.ReleasableBlobs)
		assert.Equal(t, int64(133), result.ReleasableBytes)
		assert.Equal(t, 1, result.LooseBlobsPendingGC)
		assert.Equal(t, int64(123), result.LooseBytesPendingGC)
		assert.Equal(t, 1, result.PackedBlobsPendingRepack)
		assert.Equal(t, int64(6), result.PackedBytesPendingRepack)
	}
	unreachable, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Contains(t, unreachable, BlobInfo{Hash: fakeHash("c2"), Size: 10})
	assert.NotContains(t, unreachable, BlobInfo{Hash: fakeHash("c1"), Size: 10})
}

func TestPruneContentVersionsReportsPackedAndSharedConsequences(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
