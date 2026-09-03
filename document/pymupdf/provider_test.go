package pymupdf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

const testRuntimeIdentity = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var testHelperBinary string

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "docbank-pymupdf-test-")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	testHelperBinary = filepath.Join(directory, "renderer-base"+extension)
	command := exec.Command("go", "build", "-o", testHelperBinary, "./testdata/helper")
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build PyMuPDF test helper: %v\n%s", buildErr, output)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

func TestProviderRendersExactPDFPagesThroughFixedProtocol(t *testing.T) {
	t.Setenv("DOCBANK_PYMUPDF_AMBIENT_SECRET", "must-not-reach-child")
	executable := helperExecutable(t, "success")
	provider, err := New(Profile{
		Executable: executable, ExecutableSHA256: executableSHA256(t, executable),
		RuntimeIdentity:  testRuntimeIdentity,
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		MaxPages: 10, Timeout: time.Second,
	})
	require.NoError(t, err)
	source := testPDF(2)
	upload := newTestUpload(source)
	authorization := testAuthorization(provider.Descriptor(), upload.Metadata())

	result, err := document.RenderRendition(t.Context(), provider, upload, authorization)
	require.NoError(t, err)
	assert.Equal(t, document.SourceEvidenceContractV1, result.Evidence.ContractVersion)
	assert.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
	assert.Equal(t, "pdf", result.Evidence.Family)
	assert.Equal(t, document.EvidenceUnitPage, result.Evidence.UnitKind)
	require.Len(t, result.Evidence.Units, 2)
	assert.Equal(t, "first page", result.Evidence.Units[0].Text)
	assert.Equal(t, "second page", result.Evidence.Units[1].Text)
	assert.Equal(t, document.SourceEvidenceLocatorV1{
		Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
		Start: 1, End: 1,
	}, result.Evidence.Units[0].Locator)
	assert.Equal(t, int64(len(source)), result.Receipt.Usage.InputBytes)
	assert.Equal(t, int64(2), result.Receipt.Usage.Units)
	assert.Equal(t, upload.metadata.SHA256, result.Receipt.SourceSHA256)
}

func TestNewPinsExecutableAndRuntimeIdentityInDescriptor(t *testing.T) {
	firstExecutable := helperExecutable(t, "success")
	first, err := New(Profile{
		Executable: firstExecutable, ExecutableSHA256: executableSHA256(t, firstExecutable),
		RuntimeIdentity:  testRuntimeIdentity,
		MaxDocumentBytes: 1024, MaxResponseBytes: 1024, MaxPages: 2, Timeout: time.Second,
	})
	require.NoError(t, err)
	secondExecutable := helperExecutable(t, "success-copy")
	second, err := New(Profile{
		Executable: secondExecutable, ExecutableSHA256: executableSHA256(t, secondExecutable),
		RuntimeIdentity:  testRuntimeIdentity + ".revision",
		MaxDocumentBytes: 1024, MaxResponseBytes: 1024, MaxPages: 2, Timeout: time.Second,
	})
	require.NoError(t, err)

	descriptor := first.Descriptor()
	assert.Equal(t, document.RenditionTrustLocalProcess, descriptor.TrustBoundary)
	assert.Equal(t, []document.RenditionFormatCapability{{
		MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile,
	}}, descriptor.SupportedFormats)
	assert.True(t, descriptor.ReturnsStructured)
	assert.False(t, descriptor.ReturnsMarkdown)
	assert.NotEqual(t, descriptor.Fingerprint, second.Descriptor().Fingerprint)
	descriptor.SupportedFormats[0].MediaType = "text/plain"
	assert.Equal(t, "application/pdf", first.Descriptor().SupportedFormats[0].MediaType)
}

func TestProviderRejectsExecutableReplacementBeforeExecution(t *testing.T) {
	executable := helperExecutable(t, "success")
	provider := newTestProvider(t, executable, time.Second, 1<<20)
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(executable, append(data, []byte("replacement")...), 0o700))
	upload := newTestUpload(testPDF(2))

	_, err = provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
}

func TestProviderRejectsMalformedPartialAndDriftedOutput(t *testing.T) {
	for _, mode := range []string{
		"malformed", "unknown-field", "version-drift", "runtime-drift", "source-hash-drift",
		"source-size-drift", "partial", "page-count-drift", "gap", "duplicate", "empty-unexplained",
	} {
		t.Run(mode, func(t *testing.T) {
			provider := newTestProvider(t, helperExecutable(t, mode), 30*time.Second, 1<<20)
			upload := newTestUpload(testPDF(2))

			_, err := provider.Render(t.Context(), upload,
				testAuthorization(provider.Descriptor(), upload.Metadata()))
			assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
		})
	}
}

