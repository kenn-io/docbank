package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	"go.kenn.io/docbank/sqlite"
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

func startMistralOCRDaemon(t *testing.T, watches []config.WatchConfig, terminalFailure bool) (*daemonconn.Connection, *atomic.Int64, func()) {
	t.Helper()
	var attempts atomic.Int64
	var heldClock atomic.Int64
	previousClient := mistralOCRTestClient
	mistralOCRTestClient = func(client *http.Client) *http.Client {
		client.Transport = mistralProbeTransport(func(*http.Request) (*http.Response, error) {
			heldClock.CompareAndSwap(0, time.Now().UTC().UnixNano())
			attempts.Add(1)
			if terminalFailure {
				return &http.Response{StatusCode: http.StatusUnsupportedMediaType, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			return nil, errors.New("synthetic provider failure")
		})
		return client
	}
	t.Cleanup(func() { mistralOCRTestClient = previousClient })
	// The synthetic provider fails immediately; a nanosecond cap keeps its
	// HTTP retries from waiting between attempts.
	previousRetryDelay := mistralMaxRetryDelay
	mistralMaxRetryDelay = time.Nanosecond
	t.Cleanup(func() { mistralMaxRetryDelay = previousRetryDelay })
	// Match the authorization clock until the provider is reached. Then hold
	// time still so the recorded retry cannot become due during assertions.
	previousClock := renditionWorkerClock
	renditionWorkerClock = func() time.Time {
		if held := heldClock.Load(); held != 0 {
			return time.Unix(0, held).UTC()
		}
		return time.Now().UTC()
	}
	t.Cleanup(func() { renditionWorkerClock = previousClock })
	cfg, _ := mistralOCRProcessingConfig(t)
	providerRuntime := cfg.RenditionProfiles["documents"].Runtime
	providerRuntime.AllowedCIDRs = []string{"192.0.2.0/32"}
	providerRuntime.RequestTimeout = config.Duration(5 * time.Second)
	providerRuntime.ConnectTimeout = config.Duration(time.Nanosecond)
	providerRuntime.KeepAlive = config.Duration(time.Nanosecond)
	providerRuntime.TLSHandshakeTimeout = config.Duration(time.Nanosecond)
	require.NoError(t, cfg.Validate())
	cfg.Server.APIKey = "synthetic-mistral-daemon-key"
	t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", "synthetic-mistral-key")
	root := t.TempDir()
	cfg.Watches = watches
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	t.Setenv("DOCBANK_HOME", root)
	stop := startServe(t)
	runtime := waitForDaemon(t, root)
	daemon := daemonconn.New("http://"+runtime.Address, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })
	return daemon, &attempts, stop
}

func TestDaemonMistralOCRProcessing(t *testing.T) {
	daemon, attempts, _ := startMistralOCRDaemon(t, nil, true)
	source := mistraltest.MinimalPDF("synthetic daemon OCR")
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
	job, err := daemon.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.Error(t, err)
	require.Equal(t, node.CurrentVersionID, job.ContentVersionID)
	// The stream ends after the provider rejects the input and the durable
	// job records the terminal failure.
	status, err := daemon.ProcessingStatus(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", status.State, "%+v", status)
	require.Equal(t, "terminal", status.FailureCode)
	require.Positive(t, attempts.Load(), "the Mistral provider must be attempted")
}

func TestDaemonWatchProfileAdmitsImports(t *testing.T) {
	inbox := t.TempDir()
	daemon, attempts, stop := startMistralOCRDaemon(t, []config.WatchConfig{{Name: "inbox", Source: inbox, Destination: "/inbox", Profile: "documents", CaptureOnChange: true, ScanInterval: config.Duration(time.Hour)}}, false)
	path := filepath.Join(t.TempDir(), "consent.pdf")
	require.NoError(t, os.WriteFile(path, mistraltest.MinimalPDF("consent source"), 0600))
	_, err := runCLI(t, "add", path, "--dest", "/documents")
	require.NoError(t, err)
	node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/documents/consent.pdf"}})
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "documents"}
	plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	_, err = daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(inbox, "watched.pdf"), mistraltest.MinimalPDF("watched after consent"), 0600))
	// Settle asks the watcher for a scan; the interval never elapses.
	settleDaemon(t)
	watched, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/inbox/watched.pdf"}})
	require.NoError(t, err)
	stop()
	db, err := store.DefaultSQLiteDriver().Open(filepath.Join(os.Getenv("DOCBANK_HOME"), "docbank.db"),
		sqlite.OpenOptions{Access: sqlite.ReadOnlyImmutable, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	// The daemon watch callback queues the exact imported version. The
	// synthetic provider fails, so after the client's HTTP retries the worker
	// records a transient failure that waits to retry; the held worker clock
	// keeps that retry from coming due.
	// The latest document-wide status may belong to automatic PDF extraction.
	var state string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT j.state FROM rendition_jobs j
		JOIN rendition_job_waiters w ON w.job_id=j.job_id
		WHERE w.content_version_id=? AND w.profile_fingerprint=?`, watched.CurrentVersionID, plan.ProfileFingerprint).Scan(&state))
	require.Equal(t, "retry_wait", state)
	require.Positive(t, attempts.Load(), "the Mistral provider must be attempted")
}

func TestMistralPasswordFileProcessing(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	path := filepath.Join(t.TempDir(), "passwords")
	cfg.Media.PDFPasswordsFile = path
	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.WriteString("wrong\nopen\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", "synthetic-key")
	source, err := mediatest.EncryptedPDF(t.Context(), "open", "owner", 256)
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
	catalog, err := store.Open(t.Context(), filepath.Join(root, "docbank.db"))
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
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: catalog, Blobs: blobs, Gate: api.NewOperationGate(), Profiles: profiles, SpoolDirectory: t.TempDir(), Lifecycle: t.Context()})
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
	require.Empty(t, plan.RenditionIneligibleReason)
	// The stream's terminal event arrives once the run has finished.
	stream, err := daemon.StartProcessingStream(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	_, err = stream.Next()
	require.NoError(t, err)
	terminal, err := stream.Next()
	require.NoError(t, err)
	require.NotNil(t, terminal.Status)
	require.Equal(t, "completed", terminal.Status.State)
	transport.mu.Lock()
	requests := transport.requests
	transport.mu.Unlock()
	require.Equal(t, 1, requests)
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
