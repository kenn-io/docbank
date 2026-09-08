package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestQMDExportSourcesDiscoversCurrentLiveSanitizedAuthority(t *testing.T) {
	// Mutation caught: dropping any live/current/head/verified role join would
	// expose the wrong artifact or fail to discover the retained rendition.
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339(),
	}
	require.NoError(t, publishAttachmentForTest(t, s, attachment))

	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	source := sources[0]
	require.Equal(t, s.VaultID(), source.VaultUID)
	require.Equal(t, versions[0], source.ContentVersionID)
	require.Equal(t, profile.Fingerprint, source.ProcessingProfileFingerprint)
	require.Equal(t, attachment.ID, source.AttachmentID)
	require.Equal(t, build.ID, source.BuildID)
	require.Equal(t, build.Artifacts[1].ID, source.ArtifactID)
	require.Equal(t, catalogMarkdownBlobHash, source.BlobSHA256)
	require.Equal(t, int64(len(catalogBlobContents[catalogMarkdownBlobHash])), source.BlobSize)
	require.Equal(t, catalogMarkdownBlobHash, source.ArtifactChecksum)
	require.Equal(t, catalogMarkdownBlobHash, source.MarkdownChecksum)
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	require.Equal(t, node.ID, source.NodeID)
}

func TestQMDExportSourcesRejectsContextAndLimits(t *testing.T) {
	// Mutation caught: deferring context/limit checks to SQL can accept nil or
	// return a partial snapshot instead of failing at the public boundary.
	s, _ := newRenditionCatalogFixture(t)
	_, err := s.QMDExportSources(nil, 1) //nolint:staticcheck // Nil is an explicit API contract case.
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.QMDExportSources(ctx, 1)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.QMDExportSources(t.Context(), 0)
	require.Error(t, err)
	_, err = s.QMDExportSources(t.Context(), maxQMDExportSources+1)
	require.Error(t, err)
}

func TestQMDExportSourcesOmitsTrashedAndReplacedVersions(t *testing.T) {
	// Mutation caught: joining heads without current/live node predicates would
	// preserve stale rendition authority after either node transition.
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Store)
	}{
		{name: "trash", mutate: func(t *testing.T, s *Store) {
			t.Helper()
			node, err := s.NodeViewByPath(t.Context(), "/synthetic-source-a.pdf")
			require.NoError(t, err)
			_, _, err = s.Trash(t.Context(), node.Node.ID, node.Node.Revision)
			require.NoError(t, err)
		}},
		{name: "replace current version", mutate: func(t *testing.T, s *Store) {
			t.Helper()
			node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
			require.NoError(t, err)
			_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("qmd-replacement"), 11, "application/pdf")
			require.NoError(t, err)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, versions := newRenditionCatalogFixture(t)
			publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst, catalogRenditionBuild(s, catalogProcessingProfile(t, false)))
			test.mutate(t, s)
			sources, err := s.QMDExportSources(t.Context(), 10)
			require.NoError(t, err)
			assert.Empty(t, sources)
		})
	}
}

func TestQMDExportSourcesOmitsPurgedSuppression(t *testing.T) {
	// Mutation caught: treating immutable artifact rows as live after explicit
	// purge would re-export revoked derivative authority.
	s, versions := newRenditionCatalogFixture(t)
	build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst, build)
	_, err := s.PurgeDerivatives(t.Context(), PurgeRequest{BuildIDs: []string{build.ID}})
	require.NoError(t, err)
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestQMDExportSourcesAppliesExactActiveSuppressionScope(t *testing.T) {
	// Mutation caught: matching suppression by source/build alone hides another
	// live attachment that shares the immutable build but was never purged.
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst, build)
	publishQMDStoreSource(t, s, versions[1], catalogAttachmentSecond, build)
	firstScope := derivativeAttachmentSuppressionScope(versions[0], profile.Fingerprint)
	_, err := s.db.Exec(`INSERT INTO derivative_purge_suppressions(
		source_sha256,profile_fingerprint,build_id,purged_at,active,
		superseded_at,superseding_build_id) VALUES(?,?,?,?,1,NULL,NULL)`,
		build.SourceSHA256, firstScope, build.ID, nowRFC3339())
	require.NoError(t, err)

	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, versions[1], sources[0].ContentVersionID)

	_, err = s.db.Exec(`UPDATE derivative_purge_suppressions SET active=0,
		superseded_at=?,superseding_build_id=? WHERE source_sha256=? AND
		profile_fingerprint=? AND build_id=?`, nowRFC3339(), fakeHash("qmd-superseding"),
		build.SourceSHA256, firstScope, build.ID)
	require.NoError(t, err)
	sources, err = s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 2)
}