func TestProviderAcceptsExplicitlyExplainedEmptyPage(t *testing.T) {
	provider := newTestProvider(t, helperExecutable(t, "empty-explained"), time.Second, 1<<20)
	upload := newTestUpload(testPDF(2))

	result, err := provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 2)
	assert.Empty(t, result.Evidence.Units[0].Text)
}

func TestProviderBoundsOutputAndSanitizesProcessFailure(t *testing.T) {
	t.Run("oversized stdout", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "oversized"), time.Second, 1024)
		upload := newTestUpload(testPDF(2))
		_, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	})
	t.Run("authorization response limit", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "oversized"), time.Second, 1<<20)
		upload := newTestUpload(testPDF(2))
		authorization := testAuthorization(provider.Descriptor(), upload.Metadata())
		authorization.MaxTotalResultBytes = 1024
		_, err := provider.Render(t.Context(), upload, authorization)
		assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	})
	t.Run("private stderr", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "failure"), time.Second, 1<<20)
		upload := newTestUpload(testPDF(2))
		_, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorTransient)
		assert.NotContains(t, err.Error(), "private-stderr-token")
		assert.NotContains(t, err.Error(), provider.executable)
	})
}

func TestProviderTerminatesChildThatNeverStopsOversizedOutput(t *testing.T) {
	provider := newTestProvider(t, helperExecutable(t, "unbounded-output"), 2*time.Second, 1024)
	upload := newTestUpload(testPDF(2))
	started := time.Now()

	_, err := provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	assert.Less(t, time.Since(started), time.Second)
	assert.NotContains(t, err.Error(), "broken pipe")
}

func TestProviderEnforcesTimeoutAndCancellation(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "wait"), 20*time.Millisecond, 1<<20)
		upload := newTestUpload(testPDF(2))
		started := time.Now()
		_, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorTransient)
		assert.Less(t, time.Since(started), time.Second)
	})
	t.Run("cancellation", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "wait"), time.Second, 1<<20)
		upload := newTestUpload(testPDF(2))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := provider.Render(ctx, upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorCanceled)
		assert.ErrorIs(t, err, context.Canceled)
	})
	t.Run("authorization expiry", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "wait"), time.Second, 1<<20)
		upload := newTestUpload(testPDF(2))
		authorization := testAuthorization(provider.Descriptor(), upload.Metadata())
		authorization.ExpiresAt = time.Now().UTC().Add(20 * time.Millisecond).Format(timestampForm)
		_, err := provider.Render(t.Context(), upload, authorization)
		assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestProviderRejectsCancellationObservedAfterLargeChildResponse(t *testing.T) {
	const pages = 20_000
	executable := helperExecutable(t, "many-pages")
	provider, err := New(Profile{
		Executable: executable, ExecutableSHA256: executableSHA256(t, executable),
		RuntimeIdentity:  testRuntimeIdentity,
		MaxDocumentBytes: 8 << 20, MaxResponseBytes: 8 << 20,
		MaxPages: pages, Timeout: 30 * time.Second,
	})
	require.NoError(t, err)
	upload := newTestUpload(testPDF(pages))
	ctx := newCancelWhenCheckedContext(t.Context())
	defer ctx.cancel()

	_, err = provider.Render(ctx, upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorCanceled)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestProviderRejectsUnverifiedOrSubstitutedInputBeforeExecution(t *testing.T) {
	provider := newTestProvider(t, helperExecutable(t, "failure"), time.Second, 1<<20)
	t.Run("not PDF", func(t *testing.T) {
		upload := newTestUpload([]byte("not a PDF"))
		_, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorUnsupportedInput)
	})
	t.Run("substituted bytes", func(t *testing.T) {
		upload := newTestUpload(testPDF(2))
		upload.Reader = bytes.NewReader(testPDF(1))
		_, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
	})
}

