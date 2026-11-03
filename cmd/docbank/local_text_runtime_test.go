package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
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

func TestLocalTextInstallWithoutPrompt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		ready        bool
		missingBytes int64
		installed    bool
		output       string
	}{
		{"ready", true, 0, false, ""},
		{"installed_offline", false, 0, true, ""},
		{"missing", false, 295000000, true, "Downloading local embeddings: 295000000 bytes\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			// A failing stdin proves the command never waits for an answer.
			cmd.SetIn(iotest.ErrReader(errors.New("stdin must not be read")))
			output := new(bytes.Buffer)
			cmd.SetOut(output)
			installed := false
			err := installLocalTextWithRuntime(cmd, true, func(_ context.Context, opts embedlocal.Options) (string, error) {
				require.True(t, opts.SkipOllama)
				if tc.ready {
					return "http://127.0.0.1:11434/v1", nil
				}
				return "", embedlocal.ErrNotFound
			}, func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				installed = true
				if tc.missingBytes > 0 {
					approved, err := opts.Confirm(ctx, tc.missingBytes)
					require.NoError(t, err)
					require.True(t, approved, "running the command approves the download")
				}
				return embedlocal.Installation{Executable: "synthetic-kennembed", DataDir: t.TempDir()}, nil
			})
			require.NoError(t, err)
			require.Equal(t, tc.installed, installed)
			require.Equal(t, tc.output, output.String())
		})
	}
}

func TestLocalTextSelectionFormat(t *testing.T) {
	t.Parallel()
	for _, data := range []string{`{"version":1`, `{"version":1,"profile":"local-text","server":"invalid"}`, `{"profile":"local-text","server":"local"}`, `{"version":2,"profile":"local-text","server":"local"}`, `{"version":1,"profile":"local-text","server":"local","typo":true}`} {
		t.Run(data, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "local-text.json"), []byte(data), 0600))
			_, err := readLocalText(root)
			require.Error(t, err)
		})
	}
	root := t.TempDir()
	require.NoError(t, saveLocalText(root, config.LocalTextConfig{Profile: config.LocalTextProfile, Server: "local"}))
	choice, err := readLocalText(root)
	require.NoError(t, err)
	require.Equal(t, config.LocalTextProfile, choice.Profile)
}

func TestLocalTextStaleSelectionLeavesDaemonAvailable(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("DOCBANK_HOME", root)
		t.Setenv("DO_NOT_TRACK", "1")
		data := []byte(`{"version":1`)
		require.NoError(t, os.WriteFile(filepath.Join(root, "local-text.json"), data, 0600))
		stop := startServe(t)
		defer stop()
		record := waitForDaemon(t, root)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+record.Address+"/api/v1/jobs", nil)
		require.NoError(t, err)
		req.Header.Set("X-Api-Key", record.Metadata["api_key"])
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		var jobs bytes.Buffer
		_, err = jobs.ReadFrom(response.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode, jobs.String())
		require.NotContains(t, jobs.String(), "embed:local-text")
		saved, err := os.ReadFile(filepath.Join(root, "local-text.json"))
		require.NoError(t, err)
		require.Equal(t, data, saved)
	})
}

