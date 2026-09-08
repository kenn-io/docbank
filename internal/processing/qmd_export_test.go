package processing

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/qmdexport"
	"go.kenn.io/docbank/internal/retrieval/qmdbridge"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestQMDBridgeQueriesCurrentRealLooseAndPackedAuthority(t *testing.T) {
	for _, packed := range []bool{false, true} {
		name := "loose"
		if packed {
			name = "packed"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
			require.NoError(t, err)
			publishTwoProfileRenditions(t, fixture, publisher)
			if packed {
				stats, err := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
				require.NoError(t, err)
				require.Positive(t, stats.BlobsPacked)
			}
			exportRoot := canonicalQMDExportRoot(t)
			receipt, err := qmdexport.PublishActive(t.Context(), exportRoot, "synthetic", fixture.catalog, fixture.blobs, qmdexport.Options{})
			require.NoError(t, err)
			require.Len(t, receipt.Manifest.Entries, 2)
			first, second := receipt.Manifest.Entries[1], receipt.Manifest.Entries[0]
			server := processingQMDServerResults(t, []processingQMDWireResult{
				{uri: first.URI, score: 0.9, snippet: "first remote"},
				{uri: second.URI, score: 0.4, snippet: "second remote"},
			})
			authorizer := &processingQMDAuthorizer{}
			client := newProcessingQMDClient(t, server, exportRoot, fixture.catalog, authorizer)
			results, err := client.Search(t.Context(), qmdbridge.Request{Searches: []qmdbridge.Search{{Type: qmdbridge.SearchLexical, Query: "synthetic terms"}}, Limit: 5})
			require.NoError(t, err)
			require.Len(t, results, 2)
			node, err := fixture.catalog.NodeByPath(t.Context(), "/source.pdf")
			require.NoError(t, err)
			require.Equal(t, node.ID, results[0].Document.NodeID)
			require.Equal(t, node.CurrentVersionID, results[0].Document.ContentVersionID)
			require.Equal(t, node.Revision, results[0].NodeRevision)
			require.Equal(t, "/source.pdf", results[0].Path)
			require.Equal(t, first.URI, results[0].QMDURI)
			require.Equal(t, first.AttachmentID, results[0].AttachmentID)
			require.InDelta(t, 0.9, results[0].Score, 0)
			require.Equal(t, "first remote", results[0].Excerpt)
			require.Equal(t, second.URI, results[1].QMDURI)
			require.Equal(t, second.AttachmentID, results[1].AttachmentID)
			require.InDelta(t, 0.4, results[1].Score, 0)
			require.Equal(t, "second remote", results[1].Excerpt)
			require.Equal(t, results[0].Document, results[1].Document)
			require.Equal(t, receipt.GenerationID, authorizer.operation.GenerationID)
			require.Equal(t, len("synthetic terms"), authorizer.operation.DisclosedBytes)
		})
	}
}

