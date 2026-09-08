package retrieval

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestSearcherRealStoreExpansionMutationUsesCurrentAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mode   Mode
		mutate func(*testing.T, *retrievalStoreFixture)
		check  func(*testing.T, *retrievalStoreFixture, Report, error)
	}{
		{name: "source replacement", mode: ModeLexical,
			mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
				t.Helper()
				contents := []byte("synthetic replacement")
				replaced, _, err := fixture.store.ReplaceContent(t.Context(), fixture.target.ID,
					fixture.target.Revision, retrievalHashBytes(contents), int64(len(contents)), "text/plain")
				require.NoError(t, err)
				fixture.target = replaced
			}, check: func(t *testing.T, fixture *retrievalStoreFixture, report Report, err error) {
				t.Helper()
				require.NoError(t, err)
				requireResultVersion(t, report, fixture.target.ID, fixture.target.CurrentVersionID)
			}},
		{name: "trash", mode: ModeLexical,
			mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
				t.Helper()
				_, _, err := fixture.store.Trash(t.Context(), fixture.target.ID, fixture.target.Revision)
				require.NoError(t, err)
			}, check: func(t *testing.T, fixture *retrievalStoreFixture, report Report, err error) {
				t.Helper()
				require.NoError(t, err)
				assertNoResultNode(t, report, fixture.target.ID)
			}},
		{name: "ancestor rename", mode: ModeLexical,
			mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
				t.Helper()
				parent, err := fixture.store.NodeByID(t.Context(), fixture.parent.ID)
				require.NoError(t, err)
				_, _, err = fixture.store.Move(t.Context(), parent.ID, fixture.store.RootID(), "renamed-parent", parent.Revision)
				require.NoError(t, err)
			}, check: func(t *testing.T, fixture *retrievalStoreFixture, report Report, err error) {
				t.Helper()
				require.NoError(t, err)
				result := requireResultNode(t, report, fixture.target.ID)
				assert.Equal(t, "/renamed-parent/match-alpha.txt", result.Path)
			}},
		{name: "required coverage drift", mode: ModeAuto,
			mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
				t.Helper()
				contents := []byte("synthetic late document")
				_, err := fixture.store.CreateFile(t.Context(), fixture.store.RootID(), "match-late.txt",
					retrievalHashBytes(contents), int64(len(contents)), "text/plain")
				require.NoError(t, err)
			}, check: func(t *testing.T, _ *retrievalStoreFixture, report Report, err error) {
				t.Helper()
				require.NoError(t, err)
				assert.Equal(t, ModeLexical, report.ActualMode)
				assert.Equal(t, DegradationIncompleteCoverage, report.Degradation)
				assert.Equal(t, Coverage{BindingRequired: true, ScopedDocuments: 3,
					CompleteDocuments: 2, State: CoverageIncomplete}, report.Coverage)
			}},
		{name: "semantic source drift", mode: ModeHybrid,
			mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
				t.Helper()
				fixture.replaceEmbeddingHead(t, fixture.target, []float64{0, 1})
			}, check: func(t *testing.T, _ *retrievalStoreFixture, _ Report, err error) {
				t.Helper()
				require.ErrorIs(t, err, store.ErrVectorIndexSourceStale)
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRetrievalStoreFixture(t)
			expander := &stageExpander{variants: []string{"expanded"}, after: func() { test.mutate(t, fixture) }}
			searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{Enabled: true,
				Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1}, Provider: expander,
				Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade},
				RerankingConfig{})

			report, err := searcher.Search(t.Context(), fixture.query(test.mode, 10, store.SearchOptions{}))

			assert.Equal(t, 1, expander.calls)
			test.check(t, fixture, report, err)
		})
	}
}

