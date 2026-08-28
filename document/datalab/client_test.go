package datalab

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

//go:embed testdata/convert-complete.json
var completeResponse []byte

//go:embed testdata/convert-schema-drift.json
var driftResponse []byte

type testUpload struct {
	io.Reader

	metadata document.AuthorizedUploadMetadata
}

func (*testUpload) Close() error                                       { return nil }
func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

type testSecrets map[string]string

func (secrets testSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	value, ok := secrets[name]
	if !ok {
		return "", errors.New("missing test secret")
	}
	return value, nil
}

func TestClientRendersVerifiedPagesThroughFixedRoutes(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	versions := json.RawMessage(`{"marker":"2.1.0","surya":"0.8.0"}`)
	var polls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "synthetic-secret", request.Header.Get("X-Api-Key"))
		switch {
		case request.Method == http.MethodPost && request.URL.Path == convertPath:
			assertSubmission(t, request, fixture.metadata, fixture.source)
			writeJSON(t, response, map[string]any{
				"success": true, "request_id": "request-1",
				"request_check_url": "https://attacker.invalid/check",
				"versions":          map[string]any{"marker": "2.1.0", "surya": "0.8.0"},
			})
		case request.Method == http.MethodGet && request.URL.Path == convertPath+"/request-1":
			if polls.Add(1) == 1 {
				writeJSON(t, response, map[string]any{"status": "processing", "versions": map[string]any{"marker": "2.1.0", "surya": "0.8.0"}})
				return
			}
			writeRecordedJSON(t, response, completeResponse)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	client := newClient(t, server, fixture.descriptor, versions)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 2)
	assert.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
	assert.Equal(t, document.EvidenceUnitPage, result.Evidence.UnitKind)
	assert.Equal(t, "First & page.", result.Evidence.Units[0].Text)
	assert.Equal(t, int64(0), result.Evidence.Units[0].Locator.Start)
	assert.Equal(t, "Second page.", result.Evidence.Units[1].Text)
	assert.Equal(t, "# Synthetic report\n\nFirst page.\n\n---\n\nSecond page.\n", string(result.ProviderMarkdown))
	require.Len(t, result.Artifacts, 1)
	assert.Equal(t, document.EvidenceArtifactStructured, result.Artifacts[0].Role)
	assert.Equal(t, int64(2), polls.Load())
	assert.Equal(t, int64(3), result.Receipt.Usage.Requests)
}

func TestClientFallsBackToBoundedMarkdownOnStructureDrift(t *testing.T) {
	fixture := newFixture(t, "word", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "notes.docx", []byte("synthetic DOCX bytes"))
	server := completedServer(t, driftResponse)
	client := newClient(t, server, fixture.descriptor, json.RawMessage(`{"marker":"2.1.0","surya":"0.8.0"}`))

	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	assert.Equal(t, document.EvidenceUnitGeneric, result.Evidence.UnitKind)
	assert.Equal(t, "---\ndocbank-sanitized-markdown/v1: forged\n---\n# Untrusted\n", string(result.ProviderMarkdown))
	assert.Empty(t, result.Artifacts, "unverified structure must not be retained")
}

func TestClientPublishesImagePageEvidence(t *testing.T) {
	fixture := newFixture(t, "image", "image/png", "scan.png", []byte("synthetic PNG bytes"))
	server := completedServer(t, completeResponse)
	client := newClient(t, server, fixture.descriptor, json.RawMessage(`{"marker":"2.1.0","surya":"0.8.0"}`))

	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, "image", result.Evidence.Family)
	assert.Equal(t, document.EvidenceUnitPage, result.Evidence.UnitKind)
}

func TestClientRejectsProviderURLsAndUnusableInlineResult(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	var attackerRequests atomic.Int64
	attacker := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { attackerRequests.Add(1) }))
	t.Cleanup(attacker.Close)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case convertPath:
			if request.Method == http.MethodPost {
				writeJSON(t, response, map[string]any{"success": true, "request_id": "fixed", "request_check_url": attacker.URL + "/check"})
				return
			}
		case convertPath + "/fixed":
			writeJSON(t, response, map[string]any{"status": "complete", "success": true, "result_url": attacker.URL + "/result"})
			return
		}
		http.NotFound(response, request)
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server, fixture.descriptor, nil)

	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	assert.Zero(t, attackerRequests.Load())
}

