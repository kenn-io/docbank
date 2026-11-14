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
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/atomicfile"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
)

func localTextDiscoveryHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/ping", kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{Service: embedlocal.Service, Version: embedlocal.Version}))
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":["nomic-embed-text"]}`))
	})
	return mux
}

func TestLocalTextInstallWithoutPrompt(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
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
				if tc.ready || installed {
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

func TestLocalEmbeddingOriginWithRuntime(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	for _, tc := range []struct{ name, failure string }{
		{"installed", ""},
		{"discovery_failure", "discover local embeddings"},
		{"install_failure", "reuse local embeddings installation"},
		{"startup_failure", "start local embeddings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installed := false
			dataDir := t.TempDir()
			origin, err := localEmbeddingOriginWithRuntime(t.Context(), nil, func(_ context.Context, opts embedlocal.Options) (string, error) {
				require.True(t, opts.SkipOllama)
				if tc.name == "discovery_failure" || (installed && tc.name == "startup_failure") {
					return "", errors.New("synthetic runtime failure")
				}
				if !installed {
					return "", embedlocal.ErrNotFound
				}
				require.Equal(t, "synthetic-kennembed", opts.Executable)
				require.Equal(t, dataDir, opts.DataDir)
				return "http://127.0.0.1:12345/v1", nil
			}, func(_ context.Context, _ embedlocal.InstallOptions) (embedlocal.Installation, error) {
				installed = true
				if tc.name == "install_failure" {
					return embedlocal.Installation{}, errors.New("synthetic install failure")
				}
				return embedlocal.Installation{Executable: "synthetic-kennembed", DataDir: dataDir}, nil
			})
			if tc.failure != "" {
				require.ErrorContains(t, err, tc.failure)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "http://127.0.0.1:12345", origin)
		})
	}
}

func TestLocalTextSaveRestartRequired(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failure       error
		published     bool
		markerFailure bool
	}{
		{"success", nil, true, false},
		{"failed", errors.New("synthetic unpublished write failure"), false, false},
		{"published", errors.Join(atomicfile.ErrPublished, errors.New("synthetic directory sync failure")), true, false},
		{"marker failure", nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", os.Getenv("HOME"))
			root := t.TempDir()
			var logs bytes.Buffer
			if tc.markerFailure {
				require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), ".kenn"), []byte("blocked"), 0600))
			}
			var pending atomic.Bool
			save := localTextSave(root, &pending, func(root string, choice config.LocalTextConfig) error {
				if tc.published {
					require.NoError(t, saveLocalText(root, choice))
				}
				return tc.failure
			}, slog.New(slog.NewTextHandler(&logs, nil)))
			err := save(config.LocalTextProfile, "local")
			if tc.failure == nil {
				require.NoError(t, err)
			} else {
				require.Same(t, tc.failure, err)
			}
			if tc.markerFailure {
				require.Contains(t, logs.String(), "record shared search by meaning choice")
			} else {
				recorded, err := embedlocal.ChoiceRecorded()
				require.NoError(t, err)
				require.Equal(t, tc.failure == nil, recorded)
			}
			require.Equal(t, tc.published, pending.Load())
			choice, err := readLocalText(root)
			require.NoError(t, err)
			if tc.published {
				require.Equal(t, config.LocalTextConfig{Version: 1, Profile: config.LocalTextProfile, Server: "local"}, choice)
			} else {
				require.Empty(t, choice.Profile)
			}
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
	require.NoError(t, saveLocalText(root, config.LocalTextConfig{Profile: "archive", Server: "local"}))
	choice, err := readLocalText(root)
	require.NoError(t, err)
	require.Equal(t, "archive", choice.Profile)
}