func TestQMDExportSourcesAppliesBuildSuppressionAndFiltersBeforeCallerLimit(t *testing.T) {
	// Mutation caught: applying the caller limit before exact suppression either
	// truncates the surviving source or exposes build-wide revoked authority.
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst, build)
	publishQMDStoreSource(t, s, versions[1], catalogAttachmentSecond, build)
	firstScope := derivativeAttachmentSuppressionScope(versions[0], profile.Fingerprint)
	_, err := s.db.Exec(`INSERT INTO derivative_purge_suppressions(
		source_sha256,profile_fingerprint,build_id,purged_at,active,
		superseded_at,superseding_build_id) VALUES(?,?,?,?,1,NULL,NULL)`,
		build.SourceSHA256, firstScope, build.ID, nowRFC3339())
	require.NoError(t, err)

	sources, err := s.qmdExportSources(t.Context(), 1, maxQMDExportSources, 1)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, versions[1], sources[0].ContentVersionID)

	_, err = s.db.Exec(`INSERT INTO derivative_purge_suppressions(
		source_sha256,profile_fingerprint,build_id,purged_at,active,
		superseded_at,superseding_build_id) VALUES(?,?,?,?,1,NULL,NULL)`,
		build.SourceSHA256, derivativeBuildSuppressionProfile, build.ID, nowRFC3339())
	require.NoError(t, err)
	sources, err = s.qmdExportSources(t.Context(), 1, maxQMDExportSources, 1)
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestQMDExportSourcesPaginatesAndFailsClosedAtCandidateScanBound(t *testing.T) {
	// Mutation caught: offset/truncating pagination can skip stable candidates,
	// while omitting the overflow probe makes an incomplete scan look complete.
	s, versions := newRenditionCatalogFixture(t)
	build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst, build)
	publishQMDStoreSource(t, s, versions[1], catalogAttachmentSecond, build)

	sources, err := s.qmdExportSources(t.Context(), 2, 2, 1)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	_, err = s.qmdExportSources(t.Context(), 2, 1, 1)
	require.ErrorContains(t, err, "scan")
}

func TestQMDExportSourcesUsesReplacementRenditionHead(t *testing.T) {
	// Mutation caught: joining an attachment by version without the exact head
	// would return both old and replacement sanitized artifacts.
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	first := catalogRenditionBuild(s, profile)
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst, first)
	replacement := cloneCatalogBuild(first)
	replacement.ID = catalogBuildReplacement
	replacement.ProviderOperationID = "synthetic-operation-replacement"
	replacement.CapturedArtifactPolicy = []byte(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":0,"role":"sanitized_markdown"}],"version":1}`)
	replacement.CapturedArtifactPolicyFingerprint = testSHA256(replacement.CapturedArtifactPolicy)
	replacement.CompletedAt = "2026-08-22T11:00:00.000000000Z"
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentSecond, replacement)

	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, replacement.ID, sources[0].BuildID)
	assert.Equal(t, catalogAttachmentSecond, sources[0].AttachmentID)
}

func TestQMDExportSourcesExactLimitAndOverflow(t *testing.T) {
	// Mutation caught: LIMIT without a one-row overflow probe silently truncates
	// current membership and publishes an incomplete collection.
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst, build)
	publishQMDStoreSource(t, s, versions[1], catalogAttachmentSecond, build)

	sources, err := s.QMDExportSources(t.Context(), 2)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	_, err = s.QMDExportSources(t.Context(), 1)
	require.ErrorContains(t, err, "exceeds limit")
}

func TestRevalidateQMDExportCandidatesReturnsCurrentLiveIdentity(t *testing.T) {
	// Mutation caught: omitting the live fence or its current path/revision
	// projection would let an exported identity bypass current Store authority.
	s, versions := newRenditionCatalogFixture(t)
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst,
		catalogRenditionBuild(s, catalogProcessingProfile(t, false)))
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	normalized, err := s.NormalizeQMDSearchScope(t.Context(), SearchOptions{MIMEType: " Application/PDF "})
	require.NoError(t, err)
	require.Equal(t, "application/pdf", normalized.MIMEType)
	live, err := s.RevalidateQMDExportCandidates(t.Context(), sources, normalized)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, sources[0].NodeID, live[0].NodeID)
	assert.Equal(t, sources[0].ContentVersionID, live[0].ContentVersionID)
	assert.Equal(t, "/synthetic-source-a.pdf", live[0].Path)
	assert.Positive(t, live[0].NodeRevision)
}

