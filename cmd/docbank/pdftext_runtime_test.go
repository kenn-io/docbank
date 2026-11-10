package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/pdftext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	vaulthome "go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

func pdfTextRuntimeConfig(t *testing.T) config.Config {
	t.Helper()
	p, err := pdftext.New(pdftext.Profile{MaxDocumentBytes: 64 << 20, MaxPages: 1000})
	require.NoError(t, err)
	fingerprint, err := plaintextProviderForDaemonTest()
	require.NoError(t, err)
	cfg := plaintextProcessingConfig(fingerprint)
	binding := cfg.RenditionProfiles["plaintext"]
	binding.AdapterContract = pdftext.AdapterContract
	binding.DescriptorID = p.Descriptor().ID
	binding.DescriptorFingerprint = p.Descriptor().Fingerprint
	binding.MaxDocumentBytes = 64 << 20
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

func isolatedPDFTextRuntime(t *testing.T) bool {
	t.Helper()
	const worker = "DOCBANK_TEST_PDFTEXT_RUNTIME_WORKER"
	if os.Getenv(worker) == t.Name() {
		return true
	}
	// PDFium qualifies whole-process RSS; earlier daemon heaps aren't its workload.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v") //nolint:gosec // Runs this test binary with the current test's name.
	command.Env = append(os.Environ(), worker+"="+t.Name())
	output, err := command.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err)
	return false
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
	if !isolatedPDFTextRuntime(t) {
		return
	}
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
	}{{"born", 3, 3}, {"image-only", 3, 0}} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.name+".pdf")
			data := mediatest.PDFTextPages(pdfTextRuntimePages(test.pages, test.text), false)
			query := "needle"
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
			if test.text > 0 {
				require.Contains(t, string(body), query)
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
	if !isolatedPDFTextRuntime(t) {
		return
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
		{"large.pdf", mediatest.PDFTextScanPages(pdfTextRuntimePages(500, 500))},
		{"redact.pdf", redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, "preparation")},
		{"ordinary.txt", []byte("needle plain text stays responsive")},
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

	plain, err := daemon.API().ResolvePath(ctx, &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/concurrent/ordinary.txt"}})
	require.NoError(t, err)
	plainSelector := api.ProcessingSelector{NodeID: plain.ID, ContentVersionID: plain.CurrentVersionID, Profile: "private-text"}
	plainPlan, err := daemon.API().PlanDocumentProcessing(ctx, &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: plainSelector}})
	require.NoError(t, err)
	plainJob, err := daemon.StartProcessing(ctx, api.StartProcessingRequest{Selector: plainSelector, PlanFingerprint: plainPlan.Fingerprint, Consent: true}, plainPlan.ProfileFingerprint)
	require.NoError(t, err)
	plainStatus, err := daemon.ProcessingStatus(ctx, plainJob.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", plainStatus.State)
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

func TestDaemonAutomaticPDFText(t *testing.T) {
	if !isolatedPDFTextRuntime(t) {
		return
	}
	for _, test := range []struct {
		name     string
		existing bool
	}{
		{name: "import"}, {name: "existing vault", existing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(t.TempDir(), "source.pdf")
			data := mediatest.PDFTextPages([]string{"first page", "distinctivequartz on page two"}, false)
			if test.existing {
				layout := vaulthome.Layout{Root: home}
				require.NoError(t, layout.Ensure())
				catalog, err := store.Open(t.Context(), layout.DBPath())
				require.NoError(t, err)
				blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
				require.NoError(t, err)
				written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
				require.NoError(t, err)
				encoding, err := written.EncodingName()
				require.NoError(t, err)
				directory, err := catalog.Mkdir(t.Context(), catalog.RootID(), "pdfs")
				require.NoError(t, err)
				_, err = catalog.CreateFile(t.Context(), directory.ID, "source.pdf", written.Hash, written.Size, "application/pdf", store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize})
				require.NoError(t, err)
				require.NoError(t, blobs.Close())
				require.NoError(t, catalog.Close())
			}
			t.Setenv("DOCBANK_HOME", home)
			stop := startServe(t)
			defer stop()
			waitForDaemon(t, home)
			require.NoError(t, os.WriteFile(path, data, 0600))
			var deadline context.Context
			if !test.existing {
				settleDaemon(t)
				var cancel context.CancelFunc
				deadline, cancel = context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				_, err := runCLI(t, "document", "add", path, "--dest", "/pdfs")
				require.NoError(t, err)
			}
			if test.existing {
				settleDaemon(t)
			} else {
				value, ok := testDaemons.Load(home)
				require.True(t, ok)
				daemon, ok := value.(serveReady)
				require.True(t, ok)
				require.NoError(t, daemon.jobs.Settle(deadline))
			}
			out, err := runCLI(t, "search", "distinctivequartz", "--mode", "lexical")
			require.NoError(t, err)
			require.Contains(t, out, "/pdfs/source.pdf")
			if deadline != nil {
				require.NoError(t, deadline.Err(), "import and search exceeded ten seconds")
			}
			out, err = runCLI(t, "rendition", "window", "/pdfs/source.pdf")
			require.NoError(t, err)
			require.Contains(t, out, "distinctivequartz on page two")
		})
	}
}

