package trafilatura

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
)

const testRuntimeIdentity = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestProviderRendersExactSuppliedHTMLWithoutFetchingRemoteReferences(t *testing.T) {
	t.Setenv("DOCBANK_TRAFILATURA_AMBIENT_SECRET", "must-not-reach-child")
	provider := newTestProvider(t, helperExecutable(t, "complete"), time.Second, 1<<20)
	source := []byte(`<!doctype html><html><body><h1>Local title</h1><p>Local body</p><a href="https://private.example/fetch-me">link</a><img src="https://private.example/image.png"></body></html>`)
	upload := newTestUpload(source, "article.html", "text/html")

	result, err := document.RenderRendition(t.Context(), provider, upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	assert.Equal(t, "text", result.Evidence.Family)
	assert.Equal(t, document.EvidenceUnitGeneric, result.Evidence.UnitKind)
	require.Len(t, result.Evidence.Units, 1)
	assert.Equal(t, "Local title Local body link", result.Evidence.Units[0].Text)
	assert.NotContains(t, result.Evidence.Units[0].Text, "private.example")
	assert.Equal(t, int64(len(source)), result.Receipt.Usage.InputBytes)
	assert.Equal(t, upload.metadata.SHA256, result.Receipt.SourceSHA256)
}

func TestProviderDeclaresDegradedEvidenceWithoutCompleteSourceProof(t *testing.T) {
	provider := newTestProvider(t, helperExecutable(t, "degraded"), time.Second, 1<<20)
	upload := newTestUpload(testHTML, "article.html", "text/html")

	result, err := provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.NoError(t, err, "cause: %v", errors.Unwrap(err))
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	assert.Equal(t, document.EvidenceUnitGeneric, result.Evidence.UnitKind)
	require.Len(t, result.Evidence.Units, 1)
	assert.Equal(t, "Local title\n\nLocal body", result.Evidence.Units[0].Text)
	assert.Equal(t, []string{"degraded_provenance"}, result.Receipt.Warnings)
}

func TestProviderSupportsSuppliedXHTMLBytes(t *testing.T) {
	provider := newTestProvider(t, helperExecutable(t, "xhtml"), time.Second, 1<<20)
	source := []byte(`<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><body><h1>Local title</h1><p>Local body</p></body></html>`)
	upload := newTestUpload(source, "article.xhtml", "application/xhtml+xml")

	result, err := provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
}

func TestProviderRendersWithoutDisclosingFilename(t *testing.T) {
	provider := newTestProvider(t, helperExecutable(t, "complete"), time.Second, 1<<20)
	upload := newTestUpload(testHTML, "article.html", "text/html")
	authorization := testAuthorization(provider.Descriptor(), upload.Metadata())
	authorization.DiscloseFilename = false

	result, err := document.RenderRendition(t.Context(), provider, upload, authorization)
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
}

func TestProviderRejectsIncompleteOrTruncatedExtraction(t *testing.T) {
	for _, mode := range []string{"extraction-incomplete", "truncated-degraded"} {
		t.Run(mode, func(t *testing.T) {
			provider := newTestProvider(t, helperExecutable(t, mode), time.Second, 1<<20)
			upload := newTestUpload(testHTML, "article.html", "text/html")
			_, err := provider.Render(t.Context(), upload,
				testAuthorization(provider.Descriptor(), upload.Metadata()))
			assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
		})
	}
}

func TestProviderClaimsCompleteSectionsOnlyForLocallyVerifiedStructure(t *testing.T) {
	source := []byte(`<!doctype html><html><body><section><h1>Local title</h1><p>Local body</p></section></body></html>`)
	t.Run("verified path", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "structured"), time.Second, 1<<20)
		upload := newTestUpload(source, "article.html", "text/html")
		result, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		require.NoError(t, err)
		assert.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
		assert.Equal(t, document.EvidenceUnitSection, result.Evidence.UnitKind)
		require.Len(t, result.Evidence.Units, 1)
		assert.Equal(t, "/html[1]/body[1]/section[1]", result.Evidence.Units[0].Locator.Name)
	})
	for _, mode := range []string{"structured-path-drift", "structured-boundary-drift"} {
		t.Run(mode, func(t *testing.T) {
			provider := newTestProvider(t, helperExecutable(t, mode), time.Second, 1<<20)
			upload := newTestUpload(source, "article.html", "text/html")
			_, err := provider.Render(t.Context(), upload,
				testAuthorization(provider.Descriptor(), upload.Metadata()))
			assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
		})
	}
}

