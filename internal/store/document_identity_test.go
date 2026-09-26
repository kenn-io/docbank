package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestDocumentIdentityIsStableAcrossMoveReplaceAndPathReuse(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(t.Context(), node.ID)
	require.NoError(t, err)
	require.NoError(t, validateUUIDv4(identity.DocumentUID))

	moved, _, err := s.MoveToPath(t.Context(), node.ID, node.Revision, "/renamed.pdf")
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(t.Context(), moved.ID, moved.Revision,
		testSHA256([]byte("replacement")), 11, "application/pdf")
	require.NoError(t, err)
	after, err := s.EnsureDocumentIdentity(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, identity, after)

	run, err := s.BeginIngest(t.Context(), "test", "path reuse")
	require.NoError(t, err)
	reused, err := s.IngestFileExact(t.Context(), run, s.RootID(), "synthetic-source-a.pdf",
		testSHA256([]byte("reused")), 6, "application/pdf", "synthetic-source-a.pdf", "")
	require.NoError(t, err)
	reusedIdentity, err := s.EnsureDocumentIdentity(t.Context(), reused.ID)
	require.NoError(t, err)
	assert.NotEqual(t, identity.DocumentUID, reusedIdentity.DocumentUID)
	assert.NotEmpty(t, versions)
}

func TestDocumentIdentityByNodeDoesNotAllocateOnRead(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	_, err = s.DocumentIdentityByNode(t.Context(), node.ID)
	require.ErrorIs(t, err, ErrDocumentIdentityUnavailable)
	created, err := s.EnsureDocumentIdentity(t.Context(), node.ID)
	require.NoError(t, err)
	read, err := s.DocumentIdentityByNode(t.Context(), node.ID)
	require.NoError(t, err)
	require.Equal(t, created, read)
}

func TestResolvePassageAuthorityUsesHistoricalTupleAndAliases(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("passage-generation"))))
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(t.Context(), node.ID)
	require.NoError(t, err)
	ref := document.PassageRefV1{Version: 1, VaultUID: s.VaultID(), DocumentUID: identity.DocumentUID,
		ContentVersionID: versions[0], SourceSHA256: build.SourceSHA256,
		RenditionBuildID: build.ID, AttachmentID: attachment.ID,
		BodySHA256: testSHA256([]byte("body")), ByteStart: 0, ByteEnd: 1,
		QuoteSHA256: testSHA256([]byte("b"))}

	resolved, err := s.ResolvePassageAuthority(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, versions[0], resolved.Version.ID)
	assert.Equal(t, build.ID, resolved.Build.ID)
	assert.Equal(t, attachment.ID, resolved.Attachment.ID)
	assert.True(t, resolved.Fresh)

	// A changed current version must not retarget the historical tuple.
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision,
		testSHA256([]byte("new current")), 11, "application/pdf")
	require.NoError(t, err)
	resolved, err = s.ResolvePassageAuthority(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, versions[0], resolved.Version.ID)
	assert.False(t, resolved.Fresh)

	domain := "99999999-9999-4999-8999-999999999999"
	sourceVault := "88888888-8888-4888-8888-888888888888"
	sourceDocument := "77777777-7777-4777-8777-777777777777"
	require.NoError(t, s.PutDocumentIdentityAlias(t.Context(), domain, sourceVault,
		sourceDocument, identity.DocumentUID))
	aliased := ref
	aliased.FederationDomainUID, aliased.VaultUID, aliased.DocumentUID = domain, sourceVault, sourceDocument
	_, err = s.ResolvePassageAuthority(t.Context(), aliased)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable,
		"document alias alone must not authorize a foreign rendition tuple")
	require.NoError(t, s.PutAdoptedPassageAuthority(t.Context(), domain, aliased,
		identity.DocumentUID, versions[0], build.ID, attachment.ID))
	resolved, err = s.ResolvePassageAuthority(t.Context(), aliased)
	require.NoError(t, err)
	assert.Equal(t, identity, resolved.Identity,
		"an adopted source identity must resolve to the exact local document identity")

	for name, mutate := range map[string]func(*document.PassageRefV1){
		"vault": func(value *document.PassageRefV1) {
			value.VaultUID = "66666666-6666-4666-8666-666666666666"
		},
		"document": func(value *document.PassageRefV1) {
			value.DocumentUID = "55555555-5555-4555-8555-555555555555"
		},
		"content version": func(value *document.PassageRefV1) {
			value.ContentVersionID = "44444444-4444-4444-8444-444444444444"
		},
		"source": func(value *document.PassageRefV1) {
			value.SourceSHA256 = testSHA256([]byte("wrong source"))
		},
		"attachment": func(value *document.PassageRefV1) {
			value.AttachmentID = testSHA256([]byte("wrong attachment"))
		},
		"build": func(value *document.PassageRefV1) {
			value.RenditionBuildID = testSHA256([]byte("wrong build"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := ref
			mutate(&changed)
			_, err := s.ResolvePassageAuthority(t.Context(), changed)
			require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)
		})
	}

	trashed, _, err := s.Trash(t.Context(), node.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.ResolvePassageAuthority(t.Context(), ref)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable,
		"revoked source visibility must deny historical passage resolution")
	_, _, err = s.Restore(t.Context(), trashed.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.db.Exec(`DELETE FROM rendition_attachments WHERE attachment_id=?`, attachment.ID)
	require.NoError(t, err)
	_, err = s.ResolvePassageAuthority(t.Context(), ref)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable,
		"pruned historical rendition authority must not fall back to the current head")
}

