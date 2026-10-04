package main

import (
	"bytes"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type reportRenditionFixture struct {
	config  config.Config
	profile string
}

func newReportRenditionFixture(t *testing.T) reportRenditionFixture {
	t.Helper()
	f := reportRenditionFixture{config: config.Default()}
	hash := sha256Hex
	f.config.RenditionProfiles["primary"] = config.RenditionProfileConfig{
		AdapterContract: "synthetic-pdf/v1", CredentialBinding: "credential:synthetic",
		AuthorizationFingerprint: hash("authorization"), DeploymentFingerprint: hash("deployment"),
		DescriptorID: "synthetic-pdf", DescriptorFingerprint: hash("descriptor"),
		DisclosureFingerprint: hash("disclosure"), UploadOptionsFingerprint: hash("upload"),
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 10,
		RequestedArtifacts: []string{"structured_evidence"}, TrustBoundary: "synthetic-vault",
	}
	f.config.RetrievalProfiles["local"] = config.RetrievalProfileConfig{
		LexicalLimit: 10, VectorLimit: 10,
	}
	f.config.ProcessingProfiles["archive"] = config.ProcessingProfileConfig{
		Rendition: "primary", Retrieval: "local", AttachmentPolicyFingerprint: hash("attachments"),
		CompletenessFingerprint: hash("completeness"), ConsentFingerprint: hash("consent"),
		LexicalSegmenterFingerprint: hash("segments"), NormalizerFingerprint: hash("normalizer"),
		MaxDocumentChars: 100_000, MaxSegmentRunes: 100, MaxUnitRunes: 1_000,
		RetainSanitizedMarkdown: true, SanitizerFingerprint: hash("sanitizer"),
		TrustBoundary: "synthetic-vault",
	}
	require.NoError(t, f.config.Validate())
	resolved, err := f.config.ProcessingProfile("archive")
	require.NoError(t, err)
	_, fingerprints, err := document.CanonicalProfile(resolved.Document)
	require.NoError(t, err)
	f.profile = fingerprints.Profile
	return f
}

func (f reportRenditionFixture) publish(
	t *testing.T, catalog *store.Store, blobs *blob.Store,
	node store.Node, source document.SourceEvidenceV1,
) {
	t.Helper()
	resolved, err := f.config.ProcessingProfile("archive")
	require.NoError(t, err)
	canonical, fingerprints, err := document.CanonicalProfile(resolved.Document)
	require.NoError(t, err)
	require.Equal(t, f.profile, fingerprints.Profile)
	profile := store.ProcessingProfileRecord{
		Fingerprint: f.profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    resolved.Document.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             resolved.Document.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: resolved.Document.Rendition.DisclosureFingerprint,
		TrustBoundary:                  resolved.Document.RetentionDisclosure.TrustBoundary,
	}
	evidencePolicy, renditionPolicy, err := document.RenditionExecutionPoliciesForProfileV1(
		resolved.Document)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(source, evidencePolicy)
	require.NoError(t, err)
	evidence, evidenceHash, err := document.MarshalNormalizedEvidenceV1(normalized)
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(normalized, renditionPolicy)
	require.NoError(t, err)
	policy := jsontext.Value(`{"roles":[` +
		`{"max_count":1,"min_count":1,"role":"normalized_evidence"},` +
		`{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	build := store.RenditionBuildRecord{
		ID: sha256Hex("build:" + node.CurrentVersionID), VaultID: catalog.VaultID(),
		SourceSHA256: node.BlobHash, RenditionRequestFingerprint: fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint: fingerprints.EvidenceLexical,
		CapturedArtifactPolicy:     policy, CapturedArtifactPolicyFingerprint: sha256Hex(string(policy)),
		AuthorizationChecksum: sha256Hex("authorization"), ProviderOperationID: "synthetic-pdf",
		ProviderReceipt:  jsontext.Value(`{"provider":"synthetic"}`),
		EvidenceChecksum: evidenceHash, RenditionChecksum: rendition.Checksum,
		MarkdownChecksum: rendition.MarkdownChecksum, Completeness: document.EvidenceComplete,
		CompletedAt: "2026-09-11T12:00:00.000000000Z", DeclaredArtifactCount: 2,
		Artifacts: []store.RenditionArtifactRecord{
			{ID: "artifact_" + evidenceHash, Role: "normalized_evidence", BlobHash: evidenceHash,
				Size: int64(len(evidence)), Checksum: evidenceHash, State: store.RenditionArtifactVerified},
			{ID: "artifact_" + rendition.MarkdownChecksum, Role: "sanitized_markdown",
				BlobHash: rendition.MarkdownChecksum, Size: int64(len(rendition.Markdown)),
				Checksum: rendition.MarkdownChecksum, State: store.RenditionArtifactVerified},
		},
	}
	for _, unit := range rendition.Units {
		build.Units = append(build.Units, store.RenditionUnitRecord{ID: unit.ID,
			EvidenceUnitID: unit.EvidenceUnitID, Order: unit.Order, Checksum: unit.Checksum,
			HeadingPath: unit.HeadingPath, Locator: unit.Locator})
	}
	for _, segment := range rendition.LexicalSegments {
		build.LexicalSegments = append(build.LexicalSegments, store.RenditionLexicalSegmentRecord{
			ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order, CharStart: segment.CharStart,
			CharEnd: segment.CharEnd, Checksum: segment.Checksum, Text: segment.Text})
	}
	attachment := store.RenditionAttachmentRecord{
		ID: sha256Hex("attachment:" + node.CurrentVersionID), VaultID: catalog.VaultID(),
		ContentVersionID: node.CurrentVersionID, BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-09-11T12:01:00.000000000Z",
	}
	publisher, err := processing.NewArtifactPublisher(catalog, blobs)
	require.NoError(t, err)
	_, err = publisher.PublishRendition(t.Context(), processing.StagedRendition{
		Rendition: rendition, RenditionPolicy: renditionPolicy, Build: build, Attachment: attachment,
		Head: store.RenditionHeadRecord{ContentVersionID: node.CurrentVersionID,
			ProcessingProfileFingerprint: f.profile, AttachmentID: attachment.ID,
			PublishedAt: "2026-09-11T12:02:00.000000000Z"},
		LexicalGenerationID: sha256Hex("generation:" + node.CurrentVersionID),
		Artifacts: []processing.StagedArtifact{
			{ID: build.Artifacts[0].ID, Payload: bytes.NewReader(evidence)},
			{ID: build.Artifacts[1].ID, Payload: bytes.NewReader(rendition.Markdown)},
		},
	})
	require.NoError(t, err)
}