func TestLocalTextStaleSelectionLeavesDaemonAvailable(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"malformed", `{"version":1`},
		{"missing_profile", `{"version":1,"profile":"missing-profile","server":"auto"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			homeDir := t.TempDir()
			t.Setenv("HOME", homeDir)
			t.Setenv("USERPROFILE", homeDir)
			t.Setenv("DO_NOT_TRACK", "1")
			data := []byte(tc.data)
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
			connection, err := daemonconn.Ensure(t.Context())
			require.NoError(t, err)
			defer func() { require.NoError(t, connection.Close()) }()
			status, err := connection.API().GetLocalTextStatus(t.Context())
			require.NoError(t, err)
			require.False(t, status.Active)
			require.False(t, status.RestartRequired)
		})
	}
}

func TestLocalTextBackfillHoldsDaemonUntilDrained(t *testing.T) {
	bin := buildDocbank(t)
	for _, result := range []struct {
		name       string
		lateImport bool
	}{{"completed", false}, {"new_import", true}} {
		t.Run(result.name, func(t *testing.T) {
			homeDir := t.TempDir()
			t.Setenv("HOME", homeDir)
			t.Setenv("USERPROFILE", homeDir)
			t.Setenv("DO_NOT_TRACK", "1")
			t.Setenv(daemonconn.EnvBackgroundDaemon, "1")
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			started, release := make(chan struct{}), make(chan struct{})
			releaseRequest := sync.OnceFunc(func() { close(release) })
			var once sync.Once
			var calls atomic.Int64
			ping := localTextDiscoveryHandler()
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
				cfg, err := localTextCandidate(config.Default(), "local", "")
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
			stop, daemon := startServeReady(t)
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
			// The idle watcher stops the daemon only once the tracker is quiet,
			// and the backfill holds it for the whole request.
			require.Zero(t, daemon.tracker.IdleFor(), "an embedding request holds the daemon open")
			_, _, found, err := daemonconn.Find(t.Context(), root)
			require.NoError(t, err)
			require.True(t, found, "the daemon remains discoverable during an embedding request")
			releaseRequest()
			settleDaemon(t)
			catalog, err := store.Open(t.Context(), layout.DBPath())
			require.NoError(t, err)
			defer func() { require.NoError(t, catalog.Close()) }()
			spaces, err := catalog.ListVectorIndexSpaces(t.Context())
			require.NoError(t, err)
			require.Len(t, spaces, 1)
			source, err := catalog.CaptureVectorIndexSource(t.Context(), spaces[0])
			require.NoError(t, err)
			require.NotEmpty(t, source.Members)
			generation, err := catalog.ActiveVectorIndexGeneration(t.Context(), spaces[0])
			require.NoError(t, err)
			require.Equal(t, source.ManifestChecksum, generation.SourceManifestChecksum, "completed embeddings become search-visible")
			require.Positive(t, generation.RowCount)
			stop()
			waitForDaemonStop(t, root)
			jobs, err := catalog.EmbeddingJobsForVersionProfile(t.Context(), version, profileFingerprint)
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			require.Equal(t, "completed", jobs[0].State)
			require.Equal(t, int64(1), calls.Load())
		})
	}
}

func TestLocalTextAutoSendsOnlyToOwnerEndpoint(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
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
	ping := localTextDiscoveryHandler()
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
	cfg, err := localTextCandidate(config.Default(), "auto", "")
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

func TestLocalTextEnablesNoninteractivelyAndRestartsExplicitDaemon(t *testing.T) {
	bin := buildDocbank(t)
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("DO_NOT_TRACK", "1")
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	ping := localTextDiscoveryHandler()
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
	data, err := exec.CommandContext(ctx, bin, "daemon", "start").CombinedOutput()
	require.NoError(t, err, string(data))
	before := waitForDaemon(t, root)
	command := exec.CommandContext(ctx, bin, "processing", "local", "--server", "local")
	// No stdin: the command must finish without asking a question.
	command.Stdin = nil
	data, err = command.CombinedOutput()
	require.NoError(t, err, string(data))
	require.Contains(t, string(data), "Text indexing enabled for this vault and new imports.")
	choice, err := readLocalText(root)
	require.NoError(t, err)
	require.Equal(t, config.LocalTextProfile, choice.Profile)
	after := waitForDaemon(t, root)
	require.NotEqual(t, before.PID, after.PID)
	require.True(t, daemonconn.Explicit(after))
}

func TestLocalTextCandidateRejections(t *testing.T) {
	cfg := localTextSourceConfig(t, "http://127.0.0.1:12345", "source-revision", "prefix-free", "DOCBANK_TEST_RESERVED_KEY")
	cfg.ProcessingProfiles = map[string]config.ProcessingProfileConfig{config.LocalTextProfile: cfg.ProcessingProfiles["archive"]}
	_, err := localTextCandidate(cfg, "local", config.LocalTextProfile)
	require.ErrorContains(t, err, "reserved for local text")
	cfg = localTextSourceConfig(t, "http://127.0.0.1:12345", "source-revision", "revision-header", "DOCBANK_TEST_REVISION_KEY")
	_, err = localTextCandidate(cfg, "local", "archive")
	require.ErrorIs(t, err, processing.ErrProfileNotConfigured)
}

func TestLocalTextStatusProvider(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	for _, tc := range []struct {
		name, endpoint, provider string
		pending                  bool
		moveFirst                bool
		inherited                bool
		startupFailed            bool
	}{
		{"off", "", "", false, false, false, false},
		{"restart_pending", "", "", true, false, false, false},
		{"ollama", "http://127.0.0.1:11434", "ollama", false, false, false, false},
		{"configured", "http://127.0.0.1:12345", "configured", false, false, false, false},
		{"shared_binding_after_move", "http://127.0.0.1:11434", "local", false, true, false, false},
		{"inherited_revoked", "", "", false, false, true, false},
		{"inherited_startup_failed", "", "", false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := home.Layout{Root: t.TempDir()}
			require.NoError(t, layout.Ensure())
			catalog, err := store.Open(t.Context(), layout.DBPath())
			require.NoError(t, err)
			defer func() { require.NoError(t, catalog.Close()) }()
			blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
			require.NoError(t, err)
			defer func() { require.NoError(t, blobs.Close()) }()
			cfg := config.Default()
			if tc.inherited {
				cfg, err = cfg.WithLocalText()
				require.NoError(t, err)
			}
			if tc.endpoint != "" {
				cfg = localTextSourceConfig(t, tc.endpoint, "synthetic-revision", "prefix-free", "DOCBANK_TEST_STATUS_KEY")
			}
			if tc.moveFirst {
				cfg.ProcessingProfiles["later"] = cfg.ProcessingProfiles["archive"]
			}
			bundle, err := configureEmbeddingRuntimeBundle(cfg)
			require.NoError(t, err)
			profiles, err := executableProcessingProfiles(cfg, bundle)
			require.NoError(t, err)
			if tc.moveFirst {
				candidate, err := localTextCandidate(cfg, "local", "archive")
				require.NoError(t, err)
				selectedBundle, err := configureEmbeddingRuntimeBundle(candidate)
				require.NoError(t, err)
				selectedProfiles, err := executableProcessingProfiles(candidate, selectedBundle)
				require.NoError(t, err)
				_, err = projectLocalTextProfile(cfg, profiles, candidate, "archive", selectedProfiles["archive"])
				require.NoError(t, err)
			}
			service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: catalog, Blobs: blobs, Profiles: profiles, Gate: api.NewOperationGate(), Principal: "daemon:operator", Scope: "document-processing", SpoolDirectory: layout.BlobTmpDir()})
			require.NoError(t, err)
			defer service.Stop()
			inheritedReady := false
			if tc.inherited {
				setup := &processing.LocalTextSetup{Service: service, Selected: config.LocalTextProfile, Startup: func(context.Context, bool, string) error { return nil }}
				active, err := setup.InitializeInherited(t.Context())
				require.NoError(t, err)
				require.True(t, active)
				inheritedReady = active
				if tc.startupFailed {
					setup.Startup = func(context.Context, bool, string) error { return errors.New("startup failed") }
					active, err = setup.InitializeInherited(t.Context())
					require.Error(t, err)
					require.False(t, active)
					inheritedReady = active
				} else {
					_, err = service.RevokeConsent(t.Context())
					require.NoError(t, err)
				}
			}
			var restartPending atomic.Bool
			restartPending.Store(tc.pending)

			status, err := localTextStatus(t.Context(), profiles, service, restartPending.Load(), tc.inherited, inheritedReady)
			require.NoError(t, err)
			require.Equal(t, tc.provider, status.Provider)
			require.Equal(t, tc.pending, status.RestartRequired)
			require.Equal(t, tc.provider != "", status.Active)
			if tc.provider == "" {
				require.Positive(t, status.DownloadBytes)
			}
		})
	}
}

func TestLocalTextStartupActivatesAfterRestart(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "explicit"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			homeDir := t.TempDir()
			t.Setenv("HOME", homeDir)
			t.Setenv("USERPROFILE", homeDir)
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			t.Setenv("DO_NOT_TRACK", "1")
			if shared {
				require.NoError(t, embedlocal.RecordChoice())
			}
			server := httptest.NewServer(localTextDiscoveryHandler())
			defer server.Close()
			dir, err := embedlocal.DefaultDir()
			require.NoError(t, err)
			endpoint, err := kitdaemon.ParseEndpoint(server.URL, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
			require.NoError(t, err)
			_, err = (kitdaemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(kitdaemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
			require.NoError(t, err)
			stop, _ := startServeReady(t)
			defer stop()
			if shared {
				settleDaemon(t)
			}
			connection, err := daemonconn.Ensure(t.Context())
			require.NoError(t, err)
			defer func() { require.NoError(t, connection.Close()) }()
			if !shared {
				plan, err := connection.API().PlanLocalText(t.Context(), &apiclient.PlanLocalTextRequestOptions{Body: &api.LocalTextPlanRequest{Server: "auto"}})
				require.NoError(t, err)
				_, err = connection.API().ApplyLocalText(t.Context(), &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: "auto", PlanFingerprint: plan.Fingerprint, Consent: true}})
				require.NoError(t, err)
				require.NoError(t, connection.Close())
				stop()
				waitForDaemonStop(t, root)
				stop, _ = startServeReady(t)
				defer stop()
				connection, err = daemonconn.Ensure(t.Context())
				require.NoError(t, err)
			}
			status, err := connection.API().GetLocalTextStatus(t.Context())
			require.NoError(t, err)
			require.True(t, status.Active)
			require.Equal(t, "local", status.Provider)
			if shared {
				require.NoFileExists(t, filepath.Join(root, "local-text.json"))
			}
		})
	}
}

func TestInheritedLocalText(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	choice, err := inheritedLocalText(nil)
	require.NoError(t, err)
	require.Empty(t, choice.Profile)
	cfg := localTextSourceConfig(t, "http://127.0.0.1:12345", "synthetic-revision", "prefix-free", "DOCBANK_TEST_STATUS_KEY")
	bundle, err := configureEmbeddingRuntimeBundle(cfg)
	require.NoError(t, err)
	profiles, err := executableProcessingProfiles(cfg, bundle)
	require.NoError(t, err)
	require.NoError(t, embedlocal.RecordChoice())
	choice, err = inheritedLocalText(profiles)
	require.NoError(t, err)
	require.Empty(t, choice.Profile)
	choice, err = inheritedLocalText(nil)
	require.NoError(t, err)
	require.Equal(t, config.LocalTextProfile, choice.Profile)
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "search-by-meaning")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "search-by-meaning"), 0700))
	_, err = inheritedLocalText(nil)
	require.Error(t, err)
}
