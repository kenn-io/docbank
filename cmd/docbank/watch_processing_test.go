package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestDaemonRejectsWatchProfileWithoutRuntime(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	profile := cfg.RenditionProfiles["documents"]
	profile.Runtime = nil
	cfg.RenditionProfiles["documents"] = profile
	cfg.Watches = []config.WatchConfig{{Name: "inbox", Source: t.TempDir(), Destination: "/inbox", Profile: "documents", CaptureOnChange: true, SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second)}}
	require.NoError(t, cfg.Validate())
	root := t.TempDir()
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	t.Setenv("DOCBANK_HOME", root)
	require.ErrorContains(t, runServe(t.Context()), `watch "inbox" profile "documents" has no executable runtime`)
}

func TestDaemonWatchMediaProfilesAdmitImports(t *testing.T) {
	// Terminal provider responses preserve the durable admission evidence without
	// depending on a provider implementation or advancing a retry clock.
	var mu sync.Mutex
	received := make(map[string][]string)
	provider := func(profile string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, "invalid multipart source", http.StatusBadRequest)
				return
			}
			defer func() { assert.NoError(t, r.MultipartForm.RemoveAll()) }()
			files := r.MultipartForm.File["files"]
			if len(files) != 1 {
				http.Error(w, "expected one source", http.StatusBadRequest)
				return
			}
			mu.Lock()
			received[profile] = append(received[profile], files[0].Filename)
			mu.Unlock()
			http.Error(w, "synthetic terminal rejection", http.StatusBadRequest)
		}))
		t.Cleanup(server.Close)
		return server
	}
	docs := provider("documents")
	audio := provider("asr")
	cfg, _ := doclingDocumentProcessingConfig(t, docs.URL)
	asr, _ := doclingASRProcessingConfig(t, audio.URL)
	cfg.RenditionProfiles["asr"] = asr.RenditionProfiles["asr"]
	cfg.ProcessingProfiles["asr"] = asr.ProcessingProfiles["asr"]
	for _, name := range []string{"documents", "asr"} {
		profile := cfg.RenditionProfiles[name]
		profile.DiscloseFilename = true
		cfg.RenditionProfiles[name] = profile
	}
	inbox := t.TempDir()
	cfg.Server.APIKey = daemonASRAPIKey
	cfg.Watches = []config.WatchConfig{{Name: "mixed", Source: inbox, Destination: "/mixed", Profiles: map[string]string{"document": "documents", "audio_video": "asr"}, CaptureOnChange: true, SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Hour)}}
	require.NoError(t, cfg.Validate())
	root := t.TempDir()
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	encoded, err := os.ReadFile(filepath.Join(root, "config.toml"))
	require.NoError(t, err)
	require.Contains(t, string(encoded), "[watch.profiles]")
	t.Setenv("DOCBANK_TEST_DOCLING_KEY", daemonASRProviderKey)
	t.Setenv("DOCBANK_HOME", root)
	startServe(t)
	record := waitForDaemon(t, root)
	daemon := daemonconn.New("http://"+record.Address, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })
	sources := []struct {
		name, profile string
		content       []byte
	}{
		{"report.pdf", "documents", mediatest.PDF()},
		{"memo.wav", "asr", mediatest.WAV()},
		{"notes.txt", "", []byte("synthetic unmapped text")},
		{"clip.mp4", "", mediatest.MP4(16, 16, 1000)},
	}
	// Grant both profile consents using sources outside the watched inbox.
	for _, source := range sources[:2] {
		path := filepath.Join(t.TempDir(), source.name)
		require.NoError(t, os.WriteFile(path, source.content, 0600))
		_, err := runCLI(t, "add", path, "--dest", "/consent")
		require.NoError(t, err)
		node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/consent/" + source.name}})
		require.NoError(t, err)
		selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: source.profile}
		plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
		require.NoError(t, err)
		_, err = daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}})
		require.NoError(t, err)
	}
	for _, source := range sources {
		require.NoError(t, os.WriteFile(filepath.Join(inbox, source.name), source.content, 0600))
	}
	settleDaemon(t)
	for _, source := range sources {
		node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/mixed/" + source.name}})
		require.NoError(t, err)
		summaries, err := daemon.ResolveDocumentSummaries(t.Context(), api.DocumentSummaryResolveRequest{Identities: []api.DocumentIdentity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Path: "/mixed/" + source.name}}})
		require.NoError(t, err)
		require.Len(t, summaries, 1)
		if source.profile == "" {
			require.Empty(t, summaries[0].LatestProcessingState, source.name)
		} else {
			require.Equal(t, "failed", summaries[0].LatestProcessingState, source.name)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, map[string][]string{"documents": {"report.pdf"}, "asr": {"memo.wav"}}, received)
}

func TestDaemonRejectsWatchMappedProfilesWithoutRuntime(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	rendition := cfg.RenditionProfiles["documents"]
	rendition.Runtime = nil
	cfg.RenditionProfiles["documents"] = rendition
	cfg.ProcessingProfiles["other"] = cfg.ProcessingProfiles["documents"]
	cfg.Watches = []config.WatchConfig{{Name: "mixed", Source: t.TempDir(), Destination: "/mixed", Profiles: map[string]string{"document": "other", "audio_video": "documents"}, CaptureOnChange: true, SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Hour)}}
	require.NoError(t, cfg.Validate())
	root := t.TempDir()
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	t.Setenv("DOCBANK_HOME", root)
	require.ErrorContains(t, runServe(t.Context()), `watch "mixed" profile "documents" has no executable runtime`)
}