func TestSearcherRealStoreRerankingAuthorizationMutationFailsBeforeEgress(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(*testing.T, *retrievalStoreFixture) store.SearchOptions
		mutate func(*testing.T, *retrievalStoreFixture)
	}{
		{name: "source replacement", mutate: replaceTargetSource},
		{name: "trash", mutate: trashTarget},
		{name: "tag scope removal", setup: tagBothFiles, mutate: untagTarget},
		{name: "ancestor rename", mutate: renameTargetAncestor},
		{name: "ancestor move", mutate: moveTargetAncestor},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRetrievalStoreFixture(t)
			var scope store.SearchOptions
			if test.setup != nil {
				scope = test.setup(t, fixture)
			}
			reranker := &stageReranker{}
			authorizer := &stageAuthorizer{after: func() { test.mutate(t, fixture) }}
			searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{},
				rankingConfig(reranker, authorizer, ProviderFailureDegrade))

			_, err := searcher.Search(t.Context(), fixture.query(ModeLexical, 2, scope))

			require.ErrorIs(t, err, ErrRerankingFailed)
			assert.Zero(t, reranker.calls)
			assert.Empty(t, reranker.candidates, "revoked excerpts must not reach provider payload capture")
		})
	}
}

func TestSearcherRealStoreRerankingAuthorizationRejectsReplacedRendition(t *testing.T) {
	fixture := newRetrievalStoreFixture(t)
	fixture.publishLexicalRendition(t, fixture.target, "initial", "classified original excerpt")
	reranker := &stageReranker{}
	authorizer := &stageAuthorizer{after: func() {
		fixture.publishLexicalRendition(t, fixture.target, "replacement", "replacement excerpt")
	}}
	searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{},
		rankingConfig(reranker, authorizer, ProviderFailureFailClosed))
	query := fixture.query(ModeLexical, 1, store.SearchOptions{})
	query.Text = "classified"

	_, err := searcher.Search(t.Context(), query)

	require.ErrorIs(t, err, ErrRerankingFailed)
	assert.Zero(t, reranker.calls)
	assert.NotContains(t, fmt.Sprintf("%#v", reranker.candidates), "classified original excerpt")
}

func TestSearcherRealStoreRerankingExecutionCannotReturnRevokedLexicalResults(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(*testing.T, *retrievalStoreFixture) store.SearchOptions
		mutate func(*testing.T, *retrievalStoreFixture)
	}{
		{name: "source replacement", mutate: replaceTargetSource},
		{name: "trash", mutate: trashTarget},
		{name: "tag scope removal", setup: tagBothFiles, mutate: untagTarget},
		{name: "ancestor rename", mutate: renameTargetAncestor},
		{name: "ancestor move", mutate: moveTargetAncestor},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRetrievalStoreFixture(t)
			originalPath := "/docs/match-alpha.txt"
			var scope store.SearchOptions
			if test.setup != nil {
				scope = test.setup(t, fixture)
			}
			reranker := &stageReranker{after: func() { test.mutate(t, fixture) }}
			searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{},
				rankingConfig(reranker, &stageAuthorizer{}, ProviderFailureDegrade))

			report, err := searcher.Search(t.Context(), fixture.query(ModeLexical, 2, scope))

			require.NoError(t, err)
			assert.Equal(t, 1, reranker.calls)
			assertNoResultNode(t, report, fixture.target.ID)
			assert.NotContains(t, fmt.Sprintf("%#v", report.Results), originalPath)
			assert.NotContains(t, fmt.Sprintf("%#v", report.Results), "match-alpha.txt")
		})
	}
}

func TestSearcherRealStoreRerankingExecutionCannotReturnReplacedRendition(t *testing.T) {
	fixture := newRetrievalStoreFixture(t)
	fixture.publishLexicalRendition(t, fixture.target, "initial", "classified original excerpt")
	reranker := &stageReranker{after: func() {
		fixture.publishLexicalRendition(t, fixture.target, "replacement", "replacement excerpt")
	}}
	searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{},
		rankingConfig(reranker, &stageAuthorizer{}, ProviderFailureDegrade))
	query := fixture.query(ModeLexical, 1, store.SearchOptions{})
	query.Text = "classified"

	report, err := searcher.Search(t.Context(), query)

	require.NoError(t, err)
	assert.Equal(t, 1, reranker.calls)
	assert.Empty(t, report.Results)
	assert.NotContains(t, fmt.Sprintf("%#v", report), "classified original excerpt")
}

