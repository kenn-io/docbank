package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func nextPhotoEvent(t *testing.T, events <-chan PhotoEvent) PhotoEvent {
	t.Helper()
	event := nextPhotoEventWithAudience(t, events)
	event.audience = 0
	return event
}

func nextPhotoEventWithAudience(t *testing.T, events <-chan PhotoEvent) PhotoEvent {
	t.Helper()
	// Writes publish before they return; the test timeout bounds a missing event.
	event, ok := <-events
	require.True(t, ok)
	return event
}

func noPhotoEvent(t *testing.T, events <-chan PhotoEvent) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("unexpected event: %+v", event)
	default:
	}
}

func TestPhotoEventsRollback(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	events, cancel := s.SubscribePhotoEvents(t.Context())
	defer cancel()
	committed, err := s.CreatePhotoSet(t.Context(), "committed")
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{AlbumID: committed.ID}, nextPhotoEvent(t, events))
	rejected := errors.New("reject")
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := insertPhotoSet(t.Context(), tx, "rolled-back")
		if err != nil {
			return err
		}
		return rejected
	})
	require.ErrorIs(t, err, rejected)
	noPhotoEvent(t, events)
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `PRAGMA defer_foreign_keys=ON`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE photo_sets SET name='failed commit',revision=revision+1 WHERE set_id=?`, committed.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `INSERT INTO photo_set_members(set_id,asset_id,added_at) VALUES('missing','missing',?)`, nowRFC3339())
		return err
	})
	require.ErrorContains(t, err, "committing transaction")
	noPhotoEvent(t, events)
}

func TestPhotoEventsDeliveryFilteringAndAlbumRevisions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	node, err := s.CreateFile(t.Context(), s.RootID(), "private.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	queued := PhotoEvent{AssetID: asset.ID, audience: photoHidden}
	asset, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	public, err := s.VisiblePhotoEvent(t.Context(), PhotoEvent{AssetID: asset.ID, audience: photoPublic})
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, public)
	visible, err := s.VisiblePhotoEvent(t.Context(), queued)
	require.NoError(t, err)
	require.Zero(t, visible)
	mixed, err := s.VisiblePhotoEvent(t.Context(), PhotoEvent{Refetch: true, audience: photoPublic | photoHidden})
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, mixed)
	visible, err = s.VisiblePhotoEvent(t.Context(), PhotoEvent{Refetch: true, audience: photoHidden})
	require.NoError(t, err)
	require.Zero(t, visible)
	token, _, err := s.UnlockPhotoHidden(t.Context(), "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(t.Context(), token)
	visible, err = s.VisiblePhotoEvent(unlocked, queued)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{AssetID: asset.ID, Revision: asset.Revision}, visible)
	visible, err = s.VisiblePhotoEvent(unlocked, PhotoEvent{Refetch: true, audience: photoHidden})
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, visible)
	require.NoError(t, s.LockPhotoHidden(unlocked))
	visible, err = s.VisiblePhotoEvent(unlocked, queued)
	require.NoError(t, err)
	require.Zero(t, visible)
	visible, err = s.VisiblePhotoEvent(unlocked, PhotoEvent{Refetch: true, audience: photoHidden})
	require.NoError(t, err)
	require.Zero(t, visible)
	_, err = s.writeDB.ExecContext(t.Context(), `DELETE FROM photo_assets WHERE asset_id=?`, asset.ID)
	require.NoError(t, err)
	visible, err = s.VisiblePhotoEvent(t.Context(), PhotoEvent{AssetID: asset.ID, audience: photoPublic})
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, visible)
	album, err := s.CreatePhotoSet(t.Context(), "Summer")
	require.NoError(t, err)
	queued = PhotoEvent{AlbumID: album.ID, Revision: album.Revision}
	album, err = s.UpdatePhotoSet(t.Context(), album.ID, album.Revision, new("Autumn"), nil, nil)
	require.NoError(t, err)
	visible, err = s.VisiblePhotoEvent(t.Context(), queued)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{AlbumID: album.ID, Revision: album.Revision}, visible)
}

func TestPhotoEventsAudienceClassification(t *testing.T) {
	t.Parallel()
	t.Run("locked subscriber with shared blob", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
		hidden, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", fakeHash("11"), 4, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
		require.NoError(t, err)
		_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
		require.NoError(t, err)
		public, err := s.CreateFile(ctx, s.RootID(), "public.jpg", hidden.BlobHash, 4, "image/jpeg")
		require.NoError(t, err)
		visible, err := s.PhotoAssetForNode(ctx, public.ID)
		require.NoError(t, err)
		events, cancel := s.SubscribePhotoEvents(ctx)
		defer cancel()
		require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: hidden.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: "shared"}))
		require.Equal(t, PhotoEvent{AssetID: visible.ID}, nextPhotoEvent(t, events))
		noPhotoEvent(t, events)
	})
	for _, tc := range []struct {
		name                      string
		documents, hidden, public int
		visibility                *bool
	}{
		{"hidden after documents", 66, 1, 0, nil},
		{"hidden overflow", 0, 66, 0, nil},
		{"public overflow", 0, 0, 66, nil},
		{"bulk unhide", 0, 70, 0, new(false)},
		{"bulk hide", 0, 0, 70, new(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
			var changed Node
			var hiddenAssetID string
			for i := range tc.documents + tc.hidden + tc.public {
				mime := "image/jpeg"
				if i < tc.documents {
					mime = "application/pdf"
				}
				hash := fakeHash("11")
				if tc.visibility != nil {
					hash = fmt.Sprintf("%064x", i+1)
				}
				node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("peer-%d", i), hash, 4, mime)
				require.NoError(t, err)
				if tc.documents == 0 || i == 0 {
					changed = node
				}
				if i >= tc.documents && i < tc.documents+tc.hidden {
					asset, err := s.PhotoAssetForNode(ctx, node.ID)
					require.NoError(t, err)
					hiddenAssetID = asset.ID
					_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
					require.NoError(t, err)
				}
			}
			token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
			require.NoError(t, err)
			unlocked := WithPhotoHiddenToken(ctx, token)
			events, cancel := s.SubscribePhotoEvents(unlocked)
			defer cancel()
			if tc.visibility != nil {
				var hiddenAt any
				if *tc.visibility {
					hiddenAt = nowRFC3339()
				}
				require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET hidden_at=?,revision=revision+1`, hiddenAt)
					return err
				}))
				summary := nextPhotoEventWithAudience(t, events)
				require.Equal(t, photoPublic|photoHidden, summary.audience)
				return
			}
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE nodes SET trashed_at=? WHERE id=?`, nowRFC3339(), changed.ID)
				return err
			}))
			var audience photoAudience
			var refetch bool
			var assetIDs []string
			for len(events) > 0 {
				event := nextPhotoEventWithAudience(t, events)
				audience |= event.audience
				refetch = refetch || event.Refetch
				assetIDs = append(assetIDs, event.AssetID)
			}
			want := photoHidden
			if tc.hidden == 0 {
				want = 0
			}
			if tc.public > 0 {
				want |= photoPublic
			}
			require.Equal(t, want, audience)
			require.Equal(t, tc.hidden > 64 || tc.public > 64, refetch)
			if tc.documents > 0 {
				require.Contains(t, assetIDs, hiddenAssetID)
			}
		})
	}
}

func TestPhotoEventsSubscriberSummariesUnionBatchAndQueuedAudiences(t *testing.T) {
	t.Parallel()
	t.Run("queue overflow unions whole batch", func(t *testing.T) {
		s := newTestStore(t)
		events, cancel := s.SubscribePhotoEvents(t.Context())
		defer cancel()
		var hidden []PhotoEvent
		for i := range 64 {
			hidden = append(hidden, PhotoEvent{AssetID: strconv.Itoa(i), audience: photoHidden})
		}
		s.publishPhotoChanges(hidden)
		for _, event := range hidden {
			require.Equal(t, event, nextPhotoEventWithAudience(t, events))
		}
		noPhotoEvent(t, events)
		s.publishPhotoChanges(hidden)
		public := PhotoEvent{AlbumID: "public", audience: photoPublic}
		overflow := append(append([]PhotoEvent{}, hidden...), public)
		s.publishPhotoChanges(overflow)
		summary := nextPhotoEventWithAudience(t, events)
		require.Equal(t, PhotoEvent{Refetch: true, audience: photoPublic | photoHidden}, summary)
		noPhotoEvent(t, events)
	})
	t.Run("queued summaries union audiences", func(t *testing.T) {
		s := newTestStore(t)
		events, cancel := s.SubscribePhotoEvents(t.Context())
		defer cancel()
		s.publishPhotoChanges([]PhotoEvent{{Refetch: true, audience: photoHidden}})
		s.publishPhotoChanges([]PhotoEvent{{Refetch: true, audience: photoPublic}})
		require.Equal(t, photoPublic|photoHidden, (nextPhotoEventWithAudience(t, events)).audience)
	})
}