func TestQMDBridgePreservesRealStoreSourceFenceAcrossCallbacks(t *testing.T) {
	// Mutations caught: any callback alias can otherwise widen the local fence
	// from one exported live version to another without changing CURRENT.
	for _, packed := range []bool{false, true} {
		storageName := "loose"
		if packed {
			storageName = "packed"
		}
		for _, returnIncluded := range []bool{false, true} {
			caseName := "excluded"
			if returnIncluded {
				caseName = "included"
			}
			t.Run(storageName+"/"+caseName, func(t *testing.T) {
				fixture := newPublicationFixture(t)
				publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
				require.NoError(t, err)
				includedStage := fixture.stage(t, publicationIDs{"b1", "51", "91"},
					"synthetic included text", "synthetic included title")
				_, err = publisher.PublishRendition(t.Context(), includedStage)
				require.NoError(t, err)
				excludedStage := fixture.replacementStage(t, publicationIDs{"b2", "52", "92"},
					"synthetic excluded text", "synthetic excluded title")
				_, err = publisher.PublishRendition(t.Context(), excludedStage)
				require.NoError(t, err)
				if packed {
					stats, packErr := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
					require.NoError(t, packErr)
					require.Positive(t, stats.BlobsPacked)
				}

				exportRoot := canonicalQMDExportRoot(t)
				receipt, err := qmdexport.PublishActive(t.Context(), exportRoot, "synthetic",
					fixture.catalog, fixture.blobs, qmdexport.Options{})
				require.NoError(t, err)
				require.Len(t, receipt.Manifest.Entries, 2)
				entries := make(map[string]qmdexport.Entry, 2)
				for _, entry := range receipt.Manifest.Entries {
					entries[entry.ContentVersionID] = entry
				}
				included := includedStage.Attachment.ContentVersionID
				excluded := excludedStage.Attachment.ContentVersionID
				require.Contains(t, entries, included)
				require.Contains(t, entries, excluded)
				remoteVersion := excluded
				if returnIncluded {
					remoteVersion = included
				}
				payloads := make(chan string, 1)
				server := processingQMDSourceFenceServer(t, entries[remoteVersion].URI, payloads)

				callerIDs := []string{included}
				gate := &sourceFenceQMDGate{t: t, delegate: fixture.catalog, expected: included,
					excluded: excluded, callerIDs: callerIDs}
				authorizer := authorizerFunc(func(_ context.Context, operation qmdbridge.Operation) error {
					gate.returnedIDs[0] = excluded
					require.Equal(t, []string{included}, operation.Scope.ContentVersionIDs)
					operation.Scope.ContentVersionIDs[0] = excluded
					return nil
				})
				client := newProcessingQMDClient(t, server, exportRoot, gate, authorizer)
				results, searchErr := client.Search(t.Context(), qmdbridge.Request{
					Searches: []qmdbridge.Search{{Type: qmdbridge.SearchLexical, Query: "synthetic terms"}},
					Limit:    1, Scope: store.SearchOptions{ContentVersionIDs: callerIDs},
				})
				payload := <-payloads
				require.NotContains(t, payload, included)
				require.NotContains(t, payload, excluded)
				if !returnIncluded {
					require.ErrorIs(t, searchErr, store.ErrQMDExportAuthorityStale)
					require.Nil(t, results)
					return
				}
				require.NoError(t, searchErr)
				require.Len(t, results, 1)
				require.Equal(t, included, results[0].Document.ContentVersionID)
				require.Equal(t, "/source.pdf", results[0].Path)
			})
		}
	}
}

func TestQMDBridgeRejectsOneStaleSameNodeProfileWithoutPartialResult(t *testing.T) {
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	publishTwoProfileRenditions(t, fixture, publisher)
	exportRoot := canonicalQMDExportRoot(t)
	receipt, err := qmdexport.PublishActive(t.Context(), exportRoot, "synthetic", fixture.catalog, fixture.blobs, qmdexport.Options{})
	require.NoError(t, err)
	require.Len(t, receipt.Manifest.Entries, 2)
	server := processingQMDServerResults(t, []processingQMDWireResult{
		{uri: receipt.Manifest.Entries[1].URI, score: 0.9, snippet: "first remote"},
		{uri: receipt.Manifest.Entries[0].URI, score: 0.4, snippet: "second remote"},
	})
	gate := qmdAuthorityGate{delegate: fixture.catalog, before: func() {
		_, purgeErr := fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{
			AttachmentIDs: []string{receipt.Manifest.Entries[0].AttachmentID},
		})
		require.NoError(t, purgeErr)
	}}
	client := newProcessingQMDClient(t, server, exportRoot, gate, &processingQMDAuthorizer{})
	results, err := client.Search(t.Context(), qmdbridge.Request{Searches: []qmdbridge.Search{{Type: qmdbridge.SearchVector, Query: "synthetic query"}}, Limit: 5})
	require.ErrorIs(t, err, store.ErrQMDExportAuthorityStale)
	require.Nil(t, results)
}

func publishTwoProfileRenditions(t *testing.T, fixture publicationFixture, publisher *ArtifactPublisher) {
	t.Helper()
	first := fixture.stage(t, publicationIDs{"b1", "51", "91"}, "synthetic searchable text", "synthetic title")
	_, err := publisher.PublishRendition(t.Context(), first)
	require.NoError(t, err)
	second := fixture.stage(t, publicationIDs{"b1", "51", "91"}, "synthetic searchable text", "synthetic title")
	updateStagedProfile(t, &second, func(profile *document.ProcessingProfileV1) {
		profile.Retrieval.LexicalLimit = 99
	})
	second.Attachment.ID = processingHash("52")
	second.Head.AttachmentID = second.Attachment.ID
	second.LexicalGenerationID = processingHash("92")
	_, err = publisher.PublishRendition(t.Context(), second)
	require.NoError(t, err)
}