func TestSearcherRealStoreRerankingExecutionPreservesHardSemanticAuthorityFailures(t *testing.T) {
	t.Run("source drift", func(t *testing.T) {
		fixture := newRetrievalStoreFixture(t)
		reranker := &stageReranker{after: func() {
			fixture.replaceEmbeddingHead(t, fixture.target, []float64{0, 1})
		}}
		searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{},
			rankingConfig(reranker, &stageAuthorizer{}, ProviderFailureDegrade))

		_, err := searcher.Search(t.Context(), fixture.query(ModeHybrid, 2, store.SearchOptions{}))

		require.ErrorIs(t, err, store.ErrVectorIndexSourceStale)
		assert.Equal(t, 1, reranker.calls)
	})

	t.Run("required coverage drift", func(t *testing.T) {
		fixture := newRetrievalStoreFixture(t)
		reranker := &stageReranker{after: func() {
			contents := []byte("synthetic late document")
			_, err := fixture.store.CreateFile(t.Context(), fixture.store.RootID(), "match-late.txt",
				retrievalHashBytes(contents), int64(len(contents)), "text/plain")
			require.NoError(t, err)
		}}
		searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{},
			rankingConfig(reranker, &stageAuthorizer{}, ProviderFailureDegrade))

		_, err := searcher.Search(t.Context(), fixture.query(ModeAuto, 2, store.SearchOptions{}))

		require.ErrorContains(t, err, "required semantic coverage changed during candidate revalidation")
		assert.Equal(t, 1, reranker.calls)
	})
}

func TestSearcherRealStoreOptionalStageControls(t *testing.T) {
	t.Run("unchanged source", func(t *testing.T) {
		fixture := newRetrievalStoreFixture(t)
		reranker := &stageReranker{}
		searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{},
			rankingConfig(reranker, &stageAuthorizer{}, ProviderFailureFailClosed))

		report, err := searcher.Search(t.Context(), fixture.query(ModeLexical, 2, store.SearchOptions{}))

		require.NoError(t, err)
		assert.Equal(t, 1, reranker.calls)
		assert.Len(t, report.Results, 2)
	})

	t.Run("provider degradation", func(t *testing.T) {
		fixture := newRetrievalStoreFixture(t)
		reranker := &stageReranker{err: assert.AnError}
		searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{},
			rankingConfig(reranker, &stageAuthorizer{}, ProviderFailureDegrade))

		report, err := searcher.Search(t.Context(), fixture.query(ModeLexical, 2, store.SearchOptions{}))

		require.NoError(t, err)
		assert.Equal(t, []Degradation{DegradationRerankingDegraded}, report.Degradations)
		assert.Equal(t, []ProviderReceipt{{Stage: ProviderStageReranking,
			Outcome: ProviderOutcomeUnavailable, CandidateCount: 2}}, report.Receipts)
		assert.Len(t, report.Results, 2)
	})

	t.Run("disabled", func(t *testing.T) {
		fixture := newRetrievalStoreFixture(t)
		expander := &stageExpander{variants: []string{"expanded"}}
		reranker := &stageReranker{}
		searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{
			Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1}, Provider: expander,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade,
		}, RerankingConfig{Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade})

		_, err := searcher.Search(t.Context(), fixture.query(ModeLexical, 2, store.SearchOptions{}))

		require.NoError(t, err)
		assert.Zero(t, expander.calls)
		assert.Zero(t, reranker.calls)
	})
}

