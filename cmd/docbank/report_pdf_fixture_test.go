package main

import (
	"bytes"
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
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

type pdfReportFixture struct {
	reportRenditionFixture

	root      string
	originals []reportOriginal
	request   report.Request
}

// Seed retained authority before daemon ownership. No PDF provider is involved.
func newPDFReportFixture(t *testing.T) *pdfReportFixture {
	t.Helper()
	f := &pdfReportFixture{root: t.TempDir(), reportRenditionFixture: newReportRenditionFixture(t)}
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
		f.originals = append(f.originals, reportOriginal{bytes: content, member: bundle.Member{
			NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: digest, Size: int64(len(content)),
		}})
		if source.name != "missing.pdf" {
			f.publish(t, catalog, blobs, node, document.SourceEvidenceV1{
				ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
				Family: "pdf", UnitKind: document.EvidenceUnitPage,
				Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: source.text,
					Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
						IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}},
			})
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