func TestDaemonWindowRetainedPDFProfiles(t *testing.T) {
	if !isolatedPDFTextRuntime(t) {
		return
	}
	fixture := newReportRenditionFixture(t)
	home := t.TempDir()
	layout := vaulthome.Layout{Root: home}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(t.Context(), layout.DBPath())
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
	require.NoError(t, err)
	data := mediatest.PDFTextPages([]string{"native distinctive text"}, false)
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "text.pdf", written.Hash, written.Size, "application/pdf", store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize})
	require.NoError(t, err)
	fixture.publish(t, catalog, blobs, node, document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete, Family: "pdf", UnitKind: document.EvidenceUnitPage, Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: "retained distinctive text", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}}})
	require.NoError(t, blobs.Close())
	require.NoError(t, catalog.Close())
	writeConfig := func() {
		var data bytes.Buffer
		require.NoError(t, toml.NewEncoder(&data).Encode(map[string]any{"rendition_profiles": fixture.config.RenditionProfiles, "retrieval_profiles": fixture.config.RetrievalProfiles, "processing_profiles": fixture.config.ProcessingProfiles}))
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), data.Bytes(), 0600))
	}
	writeConfig()
	t.Setenv("DOCBANK_HOME", home)
	stop := startServe(t)
	waitForDaemon(t, home)
	settleDaemon(t)
	id := fmt.Sprintf("id:%d", node.ID)
	output, err := runCLI(t, "rendition", "window", id, "--profile", "archive")
	require.NoError(t, err)
	require.Contains(t, output, "retained distinctive text")
	first, err := runCLI(t, "rendition", "window", id, "--profile", "archive", "--max-chars", "16")
	require.NoError(t, err)
	parts := strings.Split(first, "Next window: ")
	require.Len(t, parts, 2)
	continuation := strings.Fields(parts[1])
	require.Equal(t, "docbank", continuation[0])
	continuation = append(continuation[1:], "--json")
	expectedJSON, err := runCLI(t, continuation...)
	require.NoError(t, err)
	var expected api.RenditionTextWindow
	require.NoError(t, json.Unmarshal([]byte(expectedJSON), &expected))
	require.Positive(t, expected.ActualStart)
	require.NotEmpty(t, expected.Text)
	stop()
	delete(fixture.config.ProcessingProfiles, "archive")
	writeConfig()
	stop = startServe(t)
	defer stop()
	waitForDaemon(t, home)
	continuedJSON, err := runCLI(t, continuation...)
	require.NoError(t, err)
	var continued api.RenditionTextWindow
	require.NoError(t, json.Unmarshal([]byte(continuedJSON), &continued))
	require.Equal(t, expected.Text, continued.Text)
	require.Equal(t, expected.ActualStart, continued.ActualStart)
	require.Equal(t, expected.AttachmentID, continued.AttachmentID)
}