func TestQMDBridgeRealAuthorityDenialStopsHTTP(t *testing.T) {
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	staged := fixture.stage(t, publicationIDs{"b1", "51", "91"}, "synthetic searchable text", "synthetic title")
	_, err = publisher.PublishRendition(t.Context(), staged)
	require.NoError(t, err)
	exportRoot := canonicalQMDExportRoot(t)
	_, err = qmdexport.PublishActive(t.Context(), exportRoot, "synthetic", fixture.catalog, fixture.blobs, qmdexport.Options{})
	require.NoError(t, err)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	authorizer := &processingQMDAuthorizer{err: store.ErrQMDExportAuthorityStale}
	client := newProcessingQMDClient(t, server, exportRoot, fixture.catalog, authorizer)
	_, err = client.Search(t.Context(), qmdbridge.Request{Searches: []qmdbridge.Search{{Type: qmdbridge.SearchLexical, Query: "synthetic terms"}}, Limit: 5})
	require.ErrorIs(t, err, store.ErrQMDExportAuthorityStale)
	require.Zero(t, requests)
}

func TestQMDBridgeFencesRealStoreChangesWithoutRepublishingExport(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(*testing.T, publicationFixture) store.SearchOptions
		mutate   func(*testing.T, publicationFixture, *ArtifactPublisher)
		wantPath string
	}{
		{name: "unchanged", wantPath: "/source.pdf"},
		{name: "rendition head", mutate: func(t *testing.T, f publicationFixture, _ *ArtifactPublisher) {
			t.Helper()
			_, err := f.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{AttachmentIDs: []string{processingHash("51")}})
			require.NoError(t, err)
		}},
		{name: "content head and version", mutate: func(t *testing.T, f publicationFixture, _ *ArtifactPublisher) {
			t.Helper()
			node, err := f.catalog.NodeByPath(t.Context(), "/source.pdf")
			require.NoError(t, err)
			blobReceipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader([]byte("synthetic replacement source")))
			require.NoError(t, err)
			_, _, err = f.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, blobReceipt.Hash, blobReceipt.Size, "application/pdf", processingBlobPhysical(t, blobReceipt))
			require.NoError(t, err)
		}},
		{name: "trash", mutate: func(t *testing.T, f publicationFixture, _ *ArtifactPublisher) {
			t.Helper()
			node, err := f.catalog.NodeByPath(t.Context(), "/source.pdf")
			require.NoError(t, err)
			_, _, err = f.catalog.Trash(t.Context(), node.ID, node.Revision)
			require.NoError(t, err)
		}},
		{name: "suppression", mutate: func(t *testing.T, f publicationFixture, _ *ArtifactPublisher) {
			t.Helper()
			_, err := f.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{f.versionID}})
			require.NoError(t, err)
		}},
		{name: "tag removed", prepare: func(t *testing.T, f publicationFixture) store.SearchOptions {
			t.Helper()
			tag, err := f.catalog.CreateTag(t.Context(), "synthetic-scope")
			require.NoError(t, err)
			node, err := f.catalog.NodeByPath(t.Context(), "/source.pdf")
			require.NoError(t, err)
			_, err = f.catalog.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
			return store.SearchOptions{TagID: tag.ID}
		}, mutate: func(t *testing.T, f publicationFixture, _ *ArtifactPublisher) {
			t.Helper()
			tag, err := f.catalog.TagByName(t.Context(), "synthetic-scope")
			require.NoError(t, err)
			node, err := f.catalog.NodeByPath(t.Context(), "/source.pdf")
			require.NoError(t, err)
			_, err = f.catalog.UnassignTag(t.Context(), tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
		}},
		{name: "moved outside directory", prepare: prepareDirectoryScope, mutate: func(t *testing.T, f publicationFixture, _ *ArtifactPublisher) {
			t.Helper()
			outside, err := f.catalog.EnsureDir(t.Context(), f.catalog.RootID(), "outside")
			require.NoError(t, err)
			node, err := f.catalog.NodeByPath(t.Context(), "/inside/source.pdf")
			require.NoError(t, err)
			_, _, err = f.catalog.Move(t.Context(), node.ID, outside.ID, "source.pdf", node.Revision)
			require.NoError(t, err)
		}},
		{name: "renamed inside directory", prepare: prepareDirectoryScope, mutate: func(t *testing.T, f publicationFixture, _ *ArtifactPublisher) {
			t.Helper()
			inside, err := f.catalog.NodeByPath(t.Context(), "/inside")
			require.NoError(t, err)
			node, err := f.catalog.NodeByPath(t.Context(), "/inside/source.pdf")
			require.NoError(t, err)
			_, _, err = f.catalog.Move(t.Context(), node.ID, inside.ID, "renamed.pdf", node.Revision)
			require.NoError(t, err)
		}, wantPath: "/inside/renamed.pdf"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
			require.NoError(t, err)
			staged := fixture.stage(t, publicationIDs{"b1", "51", "91"}, "synthetic searchable text", "synthetic title")
			var scope store.SearchOptions
			if test.prepare != nil {
				scope = test.prepare(t, fixture)
			}
			_, err = publisher.PublishRendition(t.Context(), staged)
			require.NoError(t, err)
			exportRoot := canonicalQMDExportRoot(t)
			receipt, err := qmdexport.PublishActive(t.Context(), exportRoot, "synthetic", fixture.catalog, fixture.blobs, qmdexport.Options{})
			require.NoError(t, err)
			require.Len(t, receipt.Manifest.Entries, 1)
			gate := qmdAuthorityGate{delegate: fixture.catalog, before: func() {
				if test.mutate != nil {
					test.mutate(t, fixture, publisher)
				}
			}}
			client := newProcessingQMDClient(t, processingQMDServer(t, receipt.Manifest.Entries[0].URI), exportRoot, gate, &processingQMDAuthorizer{})
			results, err := client.Search(t.Context(), qmdbridge.Request{Searches: []qmdbridge.Search{{Type: qmdbridge.SearchVector, Query: "synthetic query"}}, Limit: 5, Scope: scope})
			if test.wantPath == "" {
				require.ErrorIs(t, err, store.ErrQMDExportAuthorityStale)
				require.Nil(t, results)
				return
			}
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Equal(t, test.wantPath, results[0].Path)
		})
	}
}

