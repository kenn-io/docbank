package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/renderpdf"
)

const testRenderRunnerIdentity = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type testRenderRunner struct {
	mu         sync.Mutex
	start      chan struct{}
	startOnce  sync.Once
	calls      []renderpdf.Request
	identity   string
	pdf        []byte
	err        error
	normalized []byte
}

func (runner *testRenderRunner) Identity() string {
	if runner.identity != "" {
		return runner.identity
	}
	return testRenderRunnerIdentity
}

func (runner *testRenderRunner) Run(ctx context.Context, request renderpdf.Request) (renderpdf.StageResult, error) {
	runner.mu.Lock()
	runner.calls = append(runner.calls, request)
	runner.mu.Unlock()
	if runner.start != nil {
		runner.startOnce.Do(func() { close(runner.start) })
		<-ctx.Done()
		return renderpdf.StageResult{}, ctx.Err()
	}
	if runner.err != nil {
		return renderpdf.StageResult{}, runner.err
	}
	output := runner.normalized
	if request.Stage == "pdf" {
		output = runner.pdf
	}
	return renderStageResult(request, output), nil
}

func renderStageResult(request renderpdf.Request, output []byte) renderpdf.StageResult {
	digest := sha256.Sum256(output)
	return renderpdf.StageResult{
		Output: bytes.Clone(output),
		Attestation: renderpdf.Attestation{
			RunnerIdentity: testRenderRunnerIdentity, PolicyFingerprint: request.PolicyFingerprint,
			ExecutableSHA256: request.ExecutableSHA256, InputSHA256: request.InputSHA256,
			OutputSHA256: hex.EncodeToString(digest[:]), NetworkDisabled: true,
			ProcessTreeContained: true, DigestVerifiedLaunch: true, FilesystemIsolated: true,
			FilesystemMode: "private-root-v1", PrivateRootInstalled: true,
			RuntimeIdentity: request.RuntimeIdentity, UnixIPCAllowed: true,
		},
	}
}

func testRenderPDFPolicy(t *testing.T, pdf []byte, runnerErr error) renderpdf.Policy {
	t.Helper()
	policy, _ := testRenderPDFPolicyAndRunner(t, pdf, runnerErr)
	return policy
}

func testRenderPDFPolicyAndRunner(t *testing.T, pdf []byte, runnerErr error) (renderpdf.Policy, *testRenderRunner) {
	t.Helper()
	runner := &testRenderRunner{
		pdf: pdf, err: runnerErr,
		normalized: []byte(`<?xml version="1.0" encoding="UTF-8"?><office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:mimetype="application/vnd.oasis.opendocument.text"><office:body><office:text><text:p>Synthetic DOCX conversion</text:p></office:text></office:body></office:document>`),
	}
	return testRenderPDFPolicyWithRunner(t, runner), runner
}

func testRenderPDFPolicyWithRunner(t *testing.T, runner *testRenderRunner) renderpdf.Policy {
	t.Helper()
	return testRenderPDFPolicyWithRunnerLimits(t, runner, renderpdf.DefaultLimits())
}

func testRenderPDFPolicyWithRunnerLimits(
	t *testing.T, runner *testRenderRunner, limits renderpdf.Limits,
) renderpdf.Policy {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	executableBytes, err := os.ReadFile(executable)
	require.NoError(t, err)
	executableDigest := sha256.Sum256(executableBytes)
	runtimeFile := renderpdf.RuntimeFile{
		SourcePath: executable, GuestPath: "/usr/bin/test-runner",
		SHA256: hex.EncodeToString(executableDigest[:]), Executable: true,
	}
	runtimeIdentity := testRuntimeIdentity(t, runtimeFile)
	policy, err := renderpdf.NewPolicy(renderpdf.Renderer{
		Executable: executable, ExecutableSHA256: hex.EncodeToString(executableDigest[:]),
		Runtime: []renderpdf.RuntimeFile{runtimeFile}, RuntimeIdentity: runtimeIdentity, Runner: runner,
	}, limits)
	require.NoError(t, err)
	return policy
}

