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
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

var _ document.RenditionProvider = (*RenditionClient)(nil)

type renditionSecrets map[string]string

func (secrets renditionSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	return secrets[name], nil
}

func TestNewRenditionProviderAdvertisesOnlyManifestAuthorizedFormats(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")

	client, err := NewRenditionProvider(Profile{
		Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: "mistral-ocr",
	}, renditionSecrets{"mistral-ocr": "synthetic-key"}, http.DefaultClient)
	require.NoError(t, err)
	assert.Equal(t, descriptor, client.Descriptor())

	broader := renditionDescriptor(t, policy, manifest, "pdf", "doc")
	_, err = NewRenditionProvider(Profile{
		Policy: policy, CapabilityManifest: manifest, Descriptor: broader,
		SecretBinding: "mistral-ocr",
	}, renditionSecrets{"mistral-ocr": "synthetic-key"}, http.DefaultClient)
	require.ErrorContains(t, err, "no enforceable upload authority")

	incomplete := manifest
	incomplete.Results = incomplete.Results[:1]
	_, err = NewRenditionProvider(Profile{
		Policy: policy, CapabilityManifest: incomplete, Descriptor: descriptor,
		SecretBinding: "mistral-ocr",
	}, renditionSecrets{"mistral-ocr": "synthetic-key"}, http.DefaultClient)
	require.ErrorContains(t, err, "capability manifest")
}

func TestRenditionClientMapsExactMistralOCRResponse(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	source := testPDF("rendition")
	fixture := renditionFixture(t, descriptor, source)
	var uploaded []byte
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assert.Equal(t, "Bearer synthetic-key", request.Header.Get("Authorization"))
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		var wire struct {
			Document struct {
				URL string `json:"document_url"`
			} `json:"document"`
		}
		require.NoError(t, json.Unmarshal(body, &wire))
		uploaded, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(
			wire.Document.URL, "data:application/pdf;base64,",
		))
		require.NoError(t, err)
		response := fmt.Sprintf(`{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"# Synthetic","header":"Header","footer":"Footer","dimensions":{"dpi":144,"height":792,"width":612}}],"usage_info":{"pages_processed":1,"doc_size_bytes":%d}}`, len(source))
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(response)), Request: request,
		}, nil
	})}
	client, err := NewRenditionProvider(Profile{
		Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: "mistral-ocr", Timeout: time.Second, MaxRetries: 1,
		MaxRetryDelay: time.Millisecond,
	}, renditionSecrets{"mistral-ocr": "synthetic-key"}, httpClient)
	require.NoError(t, err)

	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, source, uploaded)
	assert.Equal(t, document.SourceEvidenceContractV1, result.Evidence.ContractVersion)
	assert.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
	assert.Equal(t, "pdf", result.Evidence.Family)
	assert.Equal(t, document.EvidenceUnitPage, result.Evidence.UnitKind)
	require.Len(t, result.Evidence.Units, 1)
	assert.Equal(t, "Header\n\n# Synthetic\n\nFooter", result.Evidence.Units[0].Text)
	assert.Equal(t, document.SourceEvidenceLocatorV1{
		Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginZero,
		Start: 0, End: 0,
	}, result.Evidence.Units[0].Locator)
	assert.Equal(t, "# Synthetic", string(result.ProviderMarkdown))
	assert.Equal(t, fixture.metadata.SHA256, result.Receipt.SourceSHA256)
	assert.Equal(t, int64(1), result.Receipt.Usage.Requests)
	assert.Equal(t, int64(len(source)), result.Receipt.Usage.InputBytes)
	assert.Equal(t, int64(1), result.Receipt.Usage.Units)
	assert.NotContains(t, fmt.Sprintf("%+v", result.Receipt), "synthetic-key")
}

func TestRenditionClientClassifiesHTTPAndModelFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		body   string
		want   document.RenditionErrorCode
	}{
		{name: "authentication", status: http.StatusUnauthorized, want: document.RenditionErrorAuthentication},
		{name: "unsupported", status: http.StatusUnsupportedMediaType, want: document.RenditionErrorUnsupportedInput},
		{name: "too large", status: http.StatusRequestEntityTooLarge, want: document.RenditionErrorPolicyRejected},
		{name: "capacity", status: http.StatusServiceUnavailable, want: document.RenditionErrorCapacity},
		{name: "rate limited", status: http.StatusTooManyRequests, want: document.RenditionErrorRateLimited},
		{name: "model drift", status: http.StatusOK, body: `{"model":"mistral-ocr-next","pages":[{"index":0,"markdown":"safe"}],"usage_info":{"pages_processed":1}}`, want: document.RenditionErrorPolicyRejected},
		{name: "malformed", status: http.StatusOK, body: `{`, want: document.RenditionErrorMalformedEvidence},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			policy := testPolicy(t, 1<<20, 10)
			manifest := syntheticManifest(t, policy, true)
			descriptor := renditionDescriptor(t, policy, manifest, "pdf")
			fixture := renditionFixture(t, descriptor, testPDF("classified"))
			body := testCase.body
			if body == "" {
				body = `{"private":"provider detail"}`
			}
			client := newRenditionTestClient(t, policy, manifest, descriptor,
				renditionSecrets{"mistral-ocr": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: testCase.status, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(body)), Request: request,
					}, nil
				}))
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			assertRenditionCode(t, err, testCase.want)
			assert.NotContains(t, err.Error(), "provider detail")
		})
	}
}