func TestLocalTextBackfillHoldsDaemonUntilDrained(t *testing.T) {
	bin := buildDocbank(t)
	for _, result := range []struct {
		name       string
		lateImport bool
	}{{"completed", false}, {"new_import", true}} {
		t.Run(result.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("DO_NOT_TRACK", "1")
			t.Setenv(daemonconn.EnvBackgroundDaemon, "1")
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			started, release := make(chan struct{}), make(chan struct{})
			releaseRequest := sync.OnceFunc(func() { close(release) })
			var once sync.Once
			var calls atomic.Int64
			ping := kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{Service: embedlocal.Service, Version: embedlocal.Version})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/embeddings" {
					ping.ServeHTTP(w, r)
					return
				}
				calls.Add(1)
				once.Do(func() { close(started) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				var body struct {
					Input []string `json:"input"`
				}
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
					return
				}
				data := make([]map[string]any, len(body.Input))
				for i := range data {
					vector := make([]float32, 768)
					vector[0] = 1
					data[i] = map[string]any{"index": i, "object": "embedding", "embedding": vector}
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"object": "list", "model": "nomic-embed-text", "data": data}))
			}))
			defer server.Close()
			defer releaseRequest()
			dir, err := embedlocal.DefaultDir()
			require.NoError(t, err)
			endpoint, err := kitdaemon.ParseEndpoint(server.URL, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
			require.NoError(t, err)
			_, err = (kitdaemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(kitdaemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
			require.NoError(t, err)
			layout := home.Layout{Root: root}
			require.NoError(t, layout.Ensure())
			var profileFingerprint, version string
			func() {
				catalog, err := store.Open(t.Context(), layout.DBPath())
				require.NoError(t, err)
				defer func() { require.NoError(t, catalog.Close()) }()
				blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
				require.NoError(t, err)
				defer func() { require.NoError(t, blobs.Close()) }()
				cfg, err := localTextCandidate(config.Default(), "local")
				require.NoError(t, err)
				bundle, err := configureEmbeddingRuntimeBundle(cfg)
				require.NoError(t, err)
				profiles, err := executableProcessingProfiles(cfg, bundle)
				require.NoError(t, err)
				service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: catalog, Blobs: blobs, Gate: api.NewOperationGate(), Profiles: profiles, Principal: "daemon:operator", Scope: "document-processing", SpoolDirectory: layout.BlobTmpDir()})
				require.NoError(t, err)
				defer service.Stop()
				written, err := blobs.WriteDetailedContext(t.Context(), strings.NewReader("synthetic local text"))
				require.NoError(t, err)
				encoding, err := written.EncodingName()
				require.NoError(t, err)
				node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "document.txt", written.Hash, written.Size, "text/plain", store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize})
				require.NoError(t, err)
				selector := processing.Selector{Profile: config.LocalTextProfile, NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
				plan, err := service.Plan(t.Context(), selector)
				require.NoError(t, err)
				_, err = service.GrantConsent(t.Context(), processing.ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
				require.NoError(t, err)
				profileFingerprint, version = plan.ProfileFingerprint, node.CurrentVersionID
				if result.lateImport {
					_, _, err = catalog.Trash(t.Context(), node.ID, node.Revision)
					require.NoError(t, err)
					synctest.Test(t, func(t *testing.T) {
						setup := &processing.LocalTextSetup{Service: service, Selected: config.LocalTextProfile}
						tracker := api.NewActivityTracker()
						time.Sleep(time.Second)
						logger := slog.New(slog.DiscardHandler)
						require.True(t, localTextIdle(t.Context(), tracker, time.Second, setup.Pending, logger), "the initial scan has no pending work")
						imported, err := catalog.CreateFile(t.Context(), catalog.RootID(), "between-scans.txt", written.Hash, written.Size, "text/plain", store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize})
						require.NoError(t, err)
						require.False(t, localTextIdle(t.Context(), tracker, time.Second, setup.Pending, logger), "an import after the empty scan prevents idle shutdown")
						require.Zero(t, tracker.IdleFor(), "pending work resets the idle window")
						_, _, err = catalog.Trash(t.Context(), imported.ID, imported.Revision)
						require.NoError(t, err)
					})
				}
			}()
			require.NoError(t, saveLocalText(root, config.LocalTextConfig{Profile: config.LocalTextProfile, Server: "local"}))
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[server]\nidle_timeout = \"500ms\"\n"), 0600))
			stop := startServe(t)
			defer stop()
			if result.lateImport {
				waitForDaemon(t, root)
				source := filepath.Join(t.TempDir(), "new.txt")
				require.NoError(t, os.WriteFile(source, []byte("new synthetic local text"), 0600))
				command := exec.CommandContext(t.Context(), bin, "add", source)
				output, err := command.CombinedOutput()
				require.NoError(t, err, string(output))
				catalog, err := store.Open(t.Context(), layout.DBPath())
				require.NoError(t, err)
				imported, err := catalog.NodeByPath(t.Context(), "/inbox/new.txt")
				require.NoError(t, err)
				version = imported.CurrentVersionID
				require.NoError(t, catalog.Close())
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("embedding request never started")
			}
			time.Sleep(time.Second) //nolint:kennlint // sleeptest: the real daemon idle ticker and loopback listener use wall time
			_, _, found, err := daemonconn.Find(t.Context(), root)
			require.NoError(t, err)
			require.True(t, found, "the daemon remains discoverable during an embedding request")
			releaseRequest()
			catalog, err := store.Open(t.Context(), layout.DBPath())
			require.NoError(t, err)
			defer func() { require.NoError(t, catalog.Close()) }()
			require.Eventually(t, func() bool {
				spaces, err := catalog.ListVectorIndexSpaces(t.Context())
				if err != nil || len(spaces) != 1 {
					return false
				}
				source, err := catalog.CaptureVectorIndexSource(t.Context(), spaces[0])
				if err != nil || len(source.Members) == 0 {
					return false
				}
				generation, err := catalog.ActiveVectorIndexGeneration(t.Context(), spaces[0])
				return err == nil && generation.SourceManifestChecksum == source.ManifestChecksum && generation.RowCount > 0
			}, 5*time.Second, 10*time.Millisecond, "completed embeddings become search-visible before idle shutdown")
			records, err := daemonconn.RuntimeStore(root).List()
			require.NoError(t, err)
			require.Len(t, records, 1, "the daemon stays discoverable through vector publication")
			require.Eventually(t, func() bool {
				records, err := daemonconn.RuntimeStore(root).List()
				return err == nil && len(records) == 0
			}, 8*time.Second, 20*time.Millisecond)
			stop()
			jobs, err := catalog.EmbeddingJobsForVersionProfile(t.Context(), version, profileFingerprint)
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			require.Equal(t, "completed", jobs[0].State)
			require.Equal(t, int64(1), calls.Load())
		})
	}
}

