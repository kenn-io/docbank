package mistral

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareMemoryProcessesBytesWithoutDisk(t *testing.T) {
	original := testPDF("synthetic memory input")
	policy := testPolicy(t, 1024, 10)
	staged := prepareTestDocument(t, policy, original)
	temporary := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, temporary)
	}
	content := bytes.Clone(original)
	prepared, err := PrepareMemory(t.Context(), content, policy, MemoryPrepareOptions{DeclaredMediaType: mediaTypePDF})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	clear(content)
	assert.Equal(t, staged.Format(), prepared.Format())
	assert.Equal(t, staged.Size(), prepared.Size())
	assert.Equal(t, staged.SHA256(), prepared.SHA256())

	var requests atomic.Int32
	var requestBody []byte
	client := clientWithTransport(t, policy, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		var readErr error
		requestBody, readErr = io.ReadAll(request.Body)
		require.NoError(t, readErr)
		return probeHTTPResponse(request, http.StatusOK, testSuccessfulResponse), nil
	}))
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "pdf")
	require.NoError(t, err)
	_, err = client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	var decoded struct {
		Document struct {
			URL string `json:"document_url"`
		} `json:"document"`
	}
	require.NoError(t, json.Unmarshal(requestBody, &decoded))
	uploaded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(decoded.Document.URL, "data:"+mediaTypePDF+";base64,"))
	require.NoError(t, err)
	assert.Equal(t, original, uploaded)
	entries, err := os.ReadDir(temporary)
	require.NoError(t, err)
	assert.Empty(t, entries)

	retained := prepared.contents
	require.Equal(t, original, retained)
	require.NoError(t, prepared.Release())
	assert.Equal(t, make([]byte, len(original)), retained)
	_, err = client.Process(t.Context(), prepared, authorization)
	require.ErrorContains(t, err, "released")
	assert.Equal(t, int32(1), requests.Load())
	require.NoError(t, prepared.Release())
}

func TestPrepareMemoryCountsPPTXSlides(t *testing.T) {
	content := pptxArchive(t, []pptxTestSlide{
		{id: "256", relationshipID: "rId1", target: "slides/slide1.xml"},
		{id: "257", relationshipID: "rId2", target: "slides/slide2.xml"},
	})
	candidate, ok := CandidateFormatByID("pptx")
	require.True(t, ok)
	prepared, err := PrepareMemory(t.Context(), content, testPolicy(t, 1<<20, 10), MemoryPrepareOptions{DeclaredMediaType: candidate.MediaType})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	assert.Equal(t, 2, prepared.localUnits)
}

func TestPrepareMemoryProcessesRenderedDOCX(t *testing.T) {
	pdf := testMultipagePDF(1)
	renderPolicy, runner := testRenderPDFPolicyAndRunner(t, pdf, nil)
	policy := testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 10)
	candidate, ok := CandidateFormatByID("docx")
	require.True(t, ok)
	prepared, err := PrepareMemory(t.Context(), loadDOCXFixture(t, "explicit-breaks.docx"), policy, MemoryPrepareOptions{DeclaredMediaType: candidate.MediaType})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "docx")
	require.NoError(t, err)

	requests := 0
	client := clientWithTransport(t, policy, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		body, readErr := io.ReadAll(request.Body)
		require.NoError(t, readErr)
		assert.Contains(t, string(body), "data:"+mediaTypePDF+";base64,"+base64.StdEncoding.EncodeToString(pdf))
		return probeHTTPResponse(request, http.StatusOK, ocrResponse(1, len(pdf))), nil
	}))
	result, err := client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
	assert.Len(t, runner.calls, 2)
	assert.Equal(t, 1, result.UnitsProcessed)
	require.NotNil(t, result.ConversionReceipt)
	assert.Equal(t, prepared.SHA256(), result.ConversionReceipt.SourceSHA256)
	assert.Equal(t, digestBytes(pdf), result.ConversionReceipt.PDFSHA256)
}

func TestPrepareMemoryReleaseDuringProcess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			content := testPDF("release during upload")
			policy := testPolicy(t, 1024, 10)
			prepared, err := PrepareMemory(t.Context(), content, policy, MemoryPrepareOptions{DeclaredMediaType: mediaTypePDF})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, prepared.Release()) })
			authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "pdf")
			require.NoError(t, err)

			requests := 0
			client := clientWithTransport(t, policy, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				require.NoError(t, prepared.Release())
				body, readErr := io.ReadAll(request.Body)
				require.NoError(t, readErr)
				assert.Contains(t, string(body), "data:"+mediaTypePDF+";base64,"+base64.StdEncoding.EncodeToString(content))
				response := probeHTTPResponse(request, status, testSuccessfulResponse)
				response.Header.Set("Retry-After", "0")
				return response, nil
			}))
			result, err := client.Process(t.Context(), prepared, authorization)
			if status == http.StatusOK {
				require.NoError(t, err)
				assert.Equal(t, 1, result.Metrics.Requests)
			} else {
				require.ErrorContains(t, err, "released")
				assert.Equal(t, 1, MetricsFromError(err).Requests)
			}
			_, err = client.Process(t.Context(), prepared, authorization)
			require.ErrorContains(t, err, "released")
			assert.Equal(t, 1, requests)
		})
	}
}

func TestPrepareMemoryRejectsInvalidSource(t *testing.T) {
	content := testPDF("synthetic policy boundary")
	policy := testPolicy(t, int64(len(content)), 10)
	prepared, err := PrepareMemory(t.Context(), content, policy, MemoryPrepareOptions{DeclaredMediaType: mediaTypePDF})
	require.NoError(t, err)
	require.NoError(t, prepared.Release())

	for _, test := range []struct {
		name     string
		content  []byte
		policy   Policy
		declared string
	}{
		{name: "over limit", content: content, policy: testPolicy(t, int64(len(content))-1, 10), declared: mediaTypePDF},
		{name: "media type mismatch", content: content, policy: policy, declared: "image/png"},
		{name: "malformed PDF", content: []byte("%PDF-1.7\nmalformed\n%%EOF"), policy: policy, declared: mediaTypePDF},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := PrepareMemory(t.Context(), test.content, test.policy, MemoryPrepareOptions{DeclaredMediaType: test.declared})
			require.ErrorIs(t, err, ErrInvalidSource)
			require.Nil(t, prepared)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	prepared, err = PrepareMemory(ctx, content, policy, MemoryPrepareOptions{})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, prepared)
	prepared, err = PrepareMemory(t.Context(), content, Policy{}, MemoryPrepareOptions{})
	require.ErrorContains(t, err, "use NewPolicy")
	require.Nil(t, prepared)
}
