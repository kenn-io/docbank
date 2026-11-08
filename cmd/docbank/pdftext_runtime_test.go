package main

import (
	"bytes"
	"context"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/pdftext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

func pdfTextRuntimeConfig(t *testing.T) config.Config {
	t.Helper()
	p, err := pdftext.New(pdftext.Profile{MaxDocumentBytes: 16 << 20, MaxPages: 1000})
	require.NoError(t, err)
	fingerprint, err := plaintextProviderForDaemonTest()
	require.NoError(t, err)
	cfg := plaintextProcessingConfig(fingerprint)
	binding := cfg.RenditionProfiles["plaintext"]
	binding.AdapterContract = pdftext.AdapterContract
	binding.DescriptorID = p.Descriptor().ID
	binding.DescriptorFingerprint = p.Descriptor().Fingerprint
	binding.MaxDocumentBytes = 16 << 20
	binding.MaxUnits = 1000
	cfg.RenditionProfiles["pdf"] = binding
	profile := cfg.ProcessingProfiles["private-text"]
	profile.Rendition = "pdf"
	cfg.ProcessingProfiles["pdf"] = profile
	return cfg
}

func pdfTextRuntimePDF(pages, textPages int) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	var kids []string
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", i+3))
		contents := fmt.Sprintf(" /Contents %d 0 R", pages+5)
		if i < textPages {
			contents = fmt.Sprintf(" /Contents %d 0 R", pages+4)
		}
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R >> /XObject << /Photo %d 0 R >> >>%s >>", pages+3, pages+6, contents))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages)
	content := "q 8 0 0 8 72 650 cm /Photo Do Q BT /F1 12 Tf 72 720 Td (needle synthetic PDF text) Tj ET"
	objects = append(objects, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	imageContent := "q 8 0 0 8 72 650 cm /Photo Do Q"
	image := mediatest.JPEG(8, 8, color.White)
	objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(imageContent), imageContent), fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 8 /Height 8 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", len(image), image))
	return mediatest.PDFObjects("local PDF text", objects)
}

func TestPDFTextRuntimeDescriptorAndLimits(t *testing.T) {
	for _, field := range []string{"descriptor", "trust", "bytes", "pages"} {
		t.Run(field, func(t *testing.T) {
			cfg := pdfTextRuntimeConfig(t)
			_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
			require.NoError(t, err)
			binding := cfg.RenditionProfiles["pdf"]
			switch field {
			case "descriptor":
				binding.DescriptorFingerprint = strings.Repeat("0", 64)
			case "trust":
				binding.TrustBoundary = "operator_network"
			case "bytes":
				binding.MaxDocumentBytes--
			case "pages":
				binding.MaxUnits--
			}
			cfg.RenditionProfiles["pdf"] = binding
			_, err = executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
			require.ErrorContains(t, err, "descriptor differs")
		})
	}
}

func TestDaemonPDFTextProcessing(t *testing.T) {
	cfg := pdfTextRuntimeConfig(t)
	cfg.Server.APIKey = "synthetic-pdftext-key"
	home := t.TempDir()
	require.NoError(t, writeDaemonASRConfig(home, cfg))
	t.Setenv("DOCBANK_HOME", home)
	startServe(t)
	runtime := waitForDaemon(t, home)
	daemon := daemonconn.New("http://"+runtime.Address, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })
	for _, test := range []struct {
		name        string
		pages, text int
		mediaType   string
	}{{"born", 3, 3, ""}, {"image-only", 3, 0, ""}, {"mixed", 3, 1, ""}, {"parameterized", 3, 0, "application/pdf; version=1.7"}} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.name+".pdf")
			require.NoError(t, os.WriteFile(path, pdfTextRuntimePDF(test.pages, test.text), 0600))
			_, err := runCLI(t, "add", path, "--dest", "/pdfs")
			require.NoError(t, err)
			node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/pdfs/" + test.name + ".pdf"}})
			require.NoError(t, err)
			if test.mediaType != "" {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				replacement, err := daemon.ReplaceContent(t.Context(), node.ID, node.Revision, test.mediaType, node.BlobHash, node.Size, bytes.NewReader(data))
				require.NoError(t, err)
				node = &replacement.Node
				require.Equal(t, test.mediaType, node.MimeType)
			}
			selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "pdf"}
			plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
			require.NoError(t, err)
			require.Equal(t, "local_process", plan.Flow[0].TrustBoundary)
			job, err := daemon.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
			require.NoError(t, err)
			status, err := daemon.ProcessingStatus(t.Context(), job.ID)
			require.NoError(t, err)
			require.Equal(t, "completed", status.State)
			stream, err := daemon.RenditionForSelector(t.Context(), selector, 1<<20)
			require.NoError(t, err)
			var retained bytes.Buffer
			_, err = stream.CopyVerified(&retained)
			require.NoError(t, err)
			require.NoError(t, stream.Close())
			header, body, err := document.ParseRenditionFrontMatterV1(retained.Bytes())
			require.NoError(t, err)
			require.Equal(t, test.text, header.Document.UnitCount)
			if test.text == 0 {
				require.Empty(t, body)
				versionID, err := uuid.Parse(node.CurrentVersionID)
				require.NoError(t, err)
				receipt, err := daemon.API().ResolveRenditionText(t.Context(), &apiclient.ResolveRenditionTextRequestOptions{Body: &apiclient.ResolveRenditionTextBody{NodeID: node.ID, Revision: node.Revision, VersionID: versionID, BlobHash: node.BlobHash, Size: node.Size, Profile: new("pdf")}})
				require.NoError(t, err)
				require.Equal(t, "verified_empty", string(receipt.State))
			} else {
				require.Contains(t, string(body), "needle synthetic PDF text")
			}
			search, err := daemon.SearchDocuments(t.Context(), api.DocumentSearchRequest{Query: "needle", Mode: "lexical", Profile: "pdf", Limit: 10, Fence: api.DocumentSourceFence{VaultUID: plan.VaultUID, ContentVersionIDs: []string{selector.ContentVersionID}}})
			require.NoError(t, err)
			if test.text == 0 {
				require.Empty(t, search.Results)
			} else {
				require.Len(t, search.Results, 1)
				require.Equal(t, selector.ContentVersionID, search.Results[0].ContentVersionID)
				require.NotEmpty(t, search.Results[0].Evidence)
				for _, entry := range header.Navigation.Entries {
					require.Equal(t, document.EvidenceLocatorPage, entry.Kind)
				}
			}
		})
	}
}

