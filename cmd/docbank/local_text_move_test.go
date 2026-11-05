package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
)

func localTextSourceConfig(t *testing.T, origin, revision, format string) config.Config {
	t.Helper()
	cfg, err := config.Default().WithLocalText()
	require.NoError(t, err)
	recipe := cfg.ProcessingProfiles[config.LocalTextProfile]
	recipe.Embeddings = []string{"semantic"}
	cfg.ProcessingProfiles = map[string]config.ProcessingProfileConfig{"archive": recipe}
	p := cfg.EmbeddingProfiles[config.LocalTextProfile]
	p.Runtime = new(*p.Runtime)
	p.Runtime.Managed = false
	p.Runtime.Endpoint = origin
	p.Runtime.AllowedCIDRs = []string{"127.0.0.0/8"}
	p.Runtime.ProxyMode = "disabled"
	p.Runtime.ConnectTimeout, p.Runtime.KeepAlive, p.Runtime.TLSHandshakeTimeout = config.Duration(time.Second), config.Duration(time.Second), config.Duration(time.Second)
	p.Runtime.ModelRevision = revision
	p.Runtime.DeploymentEpoch = p.Runtime.ModelRevision
	p.Chunk.MaxTokens, p.Chunk.OverlapTokens = 800, 100
	if format == "other-model" {
		p.Model = "other-model"
	}
	if format == "prefix-free" {
		p.ModelInput = config.EmbeddingModelInputConfig{Profile: "custom/v1", CompatibilityID: p.CompatibilityID,
			Document: config.EmbeddingModelInputEncoderConfig{Mode: "text", Template: "{{content}}"},
			Query:    config.EmbeddingModelInputEncoderConfig{Mode: "text", Template: "{{content}}"}}
	}
	cfg.EmbeddingProfiles[config.LocalTextProfile] = p
	input, err := cfg.EmbeddingModelInput(config.LocalTextProfile)
	require.NoError(t, err)
	descriptor, _, err := finalizeOpenAIEmbeddingDescriptor(openaicompat.Profile{Origin: origin,
		EgressPolicy: providerEgressPolicy(p.Runtime.ProviderEgressConfig), Descriptor: config.EmbeddingDescriptor(p, input), ModelInput: input, SecretBinding: p.CredentialBinding,
		DeploymentEpoch: p.Runtime.DeploymentEpoch, RequestTimeout: p.Runtime.RequestTimeout.Std(),
		MaxBatchItems: p.MaxBatchItems, MaxInputBytes: p.MaxInputBytes, MaxResponseBytes: p.MaxResponseBytes, MaxRequestBytes: p.Runtime.MaxRequestBytes})
	require.NoError(t, err)
	p.DescriptorFingerprint = descriptor.Fingerprint
	cfg.EmbeddingProfiles = map[string]config.EmbeddingProfileConfig{"semantic": p}
	cfg.CredentialBindings = map[string]config.CredentialBindingConfig{"local-text": {EnvironmentVariable: "DOCBANK_TEST_MOVE_KEY"}}
	return cfg
}