func TestAdoptedStandalonePassageResolvesOnlyExactLocalTuple(t *testing.T) {
	source, sourceVersions := newRenditionCatalogFixture(t)
	target, targetVersions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	sourceBuild := catalogRenditionBuild(source, profile)
	targetBuild := catalogRenditionBuild(target, profile)
	targetBuild.ID = catalogBuildReplacement
	require.NoError(t, target.StageRenditionBuild(t.Context(), targetBuild))
	targetAttachment := RenditionAttachmentRecord{ID: catalogAttachmentSecond, VaultID: target.VaultID(),
		ContentVersionID: targetVersions[0], BuildID: targetBuild.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, target, targetAttachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("adopted-generation"))))
	sourceNode, err := source.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	sourceIdentity, err := source.EnsureDocumentIdentity(t.Context(), sourceNode.ID)
	require.NoError(t, err)
	targetNode, err := target.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	targetIdentity, err := target.EnsureDocumentIdentity(t.Context(), targetNode.ID)
	require.NoError(t, err)
	ref := document.PassageRefV1{Version: 1, VaultUID: source.VaultID(), DocumentUID: sourceIdentity.DocumentUID,
		ContentVersionID: sourceVersions[0], SourceSHA256: sourceBuild.SourceSHA256,
		RenditionBuildID: sourceBuild.ID, AttachmentID: catalogAttachmentFirst,
		BodySHA256: testSHA256([]byte("body")), ByteStart: 0, ByteEnd: 1, QuoteSHA256: testSHA256([]byte("b"))}
	require.NotEqual(t, source.VaultID(), target.VaultID())
	require.NotEqual(t, sourceVersions[0], targetVersions[0])
	require.NotEqual(t, ref.RenditionBuildID, targetBuild.ID)
	require.NotEqual(t, ref.AttachmentID, targetAttachment.ID)
	_, err = target.ResolvePassageAuthority(t.Context(), ref)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)
	domain := "99999999-9999-4999-8999-999999999999"
	require.NoError(t, target.PutDocumentIdentityAlias(t.Context(), domain, ref.VaultUID, ref.DocumentUID,
		targetIdentity.DocumentUID))
	_, err = target.ResolvePassageAuthority(t.Context(), ref)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable, "document adoption alone cannot authorize a passage")
	require.NoError(t, target.PutAdoptedPassageAuthority(t.Context(), domain, ref, targetIdentity.DocumentUID,
		targetVersions[0], targetBuild.ID, targetAttachment.ID))
	resolved, err := target.ResolvePassageAuthority(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, targetVersions[0], resolved.Version.ID)
	assert.Equal(t, targetBuild.ID, resolved.Build.ID)
	assert.Equal(t, targetAttachment.ID, resolved.Attachment.ID)
	assert.True(t, resolved.Fresh)
	var exported bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	restoredPassage, err := restored.ResolvePassageAuthority(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, targetBuild.ID, restoredPassage.Build.ID)
	var reexported bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &reexported))
	assert.Equal(t, exported.Bytes(), reexported.Bytes())
	restoredNode, err := restored.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	_, _, err = restored.Trash(t.Context(), restoredNode.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = restored.ResolvePassageAuthority(t.Context(), ref)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable, "trashed local source must revoke passage access")
	_, _, err = restored.Restore(t.Context(), restoredNode.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = restored.db.Exec(`DELETE FROM rendition_attachments WHERE attachment_id=?`, targetAttachment.ID)
	require.NoError(t, err)
	_, err = restored.ResolvePassageAuthority(t.Context(), ref)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable, "pruned local attachment must revoke passage access")

	require.Error(t, target.PutAdoptedPassageAuthority(t.Context(), domain, ref,
		targetIdentity.DocumentUID, targetVersions[1], targetBuild.ID, targetAttachment.ID),
		"the local version must belong to the adopted document")
	otherNode, err := target.NodeByPath(t.Context(), "/synthetic-source-b.pdf")
	require.NoError(t, err)
	otherIdentity, err := target.EnsureDocumentIdentity(t.Context(), otherNode.ID)
	require.NoError(t, err)
	require.Error(t, target.PutAdoptedPassageAuthority(t.Context(), domain, ref,
		otherIdentity.DocumentUID, targetVersions[0], targetBuild.ID, targetAttachment.ID),
		"the local document must match the installed document alias")
	wrongSource := ref
	wrongSource.SourceSHA256 = testSHA256([]byte("wrong origin source"))
	require.Error(t, target.PutAdoptedPassageAuthority(t.Context(), domain, wrongSource,
		targetIdentity.DocumentUID, targetVersions[0], targetBuild.ID, targetAttachment.ID),
		"the origin source hash must match the local retained source")
	require.Error(t, target.PutAdoptedPassageAuthority(t.Context(), domain, ref,
		targetIdentity.DocumentUID, targetVersions[0], catalogBuildID, targetAttachment.ID),
		"the local build must belong to the local attachment")
	require.Error(t, target.PutAdoptedPassageAuthority(t.Context(), domain, ref,
		targetIdentity.DocumentUID, targetVersions[0], targetBuild.ID, catalogAttachmentFirst),
		"the local attachment must exist")

	alternateBuild := catalogRenditionBuild(target, profile)
	require.NoError(t, target.StageRenditionBuild(t.Context(), alternateBuild))
	alternateAttachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: target.VaultID(),
		ContentVersionID: targetVersions[0], BuildID: alternateBuild.ID, Profile: profile,
		AttachedAt: "2026-08-22T11:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, target, alternateAttachment,
		"2026-08-22T11:01:00.000000000Z", testSHA256([]byte("alternate-generation"))))
	require.ErrorIs(t, target.PutAdoptedPassageAuthority(t.Context(), domain, ref,
		targetIdentity.DocumentUID, targetVersions[0], alternateBuild.ID, alternateAttachment.ID),
		ErrPassageAuthorityUnavailable, "an installed origin tuple cannot be retargeted")
	wrong := ref
	wrong.ContentVersionID = "44444444-4444-4444-8444-444444444444"
	_, err = target.ResolvePassageAuthority(t.Context(), wrong)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)
	wrong = ref
	wrong.RenditionBuildID = testSHA256([]byte("wrong origin build"))
	_, err = target.ResolvePassageAuthority(t.Context(), wrong)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)
	wrong = ref
	wrong.AttachmentID = testSHA256([]byte("wrong origin attachment"))
	_, err = target.ResolvePassageAuthority(t.Context(), wrong)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)

	secondDomain := "66666666-6666-4666-8666-666666666666"
	require.NoError(t, target.PutDocumentIdentityAlias(t.Context(), secondDomain,
		ref.VaultUID, ref.DocumentUID, targetIdentity.DocumentUID))
	require.NoError(t, target.PutAdoptedPassageAuthority(t.Context(), secondDomain, ref,
		targetIdentity.DocumentUID, targetVersions[0], targetBuild.ID, targetAttachment.ID))
	_, err = target.ResolvePassageAuthority(t.Context(), ref)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable,
		"an unchanged domainless ref cannot guess between two enrolled aliases")
	domainRef := ref
	domainRef.FederationDomainUID = domain
	_, err = target.ResolvePassageAuthority(t.Context(), domainRef)
	require.NoError(t, err, "an explicit domain still selects its exact adopted tuple")
}
