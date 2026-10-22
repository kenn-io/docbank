package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
		{"deployment drift", func(c *config.Config) {
			p := c.RenditionProfiles["documents"]
			p.DeploymentFingerprint = strings.Repeat("0", 64)
			c.RenditionProfiles["documents"] = p
		}, "disclosure fingerprint"},
		{"missing manifest", func(c *config.Config) {
			c.RenditionProfiles["documents"].Runtime.CapabilityManifest = filepath.Join(t.TempDir(), "missing.json")
		}, "manifest is unavailable"},
		{"missing binding", func(c *config.Config) { c.CredentialBindings = nil }, "credential binding"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, _ := mistralOCRProcessingConfig(t)
			test.mutate(&cfg)
			_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
			require.ErrorContains(t, err, test.want)
		})
	}
	t.Run("staged runtime", func(t *testing.T) {
		cfg, _ := mistralOCRProcessingConfig(t)
		cfg.ProcessingProfiles = nil
		cfg.RenditionProfiles["documents"].Runtime.CapabilityManifest = filepath.Join(t.TempDir(), "missing.json")
		profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.NoError(t, err)
		require.Empty(t, profiles)
	})
}

func TestMistralOCRSharedProvider(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflicting timeout %t", conflict), func(t *testing.T) {
			cfg, _ := mistralOCRProcessingConfig(t)
			other := cfg.RenditionProfiles["documents"]
			runtime := *other.Runtime
			other.Runtime = &runtime
			manifest, err := os.ReadFile(runtime.CapabilityManifest)
			require.NoError(t, err)
			runtime.CapabilityManifest = filepath.Join(t.TempDir(), "other.json")
			require.NoError(t, os.WriteFile(runtime.CapabilityManifest, manifest, 0600))
			if conflict {
				runtime.RequestTimeout += config.Duration(time.Second)
			}
			cfg.RenditionProfiles["other"] = other
			processing := cfg.ProcessingProfiles["documents"]
			processing.Rendition = "other"
			cfg.ProcessingProfiles["other"] = processing
			profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
			if conflict {
				require.ErrorContains(t, err, "conflicts with another profile")
				return
			}
			require.NoError(t, err)
			require.Same(t, profiles["documents"].RenditionProvider, profiles["other"].RenditionProvider)
		})
	}
}

func TestRenditionSharedProviderEquivalentCIDRs(t *testing.T) {
	for _, adapter := range []string{"Mistral", "Docling"} {
		t.Run(adapter, func(t *testing.T) {
			var cfg config.Config
			if adapter == "Mistral" {
				cfg, _ = mistralOCRProcessingConfig(t)
			} else {
				cfg, _ = doclingDocumentProcessingConfig(t, "http://127.0.0.1:5001")
			}
			cfg.RenditionProfiles["documents"].Runtime.AllowedCIDRs = []string{"fd00::/64"}
			other := cfg.RenditionProfiles["documents"]
			other.Runtime = new(*other.Runtime)
			other.Runtime.AllowedCIDRs = []string{"fd00:0:0:0::/64"}
			cfg.RenditionProfiles["other"] = other
			processing := cfg.ProcessingProfiles["documents"]
			processing.Rendition = "other"
			cfg.ProcessingProfiles["other"] = processing
			require.NoError(t, cfg.Validate())
			profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
			require.NoError(t, err)
			require.Same(t, profiles["documents"].RenditionProvider, profiles["other"].RenditionProvider)
		})
	}
}

