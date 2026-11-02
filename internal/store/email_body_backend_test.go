package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// Build a real local body receipt from the MIME selection, without a provider.
func stageEmailBodyForTest(t *testing.T, s *Store, view EmailMetadataView) EmailBodyPublication {
	t.Helper()
	profile, err := emailBodyProfileRecord(view.Evidence.Recipe)
	require.NoError(t, err)
	p, err := document.EmailBodyProfileV1(view.Evidence.Recipe)
	require.NoError(t, err)
	ep, rp, err := document.RenditionExecutionPoliciesForProfileV1(p)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceDegradedProvenance,
		Family: "text", UnitKind: document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "Derived body text retains its MIME authority separately."}},
		Units:     []document.SourceEvidenceUnitV1{{Order: 0, Text: "Chosen synthetic body.", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone}}},
	}, ep)
	require.NoError(t, err)
	evidenceBytes, evidenceHash, err := document.MarshalNormalizedEvidenceV1(evidence)
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(evidence, rp)
	require.NoError(t, err)
	path, body := emailSelectedBody(view.Evidence)
	require.NotNil(t, path)
	require.NotNil(t, body)
	recipe, err := document.EmailBodyRecipeFingerprint(view.Evidence.Recipe)
	require.NoError(t, err)
	receipt := document.EmailBodyReceiptV1{ContractVersion: document.EmailBodyReceiptContractV1,
		SourceSHA256: view.Version.BlobHash, SourceSize: view.Version.Size,
		EmailGenerationID: view.Generation.ID, EmailChecksum: view.Generation.Checksum,
		PartPath: *path, BodySHA256: body.SHA256, BodySize: body.Size, BodyRecipeFingerprint: recipe,
		EvidenceChecksum: evidenceHash, RenditionChecksum: rendition.Checksum, MarkdownChecksum: rendition.MarkdownChecksum}
	raw, _, err := document.MarshalEmailBodyReceiptV1(receipt)
	require.NoError(t, err)
	id, err := document.EmailBodyBuildID(receipt)
	require.NoError(t, err)
	operation, err := document.EmailBodyOperationID(recipe)
	require.NoError(t, err)
	authorization, err := document.EmailBodyAuthorizationChecksum(view.Version.BlobHash, view.Version.Size, recipe)
	require.NoError(t, err)
	build := RenditionBuildRecord{ID: id, VaultID: s.VaultID(), SourceSHA256: view.Version.BlobHash,
		RenditionRequestFingerprint: profile.RenditionRequestFingerprint, EvidenceLexicalFingerprint: profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicy: []byte(catalogCapturedPolicy), CapturedArtifactPolicyFingerprint: testSHA256([]byte(catalogCapturedPolicy)),
		AuthorizationChecksum: authorization, ProviderOperationID: operation, ProviderReceipt: raw,
		EvidenceChecksum: evidenceHash, RenditionChecksum: rendition.Checksum, MarkdownChecksum: rendition.MarkdownChecksum,
		Completeness: rendition.Completeness, CompletedAt: nowRFC3339(), DeclaredArtifactCount: 2}
	for _, a := range []struct {
		role string
		data []byte
	}{{"normalized_evidence", evidenceBytes}, {"sanitized_markdown", rendition.Markdown}} {
		hash := testSHA256(a.data)
		size := int64(len(a.data))
		require.NoError(t, s.RecordRenditionBlob(t.Context(), hash, size, BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: size}))
		build.Artifacts = append(build.Artifacts, RenditionArtifactRecord{ID: testSHA256([]byte(id + a.role)), Role: a.role, BlobHash: hash, Checksum: hash, Size: size, State: RenditionArtifactVerified})
	}
	for _, u := range rendition.Units {
		build.Units = append(build.Units, RenditionUnitRecord{ID: u.ID, EvidenceUnitID: u.EvidenceUnitID, Order: u.Order, Checksum: u.Checksum, HeadingPath: u.HeadingPath, Locator: u.Locator})
	}
	for _, seg := range rendition.LexicalSegments {
		build.LexicalSegments = append(build.LexicalSegments, RenditionLexicalSegmentRecord{ID: seg.ID, UnitID: seg.UnitID, Order: seg.Order, CharStart: seg.CharStart, CharEnd: seg.CharEnd, Checksum: seg.Checksum, Text: seg.Text})
	}
	for _, w := range rendition.Warnings {
		build.Warnings = append(build.Warnings, w.Code)
	}
	require.NoError(t, s.StageEmailBodyBuild(t.Context(), build))
	// Re-observation retains the first build's completion time.
	build.CompletedAt = "2099-01-01T00:00:00.000000000Z"
	require.NoError(t, s.StageEmailBodyBuild(t.Context(), build))
	a := RenditionAttachmentRecord{ID: RenditionAttachmentID(id, view.Version.ID, profile.Fingerprint), VaultID: s.VaultID(), ContentVersionID: view.Version.ID, BuildID: id, Profile: profile, AttachedAt: nowRFC3339()}
	generation, err := s.StageLexicalGeneration(t.Context(), lexicalReplacementGenerationIDForTest(t, s))
	require.NoError(t, err)
	return EmailBodyPublication{EmailAttachmentID: view.Attachment.ID, PartPath: *path, BodyRecipeFingerprint: recipe, RenditionAttachment: a,
		RenditionHead: RenditionHeadRecord{ContentVersionID: a.ContentVersionID, ProcessingProfileFingerprint: profile.Fingerprint, AttachmentID: a.ID, PublishedAt: a.AttachedAt}, LexicalGenerationID: generation.ID}
}

func checkEmailBodyPublication(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	f := newEmailFixture(t, s, "body.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	publication := stageEmailBodyForTest(t, s, view)
	require.NoError(t, s.PublishEmailBody(t.Context(), publication))
	require.NoError(t, s.PublishEmailBody(t.Context(), publication), "exact publication replay")
	current, err := s.EmailMetadata(t.Context(), view.Version.ID)
	require.NoError(t, err)
	require.Equal(t, "available", current.BodySearch.State)
	require.Equal(t, publication.RenditionAttachment.BuildID, *current.BodySearch.RenditionBuildID)
	active, err := s.ActiveRendition(t.Context(), view.Version.ID, publication.RenditionAttachment.Profile.Fingerprint)
	require.NoError(t, err)
	require.Len(t, active.Build.LexicalSegments, 1)
	require.Equal(t, `Chosen synthetic body\.`, active.Build.LexicalSegments[0].Text)
	require.NotEqual(t, "2099-01-01T00:00:00.000000000Z", active.Build.CompletedAt)
	checkRetentionMetadata(t, s)
	report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{BuildIDs: []string{publication.RenditionAttachment.BuildID}})
	require.NoError(t, err)
	require.Equal(t, 1, report.RemovedBuilds)
	require.ErrorIs(t, s.PublishEmailBody(t.Context(), publication), ErrEmailDerivativeSuppressed)
	checkRetentionMetadata(t, s)
}