func TestRevalidateQMDExportCandidatesPreservesDistinctSameNodeProfilesInInputOrder(t *testing.T) {
	s, sources, _, _ := newSameNodeQMDProfilesFixture(t)
	reversed := []QMDExportSource{sources[1], sources[0]}
	live, err := s.RevalidateQMDExportCandidates(t.Context(), reversed, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, live, 2)
	for index := range live {
		assert.Equal(t, reversed[index].NodeID, live[index].NodeID)
		assert.Equal(t, reversed[index].ContentVersionID, live[index].ContentVersionID)
		assert.Equal(t, "/synthetic-source-a.pdf", live[index].Path)
		assert.Positive(t, live[index].NodeRevision)
	}
}

func TestRevalidateQMDExportCandidatesUsesExactSourceIdentityForDuplicates(t *testing.T) {
	s, sources, _, _ := newSameNodeQMDProfilesFixture(t)
	_, err := s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{sources[0], sources[0]}, SearchOptions{})
	require.ErrorContains(t, err, "duplicated")
	contradictory := sources[0]
	contradictory.BlobSize++
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{sources[0], contradictory}, SearchOptions{})
	require.ErrorContains(t, err, "duplicated")
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{contradictory}, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
}

func TestRevalidateQMDExportCandidatesSameNodeSiblingDriftFailsWholeBatch(t *testing.T) {
	s, sources, profiles, build := newSameNodeQMDProfilesFixture(t)
	validSibling, err := s.RevalidateQMDExportCandidates(t.Context(), sources[1:], SearchOptions{})
	require.NoError(t, err)
	require.Len(t, validSibling, 1)

	replacement := cloneCatalogBuild(build)
	replacement.ID = catalogBuildReplacement
	replacement.ProviderOperationID = "synthetic-operation-same-node-replacement"
	replacement.CapturedArtifactPolicy = []byte(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":0,"role":"sanitized_markdown"}],"version":1}`)
	replacement.CapturedArtifactPolicyFingerprint = testSHA256(replacement.CapturedArtifactPolicy)
	replacement.CompletedAt = "2026-08-22T12:00:00.000000000Z"
	require.NoError(t, s.StageRenditionBuild(t.Context(), replacement))
	attachment := RenditionAttachmentRecord{ID: fakeHash("59"), VaultID: s.VaultID(),
		ContentVersionID: sources[0].ContentVersionID, BuildID: replacement.ID,
		Profile: profiles[sources[0].ProcessingProfileFingerprint], AttachedAt: nowRFC3339()}
	require.NoError(t, publishAttachmentForTest(t, s, attachment))
	results, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
	require.Nil(t, results)
}

func TestRevalidateQMDExportCandidatesSameNodeExactSuppressionFailsWholeBatch(t *testing.T) {
	s, sources, _, build := newSameNodeQMDProfilesFixture(t)
	suppressed := sources[0]
	scope := derivativeAttachmentSuppressionScope(suppressed.ContentVersionID, suppressed.ProcessingProfileFingerprint)
	_, err := s.db.Exec(`INSERT INTO derivative_purge_suppressions(
		source_sha256,profile_fingerprint,build_id,purged_at,active,
		superseded_at,superseding_build_id) VALUES(?,?,?,?,1,NULL,NULL)`,
		build.SourceSHA256, scope, build.ID, nowRFC3339())
	require.NoError(t, err)
	results, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
	require.Nil(t, results)
	validSibling, err := s.RevalidateQMDExportCandidates(t.Context(), sources[1:], SearchOptions{})
	require.NoError(t, err)
	require.Len(t, validSibling, 1)

	_, err = s.db.Exec(`UPDATE derivative_purge_suppressions SET active=0,
		superseded_at=?,superseding_build_id=? WHERE source_sha256=? AND
		profile_fingerprint=? AND build_id=?`, nowRFC3339(), fakeHash("a8"),
		build.SourceSHA256, scope, build.ID)
	require.NoError(t, err)
	live, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, live, 2)
}

