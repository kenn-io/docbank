package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

type pdftotextBrowserRuntime struct {
	base       *workerProvider
	source     []byte
	executable string
}

func (runtime pdftotextBrowserRuntime) Prepare(
	ctx context.Context, work store.RenditionJobWork, now time.Time,
) (RenditionExecution, error) {
	execution, err := (workerRuntime{provider: runtime.base}).Prepare(ctx, work, now)
	if err != nil {
		return RenditionExecution{}, err
	}
	execution.Upload = &workerUpload{Reader: bytes.NewReader(runtime.source), metadata: work.ExecutionIdentity.Upload}
	execution.Provider = pdftotextBrowserProvider{base: runtime.base, executable: runtime.executable}
	return execution, nil
}

type pdftotextBrowserProvider struct {
	base       *workerProvider
	executable string
}

func (provider pdftotextBrowserProvider) Descriptor() document.RenditionDescriptor {
	return provider.base.Descriptor()
}

func (provider pdftotextBrowserProvider) Render(
	ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	source, err := io.ReadAll(io.LimitReader(upload, 1<<20))
	if err != nil {
		return document.RenditionResult{}, err
	}
	command := exec.CommandContext(ctx, provider.executable, "-layout", "-", "-")
	command.Stdin = bytes.NewReader(source)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	extracted, err := command.Output()
	if err != nil {
		return document.RenditionResult{}, fmt.Errorf("pdftotext fixture failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	text := strings.TrimSpace(string(extracted))
	if text == "" {
		return document.RenditionResult{}, errors.New("pdftotext fixture returned no text")
	}
	result, err := provider.base.Render(ctx, upload, authorization)
	if err != nil {
		return document.RenditionResult{}, err
	}
	result.Evidence.Completeness = document.EvidenceComplete
	result.Evidence.UnitKind = document.EvidenceUnitPage
	result.Evidence.Omissions = nil
	result.Evidence.Units[0].Text = text
	result.Evidence.Units[0].Locator = document.SourceEvidenceLocatorV1{
		Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
		Start: 1, End: 1,
	}
	if err := document.ValidateSourceEvidenceV1(result.Evidence); err != nil {
		return document.RenditionResult{}, err
	}
	return result, nil
}

func TestDB17RealPDFBrowserProof(t *testing.T) {
	screenshotDir := os.Getenv("DOCBANK_VERIFIED_INSPECTOR_SCREENSHOT_DIR")
	if screenshotDir == "" {
		t.Skip("PR-only real PDF browser proof")
	}
	pdftotext, err := exec.LookPath("pdftotext")
	require.NoError(t, err)

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetCompression(false)
	pdf.AddPage()
	pdf.SetFont("Arial", "", 14)
	pdf.Cell(170, 10, "Extracted alpha evidence from a real synthetic PDF.")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	source := encoded.Bytes()
	command := exec.CommandContext(t.Context(), pdftotext, "-layout", "-", "-")
	command.Stdin = bytes.NewReader(source)
	preview, err := command.Output()
	require.NoError(t, err)
	require.Contains(t, string(preview), "Extracted alpha evidence from a real synthetic PDF.")

	vault := t.TempDir()
	catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalog.Close() })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	receipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
	require.NoError(t, err)
	physical := processingBlobPhysical(t, receipt)
	nodes := make([]store.Node, 0, 51)
	for index := 1; index <= 51; index++ {
		node, createErr := catalog.CreateFile(t.Context(), catalog.RootID(),
			fmt.Sprintf("%02d-evidence.pdf", index), receipt.Hash, receipt.Size,
			"application/pdf", physical)
		require.NoError(t, createErr)
		nodes = append(nodes, node)
	}
	base := newWorkerProvider(t)
	profile := workerProcessingProfile(t, base.Descriptor())
	firstRequest := workerJobRequest(nodes[0].CurrentVersionID, profile, base.Descriptor())
	firstRequest.ExecutionIdentity.Upload.SHA256 = receipt.Hash
	firstRequest.ExecutionIdentity.Upload.ByteLength = receipt.Size
	firstRequest.ExecutionIdentity.Authorization.SourceSHA256 = receipt.Hash
	firstRequest.ExecutionIdentity.Authorization.SourceBytes = receipt.Size
	grantWorkerConsent(t, catalog, firstRequest)
	for _, node := range nodes {
		request := workerJobRequest(node.CurrentVersionID, profile, base.Descriptor())
		request.ExecutionIdentity.Upload.SHA256 = receipt.Hash
		request.ExecutionIdentity.Upload.ByteLength = receipt.Size
		request.ExecutionIdentity.Authorization.SourceSHA256 = receipt.Hash
		request.ExecutionIdentity.Authorization.SourceBytes = receipt.Size
		_, _, err = catalog.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
	}
	registry := NewRenditionRuntimeRegistry()
	require.NoError(t, registry.Register(base.Descriptor().Fingerprint,
		pdftotextBrowserRuntime{base: base, source: source, executable: pdftotext}))
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: catalog, Blobs: blobs,
		Runtime: registry, Gate: api.NewOperationGate(), Owner: "db17-real-pdf",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Clock: time.Now})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	for _, node := range nodes {
		_, err = catalog.ActiveRendition(t.Context(), node.CurrentVersionID, profile.Fingerprint)
		require.NoError(t, err)
	}

	cfg := coverageProcessingConfig(t, profile)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	webOrigin := "http://docbank-0123456789abcdef0123456789abcdef.localhost:" + port + "/"
	apiServer := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: vault,
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
