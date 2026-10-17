package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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

func TestPhotoEventsRenditionSearchMembership(t *testing.T) {
	t.Parallel()
	s, _ := newRenditionCatalogFixture(t)
	ctx := t.Context()
	photo, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", catalogSourceHash, 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "mercury phrase")
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	generation, err := s.StageLexicalGeneration(ctx, fakeHash("a9"))
	require.NoError(t, err)
	before := browsePhotoPage(t, s, `{"text":"mercury"}`)
	require.Zero(t, before.Total)
	events, stop := s.SubscribePhotoEvents()
	defer stop()
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: photo.CurrentVersionID, BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, attachment, RenditionHeadRecord{
		ContentVersionID: photo.CurrentVersionID, ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: embeddingCatalogTime}, generation.ID))
	event := nextPhotoEvent(t, events)
	require.Equal(t, asset.ID, event.AssetID)
	require.True(t, event.Refetch)
	after := browsePhotoPage(t, s, `{"text":"mercury"}`)
	require.EqualValues(t, 1, after.Total)
	require.Len(t, after.Items, 1)
	require.Equal(t, asset.ID, after.Items[0].AssetID)
}
