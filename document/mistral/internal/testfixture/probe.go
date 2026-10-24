package testfixture

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/formatdetect"
)

// ProbeTransport simulates OCR responses for the synthetic probe fixtures.
type ProbeTransport struct {
	T             testing.TB
	Model         string
	ExtractHeader bool
	ExtractFooter bool
	BoundMode     string
	FailureMode   string
	Calls         int
}

// RoundTrip checks probe requests and returns synthetic OCR responses.
func (transport *ProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.T.Helper()
	call := transport.Calls
	transport.Calls++
	require.Equal(transport.T, "Bearer synthetic-key", request.Header.Get("Authorization"), "call %d", call)
	body, err := io.ReadAll(request.Body)
	require.NoError(transport.T, err)
	assert.NotContains(transport.T, string(body), "image_limit")
	var input struct {
		Pages         string `json:"pages"`
		ExtractHeader bool   `json:"extract_header"`
		ExtractFooter bool   `json:"extract_footer"`
		Document      struct {
			URL string `json:"document_url"`
		} `json:"document"`
	}
	require.NoError(transport.T, json.Unmarshal(body, &input))
	assert.Equal(transport.T, transport.ExtractHeader, input.ExtractHeader)
	assert.Equal(transport.T, transport.ExtractFooter, input.ExtractFooter)

	mediaType := strings.TrimPrefix(strings.SplitN(input.Document.URL, ";base64,", 2)[0], "data:")
	candidate, found := formatdetect.CandidateFormatByMediaType(mediaType)
	require.True(transport.T, found)
	if candidate.ID == "pdf" && input.Pages == "0-0" && transport.BoundMode == "request_failed" {
		return ProbeHTTPResponse(request, http.StatusServiceUnavailable, `{}`), nil
	}
	if candidate.ID == "pdf" {
		switch transport.FailureMode {
		case "invalid_model":
			return ProbeHTTPResponse(request, http.StatusBadRequest, `{"object":"error","message":"Invalid model: mistral-ocr-4-0","type":"invalid_model","code":"1500"}`), nil
		case "provider_4xx":
			return ProbeHTTPResponse(request, http.StatusBadRequest, `{}`), nil
		case "transient_exhausted":
			return ProbeHTTPResponse(request, http.StatusServiceUnavailable, `{}`), nil
		}
	}

	units := 1
	if candidate.ID == "pdf" {
		assert.NotEmpty(transport.T, input.Pages)
		if input.Pages != "0-0" || transport.BoundMode == "units_mismatch" {
			units = 2
		}
	} else {
		assert.Empty(transport.T, input.Pages)
	}
	sentinel, err := formatdetect.ProbeFixtureSentinel(candidate.ID)
	require.NoError(transport.T, err)
	pages := make([]map[string]any, units)
	for index := range pages {
		markdown := ""
		if index == 0 {
			switch transport.FailureMode {
			case "empty_output":
			case "sentinel_missing":
				markdown = "synthetic text without the sentinel"
			default:
				markdown = sentinel
			}
		}
		pages[index] = map[string]any{"index": index, "markdown": markdown}
	}
	encoded, err := json.Marshal(map[string]any{
		"model": transport.Model, "pages": pages,
		"usage_info": map[string]int{"pages_processed": units},
	})
	require.NoError(transport.T, err)
	return ProbeHTTPResponse(request, http.StatusOK, string(encoded)), nil
}

// ProbeHTTPResponse builds a synthetic JSON response for a probe request.
func ProbeHTTPResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}
}