func prepareDirectoryScope(t *testing.T, fixture publicationFixture) store.SearchOptions {
	t.Helper()
	inside, err := fixture.catalog.EnsureDir(t.Context(), fixture.catalog.RootID(), "inside")
	require.NoError(t, err)
	node, err := fixture.catalog.NodeByPath(t.Context(), "/source.pdf")
	require.NoError(t, err)
	_, _, err = fixture.catalog.Move(t.Context(), node.ID, inside.ID, "source.pdf", node.Revision)
	require.NoError(t, err)
	return store.SearchOptions{UnderNodeID: inside.ID}
}

type qmdAuthorityGate struct {
	delegate *store.Store
	before   func()
}

func (a qmdAuthorityGate) NormalizeQMDSearchScope(ctx context.Context, scope store.SearchOptions) (store.SearchOptions, error) {
	return a.delegate.NormalizeQMDSearchScope(ctx, scope)
}
func (a qmdAuthorityGate) RevalidateQMDExportCandidates(ctx context.Context, candidates []store.QMDExportSource, scope store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
	if a.before != nil {
		a.before()
	}
	return a.delegate.RevalidateQMDExportCandidates(ctx, candidates, scope)
}

type processingQMDSecret struct{}

func (processingQMDSecret) ResolveSecret(context.Context, string) (string, error) {
	return "synthetic-secret", nil
}

type processingQMDAuthorizer struct {
	operation qmdbridge.Operation
	err       error
}

type authorizerFunc func(context.Context, qmdbridge.Operation) error

func (f authorizerFunc) AuthorizeQMDQuery(ctx context.Context, operation qmdbridge.Operation) error {
	return f(ctx, operation)
}

type sourceFenceQMDGate struct {
	t           *testing.T
	delegate    *store.Store
	expected    string
	excluded    string
	callerIDs   []string
	returnedIDs []string
}

func (a *sourceFenceQMDGate) NormalizeQMDSearchScope(ctx context.Context, scope store.SearchOptions) (store.SearchOptions, error) {
	normalized, err := a.delegate.NormalizeQMDSearchScope(ctx, scope)
	if err != nil {
		return store.SearchOptions{}, err
	}
	a.callerIDs[0] = a.excluded
	require.Equal(a.t, []string{a.expected}, scope.ContentVersionIDs)
	a.returnedIDs = normalized.ContentVersionIDs
	return normalized, nil
}

