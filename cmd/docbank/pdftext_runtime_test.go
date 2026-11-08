package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

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
	binding.RequestedArtifacts = []string{"structured_evidence"}
	cfg.RenditionProfiles["pdf"] = binding
	profile := cfg.ProcessingProfiles["private-text"]
	profile.Rendition = "pdf"
	cfg.ProcessingProfiles["pdf"] = profile
	return cfg
}

func pdfTextRuntimePages(pages, textPages int) []string {
	text := make([]string, pages)
	for index := range textPages {
		text[index] = "needle synthetic PDF text"
	}
	return text
}

func TestPDFTextRuntimeDescriptorAndLimits(t *testing.T) {
	for _, field := range []string{"bytes", "pages"} {
		t.Run(field, func(t *testing.T) {
			cfg := pdfTextRuntimeConfig(t)
			require.NoError(t, cfg.Validate())
			_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
			require.NoError(t, err)
			binding := cfg.RenditionProfiles["pdf"]
			switch field {
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
	}{{"born", 3, 3}, {"image-only", 3, 0}, {"wrapped", 1, 1}} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.name+".pdf")
			data := mediatest.PDFTextPages(pdfTextRuntimePages(test.pages, test.text), false)
			query := "needle"
			if test.name == "wrapped" {
				data = redactiontest.PDF(t, []string{"inter-", "national", "part-time"}, "wrapped word")
				query = "international"
			}
			require.NoError(t, os.WriteFile(path, data, 0600))
			_, err := runCLI(t, "add", path, "--dest", "/pdfs")
			require.NoError(t, err)
			node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/pdfs/" + test.name + ".pdf"}})
			require.NoError(t, err)
			selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "pdf"}
			plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
			require.NoError(t, err)
			require.Equal(t, "local_process", plan.Flow[0].TrustBoundary)
			job, err := daemon.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
			require.NoError(t, err)
			status, err := daemon.ProcessingStatus(t.Context(), job.ID)
			require.NoError(t, err)
			require.Equal(t, "completed", status.State)
			coverageJSON, err := runCLI(t, "processing", "coverage", selector.ContentVersionID, "--profile", "pdf", "--json")
			require.NoError(t, err)
			var coverage processingCoverageOutput
			require.NoError(t, json.Unmarshal([]byte(coverageJSON), &coverage))
			require.Equal(t, "complete", coverage.Coverage.Renditions.State)
			stream, err := daemon.RenditionForSelector(t.Context(), selector, 1<<20)
			require.NoError(t, err)
			var retained bytes.Buffer
			_, err = stream.CopyVerified(&retained)
			require.NoError(t, err)
			require.NoError(t, stream.Close())
			_, body, err := document.ParseRenditionFrontMatterV1(retained.Bytes())
			require.NoError(t, err)
			if test.text == 0 {
				window, err := daemon.RenditionTextWindow(t.Context(), api.RenditionWindowRequest{VaultID: plan.VaultUID, NodeID: node.ID, ContentVersionID: node.CurrentVersionID, AttachmentID: stream.AttachmentID, Offset: utf8.RuneCount(retained.Bytes()), MaxChars: 100})
				require.NoError(t, err)
				require.Empty(t, window.Text)
				require.True(t, window.EOF)
			} else {
				require.Contains(t, string(body), query)
			}
			if test.text > 0 {
				search, err := daemon.SearchDocuments(t.Context(), api.DocumentSearchRequest{Query: query, Mode: "lexical", Profile: "pdf", Limit: 10, Fence: api.DocumentSourceFence{VaultUID: plan.VaultUID, ContentVersionIDs: []string{selector.ContentVersionID}}})
				require.NoError(t, err)
				require.Len(t, search.Results, 1)
				require.Equal(t, selector.ContentVersionID, search.Results[0].ContentVersionID)
				require.NotEmpty(t, search.Results[0].Evidence)
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
		{"large.pdf", mediatest.PDFTextPages(pdfTextRuntimePages(500, 500), false)},
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
	started := time.Now()
	stream, err := daemon.StartProcessingStream(ctx, api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()
	first, err := stream.Next()
	require.NoError(t, err)
	require.NotNil(t, first.Job)
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

	_, err = stream.Next()
	require.NoError(t, err)
	status, err = daemon.ProcessingStatus(ctx, first.Job.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
	t.Logf("500-page PDF processing with concurrent redaction: %s", time.Since(started))
}