func TestRevalidateQMDExportCandidatesRequiresExactCurrentAttachment(t *testing.T) {
	// Mutation caught: joining only the node/version would accept a relabeled
	// manifest row or an attachment superseded by a new rendition head.
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	first := catalogRenditionBuild(s, profile)
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst, first)
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	drifted := sources[0]
	drifted.AttachmentID = "synthetic-other-attachment"
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{drifted}, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)

	replacement := cloneCatalogBuild(first)
	replacement.ID = catalogBuildReplacement
	replacement.ProviderOperationID = "synthetic-operation-qmd-replacement"
	replacement.CapturedArtifactPolicy = []byte(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":0,"role":"sanitized_markdown"}],"version":1}`)
	replacement.CapturedArtifactPolicyFingerprint = testSHA256(replacement.CapturedArtifactPolicy)
	replacement.CompletedAt = "2026-08-22T12:00:00.000000000Z"
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentSecond, replacement)
	_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
}

func TestRevalidateQMDExportCandidatesRejectsInvalidAndDuplicateAuthority(t *testing.T) {
	// Mutation caught: accepting incomplete, oversized, malformed, or duplicate
	// identities can turn a bounded exact fence into ambiguous partial authority.
	s, sources := newQMDRevalidationFixture(t)

	_, err := s.RevalidateQMDExportCandidates(nil, sources, SearchOptions{}) //nolint:staticcheck // Explicit API contract.
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.RevalidateQMDExportCandidates(ctx, sources, SearchOptions{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.NormalizeQMDSearchScope(nil, SearchOptions{}) //nolint:staticcheck // Explicit API contract.
	require.Error(t, err)
	_, err = s.NormalizeQMDSearchScope(ctx, SearchOptions{})
	require.ErrorIs(t, err, context.Canceled)

	tooMany := make([]QMDExportSource, document.MaxRetrievalCandidateLimit+1)
	for index := range tooMany {
		tooMany[index] = sources[0]
	}
	_, err = s.RevalidateQMDExportCandidates(t.Context(), tooMany, SearchOptions{})
	require.ErrorContains(t, err, "retrieval limit")
	atLimit := tooMany[:document.MaxRetrievalCandidateLimit]
	for index := range atLimit {
		atLimit[index].NodeID += int64(index)
	}
	_, err = s.RevalidateQMDExportCandidates(t.Context(), atLimit, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
	require.NotContains(t, err.Error(), "retrieval limit")
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{sources[0], sources[0]}, SearchOptions{})
	require.ErrorContains(t, err, "duplicated")

	invalid := sources[0]
	invalid.AttachmentID = ""
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{invalid}, SearchOptions{})
	require.ErrorContains(t, err, "invalid")
	invalid = sources[0]
	invalid.AttachmentID = strings.Repeat("x", 1025)
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{invalid}, SearchOptions{})
	require.ErrorContains(t, err, "invalid")
	invalid = sources[0]
	invalid.AttachmentID = string([]byte{0xff})
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{invalid}, SearchOptions{})
	require.ErrorContains(t, err, "invalid")
	invalid = sources[0]
	invalid.BlobSHA256 = strings.Repeat("A", 64)
	invalid.ArtifactChecksum = invalid.BlobSHA256
	invalid.MarkdownChecksum = invalid.BlobSHA256
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{invalid}, SearchOptions{})
	require.ErrorContains(t, err, "invalid")
	invalid = sources[0]
	invalid.BlobSize = -1
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{invalid}, SearchOptions{})
	require.ErrorContains(t, err, "invalid")

	empty, err := s.RevalidateQMDExportCandidates(t.Context(), nil, SearchOptions{})
	require.NoError(t, err)
	require.Empty(t, empty)
	require.NotNil(t, empty)
	_, err = s.RevalidateQMDExportCandidates(t.Context(), nil, SearchOptions{UnderNodeID: -1})
	require.Error(t, err)
}

