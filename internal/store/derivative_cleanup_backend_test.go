package store

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"uuid"
)

func TestDerivativeCleanupPostgres(t *testing.T) {
	t.Parallel()
	t.Run("complete manifest", func(t *testing.T) {
		t.Parallel()
		checkPurgeDerivativesRemovesCompleteLiveManifestButNeverOriginal(t, true)
	})
	t.Run("worker fencing", func(t *testing.T) {
		t.Parallel()
		checkPurgeDerivativesRetainsActiveWorkerThenCollectsExpiredFence(t, true)
	})
}

func TestDerivativeSharedLegacyPostgres(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	const text = "shared retained text"
	hash := fakeHash("1c")
	first, err := source.CreateFile(ctx, source.RootID(), "first.txt", hash, int64(len(text)), "text/plain")
	require.NoError(t, err)
	second, err := source.CreateFile(ctx, source.RootID(), "second.txt", hash, int64(len(text)), "text/plain")
	require.NoError(t, err)
	require.NoError(t, source.RecordExtraction(ctx, ExtractionResult{BlobHash: hash, Extractor: legacyPlainTextExtractor, ExtractorVersion: legacyPlainTextExtractorVersion, Status: ExtractionOK, Text: text}))
	target, _ := copyRetentionStore(t, source)
	for _, s := range []*Store{source, target} {
		report, err := s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{first.CurrentVersionID}})
		require.NoError(t, err)
		require.Zero(t, report.RemovedLegacyCacheRows)
		var version string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT version_id FROM text_searchable_versions`).Scan(&version))
		require.Equal(t, second.CurrentVersionID, version)
		report, err = s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{second.CurrentVersionID}})
		require.NoError(t, err)
		require.Equal(t, 1, report.RemovedLegacyCacheRows)
		var cache, eligibility, originals int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM extracted_text),(SELECT COUNT(*) FROM text_searchable_versions),(SELECT COUNT(*) FROM content_versions)`).Scan(&cache, &eligibility, &originals))
		require.Equal(t, []int{0, 0, 2}, []int{cache, eligibility, originals})
		checkRetentionMetadata(t, s)
	}
}