func TestLocalTextKeepsOllamaVectors(t *testing.T) {
	const name = "archive"
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCBANK_TEST_MOVE_KEY", "synthetic-key")
	t.Setenv("DOCBANK_HOME", t.TempDir())
	var localDocuments atomic.Int64
	handler := func(counter *atomic.Int64) http.Handler {
		ping := kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{Service: embedlocal.Service, Version: embedlocal.Version})
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/embeddings" {
				ping.ServeHTTP(w, r)
				return
			}
			var request struct {
				Input []string `json:"input"`
				Model string   `json:"model"`
			}
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
				return
			}
			data := make([]map[string]any, len(request.Input))
			for i, text := range request.Input {
				if text != "lease renewal" && !strings.HasPrefix(text, "search_query:") && !strings.HasPrefix(text, "query:") {
					counter.Add(1)
				}
				vector := make([]float32, 768)
				if strings.Contains(text, "lease") {
					vector[0] = 1
				} else {
					vector[1] = 1
				}
				data[i] = map[string]any{"object": "embedding", "index": i, "embedding": vector}
			}
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"object": "list", "model": request.Model, "data": data}))
		})
	}
	var ollamaDocuments atomic.Int64
	ollama := httptest.NewServer(handler(&ollamaDocuments))
	defer ollama.Close()
	local := httptest.NewServer(handler(&localDocuments))
	defer local.Close()
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	endpoint, err := kitdaemon.ParseEndpoint(local.URL, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
	require.NoError(t, err)
	_, err = (kitdaemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(kitdaemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
	require.NoError(t, err)
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(t.Context(), layout.DBPath())
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, blobs.Close()) }()
	cfg := localTextSourceConfig(t, ollama.URL, "ollama-nomic-september", "prefix-free")
	unused := cfg.EmbeddingProfiles["semantic"]
	unused.Chunk.MaxTokens = 600
	cfg.EmbeddingProfiles["unused"] = unused
	recipe := cfg.ProcessingProfiles[name]
	recipe.Embeddings = []string{"unused"}
	cfg.ProcessingProfiles["aaa-unused"] = recipe
	cfg.EmbeddingProfiles["other"] = localTextSourceConfig(t, ollama.URL, "other-revision", "other-model").EmbeddingProfiles["semantic"]
	other := cfg.ProcessingProfiles[name]
	other.Embeddings = []string{"other"}
	cfg.ProcessingProfiles["bbb-other"] = other
	serviceFor := func(cfg config.Config) *processing.Service {
		bundle, err := configureEmbeddingRuntimeBundle(cfg)
		require.NoError(t, err)
		profiles, err := executableProcessingProfiles(cfg, bundle)
		require.NoError(t, err)
		service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: catalog, Blobs: blobs, Gate: api.NewOperationGate(), Profiles: profiles, Principal: "daemon:operator", Scope: "document-processing", SpoolDirectory: layout.BlobTmpDir()})
		require.NoError(t, err)
		t.Cleanup(service.Stop)
		return service
	}
	service := serviceFor(cfg)
	require.NoError(t, saveLocalText(layout.Root, config.LocalTextConfig{Profile: config.LocalTextProfile, Server: "local"}))
	bundle, err := configureEmbeddingRuntimeBundle(cfg)
	require.NoError(t, err)
	profiles, err := executableProcessingProfiles(cfg, bundle)
	require.NoError(t, err)
	compatible, err := localTextCompatible(cfg, profiles)
	require.NoError(t, err)
	setup := processing.LocalTextSetup{Service: service, Selected: config.LocalTextProfile, Startup: localTextStartup,
		Compatible: compatible,
		Candidate: func(mode, name string) (*processing.Service, error) {
			candidate, err := localTextCandidate(cfg, mode, name)
			if err != nil {
				return nil, err
			}
			return serviceFor(candidate), nil
		},
		Save: func(name, mode string) error {
			return saveLocalText(layout.Root, config.LocalTextConfig{Profile: name, Server: mode})
		}}
	var nodes []store.Node
	add := func(name, text string) store.Node {
		written, err := blobs.WriteDetailedContext(t.Context(), strings.NewReader(text))
		require.NoError(t, err)
		encoding, err := written.EncodingName()
		require.NoError(t, err)
		node, err := catalog.CreateFile(t.Context(), catalog.RootID(), name, written.Hash, written.Size, "text/plain", store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize})
		require.NoError(t, err)
		return node
	}
	process := func(service *processing.Service, node store.Node, profile string) {
		selector := processing.Selector{Profile: profile, NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
		plan, err := service.Plan(t.Context(), selector)
		require.NoError(t, err)
		_, err = service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
		require.NoError(t, err)
	}
	empty, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	require.Equal(t, "Build local index", empty.VectorEffect)
	process(service, add("other.txt", "another model"), "bbb-other")
	rebuild, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	require.Contains(t, rebuild.VectorEffect, "Rebuild required")
	for i, text := range []string{"lease renewal terms", "garden planting instructions"} {
		node := add(fmt.Sprintf("document-%d.txt", i), text)
		nodes = append(nodes, node)
		process(service, node, name)
	}
	search := func(service *processing.Service) []string {
		report, err := service.Search(t.Context(), processing.SearchRequest{Profile: name, Query: "lease renewal", Mode: "semantic", Limit: 2, AllLive: &store.SearchOptions{}})
		require.NoError(t, err)
		var ids []string
		for _, result := range report.Results {
			ids = append(ids, result.Document.ContentVersionID)
		}
		return ids
	}
	before := search(service)
	require.Equal(t, nodes[0].CurrentVersionID, before[0])
	plan, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	require.Equal(t, name, plan.Profile, "an unused earlier recipe is skipped")
	require.Equal(t, "Reuse existing vectors", plan.VectorEffect)
	_, err = setup.Apply(t.Context(), "local", plan.Fingerprint)
	require.NoError(t, err)
	choice, err := readLocalText(layout.Root)
	require.NoError(t, err)
	require.Equal(t, name, choice.Profile)
	candidate, err := localTextCandidate(cfg, choice.Server, choice.Profile)
	require.NoError(t, err)
	service.Stop()
	ollama.Close()
	moved := serviceFor(candidate)
	setup.Service, setup.Selected = moved, name
	rerun, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	require.Equal(t, name, rerun.Profile, "rerunning before backfill keeps the approved recipe")
	require.Equal(t, "Reuse existing vectors", rerun.VectorEffect)
	_, err = setup.Apply(t.Context(), "local", rerun.Fingerprint)
	require.NoError(t, err)
	choice, err = readLocalText(layout.Root)
	require.NoError(t, err)
	require.Equal(t, name, choice.Profile)
	for _, node := range nodes {
		process(moved, node, name)
	}
	require.Zero(t, localDocuments.Load(), "unchanged documents reuse their retained vectors")
	require.Equal(t, before, search(moved))
	process(moved, add("new.txt", "new lease renewal document"), name)
	require.Equal(t, int64(1), localDocuments.Load())
	moved.Stop()
	builtinConfig, err := localTextCandidate(cfg, "local", "")
	require.NoError(t, err)
	builtin := serviceFor(builtinConfig)
	process(builtin, nodes[0], config.LocalTextProfile)
	setup.Service, setup.Selected = builtin, config.LocalTextProfile
	repeated, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	require.Equal(t, "Reuse existing vectors", repeated.VectorEffect)
	builtin.Stop()
	writeDaemonEmbeddingConfig(t, layout.Root, cfg)
	t.Setenv("DOCBANK_HOME", layout.Root)
	t.Setenv("DO_NOT_TRACK", "1")
	stop := startServe(t)
	defer stop()
	waitForDaemon(t, layout.Root)
	settleDaemon(t)
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, connection.Close()) }()
	outcome, err := tryAutomaticSearch(t.Context(), connection, "lease renewal", 2, daemonconn.SearchOptions{})
	require.NoError(t, err)
	require.NotNil(t, outcome.report, "automatic search must use the local provider after Ollama stops")
	require.Equal(t, before[0], outcome.report.Results[0].ContentVersionID)
}