func TestRevalidateQMDExportCandidatesRejectsEveryDriftedSourceField(t *testing.T) {
	// Mutation caught: dropping any exported scalar from the exact join lets a
	// remote result relabel itself as different local rendition authority.
	s, sources := newQMDRevalidationFixture(t)
	base := sources[0]
	tests := []struct {
		name   string
		mutate func(*QMDExportSource)
	}{
		{name: "vault", mutate: func(value *QMDExportSource) { value.VaultUID = "synthetic-other-vault" }},
		{name: "node", mutate: func(value *QMDExportSource) { value.NodeID++ }},
		{name: "version", mutate: func(value *QMDExportSource) { value.ContentVersionID = "synthetic-other-version" }},
		{name: "profile", mutate: func(value *QMDExportSource) { value.ProcessingProfileFingerprint = fakeHash("other-profile") }},
		{name: "attachment", mutate: func(value *QMDExportSource) { value.AttachmentID = "synthetic-other-attachment" }},
		{name: "build", mutate: func(value *QMDExportSource) { value.BuildID = fakeHash("other-build") }},
		{name: "artifact", mutate: func(value *QMDExportSource) { value.ArtifactID = "artifact_" + fakeHash("other-artifact") }},
		{name: "blob hash", mutate: func(value *QMDExportSource) { value.BlobSHA256 = fakeHash("other-blob") }},
		{name: "blob size", mutate: func(value *QMDExportSource) { value.BlobSize++ }},
		{name: "artifact checksum", mutate: func(value *QMDExportSource) { value.ArtifactChecksum = fakeHash("other-artifact-checksum") }},
		{name: "markdown checksum", mutate: func(value *QMDExportSource) { value.MarkdownChecksum = fakeHash("other-markdown-checksum") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			drifted := base
			test.mutate(&drifted)
			_, err := s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{drifted}, SearchOptions{})
			require.Error(t, err)
		})
	}
}

func TestRevalidateQMDExportCandidatesOwnsSourceSnapshotBeforeTransaction(t *testing.T) {
	// Mutation caught: retaining the caller's slice lets synchronous mutation
	// relabel a candidate or replace source membership while Store waits for SQLite.
	s, sources := newQMDRevalidationFixture(t)
	originalVersion := sources[0].ContentVersionID
	scopeIDs := []string{originalVersion}
	s.db.SetMaxOpenConns(1)
	blocker, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })

	type outcome struct {
		live []QMDExportLiveCandidate
		err  error
	}
	done := make(chan outcome, 1)
	waitCount := s.db.Stats().WaitCount
	go func() {
		live, err := s.RevalidateQMDExportCandidates(t.Context(), sources,
			SearchOptions{ContentVersionIDs: scopeIDs})
		done <- outcome{live: live, err: err}
	}()
	require.Eventually(t, func() bool {
		return s.db.Stats().WaitCount > waitCount
	}, 5*time.Second, 10*time.Millisecond, "revalidation did not reach the transaction boundary")
	sources[0].AttachmentID = "synthetic-caller-mutation"
	scopeIDs[0] = "00000000-0000-4000-8000-000000000099"
	require.NoError(t, blocker.Rollback())

	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.Len(t, result.live, 1)
		require.Equal(t, originalVersion, result.live[0].ContentVersionID)
	case <-time.After(5 * time.Second):
		t.Fatal("revalidation did not finish after releasing SQLite")
	}
}

func TestRevalidateQMDExportCandidatesBoundsSourceScopeBeforeTransaction(t *testing.T) {
	// Mutation caught: relying on transaction-local normalization for the count
	// waits for storage before rejecting an untrusted oversized source slice.
	s, _ := newQMDRevalidationFixture(t)
	s.db.SetMaxOpenConns(1)
	blocker, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })

	ids := make([]string, MaxSearchSourceFenceIDs+1)
	for index := range ids {
		ids[index] = fmt.Sprintf("00000000-0000-4000-8000-%012x", index)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.RevalidateQMDExportCandidates(t.Context(), nil,
			SearchOptions{ContentVersionIDs: ids})
		done <- err
	}()
	select {
	case err := <-done:
		require.EqualError(t, err, "search source fence exceeds 4096 content versions")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("oversized source fence reached the storage transaction")
	}
}