func (a *sourceFenceQMDGate) RevalidateQMDExportCandidates(ctx context.Context, candidates []store.QMDExportSource, scope store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
	require.Equal(a.t, []string{a.expected}, scope.ContentVersionIDs)
	live, err := a.delegate.RevalidateQMDExportCandidates(ctx, candidates, scope)
	scope.ContentVersionIDs[0] = a.excluded
	return live, err
}

func (a *processingQMDAuthorizer) AuthorizeQMDQuery(_ context.Context, operation qmdbridge.Operation) error {
	a.operation = operation
	return a.err
}

type processingQMDResolver struct{}

func (processingQMDResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

func newProcessingQMDClient(t *testing.T, server *httptest.Server, root string, authority qmdbridge.Authority, authorizer qmdbridge.Authorizer) *qmdbridge.Client {
	t.Helper()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client, err := qmdbridge.New(qmdbridge.Profile{ID: "synthetic-qmd", CompatibilityEpoch: "synthetic-current", SecretBinding: "secret:synthetic-qmd", EndpointPath: "/query", RequestTimeout: time.Second,
		EgressPolicy: providerhttp.EgressPolicy{Scheme: "http", Host: "qmd.test", Port: uint16(port), AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, ProxyMode: providerhttp.ProxyDisabled}},
		processingQMDSecret{}, authorizer, authority, root, processingQMDResolver{}, &http.Client{})
	require.NoError(t, err)
	return client
}

func processingQMDServer(t *testing.T, uri string) *httptest.Server {
	t.Helper()
	return processingQMDServerResults(t, []processingQMDWireResult{{uri: uri, score: 0.75, snippet: "synthetic excerpt"}})
}

type processingQMDWireResult struct {
	uri     string
	score   float64
	snippet string
}

func processingQMDServerResults(t *testing.T, results []processingQMDWireResult) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer synthetic-secret" {
			t.Errorf("unexpected synthetic QMD request method/auth: %s/%q", request.Method, request.Header.Get("Authorization"))
			http.Error(writer, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		var payload struct {
			Searches    []qmdbridge.Search `json:"searches"`
			Limit       int                `json:"limit"`
			MinScore    float64            `json:"minScore"`
			Collections []string           `json:"collections"`
			Intent      string             `json:"intent,omitempty"`
			Rerank      *bool              `json:"rerank,omitempty"`
		}
		if err := json.UnmarshalRead(request.Body, &payload, json.RejectUnknownMembers(true)); err != nil {
			t.Errorf("decode synthetic QMD request: %v", err)
			http.Error(writer, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		if len(payload.Collections) != 1 || payload.Collections[0] != "synthetic" {
			t.Errorf("unexpected synthetic QMD collection: %v", payload.Collections)
			http.Error(writer, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		var wire strings.Builder
		wire.WriteString(`{"results":[`)
		for index, result := range results {
			if index > 0 {
				wire.WriteString(`,`)
			}
			wire.WriteString(`{"docid":"#synthetic","file":"` + result.uri + `","title":"remote title","score":` +
				strconv.FormatFloat(result.score, 'f', -1, 64) + `,"context":null,"snippet":"` + result.snippet + `"}`)
		}
		wire.WriteString(`]}`)
		_, _ = writer.Write([]byte(wire.String()))
	}))
	t.Cleanup(server.Close)
	return server
}

func processingQMDSourceFenceServer(t *testing.T, uri string, payloads chan<- string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read synthetic QMD source-fence request: %v", err)
			http.Error(writer, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		payloads <- string(body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"results":[{"docid":"#synthetic","file":"` + uri +
			`","title":"remote title","score":0.75,"context":null,"snippet":"synthetic excerpt"}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func canonicalQMDExportRoot(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return filepath.Join(base, "qmd-export")
}

func TestQMDExportPublishesOnlyBodyFromRealWorkerEnvelope(t *testing.T) {
	// Mutations caught: exporting the retained bytes verbatim leaks the canonical
	// worker header; bypassing managed blobs hides packed-read regressions.
	for _, packed := range []bool{false, true} {
		name := "loose"
		if packed {
			name = "packed"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			provider := newWorkerProvider(t)
			profile := workerProcessingProfile(t, provider.Descriptor())
			request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
			grantWorkerConsent(t, fixture.catalog, request)
			job, view := runWorkerEnvelopeJob(t, fixture, provider, profile, request)
			retained := retainedWorkerMarkdown(t, fixture, view.Build)
			facts, body, err := document.ParseRenditionFrontMatterV1(retained)
			require.NoError(t, err)
			require.Equal(t, job.ID, facts.Rendition.BuildID)
			if packed {
				stats, packErr := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
				require.NoError(t, packErr)
				require.Positive(t, stats.BlobsPacked)
			}

			sources, err := fixture.catalog.QMDExportSources(t.Context(), 10)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			generation, err := qmdexport.Build(t.Context(), "worker-envelope", sources,
				fixture.blobs, qmdexport.Options{})
			require.NoError(t, err)
			require.Len(t, generation.Manifest.Entries, 1)
			entry := generation.Manifest.Entries[0]
			require.Equal(t, sources[0].VaultUID, entry.VaultUID)
			require.Equal(t, sources[0].NodeID, entry.NodeID)
			require.Equal(t, sources[0].ContentVersionID, entry.ContentVersionID)
			require.Equal(t, sources[0].ProcessingProfileFingerprint, entry.ProcessingProfileFingerprint)
			require.Equal(t, sources[0].AttachmentID, entry.AttachmentID)
			require.Equal(t, sources[0].BuildID, entry.BuildID)
			require.Equal(t, sources[0].ArtifactID, entry.ArtifactID)
			require.Equal(t, sources[0].BlobSHA256, entry.BlobSHA256)
			require.Equal(t, sources[0].BlobSize, entry.BlobSize)
			require.Equal(t, sources[0].ArtifactChecksum, entry.ArtifactChecksum)
			require.Equal(t, sources[0].MarkdownChecksum, entry.MarkdownChecksum)
			require.Equal(t, processingSHA256(body), entry.ExportedMarkdownSHA256)
			require.Contains(t, entry.Frontmatter, document.RenditionMarkdownContractV1)

			exportRoot := canonicalQMDExportRoot(t)
			receipt, err := qmdexport.Publish(t.Context(), exportRoot, "worker-envelope",
				sources, fixture.blobs, qmdexport.Options{})
			require.NoError(t, err)
			require.Len(t, receipt.Manifest.Entries, 1)
			exported, err := os.ReadFile(filepath.Join(
				receipt.CollectionPath, receipt.Manifest.Entries[0].RelativePath))
			require.NoError(t, err)
			require.Equal(t, body, exported)
			require.Equal(t, processingSHA256(body), processingSHA256(exported))
			require.NotContains(t, string(exported), document.RenditionMarkdownContractV1)
			require.NotContains(t, string(exported), job.ID)
		})
	}
}

func TestQMDExportBuildsFromPublishedLooseAndPackedRenditions(t *testing.T) {
	// Mutation caught: bypassing the real publisher/catalog/blob graph can hide
	// loose-versus-packed read failures or select a non-sanitized artifact.
	for _, packed := range []bool{false, true} {
		name := "loose"
		if packed {
			name = "packed"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
			require.NoError(t, err)
			staged := fixture.stage(t, publicationIDs{"b1", "51", "91"},
				"synthetic lexical text", "synthetic title")
			providerMarkdown := []byte("synthetic provider-only markdown")
			updateStagedProfile(t, &staged, func(profile *document.ProcessingProfileV1) {
				profile.Rendition.RequestedArtifacts = append(
					profile.Rendition.RequestedArtifacts, document.EvidenceArtifactMarkdown)
				profile.RetentionDisclosure.RetainProviderMarkdown = true
			})
			policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"provider_markdown"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
			staged.Build.CapturedArtifactPolicy = policy
			staged.Build.CapturedArtifactPolicyFingerprint = processingSHA256(policy)
			providerHash := processingSHA256(providerMarkdown)
			providerArtifactID := "artifact_" + processingHash("provider-markdown")
			staged.Build.Artifacts = append(staged.Build.Artifacts, store.RenditionArtifactRecord{
				ID: providerArtifactID, Role: string(document.EvidenceArtifactMarkdown),
				BlobHash: providerHash, Size: int64(len(providerMarkdown)), Checksum: providerHash,
				State: store.RenditionArtifactVerified,
			})
			staged.Build.DeclaredArtifactCount++
			staged.Artifacts = append(staged.Artifacts, StagedArtifact{
				ID: providerArtifactID, Payload: bytes.NewReader(providerMarkdown),
			})
			_, err = publisher.PublishRendition(t.Context(), staged)
			require.NoError(t, err)
			if packed {
				stats, packErr := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
				require.NoError(t, packErr)
				require.Positive(t, stats.BlobsPacked)
			}

			sources, err := fixture.catalog.QMDExportSources(t.Context(), 10)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			generation, err := qmdexport.Build(t.Context(), "synthetic", sources, fixture.blobs, qmdexport.Options{})
			require.NoError(t, err)
			require.Len(t, generation.Manifest.Entries, 1)
			entry := generation.Manifest.Entries[0]
			markdownArtifactIndex := -1
			for index, artifact := range staged.Build.Artifacts {
				if artifact.Role == "sanitized_markdown" {
					markdownArtifactIndex = index
				}
			}
			require.NotEqual(t, -1, markdownArtifactIndex)
			markdownArtifact := staged.Build.Artifacts[markdownArtifactIndex]
			require.Equal(t, staged.Build.ID, entry.BuildID)
			require.Equal(t, staged.Attachment.ID, entry.AttachmentID)
			require.Equal(t, markdownArtifact.ID, entry.ArtifactID)
			require.Equal(t, markdownArtifact.BlobHash, entry.BlobSHA256)
			require.Equal(t, staged.Rendition.MarkdownChecksum, entry.ExportedMarkdownSHA256)
			require.NotContains(t, string(staged.Rendition.Markdown), string(providerMarkdown))
			require.NotContains(t, entry.RelativePath, "source.pdf")
		})
	}
}

func TestQMDPublishActiveRetiresRealLooseAndPackedAuthority(t *testing.T) {
	// Catches exporting provider/header bytes, ignoring live catalog purge, or
	// reporting success while unverifiable stale exported text remains.
	for _, packed := range []bool{false, true} {
		for _, damaged := range []bool{false, true} {
			name := "loose"
			if packed {
				name = "packed"
			}
			if damaged {
				name += "-preserved"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newPublicationFixture(t)
				publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
				require.NoError(t, err)
				staged := fixture.stage(t, publicationIDs{"b1", "51", "91"}, "synthetic lexical text", "synthetic title")
				_, err = publisher.PublishRendition(t.Context(), staged)
				require.NoError(t, err)
				if packed {
					stats, err := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
					require.NoError(t, err)
					require.Positive(t, stats.BlobsPacked)
				}
				base, err := filepath.EvalSymlinks(t.TempDir())
				require.NoError(t, err)
				target := filepath.Join(base, "synthetic-export")
				first, err := qmdexport.PublishActive(t.Context(), target, "synthetic", fixture.catalog, fixture.blobs, qmdexport.Options{})
				require.NoError(t, err)
				require.Len(t, first.Manifest.Entries, 1)
				bodyPath := filepath.Join(first.CollectionPath, first.Manifest.Entries[0].RelativePath)
				body, err := os.ReadFile(bodyPath)
				require.NoError(t, err)
				require.Equal(t, staged.Rendition.Markdown, body)
				require.NotContains(t, string(body), "docbank: ")
				sentinel := filepath.Join(filepath.Dir(first.CollectionPath), "synthetic-unknown")
				if damaged {
					require.NoError(t, os.WriteFile(bodyPath, []byte("synthetic replacement"), 0o600))
					require.NoError(t, os.WriteFile(sentinel, []byte("preserve me"), 0o600))
				}
				_, err = fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{BuildIDs: []string{staged.Build.ID}})
				require.NoError(t, err)
				empty, err := qmdexport.PublishActive(t.Context(), target, "synthetic", fixture.catalog, fixture.blobs, qmdexport.Options{})
				require.NotEmpty(t, empty.GenerationID)
				require.NotEqual(t, first.GenerationID, empty.GenerationID)
				require.Empty(t, empty.Manifest.Entries)
				if damaged {
					var post *qmdexport.PostPublicationError
					require.ErrorAs(t, err, &post)
					require.Equal(t, "cleanup_incomplete", post.Code)
					require.Equal(t, 1, post.Cleanup.PreservedCandidates)
					value, err := os.ReadFile(sentinel)
					require.NoError(t, err)
					require.Equal(t, "preserve me", string(value))
					value, err = os.ReadFile(bodyPath)
					require.NoError(t, err)
					require.Equal(t, "synthetic replacement", string(value))
				} else {
					require.NoError(t, err)
					require.NoFileExists(t, bodyPath)
				}
				current, err := qmdexport.LoadCurrent(target)
				require.NoError(t, err)
				require.Equal(t, empty.GenerationID, current.GenerationID)
			})
		}
	}
}