func TestDaemonMistralOCRProcessing(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	cfg.Server.APIKey = "synthetic-mistral-daemon-key"
	t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", "synthetic-mistral-key")
	source := mistraltest.MinimalPDF("synthetic daemon OCR")
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/ocr", r.URL.Path)
		assert.Equal(t, "Bearer synthetic-mistral-key", r.Header.Get("Authorization"))
		var wire struct {
			Model         string `json:"model"`
			ExtractHeader bool   `json:"extract_header"`
			ExtractFooter bool   `json:"extract_footer"`
			Document      struct {
				URL string `json:"document_url"`
			} `json:"document"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &wire)) {
			return
		}
		assert.Equal(t, mistral.DefaultModel, wire.Model)
		assert.False(t, wire.ExtractHeader)
		assert.False(t, wire.ExtractFooter)
		uploaded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(wire.Document.URL, "data:application/pdf;base64,"))
		assert.NoError(t, err)
		assert.Equal(t, source, uploaded)
		w.Header().Set("Content-Type", "application/json")
		_, err = fmt.Fprintf(w, `{"model":%q,"pages":[{"index":0,"markdown":"# Synthetic needle report","dimensions":{"dpi":144,"height":792,"width":612}}],"usage_info":{"pages_processed":1,"doc_size_bytes":%d}}`, mistral.DefaultModel, len(source))
		assert.NoError(t, err)
	}))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"api.eu.mistral.ai"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	previousEgress := renditionTestEgress
	t.Cleanup(func() { renditionTestEgress = previousEgress })
	renditionTestEgress.resolver = mistralTestResolver{t: t}
	renditionTestEgress.roots = x509.NewCertPool()
	renditionTestEgress.roots.AddCert(leaf)
	renditionTestEgress.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		assert.Equal(t, "tcp4", network)
		assert.Equal(t, "127.0.0.1:443", address)
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	cfg.RenditionProfiles["documents"].Runtime.AllowedCIDRs = []string{"127.0.0.1/32"}
	cfg.RenditionProfiles["documents"].Runtime.SPKISHA256 = []string{hex.EncodeToString(pin[:])}
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
	require.Equal(t, "hosted_provider", plan.Flow[0].TrustBoundary)
	require.Equal(t, "https://api.eu.mistral.ai", plan.Flow[0].RuntimeDisclosure.Endpoint)
	require.Equal(t, mistral.DefaultModel, plan.Flow[0].RuntimeDisclosure.Model)
	require.True(t, plan.ConsentRequired)
	_, err = daemon.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}, plan.ProfileFingerprint)
	require.ErrorContains(t, err, "consent")
	require.Zero(t, requests.Load(), "planning and builds without consent must not upload")
	job, err := daemon.EnqueueProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		status, statusErr := daemon.ProcessingStatus(t.Context(), job.ID)
		require.NoError(c, statusErr)
		require.Equal(c, "completed", status.State, "%+v", status)
	}, 10*time.Second, 20*time.Millisecond)
	original, err := daemon.VersionContent(t.Context(), selector.ContentVersionID)
	require.NoError(t, err)
	var data bytes.Buffer
	_, err = original.CopyVerified(&data)
	require.NoError(t, err)
	require.NoError(t, original.Close())
	require.Equal(t, source, data.Bytes())
	rendition, err := daemon.RenditionForSelector(t.Context(), selector, 1<<20)
	require.NoError(t, err)
	var markdown bytes.Buffer
	_, err = rendition.CopyVerified(&markdown)
	require.NoError(t, err)
	require.NoError(t, rendition.Close())
	require.Contains(t, markdown.String(), "Synthetic needle report")
	require.EqualValues(t, 1, requests.Load())
	search, err := daemon.SearchDocuments(t.Context(), api.DocumentSearchRequest{Query: "needle", Mode: "lexical", Profile: "documents", Limit: 10,
		Fence: api.DocumentSourceFence{VaultUID: plan.VaultUID, ContentVersionIDs: []string{selector.ContentVersionID}}})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	require.Equal(t, selector.ContentVersionID, search.Results[0].ContentVersionID)
}

type mistralTestResolver struct{ t *testing.T }

func (resolver mistralTestResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	assert.Equal(resolver.t, "ip", network)
	assert.Equal(resolver.t, "api.eu.mistral.ai", host)
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}
