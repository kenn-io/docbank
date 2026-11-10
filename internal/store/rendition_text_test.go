package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func checkResolveRenditionTextReportsExactFailedAttempts(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	for _, operator := range []bool{false, true} {
		t.Run(fmt.Sprintf("operator_required=%t", operator), func(t *testing.T) {
			s := open(t)
			seedRenditionCatalogVersions(t, s)
			profile := catalogProcessingProfile(t, false)
			node, err := s.CreateFile(t.Context(), s.RootID(), "failed-source.pdf", catalogSourceHash, 20, "application/pdf")
			require.NoError(t, err)
			collectionCoverageFail(t, s, node, profile, operator)
			binding := RenditionTextBinding{NodeID: node.ID, NodeRevision: node.Revision,
				ContentVersionID: node.CurrentVersionID, SourceSHA256: node.BlobHash,
				SourceSize: node.Size, ProfileFingerprint: profile.Fingerprint}
			_, err = s.ResolveRenditionText(t.Context(), binding)
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

func checkResolveRenditionTextBindsLiveNodeSourceHeadAndGeneration(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	versions := seedRenditionCatalogVersions(t, s)
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

func TestResolveRenditionTextIgnoresFailedPhotoDescriptions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "described.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	request := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
	grantRenditionJobConsent(t, s, request)
	at := time.Now().UTC().Add(time.Minute)
	_, _, claim := startPhotoDescription(t, s, request, at)
	require.NoError(t, s.MarkRenditionJobFailed(ctx, claim, RenditionFailureTerminal, at.Add(time.Second)))

	_, err = s.ResolveRenditionText(ctx, RenditionTextBinding{NodeID: node.ID, NodeRevision: node.Revision,
		ContentVersionID: node.CurrentVersionID, SourceSHA256: node.BlobHash, SourceSize: node.Size,
		ProfileFingerprint: request.Profile.Fingerprint})
	require.ErrorIs(t, err, ErrNotFound, "a description never produces text, so its failure is not a text failure")
}

func checkResolveRenditionTextOmissionOnlyPDF(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	for _, test := range []struct {
		name    string
		corrupt bool
	}{
		{name: "plain"},
		{name: "corrupt", corrupt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := open(t)
			node, profile, artifactSize := seedOmissionOnlyPDF(t, s, test.corrupt)
			view, err := s.ResolveRenditionText(t.Context(), RenditionTextBinding{NodeID: node.ID, NodeRevision: node.Revision, ContentVersionID: node.CurrentVersionID, SourceSHA256: node.BlobHash, SourceSize: node.Size, ProfileFingerprint: profile.Fingerprint})
			if test.corrupt {
				require.ErrorIs(t, err, ErrRenditionTextUnavailable)
				return
			}
			require.NoError(t, err)
			require.True(t, view.Empty)
			require.NotNil(t, view.Artifact)
			require.Equal(t, artifactSize, view.Artifact.Size)
		})
	}
}

func seedOmissionOnlyPDF(t *testing.T, s *Store, corrupt bool) (Node, ProcessingProfileRecord, int64) {
	t.Helper()
	seedRenditionCatalogVersions(t, s)
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	profile := embeddingCatalogProfile(t)
	build := catalogRenditionBuild(s, profile)
	build.Units = nil
	build.LexicalSegments = nil
	build.Completeness = document.EvidencePartial
	build.PartialSuccess = true
	empty, _, err := document.EnvelopeRenditionV1(document.RenditionV1{Completeness: document.EvidencePartial, MarkdownChecksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}, document.RenditionEnvelopeV1{
		BuildID: build.ID, SourceSHA256: build.SourceSHA256, SourceFormat: "pdf", SourceMediaType: "application/pdf",
		RenditionRequestFingerprint: build.RenditionRequestFingerprint, EvidenceLexicalFingerprint: build.EvidenceLexicalFingerprint,
		NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, UnitKind: document.EvidenceUnitPage,
	})
	require.NoError(t, err)
	if corrupt {
		empty.Markdown = append(empty.Markdown, []byte("missing text")...)
	}
	build.MarkdownChecksum = testSHA256(empty.Markdown)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(t.Context(), tx, build.MarkdownChecksum, int64(len(empty.Markdown)))
	}))
	for i := range build.Artifacts {
		if build.Artifacts[i].Role == catalogArtifactSanitizedMarkdown {
			build.Artifacts[i].Checksum = build.MarkdownChecksum
			build.Artifacts[i].BlobHash = build.MarkdownChecksum
			build.Artifacts[i].Size = int64(len(empty.Markdown))
		}
	}
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: node.CurrentVersionID, BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment, "2026-08-22T10:01:00.000000000Z", testSHA256([]byte("empty PDF generation"))))
	return node, profile, int64(len(empty.Markdown))
}

func checkEmptyPDFChunkSettlement(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	node, profile, _ := seedOmissionOnlyPDF(t, s, false)
	profileFingerprint := profile.Fingerprint

	coverage, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{ContentVersionIDs: []string{node.CurrentVersionID}, ProcessingProfileFingerprint: profileFingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "chunk"}, {BindingID: "optional"}}})
	require.NoError(t, err)
	require.Equal(t, "complete", coverage.Embeddings[0].State)
	require.Equal(t, "unavailable", coverage.Embeddings[1].State)
	targets, err := s.MissingProcessingTargetsAfter(t.Context(), profileFingerprint, "", 4, 100, false)
	require.NoError(t, err)
	require.Contains(t, targets, DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name}, "original-file bindings still need work")
	targets, err = s.MissingProcessingTargetsAfter(t.Context(), profileFingerprint, "", 2, 100, false)
	require.NoError(t, err)
	require.NotContains(t, targets, DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name}, "both chunk bindings have a retained empty outcome")
	node, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, node.BlobHash, node.Size, node.MimeType)
	require.NoError(t, err)
	targets, err = s.MissingProcessingTargetsAfter(t.Context(), profileFingerprint, "", 2, 100, false)
	require.NoError(t, err)
	require.Contains(t, targets, DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name}, "a new source version needs its own outcome")
}
