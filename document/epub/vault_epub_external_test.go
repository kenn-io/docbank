package epub_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/epub"
)

func TestEmbeddedEPUBPlanRunReadAndSearch(t *testing.T) {
	for _, mode := range []string{"ordinary", "over budget"} {
		t.Run(mode, func(t *testing.T) {
			maxUnits := int64(3)
			if mode == "over budget" {
				maxUnits = 2
			}
			provider, err := epub.New(epub.Profile{MaxDocumentBytes: 1 << 20, MaxUnits: maxUnits})
			require.NoError(t, err)
			profile := epubProcessingProfile(t, provider.Descriptor())
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root, Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
				"epub": {Profile: profile, RenditionProvider: provider},
			}}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			data := epubBytes(t, nil)
			receipt, err := vault.Put(t.Context(), "/book.epub", bytes.NewReader(data), docbank.PutOptions{MediaType: "application/epub+zip"})
			require.NoError(t, err)
			selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "epub"}
			request := docbank.ProcessingPlanRequest{Selector: selector}
			plan, err := vault.PlanProcessing(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, "local_process", plan.Flow[0].TrustBoundary)
			require.Equal(t, "in-process", plan.Flow[0].RuntimeDisclosure.Endpoint)
			require.True(t, plan.ConsentRequired)
			job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
			if mode == "ordinary" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, docbank.ErrRenditionFailed)
			}
			var status docbank.ProcessingStatus
			if mode == "ordinary" {
				status, err = vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
				require.NoError(t, err)
			}
			fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
			if mode != "ordinary" {
				require.Empty(t, job.ID)
				_, err := vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: selector})
				require.Error(t, err)
				report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "needle", Mode: docbank.DocumentSearchLexical, Profile: "epub", Fence: fence})
				require.NoError(t, err)
				require.Empty(t, report.Results)
				t.Log("ErrRenditionFailed; no retained rendition or rejected-content search hit")
				return
			}
			require.Equalf(t, "completed", status.State, "%+v", status)
			rendition, err := vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: selector})
			require.NoError(t, err)
			body, err := io.ReadAll(rendition.Reader)
			require.NoError(t, err)
			require.NoError(t, rendition.Reader.Close())
			require.Contains(t, string(body), "beta\n\n---\n\nalpha needle\n\n---\n\nbeta")
			require.NotContains(t, string(body), "hidden metadata")
			require.NotContains(t, string(body), "omitted nonspine")
			report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "needle", Mode: docbank.DocumentSearchLexical, Profile: "epub", Fence: fence})
			require.NoError(t, err)
			require.Len(t, report.Results, 1)
			require.Equal(t, receipt.Version.ID, report.Results[0].ContentVersionID)
			t.Log("local plan, ordered repeated spine rendition, retained read and lexical hit observed; reader closed; vault cleanup registered")
		})
	}
}

func TestEmbeddedEPUBVirtualBoundaryThroughPublicAPI(t *testing.T) {
	for _, test := range []struct {
		name, text string
		reject     bool
	}{
		{"exact 80 by 48", strings.Repeat("界", 80*48), false},
		{"next virtual unit", strings.Repeat("界", 80*48+1), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, err := epub.New(epub.Profile{MaxDocumentBytes: 1 << 20, MaxUnits: 1})
			require.NoError(t, err)
			profile := epubProcessingProfile(t, provider.Descriptor())
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root, Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
				"epub": {Profile: profile, RenditionProvider: provider},
			}}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			data := epubBytes(t, map[string]string{
				"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="a"/></spine></package>`,
				"OPS/a.xhtml":  `<html xmlns="http://www.w3.org/1999/xhtml"><body><p>` + test.text + `</p></body></html>`,
			})
			receipt, err := vault.Put(t.Context(), "/book.epub", bytes.NewReader(data), docbank.PutOptions{MediaType: "application/epub+zip"})
			require.NoError(t, err)
			selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "epub"}
			plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
			require.NoError(t, err)
			job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true})
			if test.reject {
				require.ErrorIs(t, err, docbank.ErrRenditionFailed)
				require.Empty(t, job.ID)
				return
			}
			require.NoError(t, err)
			status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
			require.NoError(t, err)
			require.Equal(t, "completed", status.State)
			rendition, err := vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: selector})
			require.NoError(t, err)
			body, err := io.ReadAll(rendition.Reader)
			require.NoError(t, err)
			require.NoError(t, rendition.Reader.Close())
			require.Contains(t, string(body), test.text)
		})
	}
}

func epubBytes(t *testing.T, overrides map[string]string) []byte {
	t.Helper()
	entries := map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OPS/book.opf"/></rootfiles></container>`,
		"OPS/book.opf":           `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/><item id="b" href="b.xhtml" media-type="application/xhtml+xml"/><item id="unused" href="unused.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="b"/><itemref idref="a"/><itemref idref="b" linear="no"/></spine></package>`,
		"OPS/a.xhtml":            `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>hidden metadata</title></head><body><p>alpha needle</p></body></html>`,
		"OPS/b.xhtml":            `<html xmlns="http://www.w3.org/1999/xhtml"><body><script/><p>beta</p></body></html>`,
		"OPS/unused.xhtml":       `<html xmlns="http://www.w3.org/1999/xhtml"><body>omitted nonspine</body></html>`,
	}
	for name, body := range overrides {
		if body == "" {
			delete(entries, name)
		} else {
			entries[name] = body
		}
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range entries {
		file, err := writer.Create(name)
		require.NoError(t, err)
		_, err = file.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func epubProcessingProfile(t *testing.T, descriptor document.RenditionDescriptor) document.ProcessingProfileV1 {
	t.Helper()
	hash := func(value string) string {
		digest := sha256.Sum256([]byte(value))
		return hex.EncodeToString(digest[:])
	}
	return document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "epub.in-process/v1", AuthorizationFingerprint: hash("authorization"),
			CredentialBinding: "credential:none", DeploymentFingerprint: hash("deployment"),
			Descriptor:            document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			DisclosureFingerprint: hash("rendition-disclosure"), MaxDocumentBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, MaxUnits: 1000, Name: "epub",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      string(descriptor.TrustBoundary), UploadOptionsFingerprint: hash("upload"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: hash("completeness"), LexicalSegmenterFingerprint: hash("segments"),
			MaxDocumentChars: 1_000_000, MaxSegmentRunes: 1000, MaxUnitRunes: 100_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      hash("normalizer"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: hash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: hash("attachment"), ConsentFingerprint: hash("consent"),
			RetainSanitizedMarkdown: true, TrustBoundary: string(descriptor.TrustBoundary),
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 50, VectorLimit: 50},
	}
}
