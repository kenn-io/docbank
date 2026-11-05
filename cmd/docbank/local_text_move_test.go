package main

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
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
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/kit/secretref"
)

func localTextSourceConfig(t *testing.T, origin, revision, format, credentialEnvironment string) config.Config {
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
	if format == "revision-header" {
		p.Runtime.DeploymentEpoch, p.Runtime.ProviderRevisionHeader = "", "X-Model-Revision"
	}
	p.Chunk.MaxTokens, p.Chunk.OverlapTokens = 800, 100
	p.AuthorizationFingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic-source-authorization")))
	p.DisclosureFingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic-source-disclosure")))
	p.CredentialBinding = "credential:source-embedding"
	if format == "dot-product" {
		p.Metric = "dot_product"
	}
	if format == "unconstrained-normalization" {
		p.Normalization = "none"
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
		DeploymentEpoch: p.Runtime.DeploymentEpoch, ProviderRevisionHeader: p.Runtime.ProviderRevisionHeader, RequestTimeout: p.Runtime.RequestTimeout.Std(),
		MaxBatchItems: p.MaxBatchItems, MaxInputBytes: p.MaxInputBytes, MaxResponseBytes: p.MaxResponseBytes, MaxRequestBytes: p.Runtime.MaxRequestBytes})
	require.NoError(t, err)
	p.DescriptorFingerprint = descriptor.Fingerprint
	cfg.EmbeddingProfiles = map[string]config.EmbeddingProfileConfig{"semantic": p}
	cfg.CredentialBindings = map[string]config.CredentialBindingConfig{"source-embedding": {EnvironmentVariable: credentialEnvironment}}
	return cfg
}

func TestLocalTextKeepsOllamaVectors(t *testing.T) {
	for _, variant := range []struct{ name, binding, format string }{
		{"semantic", "semantic", "prefix-free"},
		{"revision-header", "semantic", "revision-header"},
		{"local-text", config.LocalTextProfile, "prefix-free"},
		{"dot-product", "semantic", "dot-product"},
		{"unconstrained-normalization", "semantic", "unconstrained-normalization"},
		{"kit-embedder", "semantic", "kit-embedder"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			binding := variant.binding
			const name, credentialEnvironment = "archive", "DOCBANK_TEST_MOVE_KEY"
			t.Setenv("HOME", t.TempDir())
			t.Setenv(credentialEnvironment, "synthetic-key")
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
					w.Header().Set("X-Model-Revision", "ollama-nomic-september")
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
			cfg := localTextSourceConfig(t, ollama.URL, "ollama-nomic-september", variant.format, credentialEnvironment)
			if binding == config.LocalTextProfile {
				cfg.EmbeddingProfiles[binding] = cfg.EmbeddingProfiles["semantic"]
				delete(cfg.EmbeddingProfiles, "semantic")
				recipe := cfg.ProcessingProfiles[name]
				recipe.Embeddings = []string{binding}
				recipe.MaxSegmentRunes = 2048
				cfg.ProcessingProfiles[name] = recipe
			}
			if variant.format == "kit-embedder" {
				p := cfg.EmbeddingProfiles[binding]
				p.Embedder = &embedconfig.Embedder{BaseURL: p.Runtime.Endpoint + "/v1", Model: p.Model, Dims: p.Dimensions, FingerprintSalt: p.Runtime.ModelRevision,
					BatchSize: p.MaxBatchItems, TimeoutSeconds: int(p.Runtime.RequestTimeout.Std() / time.Second), APIKey: secretref.Ref{Env: credentialEnvironment}}
				cfg.EmbeddingProfiles[binding] = p
				writeDaemonEmbeddingConfig(t, layout.Root, cfg)
				cfg, err = config.Load(layout.Root)
				require.NoError(t, err)
			}
			require.NoError(t, cfg.Validate())
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
			for i, text := range []string{"lease renewal terms", "garden planting instructions"} {
				node := add(fmt.Sprintf("document-%d.txt", i), text)
				nodes = append(nodes, node)
				process(service, node, name)
			}
			search := func(service *processing.Service, profile string) []string {
				report, err := service.Search(t.Context(), processing.SearchRequest{Profile: profile, Query: "lease renewal", Mode: "semantic", Limit: 2, AllLive: &store.SearchOptions{}})
				require.NoError(t, err)
				var ids []string
				for _, result := range report.Results {
					ids = append(ids, result.Document.ContentVersionID)
				}
				return ids
			}
			before := search(service, name)
			require.Equal(t, nodes[0].CurrentVersionID, before[0])
			localDocuments.Store(0)
			require.NoError(t, os.WriteFile(filepath.Join(layout.Root, "local-text.json"), []byte(`{"version":1,"profile":"local-text","server":"local"}`), 0600))
			writeDaemonEmbeddingConfig(t, layout.Root, cfg)
			t.Setenv("DOCBANK_HOME", layout.Root)
			t.Setenv("DO_NOT_TRACK", "1")
			stop := startServe(t)
			defer stop()
			waitForDaemon(t, layout.Root)
			connection, err := daemonconn.Ensure(t.Context())
			require.NoError(t, err)
			plan, err := connection.API().PlanLocalText(t.Context(), &apiclient.PlanLocalTextRequestOptions{Body: &api.LocalTextPlanRequest{Server: "local"}})
			require.NoError(t, err)
			require.Equal(t, name, plan.Profile)
			require.Equal(t, "Keep existing index; replaces local-text for new imports; processes 2 documents before setup returns; embeds only new content", plan.VectorEffect)
			_, err = connection.API().ApplyLocalText(t.Context(), &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: "local", PlanFingerprint: plan.Fingerprint, Consent: true}})
			require.NoError(t, err)
			require.NoError(t, connection.Close())
			stop()
			choice, err := readLocalText(layout.Root)
			require.NoError(t, err)
			require.Equal(t, plan.Profile, choice.Profile)
			ollama.Close()
			restored, err := localTextCandidate(cfg, choice.Server, choice.Profile)
			require.NoError(t, err)
			require.Equal(t, before, search(serviceFor(restored), name))
			writeDaemonEmbeddingConfig(t, layout.Root, cfg)
			t.Setenv("DOCBANK_HOME", layout.Root)
			t.Setenv("DO_NOT_TRACK", "1")
			stop = startServe(t)
			defer stop()
			waitForDaemon(t, layout.Root)
			connection, err = daemonconn.Ensure(t.Context())
			require.NoError(t, err)
			outcome, err := tryAutomaticSearch(t.Context(), connection, "lease renewal", 2, daemonconn.SearchOptions{})
			require.NoError(t, err)
			require.NotNil(t, outcome.report, "automatic search must use the local provider after Ollama stops")
			profile := choice.Profile
			semantic, err := connection.SearchDocuments(t.Context(), api.DocumentSearchRequest{Profile: profile, Query: "lease renewal", Mode: "semantic", Limit: 2, Scope: &api.DocumentSearchScope{Kind: "all_live"}})
			require.NoError(t, err)
			var after []string
			for _, result := range semantic.Results {
				after = append(after, result.ContentVersionID)
			}
			require.Equal(t, before, after, profile)
			require.Zero(t, localDocuments.Load(), "the existing index retains its vectors")
			text := "new lease renewal document"
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
			_, err = connection.Upload(t.Context(), catalog.RootID(), "new.txt", "text/plain", hash, int64(len(text)), strings.NewReader(text))
			require.NoError(t, err)
			settleDaemon(t)
			require.Equal(t, int64(1), localDocuments.Load(), "the selected profile embeds the new document")
			require.NoError(t, connection.Close())
			stop()
		})
	}
}
