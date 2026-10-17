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
	events, err := s.VisiblePhotoEvents(ctx, []PhotoEvent{nextPhotoEvent(t, ch)})
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID, Revision: asset.Revision}}, events)
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

func TestPhotoEventsQueryMembershipKeepsAffectedAsset(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	photo := browsePhotoNode(t, s, "before.jpg", browseHash("query-event"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	_, _, err = s.Move(ctx, photo.ID, s.RootID(), "after.jpg", photo.Revision)
	require.NoError(t, err)
	batch := []PhotoEvent{nextPhotoEvent(t, ch)}
	visible, err := s.VisiblePhotoEvents(ctx, batch)
	require.NoError(t, err)
	current, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	// A rename can enter or leave a name query, but its ID still scopes detail reconciliation.
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID, Revision: current.Revision, Refetch: true}}, visible)
}

func TestPhotoEventsHiddenAlbumMembershipVisibility(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset := albumAsset(t, s, "hidden.jpg")
	asset, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	set, err := s.CreatePhotoSet(ctx, "Album")
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	_, err = s.ChangePhotoSetMembers(unlocked, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	batch := []PhotoEvent{nextPhotoEvent(t, ch)}
	lockedEvents, err := s.VisiblePhotoEvents(ctx, batch)
	require.NoError(t, err)
	unlockedEvents, err := s.VisiblePhotoEvents(unlocked, batch)
	require.NoError(t, err)
	// Check both audiences even when the locked delivery is wrong.
	if len(lockedEvents) != 0 {
		t.Errorf("locked viewer received Hidden-only membership events: %+v", lockedEvents)
	}
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID, Revision: asset.Revision, Refetch: true}}, unlockedEvents)
}
