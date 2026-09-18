package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
	"go.kenn.io/docbank/document/renderpdf"
)

func TestProcessorSnapshotsCapabilityManifest(t *testing.T) {
	content := testPDF("processor snapshot")
	policy := testPolicy(t, 1024, 10)
	manifest := syntheticManifest(t, policy, true)
	providerBytes := int64(len(content))
	for index := range manifest.Results {
		if manifest.Results[index].FormatID == formatIDPDF {
			manifest.Results[index].ProviderBytes = &providerBytes
		}
	}
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			_, _ = io.Copy(io.Discard, request.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"# Synthetic snapshot"}],"usage_info":{"pages_processed":1}}`,
				)),
			}, nil
		})},
	})
	require.NoError(t, err)
	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, spoolDirectory)
	processor, err := NewProcessor(ProcessorConfig{
		Client: client, Policy: policy, CapabilityManifest: manifest,
		SpoolDirectory: spoolDirectory, MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.NoError(t, err)

	providerBytes = -1
	for index := range manifest.Results {
		if manifest.Results[index].FormatID == formatIDPDF {
			manifest.Results[index].Status = ProbeStatusRejected
		}
	}
	digest := sha256.Sum256(content)
	source, err := ocr.NewSource(
		io.NopCloser(bytes.NewReader(content)), mediaTypePDF, int64(len(content)), hex.EncodeToString(digest[:]),
	)
	require.NoError(t, err)

	result, err := processor.Process(t.Context(), source)
	require.NoError(t, err)
	require.NotEmpty(t, result.Document.Chunks)
	assert.Equal(t, []string{"Synthetic snapshot"}, result.Document.Chunks[0].HeadingPath)
	assert.Equal(t, processor.policyFingerprint, result.PolicyFingerprint)
	assert.Equal(t, source.SHA256, result.SourceSHA256)
	assert.Empty(t, result.UploadSHA256)
}

func TestProcessorClassifiesStagingAndSourceFailures(t *testing.T) {
	content := testPDF("processor classification")
	digest := sha256.Sum256(content)
	newSource := func(hash string) ocr.Source {
		source, err := ocr.NewSource(
			io.NopCloser(bytes.NewReader(content)), mediaTypePDF, int64(len(content)), hash,
		)
		require.NoError(t, err)
		return source
	}

	missingDirectory := filepath.Join(t.TempDir(), "missing")
	processor := newProcessorWithoutRequests(t, missingDirectory)
	_, err := processor.Process(t.Context(), newSource(hex.EncodeToString(digest[:])))
	require.Error(t, err)
	assert.Equal(t, ocr.ErrorTransient, ocr.ErrorKindOf(err))
	assert.True(t, ocr.IsRetryable(err))

	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, spoolDirectory)
	processor = newProcessorWithoutRequests(t, spoolDirectory)
	_, err = processor.Process(t.Context(), newSource(zeroSHA256()))
	require.Error(t, err)
	assert.Equal(t, ocr.ErrorInvalidInput, ocr.ErrorKindOf(err))
	assert.False(t, ocr.IsRetryable(err))
}

func TestClassifyProcessorErrorUsesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := classifyProcessorError(ctx, errors.Join(errors.New("transport failed"), context.DeadlineExceeded), RequestMetrics{Requests: 1})
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, ocr.ErrorKindOf(err))
	assert.False(t, ocr.IsRetryable(err))
	assert.Equal(t, 1, ocr.MetricsFromError(err).Requests)

	metrics := RequestMetrics{Requests: 2, Retries: 1}
	err = classifyProcessorError(t.Context(), errors.Join(ErrTransientResponse, context.DeadlineExceeded), metrics)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, ocr.ErrorTransient, ocr.ErrorKindOf(err))
	assert.Equal(t, toOCRMetrics(metrics), ocr.MetricsFromError(err))
	assert.True(t, ocr.IsRetryable(err))
}

func TestProcessorCancellationDoesNotWaitForSpoolReservation(t *testing.T) {
	content := testPDF("cancel-release")
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	policy := testPolicy(t, 1024, 10)
	requestStarted := make(chan struct{})
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			close(requestStarted)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})},
	})
	require.NoError(t, err)
	processor, err := NewProcessor(ProcessorConfig{
		Client: client, Policy: policy, CapabilityManifest: syntheticManifest(t, policy, true),
		SpoolDirectory: directory, MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	source, err := ocr.NewSource(
		io.NopCloser(bytes.NewReader(content)), mediaTypePDF, int64(len(content)), hex.EncodeToString(digest[:]),
	)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, processErr := processor.Process(ctx, source)
		done <- processErr
	}()
	<-requestStarted
	releaseLock, err := acquireSpoolReservationLock(t.Context(), directory)
	require.NoError(t, err)
	cancel()

	var processErr error
	select {
	case processErr = <-done:
	case <-time.After(2 * spoolLockRetryInterval):
		releaseLock()
		<-done
		t.Fatal("Process waited for the spool reservation after cancellation")
	}
	releaseLock()
	require.ErrorIs(t, processErr, context.Canceled)
	removed, err := ScavengeSpoolDirectory(directory, time.Now().Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
}

func TestProcessorDOCXRoute(t *testing.T) {
	pdf := testMultipagePDF(2)
	policy := testPolicyWithRenderPDF(t, testRenderPDFPolicy(t, pdf, nil), 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	var requests int
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			_, readErr := io.Copy(io.Discard, request.Body)
			if readErr != nil {
				return nil, readErr
			}
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(ocrResponse(2, len(pdf)))), Request: request,
			}, nil
		})},
	})
	require.NoError(t, err)
	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, spoolDirectory)
	processor, err := NewProcessor(ProcessorConfig{
		Client: client, Policy: policy, CapabilityManifest: manifest,
		SpoolDirectory: spoolDirectory, MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	sourceBytes := loadDOCXFixture(t, "realistic-word.docx")
	sourceDigest := digestBytes(sourceBytes)
	source, err := ocr.NewSource(io.NopCloser(bytes.NewReader(sourceBytes)),
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document", int64(len(sourceBytes)), sourceDigest)
	require.NoError(t, err)

	result, err := processor.Process(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, sourceDigest, result.SourceSHA256)
	assert.Equal(t, digestBytes(pdf), result.UploadSHA256)
	assert.Equal(t, "word", result.Source.Family)
	assert.Equal(t, 2, result.UnitsProcessed)
	assert.Equal(t, 1, requests)
	requireOnlySpoolReservationFile(t, spoolDirectory)
}

func TestProcessorDOCXConversionError(t *testing.T) {
	tests := []struct {
		name      string
		pdf       []byte
		runnerErr error
		wantKind  ocr.ErrorKind
		wantCause error
	}{
		{name: "invalid PDF", pdf: []byte("not a PDF"), wantKind: ocr.ErrorCapabilityChanged, wantCause: ErrCapabilityContract},
		{name: "over limit", pdf: testMultipagePDF(11), wantKind: ocr.ErrorCapabilityChanged, wantCause: ErrCapabilityContract},
		{name: "renderer unavailable", pdf: testMultipagePDF(1), runnerErr: renderpdf.ErrUnavailable, wantKind: ocr.ErrorTransient, wantCause: renderpdf.ErrUnavailable},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			maxUnits := 10
			policy := testPolicyWithRenderPDF(t, testRenderPDFPolicy(t, testCase.pdf, testCase.runnerErr), 1<<20, maxUnits)
			manifest := syntheticManifest(t, policy, true)
			requests := 0
			client, err := NewClient(policy, ClientConfig{
				APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					requests++
					return nil, errors.New("unexpected provider request")
				})},
			})
			require.NoError(t, err)
			directory := filepath.Join(t.TempDir(), "spool")
			makePrivateDirectory(t, directory)
			processor, err := NewProcessor(ProcessorConfig{
				Client: client, Policy: policy, CapabilityManifest: manifest,
				SpoolDirectory: directory, MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
			})
			require.NoError(t, err)
			content := loadDOCXFixture(t, "realistic-word.docx")
			source, err := ocr.NewSource(io.NopCloser(bytes.NewReader(content)),
				"application/vnd.openxmlformats-officedocument.wordprocessingml.document", int64(len(content)), digestBytes(content))
			require.NoError(t, err)
			_, err = processor.Process(t.Context(), source)
			require.Error(t, err)
			assert.Equal(t, testCase.wantKind, ocr.ErrorKindOf(err))
			require.ErrorIs(t, err, testCase.wantCause)
			assert.Zero(t, requests)
		})
	}
}

func newProcessorWithoutRequests(t *testing.T, spoolDirectory string) *Processor {
	t.Helper()
	policy := testPolicy(t, 1024, 10)
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("staging failure unexpectedly reached the provider")
			return nil, errors.New("unexpected provider request")
		})},
	})
	require.NoError(t, err)
	processor, err := NewProcessor(ProcessorConfig{
		Client: client, Policy: policy, CapabilityManifest: syntheticManifest(t, policy, true),
		SpoolDirectory: spoolDirectory, MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	return processor
}
