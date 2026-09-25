package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveRenditionTextReportsExactFailedAttempts(t *testing.T) {
	t.Parallel()
	for _, operator := range []bool{false, true} {
		t.Run(fmt.Sprintf("operator_required=%t", operator), func(t *testing.T) {
			s, _, nodes, profile := collectionCoverageFixture(t, 1)
			node := nodes[0]
			collectionCoverageFail(t, s, node, profile, operator)
			binding := RenditionTextBinding{NodeID: node.ID, NodeRevision: node.Revision,
				ContentVersionID: node.CurrentVersionID, SourceSHA256: node.BlobHash,
				SourceSize: node.Size, ProfileFingerprint: profile.Fingerprint}
			_, err := s.ResolveRenditionText(t.Context(), binding)
			require.ErrorIs(t, err, ErrRenditionTextFailed)

			otherProfile := binding
			otherProfile.ProfileFingerprint = catalogProcessingProfile(t, true).Fingerprint
			_, err = s.ResolveRenditionText(t.Context(), otherProfile)
			require.ErrorIs(t, err, ErrNotFound)

			collectionCoveragePublish(t, s, node, profile, "complete")
			_, err = s.ResolveRenditionText(t.Context(), binding)
			require.NoError(t, err, "retained readable text takes precedence over a failed attempt")

			replaced, version, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, node.BlobHash, node.Size, node.MimeType)
			require.NoError(t, err)
			binding.NodeRevision, binding.ContentVersionID = replaced.Revision, version.ID
			_, err = s.ResolveRenditionText(t.Context(), binding)
			require.ErrorIs(t, err, ErrNotFound, "same bytes in a new version must not inherit the old failure")
		})
	}
}

func TestResolveRenditionTextBindsLiveNodeSourceHeadAndGeneration(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