func testRuntimeIdentity(t *testing.T, file renderpdf.RuntimeFile) string {
	t.Helper()
	info, err := os.Stat(file.SourcePath)
	require.NoError(t, err)
	type identityFile struct {
		GuestPath  string `json:"guest_path"`
		SHA256     string `json:"sha256"`
		Bytes      int64  `json:"bytes"`
		Executable bool   `json:"executable"`
	}
	value := struct {
		Files    []identityFile             `json:"files"`
		Symlinks []renderpdf.RuntimeSymlink `json:"symlinks"`
	}{
		Files: []identityFile{{GuestPath: file.GuestPath, SHA256: file.SHA256, Bytes: info.Size(), Executable: file.Executable}},
	}
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func testPolicyWithRenderPDF(t *testing.T, renderPolicy renderpdf.Policy, maxDocumentBytes int64, maxUnits int) Policy {
	t.Helper()
	normalizePolicy, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	policy, err := NewPolicy(PolicyConfig{
		Region: defaultRegion, Model: defaultModel, Retention: RetentionZDR, Training: TrainingOptedOut,
		MaxDocumentBytes: maxDocumentBytes, MaxResponseBytes: 1 << 20, MaxUnits: maxUnits,
		ExtractHeader: true, ExtractFooter: true, NormalizePolicy: normalizePolicy, RenderPDF: &renderPolicy,
	})
	require.NoError(t, err)
	return policy
}

func loadDOCXFixture(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "docx", name))
	require.NoError(t, err)
	return content
}

func prepareTestDOCX(t *testing.T, policy Policy, content []byte) *PreparedDocument {
	t.Helper()
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	return prepared
}

func TestDOCXExplicitBreakLimit(t *testing.T) {
	renderPolicy := testRenderPDFPolicy(t, testMultipagePDF(11), nil)
	policy := testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	authorization, err := policy.Authorize(manifest, "docx")
	require.NoError(t, err)

	var requests int
	client := clientWithTransport(t, policy, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected provider request")
	}))
	_, err = client.Process(t.Context(), prepareTestDOCX(t, policy, loadDOCXFixture(t, "explicit-breaks.docx")), authorization)
	require.ErrorIs(t, err, ErrCapabilityContract)
	assert.Contains(t, err.Error(), "generated PDF")
	assert.Zero(t, requests)
	assert.Zero(t, MetricsFromError(err).Requests)
}

func TestDOCXRouteNegativeSpace(t *testing.T) {
	pdf := testPDF("PDF remains direct")
	policy := testPolicyWithRenderPDF(t, testRenderPDFPolicy(t, testMultipagePDF(1), renderpdf.ErrUnavailable), 1<<20, 10)
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "pdf")
	require.NoError(t, err)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, ocrResponse(1, len(pdf)))
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, ClientConfig{MaxRetries: 0})
	result, err := client.Process(t.Context(), prepareTestDocument(t, policy, pdf), authorization)
	require.NoError(t, err)
	assert.Nil(t, result.ConversionReceipt)
	assert.Equal(t, "pdf", result.Document.Family)
}

