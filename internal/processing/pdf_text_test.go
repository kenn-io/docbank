package processing

import (
	"bytes"
	"context"
	"encoding/base64"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
		{"mixed", []string{"distinctivequartz", ""}, true},
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
			if test.name == "born" {
				reads := 0
				_, err := fixture.catalog.SearchTextCoverage(t.Context(), store.SearchOptions{}, active.record.Fingerprint, func(context.Context, string) ([]int, error) {
					reads++
					return nil, nil
				})
				require.NoError(t, err)
				require.Zero(t, reads)
			}
			if test.name != "born" {
				pages, err := service.PDFPagesWithoutText(t.Context(), view.Rendition.Build.ID)
				require.NoError(t, err)
				if test.text {
					require.Equal(t, []int{2}, pages)
				} else {
					require.Equal(t, []int{1, 2}, pages)
				}
				report, err := service.Coverage(t.Context(), name, SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}})
				require.NoError(t, err)
				require.Equal(t, []PageTextGap{{ContentVersionID: node.CurrentVersionID, Pages: pages}}, report.PagesWithoutText)
				coverage, err := fixture.catalog.SearchTextCoverage(t.Context(), store.SearchOptions{}, active.record.Fingerprint, service.PDFPagesWithoutText)
				require.NoError(t, err)
				require.Zero(t, coverage.Pending)
				require.Equal(t, 1, coverage.PDFsWithoutText)
				wantPages := 2
				if test.text {
					wantPages = 1
				}
				require.Equal(t, wantPages, coverage.PagesWithoutText)
				if test.name == "mixed" {
					for _, artifact := range view.Rendition.Build.Artifacts {
						if artifact.Role == "normalized_evidence" {
							require.NoError(t, fixture.blobs.Remove(artifact.BlobHash))
						}
					}
					corruptReport, err := service.Coverage(t.Context(), name, SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}})
					require.NoError(t, err)
					require.Equal(t, report.Renditions, corruptReport.Renditions)
					require.Empty(t, corruptReport.PagesWithoutText)
				}
			}
		})
	}
}

func TestCoveragePageContinuesAndRejectsChangedManifest(t *testing.T) {
	pages := make([]int, 9000)
	for i := range pages {
		pages[i] = i + 1
	}
	ids := []string{"version", "later"}
	builds := map[string]string{"version": "build", "later": "later-build"}
	reads := []string{}
	read := func(_ context.Context, build string) ([]int, error) {
		reads = append(reads, build)
		if build == "build" {
			return pages, nil
		}
		return []int{1}, nil
	}
	first, cursor, _, err := readPDFCoveragePage(t.Context(), ids, builds, "scope", "manifest", "", read)
	require.NoError(t, err)
	require.Len(t, first[0].Pages, 8192)
	require.Equal(t, []string{"build"}, reads)
	require.NotEmpty(t, cursor)
	reads = nil
	last, next, _, err := readPDFCoveragePage(t.Context(), ids, builds, "scope", "manifest", cursor, read)
	require.NoError(t, err)
	require.Equal(t, pages[8192:], last[0].Pages)
	require.Equal(t, []string{"build", "later-build"}, reads)
	require.Empty(t, next)
	_, _, _, err = readPDFCoveragePage(t.Context(), ids, builds, "scope", "changed", cursor, read)
	require.Error(t, err)
	_, _, _, err = readPDFCoveragePage(t.Context(), ids, builds, "changed", "manifest", cursor, read)
	require.Error(t, err)
	for _, payload := range []string{`{"Offset":8192}`, `{"version":1,"scope":"scope","manifest":"manifest","index":0,"offset":1,"extra":true}`} {
		_, _, _, err = readPDFCoveragePage(t.Context(), ids, builds, "scope", "manifest", base64.RawURLEncoding.EncodeToString([]byte(payload)), read)
		require.Error(t, err)
	}
}

func TestCoverageContinuationDoesNotReadEarlierBuilds(t *testing.T) {
	ids := []string{"one", "two", "three"}
	builds := map[string]string{"one": "first", "two": "second", "three": "third"}
	pages := make([]int, 5000)
	for i := range pages {
		pages[i] = i + 1
	}
	var reads []string
	read := func(_ context.Context, build string) ([]int, error) {
		reads = append(reads, build)
		return pages, nil
	}
	_, cursor, _, err := readPDFCoveragePage(t.Context(), ids, builds, "scope", "manifest", "", read)
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second"}, reads)
	reads = nil
	_, _, _, err = readPDFCoveragePage(t.Context(), ids, builds, "scope", "manifest", cursor, read)
	require.NoError(t, err)
	require.Equal(t, []string{"second", "third"}, reads)
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

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestPDFTextRunnerSkipPolicy(t *testing.T) {
	for _, scenario := range []string{"trashed", "refused", "purged", "cap"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			_, profile, err := NewPDFTextProfile()
			require.NoError(t, err)
			service := newPDFTextService(t, fixture, profile)
			runner := &PDFTextRunner{Service: service}
			require.NoError(t, runner.Initialize(t.Context()))
			data := mediatest.PDFTextPages([]string{"searchable"}, false)
			if scenario == "refused" || scenario == "cap" {
				data = []byte("invalid PDF")
			}
			node := replacePDFSource(t, fixture, data)
			item := store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
			switch scenario {
			case "trashed":
				trashed, _, err := fixture.catalog.Trash(t.Context(), node.ID, node.Revision)
				require.NoError(t, err)
				require.NoError(t, runner.process(t.Context(), item))
				_, _, err = fixture.catalog.Restore(t.Context(), trashed.ID, trashed.Revision)
				require.NoError(t, err)
				items, err := runner.targets(t.Context(), "", 50)
				require.NoError(t, err)
				require.Len(t, items, 1)
				require.Equal(t, node.CurrentVersionID, items[0].ContentVersionID)
			case "refused":
				written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDFTextPages([]string{"searchable"}, false)))
				require.NoError(t, err)
				second, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "second.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
				require.NoError(t, err)
				require.NoError(t, runner.process(t.Context(), item))
				items, err := runner.targets(t.Context(), "", 1)
				require.NoError(t, err)
				require.Len(t, items, 1)
				require.Equal(t, second.CurrentVersionID, items[0].ContentVersionID)
				fresh := &PDFTextRunner{Service: service}
				items, err = fresh.targets(t.Context(), "", 50)
				require.NoError(t, err)
				require.Len(t, items, 2)
			case "purged":
				require.NoError(t, runner.process(t.Context(), item))
				runPDFTextWorker(t, service)
				_, err = fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{node.CurrentVersionID}})
				require.NoError(t, err)
				require.NoError(t, runner.process(t.Context(), item))
				pending, err := runner.Pending(t.Context())
				require.NoError(t, err)
				require.False(t, pending)
			case "cap":
				runner.skips = make(map[string]bool, localTextSkipLimit)
				for i := range localTextSkipLimit {
					runner.skips[strconv.Itoa(i)] = true
				}
				require.NoError(t, runner.process(t.Context(), item))
				require.Len(t, runner.skips, localTextSkipLimit)
			}
		})
	}
}
