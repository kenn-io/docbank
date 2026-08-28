package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	policy := []byte(`{"roles":[{"max_count":2,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	request := renditionJobTestRequest(versions[0], profile)
	request.CapturedArtifactPolicy = policy
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)

	coverage, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{
		ContentVersionIDs: []string{versions[0]}, ProcessingProfileFingerprint: profile.Fingerprint,
	})
	require.NoError(t, err)
	assert.Equal(t, CoverageRebuilding, coverage.Renditions.State)
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
	assert.Equal(t, CoverageComplete, coverage.Renditions.State)
	assert.Equal(t, 1, coverage.Renditions.Complete)
	assert.Zero(t, coverage.Renditions.Rebuilding)
	assert.Zero(t, coverage.Renditions.PreviousGenerationServing)
}
