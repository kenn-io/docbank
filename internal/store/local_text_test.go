package store

import (
	"fmt"
	"testing"

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
	plain, err := s.CreateFile(t.Context(), s.RootID(), "unrendered.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	publish := func(record EmbeddingSetRecord) {
		t.Helper()
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	original := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), original))
	publish(original)

	record := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	retained, err = s.HasEmbeddingHeads(t.Context(), profile.Fingerprint, "chunk")
	require.NoError(t, err)
	require.False(t, retained, "staging is not published authority")
	items, err := s.MissingProcessingTargetsAfter(t.Context(), profile.Fingerprint, "", 2, 100)
	require.NoError(t, err)
	require.Contains(t, versionIDsForLocalText(items), version)
	require.Contains(t, versionIDsForLocalText(items), plain.CurrentVersionID)
	watched := func(watches ...string) []WatchedRenditionTarget {
		t.Helper()
		items, err := s.MissingWatchedRenditionTargetsAfter(t.Context(), profile.Fingerprint, "", 1, 100, watches)
		require.NoError(t, err)
		return items
	}
	require.Empty(t, watched("inbox"), "a version no watch recorded is not a watched target")
	content, err := s.ContentVersionByID(t.Context(), version)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO watch_sources(watch_name,source_ref,node_id,blob_hash,size) VALUES($1,$2,$3,$4,$5)`,
		"inbox", "nested/report.pdf", content.NodeID, content.BlobHash, content.Size)
	require.NoError(t, err)
	require.Empty(t, watched(), "no routed watch selects nothing")
	require.Empty(t, watched("other"), "another watch's source does not admit the version")
	node, err := s.NodeByID(t.Context(), content.NodeID)
	require.NoError(t, err)
	require.Equal(t, []WatchedRenditionTarget{{
		NodeID: node.ID, ContentVersionID: version, Name: node.Name,
		WatchName: "inbox", SourceRef: "nested/report.pdf", MediaType: content.MimeType,
	}}, watched("other", "inbox"), "a whole-file head does not settle chunk work")
	_, err = s.db.ExecContext(t.Context(), `UPDATE watch_sources SET size=size+1 WHERE node_id=$1`, content.NodeID)
	require.NoError(t, err)
	require.Empty(t, watched("inbox"), "the watch's latest bytes must be the current version")
	_, err = s.db.ExecContext(t.Context(), `UPDATE watch_sources SET size=$1 WHERE node_id=$2`, content.Size, content.NodeID)
	require.NoError(t, err)
	publish(record)
	require.Empty(t, watched("inbox"))
	retained, err = s.HasEmbeddingHeads(t.Context(), profile.Fingerprint, "chunk")
	require.NoError(t, err)
	require.True(t, retained)
	for _, key := range [][2]string{{profile.Fingerprint, "other"}, {fakeHash("f2"), "chunk"}} {
		retained, err = s.HasEmbeddingHeads(t.Context(), key[0], key[1])
		require.NoError(t, err)
		require.False(t, retained)
	}
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

func checkPDFTextDiscovery(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	versions := seedRenditionCatalogVersions(t, s)
	profile := catalogProcessingProfileWith(t, false, func(profile *document.ProcessingProfileV1) {
		profile.Rendition.Descriptor.ID = document.PDFTextProviderID
	})
	page, err := s.PDFTextTargetsAfter(t.Context(), profile.Fingerprint, "", 500<<20, 1, false)
	require.NoError(t, err)
	require.Len(t, page, 1)
	next, err := s.PDFTextTargetsAfter(t.Context(), profile.Fingerprint, page[0].ContentVersionID, 500<<20, 1, false)
	require.NoError(t, err)
	require.Len(t, next, 1)
	oversized, err := s.PDFTextTargetsAfter(t.Context(), profile.Fingerprint, "", 1, 50, false)
	require.NoError(t, err)
	require.Empty(t, oversized)
	request := renditionJobTestRequest(versions[0], profile)
	request.ExecutionIdentity.Authorization.ProviderID = document.PDFTextProviderID
	grantRenditionJobConsent(t, s, request)
	_, _, err = s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	items, err := s.PDFTextTargetsAfter(t.Context(), profile.Fingerprint, "", 500<<20, 50, false)
	require.NoError(t, err)
	require.NotContains(t, versionIDsForLocalText(items), versions[0])
}
