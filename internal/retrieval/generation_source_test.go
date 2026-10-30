package retrieval

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

type headRetrievalBackend struct {
	*retrievalBackendStub

	fullCalls, headCalls int
}

func (b *headRetrievalBackend) AcquireSemanticSearchAuthority(ctx context.Context, p, binding, o string, at time.Time, d time.Duration, opts store.SearchOptions) (store.SemanticSearchAuthority, error) {
	b.fullCalls++
	return b.retrievalBackendStub.AcquireSemanticSearchAuthority(ctx, p, binding, o, at, d, opts)
}
func (b *headRetrievalBackend) AcquireSemanticSearchAuthorityHead(ctx context.Context, p, binding, o string, at time.Time, d time.Duration, opts store.SearchOptions) (store.SemanticSearchAuthority, error) {
	b.headCalls++
	a, err := b.retrievalBackendStub.AcquireSemanticSearchAuthority(ctx, p, binding, o, at, d, opts)
	a.Lease.Generation.Bytes = nil
	return a, err
}

type staticGenerationSource struct {
	generation *vectorindex.Generation
	err        error
}

func (s staticGenerationSource) Get(context.Context, store.VectorIndexGenerationRecord) (*vectorindex.Generation, error) {
	return s.generation, s.err
}

func TestSearcherGenerationSourceUsesHeadAndRechecksAuthority(t *testing.T) {
	t.Parallel()
	base, stub, _, descriptor := retrievalSearcherFixture(t, true, 1)
	stored := stub.authority.Lease.Generation
	g, err := vectorindex.OpenGeneration(bytes.NewReader(stored.Bytes), int64(len(stored.Bytes)))
	require.NoError(t, err)
	backend := &headRetrievalBackend{retrievalBackendStub: stub}
	searcher, err := NewSearcher(SearcherConfig{Backend: backend, Encoders: base.encoders, Owner: "test", LeaseDuration: time.Minute, GenerationSource: staticGenerationSource{generation: g}})
	require.NoError(t, err)
	query := Query{Text: "query", Mode: ModeSemantic, Limit: 3, ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required", Authorization: retrievalAuthorization(descriptor)}
	first, err := searcher.Search(t.Context(), query)
	require.NoError(t, err)
	require.NotEmpty(t, first.Results)
	require.Equal(t, 1, backend.headCalls)
	require.Zero(t, backend.fullCalls)
	stub.authority.Lease.Generation.IndexManifestChecksum = strings.Repeat("f", 64)
	_, err = searcher.Search(t.Context(), query)
	require.ErrorContains(t, err, "leased vector generation is incompatible")
	require.False(t, stub.releasedAt.IsZero())
	stub.authority.Lease.Generation = stored
	stub.acquireErr = store.ErrVectorIndexSourceStale
	_, err = searcher.Search(t.Context(), query)
	require.ErrorIs(t, err, store.ErrVectorIndexSourceStale)
}

func TestSearcherGenerationSourceFailureReleasesLease(t *testing.T) {
	t.Parallel()
	base, stub, _, descriptor := retrievalSearcherFixture(t, true, 1)
	backend := &headRetrievalBackend{retrievalBackendStub: stub}
	searcher, err := NewSearcher(SearcherConfig{Backend: backend, Encoders: base.encoders, Owner: "test", LeaseDuration: time.Minute, GenerationSource: staticGenerationSource{err: errors.New("decode failure")}})
	require.NoError(t, err)
	_, err = searcher.Search(t.Context(), Query{Text: "query", Mode: ModeSemantic, Limit: 1, ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required", Authorization: retrievalAuthorization(descriptor)})
	require.ErrorContains(t, err, "decode failure")
	require.False(t, stub.releasedAt.IsZero())
}

type orderedGenerationSource struct {
	generation *vectorindex.Generation
	obtained   *bool
}

func (s orderedGenerationSource) Get(context.Context, store.VectorIndexGenerationRecord) (*vectorindex.Generation, error) {
	*s.obtained = true
	return s.generation, nil
}

type sourceOrderResolver struct {
	QueryEncoderResolver

	obtained *bool
}

func (r sourceOrderResolver) ResolveQueryEncoder(ctx context.Context, d document.EmbeddingDescriptor) (document.EmbeddingProvider, error) {
	if !*r.obtained {
		return nil, errors.New("generation must be obtained before provider resolution")
	}
	return r.QueryEncoderResolver.ResolveQueryEncoder(ctx, d)
}

func TestSearcherGenerationSourceLoadsBeforeEncoding(t *testing.T) {
	t.Parallel()
	base, stub, _, descriptor := retrievalSearcherFixture(t, true, 1)
	stored := stub.authority.Lease.Generation
	generation, err := vectorindex.OpenGeneration(bytes.NewReader(stored.Bytes), int64(len(stored.Bytes)))
	require.NoError(t, err)
	obtained := false
	searcher, err := NewSearcher(SearcherConfig{Backend: &headRetrievalBackend{retrievalBackendStub: stub},
		Encoders: sourceOrderResolver{QueryEncoderResolver: base.encoders, obtained: &obtained}, Owner: "test", LeaseDuration: time.Minute,
		GenerationSource: orderedGenerationSource{generation: generation, obtained: &obtained}})
	require.NoError(t, err)
	_, err = searcher.Search(t.Context(), Query{Text: "query", Mode: ModeSemantic, Limit: 1, ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required", Authorization: retrievalAuthorization(descriptor)})
	require.NoError(t, err)
}
