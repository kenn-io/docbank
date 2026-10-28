//go:build !darwin || arm64

package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"hegel.dev/go/hegel"
)

func TestReaderLeaseBookkeepingPreservesEmbeddingWriteRevisionProperty(t *testing.T) {
	t.Parallel()
	s, profile, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 1)
	record := records[0]
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	now := time.Now().UTC()
	generation := vectorIndexGenerationFixture("reader-revision-property", source, []byte("index"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
	revision := s.WriteRevision()

	hegel.Test(t, func(ht *hegel.T) {
		metadataFirst := hegel.Draw(ht, hegel.Booleans())
		owner := "reader-" + hegel.Draw(ht, hegel.Text().MaxSize(32))
		duration := time.Duration(hegel.Draw(ht, hegel.Integers(1, 60))) * time.Minute
		for i, metadataOnly := range []bool{metadataFirst, !metadataFirst} {
			leaseAt := now.Add(time.Duration(i) * time.Second)
			var lease VectorIndexReaderLease
			if metadataOnly {
				authority, acquireErr := s.AcquireSemanticSearchAuthorityHead(
					t.Context(), profile.Fingerprint, record.BindingID, owner, leaseAt, duration, SearchOptions{},
				)
				if acquireErr != nil {
					ht.Fatalf("acquiring metadata-only reader lease: %v", acquireErr)
				}
				lease = authority.Lease
			} else {
				var acquireErr error
				lease, acquireErr = s.AcquireVectorIndexGeneration(
					t.Context(), record.VectorSpace.ID, owner, leaseAt, duration,
				)
				if acquireErr != nil {
					ht.Fatalf("acquiring reader lease: %v", acquireErr)
				}
			}
			afterAcquire := s.WriteRevision()
			if releaseErr := s.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, leaseAt); releaseErr != nil {
				ht.Fatalf("releasing reader lease: %v", releaseErr)
			}
			afterRelease := s.WriteRevision()
			if afterAcquire != revision || afterRelease != revision {
				ht.Fatalf("reader bookkeeping changed write revision: before=%d acquire=%d release=%d", revision, afterAcquire, afterRelease)
			}
		}
	}, hegel.WithDatabase(t.TempDir()))
}