func TestLocalTextAutoSendsOnlyToOwnerEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var attackerText, ownerText []string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var request struct {
			Input []string `json:"input"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &request))
		attackerText = append(attackerText, request.Input...)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"object": "list", "model": "nomic-embed-text", "data": []map[string]any{{"object": "embedding", "index": 0, "embedding": append([]float32{1}, make([]float32, 767)...)}, {"object": "embedding", "index": 1, "embedding": append([]float32{1}, make([]float32, 767)...)}}}))
	}))
	defer attacker.Close()
	ping := kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{Service: embedlocal.Service, Version: embedlocal.Version})
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			ping.ServeHTTP(w, r)
			return
		}
		var request struct {
			Input []string `json:"input"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &request))
		ownerText = append(ownerText, request.Input...)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"object": "list", "model": "nomic-embed-text", "data": []map[string]any{{"object": "embedding", "index": 0, "embedding": append([]float32{1}, make([]float32, 767)...)}, {"object": "embedding", "index": 1, "embedding": append([]float32{1}, make([]float32, 767)...)}}}))
	}))
	defer owner.Close()
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	endpoint, err := kitdaemon.ParseEndpoint(owner.URL, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
	require.NoError(t, err)
	_, err = (kitdaemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(kitdaemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
	require.NoError(t, err)
	attackerURL, err := url.Parse(attacker.URL)
	require.NoError(t, err)
	original := http.DefaultTransport
	base, ok := original.(*http.Transport)
	require.True(t, ok)
	transport := base.Clone()
	dial := (&net.Dialer{}).DialContext
	// Redirect only the discovery dial; every listener uses an ephemeral port.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "127.0.0.1:11434" {
			address = attackerURL.Host
		}
		return dial(ctx, network, address)
	}
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = original; transport.CloseIdleConnections() }()
	cfg, err := localTextCandidate(config.Default(), "auto")
	require.NoError(t, err)
	bundle, err := configureEmbeddingRuntimeBundle(cfg)
	require.NoError(t, err)
	provider := bundle.providers[config.LocalTextProfile]
	descriptor := provider.Descriptor()
	_, err = document.ExecuteEmbedding(t.Context(), provider, []document.EmbeddingInput{
		{Key: "document", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "private passage"},
		{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "private question"},
	}, document.EmbeddingAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 2, MaxInputBytes: 1024, MaxResponseBytes: 1 << 20})
	require.NoError(t, err)
	require.Empty(t, attackerText)
	require.Equal(t, []string{"search_document: private passage", "search_query: private question"}, ownerText)
}

func TestLocalTextEnablesWithoutInput(t *testing.T) {
	bin := buildDocbank(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DO_NOT_TRACK", "1")
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	ping := kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{Service: embedlocal.Service, Version: embedlocal.Version})
	server := httptest.NewServer(ping)
	defer server.Close()
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	endpoint, err := kitdaemon.ParseEndpoint(server.URL, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
	require.NoError(t, err)
	_, err = (kitdaemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(kitdaemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	defer func() { _, _ = daemonconn.Stop(context.Background(), root) }()
	command := exec.CommandContext(ctx, bin, "processing", "local", "--server", "local")
	// No stdin: the command must finish without asking a question.
	command.Stdin = nil
	data, err := command.CombinedOutput()
	require.NoError(t, err, string(data))
	require.Contains(t, string(data), "Text indexing enabled for this vault and new imports.")
	choice, err := readLocalText(root)
	require.NoError(t, err)
	require.Equal(t, config.LocalTextProfile, choice.Profile)
}