func TestRenditionClientRechecksIdentityAndDetectsMediaBeforeEgress(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	fixture := renditionFixture(t, descriptor, testPDF("identity"))
	var requests atomic.Int64
	client := newRenditionTestClient(t, policy, manifest, descriptor,
		renditionSecrets{"mistral-ocr": "synthetic-key"}, roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("unexpected request")
		}))

	short, ok := fixture.upload().(*renditionUpload)
	require.True(t, ok)
	short.Reader = bytes.NewReader(fixture.source[:len(fixture.source)-1])
	_, err := client.Render(t.Context(), short, fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorPolicyRejected)

	text := []byte("not a PDF")
	textFixture := renditionFixture(t, descriptor, text)
	_, err = client.Render(t.Context(), textFixture.upload(), textFixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorUnsupportedInput)
	assert.Zero(t, requests.Load())
}

type countingSecrets struct {
	calls atomic.Int64
	err   error
}

func (secrets *countingSecrets) ResolveSecret(context.Context, string) (string, error) {
	secrets.calls.Add(1)
	if secrets.err != nil {
		return "", secrets.err
	}
	return "synthetic-key", nil
}

func TestRenditionClientResolvesSecretPerAttemptAndStopsAtExpiry(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	fixture := renditionFixture(t, descriptor, testPDF("expiry"))
	fixture.authorization.ExpiresAt = time.Now().UTC().Add(20 * time.Millisecond).Format("2006-01-02T15:04:05.000000000Z")
	secrets := &countingSecrets{}
	var requests atomic.Int64
	client := newRenditionTestClient(t, policy, manifest, descriptor, secrets,
		roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": []string{"1"}, "Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{}`)), Request: request,
			}, nil
		}))

	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorPolicyRejected)
	assert.Equal(t, int64(1), requests.Load())
	assert.Equal(t, int64(1), secrets.calls.Load(), "expired retry must not resolve another credential")
}

func TestRenditionClientExpiryCancelsInFlightUpload(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	fixture := renditionFixture(t, descriptor, testPDF("slow-upload"))
	expiresAt := time.Now().UTC().Add(150 * time.Millisecond)
	fixture.authorization.ExpiresAt = expiresAt.Format("2006-01-02T15:04:05.000000000Z")
	requestStarted := make(chan struct{})
	client, err := NewRenditionProvider(Profile{
		Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: "mistral-ocr", Timeout: time.Second, MaxRetries: 1,
		MaxRetryDelay: time.Millisecond,
	}, renditionSecrets{"mistral-ocr": "synthetic-key"}, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(requestStarted)
		buffer := make([]byte, 1)
		for {
			if _, readErr := request.Body.Read(buffer); readErr != nil {
				return nil, readErr
			}
			timer := time.NewTimer(5 * time.Millisecond)
			select {
			case <-request.Context().Done():
				timer.Stop()
				return nil, request.Context().Err()
			case <-timer.C:
			}
		}
	})})
	require.NoError(t, err)
	callerCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := time.Now()
	_, err = client.Render(callerCtx, fixture.upload(), fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorPolicyRejected)
	require.ErrorContains(t, err, "authorization expired")
	assert.Less(t, time.Since(started), 600*time.Millisecond)
	select {
	case <-requestStarted:
	default:
		t.Fatal("slow upload never reached the transport")
	}
}

func TestRenditionClientRejectsPDFAboveCompleteUnitLimitBeforeEgress(t *testing.T) {
	policy := testPolicy(t, 1<<20, MinUnits)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	fixture := renditionFixture(t, descriptor, testMultipagePDF(MinUnits+1))
	var requests atomic.Int64
	client := newRenditionTestClient(t, policy, manifest, descriptor,
		renditionSecrets{"mistral-ocr": "synthetic-key"}, roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("unexpected provider request")
		}))

	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorPolicyRejected)
	require.ErrorContains(t, err, "unit limit")
	assert.Zero(t, requests.Load())
}

func TestRenditionClientRejectsIncompletePDFResponse(t *testing.T) {
	policy := testPolicy(t, 1<<20, MinUnits)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	fixture := renditionFixture(t, descriptor, testMultipagePDF(2))
	client := newRenditionTestClient(t, policy, manifest, descriptor,
		renditionSecrets{"mistral-ocr": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"first"}],"usage_info":{"pages_processed":1}}`,
				)), Request: request,
			}, nil
		}))

	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorPolicyRejected)
	require.ErrorContains(t, err, "page count changed")
}

