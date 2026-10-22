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
		Retention: mistral.RetentionZDR, Training: mistral.TrainingOptedOut, MaxDocumentBytes: 1 << 20,
		MaxResponseBytes: 1 << 20, MaxUnits: 100, ExtractHeader: true, ExtractFooter: true, NormalizePolicy: normalize})
	require.NoError(t, err)
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, mistral.EncodeCapabilityManifest(&encoded, manifest))
	path := filepath.Join(t.TempDir(), "mistral.json")
	require.NoError(t, os.WriteFile(path, encoded.Bytes(), 0600))
	fingerprint, err := policy.Fingerprint(manifest)
	require.NoError(t, err)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{ID: "mistral.ocr-v1",
		ContractVersion: document.RenditionProviderContractVersion, PolicyFingerprint: fingerprint,
		TrustBoundary: document.RenditionTrustHostedProvider, ReturnsMarkdown: true, ReturnsStructured: true,
		SupportedFormats: []document.RenditionFormatCapability{{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile}}})
	require.NoError(t, err)
	p := cfg.RenditionProfiles["documents"]
	p.AdapterContract = config.MistralOCRAdapterContract
	p.DescriptorID, p.DescriptorFingerprint = descriptor.ID, descriptor.Fingerprint
	// This is the documented NUL-separated disclosure identity, independent of runtime wiring.
	p.DisclosureFingerprint = sha256Hex(strings.Join([]string{"docbank-mistral-ocr/v1", descriptor.ID, descriptor.Fingerprint, endpoint, p.DeploymentFingerprint}, "\x00"))
	p.TrustBoundary = "hosted_provider"
	p.RequestedArtifacts = []string{"provider_markdown"}
	p.Runtime.CapabilityManifest, p.Runtime.Retention, p.Runtime.Training = path, "zdr", "opted-out"
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

func TestMistralOCRRuntimeAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"descriptor drift", func(c *config.Config) {
			p := c.RenditionProfiles["documents"]
			p.DescriptorFingerprint = strings.Repeat("0", 64)
			c.RenditionProfiles["documents"] = p
		}, "expected descriptor_fingerprint"},
		{"endpoint drift", func(c *config.Config) { c.RenditionProfiles["documents"].Runtime.Endpoint = "https://example.invalid" }, "endpoint differs"},
		{"deployment drift", func(c *config.Config) {
			p := c.RenditionProfiles["documents"]
			p.DeploymentFingerprint = strings.Repeat("0", 64)
			c.RenditionProfiles["documents"] = p
		}, "disclosure fingerprint"},
		{"retention change", func(c *config.Config) { c.RenditionProfiles["documents"].Runtime.Retention = "standard" }, "expected descriptor_fingerprint"},
		{"missing manifest", func(c *config.Config) {
			c.RenditionProfiles["documents"].Runtime.CapabilityManifest = filepath.Join(t.TempDir(), "missing.json")
		}, "manifest is unavailable"},
		{"missing binding", func(c *config.Config) { c.CredentialBindings = nil }, "credential binding"},
		{"unit bounds", func(c *config.Config) {
			p := c.RenditionProfiles["documents"]
			p.MaxUnits = 1
			c.RenditionProfiles["documents"] = p
		}, "processing bounds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, descriptor := mistralOCRProcessingConfig(t)
			test.mutate(&cfg)
			_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
			require.ErrorContains(t, err, test.want)
			if test.name == "descriptor drift" {
				require.ErrorContains(t, err, descriptor.Fingerprint)
			}
		})
	}
	t.Run("no PDF authority", func(t *testing.T) {
		cfg, _ := mistralOCRProcessingConfig(t)
		profile, err := configuredMistralProfile(cfg.RenditionProfiles["documents"])
		require.NoError(t, err)
		manifest, err := mistraltest.SyntheticManifest(profile.Policy, false)
		require.NoError(t, err)
		var encoded bytes.Buffer
		require.NoError(t, mistral.EncodeCapabilityManifest(&encoded, manifest))
		require.NoError(t, os.WriteFile(cfg.RenditionProfiles["documents"].Runtime.CapabilityManifest, encoded.Bytes(), 0600))
		fp, err := profile.Policy.Fingerprint(manifest)
		require.NoError(t, err)
		d := profile.Descriptor
		d.Fingerprint = ""
		d.PolicyFingerprint = fp
		d, err = document.NewRenditionDescriptor(d)
		require.NoError(t, err)
		p := cfg.RenditionProfiles["documents"]
		p.DescriptorFingerprint = d.Fingerprint
		p.DisclosureFingerprint = renditionDisclosureFingerprint(p.AdapterContract, d, p.Runtime.Endpoint, p.DeploymentFingerprint)
		cfg.RenditionProfiles["documents"] = p
		_, err = executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.ErrorContains(t, err, "no enforceable upload authority")
	})
	t.Run("staged runtime", func(t *testing.T) {
		cfg, _ := mistralOCRProcessingConfig(t)
		cfg.ProcessingProfiles = nil
		cfg.RenditionProfiles["documents"].Runtime.CapabilityManifest = "missing.json"
		profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.NoError(t, err)
		require.Empty(t, profiles)
	})
	t.Run("reuse and conflict", func(t *testing.T) {
		cfg, _ := mistralOCRProcessingConfig(t)
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

func TestDaemonMistralOCRProcessing(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	cfg.Server.APIKey = "synthetic-mistral-daemon-key"
	t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", "")
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
	source := mistraltest.MinimalPDF("synthetic daemon OCR")
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
	require.Equal(t, "hosted_provider", plan.Flow[0].TrustBoundary)
	require.Equal(t, "https://api.eu.mistral.ai", plan.Flow[0].RuntimeDisclosure.Endpoint)
	require.Equal(t, "docbank-mistral-ocr/v1", plan.Flow[0].RuntimeDisclosure.ImmediateProcessor)
	require.Equal(t, mistral.DefaultModel, plan.Flow[0].RuntimeDisclosure.Model)
	require.True(t, plan.ConsentRequired)
	_, err = daemon.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}, plan.ProfileFingerprint)
	require.Error(t, err)
	job, err := daemon.EnqueueProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		status, statusErr := daemon.ProcessingStatus(t.Context(), job.ID)
		require.NoError(c, statusErr)
		require.Equal(c, "failed", status.State)
		// The worker stores non-retryable provider errors as terminal.
		require.Equal(c, "terminal", status.FailureCode)
	}, 10*time.Second, 20*time.Millisecond)
	original, err := daemon.VersionContent(t.Context(), selector.ContentVersionID)
	require.NoError(t, err)
	var data bytes.Buffer
	_, err = original.CopyVerified(&data)
	require.NoError(t, err)
	require.NoError(t, original.Close())
	require.Equal(t, source, data.Bytes())
}