func TestRevalidateQMDExportCandidatesRejectsLifecycleAndScopeDrift(t *testing.T) {
	// Mutation caught: resolving against immutable source rows without current
	// lifecycle and scope predicates would retain revoked or out-of-scope rows.
	for _, test := range []struct {
		name string
		run  func(*testing.T, *Store, []QMDExportSource)
	}{
		{name: "replace content", run: func(t *testing.T, s *Store, sources []QMDExportSource) {
			t.Helper()
			node, err := s.NodeByID(t.Context(), sources[0].NodeID)
			require.NoError(t, err)
			hash := fakeHash("qmd-live-replacement")
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				return s.EnsureBlobTx(tx, hash, 11)
			}))
			_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, hash, 11, "application/pdf")
			require.NoError(t, err)
			_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
			require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
		}},
		{name: "trash", run: func(t *testing.T, s *Store, sources []QMDExportSource) {
			t.Helper()
			node, err := s.NodeByID(t.Context(), sources[0].NodeID)
			require.NoError(t, err)
			_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
			require.NoError(t, err)
			_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
			require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
		}},
		{name: "purge derivatives", run: func(t *testing.T, s *Store, sources []QMDExportSource) {
			t.Helper()
			_, err := s.PurgeDerivatives(t.Context(), PurgeRequest{BuildIDs: []string{sources[0].BuildID}})
			require.NoError(t, err)
			_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
			require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, sources := newQMDRevalidationFixture(t)
			test.run(t, s, sources)
		})
	}

	t.Run("tag removal", func(t *testing.T) {
		s, sources := newQMDRevalidationFixture(t)
		tag, err := s.CreateTag(t.Context(), "qmd-selected")
		require.NoError(t, err)
		node, err := s.NodeByID(t.Context(), sources[0].NodeID)
		require.NoError(t, err)
		_, err = s.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
		node, err = s.NodeByID(t.Context(), node.ID)
		require.NoError(t, err)
		_, err = s.UnassignTag(t.Context(), tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
		_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{TagID: tag.ID})
		require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
	})

	t.Run("move outside directory", func(t *testing.T) {
		s, sources := newQMDRevalidationFixture(t)
		directory, err := s.Mkdir(t.Context(), s.RootID(), "qmd-scope")
		require.NoError(t, err)
		node, err := s.NodeByID(t.Context(), sources[0].NodeID)
		require.NoError(t, err)
		node, _, err = s.Move(t.Context(), node.ID, directory.ID, node.Name, node.Revision)
		require.NoError(t, err)
		live, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{UnderNodeID: directory.ID})
		require.NoError(t, err)
		require.Len(t, live, 1)
		_, _, err = s.Move(t.Context(), node.ID, s.RootID(), node.Name, node.Revision)
		require.NoError(t, err)
		_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{UnderNodeID: directory.ID})
		require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
	})
}

func TestRevalidateQMDExportCandidatesReturnsCurrentInScopeRename(t *testing.T) {
	// Mutation caught: binding authority to the unpublished snapshot path would
	// reject a still-in-scope rename instead of returning its current path.
	s, sources := newQMDRevalidationFixture(t)
	directory, err := s.Mkdir(t.Context(), s.RootID(), "qmd-scope")
	require.NoError(t, err)
	node, err := s.NodeByID(t.Context(), sources[0].NodeID)
	require.NoError(t, err)
	node, _, err = s.Move(t.Context(), node.ID, directory.ID, node.Name, node.Revision)
	require.NoError(t, err)
	before, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{UnderNodeID: directory.ID})
	require.NoError(t, err)
	require.Len(t, before, 1)

	renamed, path, err := s.Move(t.Context(), node.ID, directory.ID, "renamed.pdf", node.Revision)
	require.NoError(t, err)
	live, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{UnderNodeID: directory.ID})
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, path, live[0].Path)
	assert.Equal(t, "/qmd-scope/renamed.pdf", live[0].Path)
	assert.Equal(t, renamed.Revision, live[0].NodeRevision)
	assert.Greater(t, live[0].NodeRevision, before[0].NodeRevision)
}

func TestRevalidateQMDExportCandidatesAppliesMIMEAndTimeScope(t *testing.T) {
	// Mutation caught: bypassing shared scalar scope filters would return exact
	// rendition authority that is nevertheless outside the operator selection.
	s, sources := newQMDRevalidationFixture(t)
	node, err := s.NodeByID(t.Context(), sources[0].NodeID)
	require.NoError(t, err)

	live, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{
		MIMEType: "APPLICATION/PDF", ModifiedSince: node.ModifiedAt,
	})
	require.NoError(t, err)
	require.Len(t, live, 1)
	_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{MIMEType: "text/plain"})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
	_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{ModifiedBefore: node.ModifiedAt})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
}

