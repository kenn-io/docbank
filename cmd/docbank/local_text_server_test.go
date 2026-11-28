package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedserver"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

func TestLocalTextServerCandidate(t *testing.T) {
	cfg := localTextSourceConfig(t, "http://127.0.0.1:12345", "source-revision", "prefix-free", "DOCBANK_TEST_SOURCE_KEY")
	target := processing.LocalTextTarget{Server: "auto", EmbeddingServer: "http://127.0.0.1:12345/api/v1/servers/example", EmbeddingModel: "gemini-embedding-2", EmbeddingDimensions: 2}
	candidate, _, err := localTextServerCandidate(cfg, target, "archive")
	require.NoError(t, err)
	require.NoError(t, candidate.Validate())
	_, err = configureEmbeddingRuntimeBundle(candidate)
	require.NoError(t, err)
}

func TestLocalTextServerStartupChecksCredentialsAndDimensions(t *testing.T) {
	t.Setenv("DOCBANK_TEST_EMBED_KEY", "synthetic-key")
	handler := embedserver.Handler(serverTestBackend{}, []string{"model"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	target := processing.LocalTextTarget{EmbeddingServer: server.URL + "/v1", EmbeddingModel: "model", EmbeddingDimensions: 2, EmbeddingAPIKeyEnv: "DOCBANK_TEST_EMBED_KEY"}
	require.NoError(t, localTextServerStartup(t.Context(), target))
	target.EmbeddingAPIKeyEnv = "DOCBANK_TEST_MISSING_KEY"
	require.Error(t, localTextServerStartup(t.Context(), target))
	target.EmbeddingAPIKeyEnv = ""
	require.Error(t, localTextServerStartup(t.Context(), target))
}

func TestLocalTextServerStartupPrefix(t *testing.T) {
	server := httptest.NewServer(http.StripPrefix("/api/v1/servers/example", embedserver.Handler(serverTestBackend{}, []string{"gemini-embedding-2"})))
	defer server.Close()
	require.NoError(t, localTextServerStartup(t.Context(), processing.LocalTextTarget{EmbeddingServer: server.URL + "/api/v1/servers/example", EmbeddingModel: "gemini-embedding-2", EmbeddingDimensions: 2}))
}

func TestLocalTextServerPairsMedia(t *testing.T) {
	cfg := localTextSourceConfig(t, "http://127.0.0.1:12345", "source-revision", "prefix-free", "DOCBANK_TEST_SOURCE_KEY")
	media := cfg.EmbeddingProfiles["semantic"]
	media.InputKind = string(document.EmbeddingInputOriginalFile)
	media.MaxInputTokens = 0
	media.Chunk = config.EmbeddingChunkConfig{}
	media.Runtime = new(*media.Runtime)
	media.Runtime.AdapterContract = voyageEmbeddingAdapter
	cfg.EmbeddingProfiles["media"] = media
	recipe := cfg.ProcessingProfiles["archive"]
	recipe.Embeddings = append(recipe.Embeddings, "media")
	cfg.ProcessingProfiles["archive"] = recipe
	target := processing.LocalTextTarget{Server: "auto", EmbeddingServer: "http://127.0.0.1:12345/api/v1/servers/example", EmbeddingModel: "gemini-embedding-2", EmbeddingDimensions: 2}
	candidate, retained, err := localTextServerCandidate(cfg, target, "archive")
	require.NoError(t, err)
	require.Contains(t, candidate.ProcessingProfiles["archive"].Embeddings, "media-kit")
	require.Equal(t, "media", candidate.EmbeddingProfiles["media-kit"].Runtime.MediaFallbackBinding)
	require.NotEmpty(t, retained)
	target.EmbeddingModel = "text-model"
	candidate, retained, err = localTextServerCandidate(cfg, target, "archive")
	require.NoError(t, err)
	require.NotContains(t, candidate.ProcessingProfiles["archive"].Embeddings, "media-kit")
	require.NotEmpty(t, retained)
}

type serverTestBackend struct{}

func (serverTestBackend) Encode(_ context.Context, dims int, inputs []embedclient.Input) ([][]float32, error) {
	vectors := make([][]float32, len(inputs))
	for i := range inputs {
		vectors[i] = make([]float32, dims)
		vectors[i][0] = 1
	}
	return vectors, nil
}

func addSyntheticVoyageMedia(t *testing.T, root string, cfg config.Config) config.Config {
	t.Helper()
	p := cfg.EmbeddingProfiles["semantic"]
	p.InputKind = string(document.EmbeddingInputOriginalFile)
	p.Model, p.Dimensions = voyage.DefaultModel, voyage.DefaultDimension
	p.DescriptorID = voyage.EmbeddingProviderID
	p.DocumentFormatter, p.QueryFormatter = voyage.EmbeddingDocumentFormatterV1, voyage.EmbeddingQueryFormatterV1
	p.CompatibilityID = "voyage/test-space/v1"
	p.ModelInput = config.EmbeddingModelInputConfig{Profile: "custom/v1", CompatibilityID: p.CompatibilityID, Document: config.EmbeddingModelInputEncoderConfig{Mode: "document", Template: "{{content}}"}, Query: config.EmbeddingModelInputEncoderConfig{Mode: "query", Template: "{{content}}"}}
	p.Chunk, p.MaxInputTokens = config.EmbeddingChunkConfig{}, 0
	p.MaxInputBytes, p.MaxBatchItems, p.MaxResponseBytes = 1<<20, 8, 1<<20
	p.TrustBoundary = string(document.EmbeddingTrustHostedProvider)
	p.Runtime = new(*p.Runtime)
	p.Runtime.AdapterContract, p.Runtime.Endpoint = voyageEmbeddingAdapter, voyage.DefaultEndpoint
	p.Runtime.DeploymentEpoch, p.Runtime.ProviderRevisionHeader = "", ""
	p.Runtime.MaxRequestBytes = voyage.MaxRequestBytes
	p.Runtime.AllowedCIDRs = []string{"0.0.0.0/0", "::/0"}
	p.Runtime.CapabilityManifest = filepath.Join(root, "voyage-capabilities.json")
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: p.Model, Dimension: p.Dimensions, Media: media.Policy{MaxBytes: p.MaxInputBytes, AllowStill: true, AllowVideo: true}, MaxBatchItems: p.MaxBatchItems, MaxRequestBytes: p.Runtime.MaxRequestBytes, MaxResponseBytes: p.MaxResponseBytes})
	require.NoError(t, err)
	manifest, err := voyagetest.SyntheticManifest(policy)
	require.NoError(t, err)
	f, err := os.Create(p.Runtime.CapabilityManifest)
	require.NoError(t, err)
	require.NoError(t, voyage.EncodeCapabilityManifest(f, manifest))
	require.NoError(t, f.Close())
	p.Runtime.ModelRevision, err = voyage.DirectFileModelRevision(policy, manifest)
	require.NoError(t, err)
	cfg.EmbeddingProfiles["media"] = p
	input, err := cfg.EmbeddingModelInput("media")
	require.NoError(t, err)
	provider := voyageRuntimeProfile(p, input)
	provider.Mode, provider.Policy, provider.CapabilityManifest = voyage.EmbeddingModeDirectFile, policy, manifest
	provider, err = finalizeVoyageEmbeddingDescriptor(provider)
	require.NoError(t, err)
	p.DescriptorFingerprint = provider.Descriptor.Fingerprint
	cfg.EmbeddingProfiles["media"] = p
	recipe := cfg.ProcessingProfiles["archive"]
	recipe.Rendition = ""
	recipe.Embeddings = []string{"media"}
	recipe.RetainSanitizedMarkdown = false
	recipe.RetainProviderMarkdown = false
	cfg.ProcessingProfiles["photos"] = recipe
	return cfg
}

func TestLocalTextServerDaemonRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("DO_NOT_TRACK", "1")
	t.Setenv("DOCBANK_TEST_SOURCE_KEY", "synthetic")
	t.Setenv("DOCBANK_TEST_EMBED_KEY", "synthetic-server-secret")
	handler := embedserver.Handler(serverTestBackend{}, []string{"gemini-embedding-2"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-server-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.StripPrefix("/api/v1/servers/example", handler).ServeHTTP(w, r)
	}))
	defer server.Close()
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	cfg := localTextSourceConfig(t, "http://127.0.0.1:12345", "source-revision", "prefix-free", "DOCBANK_TEST_SOURCE_KEY")
	cfg = addSyntheticVoyageMedia(t, root, cfg)
	writeDaemonEmbeddingConfig(t, root, cfg)
	configPath := filepath.Join(root, "config.toml")
	configData, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(configData), "[embedding_profiles.media.runtime]\n")
	configData = bytes.Replace(configData, []byte("[embedding_profiles.media.runtime]\n"), []byte(fmt.Sprintf("[embedding_profiles.media.runtime]\ncapability_manifest = %q\n", cfg.EmbeddingProfiles["media"].Runtime.CapabilityManifest)), 1)
	require.NoError(t, os.WriteFile(configPath, configData, 0600))
	stop := startServe(t)
	defer func() { stop() }()
	waitForDaemon(t, root)
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	body := &api.LocalTextPlanRequest{Server: "auto", Target: &processing.LocalTextTarget{EmbeddingServer: server.URL + "/api/v1/servers/example", EmbeddingModel: "gemini-embedding-2", EmbeddingDimensions: 2, EmbeddingAPIKeyEnv: "DOCBANK_TEST_EMBED_KEY"}}
	plan, err := connection.API().PlanLocalText(t.Context(), &apiclient.PlanLocalTextRequestOptions{Body: body})
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprint(plan), "synthetic-server-secret")
	t.Setenv("DOCBANK_TEST_EMBED_KEY", "")
	_, err = connection.API().ApplyLocalText(t.Context(), &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: body.Server, Target: body.Target, PlanFingerprint: plan.Fingerprint, Consent: true}})
	require.Error(t, err)
	choice, err := readLocalText(root)
	require.NoError(t, err)
	require.Empty(t, choice.Profile)
	t.Setenv("DOCBANK_TEST_EMBED_KEY", "synthetic-server-secret")
	_, err = connection.API().ApplyLocalText(t.Context(), &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: body.Server, Target: body.Target, PlanFingerprint: plan.Fingerprint, Consent: true}})
	require.NoError(t, err)
	require.NoError(t, connection.Close())
	choice, err = readLocalText(root)
	require.NoError(t, err)
	require.Equal(t, []string{"archive", "photos"}, choice.Profiles)
	require.Equal(t, "DOCBANK_TEST_EMBED_KEY", choice.EmbeddingAPIKeyEnv)
	saved, err := os.ReadFile(filepath.Join(root, "local-text.json"))
	require.NoError(t, err)
	require.NotContains(t, string(saved), "synthetic-server-secret")
	stop()
	cfg.ProcessingProfiles["new-recipe"] = cfg.ProcessingProfiles["archive"]
	original, err := cfg.ProcessingProfile("new-recipe")
	require.NoError(t, err)
	_, originalFingerprint, err := document.CanonicalProfile(original.Document)
	require.NoError(t, err)
	writeDaemonEmbeddingConfig(t, root, cfg)
	configData, err = os.ReadFile(configPath)
	require.NoError(t, err)
	configData = bytes.Replace(configData, []byte("[embedding_profiles.media.runtime]\n"), []byte(fmt.Sprintf("[embedding_profiles.media.runtime]\ncapability_manifest = %q\n", cfg.EmbeddingProfiles["media"].Runtime.CapabilityManifest)), 1)
	require.NoError(t, os.WriteFile(configPath, configData, 0600))
	stop = startServe(t)
	waitForDaemon(t, root)
	connection, err = daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	defer connection.Close()
	restoredProfiles, err := connection.API().ListDocumentProcessingProfiles(t.Context(), &apiclient.ListDocumentProcessingProfilesRequestOptions{})
	require.NoError(t, err)
	foundNewRecipe := false
	for _, profile := range *restoredProfiles {
		if profile.Name == "new-recipe" {
			foundNewRecipe = true
			require.Equal(t, originalFingerprint.Profile, profile.Fingerprint)
		}
	}
	require.True(t, foundNewRecipe)
	text := "lease renewal document"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	layout, err := home.Resolve()
	require.NoError(t, err)
	catalog, err := store.Open(t.Context(), layout.DBPath())
	require.NoError(t, err)
	rootID := catalog.RootID()
	require.NoError(t, catalog.Close())
	_, err = connection.Upload(t.Context(), rootID, "new.txt", "text/plain", hash, int64(len(text)), strings.NewReader(text))
	require.NoError(t, err)
	settleDaemon(t)
	imageData := mediatest.PNG(16, 16, color.White)
	imageHash := fmt.Sprintf("%x", sha256.Sum256(imageData))
	imageReceipt, err := connection.Upload(t.Context(), rootID, "image.png", "image/png", imageHash, int64(len(imageData)), bytes.NewReader(imageData))
	require.NoError(t, err)
	settleDaemon(t)
	imageResults, err := connection.SearchDocuments(t.Context(), api.DocumentSearchRequest{Profile: "photos", Query: "image", Mode: "semantic", Limit: 2, Scope: &api.DocumentSearchScope{Kind: "all_live"}})
	require.NoError(t, err)
	require.NotEmpty(t, imageResults.Results)
	require.Equal(t, imageReceipt.Node.ID, imageResults.Results[0].NodeID)
	results, err := connection.SearchDocuments(t.Context(), api.DocumentSearchRequest{Profile: "archive", Query: "lease renewal", Mode: "semantic", Limit: 2, Scope: &api.DocumentSearchScope{Kind: "all_live"}})
	require.NoError(t, err)
	require.NotEmpty(t, results.Results)
}

func TestLocalTextServerFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--embedding-server", "http://example.test"},
		{"--embedding-server", "bad-url", "--embedding-model", "model", "--embedding-dimensions", "2"},
		{"--embedding-server", "http://example.test", "--embedding-model", "model", "--embedding-dimensions", "1000001"},
		{"--embedding-server", "", "--embedding-model", "model", "--embedding-dimensions", "2"},
		{"--embedding-server", "http://example.test", "--embedding-model", "model", "--embedding-dimensions", "2", "--server", "auto"},
	} {
		cmd := newProcessingLocalCommand()
		cmd.SetArgs(args)
		require.Error(t, cmd.Execute())
	}
}

func TestLocalTextServerDoesNotSelectBuiltInProfile(t *testing.T) {
	cfg := localTextSourceConfig(t, "http://127.0.0.1:12345", "source-revision", "prefix-free", "DOCBANK_TEST_SOURCE_KEY")
	target := processing.LocalTextTarget{Server: "auto", EmbeddingServer: "http://127.0.0.1:12345/v1", EmbeddingModel: "model", EmbeddingDimensions: 2}
	_, _, err := localTextServerCandidate(cfg, target, config.LocalTextProfile)
	require.ErrorIs(t, err, processing.ErrProfileNotConfigured)
}

func TestLocalTextServerIdentitySeparatesModelAndDimensions(t *testing.T) {
	cfg := localTextSourceConfig(t, "http://127.0.0.1:12345", "source-revision", "prefix-free", "DOCBANK_TEST_SOURCE_KEY")
	target := processing.LocalTextTarget{Server: "auto", EmbeddingServer: "http://127.0.0.1:12345/v1", EmbeddingModel: "a", EmbeddingDimensions: 12}
	first, _, err := localTextServerCandidate(cfg, target, "archive")
	require.NoError(t, err)
	target.EmbeddingModel, target.EmbeddingDimensions = "a1", 2
	second, _, err := localTextServerCandidate(cfg, target, "archive")
	require.NoError(t, err)
	require.NotEqual(t, first.EmbeddingProfiles["semantic"].DisclosureFingerprint, second.EmbeddingProfiles["semantic"].DisclosureFingerprint)
}