func TestDerivativeEmbeddingRootsPostgres(t *testing.T) {
	t.Parallel()
	source, version, profile, attachment := newEmbeddingCatalogFixture(t)
	ctx := t.Context()
	chunk := embeddingSetFixture(source, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	direct := embeddingSetFixture(source, version, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	for _, record := range []EmbeddingSetRecord{chunk, direct} {
		require.NoError(t, source.StageEmbeddingSet(ctx, record))
		require.NoError(t, source.PublishEmbeddingHead(ctx, EmbeddingHeadRecord{FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: record.BindingID, InputKind: record.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
	}
	root := CurrentRenditionRoot{ID: "reader", Kind: RenditionRootReaderLease, TargetKind: RenditionRootEmbeddingSet, TargetID: chunk.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime, ExpiresAt: "2099-08-25T10:00:00.000000000Z"}
	require.NoError(t, source.PutCurrentRenditionRoot(ctx, root))
	target, _ := copyRetentionStore(t, source)
	before, err := source.DerivativePurgeFingerprint(ctx, PurgeRequest{All: true})
	require.NoError(t, err)
	for _, s := range []*Store{source, target} {
		fingerprint, err := s.DerivativePurgeFingerprint(ctx, PurgeRequest{All: true})
		require.NoError(t, err)
		require.Equal(t, before, fingerprint)
		report, err := s.PurgeDerivatives(ctx, PurgeRequest{AttachmentIDs: []string{attachment}})
		require.NoError(t, err)
		require.Equal(t, 1, report.RemovedEmbeddingHeads)
		require.Zero(t, report.RemovedEmbeddingSets)
		require.Zero(t, report.RemovedAttachments)
		var sets, heads int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM embedding_sets),(SELECT COUNT(*) FROM embedding_heads)`).Scan(&sets, &heads))
		require.Equal(t, []int{2, 1}, []int{sets, heads})
		checkRetentionMetadata(t, s)
		released, err := s.ReleaseCurrentRenditionRoot(ctx, root.ID, root.FencingToken)
		require.NoError(t, err)
		require.True(t, released)
		plan, err := s.DerivativeGCPlan(ctx)
		require.NoError(t, err)
		require.Len(t, plan.EmbeddingSets, 1)
		require.Equal(t, chunk.ID, plan.EmbeddingSets[0].SetID)
		report, err = s.PurgeDerivatives(ctx, PurgeRequest{AttachmentIDs: []string{attachment}})
		require.NoError(t, err)
		require.Equal(t, 1, report.RemovedEmbeddingSets)
		require.Equal(t, 1, report.RemovedEmbeddingInputGenerations)
		require.Equal(t, 1, report.RemovedEmbeddingVectorSets)
		require.Contains(t, report.PhysicalDerivativeBlobsPendingGC, chunk.VectorSet.PayloadBlobHash)
		require.NotContains(t, report.PhysicalDerivativeBlobsPendingGC, direct.VectorSet.PayloadBlobHash)
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM embedding_sets),(SELECT COUNT(*) FROM embedding_heads)`).Scan(&sets, &heads))
		require.Equal(t, []int{1, 1}, []int{sets, heads})
		require.NoError(t, s.AuthorizeEmbeddingRebuild(ctx, EmbeddingRebuildAuthorization{
			Key:                          EmbeddingHeadKey{ContentVersionID: version, BindingID: chunk.BindingID, InputKind: chunk.InputKind},
			ProcessingProfileFingerprint: profile.Fingerprint, SetID: fakeHash("f3"), AuthorizedAt: "2099-08-25T10:00:00.000000000Z",
		}))
		var authorized int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM derivative_purge_suppressions WHERE active=0 AND superseding_build_id=$1`, fakeHash("f3")).Scan(&authorized))
		require.Equal(t, 1, authorized)
		checkRetentionMetadata(t, s)
	}
}

func TestDerivativeLexicalReplacementPostgres(t *testing.T) {
	t.Parallel()
	source, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	first := lexicalSearchBuild(source, profile, catalogBuildID, "removed text")
	require.NoError(t, source.StageRenditionBuild(ctx, first))
	initial, err := source.StageLexicalGeneration(ctx, fakeHash("bd"))
	require.NoError(t, err)
	require.NoError(t, source.PublishRenditionAndLexicalHeads(ctx, RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: source.VaultID(), ContentVersionID: versions[0], BuildID: first.ID, Profile: profile, AttachedAt: "2026-08-23T13:20:00.000000000Z"}, RenditionHeadRecord{ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint, AttachmentID: catalogAttachmentFirst, PublishedAt: "2026-08-23T13:21:00.000000000Z"}, initial.ID))
	// This staged build has no index rows yet. Replacement must index it as well as preserve its manifest.
	second, _ := lexicalSearchReplacementBuild(t, source, profile, fakeHash("bc"), "retained text")
	require.NoError(t, source.StageRenditionBuild(ctx, second))
	target, _ := copyRetentionStore(t, source)
	request := PurgeRequest{BuildIDs: []string{first.ID}}
	before, err := source.DerivativePurgeFingerprint(ctx, request)
	require.NoError(t, err)
	for _, s := range []*Store{source, target} {
		fingerprint, err := s.DerivativePurgeFingerprint(ctx, request)
		require.NoError(t, err)
		require.Equal(t, before, fingerprint)
		report, err := s.PurgeDerivatives(ctx, PurgeRequest{BuildIDs: []string{first.ID}})
		require.NoError(t, err)
		require.Equal(t, 1, report.RemovedBuilds)
		active, err := s.ActiveLexicalGeneration(ctx)
		require.NoError(t, err)
		require.NotEqual(t, initial.ID, active.ID)
		require.Equal(t, 1, active.BuildCount)
		var build, text string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT gb.build_id,i.text FROM rendition_lexical_generation_builds gb JOIN rendition_lexical_index i ON i.build_id=gb.build_id WHERE gb.generation_id=$1`, active.ID).Scan(&build, &text))
		require.Equal(t, second.ID, build)
		require.Equal(t, "retained text", text)
		after, err := s.DerivativePurgeFingerprint(ctx, request)
		require.NoError(t, err)
		require.NotEqual(t, before, after)
		checkRetentionMetadata(t, s)
	}
}

