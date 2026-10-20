package main

import (
	"image/color"
	"io"
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
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/document/vision/visiontest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
)

func visionProcessingConfig(t *testing.T, endpoint string) config.Config {
	t.Helper()
	descriptor, err := vision.Descriptor("synthetic-vision", document.RenditionTrustOperatorNetwork)
	require.NoError(t, err)
	cfg := config.Default()
	cfg.CredentialBindings["vision"] = config.CredentialBindingConfig{EnvironmentVariable: "DOCBANK_TEST_VISION_KEY"}
	cfg.RenditionProfiles["vision"] = config.RenditionProfileConfig{
		AdapterContract: vision.AdapterContract, Model: "synthetic-vision",
		AuthorizationFingerprint: strings.Repeat("1", 64), CredentialBinding: "credential:vision",
		DeploymentFingerprint: strings.Repeat("2", 64), DescriptorID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		DisclosureFingerprint: vision.DisclosureFingerprint(descriptor, endpoint, strings.Repeat("2", 64)),
		MaxDocumentBytes:      1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 100,
		RequestedArtifacts: []string{string(document.EvidenceArtifactStructured)},
		TrustBoundary:      string(document.RenditionTrustOperatorNetwork), UploadOptionsFingerprint: strings.Repeat("4", 64),
		Runtime: &config.RenditionRuntimeConfig{
			Endpoint: endpoint, RequestTimeout: config.Duration(10 * time.Second),
			AllowedCIDRs: []string{"127.0.0.0/8"}, ProxyMode: "disabled",
			ConnectTimeout: config.Duration(time.Second), KeepAlive: config.Duration(time.Second), TLSHandshakeTimeout: config.Duration(time.Second),
		},
	}
	cfg.RetrievalProfiles["lexical"] = config.RetrievalProfileConfig{LexicalLimit: 20, VectorLimit: 20}
	cfg.ProcessingProfiles["vision"] = config.ProcessingProfileConfig{
		Rendition: "vision", Retrieval: "lexical", AttachmentPolicyFingerprint: strings.Repeat("5", 64),
		CompletenessFingerprint: strings.Repeat("6", 64), ConsentFingerprint: strings.Repeat("7", 64),
		LexicalSegmenterFingerprint: strings.Repeat("8", 64), MaxDocumentChars: 100_000, MaxSegmentRunes: 2000, MaxUnitRunes: 100_000,
		NormalizerFingerprint: strings.Repeat("9", 64), SanitizerFingerprint: strings.Repeat("a", 64), TrustBoundary: "vault-primary",
	}
	require.NoError(t, cfg.Validate())
	return cfg
}

func TestDaemonDescribesAPhotoWithAConfiguredVisionProfile(t *testing.T) {
	var probes, describes atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer synthetic-vision-key", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		reply := `{"caption":"A white panorama.","tags":["panorama"],"visible_text":""}`
		if visiontest.IsProbe(body) {
			probes.Add(1)
			reply = "red,blue"
		} else {
			describes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(visiontest.Completion(reply))
	}))
	t.Cleanup(provider.Close)
	cfg := visionProcessingConfig(t, provider.URL)
	cfg.Server.APIKey = "synthetic-vision-daemon-key"
	t.Setenv("DOCBANK_TEST_VISION_KEY", "synthetic-vision-key")
	root := t.TempDir()
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	t.Setenv("DOCBANK_HOME", root)
	startServe(t)
	runtime := waitForDaemon(t, root)
	daemon := daemonconn.New("http://"+runtime.Address, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })

	path := filepath.Join(t.TempDir(), "harbor.jpg")
	require.NoError(t, os.WriteFile(path, mediatest.JPEG(1024, 768, color.White), 0o600))
	_, err := runCLI(t, "add", path, "--dest", "/photos")
	require.NoError(t, err)
	node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/photos/harbor.jpg"}})
	require.NoError(t, err)
	asset, err := daemon.API().GetPhotoAssetByNode(t.Context(), &apiclient.GetPhotoAssetByNodeRequestOptions{PathParams: &apiclient.GetPhotoAssetByNodePath{NodeID: node.ID}})
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "vision"}
	// The photo is describable once its grid preview exists.
	var plan *api.ProcessingPlan
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		var err error
		plan, err = daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
		require.NoError(c, err)
		require.Empty(c, plan.RenditionIneligibleReason)
	}, 20*time.Second, 50*time.Millisecond)
	require.True(t, plan.ConsentRequired)

	_, err = daemon.EnqueueProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		prepared, err := daemon.API().PreparePhotoAuthored(t.Context(), &apiclient.PreparePhotoAuthoredRequestOptions{Body: &api.PhotoAuthoredPrepareRequest{AssetIDs: []string{asset.ID}}})
		require.NoError(c, err)
		require.Len(c, prepared.Suggestions, 1)
	}, 20*time.Second, 50*time.Millisecond)
	require.EqualValues(t, 1, describes.Load(), "one description call")
	require.Positive(t, probes.Load(), "the provider was checked before the first description")
}
