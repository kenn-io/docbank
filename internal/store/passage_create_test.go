package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPassageCreationAuthorityUsesExactRetainedTuple(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("synthetic generation"))))
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	authority, err := s.PassageCreationAuthority(t.Context(), node.ID, versions[0], build.ID, attachment.ID)
	require.NoError(t, err)
	require.Equal(t, versions[0], authority.Version.ID)
	require.Equal(t, node.ID, authority.Node.ID)
	require.Equal(t, build.SourceSHA256, authority.Version.BlobHash)
	require.NotEmpty(t, authority.Artifact.BlobHash)
	claim := PassageIdentityClaim{NodeID: node.ID, ContentVersionID: versions[0],
		RenditionBuildID: build.ID, AttachmentID: attachment.ID,
		ArtifactHash: authority.Artifact.BlobHash}
	_, err = s.EnsurePassageDocumentIdentity(t.Context(), PassageIdentityClaim{
		NodeID: claim.NodeID, ContentVersionID: claim.ContentVersionID,
		RenditionBuildID: claim.RenditionBuildID, AttachmentID: claim.AttachmentID,
		ArtifactHash: testSHA256([]byte("wrong retained artifact")),
	})
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)
	blockedInput := claim
	blockedInput.InputID = "synthetic-hidden-input"
	blockedInput.Principal = "synthetic-owner"
	_, err = s.EnsurePassageDocumentIdentity(t.Context(), blockedInput)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)
	var allocated int
	require.NoError(t, s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM document_identities WHERE node_id=?`, node.ID).Scan(&allocated))
	require.Zero(t, allocated)
	identity, err := s.EnsurePassageDocumentIdentity(t.Context(), claim)
	require.NoError(t, err)
	require.Equal(t, node.ID, identity.NodeID)

	for _, selector := range []struct {
		node                       int64
		version, build, attachment string
	}{
		{node.ID, versions[1], build.ID, attachment.ID},
		{node.ID, versions[0], testSHA256([]byte("different build")), attachment.ID},
		{node.ID, versions[0], build.ID, testSHA256([]byte("different attachment"))},
		{node.ID + 10000, versions[0], build.ID, attachment.ID},
	} {
		_, err := s.PassageCreationAuthority(t.Context(), selector.node, selector.version, selector.build, selector.attachment)
		require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)
	}
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision,
		testSHA256([]byte("replacement")), 11, "application/pdf")
	require.NoError(t, err)
	authority, err = s.PassageCreationAuthority(t.Context(), node.ID, versions[0], build.ID, attachment.ID)
	require.NoError(t, err)
	require.False(t, authority.Fresh)
	repeated, err := s.EnsurePassageDocumentIdentity(t.Context(), claim)
	require.NoError(t, err)
	require.Equal(t, identity, repeated)
	moved, _, err := s.MoveToPath(t.Context(), node.ID, UnconditionalRev, "/renamed-synthetic.pdf")
	require.NoError(t, err)
	run, err := s.BeginIngest(t.Context(), "synthetic", "path reuse")
	require.NoError(t, err)
	reused, err := s.IngestFileExact(t.Context(), run, s.RootID(), "synthetic-source-a.pdf",
		testSHA256([]byte("unrelated new node")), 18, "application/pdf", "synthetic-source-a.pdf", "")
	require.NoError(t, err)
	_, err = s.PassageCreationAuthority(t.Context(), reused.ID, versions[0], build.ID, attachment.ID)
	require.ErrorIs(t, err, ErrPassageAuthorityUnavailable)
	authority, err = s.PassageCreationAuthority(t.Context(), moved.ID, versions[0], build.ID, attachment.ID)
	require.NoError(t, err)
	require.Equal(t, "/renamed-synthetic.pdf", authority.Path)
}