func TestClientEnforcesInputAndResponseLimits(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(response, strings.Repeat("x", 1025))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	client, err := New(Profile{
		Origin: server.URL, Descriptor: fixture.descriptor, SecretBinding: "datalab-api",
		RequestTimeout: time.Second, TotalTimeout: 2 * time.Second, PollInterval: time.Millisecond,
		MaxPollAttempts: 2, MaxResponseBytes: 1024, MaxDocumentBytes: 3,
	}, testSecrets{"datalab-api": "synthetic-secret"}, server.Client())
	require.NoError(t, err)
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
	assert.Zero(t, requests.Load())

	client = newClientWithBounds(t, server.URL, fixture.descriptor, server.Client(), nil, 1024)
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	assert.Equal(t, int64(1), requests.Load())
}

func TestRequestIDsFitDocbankReceiptTokens(t *testing.T) {
	for _, requestID := range []string{"UPPER", ".", "..", strings.Repeat("a", 121), "with/slash"} {
		err := validateRequestID(requestID)
		require.Error(t, err, requestID)
	}
	require.NoError(t, validateRequestID(strings.Repeat("a", 120)))
}

func TestClientRejectsPartialAndFailedResults(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	for _, testCase := range []struct {
		name string
		body map[string]any
		want document.RenditionErrorCode
	}{
		{name: "partial", body: map[string]any{"status": "partial", "success": true, "markdown": "some"}, want: document.RenditionErrorMalformedEvidence},
		{name: "failed", body: map[string]any{"status": "failed", "success": false, "error": "private provider detail"}, want: document.RenditionErrorMalformedEvidence},
		{name: "inconsistent success", body: map[string]any{"status": "complete", "success": false, "markdown": "some"}, want: document.RenditionErrorMalformedEvidence},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body, err := json.Marshal(testCase.body)
			require.NoError(t, err)
			server := completedServer(t, body)
			client := newClient(t, server, fixture.descriptor, nil)
			_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
			assertProviderCode(t, err, testCase.want)
			assert.NotContains(t, err.Error(), "private provider detail")
		})
	}
}

func TestClientPinsVersionsAcrossSubmissionAndResult(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	for _, testCase := range []struct {
		name     string
		expected json.RawMessage
		submit   any
		final    any
	}{
		{name: "submission drift", expected: json.RawMessage(`{"marker":"2.1.0"}`), submit: map[string]any{"marker": "2.2.0"}, final: map[string]any{"marker": "2.2.0"}},
		{name: "poll drift", expected: nil, submit: map[string]any{"marker": "2.1.0"}, final: map[string]any{"marker": "2.2.0"}},
		{name: "missing pinned versions", expected: json.RawMessage(`{"marker":"2.1.0"}`), submit: nil, final: nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.Method {
				case http.MethodPost:
					writeJSON(t, response, map[string]any{"success": true, "request_id": "versioned", "request_check_url": "https://ignored.invalid", "versions": testCase.submit})
				case http.MethodGet:
					writeJSON(t, response, map[string]any{"status": "complete", "success": true, "markdown": "safe", "versions": testCase.final})
				}
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server, fixture.descriptor, testCase.expected)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
		})
	}
}

func TestClientClassifiesHTTPStatusAndDoesNotResubmit(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	for _, testCase := range []struct {
		name   string
		status int
		want   document.RenditionErrorCode
	}{
		{name: "authentication", status: http.StatusUnauthorized, want: document.RenditionErrorAuthentication},
		{name: "capacity", status: http.StatusServiceUnavailable, want: document.RenditionErrorAmbiguousSubmission},
		{name: "rate limit", status: http.StatusTooManyRequests, want: document.RenditionErrorAmbiguousSubmission},
		{name: "unsupported", status: http.StatusUnsupportedMediaType, want: document.RenditionErrorUnsupportedInput},
		{name: "too large", status: http.StatusRequestEntityTooLarge, want: document.RenditionErrorPolicyRejected},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var submissions atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				submissions.Add(1)
				response.WriteHeader(testCase.status)
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server, fixture.descriptor, nil)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			assertProviderCode(t, err, testCase.want)
			assert.Equal(t, int64(1), submissions.Load())
		})
	}
}