func TestDerivativeAuditRollbackPostgres(t *testing.T) {
	t.Parallel()
	source, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(source, profile)
	require.NoError(t, source.StageRenditionBuild(ctx, build))
	require.NoError(t, publishAttachmentForTest(t, source, RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: source.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: build.CompletedAt}))
	seedInitialAuditAuthority(t, source, source.RootID())
	s, setup := copyRetentionStore(t, source)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	quotedRole := pgx.Identifier{role}.Sanitize()
	before, err := s.AuditStatus(ctx, nil)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, `REVOKE INSERT ON documents.derivative_blob_purge_pending FROM `+quotedRole)
	require.NoError(t, err)
	report, err := s.PurgeDerivatives(ctx, PurgeRequest{All: true})
	require.ErrorContains(t, err, "persisting derivative blob purge target")
	require.Empty(t, report)
	var heads, builds, suppressions int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM rendition_heads),(SELECT COUNT(*) FROM rendition_builds),(SELECT COUNT(*) FROM derivative_purge_suppressions)`).Scan(&heads, &builds, &suppressions))
	require.Equal(t, []int{1, 1, 0}, []int{heads, builds, suppressions})
	after, err := s.AuditStatus(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = setup.ExecContext(ctx, `GRANT INSERT ON documents.derivative_blob_purge_pending TO `+quotedRole)
	require.NoError(t, err)
	report, err = s.PurgeDerivatives(ctx, PurgeRequest{All: true})
	require.NoError(t, err)
	require.Equal(t, 1, report.RemovedBuilds)
	checkRetentionMetadata(t, s)
	after, err = s.AuditStatus(ctx, nil)
	require.NoError(t, err)
	require.NotEqual(t, before, after)
	_, err = s.PurgeDerivatives(ctx, PurgeRequest{All: true})
	require.NoError(t, err)
	replay, err := s.AuditStatus(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, after, replay)
	authorization := DerivativeRebuildAuthorization{SourceSHA256: build.SourceSHA256, PurgedBuildID: build.ID, SupersedingBuildID: fakeHash("8e"), AuthorizedAt: "2026-08-23T16:20:00.000000000Z"}
	require.NoError(t, s.AuthorizeDerivativeRebuild(ctx, authorization))
	require.NoError(t, s.AuthorizeDerivativeRebuild(ctx, authorization))
	checkRetentionMetadata(t, s)
}

func TestDerivativeEmailRetentionPostgres(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	retained := newEmailFixture(t, source, "retained.eml")
	view, err := source.PublishEmailGeneration(ctx, retained.publication)
	require.NoError(t, err)
	receipt, err := source.PublishEmailDocuments(ctx, attachmentRequest(t, source, view, "retain-inventory"))
	require.NoError(t, err)
	other := newEmailSourceFixture(t, source, "other.eml", "Subject: Other\r\n\r\nOther body")
	otherView, err := source.PublishEmailGeneration(ctx, other.publication)
	require.NoError(t, err)
	pending, err := source.CreateFile(ctx, source.RootID(), "pending.eml", otherView.Version.BlobHash, otherView.Version.Size, "message/rfc822")
	require.NoError(t, err)
	target, _ := copyRetentionStore(t, source)
	for _, s := range []*Store{source, target} {
		_, err := s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{view.Version.ID}})
		require.ErrorIs(t, err, ErrEmailDocumentConflict)
		require.ErrorContains(t, err, receipt.OperationID)
		report, err := s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{pending.CurrentVersionID}})
		require.NoError(t, err)
		require.Zero(t, report.RemovedEmailGenerations, "a pending sibling must not remove the shared inventory")
		report, err = s.PurgeDerivatives(ctx, PurgeRequest{All: true})
		require.NoError(t, err)
		require.Equal(t, 1, report.RemovedEmailAttachments)
		require.Equal(t, 1, report.RemovedEmailGenerations)
		require.Equal(t, len(other.publication.Artifacts), report.RemovedEmailPartArtifacts)
		var retainedGeneration string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT generation_id FROM email_generations`).Scan(&retainedGeneration))
		require.Equal(t, view.Generation.ID, retainedGeneration)
		for _, version := range []ContentVersion{view.Version, otherView.Version, {ID: pending.CurrentVersionID, BlobHash: pending.BlobHash}} {
			suppressed, err := emailInventorySuppressed(ctx, s.db, version)
			require.NoError(t, err)
			require.Equal(t, version.ID != view.Version.ID, suppressed)
			_, err = s.ContentVersionByID(ctx, version.ID)
			require.NoError(t, err, "purging derived email data preserves the original")
		}
		checkRetentionMetadata(t, s)
	}
}

