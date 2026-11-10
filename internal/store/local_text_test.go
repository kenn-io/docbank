package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestMissingProcessingTargetsPagesCurrentLiveVersions(t *testing.T) {
	t.Parallel()
	checkMissingProcessingTargets(t, newTestStore)
}

func TestMissingProcessingTargetsPostgres(t *testing.T) {
	t.Parallel()
	checkMissingProcessingTargets(t, newPostgresQueryStore)
}

func checkMissingProcessingTargets(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	directory, err := s.Mkdir(t.Context(), s.RootID(), "temporary")
	require.NoError(t, err)
	for i := range 3 {
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("text-%02d.txt", i), fakeHash("a1"), 12, "text/plain")
		require.NoError(t, err)
	}
	hidden, err := s.CreateFile(t.Context(), directory.ID, "hidden.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	directory, err = s.NodeByID(t.Context(), directory.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), directory.ID, directory.Revision)
	require.NoError(t, err)
	first, err := s.MissingProcessingTargetsAfter(t.Context(), fakeHash("f1"), "", 1, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	last, err := s.MissingProcessingTargetsAfter(t.Context(), fakeHash("f1"), first[1].ContentVersionID, 1, 2)
	require.NoError(t, err)
	require.Len(t, last, 1)
	require.Less(t, first[0].ContentVersionID, first[1].ContentVersionID)
	require.Less(t, first[1].ContentVersionID, last[0].ContentVersionID)
	for _, item := range append(first, last...) {
		require.NotEqual(t, hidden.CurrentVersionID, item.ContentVersionID)
		node, err := s.NodeByID(t.Context(), item.NodeID)
		require.NoError(t, err)
		require.Equal(t, node.Name, item.Name)
	}
	node, err := s.NodeByID(t.Context(), last[0].NodeID)
	require.NoError(t, err)
	_, _, err = s.Move(t.Context(), node.ID, *node.ParentID, "renamed.pdf", node.Revision)
	require.NoError(t, err)
	last, err = s.MissingProcessingTargetsAfter(t.Context(), fakeHash("f1"), first[1].ContentVersionID, 1, 2)
	require.NoError(t, err)
	require.Equal(t, "renamed.pdf", last[0].Name)
	require.Equal(t, node.CurrentVersionID, last[0].ContentVersionID)
	key := EmbeddingHeadKey{ContentVersionID: node.CurrentVersionID, BindingID: "local-text", InputKind: document.EmbeddingInputRenditionChunk}
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO derivative_purge_suppressions(source_sha256,profile_fingerprint,build_id,purged_at,active) VALUES($1,$2,$3,$4,1)`, fakeHash("a1"), derivativeBuildSuppressionProfile, embeddingPurgeScope(key, fakeHash("f1")), "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	suppressed, err := s.EmbeddingPurgeSuppressedVersions(t.Context(), fakeHash("f1"), []EmbeddingHeadKey{key})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{node.CurrentVersionID: true}, suppressed)
	suppressed, err = s.EmbeddingPurgeSuppressedVersions(t.Context(), fakeHash("f2"), []EmbeddingHeadKey{key})
	require.NoError(t, err)
	require.Empty(t, suppressed, "another profile has its own suppression identity")
}

func TestLocalTextEmbeddingHeadsAndPendingTargets(t *testing.T) {
	t.Parallel()
	checkLocalTextEmbeddingHeads(t, newTestStore)
}

func TestLocalTextEmbeddingHeadsAndPendingTargetsPostgres(t *testing.T) {
	t.Parallel()
	checkLocalTextEmbeddingHeads(t, newPostgresQueryStore)
}

func checkLocalTextEmbeddingHeads(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s, version, profile, attachment := seedEmbeddingCatalog(t, open(t))
	retained, err := s.HasEmbeddingHeads(t.Context(), profile.Fingerprint, "chunk")
	require.NoError(t, err)
	require.False(t, retained)
	record := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	retained, err = s.HasEmbeddingHeads(t.Context(), profile.Fingerprint, "chunk")
	require.NoError(t, err)
	require.False(t, retained, "staging is not published authority")
	items, err := s.MissingProcessingTargetsAfter(t.Context(), profile.Fingerprint, "", 1, 100)
	require.NoError(t, err)
	require.Contains(t, versionIDsForLocalText(items), version)
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: record.BindingID, InputKind: record.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
	items, err = s.MissingProcessingTargetsAfter(t.Context(), profile.Fingerprint, "", 1, 100)
	require.NoError(t, err)
	require.NotContains(t, versionIDsForLocalText(items), version)
	retained, err = s.HasEmbeddingHeads(t.Context(), profile.Fingerprint, "chunk")
	require.NoError(t, err)
	require.True(t, retained)
	for _, key := range [][2]string{{profile.Fingerprint, "other"}, {fakeHash("f2"), "chunk"}} {
		retained, err = s.HasEmbeddingHeads(t.Context(), key[0], key[1])
		require.NoError(t, err)
		require.False(t, retained)
	}
	content, err := s.ContentVersionByID(t.Context(), version)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), content.NodeID, UnconditionalRev)
	require.NoError(t, err)
	retained, err = s.HasEmbeddingHeads(t.Context(), profile.Fingerprint, "chunk")
	require.NoError(t, err)
	require.True(t, retained, "trashed documents retain reusable vectors")
	t.Run("image head", func(t *testing.T) {
		fixture := seedImageEmbeddingCatalog(t, open(t))
		fixture.publish(t)
		items, err := fixture.s.MissingProcessingTargetsAfter(t.Context(), fixture.request.Profile.Fingerprint, "", 1, 100)
		require.NoError(t, err)
		require.Contains(t, versionIDsForLocalText(items), fixture.node.CurrentVersionID, "an image head does not finish text")
	})
}

func versionIDsForLocalText(items []DocumentSummary) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ContentVersionID
	}
	return ids
}

func TestPDFTextDiscoveryAndWaitingCoverage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	versions := seedRenditionCatalogVersions(t, s)
	profile := catalogProcessingProfileWith(t, false, func(profile *document.ProcessingProfileV1) {
		profile.Rendition.Descriptor.ID = document.PDFTextProviderID
	})
	page, err := s.MissingPDFTextTargetsAfter(t.Context(), profile.Fingerprint, "", 500<<20, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	next, err := s.MissingPDFTextTargetsAfter(t.Context(), profile.Fingerprint, page[0].ContentVersionID, 500<<20, 1)
	require.NoError(t, err)
	require.Len(t, next, 1)
	coverage, err := s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, nil)
	require.NoError(t, err)
	require.Zero(t, coverage.Pending)
	require.Zero(t, coverage.NoText)
	request := renditionJobTestRequest(versions[0], profile)
	request.ExecutionIdentity.Authorization.ProviderID = document.PDFTextProviderID
	grantRenditionJobConsent(t, s, request)
	job, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	items, err := s.PDFTextTargetsAfter(t.Context(), profile.Fingerprint, "", 500<<20, 50, false)
	require.NoError(t, err)
	require.NotContains(t, versionIDsForLocalText(items), versions[0])
	coverage, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, nil)
	require.NoError(t, err)
	require.Equal(t, 1, coverage.Pending)
	now := time.Now().UTC().Add(time.Second)
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	trashed, _, err := s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "trash-race", now, time.Minute)
	require.NoError(t, err)
	_, err = s.RenditionJobWorkByClaim(t.Context(), claim, now)
	require.ErrorIs(t, err, ErrRenditionJobStaleAuthority)
	require.NoError(t, s.MarkRenditionJobFailed(t.Context(), claim, RenditionFailureStaleAuthority, now))
	_, _, err = s.Restore(t.Context(), node.ID, trashed.Revision)
	require.NoError(t, err)
	items, err = s.PDFTextTargetsAfter(t.Context(), profile.Fingerprint, "", 500<<20, 50, false)
	require.NoError(t, err)
	require.Contains(t, versionIDsForLocalText(items), versions[0])
	job, _, err = s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	claim, err = s.ClaimRenditionJob(t.Context(), job.ID, "pdf-worker", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.MarkRenditionJobFailed(t.Context(), claim, RenditionFailureTerminal, now))
	coverage, err = s.SearchTextCoverage(t.Context(), SearchOptions{}, profile.Fingerprint, nil)
	require.NoError(t, err)
	require.Zero(t, coverage.Pending)
	require.Zero(t, coverage.NoText)
}