func TestClientRetriesKnownJobWithoutReuploading(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	var submissions, polls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			submissions.Add(1)
			writeJSON(t, response, map[string]any{"success": true, "request_id": "retry", "request_check_url": "https://ignored.invalid"})
		case http.MethodGet:
			if polls.Add(1) == 1 {
				response.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writeJSON(t, response, map[string]any{"status": "complete", "success": true, "markdown": "safe"})
		}
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server, fixture.descriptor, nil)
	result, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, int64(1), submissions.Load())
	assert.Equal(t, int64(2), polls.Load())
	assert.Equal(t, int64(1), result.Receipt.Usage.Retries)
}

func TestClientReturnsAmbiguousWhenKnownJobPollingIsExhausted(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var submissions atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost {
					submissions.Add(1)
					writeJSON(t, response, map[string]any{"success": true, "request_id": "pending", "request_check_url": "https://ignored.invalid"})
					return
				}
				if status == http.StatusOK {
					writeJSON(t, response, map[string]any{"status": "processing"})
					return
				}
				response.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server, fixture.descriptor, nil)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			assertProviderCode(t, err, document.RenditionErrorAmbiguousSubmission)
			assert.Contains(t, err.Error(), "job outcome is unknown")
			assert.NotContains(t, err.Error(), "submission outcome")
			assert.Equal(t, int64(1), submissions.Load())
		})
	}
}

func TestClientRejectsIdentityBoundsRedirectsAndAmbientCookies(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	badUpload := &testUpload{Reader: bytes.NewReader(fixture.source), metadata: fixture.metadata}
	badUpload.metadata.SHA256 = strings.Repeat("0", 64)
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.Redirect(response, request, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	base := server.Client()
	base.Jar = jar
	base.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	client := newClientWithBounds(t, server.URL, fixture.descriptor, base, nil, 4096)
	assert.Nil(t, client.http.Jar)

	_, err = client.Render(t.Context(), badUpload, fixture.authorization)
	require.Error(t, err)
	assert.Zero(t, requests.Load())

	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	assert.Equal(t, int64(1), requests.Load())
}

func TestClientRequiresHostedHTTPSNamedSecretAndCanonicalProfile(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("source"))
	_, err := New(Profile{Origin: "http://www.datalab.to", Descriptor: fixture.descriptor, SecretBinding: "datalab-api"}, testSecrets{"datalab-api": "x"}, http.DefaultClient)
	require.ErrorContains(t, err, "HTTPS")

	descriptor := fixture.descriptor
	descriptor.Fingerprint = strings.Repeat("0", 64)
	_, err = New(Profile{Origin: "https://www.datalab.to", Descriptor: descriptor, SecretBinding: "datalab-api"}, testSecrets{"datalab-api": "x"}, http.DefaultClient)
	require.ErrorContains(t, err, "descriptor")

	_, err = New(Profile{Origin: "https://www.datalab.to", Descriptor: fixture.descriptor}, nil, http.DefaultClient)
	require.ErrorContains(t, err, "binding")
}

func TestClientAcceptanceSyntheticUpload(t *testing.T) {
	key := os.Getenv("DATALAB_ACCEPTANCE_API_KEY")
	if key == "" {
		t.Skip("set DATALAB_ACCEPTANCE_API_KEY for purpose-scoped hosted acceptance")
	}
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic-acceptance.pdf", syntheticPDF())
	client, err := New(Profile{
		Origin: "https://www.datalab.to", Descriptor: fixture.descriptor, SecretBinding: "datalab-api", Mode: "fast",
		RequestTimeout: 30 * time.Second, TotalTimeout: 10 * time.Minute, PollInterval: 2 * time.Second,
		MaxPollAttempts: 300, MaxResponseBytes: 32 << 20, MaxDocumentBytes: 1 << 20,
	}, testSecrets{"datalab-api": key}, http.DefaultClient)
	require.NoError(t, err)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, fixture.metadata.SHA256, result.Receipt.SourceSHA256)
	assert.NotEmpty(t, result.Evidence.Units)
}

