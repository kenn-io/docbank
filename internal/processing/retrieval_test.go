package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
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

func (p *embeddingWorkerProvider) ResolveQueryEncoder(context.Context, document.EmbeddingDescriptor) (document.EmbeddingProvider, error) {
	return p, nil
}
