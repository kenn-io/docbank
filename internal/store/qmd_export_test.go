package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestQMDExportSourcesListsOnlyLiveCurrentSanitizedMarkdown(t *testing.T) {
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
	assert.Equal(t, s.VaultID(), source.VaultUID)
	assert.Equal(t, versions[0], source.ContentVersionID)
	assert.Equal(t, profile.Fingerprint, source.ProcessingProfileFingerprint)
	assert.Equal(t, attachment.ID, source.AttachmentID)
	assert.Equal(t, build.ID, source.BuildID)
	assert.Equal(t, catalogMarkdownBlobHash, source.BlobSHA256)
	assert.Equal(t, catalogMarkdownBlobHash, source.MarkdownChecksum)
	assert.Equal(t, int64(len(catalogBlobContents[catalogMarkdownBlobHash])), source.BlobSize)

	first, err := s.NodeViewByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	assert.Equal(t, first.Node.ID, source.NodeID)

	_, err = s.QMDExportSources(t.Context(), 0)
	require.Error(t, err)
	_, err = s.QMDExportSources(t.Context(), 100_001)
	require.Error(t, err)

	_, _, err = s.ReplaceContent(t.Context(), first.Node.ID, first.Node.Revision, fakeHash("b2"), 13, "application/pdf")
	require.NoError(t, err)
	active, err := s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, attachment.ID, active.Attachment.ID)
	sources, err = s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, sources, "a historical version's head is not current export authority")
}

func TestQMDExportSourcesOmitsTrashedCurrentHead(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339()}
	require.NoError(t, publishAttachmentForTest(t, s, attachment))
	file, err := s.NodeViewByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), file.Node.ID, file.Node.Revision)
	require.NoError(t, err)
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestQMDExportSourcesKeepsSeparateProfilesAndEnforcesMembershipLimit(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	first := catalogProcessingProfile(t, false)
	second := catalogProcessingProfileWith(t, false, func(profile *document.ProcessingProfileV1) {
		profile.Retrieval.LexicalLimit++
	})
	build := catalogRenditionBuild(s, first)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	for i, profile := range []ProcessingProfileRecord{first, second} {
		attachment := RenditionAttachmentRecord{
			ID:      []string{catalogAttachmentFirst, catalogAttachmentSecond}[i],
			VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID,
			Profile: profile, AttachedAt: nowRFC3339(),
		}
		require.NoError(t, publishAttachmentForTest(t, s, attachment))
	}
	sources, err := s.QMDExportSources(t.Context(), 2)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.Equal(t, sources[0].NodeID, sources[1].NodeID)
	assert.ElementsMatch(t, []string{first.Fingerprint, second.Fingerprint}, []string{
		sources[0].ProcessingProfileFingerprint, sources[1].ProcessingProfileFingerprint,
	})
	sources, err = s.QMDExportSources(t.Context(), 1)
	require.ErrorContains(t, err, "membership exceeds limit")
	assert.Nil(t, sources)
}

func TestRevalidateQMDExportCandidatesRequiresExactLiveAttachment(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339()}
	require.NoError(t, publishAttachmentForTest(t, s, attachment))
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	allowed, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, allowed, 1)
	assert.Equal(t, sources[0].NodeID, allowed[0].NodeID)
	assert.Equal(t, versions[0], allowed[0].ContentVersionID)
	assert.Equal(t, "/synthetic-source-a.pdf", allowed[0].Path)
	assert.Positive(t, allowed[0].NodeRevision)
	assert.True(t, allowed[0].InScope)

	excluded, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{MIMEType: "text/plain"})
	require.NoError(t, err, "an out-of-scope live document is not stale authority")
	require.Len(t, excluded, 1)
	assert.False(t, excluded[0].InScope)

	drifted := sources[0]
	drifted.AttachmentID = "different-attachment"
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{drifted}, SearchOptions{MIMEType: "text/plain"})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)

	file, err := s.NodeViewByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), file.Node.ID, file.Node.Revision)
	require.NoError(t, err)
	_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
}

func TestRevalidateQMDExportCandidatesFiltersUnknownMIME(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	file, err := s.CreateFile(t.Context(), s.RootID(), "unknown-format", catalogSourceHash, 20, "")
	require.NoError(t, err)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	require.NoError(t, publishAttachmentForTest(t, s, RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: file.CurrentVersionID,
		BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339(),
	}))
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	live, err := s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{MIMEType: "application/pdf"})
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.False(t, live[0].InScope)
}
