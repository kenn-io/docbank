package main

import (
	"bytes"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/go-pdf/fpdf"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

type pdfReportOriginal struct {
	member bundle.Member
	bytes  []byte
}

type pdfReportFixture struct {
	root      string
	config    config.Config
	profile   string
	originals []pdfReportOriginal
	request   report.Request
}

// Seed retained authority before daemon ownership. No PDF provider is involved.
func newPDFReportFixture(t *testing.T) *pdfReportFixture {
	t.Helper()
	f := &pdfReportFixture{root: t.TempDir(), config: config.Default()}
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
	catalog, err := store.Open(filepath.Join(f.root, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(f.root, "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, blobs.Close()) }()
	for _, source := range []struct{ name, text string }{
		{"alpha.pdf", "Alpha. Document dated 2024-05-06."},
		{"beta.pdf", "Beta. Document dated 2024-05-06. Document dated 2024-06-07."},
		{"missing.pdf", "Alpha beta."},
		{"excluded.pdf", "Alpha beta. Document dated 2024-05-06."},
	} {
		content := reportPDF(t, source.text)
		digest := sha256HexBytes(content)
		receipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(content))
		require.NoError(t, err)
		require.Equal(t, digest, receipt.Hash)
		encoding, err := receipt.EncodingName()
		require.NoError(t, err)
		written, err := catalog.CreateFileWithReceipt(t.Context(), catalog.RootID(), source.name,
			digest, int64(len(content)), "application/pdf", store.BlobPhysical{
				Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible,
				MD5: receipt.MD5, Created: receipt.Created,
			})
		require.NoError(t, err)
		node := written.Node
		f.originals = append(f.originals, pdfReportOriginal{bytes: content, member: bundle.Member{
			NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: digest, Size: int64(len(content)),
		}})
		if source.name != "missing.pdf" {
			f.publish(t, catalog, blobs, node, source.text)
		}
	}
	f.request = report.Request{Version: 1, Profile: "archive", Timezone: "UTC",
		CoverageMode: "available_only", SelectedDocuments: &report.SelectedDocuments{}}
	for _, source := range f.originals[:3] {
		f.request.SelectedDocuments.Documents = append(f.request.SelectedDocuments.Documents,
			report.Identity{NodeID: source.member.NodeID, VersionID: source.member.VersionID,
				SHA256: source.member.SHA256})
	}
	for i, term := range []string{"alpha", "beta"} {
		f.request.Terms = append(f.request.Terms, report.Term{Number: i + 1, Expression: term,
			Syntax: "simple", Dates: report.DateRange{Start: "2000-01-01", End: "2099-12-31"}})
	}
	return f
}

func reportPDF(t *testing.T, text string) []byte {
	t.Helper()
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.AddPage()
	pdf.SetFont("Helvetica", "", 12)
	pdf.MultiCell(500, 18, text, "", "L", false)
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	// Avoid pdfcpu's host configuration and font directory.
	pages, err := pdfapi.PageCount(bytes.NewReader(output.Bytes()), &model.Configuration{
		Reader15: true, ValidationMode: model.ValidationRelaxed, Offline: true,
		Limits: model.DefaultResourceLimits(),
	})
	require.NoError(t, err)
	require.Equal(t, 1, pages)
	return output.Bytes()
}

func (f *pdfReportFixture) start(t *testing.T, root string) func() {
	t.Helper()
	path := filepath.Join(root, "config.toml")
	prior, err := os.ReadFile(path)
	if !os.IsNotExist(err) {
		require.NoError(t, err)
	}
	var encoded bytes.Buffer
	encoded.Write(prior) // Preserve storage bindings created by restore.
	encoded.WriteByte('\n')
	require.NoError(t, toml.NewEncoder(&encoded).Encode(map[string]any{
		"rendition_profiles":  f.config.RenditionProfiles,
		"retrieval_profiles":  f.config.RetrievalProfiles,
		"processing_profiles": f.config.ProcessingProfiles,
	}))
	require.NoError(t, os.WriteFile(path, encoded.Bytes(), 0o600))
	loaded, err := config.Load(root)
	require.NoError(t, err)
	profile, err := loaded.ProcessingProfile("archive")
	require.NoError(t, err)
	_, fingerprints, err := document.CanonicalProfile(profile.Document)
	require.NoError(t, err)
	require.Equal(t, f.profile, fingerprints.Profile)
	t.Setenv("DOCBANK_HOME", root)
	stop := startServe(t)
	waitForDaemon(t, root)
	return stop
}

func (f *pdfReportFixture) publish(
	t *testing.T, catalog *store.Store, blobs *blob.Store, node store.Node, text string,
) {
	t.Helper()
	resolved, err := f.config.ProcessingProfile("archive")
	require.NoError(t, err)
	canonical, fingerprints, err := document.CanonicalProfile(resolved.Document)
	require.NoError(t, err)
	f.profile = fingerprints.Profile
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
	normalized, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: text,
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}},
	}, evidencePolicy)
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
