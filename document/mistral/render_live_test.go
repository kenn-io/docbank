package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/renderpdf"
)

func TestLiveDOCXRenderedPDF(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("live DOCX rendering proof requires the Linux native renderer; manual-owner-proof")
	}
	apiKey := os.Getenv("MISTRAL_API_KEY")
	manifestPath := os.Getenv("MISTRAL_CAPABILITY_MANIFEST")
	executable := os.Getenv("DOCBANK_TEST_LIBREOFFICE_EXECUTABLE")
	wordFixture := os.Getenv("DOCBANK_TEST_WORD_DOCX")
	if apiKey == "" || manifestPath == "" || executable == "" || wordFixture == "" {
		t.Skip("MISTRAL_API_KEY, MISTRAL_CAPABILITY_MANIFEST, DOCBANK_TEST_LIBREOFFICE_EXECUTABLE, and DOCBANK_TEST_WORD_DOCX are required; manual-owner-proof")
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	manifest, err := DecodeCapabilityManifest(bytes.NewReader(manifestBytes))
	require.NoError(t, err)
	renderPolicy := liveRenderPDFPolicy(t, executable)
	policy := testPolicyWithRenderPDF(t, renderPolicy, 50<<20, manifest.MaxUnits)
	authorization, err := policy.Authorize(manifest, "docx")
	require.NoError(t, err)
	content, err := os.ReadFile(wordFixture)
	require.NoError(t, err)
	digest := sha256.Sum256(content)
	spool := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, spool)
	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, PrepareOptions{
		Directory: spool, DeclaredMediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, prepared.Release()) }()
	client, err := NewClient(policy, ClientConfig{APIKey: apiKey, Timeout: 2 * time.Minute, MaxRetries: 1})
	require.NoError(t, err)
	result, err := client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	require.NotNil(t, result.ConversionReceipt)
	if result.UnitsProcessed != result.ConversionReceipt.Pages {
		t.Fatalf("provider_pages=%d generated_pages=%d", result.UnitsProcessed, result.ConversionReceipt.Pages)
	}
	t.Logf("source_sha256=%s upload_sha256=%s pages=%d upload_bytes=%d provider_pages=%d provider_bytes=%v",
		result.ConversionReceipt.SourceSHA256, result.ConversionReceipt.PDFSHA256,
		result.ConversionReceipt.Pages, result.ConversionReceipt.PDFBytes,
		result.UnitsProcessed, result.ProviderBytes)
}

func liveRenderPDFPolicy(t *testing.T, executable string) renderpdf.Policy {
	t.Helper()
	runtimeManifest, err := renderpdf.DiscoverRuntime(renderpdf.DefaultRuntimeRoots())
	require.NoError(t, err)
	executableBytes, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(executableBytes)
	policy, err := renderpdf.NewPolicy(renderpdf.Renderer{
		Executable: executable, ExecutableSHA256: hex.EncodeToString(digest[:]),
		Runtime: runtimeManifest.Files, RuntimeSymlinks: runtimeManifest.Symlinks,
		RuntimeIdentity: runtimeManifest.Identity,
	}, renderpdf.DefaultLimits())
	require.NoError(t, err)
	return policy
}
