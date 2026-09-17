package renderpdf

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/ocr"
)

const testRunnerIdentity = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type recordingRunner struct {
	identity string
	output   []byte
	pdf      []byte
	calls    []Request
	err      error
}

func (runner *recordingRunner) Identity() string { return runner.identity }

func (runner *recordingRunner) Run(_ context.Context, request Request) (StageResult, error) {
	runner.calls = append(runner.calls, request)
	if runner.err != nil {
		return StageResult{}, runner.err
	}
	output := runner.output
	if request.Stage == "pdf" {
		output = runner.pdf
		if output == nil {
			output = testPDFBytes()
		}
	}
	return stageResult(request, output), nil
}

func TestConvertSafeDOCXAndReceiptSource(t *testing.T) {
	normalized := flatODF(FlatTextKind, "<text:p xmlns:text=\"urn:oasis:names:tc:opendocument:xmlns:text:1.0\">Hello</text:p>")
	runner := &recordingRunner{identity: testRunnerIdentity, output: normalized}
	policy := testPolicy(t, runner)
	original := zipDocument(t, "docx")
	source := testSource(t, original, docxMediaType)
	result, err := Convert(t.Context(), source, "docx", policy)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Len(t, runner.calls, 2)
	assert.Equal(t, "normalize", runner.calls[0].Stage)
	assert.Equal(t, "pdf", runner.calls[1].Stage)
	assert.Equal(t, normalized, runner.calls[1].Input)
	receipt := result.Receipt()
	assert.Equal(t, "docx", receipt.SourceFormat)
	assert.Equal(t, digest(original), receipt.SourceSHA256)
	assert.Equal(t, digest(normalized), receipt.NormalizedSHA256)
	pdf := result.PDF()
	assert.Equal(t, digest(pdf), receipt.PDFSHA256)
	fresh, err := result.Source()
	require.NoError(t, err)
	defer func() { _ = fresh.Content.Close() }()
	assert.Equal(t, "application/pdf", fresh.MediaType)
	assert.Equal(t, int64(len(pdf)), fresh.Size)
	assert.Equal(t, receipt.PDFSHA256, fresh.SHA256)
	got, err := io.ReadAll(fresh.Content)
	require.NoError(t, err)
	assert.Equal(t, pdf, got)
	pdf[0] ^= 1
	assert.NotEqual(t, pdf[0], result.PDF()[0])
}

func TestConvertRejectsUnsafeNormalizedOutputBeforePDFStage(t *testing.T) {
	tests := []struct {
		name       string
		normalized string
	}{
		{name: "external href", normalized: string(flatODF(FlatTextKind,
			"<draw:image xmlns:draw=\"urn:oasis:names:tc:opendocument:xmlns:drawing:1.0\" xmlns:xlink=\"http://www.w3.org/1999/xlink\" xlink:href=\"https://example.test/pixel.png\"/>"))},
		{name: "linked field", normalized: string(flatODF(FlatTextKind,
			"<f>WEBSERVICE(\"https://example.test\")</f>"))},
		{name: "opaque object", normalized: string(flatODF(FlatTextKind,
			"<draw:object xmlns:draw=\"urn:oasis:names:tc:opendocument:xmlns:drawing:1.0\"/>"))},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			runner := &recordingRunner{identity: testRunnerIdentity, output: []byte(testCase.normalized)}
			policy := testPolicy(t, runner)
			_, err := Convert(t.Context(), testSource(t, zipDocument(t, "docx"), docxMediaType), "docx", policy)
			require.Error(t, err)
			assert.Len(t, runner.calls, 1)
		})
	}
}

func TestConvertRejectsSourceIdentityAndExtensionBeforeRunner(t *testing.T) {
	runner := &recordingRunner{identity: testRunnerIdentity, output: flatODF(FlatTextKind, "<text:p/>")}
	policy := testPolicy(t, runner)
	original := zipDocument(t, "docx")
	bad := testSource(t, original, docxMediaType)
	bad.SHA256 = strings.Repeat("0", sha256.Size*2)
	_, err := Convert(t.Context(), bad, "docx", policy)
	require.Error(t, err)
	assert.Empty(t, runner.calls)
	_, err = Convert(t.Context(), testSource(t, original, docxMediaType), "DOCX", policy)
	require.Error(t, err)
	assert.Empty(t, runner.calls)
}

func TestConvertRejectsStageAttestationMismatch(t *testing.T) {
	runner := &recordingRunner{identity: testRunnerIdentity, output: flatODF(FlatTextKind, "<text:p/>")}
	runner.err = nil
	policy := testPolicy(t, runner)
	original := zipDocument(t, "docx")
	result, err := Convert(t.Context(), testSource(t, original, docxMediaType), "docx", policy)
	require.NoError(t, err)
	require.NotNil(t, result)

	badRunner := &mismatchedRunner{recordingRunner{identity: testRunnerIdentity, output: flatODF(FlatTextKind, "<text:p/>")}}
	badPolicy := testPolicy(t, badRunner)
	_, err = Convert(t.Context(), testSource(t, original, docxMediaType), "docx", badPolicy)
	require.ErrorContains(t, err, "attestation")
}