func TestRenditionClientBoundsResponseAndKeepsCredentialErrorsSafe(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	policy.values.MaxResponseBytes = 64
	var err error
	policy.digest, err = policyValuesDigest(policy.values)
	require.NoError(t, err)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	fixture := renditionFixture(t, descriptor, testPDF("bounded"))
	client := newRenditionTestClient(t, policy, manifest, descriptor,
		renditionSecrets{"mistral-ocr": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 65))), Request: request,
			}, nil
		}))
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorMalformedEvidence)

	privateCause := errors.New("secret provider token=private-value")
	secrets := &countingSecrets{err: privateCause}
	client = newRenditionTestClient(t, policy, manifest, descriptor, secrets,
		roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("credential failure must precede egress")
			return nil, errors.New("unexpected egress")
		}))
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorAuthentication)
	assert.NotContains(t, err.Error(), "private-value")
}

func TestRenditionClientResolvesNamedSecretForEveryEgress(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	fixture := renditionFixture(t, descriptor, testPDF("retry-secret"))
	secrets := &countingSecrets{}
	var requests atomic.Int64
	client := newRenditionTestClient(t, policy, manifest, descriptor, secrets,
		roundTripFunc(func(request *http.Request) (*http.Response, error) {
			attempt := requests.Add(1)
			if attempt == 1 {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Retry-After": []string{"0"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"safe"}],"usage_info":{"pages_processed":1}}`,
				)), Request: request,
			}, nil
		}))
	result, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, int64(2), requests.Load())
	assert.Equal(t, int64(2), secrets.calls.Load())
	assert.Equal(t, int64(1), result.Receipt.Usage.Retries)
}

func TestRenditionClientRefusesRedirects(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	fixture := renditionFixture(t, descriptor, testPDF("redirect"))
	var targetRequests atomic.Int64
	target := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetRequests.Add(1) })
	targetServer := newTLSServer(t, target)
	redirectServer := newTLSServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, targetServer.URL, http.StatusTemporaryRedirect)
	}))
	client := renditionServerClient(t, policy, manifest, descriptor, redirectServer)

	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorMalformedEvidence)
	assert.Zero(t, targetRequests.Load())
}

func TestMistralEvidenceMapsNaturalFamilies(t *testing.T) {
	for _, testCase := range []struct {
		name, family, sourceKind string
		wantKind                 document.EvidenceUnitKind
		wantLocator              document.EvidenceLocatorKind
	}{
		{name: "pdf", family: "pdf", sourceKind: "page", wantKind: document.EvidenceUnitPage, wantLocator: document.EvidenceLocatorPage},
		{name: "word", family: "word", sourceKind: "page", wantKind: document.EvidenceUnitPage, wantLocator: document.EvidenceLocatorPage},
		{name: "presentation", family: "presentation", sourceKind: "slide", wantKind: document.EvidenceUnitSlide, wantLocator: document.EvidenceLocatorSlide},
		{name: "spreadsheet sheet", family: "spreadsheet", sourceKind: "sheet", wantKind: document.EvidenceUnitSheet, wantLocator: document.EvidenceLocatorSheet},
		{name: "spreadsheet record", family: "spreadsheet", sourceKind: "record", wantKind: document.EvidenceUnitRecord, wantLocator: document.EvidenceLocatorRecord},
		{name: "ebook", family: "ebook", sourceKind: "spine", wantKind: document.EvidenceUnitSpine, wantLocator: document.EvidenceLocatorSpine},
		{name: "structured", family: "structured", sourceKind: "record", wantKind: document.EvidenceUnitRecord, wantLocator: document.EvidenceLocatorRecord},
		{name: "text", family: "text", sourceKind: "section", wantKind: document.EvidenceUnitSection, wantLocator: document.EvidenceLocatorSection},
		{name: "source", family: "source", sourceKind: "section", wantKind: document.EvidenceUnitSection, wantLocator: document.EvidenceLocatorSection},
		{name: "mail", family: "mail", sourceKind: "message", wantKind: document.EvidenceUnitMessage, wantLocator: document.EvidenceLocatorMessage},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			evidence, markdown, err := mistralEvidence(document.SourceDocument{
				Family: testCase.family, UnitKind: testCase.sourceKind,
				Units: []document.SourceUnit{{Index: 0, Markdown: "synthetic"}},
			})
			require.NoError(t, err)
			assert.Equal(t, testCase.wantKind, evidence.UnitKind)
			assert.Equal(t, testCase.wantLocator, evidence.Units[0].Locator.Kind)
			if testCase.wantLocator == document.EvidenceLocatorSheet {
				assert.Equal(t, "mistral-unit-0", evidence.Units[0].Locator.Name)
			}
			assert.Equal(t, "synthetic", string(markdown))
		})
	}
}

