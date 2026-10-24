package api

import (
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoEventBurst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		batch := []store.PhotoEvent{{AssetID: "photo-0"}, {AssetID: "photo-1"}}
		events := make(chan []store.PhotoEvent)
		done := make(chan []store.PhotoEvent, 1)
		go func() { done <- coalescePhotoEvents(t.Context(), t.Context(), events, batch) }()
		for i := range 5 {
			time.Sleep(15 * time.Millisecond)
			events <- []store.PhotoEvent{{AssetID: "photo-" + strconv.Itoa(i+2)}}
		}
		require.Empty(t, done, "hold the merged batch until the window closes")
		time.Sleep(25 * time.Millisecond)
		synctest.Wait()
		// A window that restarted on every event would still be open here.
		require.Len(t, done, 1)
		require.ElementsMatch(t, []store.PhotoEvent{
			{AssetID: "photo-0"}, {AssetID: "photo-1"}, {AssetID: "photo-2"},
			{AssetID: "photo-3"}, {AssetID: "photo-4"}, {AssetID: "photo-5"}, {AssetID: "photo-6"},
		}, <-done)
	})
}