func TestConvertCloseFailureDiscardsResult(t *testing.T) {
	runner := &recordingRunner{identity: testRunnerIdentity, output: flatODF(FlatTextKind, "<text:p/>")}
	policy := testPolicy(t, runner)
	original := zipDocument(t, "docx")
	content := &closeErrorReader{Reader: bytes.NewReader(original)}
	digestValue := digest(original)
	source, err := ocr.NewSource(content, docxMediaType, int64(len(original)), digestValue)
	require.NoError(t, err)
	result, err := Convert(t.Context(), source, "docx", policy)
	assert.Nil(t, result)
	require.ErrorContains(t, err, "close render PDF source")
}

func testPolicy(t *testing.T, runner Runner) Policy {
	t.Helper()
	return testPolicyWithLimits(t, runner, DefaultLimits())
}

func testPolicyWithLimits(t *testing.T, runner Runner, limits Limits) Policy {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(executable)
	require.NoError(t, err)
	runtimeFile := RuntimeFile{SourcePath: executable, GuestPath: "/usr/bin/test-runner", SHA256: digest(content), Executable: true}
	runtimeIdentity, err := runtimeIdentityForManifest([]RuntimeFile{runtimeFile}, nil)
	require.NoError(t, err)
	policy, err := NewPolicy(Renderer{
		Executable: executable, ExecutableSHA256: digest(content),
		Runtime: []RuntimeFile{runtimeFile}, RuntimeIdentity: runtimeIdentity, Runner: runner,
	}, limits)
	require.NoError(t, err)
	return policy
}

//nolint:unparam // the helper keeps the source contract explicit for future profiles.
func testSource(t *testing.T, content []byte, mediaType string) ocr.Source {
	t.Helper()
	source, err := ocr.NewSource(io.NopCloser(bytes.NewReader(content)), mediaType,
		int64(len(content)), digest(content))
	require.NoError(t, err)
	return source
}

func stageResult(request Request, output []byte) StageResult {
	return StageResult{
		Output: bytes.Clone(output),
		Attestation: Attestation{
			RunnerIdentity: testRunnerIdentity, PolicyFingerprint: request.PolicyFingerprint,
			ExecutableSHA256: request.ExecutableSHA256, InputSHA256: request.InputSHA256,
			OutputSHA256: digest(output), NetworkDisabled: true,
			ProcessTreeContained: true, DigestVerifiedLaunch: true, FilesystemIsolated: true,
			FilesystemMode: "private-root-v1", PrivateRootInstalled: true,
			RuntimeIdentity: request.RuntimeIdentity, UnixIPCAllowed: true,
		},
	}
}

//nolint:unparam // the helper retains the format argument for fixture readability.
func zipDocument(t *testing.T, format string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	_ = format
	require.NoError(t, writeZip(archive, "[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`))
	require.NoError(t, writeZip(archive, "word/document.xml", `<document/>`))
	require.NoError(t, archive.Close())
	return buffer.Bytes()
}

func writeZip(archive *zip.Writer, name, content string) error {
	file, err := archive.Create(name)
	if err != nil {
		return fmt.Errorf("create zip entry: %w", err)
	}
	_, err = io.WriteString(file, content)
	return err
}

//nolint:unparam // the helper retains the profile argument for fixture readability.
func flatODF(kind, body string) []byte {
	_ = kind
	mimeType := "application/vnd.oasis.opendocument.text"
	return []byte(`<?xml version="1.0" encoding="UTF-8"?><office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" office:mimetype="` + mimeType + `"><office:body>` + body + `</office:body></office:document>`)
}

func testPDFBytes() []byte {
	return testPDFBytesPages(1)
}

func testPDFBytesPages(pages int) []byte {
	pdf := fpdf.New("P", "pt", "A4", "")
	for range pages {
		pdf.AddPage()
		pdf.SetFont("Arial", "", 12)
		pdf.Cell(40, 10, "synthetic")
	}
	var output bytes.Buffer
	if err := pdf.Output(&output); err != nil {
		return nil
	}
	return output.Bytes()
}

type closeErrorReader struct {
	io.Reader
}

func (*closeErrorReader) Close() error { return errors.New("synthetic close failure") }

type mismatchedRunner struct {
	recordingRunner
}

func (runner *mismatchedRunner) Run(_ context.Context, request Request) (StageResult, error) {
	runner.calls = append(runner.calls, request)
	output := runner.output
	if request.Stage == "pdf" {
		output = testPDFBytes()
	}
	result := stageResult(request, output)
	result.Attestation.OutputSHA256 = strings.Repeat("0", sha256.Size*2)
	return result, nil
}
