package api_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func renditionTextConfig(d *api.Deps) {
	d.Cfg.RenditionProfiles = map[string]config.RenditionProfileConfig{"primary": {
		AdapterContract: "adapter/v1", AuthorizationFingerprint: testHash("authorization"),
		CredentialBinding: "credential:synthetic", DeploymentFingerprint: testHash("deployment"),
		DescriptorID: "synthetic-pdf", DescriptorFingerprint: testHash("descriptor"),
		DisclosureFingerprint: testHash("disclosure"), MaxDocumentBytes: 1 << 20,
		MaxResponseBytes: 1 << 20, MaxUnits: 10,
		RequestedArtifacts: []string{"structured_evidence"}, TrustBoundary: "synthetic-vault",
		UploadOptionsFingerprint: testHash("upload-options"),
	}}
	d.Cfg.RetrievalProfiles = map[string]config.RetrievalProfileConfig{"local": {LexicalLimit: 10, VectorLimit: 10}}
	d.Cfg.ProcessingProfiles = map[string]config.ProcessingProfileConfig{"archive": {
		Rendition: "primary", Retrieval: "local", AttachmentPolicyFingerprint: testHash("attachments"),
		CompletenessFingerprint: testHash("completeness"), ConsentFingerprint: testHash("consent"),
		LexicalSegmenterFingerprint: testHash("segments"), MaxDocumentChars: 100_000,
		MaxSegmentRunes: 100, MaxUnitRunes: 1_000, NormalizerFingerprint: testHash("normalizer"),
		RetainSanitizedMarkdown: true, SanitizerFingerprint: testHash("sanitizer"), TrustBoundary: "synthetic-vault",
	}}
}

type renditionTextFixture struct {
	node       store.Node
	profile    store.ProcessingProfileRecord
	generation string
	attachment string
	build      string
	artifact   string
	markdown   []byte
}

