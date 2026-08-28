package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, s.AttachRenditionBuild(t.Context(), attachment))
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
}

func TestQMDExportSourcesOmitsTrashedCurrentHead(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339()}
	require.NoError(t, s.AttachRenditionBuild(t.Context(), attachment))
	require.NoError(t, publishAttachmentForTest(t, s, attachment))
	file, err := s.NodeViewByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), file.Node.ID, file.Node.Revision)
	require.NoError(t, err)
	sources, err := s.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestRevalidateQMDExportCandidatesRequiresExactLiveAttachment(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339()}
	require.NoError(t, s.AttachRenditionBuild(t.Context(), attachment))
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

	drifted := sources[0]
	drifted.AttachmentID = "different-attachment"
	_, err = s.RevalidateQMDExportCandidates(t.Context(), []QMDExportSource{drifted}, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)

	file, err := s.NodeViewByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), file.Node.ID, file.Node.Revision)
	require.NoError(t, err)
	_, err = s.RevalidateQMDExportCandidates(t.Context(), sources, SearchOptions{})
	require.ErrorIs(t, err, ErrQMDExportAuthorityStale)
}

func TestNormalizeQMDSearchScopeRejectsInvalidScope(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	_, err := s.NormalizeQMDSearchScope(t.Context(), SearchOptions{UnderNodeID: -1})
	require.Error(t, err)
}
