package store

import (
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func citationForAttachment(t *testing.T, s *Store, attachmentID string) document.TextCitation {
	t.Helper()
	attachment, err := loadRenditionAttachment(t.Context(), s.db, attachmentID)
	require.NoError(t, err)
	version, err := s.ContentVersionByID(t.Context(), attachment.ContentVersionID)
	require.NoError(t, err)
	build, err := s.RenditionBuild(t.Context(), attachment.BuildID)
	require.NoError(t, err)
	vaultID, err := uuid.Parse(s.VaultID())
	require.NoError(t, err)
	versionID, err := uuid.Parse(version.ID)
	require.NoError(t, err)
	return document.TextCitation{
		Version: 1, VaultUID: vaultID, NodeID: version.NodeID, ContentVersionID: versionID,
		ContentSHA256: version.BlobHash, RenditionAttachmentID: attachmentID,
		BuildID: build.ID, RenditionSHA256: build.MarkdownChecksum, End: 3,
	}
}

func TestRetainedTextCitationAuthority(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"rename", "source replacement", "head replacement", "trash", "prune"} {
		t.Run(action, func(t *testing.T) {
			s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
			citation := citationForAttachment(t, s, attachmentID)
			artifact, err := s.RetainedTextCitation(t.Context(), citation)
			require.NoError(t, err)
			require.Equal(t, catalogMarkdownBlobHash, artifact.BlobHash)
			require.Equal(t, int64(len(catalogBlobContents[catalogMarkdownBlobHash])), artifact.Size)
			switch action {
			case "rename":
				_, _, err = s.Move(t.Context(), citation.NodeID, s.RootID(), "renamed.pdf", UnconditionalRev)
			case "source replacement", "prune":
				_, _, err = s.ReplaceContent(t.Context(), citation.NodeID, UnconditionalRev,
					fakeHash("99"), 3, "text/plain")
			case "head replacement":
				build := catalogRenditionBuild(s, profile)
				build.ID = fakeHash("98")
				for i := range build.Artifacts {
					build.Artifacts[i].ID += "_new"
				}
				for i := range build.Units {
					build.Units[i].ID += "_new"
				}
				for i := range build.LexicalSegments {
					build.LexicalSegments[i].ID += "_new"
					build.LexicalSegments[i].UnitID += "_new"
				}
				require.NoError(t, s.StageRenditionBuild(t.Context(), build))
				err = publishRenditionForTest(t, s, RenditionAttachmentRecord{
					ID: fakeHash("97"), VaultID: s.VaultID(), ContentVersionID: versionID,
					BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime,
				}, embeddingCatalogTime, fakeHash("96"))
			case "trash":
				_, _, err = s.Trash(t.Context(), citation.NodeID, UnconditionalRev)
			}
			require.NoError(t, err)
			if action == "prune" {
				_, err = s.PruneContentVersions(t.Context(), citation.NodeID, UnconditionalRev,
					VersionPruneSelector{VersionIDs: []string{versionID}}, true)
				require.NoError(t, err)
			}
			got, err := s.RetainedTextCitation(t.Context(), citation)
			if action == "trash" || action == "prune" {
				require.ErrorIs(t, err, document.ErrCitationUnavailable)
			} else {
				require.NoError(t, err)
				require.Equal(t, artifact, got)
			}
			if action == "trash" {
				_, _, err = s.Restore(t.Context(), citation.NodeID, UnconditionalRev)
				require.NoError(t, err)
				got, err = s.RetainedTextCitation(t.Context(), citation)
				require.NoError(t, err)
				require.Equal(t, artifact, got)
			}
			if action == "source replacement" || action == "head replacement" {
				_, _, err = s.CurrentRenditionByAttachment(t.Context(), citation.NodeID,
					versionID, citation.ContentSHA256, attachmentID)
				require.ErrorIs(t, err, ErrNotFound)
			}
		})
	}
}

func TestRetainedTextCitationMismatches(t *testing.T) {
	t.Parallel()
	s, _, _, attachmentID := newEmbeddingCatalogFixture(t)
	citation := citationForAttachment(t, s, attachmentID)
	for name, change := range map[string]func(*document.TextCitation){
		"vault":      func(c *document.TextCitation) { c.VaultUID = uuid.New() },
		"node":       func(c *document.TextCitation) { c.NodeID = s.RootID() },
		"version":    func(c *document.TextCitation) { c.ContentVersionID = uuid.New() },
		"content":    func(c *document.TextCitation) { c.ContentSHA256 = strings.Repeat("e", 64) },
		"attachment": func(c *document.TextCitation) { c.RenditionAttachmentID = strings.Repeat("e", 64) },
		"build":      func(c *document.TextCitation) { c.BuildID = strings.Repeat("e", 64) },
		"rendition":  func(c *document.TextCitation) { c.RenditionSHA256 = strings.Repeat("e", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := citation
			change(&bad)
			_, err := s.RetainedTextCitation(t.Context(), bad)
			require.ErrorIs(t, err, document.ErrCitationUnavailable)
		})
	}
	build, err := s.RenditionBuild(t.Context(), citation.BuildID)
	require.NoError(t, err)
	build.ID = fakeHash("95")
	for i := range build.Artifacts {
		build.Artifacts[i].ID += "_dormant"
	}
	build.Units, build.LexicalSegments = nil, nil
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	citation.BuildID = build.ID
	_, err = s.RetainedTextCitation(t.Context(), citation)
	require.ErrorIs(t, err, document.ErrCitationUnavailable)
}

func TestRetainedTextCitationSuppression(t *testing.T) {
	t.Parallel()
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	citation := citationForAttachment(t, s, attachmentID)
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), chunk))
	require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), CurrentRenditionRoot{
		ID: "citation-embedding-pin", Kind: RenditionRootBackupPin,
		TargetKind: RenditionRootEmbeddingSet, TargetID: chunk.ID,
		FencingToken: 1, RecordedAt: embeddingCatalogTime,
	}))
	version, err := s.ContentVersionByID(t.Context(), versionID)
	require.NoError(t, err)
	other, err := s.CreateFile(t.Context(), s.RootID(), "shared.pdf",
		version.BlobHash, version.Size, version.MimeType)
	require.NoError(t, err)
	otherAttachment := RenditionAttachmentRecord{
		ID: fakeHash("94"), VaultID: s.VaultID(), ContentVersionID: other.CurrentVersionID,
		BuildID: citation.BuildID, Profile: profile, AttachedAt: embeddingCatalogTime,
	}
	require.NoError(t, publishRenditionForTest(t, s, otherAttachment, embeddingCatalogTime, fakeHash("93")))
	otherCitation := citationForAttachment(t, s, otherAttachment.ID)
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{AttachmentIDs: []string{attachmentID}})
	require.NoError(t, err)
	_, err = loadRenditionAttachment(t.Context(), s.db, attachmentID)
	require.NoError(t, err, "the embedding root must leave the purged attachment retained")
	_, err = s.RetainedTextCitation(t.Context(), citation)
	require.ErrorIs(t, err, document.ErrCitationUnavailable)
	_, err = s.RetainedTextCitation(t.Context(), otherCitation)
	require.NoError(t, err, "purge must not deny the unaffected shared attachment")
}
