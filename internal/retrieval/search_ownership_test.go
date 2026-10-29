package retrieval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestSearcherRetiresSerializedGenerationBeforeResolution(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"explicit", "all_live"} {
		t.Run(name, func(t *testing.T) {
			searcher, original, _, descriptor := retrievalSearcherFixture(t, true, 1)
			require.Greater(t, len(original.authority.Lease.Generation.Bytes), 16,
				"query clone must not share a tiny allocation slot")
			originalChecksum := sha256.Sum256(original.authority.Lease.Generation.Bytes)
			backend := &retrievalOwnershipBackend{retrievalBackendStub: original}
			searcher.backend = backend
			scope := store.SearchOptions{}
			if name == "explicit" {
				scope.ContentVersionIDs = []string{"version-semantic"}
			}
			report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeSemantic,
				Limit: 1, ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
				Scope: scope, Authorization: retrievalAuthorization(descriptor)})
			require.NoError(t, err)
			require.Len(t, report.Results, 1)
			assert.Equal(t, ModeSemantic, report.ActualMode)
			assert.Equal(t, CoverageComplete, report.Coverage.State)
			assert.Equal(t, 1, report.Coverage.ScopedDocuments)
			assert.Equal(t, 1, report.Coverage.CompleteDocuments)
			assert.Equal(t, DocumentIdentity{VaultID: "vault", NodeID: 8,
				ContentVersionID: "version-semantic"}, report.Results[0].Document)
			require.Len(t, report.Results[0].Evidence, 1)
			assert.Equal(t, original.authority.VectorSpace.ID, report.Results[0].Evidence[0].VectorSpaceID)
			assert.Equal(t, original.authority.Lease.Generation.SourceManifestChecksum,
				report.Results[0].Evidence[0].SourceManifestChecksum)
			assert.Equal(t, original.authority.Lease.Generation.SourceManifestChecksum, original.sourceManifest)
			assert.Equal(t, originalChecksum, sha256.Sum256(original.authority.Lease.Generation.Bytes),
				"dropping query ownership must not modify immutable encoded bytes")
			assert.Equal(t, 3, original.neighborCount, "the complete valid generation was opened and scored")
			assert.Equal(t, 1, backend.resolutionCalls)
			assert.Equal(t, 1, backend.releaseCalls)
			assert.Equal(t, "lease", backend.releasedID)
			assert.EqualValues(t, 1, backend.releasedToken)
			require.NoError(t, original.releaseContextErr)
			assert.True(t, original.releasedAt.After(original.acquiredAt))
			assert.False(t, backend.retainedAtResolution,
				"serialized query bytes remain reachable after complete decoding and scoring")
		})
	}
}

// The fixture's original bytes remain independently owned. Only the authority
// returned to the real searcher owns this fresh clone; the observer keeps a weak
// pointer, never a slice, reader, closure or other strong reference to it.
type retrievalOwnershipBackend struct {
	*retrievalBackendStub

	queryBytes           weak.Pointer[byte]
	retainedAtResolution bool
	resolutionCalls      int
	releaseCalls         int
	releasedID           string
	releasedToken        int64
}

func (backend *retrievalOwnershipBackend) AcquireSemanticSearchAuthority(ctx context.Context,
	profile, binding, owner string, at time.Time, duration time.Duration, scope store.SearchOptions,
) (store.SemanticSearchAuthority, error) {
	authority, err := backend.retrievalBackendStub.AcquireSemanticSearchAuthority(ctx,
		profile, binding, owner, at, duration, scope)
	if err == nil {
		clone := bytes.Clone(authority.Lease.Generation.Bytes)
		backend.queryBytes = weak.Make(&clone[0])
		authority.Lease.Generation.Bytes = clone
	}
	return authority, err
}

func (backend *retrievalOwnershipBackend) ResolveSemanticCandidates(ctx context.Context,
	profile, binding string, kind document.EmbeddingInputKind, space, source string,
	neighbors []vectorindex.Neighbor, limit int, scope store.SearchOptions, excluded map[int64]struct{},
) (store.SemanticSearchResolution, error) {
	// GC is confined to this test boundary, after the real Open and SearchRows.
	// Observe only after collection so Value cannot keep a strong pointer alive.
	runtime.GC()
	runtime.GC()
	backend.retainedAtResolution = backend.queryBytes.Value() != nil
	backend.resolutionCalls++
	return backend.retrievalBackendStub.ResolveSemanticCandidates(ctx, profile, binding,
		kind, space, source, neighbors, limit, scope, excluded)
}

func (backend *retrievalOwnershipBackend) ReleaseVectorIndexGeneration(ctx context.Context,
	id string, token int64, at time.Time,
) error {
	backend.releaseCalls++
	backend.releasedID, backend.releasedToken = id, token
	return backend.retrievalBackendStub.ReleaseVectorIndexGeneration(ctx, id, token, at)
}
