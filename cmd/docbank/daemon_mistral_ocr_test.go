package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
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
