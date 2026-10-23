package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/safefileio"
)

func mistralOCRProcessingConfig(t *testing.T) (config.Config, document.RenditionDescriptor) {
	t.Helper()
	const endpoint = "https://api.eu.mistral.ai"
	cfg, _ := doclingDocumentProcessingConfig(t, endpoint)
	normalize, err := document.NewNormalizePolicy(25_000_000)
	require.NoError(t, err)
	policy, err := mistral.NewPolicy(mistral.PolicyConfig{Region: mistral.RegionEU, Model: mistral.DefaultModel,
		Retention: mistral.RetentionStandard, Training: mistral.TrainingDefaultOptOut, MaxDocumentBytes: 1 << 20,
		MaxResponseBytes: 1 << 20, MaxUnits: 100, ExtractHeader: false, ExtractFooter: false, NormalizePolicy: normalize})
	require.NoError(t, err)
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, mistral.EncodeCapabilityManifest(&encoded, manifest))
	path := filepath.Join(t.TempDir(), "mistral.json")
	require.NoError(t, os.WriteFile(path, encoded.Bytes(), 0600))
	descriptor, err := mistral.DocumentDescriptor(policy, manifest)
	require.NoError(t, err)
	p := cfg.RenditionProfiles["documents"]
	p.AdapterContract = config.MistralOCRAdapterContract
	p.DescriptorID, p.DescriptorFingerprint = descriptor.ID, descriptor.Fingerprint
	// This is the documented NUL-separated disclosure identity, independent of runtime wiring.
	p.DisclosureFingerprint = sha256Hex(strings.Join([]string{"docbank-mistral-ocr/v1", descriptor.ID, descriptor.Fingerprint, endpoint, p.DeploymentFingerprint}, "\x00"))
	p.TrustBoundary = "hosted_provider"
	p.RequestedArtifacts = []string{"provider_markdown"}
	p.Runtime.CapabilityManifest = path
	p.Runtime.TotalTimeout, p.Runtime.PollInterval, p.Runtime.MaxPollAttempts = 0, 0, 0
	p.Runtime.AllowedCIDRs = []string{"192.0.2.0/24"}
	cfg.RenditionProfiles["documents"] = p
	cfg.CredentialBindings["docling"] = config.CredentialBindingConfig{EnvironmentVariable: "DOCBANK_TEST_MISTRAL_OCR_KEY"}
	return cfg, descriptor
}

func TestExecutableProcessingProfilesRegistersMistralOCR(t *testing.T) {
	cfg, descriptor := mistralOCRProcessingConfig(t)
	require.NoError(t, cfg.Validate())
	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	require.Equal(t, descriptor, profiles["documents"].RenditionProvider.Descriptor())
	disclosure := profiles["documents"].RenditionDisclosure
	require.Equal(t, "docbank-mistral-ocr/v1", disclosure.ImmediateProcessor)
	require.Equal(t, "mistral.ocr-v1", disclosure.UltimateProcessor)
	require.Equal(t, "https://api.eu.mistral.ai", disclosure.Endpoint)
	require.Equal(t, strings.Repeat("2", 64), disclosure.Deployment)
	require.Equal(t, mistral.DefaultModel, disclosure.Model)
}

func TestMistralOCRRuntimeManifest(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	cfg.RenditionProfiles["documents"].Runtime.CapabilityManifest = filepath.Join(t.TempDir(), "missing.json")
	_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.ErrorContains(t, err, "manifest is unavailable")
	cfg.ProcessingProfiles = nil
	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	require.Empty(t, profiles)
}

func TestDaemonMistralOCRProcessing(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	providerRuntime := cfg.RenditionProfiles["documents"].Runtime
	providerRuntime.AllowedCIDRs = []string{"192.0.2.0/32"}
	providerRuntime.RequestTimeout = config.Duration(time.Nanosecond)
	providerRuntime.ConnectTimeout = config.Duration(time.Nanosecond)
	providerRuntime.KeepAlive = config.Duration(time.Nanosecond)
	providerRuntime.TLSHandshakeTimeout = config.Duration(time.Nanosecond)
	require.NoError(t, cfg.Validate())
	cfg.Server.APIKey = "synthetic-mistral-daemon-key"
	t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", "synthetic-mistral-key")
	source := mistraltest.MinimalPDF("synthetic daemon OCR")
	root := t.TempDir()
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	t.Setenv("DOCBANK_HOME", root)
	startServe(t)
	runtime := waitForDaemon(t, root)
	daemon := daemonconn.New("http://"+runtime.Address, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })
	output, err := runCLI(t, "processing", "profiles", "--json")
	require.NoError(t, err)
	var profiles []api.ProcessingProfileSummary
	require.NoError(t, json.Unmarshal([]byte(output), &profiles))
	var names []string
	for _, profile := range profiles {
		names = append(names, profile.Name)
	}
	require.Contains(t, names, "documents")
	path := filepath.Join(t.TempDir(), "report.pdf")
	require.NoError(t, os.WriteFile(path, source, 0600))
	_, err = runCLI(t, "add", path, "--dest", "/documents")
	require.NoError(t, err)
	node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/documents/report.pdf"}})
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "documents"}
	plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	require.Len(t, plan.Flow, 1)
	job, err := daemon.EnqueueProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	// The runtime keeps Mistral's default HTTP retries before recording a failed attempt.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		status, statusErr := daemon.ProcessingStatus(t.Context(), job.ID)
		require.NoError(c, statusErr)
		require.Equal(c, "retry_wait", status.State, "%+v", status)
		require.Equal(c, "transient", status.FailureCode)
	}, 20*time.Second, 20*time.Millisecond)
}

