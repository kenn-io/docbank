package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

func TestHybridSearcherUsesRealStoreBindingAuthority(t *testing.T) {
	t.Parallel()
	fixture, fake, embedding, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile, "alternate")
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	var metadata bytes.Buffer
	require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &metadata))
	heads := make(map[string]string)
	for line := range bytes.SplitSeq(metadata.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var head struct {
			Type    string `json:"type"`
			Binding string `json:"binding_id"`
			Set     string `json:"embedding_set_id"`
		}
		require.NoError(t, json.Unmarshal(line, &head))
		if head.Type == "embedding_head" {
			heads[head.Binding] = head.Set
		}
	}
	require.Len(t, heads, 2)
	require.NotEqual(t, heads[request.BindingID], heads["alternate"])
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	index, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
		Catalog: fixture.catalog, Mutate: newTestOperationGate().MutateContext,
		Owner: "retrieval-test", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
		},
	})
	require.NoError(t, err)
	_, err = index.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err)
	provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: "query", descriptor: fake.descriptor}
	searcher, err := retrieval.NewSearcher(retrieval.SearcherConfig{
		Backend: fixture.catalog, Encoders: provider, Owner: "retrieval-test", LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	for _, binding := range []string{request.BindingID, "alternate"} {
		t.Run(binding, func(t *testing.T) {
			report, err := searcher.Search(t.Context(), retrieval.Query{
				Text: "synthetic", Mode: retrieval.ModeHybrid, Limit: 1,
				Scope:                        store.SearchOptions{MIMEType: "image/png"},
				ProcessingProfileFingerprint: request.Profile.Fingerprint, BindingID: binding,
				Authorization: document.EmbeddingAuthorization{
					ProviderID: fake.descriptor.ID, DescriptorFingerprint: fake.descriptor.Fingerprint,
					PolicyFingerprint: fake.descriptor.PolicyFingerprint,
					MaxBatchItems:     1, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
				},
			})
			require.NoError(t, err)
			require.Len(t, report.Results, 1)
			result := report.Results[0]
			assert.Equal(t, "/synthetic.png", result.Path)
			assert.Equal(t, request.ContentVersionID, result.Document.ContentVersionID)
			assert.Equal(t, 1, result.LexicalRank)
			assert.Equal(t, 1, result.SemanticRank)
			assert.Equal(t, 1, report.Coverage.CompleteDocuments)
			require.Len(t, result.Evidence, 2)
			assert.Equal(t, "embedding", result.Evidence[1].Kind)
			assert.Equal(t, heads[binding], result.Evidence[1].EmbeddingSetID)
		})
	}
}

func TestHybridSearchRevalidatesUnembeddedMatchesAfterQueryEmbedding(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"trashed", "replaced", "ancestor moved", "indexed tagged", "indexed renamed"} {
		t.Run(change, func(t *testing.T) {
			query := "race"
			indexedChange := strings.HasPrefix(change, "indexed ")
			if indexedChange {
				query = "synthetic"
			}
			fixture, fake, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
			_, err := worker.ScanOnce(t.Context())
			require.NoError(t, err)
			spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
			require.NoError(t, err)
			require.Len(t, spaces, 1)
			index, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
				Catalog: fixture.catalog, Mutate: newTestOperationGate().MutateContext,
				Owner: "race-test", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
				ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
					return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
				},
			})
			require.NoError(t, err)
			_, err = index.Rebuild(t.Context(), spaces[0])
			require.NoError(t, err)
			parent, err := fixture.catalog.Mkdir(t.Context(), fixture.catalog.RootID(), "before")
			require.NoError(t, err)
			changing, err := fixture.catalog.CreateFile(t.Context(), parent.ID, "change.txt", strings.Repeat("c", 64), 1, "text/plain")
			require.NoError(t, err)
			require.NoError(t, fixture.catalog.RecordExtraction(t.Context(), store.ExtractionResult{BlobHash: changing.BlobHash,
				Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: query + " old-version excerpt"}))
			kept, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), query+"-keep.txt", strings.Repeat("d", 64), 1, "text/plain")
			require.NoError(t, err)
			provider := &pausedQueryEncoder{embeddingWorkerProvider: &embeddingWorkerProvider{runtime: fake.runtime, binding: "query", descriptor: fake.descriptor},
				entered: make(chan struct{}), resume: make(chan struct{})}
			release := sync.OnceFunc(func() { close(provider.resume) })
			searcher, err := retrieval.NewSearcher(retrieval.SearcherConfig{Backend: fixture.catalog, Encoders: provider,
				Owner: "race-test", LeaseDuration: time.Minute})
			require.NoError(t, err)
			type outcome struct {
				report retrieval.Report
				err    error
			}
			done := make(chan outcome, 1)
			finished := make(chan struct{})
			t.Cleanup(func() {
				release()
				<-finished
			})
			go func() {
				defer close(finished)
				report, err := searcher.Search(t.Context(), retrieval.Query{Text: query, Mode: retrieval.ModeHybrid, Limit: 10,
					Scope:                        store.SearchOptions{ContentVersionIDs: []string{request.ContentVersionID, changing.CurrentVersionID, kept.CurrentVersionID}},
					ProcessingProfileFingerprint: request.Profile.Fingerprint, BindingID: request.BindingID,
					Authorization: document.EmbeddingAuthorization{ProviderID: fake.descriptor.ID, DescriptorFingerprint: fake.descriptor.Fingerprint,
						PolicyFingerprint: fake.descriptor.PolicyFingerprint, MaxBatchItems: 1, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20}})
				done <- outcome{report: report, err: err}
			}()
			select {
			case <-provider.entered:
			case result := <-done:
				t.Fatalf("search ended before query embedding paused: %v", result.err)
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			switch change {
			case "trashed":
				_, _, err = fixture.catalog.Trash(t.Context(), changing.ID, changing.Revision)
			case "replaced":
				_, _, err = fixture.catalog.ReplaceContent(t.Context(), changing.ID, changing.Revision, strings.Repeat("e", 64), 2, "text/plain")
			case "indexed tagged":
				indexed, lookupErr := fixture.catalog.NodeByPath(t.Context(), "/synthetic.png")
				require.NoError(t, lookupErr)
				tag, tagErr := fixture.catalog.CreateTag(t.Context(), "race-tag")
				require.NoError(t, tagErr)
				_, err = fixture.catalog.AssignTag(t.Context(), tag.ID, indexed.ID, indexed.Revision)
			case "indexed renamed":
				indexed, lookupErr := fixture.catalog.NodeByPath(t.Context(), "/synthetic.png")
				require.NoError(t, lookupErr)
				_, _, err = fixture.catalog.Move(t.Context(), indexed.ID, fixture.catalog.RootID(), "renamed.png", indexed.Revision)
			case "ancestor moved":
				parent, err = fixture.catalog.NodeByID(t.Context(), parent.ID)
				require.NoError(t, err)
				_, _, err = fixture.catalog.Move(t.Context(), parent.ID, fixture.catalog.RootID(), "after", parent.Revision)
			}
			require.NoError(t, err)
			release()
			result := <-done
			require.NoError(t, result.err)
			var foundChanging, foundKept, foundSemantic bool
			for i, hit := range result.report.Results {
				assert.Equal(t, i+1, hit.Rank)
				if hit.Document.NodeID == changing.ID {
					foundChanging = true
					switch change {
					case "ancestor moved":
						assert.Equal(t, "/after/change.txt", hit.Path)
					case "indexed tagged", "indexed renamed":
						assert.Equal(t, "/before/change.txt", hit.Path)
					}
				} else if hit.Document.NodeID == kept.ID {
					foundKept = true
				} else if hit.Document.ContentVersionID == request.ContentVersionID {
					foundSemantic = true
				}
			}
			assert.Equal(t, change == "ancestor moved" || indexedChange, foundChanging)
			assert.True(t, foundKept, "unchanged unembedded lexical match must survive")
			assert.Equal(t, change != "indexed renamed", foundSemantic,
				"a tag edit keeps the both-lane match; a rename invalidates its name evidence")
			assert.Equal(t, change != "ancestor moved" && change != "indexed tagged", result.report.Truncated)
		})
	}
}

type pausedQueryEncoder struct {
	*embeddingWorkerProvider

	entered, resume chan struct{}
}

func (p *pausedQueryEncoder) ResolveQueryEncoder(context.Context, document.EmbeddingDescriptor) (document.EmbeddingProvider, error) {
	return p, nil
}

func (p *pausedQueryEncoder) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	close(p.entered)
	select {
	case <-p.resume:
	case <-ctx.Done():
		return document.EmbeddingResult{}, ctx.Err()
	}
	return p.embeddingWorkerProvider.Embed(ctx, inputs, authorization)
}

func (p *embeddingWorkerProvider) ResolveQueryEncoder(context.Context, document.EmbeddingDescriptor) (document.EmbeddingProvider, error) {
	return p, nil
}