func TestDaemonPDFTextSharesEngineWithRedaction(t *testing.T) {
	if !pagerender.SupportedPlatform() {
		t.Skip("page runtime requires Linux amd64")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cfg := pdfTextRuntimeConfig(t)
	cfg.PageRuntime = new(redactiontest.PageProfile(t))
	cfg.Server.APIKey = "synthetic-pdftext-concurrency-key"
	home := t.TempDir()
	require.NoError(t, writeDaemonASRConfig(home, cfg))
	var pageConfig bytes.Buffer
	require.NoError(t, toml.NewEncoder(&pageConfig).Encode(map[string]any{"page_runtime": cfg.PageRuntime}))
	file, err := os.OpenFile(filepath.Join(home, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = file.Write(pageConfig.Bytes())
	require.NoError(t, err)
	require.NoError(t, file.Close())
	t.Setenv("DOCBANK_HOME", home)
	startServe(t)
	runtime := waitForDaemon(t, home)
	daemon := daemonconn.New("http://"+runtime.Address, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })
	for _, input := range []struct {
		name string
		data []byte
	}{
		{"large.pdf", pdfTextRuntimePDF(500, 500)},
		{"redact.pdf", redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, "preparation")},
	} {
		path := filepath.Join(t.TempDir(), input.name)
		require.NoError(t, os.WriteFile(path, input.data, 0600))
		_, err := runCLI(t, "add", path, "--dest", "/concurrent")
		require.NoError(t, err)
	}
	large, err := daemon.API().ResolvePath(ctx, &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/concurrent/large.pdf"}})
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: large.ID, ContentVersionID: large.CurrentVersionID, Profile: "pdf"}
	plan, err := daemon.API().PlanDocumentProcessing(ctx, &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	stream, err := daemon.StartProcessingStream(ctx, api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()
	first, err := stream.Next()
	require.NoError(t, err)
	require.NotNil(t, first.Job)
	plainPath := filepath.Join(t.TempDir(), "ordinary.txt")
	require.NoError(t, os.WriteFile(plainPath, []byte("needle plain text stays responsive"), 0600))
	_, err = runCLI(t, "add", plainPath, "--dest", "/concurrent")
	require.NoError(t, err)
	status, err := daemon.ProcessingStatus(ctx, first.Job.ID)
	require.NoError(t, err)
	require.Equal(t, "running", status.State)

	redactionNode, err := daemon.API().ResolvePath(ctx, &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/concurrent/redact.pdf"}})
	require.NoError(t, err)
	prepared, err := daemon.PrepareNativePDF(ctx, processing.RedactionPreparationRequest{Selection: store.PageBinding{NodeID: redactionNode.ID, Revision: redactionNode.Revision, Source: document.PageSource{VersionID: redactionNode.CurrentVersionID, SHA256: redactionNode.BlobHash, Size: redactionNode.Size}}})
	require.NoError(t, err)
	require.Equal(t, "SECRET KEEP SECRET", prepared.Map.Text)
	status, err = daemon.ProcessingStatus(ctx, first.Job.ID)
	require.NoError(t, err)
	require.Equal(t, "running", status.State)

	plain, err := daemon.API().ResolvePath(ctx, &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/concurrent/ordinary.txt"}})
	require.NoError(t, err)
	selector = api.ProcessingSelector{NodeID: plain.ID, ContentVersionID: plain.CurrentVersionID, Profile: "private-text"}
	plan, err = daemon.API().PlanDocumentProcessing(ctx, &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	_, err = daemon.StartProcessing(ctx, api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	_, err = stream.Next()
	require.NoError(t, err)
	status, err = daemon.ProcessingStatus(ctx, first.Job.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
}
