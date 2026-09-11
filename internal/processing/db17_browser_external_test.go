package processing_test

import (
	"context"
	"encoding/json/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

func TestDB17RealPDFBrowserProof(t *testing.T) {
	screenshotDir := os.Getenv("DOCBANK_VERIFIED_INSPECTOR_SCREENSHOT_DIR")
	if screenshotDir == "" {
		t.Skip("PR-only real PDF browser proof")
	}
	fixture := processing.NewDB17BrowserTestFixture(t)
	var p document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(fixture.Profile.CanonicalProfile, &p))
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-coverage-key"
	r, e, d := p.Rendition, p.EvidenceLexical, p.RetentionDisclosure
	cfg.RenditionProfiles = map[string]config.RenditionProfileConfig{"primary": {
		AdapterContract: r.AdapterContract, AuthorizationFingerprint: r.AuthorizationFingerprint, CredentialBinding: r.CredentialBinding,
		DeploymentFingerprint: r.DeploymentFingerprint, DescriptorID: r.Descriptor.ID, DescriptorFingerprint: r.Descriptor.Fingerprint,
		DisclosureFingerprint: r.DisclosureFingerprint, MaxDocumentBytes: r.MaxDocumentBytes, MaxResponseBytes: r.MaxResponseBytes, MaxUnits: r.MaxUnits,
		RequestedArtifacts: []string{string(document.EvidenceArtifactStructured)}, TrustBoundary: r.TrustBoundary, UploadOptionsFingerprint: r.UploadOptionsFingerprint,
	}}
	cfg.RetrievalProfiles = map[string]config.RetrievalProfileConfig{"local": {LexicalLimit: p.Retrieval.LexicalLimit, VectorLimit: p.Retrieval.VectorLimit}}
	cfg.ProcessingProfiles = map[string]config.ProcessingProfileConfig{"archive": {
		Rendition: "primary", Retrieval: "local", AttachmentPolicyFingerprint: d.AttachmentPolicyFingerprint, CompletenessFingerprint: e.CompletenessFingerprint,
		ConsentFingerprint: d.ConsentFingerprint, LexicalSegmenterFingerprint: e.LexicalSegmenterFingerprint, MaxDocumentChars: e.MaxDocumentChars,
		MaxSegmentRunes: e.MaxSegmentRunes, MaxUnitRunes: e.MaxUnitRunes, NormalizerFingerprint: e.NormalizerFingerprint, SanitizerFingerprint: e.SanitizerFingerprint,
		RetainSanitizedMarkdown: d.RetainSanitizedMarkdown, TrustBoundary: d.TrustBoundary,
	}}
	resolved, err := cfg.ProcessingProfile("archive")
	require.NoError(t, err)
	_, fingerprints, err := document.CanonicalProfile(resolved.Document)
	require.NoError(t, err)
	require.Equal(t, fixture.Profile.Fingerprint, fingerprints.Profile)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	webOrigin := "http://docbank-0123456789abcdef0123456789abcdef.localhost:" + port + "/"
	apiServer := api.NewServer(api.Deps{Store: fixture.Catalog, Blobs: fixture.Blobs, VaultRoot: fixture.VaultRoot,
		Cfg: cfg, WebURL: webOrigin})
	t.Cleanup(apiServer.Close)
	httpServer := &http.Server{Handler: apiServer.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Shutdown(context.Background()) })

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+listener.Addr().String()+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	request.Host = strings.TrimSuffix(strings.TrimPrefix(webOrigin, "http://"), "/")
	request.Header.Set("X-Api-Key", cfg.Server.APIKey)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var issued struct {
		Token        string `json:"token"`
		UploadSecret string `json:"upload_secret"`
		URL          string `json:"url"`
	}
	require.NoError(t, json.UnmarshalRead(response.Body, &issued))
	launch, err := url.Parse(issued.URL)
	require.NoError(t, err)
	launch.Fragment = url.Values{"web_session": {issued.Token},
		"web_upload_secret": {issued.UploadSecret}}.Encode()

	repository, err := filepath.Abs("../..")
	require.NoError(t, err)
	playwright := exec.CommandContext(t.Context(), "node",
		filepath.Join(repository, "frontend/node_modules/@playwright/test/cli.js"), "test",
		"--config", filepath.Join(repository, "frontend/screenshots/playwright.config.ts"),
		"--project", "chromium", "--grep", "DB17 real PDF")
	playwright.Dir = repository
	playwright.Env = append(os.Environ(), "DOCBANK_DB17_BROWSER_URL="+launch.String(),
		"DOCBANK_SCREENSHOT_DIR="+screenshotDir)
	output, err := playwright.CombinedOutput()
	require.NoError(t, err, string(output))
}