func (fixture *retrievalStoreFixture) providerStageSearcher(t *testing.T, provider *retrievalMutatingProvider,
	expansion ExpansionConfig, reranking RerankingConfig,
) *Searcher {
	t.Helper()
	clockCalls := 0
	searcher, err := NewSearcher(SearcherConfig{Backend: fixture.store,
		Encoders: retrievalMutatingResolver{provider: provider}, Owner: "retrieval-stage-integration",
		LeaseDuration: time.Hour, Clock: func() time.Time {
			clockCalls++
			return time.Date(2026, 9, 7, 12, 30, clockCalls, 0, time.UTC)
		}, Expansion: expansion, Reranking: reranking})
	require.NoError(t, err)
	return searcher
}

func rankingConfig(provider RerankingProvider, authorizer RerankingAuthorizer,
	policy ProviderFailurePolicy,
) RerankingConfig {
	return RerankingConfig{Enabled: true, Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2},
		Provider: provider, Authorizer: authorizer, Deadline: time.Second, FailurePolicy: policy}
}

func replaceTargetSource(t *testing.T, fixture *retrievalStoreFixture) {
	t.Helper()
	contents := []byte("synthetic replacement")
	_, _, err := fixture.store.ReplaceContent(t.Context(), fixture.target.ID, fixture.target.Revision,
		retrievalHashBytes(contents), int64(len(contents)), "text/plain")
	require.NoError(t, err)
}

func trashTarget(t *testing.T, fixture *retrievalStoreFixture) {
	t.Helper()
	_, _, err := fixture.store.Trash(t.Context(), fixture.target.ID, fixture.target.Revision)
	require.NoError(t, err)
}

func tagBothFiles(t *testing.T, fixture *retrievalStoreFixture) store.SearchOptions {
	t.Helper()
	tag, err := fixture.store.CreateTag(t.Context(), "selected")
	require.NoError(t, err)
	assigned, err := fixture.store.AssignTag(t.Context(), tag.ID, fixture.target.ID, fixture.target.Revision)
	require.NoError(t, err)
	fixture.target = assigned.Node
	assigned, err = fixture.store.AssignTag(t.Context(), tag.ID, fixture.safe.ID, fixture.safe.Revision)
	require.NoError(t, err)
	fixture.safe = assigned.Node
	return store.SearchOptions{TagID: tag.ID}
}

func untagTarget(t *testing.T, fixture *retrievalStoreFixture) {
	t.Helper()
	_, err := fixture.store.UnassignTag(t.Context(), fixture.storeTagID(t, "selected"),
		fixture.target.ID, fixture.target.Revision)
	require.NoError(t, err)
}

func (fixture *retrievalStoreFixture) storeTagID(t *testing.T, name string) string {
	t.Helper()
	tag, err := fixture.store.TagByName(t.Context(), name)
	require.NoError(t, err)
	return tag.ID
}

func renameTargetAncestor(t *testing.T, fixture *retrievalStoreFixture) {
	t.Helper()
	parent, err := fixture.store.NodeByID(t.Context(), fixture.parent.ID)
	require.NoError(t, err)
	_, _, err = fixture.store.Move(t.Context(), parent.ID, fixture.store.RootID(), "renamed-parent", parent.Revision)
	require.NoError(t, err)
}

func moveTargetAncestor(t *testing.T, fixture *retrievalStoreFixture) {
	t.Helper()
	destination, err := fixture.store.Mkdir(t.Context(), fixture.store.RootID(), "destination")
	require.NoError(t, err)
	parent, err := fixture.store.NodeByID(t.Context(), fixture.parent.ID)
	require.NoError(t, err)
	_, _, err = fixture.store.Move(t.Context(), parent.ID, destination.ID, parent.Name, parent.Revision)
	require.NoError(t, err)
}

