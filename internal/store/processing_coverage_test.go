package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestProcessingCoverageLargeFenceKeepsStatesDisjoint(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	const total = 4096
	ids := []string{versionID}
	for i := 1; i < total-1; i++ {
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("coverage-%04d.pdf", i), catalogSourceHash, 20, "application/pdf")
		require.NoError(t, err)
		ids = append(ids, node.CurrentVersionID)
	}
	ids = append(ids, "00000000-0000-4000-8000-000000000001")
	for _, id := range ids[:2] {
		record := embeddingSetFixture(s, id, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: id, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), chunk))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: chunk.BindingID, InputKind: chunk.InputKind},
		SetID: chunk.ID, VectorSpaceID: chunk.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	for _, id := range []string{ids[0], ids[2]} {
		_, err := s.EnqueueEmbeddingJob(t.Context(), embeddingJobTestRequest(t, s, id, profile, "coverage-rebuild-"+id))
		require.NoError(t, err)
	}
	started := time.Now()
	coverage, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{
		ContentVersionIDs: ids, ProcessingProfileFingerprint: profile.Fingerprint,
		Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}, {BindingID: "chunk"}},
	})
	require.NoError(t, err)
	t.Logf("coverage of %d versions and 2 bindings: %s", total, time.Since(started))
	assert.Equal(t, ProcessingClassCoverage{Name: "rendition", State: "partial", Total: total,
		Complete: 1, Stale: 1, Unavailable: total - 2}, coverage.Renditions)
	require.Len(t, coverage.Embeddings, 2)
	assert.Equal(t, ProcessingClassCoverage{Name: "optional", State: "rebuilding", Total: total,
		Complete: 1, Stale: 1, Unavailable: total - 4, Rebuilding: 2, PreviousGenerationServing: 1}, coverage.Embeddings[0])
	assert.Equal(t, ProcessingClassCoverage{Name: "chunk", State: "partial", Total: total,
		Complete: 1, Stale: 1, Unavailable: total - 2}, coverage.Embeddings[1])
}

func TestProcessingCoverageReportsRebuildWhilePreviousGenerationServes(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	oldBuild := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), oldBuild))
	oldGeneration, err := s.StageLexicalGeneration(t.Context(), testSHA256([]byte("coverage-old-generation")))
	require.NoError(t, err)
	oldAttachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: oldBuild.ID, Profile: profile, AttachedAt: nowRFC3339(),
	}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), oldAttachment,
		RenditionHeadRecord{ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
			AttachmentID: oldAttachment.ID, PublishedAt: nowRFC3339()}, oldGeneration.ID))

	policy := []byte(catalogCapturedPolicy)
	request := renditionJobTestRequest(versions[0], profile)
	request.CapturedArtifactPolicy = policy
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)

	coverage, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{
		ContentVersionIDs: []string{versions[0]}, ProcessingProfileFingerprint: profile.Fingerprint,
	})
	require.NoError(t, err)
	assert.Equal(t, "rebuilding", coverage.Renditions.State)
	assert.Equal(t, 1, coverage.Renditions.Rebuilding)
	assert.Equal(t, 1, coverage.Renditions.PreviousGenerationServing)
	assert.Zero(t, coverage.Renditions.Complete)

	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker:coverage", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	newBuild := cloneCatalogBuild(oldBuild)
	newBuild.ID = job.ID
	newBuild.CapturedArtifactPolicy = policy
	newBuild.CapturedArtifactPolicyFingerprint = testSHA256(policy)
	require.NoError(t, s.StageRenditionJobBuild(t.Context(), claim, newBuild, now.Add(2*time.Second)))
	_, err = s.StageRenditionJobGeneration(t.Context(), claim,
		testSHA256([]byte("coverage-new-generation")), now.Add(3*time.Second))
	require.NoError(t, err)
	_, err = s.PublishRenditionJob(t.Context(), claim, now.Add(4*time.Second))
	require.NoError(t, err)

	coverage, err = s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{
		ContentVersionIDs: []string{versions[0]}, ProcessingProfileFingerprint: profile.Fingerprint,
	})
	require.NoError(t, err)
	assert.Equal(t, "complete", coverage.Renditions.State)
	assert.Equal(t, 1, coverage.Renditions.Complete)
	assert.Zero(t, coverage.Renditions.Rebuilding)
	assert.Zero(t, coverage.Renditions.PreviousGenerationServing)
}

func TestProcessingCoverageEmbeddingReplacementKeepsServingHead(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	old := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), old))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: old.BindingID, InputKind: old.InputKind},
		SetID:        old.ID, VectorSpaceID: old.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	request := embeddingJobTestRequest(t, s, versionID, profile, "coverage-replacement")
	job, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	scope := ProcessingCoverageScope{ContentVersionIDs: []string{versionID},
		ProcessingProfileFingerprint: profile.Fingerprint,
		Bindings:                     []ProcessingCoverageBinding{{BindingID: "optional"}}}
	coverage, err := s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	require.Len(t, coverage.Embeddings, 1)
	assert.Equal(t, "rebuilding", coverage.Embeddings[0].State)
	assert.Equal(t, 1, coverage.Embeddings[0].Rebuilding)
	assert.Equal(t, 1, coverage.Embeddings[0].PreviousGenerationServing)
	assert.Zero(t, coverage.Embeddings[0].Complete)

	at := time.Now().UTC()
	claim, work, found, err := s.ClaimEmbeddingWork(t.Context(), job.ID, "coverage-worker", at,
		time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureInputRejected,
		EmbeddingAttemptReceipt{AttemptID: claim.AttemptID}, at.Add(time.Second)))
	coverage, err = s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	assert.Equal(t, "complete", coverage.Embeddings[0].State)
	assert.Equal(t, 1, coverage.Embeddings[0].Complete)
	assert.Zero(t, coverage.Embeddings[0].Rebuilding)
	assert.Zero(t, coverage.Embeddings[0].PreviousGenerationServing)

	version, err := s.ContentVersionByID(t.Context(), versionID)
	require.NoError(t, err)
	node, err := s.NodeByID(t.Context(), version.NodeID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	coverage, err = s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	assert.Equal(t, "stale", coverage.Renditions.State)
	assert.Equal(t, "stale", coverage.Embeddings[0].State)
	assert.Equal(t, 1, coverage.Embeddings[0].Stale)
}

func TestProcessingCoverageChunkRequiresCurrentEvidence(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), chunk))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: chunk.BindingID, InputKind: chunk.InputKind},
		SetID:        chunk.ID, VectorSpaceID: chunk.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	scope := ProcessingCoverageScope{ContentVersionIDs: []string{versionID},
		ProcessingProfileFingerprint: profile.Fingerprint,
		Bindings:                     []ProcessingCoverageBinding{{BindingID: "chunk"}}}
	coverage, err := s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	require.Equal(t, "complete", coverage.Embeddings[0].State)
	_, err = s.db.Exec(`DROP TRIGGER rendition_builds_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE rendition_builds SET evidence_checksum=? WHERE build_id=(
		SELECT build_id FROM rendition_attachments WHERE attachment_id=?)`, testSHA256([]byte("changed-evidence")), attachmentID)
	require.NoError(t, err)
	coverage, err = s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	assert.Equal(t, "unavailable", coverage.Embeddings[0].State)
	assert.Equal(t, 1, coverage.Embeddings[0].Unavailable)
	assert.Zero(t, coverage.Embeddings[0].Complete)
}