func publishRenditionTextFixture(t *testing.T, s *testStore, cfg config.Config) renditionTextFixture {
	t.Helper()
	source := []byte("%PDF-1.4 synthetic source")
	sourceHash, sourceSize, err := s.Blobs.Write(bytes.NewReader(source))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.pdf", sourceHash, sourceSize, "application/pdf")
	require.NoError(t, err)
	resolved, err := cfg.ProcessingProfile("archive")
	require.NoError(t, err)
	canonicalProfile, fingerprints, err := document.CanonicalProfile(resolved.Document)
	require.NoError(t, err)
	profile := store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonicalProfile),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    resolved.Document.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             resolved.Document.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: resolved.Document.Rendition.DisclosureFingerprint,
		TrustBoundary:                  resolved.Document.RetentionDisclosure.TrustBoundary,
	}
	evidencePolicy, renditionPolicy, err := document.RenditionExecutionPoliciesForProfileV1(resolved.Document)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, Text: "Verified alpha <script>alert('blocked')</script>",
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1},
		}},
	}, evidencePolicy)
	require.NoError(t, err)
	evidenceBytes, evidenceHash, err := document.MarshalNormalizedEvidenceV1(normalized)
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(normalized, renditionPolicy)
	require.NoError(t, err)
	policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	units := make([]store.RenditionUnitRecord, len(rendition.Units))
	for index, unit := range rendition.Units {
		units[index] = store.RenditionUnitRecord{ID: unit.ID, EvidenceUnitID: unit.EvidenceUnitID,
			Order: unit.Order, Checksum: unit.Checksum, HeadingPath: unit.HeadingPath, Locator: unit.Locator}
	}
	segments := make([]store.RenditionLexicalSegmentRecord, len(rendition.LexicalSegments))
	for index, segment := range rendition.LexicalSegments {
		segments[index] = store.RenditionLexicalSegmentRecord{ID: segment.ID, UnitID: segment.UnitID,
			Order: segment.Order, CharStart: segment.CharStart, CharEnd: segment.CharEnd,
			Checksum: segment.Checksum, Text: segment.Text}
	}
	buildID := testHash("rendition-build")
	attachmentID := testHash("rendition-attachment")
	artifactID := "artifact_" + testHash("markdown-artifact")
	build := store.RenditionBuildRecord{
		ID: buildID, VaultID: s.VaultID(), SourceSHA256: sourceHash,
		RenditionRequestFingerprint:       profile.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: testHash(string(policy)), CapturedArtifactPolicy: policy,
		AuthorizationChecksum: testHash("publication-authorization"), ProviderOperationID: "synthetic-pdf-operation",
		ProviderReceipt:  jsontext.Value(`{"provider":"synthetic","request_id":"db17"}`),
		EvidenceChecksum: evidenceHash, RenditionChecksum: rendition.Checksum,
		MarkdownChecksum: rendition.MarkdownChecksum, Completeness: document.EvidenceComplete,
		CompletedAt: "2026-09-11T12:00:00.000000000Z", DeclaredArtifactCount: 2,
		Artifacts: []store.RenditionArtifactRecord{
			{ID: "artifact_" + testHash("evidence-artifact"), Role: "normalized_evidence", BlobHash: evidenceHash,
				Size: int64(len(evidenceBytes)), Checksum: evidenceHash, State: store.RenditionArtifactVerified},
			{ID: artifactID, Role: "sanitized_markdown", BlobHash: rendition.MarkdownChecksum,
				Size: int64(len(rendition.Markdown)), Checksum: rendition.MarkdownChecksum, State: store.RenditionArtifactVerified},
		},
		Units: units, LexicalSegments: segments,
	}
	attachment := store.RenditionAttachmentRecord{ID: attachmentID, VaultID: s.VaultID(),
		ContentVersionID: node.CurrentVersionID, BuildID: buildID, Profile: profile,
		AttachedAt: "2026-09-11T12:01:00.000000000Z"}
	generation := testHash("rendition-generation")
	publisher, err := processing.NewArtifactPublisher(s.Store, s.Blobs)
	require.NoError(t, err)
	_, err = publisher.PublishRendition(t.Context(), processing.StagedRendition{
		Rendition: rendition, RenditionPolicy: renditionPolicy, Build: build, Attachment: attachment,
		Head: store.RenditionHeadRecord{ContentVersionID: node.CurrentVersionID,
			ProcessingProfileFingerprint: profile.Fingerprint, AttachmentID: attachmentID,
			PublishedAt: "2026-09-11T12:02:00.000000000Z"},
		LexicalGenerationID: generation,
		Artifacts: []processing.StagedArtifact{
			{ID: build.Artifacts[0].ID, Payload: bytes.NewReader(evidenceBytes)},
			{ID: artifactID, Payload: bytes.NewReader(rendition.Markdown)},
		},
	})
	require.NoError(t, err)
	return renditionTextFixture{node: node, profile: profile, generation: generation,
		attachment: attachmentID, build: buildID, artifact: artifactID, markdown: rendition.Markdown}
}

