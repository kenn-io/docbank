package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPhotoEventsRenditionClaimRenewalChangesCoverage(t *testing.T) {
	t.Parallel()
	s, _, _, profile := collectionCoverageFixture(t, 0)
	ctx := t.Context()
	photo := browsePhotoNode(t, s, "photo.jpg", browseHash("renewal-coverage-event"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	request := renditionJobTestRequest(photo.CurrentVersionID, profile)
	request.ExecutionIdentity.Upload.SHA256 = photo.BlobHash
	request.ExecutionIdentity.Authorization.SourceSHA256 = photo.BlobHash
	grantRenditionJobConsent(t, s, request)
	failed, _, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Second)
	failedClaim, err := s.ClaimRenditionJob(ctx, failed.ID, "failed-worker", at, time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.MarkRenditionJobFailed(ctx, failedClaim, RenditionFailureTerminal, at.Add(time.Second)))
	request.CapturedArtifactPolicy = []byte(strings.Replace(string(request.CapturedArtifactPolicy), `"max_count":1,"min_count":0`, `"max_count":2,"min_count":0`, 1))
	running, _, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	require.NotEqual(t, failed.ID, running.ID)
	claim, err := s.ClaimRenditionJob(ctx, running.ID, "running-worker", at, time.Minute)
	require.NoError(t, err)
	// Keep both exact waiters and the running job older than the failed attempt.
	// Public APIs created the jobs and claims; only ordering timestamps are fixtures.
	_, err = s.db.Exec(`UPDATE rendition_job_waiters SET updated_at='2026-01-01T00:00:00.000000000Z'`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE rendition_jobs SET updated_at='2026-01-01T00:00:00.000000000Z' WHERE job_id=?`, running.ID)
	require.NoError(t, err)
	query := PhotoBrowseRequest{
		Query:    snapshotTestQuery(t, `{"filters":{"text_coverage":["failed"]}}`),
		Coverage: CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint},
	}
	before, err := s.ListPhotoAssets(ctx, query, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), before.Total)
	require.Equal(t, asset.ID, before.Items[0].AssetID)
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	_, err = s.RenewRenditionJobClaim(ctx, claim, at.Add(2*time.Second), time.Minute)
	require.NoError(t, err)
	after, err := s.ListPhotoAssets(ctx, query, nil)
	require.NoError(t, err)
	require.Zero(t, after.Total)
	event := nextPhotoEvent(t, ch)
	require.True(t, event.Refetch || event.AssetID == asset.ID, "claim renewal must invalidate the affected coverage query")
}