func TestNewRejectsInterpreterAndInvalidBounds(t *testing.T) {
	python := filepath.Join(t.TempDir(), "python3")
	require.NoError(t, os.WriteFile(python, []byte("synthetic"), 0o700))
	_, err := New(Profile{
		Executable: python, RuntimeIdentity: testRuntimeIdentity,
		MaxDocumentBytes: 1, MaxResponseBytes: 1, MaxPages: 1, Timeout: time.Second,
	})
	require.ErrorContains(t, err, "must not be a Python interpreter")

	validExecutable := helperExecutable(t, "success")
	valid := Profile{
		Executable: validExecutable, ExecutableSHA256: executableSHA256(t, validExecutable),
		RuntimeIdentity:  testRuntimeIdentity,
		MaxDocumentBytes: 1024, MaxResponseBytes: 1024, MaxPages: 2, Timeout: time.Second,
	}
	for _, mutate := range []func(*Profile){
		func(profile *Profile) { profile.Executable = "renderer" },
		func(profile *Profile) { profile.ExecutableSHA256 = "" },
		func(profile *Profile) { profile.ExecutableSHA256 = strings.Repeat("0", 64) },
		func(profile *Profile) { profile.RuntimeIdentity = "" },
		func(profile *Profile) { profile.MaxDocumentBytes = 0 },
		func(profile *Profile) { profile.MaxResponseBytes = 0 },
		func(profile *Profile) { profile.MaxPages = 0 },
		func(profile *Profile) { profile.MaxPages = 100_001 },
		func(profile *Profile) { profile.Timeout = 0 },
	} {
		profile := valid
		mutate(&profile)
		_, err := New(profile)
		require.Error(t, err)
	}
}

func newTestProvider(t *testing.T, executable string, timeout time.Duration, maxResponse int64) *Provider {
	t.Helper()
	provider, err := New(Profile{
		Executable: executable, ExecutableSHA256: executableSHA256(t, executable),
		RuntimeIdentity:  testRuntimeIdentity,
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: maxResponse,
		MaxPages: 10, Timeout: timeout,
	})
	require.NoError(t, err)
	return provider
}

func assertProviderCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, want, providerErr.Code())
}

type cancelWhenCheckedContext struct {
	context.Context

	done chan struct{}
	once sync.Once
}

func newCancelWhenCheckedContext(parent context.Context) *cancelWhenCheckedContext {
	return &cancelWhenCheckedContext{Context: parent, done: make(chan struct{})}
}

func (ctx *cancelWhenCheckedContext) Done() <-chan struct{} { return ctx.done }

func (ctx *cancelWhenCheckedContext) Err() error {
	ctx.cancel()
	return context.Canceled
}

func (ctx *cancelWhenCheckedContext) cancel() {
	ctx.once.Do(func() { close(ctx.done) })
}

func helperExecutable(t *testing.T, mode string) string {
	t.Helper()
	extension := filepath.Ext(testHelperBinary)
	target := filepath.Join(t.TempDir(), "renderer-"+mode+extension)
	data, err := os.ReadFile(testHelperBinary)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(target, data, 0o700))
	return target
}

func executableSHA256(t *testing.T, executable string) string {
	t.Helper()
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type testUpload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
}

func newTestUpload(data []byte) *testUpload {
	digest := sha256.Sum256(data)
	return &testUpload{
		Reader: bytes.NewReader(data),
		metadata: document.AuthorizedUploadMetadata{
			Filename: "document.pdf", MediaFamily: "pdf", MediaType: "application/pdf",
			ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
			CapabilityRecordChecksum: strings.Repeat("2", 64),
			ProviderMetadataChecksum: strings.Repeat("3", 64),
			InputKind:                document.RenditionInputOriginalFile,
		},
	}
}

func (*testUpload) Close() error { return nil }

func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

func testAuthorization(
	descriptor document.RenditionDescriptor, metadata document.AuthorizedUploadMetadata,
) document.RenditionAuthorization {
	started := time.Now().UTC().Add(-time.Minute)
	return document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType,
		InputKind: metadata.InputKind, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: started.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:    started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}
}

func testPDF(pageCount int) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", pdfPageReferences(pageCount), pageCount),
	}
	for page := range pageCount {
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Rotate %d >>", page*90))
	}
	var output bytes.Buffer
	_, _ = output.WriteString("%PDF-1.4\n%synthetic\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

func pdfPageReferences(pageCount int) string {
	references := make([]string, pageCount)
	for page := range pageCount {
		references[page] = fmt.Sprintf("%d 0 R", page+3)
	}
	return strings.Join(references, " ")
}

var _ document.AuthorizedUpload = (*testUpload)(nil)
var _ io.ReadCloser = (*testUpload)(nil)