func TestDerivativeExportRetentionPostgres(t *testing.T) {
	t.Parallel()
	source, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(source, profile)
	require.NoError(t, source.StageRenditionBuild(ctx, build))
	require.NoError(t, publishRenditionForTest(t, source, RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: source.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339()}, nowRFC3339(), fakeHash("d4")))
	node, err := source.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	frozen, err := source.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "nodes", NodeIDs: []int64{node.ID}}, nil)
	require.NoError(t, err)
	_, err = source.CreateExportPlan(ctx, "owner", bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: frozen.ID, MemberHash: frozen.MemberHash, Roles: []bundle.RolePolicy{{Role: "text"}}})
	require.NoError(t, err)
	s, _ := copyRetentionStore(t, source)
	for _, request := range []PurgeRequest{{All: true}, {ContentVersionIDs: []string{versions[0]}}, {AttachmentIDs: []string{catalogAttachmentFirst}}, {BuildIDs: []string{build.ID}}} {
		report, err := s.PurgeDerivatives(ctx, request)
		require.ErrorIs(t, err, bundle.ErrRetained)
		require.Empty(t, report)
	}
	var heads, builds int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM rendition_heads),(SELECT COUNT(*) FROM rendition_builds)`).Scan(&heads, &builds))
	require.Equal(t, []int{1, 1}, []int{heads, builds})
	checkRetentionMetadata(t, s)
}

func TestDerivativePlanSnapshotPostgres(t *testing.T) {
	t.Parallel()
	source, _ := newRenditionCatalogFixture(t)
	ctx := t.Context()
	build := catalogRenditionBuild(source, catalogProcessingProfile(t, false))
	require.NoError(t, source.StageRenditionBuild(ctx, build))
	generation, err := source.StageLexicalGeneration(ctx, fakeHash("e3"))
	require.NoError(t, err)
	root := CurrentRenditionRoot{ID: "snapshot-reader", Kind: RenditionRootReaderLease, TargetKind: RenditionRootLexicalGeneration, TargetID: generation.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime, ExpiresAt: "2099-08-25T10:00:00.000000000Z"}
	require.NoError(t, source.PutCurrentRenditionRoot(ctx, root))
	s, _ := copyRetentionStore(t, source)
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	var config *pgx.ConnConfig
	err = conn.Raw(func(c any) error {
		pgConn, ok := c.(*stdlib.Conn)
		require.True(t, ok)
		config = pgConn.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	// Commit a real root release between the plan's build and generation reads.
	var released bool
	var releaseErr error
	config.Tracer = &afterFirstSelect{run: func() {
		released, releaseErr = s.ReleaseCurrentRenditionRoot(ctx, root.ID, root.FencingToken)
	}}
	readDB := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, readDB.Close()) })
	reader := &Store{db: readDB, postgresSchema: s.postgresSchema}
	plan, err := reader.DerivativeGCPlan(ctx)
	require.NoError(t, err)
	require.NoError(t, releaseErr)
	require.True(t, released)
	require.Empty(t, plan.Builds)
	require.Empty(t, plan.LexicalGenerations)
	plan, err = s.DerivativeGCPlan(ctx)
	require.NoError(t, err)
	require.Len(t, plan.Builds, 1)
	require.Equal(t, build.ID, plan.Builds[0].BuildID)
	require.Equal(t, []string{generation.ID}, plan.LexicalGenerations)
}
