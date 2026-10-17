package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPhotoEventsRenditionCoverage(t *testing.T) {
	t.Parallel()
	for _, transition := range []string{"failure", "recovery"} {
		t.Run(transition, func(t *testing.T) {
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
			query := PhotoBrowseRequest{
				Query:    snapshotTestQuery(t, `{"filters":{"text_coverage":["failed"]}}`),
				Coverage: CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint},
			}
			if transition == "recovery" {
				require.NoError(t, s.MarkRenditionJobFailed(ctx, claim, RenditionFailureConsent, now.Add(time.Second)))
			}
			before, err := s.ListPhotoAssets(ctx, query, nil)
			require.NoError(t, err)
			var beforeCount, afterCount int64 = 0, 1
			if transition == "recovery" {
				beforeCount, afterCount = 1, 0
			}
			require.Equal(t, beforeCount, before.Total)
			ch, stop := s.SubscribePhotoEvents()
			defer stop()
			if transition == "failure" {
				require.NoError(t, s.MarkRenditionJobFailed(ctx, claim, RenditionFailureConsent, now.Add(time.Second)))
			} else {
				reopened, _, err := s.EnqueueRenditionJob(ctx, request)
				require.NoError(t, err)
				require.Equal(t, RenditionJobQueued, reopened.State)
			}
			after, err := s.ListPhotoAssets(ctx, query, nil)
			require.NoError(t, err)
			require.Equal(t, afterCount, after.Total)
			event := nextPhotoEvent(t, ch)
			require.True(t, event.Refetch || event.AssetID == asset.ID, "coverage transition must invalidate the affected photo query")
		})
	}
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
			current, err := s.TagByID(ctx, tag.ID)
			require.NoError(t, err)
			require.Equal(t, int64(2), current.Revision)
			require.Equal(t, 1, current.AssignmentCount)
			events := []PhotoEvent{}
			for len(ch) > 0 {
				events = append(events, <-ch)
			}
			require.Contains(t, events, PhotoEvent{Refetch: true}, "assignment must refresh the shared tag revision and count")
		})
	}
}

func TestPhotoEventsBulkDocumentTrashQuiet(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "documents")
	require.NoError(t, err)
	nodes := make([]Node, 100)
	for i := range nodes {
		nodes[i], err = s.CreateFile(ctx, dir.ID, fmt.Sprintf("document-%d.txt", i), browseHash(fmt.Sprintf("document-%d", i)), 4, "text/plain")
		require.NoError(t, err)
	}
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	_, _, err = s.Trash(ctx, dir.ID, UnconditionalRev)
	require.NoError(t, err)
	for _, node := range nodes {
		trashed, err := s.NodeByID(ctx, node.ID)
		require.NoError(t, err)
		require.NotNil(t, trashed.TrashedAt)
	}
	noPhotoEvent(t, ch)
}

func TestPhotoEventsPublishSourceMetadata(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	photo := browsePhotoNode(t, s, "photo.jpg", browseHash("published-metadata-event"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	first, err := s.PublishSourceMetadata(ctx, photo.BlobHash, fakeHash("e1"), photoCanonical(t,
		photoMetadataField("image.exif.iso", "image.exif", "ISO", photoInteger(200))))
	require.NoError(t, err)
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	second, err := s.PublishSourceMetadata(ctx, photo.BlobHash, fakeHash("e2"), photoCanonical(t,
		photoMetadataField("image.exif.iso", "image.exif", "ISO", photoInteger(400))))
	require.NoError(t, err)
	require.NotEqual(t, first.GenerationID, second.GenerationID)
	projection, err := s.ContentVersionPhotoMetadata(ctx, photo.CurrentVersionID)
	require.NoError(t, err)
	require.EqualValues(t, 400, *projection.Fields.ISO)
	require.Equal(t, asset.ID, nextPhotoEvent(t, ch).AssetID)
	noPhotoEvent(t, ch)
}