func TestProviderUsesVerifiedPathsForDuplicateSectionHeadings(t *testing.T) {
	source := []byte(`<!doctype html><html><body>
		<section><h2>Repeat</h2><p>First</p></section>
		<section><h2>Repeat</h2><p>Second</p></section>
	</body></html>`)
	provider := newTestProvider(t, helperExecutable(t, "structured-duplicate"), time.Second, 1<<20)
	upload := newTestUpload(source, "article.html", "text/html")

	result, err := provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 2)
	assert.Equal(t, "/html[1]/body[1]/section[1]", result.Evidence.Units[0].Locator.Name)
	assert.Equal(t, "/html[1]/body[1]/section[2]", result.Evidence.Units[1].Locator.Name)
}

func TestInspectHTMLBoundsNaturalSectionsAndPreservesSiblingOrdinals(t *testing.T) {
	source := []byte(`<!doctype html><html><body>
		<section><h1>One</h1><p>First</p></section>
		<div></div>
		<section><h2>Two</h2><p>Second</p></section>
		<section><h3>Three</h3><p>Third</p></section>
	</body></html>`)
	authority, err := inspectHTML(source, "text/html", 3)
	require.NoError(t, err)
	require.Len(t, authority.sections, 3)
	assert.Equal(t, []string{
		"/html[1]/body[1]/section[1]",
		"/html[1]/body[1]/section[2]",
		"/html[1]/body[1]/section[3]",
	}, []string{authority.sections[0].path, authority.sections[1].path, authority.sections[2].path})

	authority, err = inspectHTML(source, "text/html", 2)
	require.NoError(t, err)
	assert.Nil(t, authority.sections, "over-bound structure must use degraded provenance")
}

func TestProviderRejectsOutputNotDerivableFromSuppliedBytes(t *testing.T) {
	provider := newTestProvider(t, helperExecutable(t, "fetched"), time.Second, 1<<20)
	upload := newTestUpload(testHTML, "article.html", "text/html")

	_, err := provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	assert.NotContains(t, err.Error(), "REMOTE_FETCH_TOKEN")
}

func TestProviderRejectsMalformedPartialAndDriftedOutput(t *testing.T) {
	for _, mode := range []string{
		"malformed", "unknown-field", "missing-provenance-complete", "version-drift", "runtime-drift", "source-hash-drift",
		"source-size-drift", "partial", "empty", "bad-heading", "complete-inexact",
	} {
		t.Run(mode, func(t *testing.T) {
			provider := newTestProvider(t, helperExecutable(t, mode), time.Second, 1<<20)
			upload := newTestUpload(testHTML, "article.html", "text/html")
			_, err := provider.Render(t.Context(), upload,
				testAuthorization(provider.Descriptor(), upload.Metadata()))
			assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
		})
	}
}

