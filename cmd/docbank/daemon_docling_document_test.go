package main

import (
	"archive/zip"
	"bytes"
	"encoding/json/v2"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
)

func doclingDocumentProcessingConfig(t *testing.T, endpoint string) (config.Config, document.RenditionDescriptor) {
	t.Helper()
	cfg, _ := doclingASRProcessingConfig(t, endpoint)
	descriptor, err := docling.DocumentDescriptor(document.RenditionTrustOperatorNetwork)
	require.NoError(t, err)
	profile := cfg.RenditionProfiles["asr"]
	profile.AdapterContract = config.DoclingDocumentAdapterContract
	profile.MaxTranscriptChars = 0
	profile.RequestedArtifacts = []string{string(document.EvidenceArtifactMarkdown), string(document.EvidenceArtifactStructured)}
	profile.DescriptorFingerprint = descriptor.Fingerprint
	profile.DisclosureFingerprint = docling.DocumentDisclosureFingerprint(descriptor, endpoint, profile.DeploymentFingerprint)
	delete(cfg.RenditionProfiles, "asr")
	cfg.RenditionProfiles["documents"] = profile
	processing := cfg.ProcessingProfiles["asr"]
	processing.Rendition = "documents"
	delete(cfg.ProcessingProfiles, "asr")
	cfg.ProcessingProfiles["documents"] = processing
	return cfg, descriptor
}

func doclingTestPDFPages(pageCount int) []byte {
	kids := make([]string, 0, pageCount)
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	for index := range pageCount {
		kids = append(kids, fmt.Sprintf("%d 0 R", index+3))
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 16 16] >>")
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pageCount)
	return mediatest.PDFObjects("synthetic page-bound test", objects)
}

type doclingTestZIPEntry struct{ name, body string }

func doclingTestZIP(t *testing.T, entries []doclingTestZIPEntry) []byte {
	t.Helper()
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	for _, entry := range entries {
		file, err := archive.Create(entry.name)
		require.NoError(t, err)
		_, err = file.Write([]byte(entry.body))
		require.NoError(t, err)
	}
	require.NoError(t, archive.Close())
	return data.Bytes()
}

func doclingTestPPTX(t *testing.T, slideCount int) []byte {
	t.Helper()
	entries := []doclingTestZIPEntry{
		{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/></Types>`},
		{name: "ppt/presentation.xml", body: `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`},
	}
	for index := 1; index <= slideCount; index++ {
		entries = append(entries, doclingTestZIPEntry{
			name: fmt.Sprintf("ppt/slides/slide%d.xml", index),
			body: `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`,
		})
	}
	return doclingTestZIP(t, entries)
}

func doclingTestXLSX(t *testing.T, sheetCount int) []byte {
	t.Helper()
	entries := []doclingTestZIPEntry{
		{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/></Types>`},
		{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
	}
	for index := 1; index <= sheetCount; index++ {
		entries = append(entries, doclingTestZIPEntry{
			name: fmt.Sprintf("xl/worksheets/sheet%d.xml", index),
			body: `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`,
		})
	}
	return doclingTestZIP(t, entries)
}

func TestExecutableProcessingProfilesRegistersDoclingDocument(t *testing.T) {
	cfg, descriptor := doclingDocumentProcessingConfig(t, "http://127.0.0.1:5001")
	require.NoError(t, cfg.Validate())
	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	require.Equal(t, descriptor, profiles["documents"].RenditionProvider.Descriptor())
	disclosure := profiles["documents"].RenditionDisclosure
	require.Equal(t, config.DoclingDocumentAdapterContract, disclosure.ImmediateProcessor)
	require.Equal(t, descriptor.ID, disclosure.UltimateProcessor)
	require.Equal(t, cfg.RenditionProfiles["documents"].Runtime.Endpoint, disclosure.Endpoint)
}

func TestDoclingDocumentRuntimeAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"descriptor drift", func(cfg *config.Config) {
			p := cfg.RenditionProfiles["documents"]
			p.DescriptorFingerprint = strings.Repeat("0", 64)
			cfg.RenditionProfiles["documents"] = p
		}, "descriptor differs"},
		{"endpoint drift", func(cfg *config.Config) {
			cfg.RenditionProfiles["documents"].Runtime.Endpoint = "http://127.0.0.2:5001"
		}, "disclosure fingerprint"},
		{"deployment drift", func(cfg *config.Config) {
			p := cfg.RenditionProfiles["documents"]
			p.DeploymentFingerprint = strings.Repeat("0", 64)
			cfg.RenditionProfiles["documents"] = p
		}, "disclosure fingerprint"},
		{"missing binding", func(cfg *config.Config) { cfg.CredentialBindings = nil }, "credential binding"},
		{"upload limit", func(cfg *config.Config) {
			p := cfg.RenditionProfiles["documents"]
			p.MaxDocumentBytes = docling.MaxDocumentBytes + 1
			cfg.RenditionProfiles["documents"] = p
		}, "execution bounds"},
		{"response limit", func(cfg *config.Config) {
			p := cfg.RenditionProfiles["documents"]
			p.MaxResponseBytes = docling.MaxResponseBytes + 1
			cfg.RenditionProfiles["documents"] = p
		}, "execution bounds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, _ := doclingDocumentProcessingConfig(t, "http://127.0.0.1:5001")
			test.mutate(&cfg)
			_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
			require.ErrorContains(t, err, test.want)
		})
	}
	t.Run("staged runtime", func(t *testing.T) {
		cfg, _ := doclingDocumentProcessingConfig(t, "http://127.0.0.1:5001")
		cfg.ProcessingProfiles = nil
		profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.NoError(t, err)
		require.Empty(t, profiles)
	})
	t.Run("reuse and conflict", func(t *testing.T) {
		cfg, _ := doclingDocumentProcessingConfig(t, "http://127.0.0.1:5001")
		cfg.RenditionProfiles["copy"] = cfg.RenditionProfiles["documents"]
		second := cfg.ProcessingProfiles["documents"]
		second.Rendition = "copy"
		second.MaxDocumentChars--
		cfg.ProcessingProfiles["copy"] = second
		profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.NoError(t, err)
		require.Same(t, profiles["documents"].RenditionProvider, profiles["copy"].RenditionProvider)
		p := cfg.RenditionProfiles["copy"]
		p.Runtime = new(*p.Runtime)
		p.Runtime.RequestTimeout = config.Duration(2 * time.Second)
		cfg.RenditionProfiles["copy"] = p
		_, err = executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.ErrorContains(t, err, "conflicts with another profile")
	})
}

