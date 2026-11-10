package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

// Each case publishes real PDFium output through the same lane as the daemon.
//
//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestPDFTextBackfillRetainsPages(t *testing.T) {
	for _, test := range []struct {
		name  string
		pages []string
		text  bool
	}{
		{"born", []string{"first page", "distinctivequartz second page"}, true},
		{"image-only", []string{"", ""}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			name, profile, err := NewPDFTextProfile()
			require.NoError(t, err)
			service := newPDFTextService(t, fixture, profile)
			require.NoError(t, service.grantProfileConsent(t.Context(), service.profiles[name], nil))
			data := mediatest.PDFTextPages(test.pages, false)
			node := replacePDFSource(t, fixture, data)

			setup := &PDFTextRunner{Service: service}
			items, err := setup.targets(t.Context(), "", 50)
			require.NoError(t, err)
			require.Len(t, items, 1)
			pending, err := setup.Pending(t.Context())
			require.NoError(t, err)
			require.True(t, pending)
			require.NoError(t, setup.process(t.Context(), items[0]))
			runPDFTextWorker(t, service)
			active := service.profiles[name]
			view, err := fixture.catalog.ResolveRenditionText(t.Context(), store.RenditionTextBinding{NodeID: node.ID, NodeRevision: node.Revision, ContentVersionID: node.CurrentVersionID, SourceSHA256: node.BlobHash, SourceSize: node.Size, ProfileFingerprint: active.record.Fingerprint})
			require.NoError(t, err)
			require.Equal(t, !test.text, view.Empty)
			if test.text {
				results, err := service.Search(t.Context(), SearchRequest{Profile: name, Query: "distinctivequartz", Mode: "lexical", Limit: 10, AllLive: &store.SearchOptions{}})
				require.NoError(t, err)
				require.Len(t, results.Results, 1)
			}
			restarted := &PDFTextRunner{Service: service}
			pending, err = restarted.Pending(t.Context())
			require.NoError(t, err)
			require.False(t, pending)
			if !test.text {
				_, err = service.RevokeConsent(t.Context())
				require.NoError(t, err)
				coverage, err := fixture.catalog.SearchTextCoverage(t.Context(), store.SearchOptions{}, active.record.Fingerprint, service.PDFHasNoTextLayer)
				require.NoError(t, err)
				require.Zero(t, coverage.Pending)
				require.Equal(t, 1, coverage.NoText, "retained evidence survives consent revocation")
			}
		})
	}
}

func newPDFTextService(t *testing.T, fixture publicationFixture, profile ProfileConfig) *Service {
	t.Helper()
	service, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{PDFTextProfileName: profile}})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	return service
}

func replacePDFSource(t *testing.T, fixture publicationFixture, data []byte) store.Node {
	t.Helper()
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	node, err := fixture.catalog.NodeByPath(t.Context(), "/source.pdf")
	require.NoError(t, err)
	node, _, err = fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	return node
}

func runPDFTextWorker(t *testing.T, service *Service) {
	t.Helper()
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: service.catalog, Blobs: service.blobs, Runtime: service.renditions, Gate: service.gate, Owner: "pdf-test", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	_, err = worker.RunOne(t.Context())
	require.NoError(t, err)
}

type retryPDFProvider struct {
	document.RenditionProvider

	fail bool
}

func (p *retryPDFProvider) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	if p.fail {
		p.fail = false
		err, _ := document.NewRenditionProviderError(document.RenditionErrorCapacity, time.Minute, errors.New("synthetic resource limit"))
		return document.RenditionResult{}, err
	}
	return p.RenditionProvider.Render(ctx, upload, authorization)
}

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestPDFTextRetryDoesNotBlockDiscovery(t *testing.T) {
	fixture := newPublicationFixture(t)
	name, profile, err := NewPDFTextProfile()
	require.NoError(t, err)
	provider := &retryPDFProvider{RenditionProvider: profile.RenditionProvider, fail: true}
	profile.RenditionProvider = provider
	service := newPDFTextService(t, fixture, profile)
	require.NoError(t, service.grantProfileConsent(t.Context(), service.profiles[name], nil))
	replacePDFSource(t, fixture, mediatest.PDFTextPages([]string{"firstquartz"}, false))
	runner := &PDFTextRunner{Service: service}
	items, err := runner.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.NoError(t, runner.process(t.Context(), items[0]))

	now := time.Now().UTC().Add(time.Second)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: service.catalog, Blobs: service.blobs, Runtime: service.renditions, Gate: service.gate, Owner: "pdf-retry", LeaseDuration: time.Minute, IdleDelay: time.Second, Clock: func() time.Time { return now }})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)

	data := mediatest.PDFTextPages([]string{"secondquartz"}, false)
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	_, err = fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "second.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	items, err = runner.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.NoError(t, runner.process(t.Context(), items[0]))
	now = now.Add(time.Second)
	processed, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	results, err := service.Search(t.Context(), SearchRequest{Profile: name, Query: "secondquartz", Mode: "lexical", Limit: 10, AllLive: &store.SearchOptions{}})
	require.NoError(t, err)
	require.Len(t, results.Results, 1)
	results, err = service.Search(t.Context(), SearchRequest{Profile: name, Query: "firstquartz", Mode: "lexical", Limit: 10, AllLive: &store.SearchOptions{}})
	require.NoError(t, err)
	require.Empty(t, results.Results)
	now = now.Add(2 * time.Minute)
	processed, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	results, err = service.Search(t.Context(), SearchRequest{Profile: name, Query: "firstquartz", Mode: "lexical", Limit: 10, AllLive: &store.SearchOptions{}})
	require.NoError(t, err)
	require.Len(t, results.Results, 1)
	results, err = service.Search(t.Context(), SearchRequest{Profile: name, Query: "secondquartz", Mode: "lexical", Limit: 10, AllLive: &store.SearchOptions{}})
	require.NoError(t, err)
	require.Len(t, results.Results, 1)
	pending, err := runner.Pending(t.Context())
	require.NoError(t, err)
	require.False(t, pending)
}

