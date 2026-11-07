package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
)

func TestLocalTextSetupPreservesAudioCeiling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCBANK_TEST_DOCLING_KEY", daemonASRProviderKey)
	provider := newDaemonDoclingServer(t)
	local := httptest.NewServer(withTextModelInventory(t, kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{Service: embedlocal.Service, Version: embedlocal.Version})))
	t.Cleanup(local.Close)
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	endpoint, err := kitdaemon.ParseEndpoint(local.URL, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
	require.NoError(t, err)
	_, err = (kitdaemon.RuntimeStore{Dir: dir, Prefix: embedlocal.Service}).Write(kitdaemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, endpoint))
	require.NoError(t, err)
	cfg, _ := doclingASRProcessingConfig(t, provider.URL())
	embeddings := localTextSourceConfig(t, local.URL, "source-revision", "prefix-free", "DOCBANK_TEST_AUDIO_EMBED_KEY")
	cfg.EmbeddingProfiles = embeddings.EmbeddingProfiles
	maps.Copy(cfg.CredentialBindings, embeddings.CredentialBindings)
	recipe := cfg.ProcessingProfiles["asr"]
	recipe.Embeddings = []string{"semantic"}
	cfg.ProcessingProfiles["asr"] = recipe
	cfg.Media.MaxSourceBytes = 64
	require.NoError(t, cfg.Validate())
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, layout.Ensure())
	writeDaemonEmbeddingConfig(t, layout.Root, cfg)
	file, err := os.OpenFile(filepath.Join(layout.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = file.WriteString("\n[media]\nmax_source_bytes = 64\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	// A saved selection survives consent revocation; Apply must grant approval
	// again before it can process this recipe.
	require.NoError(t, os.WriteFile(filepath.Join(layout.Root, "local-text.json"), []byte(`{"version":1,"profile":"asr","server":"local"}`), 0600))
	t.Setenv("DOCBANK_HOME", layout.Root)
	t.Setenv("DO_NOT_TRACK", "1")
	stop := startServe(t)
	defer stop()
	waitForDaemon(t, layout.Root)
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, connection.Close()) }()
	content := mediatest.WAV()
	_, err = connection.Upload(t.Context(), 1, "recording.wav", "audio/wav", fmt.Sprintf("%x", sha256.Sum256(content)), int64(len(content)), bytes.NewReader(content))
	require.NoError(t, err)
	node, err := connection.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/recording.wav"}})
	require.NoError(t, err)
	ordinary, err := connection.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: api.ProcessingSelector{Profile: "asr", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}}})
	require.NoError(t, err)
	require.Equal(t, "source_bytes_exceeded", ordinary.RenditionIneligibleReason)
	plan, err := connection.API().PlanLocalText(t.Context(), &apiclient.PlanLocalTextRequestOptions{Body: &api.LocalTextPlanRequest{Server: "local"}})
	require.NoError(t, err)
	require.Equal(t, "asr", plan.Profile)
	_, applyErr := connection.API().ApplyLocalText(t.Context(), &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: "local", PlanFingerprint: plan.Fingerprint, Consent: true}})
	require.Zero(t, provider.submissions.Load(), "setup must preserve the ordinary processing source-size ceiling")
	require.NoError(t, applyErr)
}
