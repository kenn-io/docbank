//go:build linux

package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/renderpdf"
)

type recordingNativeRunner struct {
	inner sandbox.Runner
	calls []renderpdf.Request
}

func (runner *recordingNativeRunner) Identity() string { return runner.inner.Identity() }

func (runner *recordingNativeRunner) Run(ctx context.Context, request renderpdf.Request) (renderpdf.StageResult, error) {
	runner.calls = append(runner.calls, renderpdf.Request{
		Stage: request.Stage, Arguments: append([]string(nil), request.Arguments...),
		InputName: request.InputName, OutputName: request.OutputName,
		Input: bytes.Clone(request.Input), InputSHA256: request.InputSHA256,
		WarmupInput: bytes.Clone(request.WarmupInput), WarmupInputSHA256: request.WarmupInputSHA256,
	})
	result, err := runner.inner.Run(ctx, sandbox.Request{
		PolicyFingerprint: request.PolicyFingerprint,
		Policy: sandbox.Policy{
			Mode: sandbox.LibreOfficeMode, Executable: request.Executable,
			ExecutableSHA256: request.ExecutableSHA256, Arguments: request.Arguments,
			Environment: request.Environment, Directory: request.Directory,
			MaxStdinBytes: int64(len(request.Input)), MaxStdoutBytes: request.MaxOutputBytes,
			PrivateRoot: &sandbox.PrivateRoot{
				Runtime: request.Runtime, Symlinks: request.RuntimeSymlinks,
				RuntimeIdentity: request.RuntimeIdentity, WorkBytes: request.MaxWorkBytes,
				InputName: request.InputName, OutputName: request.OutputName,
				MaxOutputBytes: request.MaxOutputBytes, WarmupInput: request.WarmupInput,
				WarmupInputSHA256: request.WarmupInputSHA256,
			},
		},
		Stdin: request.Input, StdinSHA256: request.InputSHA256,
	})
	digest := sha256.Sum256(result.Stdout)
	return renderpdf.StageResult{
		Output: result.Stdout,
		Attestation: renderpdf.Attestation{
			RunnerIdentity:       result.Attestation.RunnerIdentity,
			PolicyFingerprint:    result.Attestation.PolicyFingerprint,
			ExecutableSHA256:     result.Attestation.ExecutableSHA256,
			InputSHA256:          result.Attestation.StdinSHA256,
			OutputSHA256:         hex.EncodeToString(digest[:]),
			NetworkDisabled:      result.Attestation.NetworkDisabled,
			ProcessTreeContained: result.Attestation.ProcessTreeContained,
			DigestVerifiedLaunch: result.Attestation.DigestVerifiedLaunch,
			FilesystemIsolated:   result.Attestation.FilesystemIsolated,
			FilesystemMode:       result.Attestation.FilesystemMode,
			PrivateRootInstalled: result.Attestation.PrivateRootInstalled,
			RuntimeIdentity:      result.Attestation.RuntimeIdentity,
			UnixIPCAllowed:       result.Attestation.UnixIPCAllowed,
			RestartCount:         result.Attestation.RestartCount,
		},
	}, err
}

