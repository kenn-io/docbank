package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedlocal"
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

func TestLocalTextServerPairsMedia(t *testing.T) {
	cfg := localTextSourceConfig(t, "http://127.0.0.1:12345", "source-revision", "prefix-free", "DOCBANK_TEST_SOURCE_KEY")
	cfg = addSyntheticVoyageMedia(t, t.TempDir(), cfg)
	original := cfg.EmbeddingProfiles["semantic"]
	target := processing.LocalTextTarget{Server: "auto", EmbeddingServer: "http://127.0.0.1:12345/api/v1/servers/example", EmbeddingModel: "gemini-embedding-2", EmbeddingDimensions: 2}
	candidate, retained, err := localTextCandidate(cfg, target, "photos")
	require.NoError(t, err)
	require.Contains(t, candidate.ProcessingProfiles["photos"].Embeddings, "media-kit")
	require.Equal(t, "media", candidate.EmbeddingProfiles["media-kit"].Runtime.MediaFallbackBinding)
	require.Equal(t, original, candidate.EmbeddingProfiles["semantic"])
	require.NotEmpty(t, retained)
	require.Contains(t, retained[0], "Voyage direct-file embeddings don't support text queries")
	target.EmbeddingModel = "text-model"
	candidate, retained, err = localTextCandidate(cfg, target, "photos")
	require.ErrorIs(t, err, processing.ErrProfileNotConfigured)
	require.NotContains(t, candidate.ProcessingProfiles["photos"].Embeddings, "media-kit")
	require.Contains(t, retained[0], "Voyage direct-file embeddings don't support text queries")
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
	t.Run("media profile", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("USERPROFILE", os.Getenv("HOME"))
		t.Setenv("DO_NOT_TRACK", "1")
		t.Setenv("DOCBANK_TEST_SOURCE_KEY", "synthetic")
		t.Setenv("DOCBANK_TEST_EMBED_KEY", "synthetic-server-secret")
		var requests atomic.Int64
		handler := embedserver.Handler(serverTestBackend{}, []string{"gemini-embedding-2"})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
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
		cfg.ProcessingProfiles["z-unused"] = cfg.ProcessingProfiles["archive"]
		delete(cfg.ProcessingProfiles, "archive")
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
		layout, err := home.Resolve()
		require.NoError(t, err)
		catalog, err := store.Open(t.Context(), layout.DBPath())
		require.NoError(t, err)
		rootID := catalog.RootID()
		require.NoError(t, catalog.Close())
		beforeData := mediatest.PNG(16, 16, color.White)
		beforeHash := fmt.Sprintf("%x", sha256.Sum256(beforeData))
		beforeReceipt, err := connection.Upload(t.Context(), rootID, "before.png", "image/png", beforeHash, int64(len(beforeData)), bytes.NewReader(beforeData))
		require.NoError(t, err)
		body := &api.LocalTextPlanRequest{Server: "auto", Target: &processing.LocalTextTarget{EmbeddingServer: server.URL + "/api/v1/servers/example", EmbeddingModel: "gemini-embedding-2", EmbeddingDimensions: 2, EmbeddingAPIKeyEnv: "DOCBANK_TEST_EMBED_KEY"}}
		plan, err := connection.API().PlanLocalText(t.Context(), &apiclient.PlanLocalTextRequestOptions{Body: body})
		require.NoError(t, err)
		planJSON, err := json.Marshal(plan)
		require.NoError(t, err)
		require.NotContains(t, string(planJSON), "synthetic-server-secret")
		t.Setenv("DOCBANK_TEST_EMBED_KEY", "")
		require.ErrorContains(t, localTextServerStartup(t.Context(), *body.Target), "secret is unset or empty")
		beforeRequests := requests.Load()
		_, err = connection.API().ApplyLocalText(t.Context(), &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: body.Server, Target: body.Target, PlanFingerprint: plan.Fingerprint, Consent: true}})
		require.ErrorContains(t, err, "processing_failed")
		require.Equal(t, beforeRequests, requests.Load())
		choice, err := readLocalText(root)
		require.NoError(t, err)
		require.Empty(t, choice.Profile)
		t.Setenv("DOCBANK_TEST_EMBED_KEY", "synthetic-server-secret")
		require.NoError(t, localTextServerStartup(t.Context(), *body.Target))
		_, err = connection.API().ApplyLocalText(t.Context(), &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: body.Server, Target: body.Target, PlanFingerprint: plan.Fingerprint, Consent: true}})
		require.NoError(t, err)
		require.NoError(t, connection.Close())
		choice, err = readLocalText(root)
		require.NoError(t, err)
		require.Equal(t, "photos", choice.Profile)
		require.Equal(t, "DOCBANK_TEST_EMBED_KEY", choice.EmbeddingAPIKeyEnv)
		saved, err := os.ReadFile(filepath.Join(root, "local-text.json"))
		require.NoError(t, err)
		require.NotContains(t, string(saved), "synthetic-server-secret")
		candidate, _, err := localTextCandidate(cfg, *body.Target, "photos")
		require.NoError(t, err)
		projected, err := candidate.ProcessingProfile("photos")
		require.NoError(t, err)
		_, projectedFingerprint, err := document.CanonicalProfile(projected.Document)
		require.NoError(t, err)
		catalog, err = store.Open(t.Context(), layout.DBPath())
		require.NoError(t, err)
		rebuilt, err := catalog.HasEmbeddingHeads(t.Context(), projectedFingerprint.Profile, "media-kit")
		require.NoError(t, err)
		require.True(t, rebuilt, "setup rebuilt the existing image before saving")
		require.NoError(t, catalog.Close())
		stop()
		original, err := cfg.ProcessingProfile("z-unused")
		require.NoError(t, err)
		_, originalFingerprint, err := document.CanonicalProfile(original.Document)
		require.NoError(t, err)
		stop = startServe(t)
		waitForDaemon(t, root)
		connection, err = daemonconn.Ensure(t.Context())
		require.NoError(t, err)
		defer connection.Close()
		restoredProfiles, err := connection.API().ListDocumentProcessingProfiles(t.Context(), &apiclient.ListDocumentProcessingProfilesRequestOptions{})
		require.NoError(t, err)
		foundUnused := false
		for _, profile := range *restoredProfiles {
			if profile.Name == "z-unused" {
				foundUnused = true
				require.Equal(t, originalFingerprint.Profile, profile.Fingerprint)
			}
		}
		require.True(t, foundUnused)
		beforeResults, err := connection.SearchDocuments(t.Context(), api.DocumentSearchRequest{Profile: "photos", Query: "image", Mode: "semantic", Limit: 2, Scope: &api.DocumentSearchScope{Kind: "all_live"}})
		require.NoError(t, err)
		require.NotEmpty(t, beforeResults.Results)
		require.Equal(t, beforeReceipt.Node.ID, beforeResults.Results[0].NodeID)
		imageData := mediatest.PNG(17, 17, color.White)
		imageHash := fmt.Sprintf("%x", sha256.Sum256(imageData))
		imageReceipt, err := connection.Upload(t.Context(), rootID, "image.png", "image/png", imageHash, int64(len(imageData)), bytes.NewReader(imageData))
		require.NoError(t, err)
		settleDaemon(t)
		imageResults, err := connection.SearchDocuments(t.Context(), api.DocumentSearchRequest{Profile: "photos", Query: "image", Mode: "semantic", Limit: 2, Scope: &api.DocumentSearchScope{Kind: "all_live"}})
		require.NoError(t, err)
		require.Len(t, imageResults.Results, 2)
		require.Contains(t, []int64{imageResults.Results[0].NodeID, imageResults.Results[1].NodeID}, imageReceipt.Node.ID)
		unused, err := cfg.ProcessingProfile("z-unused")
		require.NoError(t, err)
		_, unusedFingerprint, err := document.CanonicalProfile(unused.Document)
		require.NoError(t, err)
		catalog, err = store.Open(t.Context(), layout.DBPath())
		require.NoError(t, err)
		unusedIndexed, err := catalog.HasEmbeddingHeads(t.Context(), unusedFingerprint.Profile, "semantic")
		require.NoError(t, err)
		require.False(t, unusedIndexed)
		require.NoError(t, catalog.Close())

	})
	t.Run("built-in profile", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("DOCBANK_HOME", root)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("USERPROFILE", os.Getenv("HOME"))
		t.Setenv("DO_NOT_TRACK", "1")
		require.NoError(t, saveLocalText(root, config.LocalTextConfig{Profile: config.LocalTextProfile, Server: "local"}))
		server := httptest.NewServer(embedserver.Handler(serverTestBackend{}, []string{"model"}))
		defer server.Close()
		stop := startServe(t)
		defer func() { stop() }()
		waitForDaemon(t, root)
		connection, err := daemonconn.Ensure(t.Context())
		require.NoError(t, err)
		target := &processing.LocalTextTarget{EmbeddingServer: server.URL + "/v1", EmbeddingModel: "model", EmbeddingDimensions: 2}
		plan, err := connection.API().PlanLocalText(t.Context(), &apiclient.PlanLocalTextRequestOptions{Body: &api.LocalTextPlanRequest{Server: "auto", Target: target}})
		require.NoError(t, err)
		require.Equal(t, config.LocalTextProfile, plan.Profile)
		_, err = connection.API().ApplyLocalText(t.Context(), &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: "auto", Target: target, PlanFingerprint: plan.Fingerprint, Consent: true}})
		require.NoError(t, err)
		require.NoError(t, connection.Close())
		stop()
		stop = startServe(t)
		waitForDaemon(t, root)
		choice, err := readLocalText(root)
		require.NoError(t, err)
		require.Equal(t, config.LocalTextProfile, choice.Profile)
		require.Equal(t, server.URL+"/v1", choice.EmbeddingServer)
		local := httptest.NewServer(localTextDiscoveryHandler(embedlocal.TextModel))
		defer local.Close()
		dir, err := embedlocal.DefaultDir()
		require.NoError(t, err)
		endpoint, err := kitdaemon.ParseEndpoint(local.URL, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
		require.NoError(t, err)
		_, err = (kitdaemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(kitdaemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
		require.NoError(t, err)
		connection, err = daemonconn.Ensure(t.Context())
		require.NoError(t, err)
		defer connection.Close()
		localPlan, err := connection.API().PlanLocalText(t.Context(), &apiclient.PlanLocalTextRequestOptions{Body: &api.LocalTextPlanRequest{Server: "local"}})
		require.NoError(t, err)
		_, err = connection.API().ApplyLocalText(t.Context(), &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: "local", PlanFingerprint: localPlan.Fingerprint, Consent: true}})
		require.NoError(t, err)
		choice, err = readLocalText(root)
		require.NoError(t, err)
		require.Empty(t, choice.EmbeddingServer)

	})
}

func TestLocalTextServerFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("DOCBANK_HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--embedding-server", "http://example.test"}, "base_url, model, and positive dims"},
		{[]string{"--embedding-server", "bad-url", "--embedding-model", "model", "--embedding-dimensions", "2"}, "endpoint"},
		{[]string{"--embedding-server", "", "--embedding-model", "model", "--embedding-dimensions", "2"}, "base_url, model, and positive dims"},
		{[]string{"--embedding-server", "http://example.test", "--embedding-model", "model", "--embedding-dimensions", "2", "--server", "auto"}, "--server cannot be combined"},
	} {
		cmd := newProcessingLocalCommand()
		cmd.SetArgs(tc.args)
		require.ErrorContains(t, cmd.Execute(), tc.want)
	}
}

func TestLocalTextServerIdentitySeparatesModelAndDimensions(t *testing.T) {
	cfg := localTextSourceConfig(t, "http://127.0.0.1:12345", "source-revision", "prefix-free", "DOCBANK_TEST_SOURCE_KEY")
	target := processing.LocalTextTarget{Server: "auto", EmbeddingServer: "http://127.0.0.1:12345/v1", EmbeddingModel: "a", EmbeddingDimensions: 12}
	first, _, err := localTextCandidate(cfg, target, "archive")
	require.NoError(t, err)
	target.EmbeddingModel, target.EmbeddingDimensions = "a1", 2
	second, _, err := localTextCandidate(cfg, target, "archive")
	require.NoError(t, err)
	require.NotEqual(t, first.EmbeddingProfiles["semantic"].DisclosureFingerprint, second.EmbeddingProfiles["semantic"].DisclosureFingerprint)
	target.EmbeddingDimensions = 1_000_001
	_, _, err = localTextCandidate(cfg, target, "archive")
	require.ErrorContains(t, err, "dimension")
}