func TestRenditionTextHTTPResolvesAndStreamsOneVerifiedExactArtifact(t *testing.T) {
	var cfg config.Config
	ts, s := newTestServer(t, func(d *api.Deps) { renditionTextConfig(d); cfg = d.Cfg })
	fixture := publishRenditionTextFixture(t, s, cfg)
	body := map[string]any{
		"node_id": fixture.node.ID, "revision": fixture.node.Revision,
		"version_id": fixture.node.CurrentVersionID, "blob_hash": fixture.node.BlobHash,
		"size": fixture.node.Size, "profile": "archive",
		"observed": map[string]any{
			"configuration": "configured", "profile_fingerprint": fixture.profile.Fingerprint,
			"generation_id": fixture.generation, "coverage_state": "complete",
			"attachment_id": fixture.attachment, "build_id": fixture.build,
		},
	}
	resp, encoded := do(t, ts, http.MethodPost, "/api/v1/renditions/text", nil, body)
	require.Equal(t, http.StatusOK, resp.StatusCode, encoded)
	var receipt struct {
		State    string `json:"state"`
		Artifact struct {
			ID     string `json:"id"`
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"artifact"`
		AttachmentID string `json:"attachment_id"`
		BuildID      string `json:"build_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(encoded), &receipt))
	require.Equal(t, "ready", receipt.State)
	require.Equal(t, fixture.artifact, receipt.Artifact.ID)
	require.Equal(t, fixture.attachment, receipt.AttachmentID)
	require.Equal(t, fixture.build, receipt.BuildID)

	params := url.Values{
		"node_id": {strconv.FormatInt(fixture.node.ID, 10)}, "revision": {strconv.FormatInt(fixture.node.Revision, 10)},
		"version_id": {fixture.node.CurrentVersionID}, "blob_hash": {fixture.node.BlobHash},
		"size": {strconv.FormatInt(fixture.node.Size, 10)}, "profile_fingerprint": {fixture.profile.Fingerprint},
		"generation_id": {fixture.generation}, "attachment_id": {fixture.attachment},
		"build_id": {fixture.build}, "artifact_id": {fixture.artifact},
	}
	contentResp, content := get(t, ts, "/api/v1/renditions/text/content?"+params.Encode(), nil)
	require.Equal(t, http.StatusOK, contentResp.StatusCode, content)
	require.Equal(t, fixture.markdown, []byte(content))
	require.Equal(t, receipt.Artifact.SHA256, contentResp.Header.Get("X-Docbank-Rendition-Sha256"))
	require.Equal(t, base64.StdEncoding.EncodeToString(mustDecodeTestHash(t, receipt.Artifact.SHA256)),
		strings.TrimSuffix(strings.TrimPrefix(contentResp.Header.Get("Content-Digest"), "sha-256=:"), ":"))
	require.NotContains(t, contentResp.Header.Get("Content-Type"), "html")

	params.Set("artifact_id", "artifact_"+testHash("wrong-artifact"))
	badResp, badBody := get(t, ts, "/api/v1/renditions/text/content?"+params.Encode(), nil)
	assert.Equal(t, http.StatusConflict, badResp.StatusCode, badBody)
}

func TestRenditionTextHTTPKeepsUnavailableStatesDistinct(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node := createFileWithContent(t, ts, s, "/notes.txt", "eligible original text")
	base := map[string]any{"node_id": node.ID, "revision": node.Revision,
		"version_id": node.CurrentVersionID, "blob_hash": node.BlobHash, "size": node.Size}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/renditions/text", nil, base)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, `"state":"unconfigured"`)

	var configured config.Config
	tsConfigured, configuredStore := newTestServer(t, func(d *api.Deps) {
		renditionTextConfig(d)
		configured = d.Cfg
	})
	pdfHash, pdfSize, err := configuredStore.Blobs.Write(strings.NewReader("%PDF unprocessed"))
	require.NoError(t, err)
	pdf, err := configuredStore.CreateFile(t.Context(), configuredStore.RootID(), "unprocessed.pdf", pdfHash, pdfSize, "application/pdf")
	require.NoError(t, err)
	request := map[string]any{"node_id": pdf.ID, "revision": pdf.Revision,
		"version_id": pdf.CurrentVersionID, "blob_hash": pdf.BlobHash, "size": pdf.Size,
		"profile": "archive"}
	for state, observed := range map[string]map[string]any{
		"failed":      {"configuration": "configured", "coverage_state": "failed"},
		"unprocessed": {"configuration": "configured", "coverage_state": "unprocessed"},
		"historical_unavailable": {"configuration": "configured", "coverage_state": "complete",
			"attachment_id": testHash("gone-attachment"), "build_id": testHash("gone-build")},
	} {
		resolved, err := configured.ProcessingProfile("archive")
		require.NoError(t, err)
		_, fingerprints, err := document.CanonicalProfile(resolved.Document)
		require.NoError(t, err)
		observed["profile_fingerprint"] = fingerprints.Profile
		request["observed"] = observed
		resp, body = do(t, tsConfigured, http.MethodPost, "/api/v1/renditions/text", nil, request)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		require.Contains(t, body, `"state":"`+state+`"`)
	}
}

func mustDecodeTestHash(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	require.NoError(t, err)
	require.Len(t, decoded, 32)
	return decoded
}
