//go:build linux

package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/renderpdf"
)

func TestDOCXLibreOfficeRoute(t *testing.T) {
	executable := os.Getenv("DOCBANK_TEST_LIBREOFFICE_EXECUTABLE")
	if executable == "" {
		t.Skip("DOCBANK_TEST_LIBREOFFICE_EXECUTABLE is unset; manual-owner-proof")
	}
	wordFixture := os.Getenv("DOCBANK_TEST_WORD_DOCX")
	if wordFixture == "" {
		if os.Getenv("DOCBANK_TEST_REQUIRE_WORD_PROOF") == "1" {
			t.Fatal("DOCBANK_TEST_WORD_DOCX is required for the Microsoft Word producer-fidelity owner proof")
		}
		t.Log("DOCBANK_TEST_WORD_DOCX is unset; running the explicit-break proof only")
	}
	if _, err := os.Stat(wordFixture); err != nil {
		if wordFixture != "" {
			t.Logf("Microsoft Word-produced fixture is unavailable: %v; running the explicit-break proof only", err)
			wordFixture = ""
		}
	}
	renderPolicy := realDOCXRenderPolicy(t, executable)
	policy := testPolicyWithRenderPDF(t, renderPolicy, 50<<20, 11)
	testCases := []struct {
		name string
		path string
	}{
		{name: "explicit-breaks", path: filepath.Join("testdata", "docx", "explicit-breaks.docx")},
	}
	if wordFixture != "" {
		testCases = append([]struct {
			name string
			path string
		}{{name: "word-produced", path: wordFixture}}, testCases...)
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			content, err := os.ReadFile(testCase.path)
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
			manifest := syntheticManifest(t, policy, true)
			authorization, err := policy.Authorize(manifest, "docx")
			require.NoError(t, err)
			var uploaded []byte
			var handlerErr error
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				body, readErr := io.ReadAll(request.Body)
				if readErr != nil {
					handlerErr = readErr
					http.Error(w, "synthetic test failure", http.StatusInternalServerError)
					return
				}
				uploaded = decodeRequestDocument(t, body, mediaTypePDF)
				pages, countErr := media.CountPDFPages(uploaded)
				if countErr != nil {
					handlerErr = countErr
					http.Error(w, "synthetic test failure", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, ocrResponse(int(pages), len(uploaded)))
			}))
			defer server.Close()
			client := newServerClient(t, server, policy, ClientConfig{MaxRetries: 0})
			result, err := client.Process(t.Context(), prepared, authorization)
			require.NoError(t, err)
			require.NoError(t, handlerErr)
			require.NotNil(t, result.ConversionReceipt)
			pages, err := media.CountPDFPages(uploaded)
			require.NoError(t, err)
			assert.Equal(t, int(pages), result.ConversionReceipt.Pages)
			assert.Equal(t, result.ConversionReceipt.PDFSHA256, digestBytes(uploaded))
			assert.Equal(t, result.ConversionReceipt.PDFBytes, int64(len(uploaded)))
			assert.Equal(t, "word", result.Document.Family)
			t.Logf("fixture=%s source_sha256=%s upload_sha256=%s pages=%d upload_bytes=%d runner=%s render_policy=%s",
				testCase.name, result.ConversionReceipt.SourceSHA256, result.ConversionReceipt.PDFSHA256,
				result.ConversionReceipt.Pages, result.ConversionReceipt.PDFBytes, renderPolicy.RunnerIdentity(), renderPolicy.Fingerprint())
		})
	}
}

func realDOCXRenderPolicy(t *testing.T, executable string) renderpdf.Policy {
	t.Helper()
	manifest, err := renderpdf.DiscoverRuntime(renderpdf.DefaultRuntimeRoots())
	require.NoError(t, err)
	executableBytes, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(executableBytes)
	renderPolicy, err := renderpdf.NewPolicy(renderpdf.Renderer{
		Executable: executable, ExecutableSHA256: hex.EncodeToString(digest[:]),
		Runtime: manifest.Files, RuntimeSymlinks: manifest.Symlinks, RuntimeIdentity: manifest.Identity,
	}, renderpdf.DefaultLimits())
	require.NoError(t, err)
	return renderPolicy
}

func decodeRequestDocument(t *testing.T, body []byte, mediaType string) []byte {
	t.Helper()
	var wire struct {
		Document struct {
			URL string `json:"document_url"`
		} `json:"document"`
	}
	require.NoError(t, json.Unmarshal(body, &wire))
	uploaded, err := decodeBase64(strings.TrimPrefix(wire.Document.URL, "data:"+mediaType+";base64,"))
	require.NoError(t, err)
	return uploaded
}