func TestMistralPasswordFileProcessing(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	path := filepath.Join(t.TempDir(), "passwords")
	cfg.Media.PDFPasswordsFile = path
	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.WriteString("wrong\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", "synthetic-key")
	source, err := mediatest.EncryptedPDF("open", "owner", 256)
	require.NoError(t, err)
	transport := &passwordOCRTransport{}
	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	bound, ok := profiles["documents"].RenditionProvider.(*configuredRenditionProvider)
	require.True(t, ok)
	adapter, err := resolveRenditionAdapter(cfg.RenditionProfiles["documents"])
	require.NoError(t, err)
	bound.RenditionProvider, err = adapter.newProvider(environmentCredentialSecrets{variables: map[string]string{"docling": "DOCBANK_TEST_MISTRAL_OCR_KEY"}}, &http.Client{Transport: transport})
	require.NoError(t, err)
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, blobs.Close()) }()
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "statement.pdf", written.Hash, written.Size, "application/pdf", store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize})
	require.NoError(t, err)
	service, err := processing.NewService(processing.ServiceConfig{Catalog: catalog, Blobs: blobs, Gate: api.NewOperationGate(), Profiles: profiles, SpoolDirectory: t.TempDir(), Lifecycle: t.Context()})
	require.NoError(t, err)
	defer func() { require.NoError(t, service.Shutdown(context.Background())) }()
	cfg.Server.APIKey = "synthetic-key"
	server := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: root, Cfg: cfg, Processing: service})
	defer server.Close()
	endpoint := httptest.NewServer(server.Handler())
	defer endpoint.Close()
	daemon := daemonconn.New(endpoint.URL, cfg.Server.APIKey)
	defer func() { require.NoError(t, daemon.Close()) }()
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "documents"}
	plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	stream, err := daemon.StartProcessingStream(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()
	failed, err := stream.Next()
	require.NoError(t, err)
	terminal, err := stream.Next()
	require.Error(t, err)
	require.NotNil(t, terminal.Status)
	require.Equal(t, "failed", terminal.Status.State)
	require.Equal(t, "pdf_password_required", terminal.Status.FailureCode)
	transport.mu.Lock()
	requests := transport.requests
	transport.mu.Unlock()
	require.Zero(t, requests)
	require.NoError(t, os.WriteFile(path, []byte("wrong\nopen\n"), 0600))
	plan, err = daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	require.Empty(t, plan.RenditionIneligibleReason)
	completed, err := daemon.EnqueueProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		status, err := daemon.ProcessingStatus(t.Context(), completed.ID)
		require.NoError(c, err)
		require.Equal(c, "completed", status.State)
	}, 10*time.Second, 10*time.Millisecond)
	require.NotEqual(t, failed.Job.ID, completed.ID)
	previous, err := daemon.ProcessingStatus(t.Context(), failed.Job.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", previous.State)
	transport.mu.Lock()
	requests = transport.requests
	transport.mu.Unlock()
	require.Equal(t, 1, requests)

	original, _, err := blobs.OpenStreamContext(t.Context(), written.Hash)
	require.NoError(t, err)
	archived, err := io.ReadAll(original)
	require.NoError(t, err)
	require.NoError(t, original.Close())
	require.Equal(t, source, archived)
	rendition, err := daemon.RenditionForSelector(t.Context(), selector, 1<<20)
	require.NoError(t, err)
	var text bytes.Buffer
	_, err = rendition.CopyVerified(&text)
	require.NoError(t, err)
	require.NoError(t, rendition.Close())
	require.Contains(t, text.String(), "Password needle")
}

type passwordOCRTransport struct {
	mu       sync.Mutex
	requests int
	uploaded []byte
}

func (transport *passwordOCRTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var wire struct {
		Document struct {
			URL string `json:"document_url"`
		} `json:"document"`
	}
	if err := json.UnmarshalRead(request.Body, &wire); err != nil {
		return nil, err
	}
	uploaded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(wire.Document.URL, "data:application/pdf;base64,"))
	if err != nil {
		return nil, fmt.Errorf("decode OCR upload: %w", err)
	}
	transport.mu.Lock()
	transport.requests++
	transport.uploaded = uploaded
	transport.mu.Unlock()
	response := fmt.Sprintf(`{"model":%q,"pages":[{"index":0,"markdown":"# Password needle","dimensions":{"dpi":144,"height":16,"width":16}}],"usage_info":{"pages_processed":1}}`, mistral.DefaultModel)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
}
