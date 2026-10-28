package processing

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

func newSearchScopeFixture(tb testing.TB) (publicationFixture, *Service, *embeddingWorkerProvider, store.EmbeddingJobRequest) {
	tb.Helper()
	return newSearchScopeFixtureDimension(tb, document.EmbeddingInputOriginalFile, 2)
}

func newSearchScopeFixtureDimension(tb testing.TB, kind document.EmbeddingInputKind, dimension int) (publicationFixture, *Service, *embeddingWorkerProvider, store.EmbeddingJobRequest) {
	tb.Helper()
	fixture, fake, worker, job := newRealEmbeddingWorkerDimension(tb, kind, dimension)
	_, err := worker.ScanOnce(tb.Context())
	require.NoError(tb, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(tb.Context())
	require.NoError(tb, err)
	require.Len(tb, spaces, 1)
	index, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: fixture.catalog, Mutate: newTestOperationGate().MutateContext, Owner: "scope-test", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
		return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
	}})
	require.NoError(tb, err)
	_, err = index.Rebuild(tb.Context(), spaces[0])
	require.NoError(tb, err)
	provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: "query", descriptor: fake.descriptor}
	runtime, err := NewProviderEmbeddingRuntime(provider, fixture.blobs, tb.TempDir(), fake.runtime.Classify)
	require.NoError(tb, err)
	registry := NewEmbeddingRuntimeRegistry()
	require.NoError(tb, registry.Register(fake.descriptor.Fingerprint, runtime))
	var portable document.ProcessingProfileV1
	require.NoError(tb, json.Unmarshal(fixture.profile.CanonicalProfile, &portable))
	service := &Service{catalog: fixture.catalog, blobs: fixture.blobs, gate: newTestOperationGate(), principal: "scope-test", scope: "scope-test", embeddings: registry, profiles: map[string]configuredProfile{"private": {portable: portable, record: fixture.profile, embedders: map[string]document.EmbeddingProvider{job.BindingID: provider}}}}
	tb.Cleanup(func() { service.generationSource().Close() })
	_, err = fixture.catalog.GrantConsent(tb.Context(), store.ProcessingConsentGrantRequest{Principal: service.principal, Scope: service.scope, ProfileFingerprint: fixture.profile.Fingerprint, DisclosureFingerprint: portable.Embeddings[0].DisclosureFingerprint, InputClasses: []string{"query_text"}, RetainedArtifactClasses: []string{}})
	require.NoError(tb, err)
	return fixture, service, provider, job
}

func TestSearchScopeMatchesExplicitFence(t *testing.T) {
	t.Parallel()
	fixture, service, _, _ := newSearchScopeFixture(t)
	for i := range 298 {
		_, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), fmt.Sprintf("synthetic-%03d.txt", i), strings.Repeat("c", 64), 1, "text/plain")
		require.NoError(t, err)
	}
	resolution, err := fixture.catalog.ResolveProcessingSourceFence(t.Context(), store.ProcessingSourceFenceRequest{Filters: &store.SearchOptions{}})
	require.NoError(t, err)
	require.Len(t, resolution.ContentVersionIDs, 300)
	for _, mode := range []string{"lexical", "auto", "semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			request := SearchRequest{Query: "synthetic", Mode: mode, Profile: "private", Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: resolution.ContentVersionIDs}}
			explicit, err := service.Search(t.Context(), request)
			require.NoError(t, err)
			request.Fence.ContentVersionIDs = nil
			request.Scope = &SearchScope{Kind: "all_live"}
			scoped, err := service.Search(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, explicit.Results, scoped.Results)
			require.Equal(t, explicit.Coverage, scoped.Coverage)
			require.Equal(t, explicit.Truncated, scoped.Truncated)
			require.Equal(t, 300, scoped.ObservedScopeCount)
		})
	}
}