func TestProviderRejectsNonHTMLMalformedXHTMLAndSubstitutedInput(t *testing.T) {
	provider := newTestProvider(t, helperExecutable(t, "failure"), time.Second, 1<<20)
	for _, test := range []struct {
		name, filename, mediaType string
		content                   []byte
	}{
		{name: "plain text", filename: "article.html", mediaType: "text/html", content: []byte("not html")},
		{name: "malformed HTML", filename: "article.html", mediaType: "text/html", content: []byte("<html><body>broken\x00</body></html>")},
		{name: "control character in visible text", filename: "article.html", mediaType: "text/html", content: []byte("<html><body>broken\x07text</body></html>")},
		{name: "vertical tab in visible text", filename: "article.html", mediaType: "text/html", content: []byte("<html><body>broken\vtext</body></html>")},
		{name: "form feed in visible text", filename: "article.html", mediaType: "text/html", content: []byte("<html><body>broken\ftext</body></html>")},
		{name: "malformed XHTML", filename: "article.xhtml", mediaType: "application/xhtml+xml", content: []byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body>broken</html>`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			upload := newTestUpload(test.content, test.filename, test.mediaType)
			_, err := provider.Render(t.Context(), upload,
				testAuthorization(provider.Descriptor(), upload.Metadata()))
			assertProviderCode(t, err, document.RenditionErrorUnsupportedInput)
		})
	}
	t.Run("substituted bytes", func(t *testing.T) {
		upload := newTestUpload(testHTML, "article.html", "text/html")
		upload.Reader = bytes.NewReader([]byte(`<html><body>replacement</body></html>`))
		_, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
	})
}

func TestProviderBoundsOutputAndSanitizesProcessFailure(t *testing.T) {
	t.Run("unbounded stdout stops promptly", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "unbounded-output"), 10*time.Second, 1024)
		upload := newTestUpload(testHTML, "article.html", "text/html")
		started := time.Now()
		_, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
		assert.Less(t, time.Since(started), time.Second)
	})
	t.Run("private stderr", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "failure"), time.Second, 1<<20)
		upload := newTestUpload(testHTML, "article.html", "text/html")
		_, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorTransient)
		assert.NotContains(t, err.Error(), "private-stderr-token")
		assert.NotContains(t, err.Error(), provider.executable)
	})
}

func TestProviderEnforcesTimeoutCancellationAndExpiry(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "wait"), 20*time.Millisecond, 1<<20)
		upload := newTestUpload(testHTML, "article.html", "text/html")
		_, err := provider.Render(t.Context(), upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorCapacity)
	})
	t.Run("cancellation", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "wait"), time.Second, 1<<20)
		upload := newTestUpload(testHTML, "article.html", "text/html")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := provider.Render(ctx, upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		assertProviderCode(t, err, document.RenditionErrorCanceled)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("expired after child", func(t *testing.T) {
		provider := newTestProvider(t, helperExecutable(t, "slow-complete"), time.Second, 1<<20)
		upload := newTestUpload(testHTML, "article.html", "text/html")
		authorization := testAuthorization(provider.Descriptor(), upload.Metadata())
		authorization.ExpiresAt = time.Now().UTC().Add(15 * time.Millisecond).Format(providerutil.TimestampForm)
		_, err := provider.Render(t.Context(), upload, authorization)
		assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
	})
}

func TestProviderCancellationClosesBlockedAuthorizedUploadBeforeStartingRunner(t *testing.T) {
	executable := helperExecutable(t, "complete")
	runner := &recordingRunner{identity: testRunnerIdentity}
	profile := testProfile(t, executable, time.Second, 1<<20)
	profile.Runner = runner
	provider, err := New(profile)
	require.NoError(t, err)
	base := newTestUpload(testHTML, "article.html", "text/html")
	upload := &blockingUpload{metadata: base.metadata, started: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, renderErr := provider.Render(ctx, upload,
			testAuthorization(provider.Descriptor(), upload.Metadata()))
		result <- renderErr
	}()
	<-upload.started
	cancel()

	select {
	case err := <-result:
		assertProviderCode(t, err, document.RenditionErrorCanceled)
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("render remained blocked in the authorized upload read after cancellation")
	}
	assert.Nil(t, runner.request)
}

func TestNewPinsExecutableRuntimeIdentityAndBounds(t *testing.T) {
	first := newTestProvider(t, helperExecutable(t, "complete"), time.Second, 1<<20)
	second := newTestProvider(t, helperExecutable(t, "complete-copy"), time.Second, 1<<20)
	descriptor := first.Descriptor()
	assert.Equal(t, document.RenditionTrustLocalProcess, descriptor.TrustBoundary)
	assert.Equal(t, []document.RenditionFormatCapability{
		{MediaFamily: "text", MediaType: "application/xhtml+xml", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/html", InputKind: document.RenditionInputOriginalFile},
	}, descriptor.SupportedFormats)
	assert.True(t, descriptor.ReturnsStructured)
	assert.NotEqual(t, descriptor.Fingerprint, second.Descriptor().Fingerprint)

	for _, name := range []string{"pyw.exe", "python3", "pythonw.exe", "pythonw3.11.exe"} {
		python := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(python, []byte("synthetic"), 0o700))
		_, err := New(Profile{Executable: python, RuntimeIdentity: testRuntimeIdentity,
			MaxDocumentBytes: 1, MaxResponseBytes: 1, MaxUnits: 1, Timeout: time.Second})
		require.ErrorContains(t, err, "must not be a Python interpreter")
	}

	valid := testProfile(t, helperExecutable(t, "complete"), time.Second, 1024)
	valid.MaxDocumentBytes = 1024
	valid.MaxUnits = 1
	for _, mutate := range []func(*Profile){
		func(profile *Profile) { profile.Executable = "renderer" },
		func(profile *Profile) { profile.RuntimeIdentity = "" },
		func(profile *Profile) { profile.MaxDocumentBytes = 0 },
		func(profile *Profile) { profile.MaxResponseBytes = 0 },
		func(profile *Profile) { profile.MaxUnits = 0 },
		func(profile *Profile) { profile.Timeout = 0 },
	} {
		profile := valid
		mutate(&profile)
		_, err := New(profile)
		require.Error(t, err)
	}
}

var testHTML = []byte(`<!doctype html><html><body><h1>Local title</h1><p>Local body</p></body></html>`)

func newTestProvider(t *testing.T, executable string, timeout time.Duration, maxResponse int64) *Provider {
	t.Helper()
	profile := testProfile(t, executable, timeout, maxResponse)
	provider, err := New(profile)
	require.NoError(t, err)
	return provider
}

func testProfile(t *testing.T, executable string, timeout time.Duration, maxResponse int64) Profile {
	t.Helper()
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	return Profile{
		Executable: executable, ExecutableSHA256: hex.EncodeToString(digest[:]),
		RuntimeIdentity: testRuntimeIdentity, Runner: &recordingRunner{identity: testRunnerIdentity},
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: maxResponse, MaxUnits: 10, Timeout: timeout,
	}
}

func assertProviderCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok, "%T: %v", err, err)
	assert.Equal(t, want, providerErr.Code())
}

func helperExecutable(t *testing.T, mode string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "renderer-"+mode)
	require.NoError(t, os.WriteFile(target, []byte("synthetic isolated executable fixture\n"), 0o700))
	return target
}

type testUpload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
}

func newTestUpload(data []byte, filename, mediaType string) *testUpload {
	digest := sha256.Sum256(data)
	return &testUpload{Reader: bytes.NewReader(data), metadata: document.AuthorizedUploadMetadata{
		Filename: filename, MediaFamily: "text", MediaType: mediaType,
		ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("2", 64), ProviderMetadataChecksum: strings.Repeat("3", 64),
		InputKind: document.RenditionInputOriginalFile,
	}}
}

func (*testUpload) Close() error                                       { return nil }
func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

func testAuthorization(descriptor document.RenditionDescriptor, metadata document.AuthorizedUploadMetadata) document.RenditionAuthorization {
	started := time.Now().UTC().Add(-time.Minute)
	return document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256: metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType,
		InputKind: metadata.InputKind, DiscloseFilename: true, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: started.Format(providerutil.TimestampForm),
		ExpiresAt:    started.Add(10 * time.Minute).Format(providerutil.TimestampForm),
	}
}

var _ document.AuthorizedUpload = (*testUpload)(nil)
var _ io.ReadCloser = (*testUpload)(nil)

type blockingUpload struct {
	metadata  document.AuthorizedUploadMetadata
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func (upload *blockingUpload) Read([]byte) (int, error) {
	upload.startOnce.Do(func() { close(upload.started) })
	<-upload.closed
	return 0, errors.New("synthetic read closed")
}

func (upload *blockingUpload) Close() error {
	upload.closeOnce.Do(func() { close(upload.closed) })
	return nil
}

func (upload *blockingUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

var _ document.AuthorizedUpload = (*blockingUpload)(nil)