func TestPDFTextInitializePreservesRevokedConsent(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	name, profile, err := NewPDFTextProfile()
	require.NoError(t, err)
	service := newPDFTextService(t, fixture, profile)
	runner := &PDFTextRunner{Service: service}
	require.NoError(t, runner.Initialize(t.Context()))
	active, err := service.ProfileConsentActive(t.Context(), name)
	require.NoError(t, err)
	require.True(t, active)
	_, err = service.RevokeConsent(t.Context())
	require.NoError(t, err)
	require.NoError(t, runner.Initialize(t.Context()))
	active, err = service.ProfileConsentActive(t.Context(), name)
	require.NoError(t, err)
	require.False(t, active)
}

func TestPDFTextDiscoversRestoredSourceInSameRun(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	_, profile, err := NewPDFTextProfile()
	require.NoError(t, err)
	service := newPDFTextService(t, fixture, profile)
	runner := &PDFTextRunner{Service: service}
	require.NoError(t, runner.Initialize(t.Context()))
	node := replacePDFSource(t, fixture, mediatest.PDFTextPages([]string{"searchable"}, false))
	trashed, _, err := fixture.catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	require.NoError(t, runner.process(t.Context(), store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID}))
	_, _, err = fixture.catalog.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	items, err := runner.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, node.CurrentVersionID, items[0].ContentVersionID)
}

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestPDFTextPurgeSettlesDiscovery(t *testing.T) {
	fixture := newPublicationFixture(t)
	_, profile, err := NewPDFTextProfile()
	require.NoError(t, err)
	service := newPDFTextService(t, fixture, profile)
	runner := &PDFTextRunner{Service: service}
	require.NoError(t, runner.Initialize(t.Context()))
	node := replacePDFSource(t, fixture, mediatest.PDFTextPages([]string{"searchable"}, false))
	item := store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
	require.NoError(t, runner.process(t.Context(), item))
	runPDFTextWorker(t, service)
	_, err = fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{node.CurrentVersionID}})
	require.NoError(t, err)
	require.NoError(t, runner.process(t.Context(), item))
	pending, err := runner.Pending(t.Context())
	require.NoError(t, err)
	require.False(t, pending)
	_, err = fixture.catalog.EnsureDir(t.Context(), fixture.catalog.RootID(), "unrelated")
	require.NoError(t, err)
	pending, err = runner.Pending(t.Context())
	require.NoError(t, err)
	require.False(t, pending)
}

func TestPDFTextSkipCacheBound(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	_, profile, err := NewPDFTextProfile()
	require.NoError(t, err)
	service := newPDFTextService(t, fixture, profile)
	runner := &PDFTextRunner{Service: service, skips: make(map[string]bool, localTextSkipLimit)}
	for i := range localTextSkipLimit {
		runner.skips[fmt.Sprint(i)] = true
	}
	node := replacePDFSource(t, fixture, []byte("invalid PDF"))
	require.NoError(t, runner.process(t.Context(), store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID}))
	require.Equal(t, localTextSkipLimit, len(runner.skips))
}

func TestPDFTextDiscoverySkipsRefusedVersionsAcrossPages(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	_, profile, err := NewPDFTextProfile()
	require.NoError(t, err)
	service := newPDFTextService(t, fixture, profile)
	runner := &PDFTextRunner{Service: service}
	require.NoError(t, runner.Initialize(t.Context()))
	first := replacePDFSource(t, fixture, []byte("invalid PDF"))
	data := mediatest.PDFTextPages([]string{"searchable"}, false)
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	second, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "second.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	require.NoError(t, runner.process(t.Context(), store.DocumentSummary{NodeID: first.ID, ContentVersionID: first.CurrentVersionID}))
	items, err := runner.targets(t.Context(), "", 1)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, second.CurrentVersionID, items[0].ContentVersionID)
	fresh := &PDFTextRunner{Service: service}
	items, err = fresh.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Len(t, items, 2)
}