func TestDOCXClientUpload(t *testing.T) {
	pdf := testMultipagePDF(11)
	renderPolicy, runner := testRenderPDFPolicyAndRunner(t, pdf, nil)
	policy := testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 11)
	manifest := syntheticManifest(t, policy, true)
	authorization, err := policy.Authorize(manifest, "docx")
	require.NoError(t, err)
	original := loadDOCXFixture(t, "explicit-breaks.docx")
	prepared := prepareTestDOCX(t, policy, original)

	var bodies [][]byte
	var mediaTypes []string
	requestCount := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount++
		body, readErr := io.ReadAll(request.Body)
		if !assert.NoError(t, readErr) {
			return
		}
		var wire struct {
			Document struct {
				URL string `json:"document_url"`
			} `json:"document"`
		}
		if !assert.NoError(t, json.Unmarshal(body, &wire)) {
			return
		}
		prefix := "data:application/pdf;base64,"
		encoded := strings.TrimPrefix(wire.Document.URL, prefix)
		uploaded, decodeErr := decodeBase64(encoded)
		if !assert.NoError(t, decodeErr) {
			return
		}
		bodies = append(bodies, uploaded)
		mediaTypes = append(mediaTypes, strings.SplitN(wire.Document.URL, ";base64,", 2)[0])
		if requestCount == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, ocrResponse(11, len(pdf)))
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, ClientConfig{MaxRetries: 1, MaxRetryDelay: time.Millisecond})
	result, err := client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	require.Len(t, bodies, 2)
	assert.Equal(t, bodies[0], bodies[1])
	assert.Equal(t, pdf, bodies[0])
	assert.Equal(t, []string{"data:" + mediaTypePDF, "data:" + mediaTypePDF}, mediaTypes)
	assert.Equal(t, "word", result.Document.Family)
	assert.Equal(t, "page", result.Document.UnitKind)
	assert.Equal(t, 11, result.UnitsProcessed)
	assert.Equal(t, int64(len(pdf)), *result.ProviderBytes)
	require.NotNil(t, result.ConversionReceipt)
	assert.Equal(t, prepared.SHA256(), result.ConversionReceipt.SourceSHA256)
	assert.Equal(t, digestBytes(pdf), result.ConversionReceipt.PDFSHA256)
	assert.Equal(t, int64(len(pdf)), result.ConversionReceipt.PDFBytes)
	assert.Equal(t, 11, result.ConversionReceipt.Pages)
	assert.Equal(t, 2, result.Metrics.Requests)
	assert.Len(t, runner.calls, 2)
}

func TestDOCXConversionErrorClassification(t *testing.T) {
	_, runner := testRenderPDFPolicyAndRunner(t, testMultipagePDF(2), nil)
	limits := renderpdf.DefaultLimits()
	limits.MaxPages = 1
	renderPolicy := testRenderPDFPolicyWithRunnerLimits(t, runner, limits)
	policy := testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 10)
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "docx")
	require.NoError(t, err)
	client := baseClientForTest(t, policy)
	_, err = client.Process(t.Context(), prepareTestDOCX(t, policy, loadDOCXFixture(t, "explicit-breaks.docx")), authorization)
	require.ErrorIs(t, err, ErrCapabilityContract)
	require.ErrorIs(t, err, renderpdf.ErrPageLimit)
	require.ErrorIs(t, classifyDOCXConversionError(t.Context(), renderpdf.ErrRendererChanged), ErrTransientResponse)

	deadlineErr := classifyDOCXConversionError(t.Context(), fmt.Errorf("render PDF conversion timed out: %w", context.DeadlineExceeded))
	require.ErrorIs(t, deadlineErr, ErrTransientResponse)
	require.ErrorIs(t, deadlineErr, context.DeadlineExceeded)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	canceledErr := classifyDOCXConversionError(canceled, context.Canceled)
	require.ErrorIs(t, canceledErr, context.Canceled)
	require.NotErrorIs(t, canceledErr, ErrTransientResponse)

	parentDeadline, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	parentDeadlineErr := classifyDOCXConversionError(parentDeadline, context.DeadlineExceeded)
	require.ErrorIs(t, parentDeadlineErr, context.DeadlineExceeded)
	require.NotErrorIs(t, parentDeadlineErr, ErrTransientResponse)

	_, runner = testRenderPDFPolicyAndRunner(t, testMultipagePDF(1), nil)
	limits = renderpdf.DefaultLimits()
	limits.MaxSourceBytes = 1
	renderPolicy = testRenderPDFPolicyWithRunnerLimits(t, runner, limits)
	policy = testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 10)
	authorization, err = policy.Authorize(syntheticManifest(t, policy, true), "docx")
	require.NoError(t, err)
	client = baseClientForTest(t, policy)
	_, err = client.Process(t.Context(), prepareTestDOCX(t, policy, loadDOCXFixture(t, "explicit-breaks.docx")), authorization)
	require.ErrorIs(t, err, ErrCapabilityContract)
	require.ErrorIs(t, err, renderpdf.ErrSourceTooLarge)
	require.NotErrorIs(t, err, renderpdf.ErrOutputTooLarge)

	_, runner = testRenderPDFPolicyAndRunner(t, testMultipagePDF(1), nil)
	limits = renderpdf.DefaultLimits()
	limits.MaxXMLDepth = 1
	renderPolicy = testRenderPDFPolicyWithRunnerLimits(t, runner, limits)
	policy = testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 10)
	authorization, err = policy.Authorize(syntheticManifest(t, policy, true), "docx")
	require.NoError(t, err)
	client = baseClientForTest(t, policy)
	_, err = client.Process(t.Context(), prepareTestDOCX(t, policy, loadDOCXFixture(t, "explicit-breaks.docx")), authorization)
	require.ErrorIs(t, err, ErrCapabilityContract)
	require.ErrorIs(t, err, renderpdf.ErrXMLLimit)

	_, runner = testRenderPDFPolicyAndRunner(t, testMultipagePDF(1), nil)
	renderPolicy = testRenderPDFPolicyWithRunner(t, runner)
	runner.identity = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	policy = testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 10)
	authorization, err = policy.Authorize(syntheticManifest(t, policy, true), "docx")
	require.NoError(t, err)
	client = baseClientForTest(t, policy)
	_, err = client.Process(t.Context(), prepareTestDOCX(t, policy, loadDOCXFixture(t, "explicit-breaks.docx")), authorization)
	require.ErrorIs(t, err, ErrTransientResponse)
	require.ErrorIs(t, err, renderpdf.ErrRendererChanged)

	_, runner = testRenderPDFPolicyAndRunner(t, testMultipagePDF(1), nil)
	runner.normalized = bytes.ReplaceAll(runner.normalized, []byte("Synthetic DOCX conversion"), []byte(`<draw:object xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"/>`))
	renderPolicy = testRenderPDFPolicyWithRunner(t, runner)
	policy = testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 10)
	authorization, err = policy.Authorize(syntheticManifest(t, policy, true), "docx")
	require.NoError(t, err)
	client = baseClientForTest(t, policy)
	_, err = client.Process(t.Context(), prepareTestDOCX(t, policy, loadDOCXFixture(t, "explicit-breaks.docx")), authorization)
	require.ErrorIs(t, err, ErrInvalidSource)
	require.ErrorIs(t, err, renderpdf.ErrSourceRejected)
}

