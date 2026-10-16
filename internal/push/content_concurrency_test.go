package push

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func TestDuplicateUploadWaitIsCancelableAndLeavesOtherContentFree(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &pusher{}
		first, err := p.beginUpload(t.Context(), strings.Repeat("a1", 32))
		require.NoError(t, err)
		defer first()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			release, err := p.beginUpload(ctx, strings.Repeat("a1", 32))
			if err == nil {
				release()
			}
			done <- err
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("duplicate passed an upload whose receipt is still pending")
		default:
		}
		other, err := p.beginUpload(t.Context(), strings.Repeat("b2", 32))
		require.NoError(t, err)
		other()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}