func TestRevalidateQMDExportCandidatesRejectsExactActiveSuppression(t *testing.T) {
	// Mutation caught: exact joins alone still expose a retained current head
	// after durable attachment-scoped or build-wide derivative suppression.
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst, build)
	publishQMDStoreSource(t, s, versions[1], catalogAttachmentSecond, build)
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 2)

	firstScope := derivativeAttachmentSuppressionScope(versions[0], profile.Fingerprint)
	_, err = s.db.Exec(`INSERT INTO derivative_purge_suppressions(
		source_sha256,profile_fingerprint,build_id,purged_at,active,
		superseded_at,superseding_build_id) VALUES(?,?,?,?,1,NULL,NULL)`,
		build.SourceSHA256, firstScope, build.ID, nowRFC3339())
	require.NoError(t, err)
	var retained int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads h
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		JOIN rendition_artifacts artifact ON artifact.build_id=a.build_id
		WHERE h.attachment_id=? AND artifact.artifact_id=?`,
		sources[0].AttachmentID, sources[0].ArtifactID).Scan(&retained))
	require.Equal(t, 1, retained, "suppression control must retain otherwise-valid joined authority")
	_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
	sibling, err := s.RevalidateQMDExportCandidates(t.Context(), sources[1:], SearchOptions{})
	require.NoError(t, err)
	require.Len(t, sibling, 1)

	_, err = s.db.Exec(`UPDATE derivative_purge_suppressions SET active=0,
		superseded_at=?,superseding_build_id=? WHERE source_sha256=? AND
		profile_fingerprint=? AND build_id=?`, nowRFC3339(), fakeHash("qmd-superseding"),
		build.SourceSHA256, firstScope, build.ID)
	require.NoError(t, err)
	live, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, live, 2)

	_, err = s.db.Exec(`INSERT INTO derivative_purge_suppressions(
		source_sha256,profile_fingerprint,build_id,purged_at,active,
		superseded_at,superseding_build_id) VALUES(?,?,?,?,1,NULL,NULL)`,
		build.SourceSHA256, derivativeBuildSuppressionProfile, build.ID, nowRFC3339())
	require.NoError(t, err)
	_, err = s.RevalidateQMDExportCandidates(t.Context(), sources[1:], SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
}

func newQMDRevalidationFixture(t *testing.T) (*Store, []QMDExportSource) {
	t.Helper()
	s, versions := newRenditionCatalogFixture(t)
	publishQMDStoreSource(t, s, versions[0], catalogAttachmentFirst,
		catalogRenditionBuild(s, catalogProcessingProfile(t, false)))
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	return s, sources
}

func newSameNodeQMDProfilesFixture(
	t *testing.T,
) (*Store, []QMDExportSource, map[string]ProcessingProfileRecord, RenditionBuildRecord) {
	t.Helper()
	s, versions := newRenditionCatalogFixture(t)
	first := catalogProcessingProfile(t, false)
	second := catalogProcessingProfileWith(t, false, func(profile *document.ProcessingProfileV1) {
		profile.Retrieval.LexicalLimit = 99
	})
	require.NotEqual(t, first.Fingerprint, second.Fingerprint)
	require.Equal(t, first.RenditionRequestFingerprint, second.RenditionRequestFingerprint)
	require.Equal(t, first.EvidenceLexicalFingerprint, second.EvidenceLexicalFingerprint)
	build := catalogRenditionBuild(s, first)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	for _, attachment := range []RenditionAttachmentRecord{
		{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: first, AttachedAt: nowRFC3339()},
		{ID: catalogAttachmentSecond, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: second, AttachedAt: nowRFC3339()},
	} {
		require.NoError(t, publishAttachmentForTest(t, s, attachment))
	}
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	require.Equal(t, sources[0].NodeID, sources[1].NodeID)
	require.NotEqual(t, sources[0].ProcessingProfileFingerprint, sources[1].ProcessingProfileFingerprint)
	require.NotEqual(t, sources[0].AttachmentID, sources[1].AttachmentID)
	profiles := map[string]ProcessingProfileRecord{first.Fingerprint: first, second.Fingerprint: second}
	return s, sources, profiles, build
}

func publishQMDStoreSource(
	t *testing.T, s *Store, versionID, attachmentID string, build RenditionBuildRecord,
) {
	t.Helper()
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: attachmentID, VaultID: s.VaultID(), ContentVersionID: versionID,
		BuildID: build.ID, Profile: catalogProcessingProfile(t, false), AttachedAt: nowRFC3339(),
	}
	require.NoError(t, publishAttachmentForTest(t, s, attachment))
}