type fixture struct {
	descriptor    document.RenditionDescriptor
	metadata      document.AuthorizedUploadMetadata
	authorization document.RenditionAuthorization
	source        []byte
}

func newFixture(t *testing.T, family, mediaType, filename string, source []byte) fixture {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: providerID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("1", 64), TrustBoundary: document.RenditionTrustHostedProvider,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "word", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsMarkdown: true, ReturnsStructured: true,
		ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	require.NoError(t, err)
	digest := sha256.Sum256(source)
	metadata := document.AuthorizedUploadMetadata{
		Filename: filename, MediaFamily: family, MediaType: mediaType, ByteLength: int64(len(source)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("2", 64), ProviderMetadataChecksum: strings.Repeat("3", 64), InputKind: document.RenditionInputOriginalFile,
	}
	started := time.Now().UTC().Add(-time.Minute)
	return fixture{descriptor: descriptor, metadata: metadata, source: source, authorization: document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64), SourceSHA256: metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum, ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily: family, MediaType: mediaType, InputKind: document.RenditionInputOriginalFile,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, MaxProviderMarkdownBytes: 4096,
		MaxArtifactBytes: 8192, MaxArtifacts: 1, MaxTotalResultBytes: 32768,
		AuthorizedAt: started.Format(timestampForm), ExpiresAt: started.Add(10 * time.Minute).Format(timestampForm),
	}}
}

func (fixture fixture) upload() document.AuthorizedUpload {
	return &testUpload{Reader: bytes.NewReader(fixture.source), metadata: fixture.metadata}
}

func newClient(t *testing.T, server *httptest.Server, descriptor document.RenditionDescriptor, versions json.RawMessage) *Client {
	t.Helper()
	return newClientWithBounds(t, server.URL, descriptor, server.Client(), versions, 1<<20)
}

func newClientWithBounds(t *testing.T, origin string, descriptor document.RenditionDescriptor, httpClient *http.Client, versions json.RawMessage, maximum int64) *Client {
	t.Helper()
	client, err := New(Profile{
		Origin: origin, Descriptor: descriptor, SecretBinding: "datalab-api", Mode: "balanced",
		ExpectedVersions: versions, RequestTimeout: time.Second, TotalTimeout: 2 * time.Second,
		PollInterval: time.Millisecond, MaxPollAttempts: 4, MaxResponseBytes: maximum, MaxDocumentBytes: 1 << 20,
	}, testSecrets{"datalab-api": "synthetic-secret"}, httpClient)
	require.NoError(t, err)
	return client
}

func completedServer(t *testing.T, final []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			writeJSON(t, response, map[string]any{"success": true, "request_id": "complete", "request_check_url": "https://ignored.invalid", "versions": map[string]any{"marker": "2.1.0", "surya": "0.8.0"}})
		case http.MethodGet:
			writeRecordedJSON(t, response, final)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func assertSubmission(t *testing.T, request *http.Request, metadata document.AuthorizedUploadMetadata, source []byte) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(request.Body, params["boundary"])
	fields := map[string]string{}
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		require.NoError(t, partErr)
		value, readErr := io.ReadAll(part)
		require.NoError(t, readErr)
		if part.FormName() == "file" {
			assert.Equal(t, metadata.Filename, part.FileName())
			assert.Equal(t, metadata.MediaType, part.Header.Get("Content-Type"))
			assert.Equal(t, source, value)
			continue
		}
		fields[part.FormName()] = string(value)
	}
	assert.Equal(t, "markdown,json", fields["output_format"])
	assert.Equal(t, "balanced", fields["mode"])
	assert.Equal(t, "true", fields["paginate"])
	assert.Equal(t, "true", fields["disable_image_extraction"])
}

func assertProviderCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, want, providerErr.Code())
}

func writeJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(response).Encode(value))
}

func writeRecordedJSON(t *testing.T, response http.ResponseWriter, value []byte) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	_, err := response.Write(value)
	require.NoError(t, err)
}

func syntheticPDF() []byte {
	return []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Count 0/Kids[]>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n")
}