func TestRenderLaneLibreOfficeRoute(t *testing.T) {
	executable := os.Getenv("DOCBANK_TEST_LIBREOFFICE_EXECUTABLE")
	if executable == "" {
		t.Skip("DOCBANK_TEST_LIBREOFFICE_EXECUTABLE is unset; ci-owned")
	}
	renderPolicy, runner := realRenderLanePolicy(t, executable)
	policy := testPolicyWithRenderPDF(t, renderPolicy, 50<<20, 11)
	legacy := map[string][]byte{
		"doc": deriveLegacySeed(t, executable, loadDOCXFixture(t, "libreoffice.docx"), "docx", "doc", "MS Word 97"),
		"ppt": deriveLegacySeed(t, executable, generatedRenderFixture(t, "pptx"), "pptx", "ppt", "MS PowerPoint 97"),
		"xls": deriveLegacySeed(t, executable, syntheticFlatODF("xls"), "fods", "xls", "MS Excel 97"),
	}
	testCases := []struct {
		id        string
		mediaType string
		content   []byte
	}{
		{id: "doc", mediaType: "application/msword", content: legacy["doc"]},
		{id: "odt", mediaType: "application/vnd.oasis.opendocument.text", content: generatedRenderFixture(t, "odt")},
		{id: "rtf", mediaType: "application/rtf", content: generatedRenderFixture(t, "rtf")},
		{id: "ppt", mediaType: "application/vnd.ms-powerpoint", content: legacy["ppt"]},
		{id: "xls", mediaType: "application/vnd.ms-excel", content: legacy["xls"]},
		{id: "ods", mediaType: "application/vnd.oasis.opendocument.spreadsheet", content: generatedRenderFixture(t, "ods")},
		{id: "xlsx", mediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", content: generatedRenderFixture(t, "xlsx")},
	}
	for _, testCase := range testCases {
		t.Run(testCase.id, func(t *testing.T) {
			before := len(runner.calls)
			digest := sha256.Sum256(testCase.content)
			spool := filepath.Join(t.TempDir(), "spool")
			makePrivateDirectory(t, spool)
			prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(testCase.content)), policy, PrepareOptions{
				Directory: spool, DeclaredMediaType: testCase.mediaType,
				ExpectedSize: int64(len(testCase.content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
				MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
			})
			require.NoError(t, err)
			defer func() { require.NoError(t, prepared.Release()) }()
			authorization, err := policy.Authorize(syntheticManifest(t, policy, true), testCase.id)
			require.NoError(t, err)
			var uploaded []byte
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				body, readErr := io.ReadAll(request.Body)
				require.NoError(t, readErr)
				uploaded = decodeRequestDocument(t, body, mediaTypePDF)
				pages, countErr := media.CountPDFPages(uploaded)
				require.NoError(t, countErr)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, ocrResponse(int(pages), len(uploaded)))
			}))
			defer server.Close()
			client := newServerClient(t, server, policy, ClientConfig{MaxRetries: 0})
			result, err := client.Process(t.Context(), prepared, authorization)
			require.NoError(t, err)
			require.NotNil(t, result.ConversionReceipt)
			pages, err := media.CountPDFPages(uploaded)
			require.NoError(t, err)
			assert.Equal(t, int(pages), result.ConversionReceipt.Pages)
			assert.Equal(t, result.ConversionReceipt.PDFSHA256, digestBytes(uploaded))
			assert.Equal(t, result.ConversionReceipt.PDFBytes, int64(len(uploaded)))
			candidate, ok := CandidateFormatByID(testCase.id)
			require.True(t, ok)
			assert.Equal(t, candidate.Family, result.Document.Family)
			assert.Equal(t, "page", result.Document.UnitKind)
			require.Len(t, runner.calls, before+2)
			normalize, render := runner.calls[before], runner.calls[before+1]
			assert.Equal(t, "normalize", normalize.Stage)
			assert.Equal(t, "pdf", render.Stage)
			assert.Contains(t, normalize.Arguments, "--convert-to")
			assert.Contains(t, render.Arguments, "--convert-to")
			assert.Equal(t, "source."+testCase.id, normalize.InputName)
			assert.Equal(t, "source.pdf", render.OutputName)
			assert.NotEmpty(t, normalize.WarmupInput)
			assert.Equal(t, digestBytes(normalize.WarmupInput), normalize.WarmupInputSHA256)
			assert.Equal(t, digestBytes(render.WarmupInput), render.WarmupInputSHA256)
			assert.Equal(t, flatMIMEForRender(testCase.id), normalizedRootMIME(render.Input))
			t.Logf("format=%s source_sha256=%s upload_sha256=%s pages=%d upload_bytes=%d normalize_args=%q pdf_args=%q normalized_mime=%s warmups=%s,%s",
				testCase.id, result.ConversionReceipt.SourceSHA256, result.ConversionReceipt.PDFSHA256,
				result.ConversionReceipt.Pages, result.ConversionReceipt.PDFBytes,
				normalize.Arguments, render.Arguments, normalizedRootMIME(render.Input),
				normalize.WarmupInputSHA256, render.WarmupInputSHA256)
		})
	}
}

func generatedRenderFixture(t *testing.T, formatID string) []byte {
	t.Helper()
	content, generated, err := generatedFixture(formatID)
	require.NoError(t, err)
	require.True(t, generated)
	return content
}

func deriveLegacySeed(t *testing.T, executable string, content []byte, inputExt, outputExt, filter string) []byte {
	t.Helper()
	root := filepath.Join(t.TempDir(), "legacy-seed")
	makePrivateDirectory(t, root)
	inputPath := filepath.Join(root, "source."+inputExt)
	outputDir := filepath.Join(root, "output")
	makePrivateDirectory(t, outputDir)
	require.NoError(t, os.WriteFile(inputPath, content, 0o600))
	profile := filepath.Join(root, "profile")
	profileURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(profile)}).String()
	var output []byte
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		command := exec.CommandContext(t.Context(), executable,
			"--headless", "--norestore", "--nolockcheck", "--nodefault", "--nofirststartwizard",
			"-env:UserInstallation="+profileURL, "--convert-to", outputExt+":"+filter,
			"--outdir", outputDir, inputPath)
		command.Env = append(os.Environ(), "HOME="+root, "TMPDIR="+root)
		output, err = command.CombinedOutput()
		if err == nil {
			break
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 81 {
			break
		}
	}
	require.NoError(t, err, string(output))
	seed, err := os.ReadFile(filepath.Join(outputDir, "source."+outputExt))
	require.NoError(t, err)
	require.NotEmpty(t, seed)
	return seed
}

func realRenderLanePolicy(t *testing.T, executable string) (renderpdf.Policy, *recordingNativeRunner) {
	t.Helper()
	manifest, err := renderpdf.DiscoverRuntime(renderpdf.DefaultRuntimeRoots())
	require.NoError(t, err)
	executableBytes, err := os.ReadFile(executable)
	require.NoError(t, err)
	executableDigest := sha256.Sum256(executableBytes)
	inner, err := sandbox.NewNativeRunner()
	require.NoError(t, err)
	runner := &recordingNativeRunner{inner: inner}
	policy, err := renderpdf.NewPolicy(renderpdf.Renderer{
		Executable: executable, ExecutableSHA256: hex.EncodeToString(executableDigest[:]),
		Runtime: manifest.Files, RuntimeSymlinks: manifest.Symlinks, RuntimeIdentity: manifest.Identity,
		Runner: runner,
	}, renderpdf.DefaultLimits())
	require.NoError(t, err)
	return policy, runner
}

func normalizedRootMIME(data []byte) string {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		for _, attr := range start.Attr {
			if attr.Name.Local == "mimetype" {
				return attr.Value
			}
		}
		return ""
	}
}

func flatMIMEForRender(formatID string) string {
	switch formatID {
	case "ppt":
		return "application/vnd.oasis.opendocument.presentation"
	case "xls", "ods", "xlsx":
		return "application/vnd.oasis.opendocument.spreadsheet"
	default:
		return "application/vnd.oasis.opendocument.text"
	}
}

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
