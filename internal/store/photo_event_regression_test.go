package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPhotoEventsRenditionRecovery(t *testing.T) {
	t.Parallel()
	s, _, _, profile := collectionCoverageFixture(t, 0)
	ctx := t.Context()
	photo := browsePhotoNode(t, s, "photo.jpg", browseHash("coverage-event"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	request := renditionJobTestRequest(photo.CurrentVersionID, profile)
	request.ExecutionIdentity.Upload.SHA256 = photo.BlobHash
	request.ExecutionIdentity.Authorization.SourceSHA256 = photo.BlobHash
	grantRenditionJobConsent(t, s, request)
	job, _, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(ctx, job.ID, "coverage-worker", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.MarkRenditionJobFailed(ctx, claim, RenditionFailureConsent, now.Add(time.Second)))
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	reopened, _, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	require.Equal(t, RenditionJobQueued, reopened.State)
	require.Equal(t, PhotoEvent{AssetID: asset.ID, Revision: asset.Revision}, nextPhotoEvent(t, ch))
}

func TestPhotoEventsTagAssignmentDefinitions(t *testing.T) {
	t.Parallel()
	for _, mime := range []string{"text/plain", "image/jpeg"} {
		t.Run(mime, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			node, err := s.CreateFile(ctx, s.RootID(), "source", browseHash("tag-event"), 4, mime)
			require.NoError(t, err)
			tag, err := s.CreateTag(ctx, "Keyword")
			require.NoError(t, err)
			ch, stop := s.SubscribePhotoEvents()
			defer stop()
			_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
			require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, ch))
			noPhotoEvent(t, ch)
		})
	}
}
