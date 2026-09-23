package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestAdoptedPassageTagPreservesAndValidatesLocalAuthority(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("adopted-passage-generation"))))
	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	domain := "99999999-9999-4999-8999-999999999999"
	ref := document.PassageRefV1{Version: 1, FederationDomainUID: domain,
		VaultUID:         "88888888-8888-4888-8888-888888888888",
		DocumentUID:      "77777777-7777-4777-8777-777777777777",
		ContentVersionID: "66666666-6666-4666-8666-666666666666",
		SourceSHA256:     build.SourceSHA256,
		RenditionBuildID: testSHA256([]byte("foreign build")),
		AttachmentID:     testSHA256([]byte("foreign attachment")),
		BodySHA256:       testSHA256([]byte("body")), ByteStart: 0, ByteEnd: 1,
		QuoteSHA256: testSHA256([]byte("b"))}
	require.NoError(t, s.PutDocumentIdentityAlias(ctx, domain, ref.VaultUID, ref.DocumentUID,
		identity.DocumentUID))
	require.NoError(t, s.PutAdoptedPassageAuthority(ctx, domain, ref, identity.DocumentUID,
		versions[0], build.ID, attachment.ID))
	tag, err := s.CreateTag(ctx, "adopted passage")
	require.NoError(t, err)
	_, err = s.AssignPassageTag(ctx, tag.ID, ref)
	require.NoError(t, err)
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(metadata.Bytes())))
	var reexported bytes.Buffer
	require.NoError(t, restored.ExportMetadata(ctx, &reexported))
	require.Equal(t, metadata.Bytes(), reexported.Bytes())
	extension, err := s.ExportConceptFederationExtension(ctx, domain, true)
	require.NoError(t, err)
	require.Len(t, extension.PassageTags, 1)
	require.Equal(t, ref, extension.PassageTags[0].Ref)
	_, err = s.db.ExecContext(ctx, `UPDATE passage_tags SET document_uid=? WHERE tag_id=?`,
		"33333333-3333-4333-8333-333333333333", tag.ID)
	require.NoError(t, err)
	metadata.Reset()
	require.ErrorContains(t, s.ExportMetadata(ctx, &metadata), "passage tag local authority mismatch")
}

func TestPassageTagsStayOnExactVersion(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("passage-generation"))))
	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	ref := document.PassageRefV1{Version: 1, VaultUID: s.VaultID(), DocumentUID: identity.DocumentUID,
		ContentVersionID: versions[0], SourceSHA256: build.SourceSHA256,
		RenditionBuildID: build.ID, AttachmentID: attachment.ID,
		BodySHA256: testSHA256([]byte("body")), ByteStart: 0, ByteEnd: 1,
		QuoteSHA256: testSHA256([]byte("b"))}
	tag, err := s.CreateTag(ctx, "precise")
	require.NoError(t, err)
	assigned, err := s.AssignPassageTag(ctx, tag.ID, ref)
	require.NoError(t, err)
	require.True(t, assigned.Changed)
	require.Equal(t, ref, assigned.Ref)
	require.Greater(t, assigned.Tag.Revision, tag.Revision)
	extension, err := s.ExportConceptFederationExtension(ctx, "00000000-0000-4000-8000-000000000001", true)
	require.NoError(t, err)
	require.Len(t, extension.PassageTags, 1)
	require.Equal(t, ref, extension.PassageTags[0].Ref)
	docTags, err := s.TagsByAssignmentMode(ctx, node.ID, versions[0], "document")
	require.NoError(t, err)
	require.Empty(t, docTags)
	passageTags, err := s.TagsByAssignmentMode(ctx, node.ID, versions[0], "passage")
	require.NoError(t, err)
	require.Equal(t, tag.ID, passageTags[0].ID)
	_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision,
		testSHA256([]byte("changed source")), 14, "application/pdf")
	require.NoError(t, err)
	current, err := s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	currentTags, err := s.TagsByAssignmentMode(ctx, node.ID, current.CurrentVersionID, "either")
	require.NoError(t, err)
	require.Empty(t, currentTags)
	historical, err := s.PassageTags(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, tag.ID, historical[0].ID)
	trashed, _, err := s.Trash(ctx, node.ID, current.Revision)
	require.NoError(t, err)
	status, err := s.PassageTagAvailability(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, "unavailable", status)
	restored, _, err := s.Restore(ctx, trashed.ID, trashed.Revision)
	require.NoError(t, err)
	status, err = s.PassageTagAvailability(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, "exact", status)
	recoveredTags, err := s.PassageTags(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, tag.ID, recoveredTags[0].ID)
	_, _, err = s.Trash(ctx, restored.ID, restored.Revision)
	require.NoError(t, err)
	removed, err := s.RemovePassageTag(ctx, tag.ID, ref)
	require.NoError(t, err)
	require.True(t, removed.Changed)
	removed, err = s.RemovePassageTag(ctx, tag.ID, ref)
	require.NoError(t, err)
	require.False(t, removed.Changed)
	changed := ref
	changed.SourceSHA256 = testSHA256([]byte("different source"))
	_, err = s.AssignPassageTag(ctx, tag.ID, changed)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)
}

func TestAuditedPassageTagAssignmentRoundTrips(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("passage-generation"))))
	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	ref := document.PassageRefV1{Version: 1, VaultUID: s.VaultID(), DocumentUID: identity.DocumentUID,
		ContentVersionID: versions[0], SourceSHA256: build.SourceSHA256,
		RenditionBuildID: build.ID, AttachmentID: attachment.ID,
		BodySHA256: testSHA256([]byte("body")), ByteStart: 0, ByteEnd: 1,
		QuoteSHA256: testSHA256([]byte("b"))}
	tag, err := s.CreateTag(ctx, "audited passage")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	assigned, err := s.AssignPassageTag(ctx, tag.ID, ref)
	require.NoError(t, err)
	require.True(t, assigned.Changed)
	require.NoError(t, s.ValidateMetadata(ctx))
	removed, err := s.RemovePassageTag(ctx, tag.ID, ref)
	require.NoError(t, err)
	require.True(t, removed.Changed)
	require.NoError(t, s.ValidateMetadata(ctx))
}
