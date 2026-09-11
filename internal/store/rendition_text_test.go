package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveRenditionTextBindsLiveNodeSourceHeadAndGeneration(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	generationID := testSHA256([]byte("rendition-text-generation"))
	require.NoError(t, publishRenditionForTest(
		t, s, attachment, "2026-08-22T10:01:00.000000000Z", generationID,
	))
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)

	view, err := s.ResolveRenditionText(t.Context(), RenditionTextBinding{
		NodeID: node.ID, NodeRevision: node.Revision, ContentVersionID: versions[0],
		SourceSHA256: node.BlobHash, SourceSize: node.Size,
		ProfileFingerprint: profile.Fingerprint, GenerationID: generationID,
		AttachmentID: attachment.ID, BuildID: build.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, node.ID, view.Node.ID)
	assert.Equal(t, versions[0], view.Version.ID)
	assert.Equal(t, attachment.ID, view.Rendition.Attachment.ID)
	assert.Equal(t, build.ID, view.Rendition.Build.ID)
	assert.False(t, view.Empty)
	require.NotNil(t, view.Artifact)
	assert.Equal(t, "sanitized_markdown", view.Artifact.Role)
	assert.Equal(t, catalogMarkdownBlobHash, view.Artifact.BlobHash)
}

func TestResolveRenditionTextRejectsEveryChangedAuthority(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	generationID := testSHA256([]byte("rendition-text-generation"))
	require.NoError(t, publishRenditionForTest(
		t, s, attachment, "2026-08-22T10:01:00.000000000Z", generationID,
	))
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	valid := RenditionTextBinding{
		NodeID: node.ID, NodeRevision: node.Revision, ContentVersionID: versions[0],
		SourceSHA256: node.BlobHash, SourceSize: node.Size,
		ProfileFingerprint: profile.Fingerprint, GenerationID: generationID,
		AttachmentID: attachment.ID, BuildID: build.ID,
	}

	for name, mutate := range map[string]func(*RenditionTextBinding){
		"node revision": func(value *RenditionTextBinding) { value.NodeRevision++ },
		"source hash":   func(value *RenditionTextBinding) { value.SourceSHA256 = testSHA256([]byte("wrong source")) },
		"source size":   func(value *RenditionTextBinding) { value.SourceSize++ },
		"attachment":    func(value *RenditionTextBinding) { value.AttachmentID = testSHA256([]byte("wrong attachment")) },
		"build":         func(value *RenditionTextBinding) { value.BuildID = testSHA256([]byte("wrong build")) },
		"generation":    func(value *RenditionTextBinding) { value.GenerationID = testSHA256([]byte("wrong generation")) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := valid
			mutate(&changed)
			_, err := s.ResolveRenditionText(t.Context(), changed)
			require.ErrorIs(t, err, ErrRenditionTextStale)
		})
	}
}

func TestResolveRenditionTextDoesNotTreatMissingSegmentsAsVerifiedEmptyMarkdown(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	build.LexicalSegments = nil
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("empty-check-generation"))))
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	_, err = s.ResolveRenditionText(t.Context(), RenditionTextBinding{NodeID: node.ID,
		NodeRevision: node.Revision, ContentVersionID: versions[0], SourceSHA256: node.BlobHash,
		SourceSize: node.Size, ProfileFingerprint: profile.Fingerprint})
	require.ErrorIs(t, err, ErrRenditionTextUnavailable)
}