func TestSearchScopeFilterMatchesExplicitFence(t *testing.T) {
	t.Parallel()
	fixture, service, _, job := newSearchScopeFixture(t)
	indexed, err := fixture.catalog.NodeByPath(t.Context(), "/synthetic.png")
	require.NoError(t, err)
	parent, err := fixture.catalog.Mkdir(t.Context(), fixture.catalog.RootID(), "selected")
	require.NoError(t, err)
	indexed, _, err = fixture.catalog.Move(t.Context(), indexed.ID, parent.ID, indexed.Name, indexed.Revision)
	require.NoError(t, err)
	tag, err := fixture.catalog.CreateTag(t.Context(), "selected")
	require.NoError(t, err)
	_, err = fixture.catalog.AssignTag(t.Context(), tag.ID, indexed.ID, indexed.Revision)
	require.NoError(t, err)
	for _, filters := range []store.SearchOptions{{MIMEType: "image/png"}, {TagID: tag.ID}, {UnderNodeID: parent.ID}, {MIMEType: "image/png", ModifiedSince: "2020-01-01T00:00:00Z", ModifiedBefore: "2099-01-01T00:00:00Z"}} {
		for _, mode := range []string{"lexical", "semantic", "hybrid"} {
			request := SearchRequest{Query: "synthetic", Mode: mode, Profile: "private", Fence: SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{job.ContentVersionID}}}
			explicit, err := service.Search(t.Context(), request)
			require.NoError(t, err)
			request.Fence.ContentVersionIDs = nil
			request.Scope = &SearchScope{Kind: "all_live", Filters: &filters}
			scoped, err := service.Search(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, explicit.Results, scoped.Results)
			require.Equal(t, explicit.Coverage, scoped.Coverage)
			require.Equal(t, 1, scoped.ObservedScopeCount)
		}
	}
}

func TestSearchScopeRechecksMetadataDuringEncoding(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"tag", "ancestry", "replaced", "trashed"} {
		t.Run(change, func(t *testing.T) {
			fixture, service, provider, _ := newSearchScopeFixture(t)
			indexed, err := fixture.catalog.NodeByPath(t.Context(), "/synthetic.png")
			require.NoError(t, err)
			parent, err := fixture.catalog.Mkdir(t.Context(), fixture.catalog.RootID(), "selected")
			require.NoError(t, err)
			indexed, _, err = fixture.catalog.Move(t.Context(), indexed.ID, parent.ID, indexed.Name, indexed.Revision)
			require.NoError(t, err)
			tag, err := fixture.catalog.CreateTag(t.Context(), "selected")
			require.NoError(t, err)
			_, err = fixture.catalog.AssignTag(t.Context(), tag.ID, indexed.ID, indexed.Revision)
			require.NoError(t, err)
			indexed, err = fixture.catalog.NodeByID(t.Context(), indexed.ID)
			require.NoError(t, err)
			filters := &store.SearchOptions{TagID: tag.ID, UnderNodeID: parent.ID}
			paused := &pausedQueryEncoder{embeddingWorkerProvider: provider, entered: make(chan struct{}), resume: make(chan struct{})}
			runtime, err := NewProviderEmbeddingRuntime(paused, fixture.blobs, t.TempDir(), provider.runtime.Classify)
			require.NoError(t, err)
			service.embeddings = NewEmbeddingRuntimeRegistry()
			require.NoError(t, service.embeddings.Register(provider.descriptor.Fingerprint, runtime))
			type outcome struct {
				report retrieval.Report
				err    error
			}
			done := make(chan outcome, 1)
			release := sync.OnceFunc(func() { close(paused.resume) })
			finished := make(chan struct{})
			t.Cleanup(func() { release(); <-finished })
			go func() {
				defer close(finished)
				report, err := service.Search(t.Context(), SearchRequest{Query: "synthetic", Mode: "hybrid", Profile: "private", Fence: SourceFence{VaultUID: fixture.catalog.VaultID()}, Scope: &SearchScope{Kind: "all_live", Filters: filters}})
				done <- outcome{report, err}
			}()
			select {
			case <-paused.entered:
			case result := <-done:
				t.Fatalf("query did not pause: %v", result.err)
			case <-time.After(10 * time.Second):
				t.Fatal("query encoder did not start")
			}
			switch change {
			case "tag":
				_, err = fixture.catalog.UnassignTag(t.Context(), tag.ID, indexed.ID, indexed.Revision)
			case "ancestry":
				_, _, err = fixture.catalog.Move(t.Context(), indexed.ID, fixture.catalog.RootID(), indexed.Name, indexed.Revision)
			case "replaced":
				_, _, err = fixture.catalog.ReplaceContent(t.Context(), indexed.ID, indexed.Revision, strings.Repeat("f", 64), 1, "image/png")
			case "trashed":
				_, _, err = fixture.catalog.Trash(t.Context(), indexed.ID, indexed.Revision)
			}
			require.NoError(t, err)
			release()
			result := <-done
			if change == "replaced" || change == "trashed" {
				require.ErrorIs(t, result.err, store.ErrVectorIndexSourceStale)
				return
			}
			require.NoError(t, result.err)
			require.Empty(t, result.report.Results)
			require.True(t, result.report.Truncated)
			require.Zero(t, result.report.ObservedScopeCount)
		})
	}
}