func TestDaemonDoclingDocumentProcessing(t *testing.T) {
	for _, test := range []struct {
		name             string
		filename         string
		source           []byte
		disclose         bool
		failure          string
		maxResponseBytes int64
		maxUnits         int
	}{
		{name: "filename withheld"},
		{name: "plain text", filename: "report.txt", source: []byte("Synthetic source text\n")},
		{name: "PNG image", filename: "report.png", source: mediatest.PNG(2, 2, color.White)},
		{name: "filename disclosed", disclose: true},
		{name: "large response limit", maxResponseBytes: docling.MaxResponseBytes},
		{name: "PDF pages exceed max units", filename: "report.pdf", source: doclingTestPDFPages(2), disclose: true, maxUnits: 1, failure: "max-units"},
		{name: "PPTX slides exceed max units", filename: "deck.pptx", source: doclingTestPPTX(t, 2), disclose: true, maxUnits: 1, failure: "max-units"},
		{name: "XLSX sheets exceed max units", filename: "book.xlsx", source: doclingTestXLSX(t, 2), disclose: true, maxUnits: 1, failure: "max-units"},
		{name: "DOCX is not inspectable", filename: "report.docx", source: doclingTestZIP(t, []doclingTestZIPEntry{
			{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
			{name: "word/document.xml", body: `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"/>`},
		}), failure: "unbounded"},
		{name: "redirect refused", failure: "redirect"},
		{name: "missing secret", failure: "secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := test.source
			if source == nil {
				source = mediatest.PDF()
			}
			filename := test.filename
			if filename == "" {
				filename = "report.pdf"
			}
			var requests, submissions, redirects atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirects.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(destination.Close)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "synthetic-document-key", r.Header.Get("X-Api-Key"))
				if test.failure == "redirect" {
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/convert/file/async":
					assert.Equal(t, http.MethodPost, r.Method)
					r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
					reader, err := r.MultipartReader()
					if !assert.NoError(t, err) {
						return
					}
					form, err := reader.ReadForm(1 << 20)
					if !assert.NoError(t, err) {
						return
					}
					defer func() { assert.NoError(t, form.RemoveAll()) }()
					if !assert.Len(t, form.File["files"], 1) {
						return
					}
					header := form.File["files"][0]
					file, err := header.Open()
					if !assert.NoError(t, err) {
						return
					}
					defer func() { assert.NoError(t, file.Close()) }()
					uploaded, err := io.ReadAll(file)
					if !assert.NoError(t, err) {
						return
					}
					assert.Equal(t, source, uploaded)
					if test.disclose {
						assert.Equal(t, filename, header.Filename)
					} else {
						assert.NotEqual(t, filename, header.Filename)
					}
					assert.ElementsMatch(t, []string{"md", "json"}, form.Value["to_formats"])
					submissions.Add(1)
					_, err = io.WriteString(w, `{"task_id":"synthetic-task","task_type":"convert","task_status":"pending"}`)
					assert.NoError(t, err)
				case "/v1/status/poll/synthetic-task":
					_, err := io.WriteString(w, `{"task_id":"synthetic-task","task_type":"convert","task_status":"success"}`)
					assert.NoError(t, err)
				case "/v1/result/synthetic-task":
					err := json.MarshalWrite(w, map[string]any{"document": map[string]any{
						"filename": filename, "md_content": "# Synthetic needle report\n", "json_content": map[string]any{
							"schema_name": "DoclingDocument", "version": "1.7.0", "pages": map[string]any{"1": map[string]any{}},
							"texts": []any{map[string]any{"text": "Synthetic needle report", "prov": []any{map[string]any{"page_no": 1}}}},
						}}, "status": "success", "errors": []any{}})
					assert.NoError(t, err)
				default:
					t.Errorf("unexpected provider route %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(provider.Close)
			cfg, _ := doclingDocumentProcessingConfig(t, provider.URL)
			p := cfg.RenditionProfiles["documents"]
			p.DiscloseFilename = test.disclose
			if test.maxResponseBytes > 0 {
				p.MaxResponseBytes = test.maxResponseBytes
			}
			if test.maxUnits > 0 {
				p.MaxUnits = test.maxUnits
			}
			cfg.RenditionProfiles["documents"] = p
			require.NoError(t, cfg.Validate())
			cfg.Server.APIKey = "synthetic-document-daemon-key"
			secret := "synthetic-document-key"
			if test.failure == "secret" {
				secret = ""
			}
			t.Setenv("DOCBANK_TEST_DOCLING_KEY", secret)
			root := t.TempDir()
			require.NoError(t, writeDaemonASRConfig(root, cfg))
			t.Setenv("DOCBANK_HOME", root)
			startServe(t)
			runtime := waitForDaemon(t, root)
			daemon := daemonconn.New("http://"+runtime.Address, cfg.Server.APIKey)
			t.Cleanup(func() { require.NoError(t, daemon.Close()) })
			path := filepath.Join(t.TempDir(), filename)
			require.NoError(t, os.WriteFile(path, source, 0o600))
			_, err := runCLI(t, "add", path, "--dest", "/documents")
			require.NoError(t, err)
			node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/documents/" + filename}})
			require.NoError(t, err)
			selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "documents"}
			plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
			require.NoError(t, err)
			require.Len(t, plan.Flow, 1)
			require.Equal(t, "operator_network", plan.Flow[0].TrustBoundary)
			require.Equal(t, config.DoclingDocumentAdapterContract, plan.Flow[0].RuntimeDisclosure.ImmediateProcessor)
			require.Equal(t, provider.URL, plan.Flow[0].RuntimeDisclosure.Endpoint)
			require.Equal(t, test.disclose, plan.Flow[0].DiscloseFilename)
			require.Equal(t, p.DeploymentFingerprint, plan.Flow[0].RuntimeDisclosure.Deployment)
			require.True(t, plan.ConsentRequired)
			wantIneligible := map[string]string{"max-units": "semantic_units_exceeded", "unbounded": "unbounded_media_family"}[test.failure]
			require.Equal(t, wantIneligible, plan.RenditionIneligibleReason)
			require.Zero(t, requests.Load(), "ingest and planning must not call Docling")
			_, err = daemon.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}, plan.ProfileFingerprint)
			require.Error(t, err)
			require.Zero(t, requests.Load(), "no consent must prevent egress")
			job, err := daemon.EnqueueProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
			if wantIneligible != "" {
				require.ErrorContains(t, err, "422 rendition_source_ineligible")
				require.Zero(t, requests.Load(), "local inspection must reject the document before provider egress")
				require.Zero(t, redirects.Load(), "a rejected source must not follow provider redirects")
				return
			}
			require.NoError(t, err)
			wantState := "completed"
			if test.failure != "" {
				wantState = "failed"
			}
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				status, statusErr := daemon.ProcessingStatus(t.Context(), job.ID)
				require.NoError(c, statusErr)
				require.Equal(c, wantState, status.State, "%+v", status)
			}, 10*time.Second, 20*time.Millisecond)
			require.Zero(t, redirects.Load(), "redirect destination must never receive the upload or key")
			original, err := daemon.VersionContent(t.Context(), selector.ContentVersionID)
			require.NoError(t, err)
			var originalBytes bytes.Buffer
			_, err = original.CopyVerified(&originalBytes)
			require.NoError(t, err)
			require.NoError(t, original.Close())
			require.Equal(t, source, originalBytes.Bytes())
			rendition, err := daemon.RenditionForSelector(t.Context(), selector, 1<<20)
			if test.failure != "" {
				require.Error(t, err)
				if test.failure == "secret" || test.failure == "max-units" {
					require.Zero(t, requests.Load(), "a local preflight check must reject the document before provider egress")
				}
				return
			}
			require.NoError(t, err)
			var body bytes.Buffer
			_, err = rendition.CopyVerified(&body)
			require.NoError(t, err)
			require.NoError(t, rendition.Close())
			require.Contains(t, body.String(), "Synthetic needle report")
			require.EqualValues(t, 1, submissions.Load())
			search, err := daemon.SearchDocuments(t.Context(), api.DocumentSearchRequest{Query: "needle", Mode: "lexical", Profile: "documents", Limit: 10,
				Fence: api.DocumentSourceFence{VaultUID: plan.VaultUID, ContentVersionIDs: []string{selector.ContentVersionID}}})
			require.NoError(t, err)
			require.Len(t, search.Results, 1)
			require.Equal(t, selector.ContentVersionID, search.Results[0].ContentVersionID)
		})
	}
}