func (fixture *retrievalStoreFixture) publishLexicalRendition(t *testing.T, node store.Node, suffix, text string) {
	t.Helper()
	capturedPolicy := `{"roles":[{"max_count":1,"min_count":0,"role":"structured_evidence"}],"version":1}`
	if suffix == "replacement" {
		capturedPolicy = `{"roles":[{"max_count":2,"min_count":0,"role":"structured_evidence"}],"version":1}`
	}
	unitID := "rendition-unit-" + retrievalHash(suffix)
	build := store.RenditionBuildRecord{
		ID: retrievalHash("stage-build-" + suffix), VaultID: fixture.store.VaultID(), SourceSHA256: node.BlobHash,
		RenditionRequestFingerprint:       fixture.fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:        fixture.fingerprints.EvidenceLexical,
		CapturedArtifactPolicyFingerprint: retrievalHash(capturedPolicy),
		CapturedArtifactPolicy:            []byte(capturedPolicy),
		AuthorizationChecksum:             retrievalHash("rendition-authorization"),
		ProviderOperationID:               "synthetic-stage-rendition", ProviderReceipt: []byte(`{"provider":"synthetic"}`),
		EvidenceChecksum: retrievalHash("stage-evidence-" + suffix), RenditionChecksum: retrievalHash("stage-rendition-" + suffix),
		MarkdownChecksum: retrievalHash("stage-markdown-" + suffix), Completeness: document.EvidenceComplete,
		Warnings: []string{}, CompletedAt: "2026-09-07T12:40:00.000000000Z",
		Units: []store.RenditionUnitRecord{{ID: unitID, EvidenceUnitID: "evidence-unit-" + retrievalHash(suffix),
			Order: 0, Checksum: retrievalHash("unit-" + suffix), Locator: document.EvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}},
		LexicalSegments: []store.RenditionLexicalSegmentRecord{{ID: "lexical-segment-" + retrievalHash(suffix),
			UnitID: unitID, Order: 0, CharEnd: len([]rune(text)), Checksum: retrievalHashBytes([]byte(text)), Text: text}},
	}
	require.NoError(t, fixture.store.StageRenditionBuild(t.Context(), build))
	generation, err := fixture.store.StageLexicalGeneration(t.Context(), retrievalHash("stage-lexical-"+suffix))
	require.NoError(t, err)
	attachment := store.RenditionAttachmentRecord{ID: retrievalHash("stage-attachment-" + suffix),
		VaultID: fixture.store.VaultID(), ContentVersionID: node.CurrentVersionID, BuildID: build.ID,
		Profile: fixture.profile, AttachedAt: "2026-09-07T12:41:00.000000000Z"}
	require.NoError(t, fixture.store.PublishRenditionAndLexicalHeads(t.Context(), attachment,
		store.RenditionHeadRecord{ContentVersionID: node.CurrentVersionID,
			ProcessingProfileFingerprint: fixture.profile.Fingerprint, AttachmentID: attachment.ID,
			PublishedAt: "2026-09-07T12:42:00.000000000Z"}, generation.ID))
}

func requireResultNode(t *testing.T, report Report, nodeID int64) Result {
	t.Helper()
	for _, result := range report.Results {
		if result.Document.NodeID == nodeID {
			return result
		}
	}
	t.Fatalf("node %d not found in report", nodeID)
	return Result{}
}

func requireResultVersion(t *testing.T, report Report, nodeID int64, versionID string) {
	t.Helper()
	result := requireResultNode(t, report, nodeID)
	assert.Equal(t, versionID, result.Document.ContentVersionID)
}

func assertNoResultNode(t *testing.T, report Report, nodeID int64) {
	t.Helper()
	for _, result := range report.Results {
		assert.NotEqual(t, nodeID, result.Document.NodeID)
	}
}

var _ QueryExpansionProvider = (*stageExpander)(nil)
var _ RerankingProvider = (*stageReranker)(nil)
var _ ExpansionAuthorizer = (*stageAuthorizer)(nil)
var _ RerankingAuthorizer = (*stageAuthorizer)(nil)
