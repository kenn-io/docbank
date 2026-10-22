package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"go.kenn.io/docbank/internal/processing"
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
	descriptor, err := mistral.DocumentDescriptor(policy, manifest)
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
		p.DisclosureFingerprint = mistral.DisclosureFingerprint(d, p.Runtime.Endpoint, p.DeploymentFingerprint)
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
		// Observation dates are evidence metadata, outside reusable execution identity.
		p := cfg.RenditionProfiles["copy"]
		p.Runtime = new(*p.Runtime)
		original, err := configuredMistralProfile(p)
		require.NoError(t, err)
		manifest := original.CapabilityManifest
		manifest.ObservedOn = "2026-01-02"
		var encoded bytes.Buffer
		require.NoError(t, mistral.EncodeCapabilityManifest(&encoded, manifest))
		p.Runtime.CapabilityManifest = filepath.Join(t.TempDir(), "reprobed.json")
		require.NoError(t, os.WriteFile(p.Runtime.CapabilityManifest, encoded.Bytes(), 0600))
		cfg.RenditionProfiles["copy"] = p
		profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.NoError(t, err)
		require.Same(t, profiles["documents"].RenditionProvider, profiles["copy"].RenditionProvider)
		p = cfg.RenditionProfiles["copy"]
		p.Runtime.RequestTimeout = config.Duration(2 * time.Second)
		cfg.RenditionProfiles["copy"] = p
		_, err = executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.ErrorContains(t, err, "conflicts with another profile")
	})
}

func TestDaemonMistralOCRProcessing(t *testing.T) {
	for _, missingSecret := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing secret %t", missingSecret), func(t *testing.T) {
			cfg, _ := mistralOCRProcessingConfig(t)
			cfg.Server.APIKey = "synthetic-mistral-daemon-key"
			secret := "synthetic-mistral-key"
			if missingSecret {
				secret = ""
			}
			t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", secret)
			source := mistraltest.MinimalPDF("synthetic daemon OCR")
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/v1/ocr", r.URL.Path)
				assert.Equal(t, "Bearer synthetic-mistral-key", r.Header.Get("Authorization"))
				var wire struct {
					Model    string `json:"model"`
					Document struct {
						URL string `json:"document_url"`
					} `json:"document"`
				}
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &wire)) {
					return
				}
				assert.Equal(t, mistral.DefaultModel, wire.Model)
				uploaded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(wire.Document.URL, "data:application/pdf;base64,"))
				assert.NoError(t, err)
				assert.Equal(t, source, uploaded)
				w.Header().Set("Content-Type", "application/json")
				_, err = fmt.Fprintf(w, `{"model":%q,"pages":[{"index":0,"markdown":"# Synthetic needle report","dimensions":{"dpi":144,"height":792,"width":612}}],"usage_info":{"pages_processed":1,"doc_size_bytes":%d}}`, mistral.DefaultModel, len(source))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			renderErrors := make(chan error, 1)
			root := t.TempDir()
			require.NoError(t, writeDaemonASRConfig(root, cfg))
			t.Setenv("DOCBANK_HOME", root)
			startServeWithRunner(t, func(ctx context.Context) error {
				return runServeWithProcessingProfiles(ctx, func(cfg config.Config, bundle embeddingRuntimeBundle) (map[string]processing.ProfileConfig, error) {
					profiles, err := executableProcessingProfiles(cfg, bundle)
					if err != nil {
						return nil, err
					}
					profile, err := configuredMistralProfile(cfg.RenditionProfiles["documents"])
					if err != nil {
						return nil, err
					}
					secrets := environmentCredentialSecrets{variables: map[string]string{profile.SecretBinding: cfg.CredentialBindings[profile.SecretBinding].EnvironmentVariable}}
					client, err := mistral.NewRenditionProvider(profile, secrets, &http.Client{Transport: mistralTestTransport{server: server}})
					if err != nil {
						return nil, err
					}
					bound := profiles["documents"].RenditionProvider.(*configuredRenditionProvider)
					bound.RenditionProvider = client
					supplied := profiles["documents"]
					supplied.RenditionProvider = &observedMistralProvider{RenditionProvider: bound, errors: renderErrors}
					profiles["documents"] = supplied
					return profiles, nil
				})
			})
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
			require.Equal(t, "docbank-mistral-ocr/v1", plan.Flow[0].RuntimeDisclosure.ImmediateProcessor)
			require.Equal(t, mistral.DefaultModel, plan.Flow[0].RuntimeDisclosure.Model)
			require.True(t, plan.ConsentRequired)
			_, err = daemon.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}, plan.ProfileFingerprint)
			require.ErrorContains(t, err, "consent")
			require.Zero(t, requests.Load(), "planning and builds without consent must not upload")
			job, err := daemon.EnqueueProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
			require.NoError(t, err)
			var providerErr error
			select {
			case providerErr = <-renderErrors:
			case <-time.After(10 * time.Second):
				status, statusErr := daemon.ProcessingStatus(t.Context(), job.ID)
				require.NoError(t, statusErr)
				t.Fatalf("Mistral Render was not called: %+v", status)
			}

			require.EventuallyWithT(t, func(c *assert.CollectT) {
				status, statusErr := daemon.ProcessingStatus(t.Context(), job.ID)
				require.NoError(c, statusErr)
				if missingSecret {
					require.Equal(c, "failed", status.State)
					require.Equal(c, "terminal", status.FailureCode)
				} else {
					require.Equal(c, "completed", status.State, "%+v", status)
				}
			}, 10*time.Second, 20*time.Millisecond)
			original, err := daemon.VersionContent(t.Context(), selector.ContentVersionID)
			require.NoError(t, err)
			var data bytes.Buffer
			_, err = original.CopyVerified(&data)
			require.NoError(t, err)
			require.NoError(t, original.Close())
			require.Equal(t, source, data.Bytes())
			rendition, err := daemon.RenditionForSelector(t.Context(), selector, 1<<20)
			if missingSecret {
				require.Error(t, err)
				require.Zero(t, requests.Load())
				classified, ok := errors.AsType[*document.RenditionProviderError](providerErr)
				require.True(t, ok, "%v", providerErr)
				require.Equal(t, document.RenditionErrorAuthentication, classified.Code())
				require.EqualError(t, classified.Unwrap(), "Mistral credential is unavailable: Mistral OCR credential is unavailable")
				return
			}
			require.NoError(t, providerErr)
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
		})
	}
}

// Only the HTTP destination changes; the real Mistral client still validates and maps OCR.
type mistralTestTransport struct{ server *httptest.Server }

func (transport mistralTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	forwarded := request.Clone(request.Context())
	destination, err := url.Parse(transport.server.URL)
	if err != nil {
		return nil, err
	}
	forwarded.URL.Scheme, forwarded.URL.Host = destination.Scheme, destination.Host
	return transport.server.Client().Transport.RoundTrip(forwarded)
}

type observedMistralProvider struct {
	document.RenditionProvider
	errors chan<- error
}

func (provider *observedMistralProvider) Render(ctx context.Context, upload document.AuthorizedUpload, auth document.RenditionAuthorization) (document.RenditionResult, error) {
	result, err := provider.RenditionProvider.Render(ctx, upload, auth)
	provider.errors <- err
	return result, err
}