func newRenditionTestClient(
	t *testing.T, policy Policy, manifest CapabilityManifest, descriptor document.RenditionDescriptor,
	secrets SecretResolver, transport http.RoundTripper,
) *RenditionClient {
	t.Helper()
	drainingTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		_, err := io.Copy(io.Discard, request.Body)
		require.NoError(t, err)
		return transport.RoundTrip(request)
	})
	client, err := NewRenditionProvider(Profile{
		Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: "mistral-ocr", Timeout: time.Second, MaxRetries: 1,
		MaxRetryDelay: 50 * time.Millisecond,
	}, secrets, &http.Client{Transport: drainingTransport})
	require.NoError(t, err)
	return client
}

func newTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return server
}

func renditionServerClient(
	t *testing.T, policy Policy, manifest CapabilityManifest,
	descriptor document.RenditionDescriptor, server *httptest.Server,
) *RenditionClient {
	t.Helper()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	base := server.Client()
	transport := base.Transport
	client, err := NewRenditionProvider(Profile{
		Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: "mistral-ocr", Timeout: time.Second, MaxRetries: 1,
		MaxRetryDelay: 50 * time.Millisecond,
	}, renditionSecrets{"mistral-ocr": "synthetic-key"}, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
		return transport.RoundTrip(clone)
	})})
	require.NoError(t, err)
	return client
}

func assertRenditionCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerError, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok, "%T: %v", err, err)
	assert.Equal(t, want, providerError.Code())
}

type renditionUpload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
}

func (upload *renditionUpload) Close() error { return nil }

func (upload *renditionUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

type renditionTestFixture struct {
	metadata      document.AuthorizedUploadMetadata
	authorization document.RenditionAuthorization
	source        []byte
}

func renditionFixture(
	t *testing.T, descriptor document.RenditionDescriptor, source []byte,
) renditionTestFixture {
	t.Helper()
	digest := sha256.Sum256(source)
	metadata := document.AuthorizedUploadMetadata{
		Filename: "document.pdf", MediaFamily: "pdf", MediaType: "application/pdf",
		ByteLength: int64(len(source)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("2", 64),
		ProviderMetadataChecksum: strings.Repeat("3", 64),
		InputKind:                document.RenditionInputOriginalFile,
	}
	started := time.Now().UTC().Add(-time.Minute)
	return renditionTestFixture{metadata: metadata, source: source, authorization: document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64), SourceSHA256: metadata.SHA256,
		SourceBytes: metadata.ByteLength, CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum, MediaFamily: metadata.MediaFamily,
		MediaType: metadata.MediaType, InputKind: metadata.InputKind,
		MaxProviderMarkdownBytes: 4096, MaxTotalResultBytes: 32768,
		AuthorizedAt: started.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:    started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}}
}

func (fixture renditionTestFixture) upload() document.AuthorizedUpload {
	return &renditionUpload{Reader: bytes.NewReader(fixture.source), metadata: fixture.metadata}
}

func renditionDescriptor(
	t *testing.T, policy Policy, manifest CapabilityManifest, formatIDs ...string,
) document.RenditionDescriptor {
	t.Helper()
	formats := make([]document.RenditionFormatCapability, 0, len(formatIDs))
	for _, formatID := range formatIDs {
		candidate, ok := CandidateFormatByID(formatID)
		require.True(t, ok)
		formats = append(formats, document.RenditionFormatCapability{
			MediaFamily: candidate.Family, MediaType: candidate.MediaType,
			InputKind: document.RenditionInputOriginalFile,
		})
	}
	fingerprint, err := policy.Fingerprint(manifest)
	require.NoError(t, err)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "mistral.ocr-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: fingerprint, TrustBoundary: document.RenditionTrustHostedProvider,
		SupportedFormats: formats, ReturnsMarkdown: true, ReturnsStructured: true,
	})
	require.NoError(t, err)
	require.NotContains(t, descriptor.Fingerprint, " ")
	return descriptor
}

func testMultipagePDF(pages int) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
	}
	kids := make([]string, pages)
	for index := range pages {
		kids[index] = fmt.Sprintf("%d 0 R", index+3)
	}
	objects = append(objects, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))
	for index := range pages {
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R >>",
			pages+index+3,
		))
	}
	for index := range pages {
		objects = append(objects, fmt.Sprintf("<< /Length 0 >>\nstream\n\nendstream %% %d", index))
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
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
	_, _ = fmt.Fprintf(&output,
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}
