package processing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

type coverageRuntime struct {
	t      *testing.T
	base   *workerProvider
	source []byte
	mode   string
}

func (r coverageRuntime) Prepare(ctx context.Context, work store.RenditionJobWork, now time.Time) (RenditionExecution, error) {
	execution, err := (workerRuntime{provider: r.base}).Prepare(ctx, work, now)
	if err != nil {
		return RenditionExecution{}, err
	}
	execution.Upload = &workerUpload{Reader: bytes.NewReader(r.source), metadata: work.ExecutionIdentity.Upload}
	execution.Provider = coverageProvider{base: r.base, mode: r.mode, check: func(result document.RenditionResult) {
		r.t.Helper()
		require.NoError(r.t, document.ValidateRenditionResult(r.base.Descriptor(), execution.Authorization, result))
		candidate, err := buildRenditionJobCandidate(work, execution, result, 1, now)
		if r.mode == "blank" {
			require.ErrorContains(r.t, err, "evidence produced no readable text")
			return
		}
		require.NoError(r.t, err)
		require.NoError(r.t, store.ValidateRenditionBuildRecord(candidate.Build))
	}}
	return execution, nil
}

type coverageProvider struct {
	check func(document.RenditionResult)
	base  *workerProvider
	mode  string
}

func (p coverageProvider) Descriptor() document.RenditionDescriptor { return p.base.Descriptor() }
func (p coverageProvider) Render(ctx context.Context, upload document.AuthorizedUpload, auth document.RenditionAuthorization) (document.RenditionResult, error) {
	source, err := io.ReadAll(io.LimitReader(upload, 1<<20))
	if err != nil {
		return document.RenditionResult{}, err
	}
	if err = pdfapi.Validate(bytes.NewReader(source), nil); err != nil {
		return document.RenditionResult{}, fmt.Errorf("synthetic provider received invalid PDF: %w", err)
	}
	if p.mode == "failed" {
		failure, err := document.NewRenditionProviderError(document.RenditionErrorUnsupportedInput, 0, nil)
		if err != nil {
			return document.RenditionResult{}, err
		}
		return document.RenditionResult{}, failure
	}
	result, err := p.base.Render(ctx, upload, auth)
	if err != nil {
		return result, err
	}
	result.Evidence.Units[0].Text = "renditiononlyneedle"
	if p.mode == "partial" {
		result.Evidence.Completeness = document.EvidencePartial
		result.Evidence.UnitKind = document.EvidenceUnitPage
		result.Evidence.Units[0].Locator = document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}
		result.Evidence.Omissions = []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField, Field: "page_graphics", Reason: "Synthetic provider omitted page graphics"}}
	}
	if p.mode == "blank" {
		result.Evidence.Units[0].Text = ""
	}
	if err := document.ValidateSourceEvidenceV1(result.Evidence); err != nil {
		return document.RenditionResult{}, fmt.Errorf("invalid synthetic evidence: %w", err)
	}
	p.check(result)
	return result, nil
}

func TestCollectionCoverageConfiguredPDFWorkerToSearch(t *testing.T) {
	// Blank provider output is rejected by the existing worker. The store suite
	// separately proves that an activated empty build is classified as none.
	for _, mode := range []string{"complete", "partial", "blank", "failed"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			pdf := fpdf.New("P", "mm", "A4", "")
			pdf.SetCompression(false)
			pdf.AddPage()
			pdf.SetFont("Arial", "", 12)
			pdf.Cell(50, 10, "Synthetic blank manual")
			var encoded bytes.Buffer
			require.NoError(t, pdf.Output(&encoded))
			source := encoded.Bytes()
			require.NoError(t, pdfapi.Validate(bytes.NewReader(source), nil))
			require.NotContains(t, string(source), "renditiononlyneedle")
			receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
			require.NoError(t, err)
			run, err := fixture.catalog.BeginIngest(t.Context(), "cli", "Synthetic PDF coverage")
			require.NoError(t, err)
			node, _, err := fixture.catalog.IngestFile(t.Context(), run, fixture.catalog.RootID(), "manual.pdf", receipt.Hash, receipt.Size, "application/pdf", "synthetic://manual.pdf", "", processingBlobPhysical(t, receipt))
			require.NoError(t, err)
			provider := newWorkerProvider(t)
			profile := workerProcessingProfile(t, provider.Descriptor())
			request := workerJobRequest(node.CurrentVersionID, profile, provider.Descriptor())
			request.ExecutionIdentity.Upload.SHA256 = receipt.Hash
			request.ExecutionIdentity.Upload.ByteLength = receipt.Size
			request.ExecutionIdentity.Authorization.SourceSHA256 = receipt.Hash
			request.ExecutionIdentity.Authorization.SourceBytes = receipt.Size
			grantWorkerConsent(t, fixture.catalog, request)
			_, _, err = fixture.catalog.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			selection := store.CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint}
			before, err := fixture.catalog.CollectionQuality(t.Context(), run.ID(), selection, nil)
			require.NoError(t, err)
			require.NotNil(t, before.Collection.Coverage.Counts)
			assert.Equal(t, int64(1), before.Collection.Coverage.Counts.Unprocessed)
			hits, _, err := fixture.catalog.SearchPage(t.Context(), "renditiononlyneedle", 10)
			require.NoError(t, err)
			require.Empty(t, hits)
			registry := NewRenditionRuntimeRegistry()
			require.NoError(t, registry.Register(provider.Descriptor().Fingerprint, coverageRuntime{t: t, base: provider, source: source, mode: mode}))
			worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: registry, Gate: newWorkerTestGate(), Owner: "synthetic-coverage", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
			require.NoError(t, err)
			processed, err := worker.RunOne(t.Context())
			require.NoError(t, err)
			require.True(t, processed)
			after, err := fixture.catalog.CollectionQuality(t.Context(), run.ID(), selection, nil)
			require.NoError(t, err)
			counts := after.Collection.Coverage.Counts
			require.NotNil(t, counts)
			expected := map[string]store.CoverageCounts{"complete": {Complete: 1}, "partial": {Partial: 1}, "blank": {Failed: 1}, "failed": {Failed: 1}}
			assert.Equal(t, expected[mode], *counts)
			hits, _, err = fixture.catalog.SearchPage(t.Context(), "renditiononlyneedle", 10)
			require.NoError(t, err)
			if mode == "complete" || mode == "partial" {
				require.Len(t, hits, 1)
				assert.Equal(t, node.ID, hits[0].Node.ID)
			} else {
				assert.Empty(t, hits)
			}
		})
	}
}
