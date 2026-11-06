package processing

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// The existing rendition publication and independent embedding queue are the
// integration boundary. Retrying vector work must not replace searchable text.
func TestConversionTextRemainsSearchableDuringEmbeddingRetry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fixture, fake, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputRenditionChunk)
		before, err := fixture.catalog.ActiveRendition(t.Context(), fixture.versionID, fixture.profile.Fingerprint)
		require.NoError(t, err)
		lexical, err := fixture.catalog.ActiveLexicalGeneration(t.Context())
		require.NoError(t, err)
		fake.runtime.failures[request.BindingID] = []error{embeddingTransientError{retryAfter: time.Second}}
		waits := 0
		worker.wait = func(ctx context.Context, _ time.Duration) error {
			waits++
			hits, _, err := fixture.catalog.SearchPage(ctx, "synthetic", 10)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, fixture.versionID, hits[0].Node.CurrentVersionID)
			current, err := fixture.catalog.ActiveRendition(ctx, fixture.versionID, fixture.profile.Fingerprint)
			require.NoError(t, err)
			require.Equal(t, before.Build.ID, current.Build.ID)
			return nil
		}
		processed, err := worker.ScanOnce(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, processed)
		require.Positive(t, waits)
		// A completed embedding job is not discovered again, and no converter is
		// registered with this worker: it consumes only the retained generation.
		processed, err = worker.ScanOnce(t.Context())
		require.NoError(t, err)
		require.Zero(t, processed)
		after, err := fixture.catalog.ActiveRendition(t.Context(), fixture.versionID, fixture.profile.Fingerprint)
		require.NoError(t, err)
		require.Equal(t, before.Build.ID, after.Build.ID)
		currentLexical, err := fixture.catalog.ActiveLexicalGeneration(t.Context())
		require.NoError(t, err)
		require.Equal(t, lexical, currentLexical)
	})
}
