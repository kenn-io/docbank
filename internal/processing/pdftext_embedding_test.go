package processing

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/pdftext"
	"go.kenn.io/docbank/internal/store"
)

func TestPDFTextOmissionOnlyCompletesWithoutEmbeddingRequests(t *testing.T) {
	t.Parallel()
	renderer, err := pdftext.New(pdftext.Profile{MaxDocumentBytes: 1 << 20, MaxPages: 10})
	require.NoError(t, err)
	var calls atomic.Int64
	provider := localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(t), embed: func(context.Context) error { calls.Add(1); return nil }}
	fixture, original := newLocalTextBackfillFixture(t, renderer, provider)
	source := original.Service.profiles["pdf"]
	portable := source.portable
	portable.Rendition = new(*portable.Rendition)
	portable.Rendition.AdapterContract = pdftext.AdapterContract
	service, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"pdf": {Profile: portable, RenditionProvider: renderer, EmbeddingProviders: source.embedders, Tokenizers: source.tokenizers}}})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	require.NoError(t, service.grantProfileConsent(t.Context(), service.profiles["pdf"], nil))
	receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDFTextPages([]string{""}, false)))
	require.NoError(t, err)
	node, err := fixture.catalog.NodeByPath(t.Context(), "/source.pdf")
	require.NoError(t, err)
	node, _, err = fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	setup := &LocalTextSetup{Service: service, Selected: "pdf"}
	err = setup.process(t.Context(), store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name})
	require.NoError(t, err)
	require.Zero(t, calls.Load())
	profile := service.profiles["pdf"]
	jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), node.CurrentVersionID, profile.record.Fingerprint)
	require.NoError(t, err)
	require.Empty(t, jobs)
	version, err := fixture.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	gate := service.gate
	service.gate = &localTextGrantGate{workerTestGate: newWorkerTestGate(), err: errors.New("unexpected empty-generation write")}
	ids, err := service.runEmbeddings(t.Context(), version, profile, "", "", "", nil, processingRunOptions{})
	service.gate = gate
	require.NoError(t, err)
	require.Empty(t, ids)
	require.Zero(t, calls.Load())

	// The same configured embedder still receives readable page text.
	receipt, err = fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDFTextPages([]string{"synthetic readable text"}, false)))
	require.NoError(t, err)
	node, _, err = fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	err = setup.process(t.Context(), store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name})
	require.NoError(t, err)
	require.Positive(t, calls.Load())
}