func TestDOCXNoNativeFallback(t *testing.T) {
	renderPolicy := testRenderPDFPolicy(t, testMultipagePDF(1), renderpdf.ErrUnavailable)
	policy := testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 10)
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "docx")
	require.NoError(t, err)
	requests := 0
	client := clientWithTransport(t, policy, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected provider request")
	}))
	_, err = client.Process(t.Context(), prepareTestDOCX(t, policy, loadDOCXFixture(t, "realistic-word.docx")), authorization)
	require.ErrorIs(t, err, ErrTransientResponse)
	require.ErrorIs(t, err, renderpdf.ErrUnavailable)
	assert.Zero(t, requests)
}

func TestDOCXProviderCountMismatch(t *testing.T) {
	pdf := testMultipagePDF(3)
	renderPolicy := testRenderPDFPolicy(t, pdf, nil)
	policy := testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 10)
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "docx")
	require.NoError(t, err)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, ocrResponse(2, len(pdf)))
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, ClientConfig{MaxRetries: 0})
	_, err = client.Process(t.Context(), prepareTestDOCX(t, policy, loadDOCXFixture(t, "libreoffice.docx")), authorization)
	require.ErrorIs(t, err, ErrCapabilityContract)
	assert.Contains(t, err.Error(), "local exact count 3")
}

func baseClientForTest(t *testing.T, policy Policy) *Client {
	t.Helper()
	return clientWithTransport(t, policy, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected provider request")
	}))
}

func clientWithTransport(t *testing.T, policy Policy, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key", MaxRetries: 0,
		HTTPClient: &http.Client{Transport: transport},
	})
	require.NoError(t, err)
	return client
}

func ocrResponse(pages, bytes int) string {
	pageValues := make([]string, pages)
	for index := range pageValues {
		pageValues[index] = `{"index":` + itoa(index) + `,"markdown":"synthetic"}`
	}
	return `{"model":"mistral-ocr-4-0","pages":[` + strings.Join(pageValues, ",") + `],"usage_info":{"pages_processed":` + itoa(pages) + `,"doc_size_bytes":` + itoa(bytes) + `}}`
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

func decodeBase64(value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode synthetic request document: %w", err)
	}
	return decoded, nil
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
