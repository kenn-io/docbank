package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
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

// DB17BrowserTestFixture exposes the real PDF worker fixture to the external API test.
type DB17BrowserTestFixture struct {
	Catalog   *store.Store
	Blobs     *blob.Store
	VaultRoot string
	Profile   store.ProcessingProfileRecord
}

func NewDB17BrowserTestFixture(t *testing.T) DB17BrowserTestFixture {
	t.Helper()
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
		Runtime: registry, Gate: newTestOperationGate(), Owner: "db17-real-pdf",
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

	return DB17BrowserTestFixture{Catalog: catalog, Blobs: blobs, VaultRoot: vault, Profile: profile}
}
