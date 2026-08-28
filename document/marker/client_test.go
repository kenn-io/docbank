package marker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

type testUpload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
}

func (*testUpload) Close() error                                       { return nil }
func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

type testSecrets map[string]string

func (secrets testSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	value, ok := secrets[name]
	if !ok {
		return "", errors.New("missing secret")
	}
	return value, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

type callbackReadCloser struct {
	reader io.Reader
	once   sync.Once
	before func()
}

func (body *callbackReadCloser) Read(value []byte) (int, error) {
	body.once.Do(body.before)
	return body.reader.Read(value)
}

func (*callbackReadCloser) Close() error { return nil }

type errorReadCloser struct {
	once   sync.Once
	before func()
	err    error
}

func (body *errorReadCloser) Read([]byte) (int, error) {
	body.once.Do(body.before)
	return 0, body.err
}

func (*errorReadCloser) Close() error { return nil }

func TestClientUploadsExactBytesToFixedRouteAndMapsProvenPages(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(2))
	var calls atomic.Int64
	client := newClient(t, fixture.profile, testSecrets{"marker-front": "synthetic-secret"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "https://marker.internal/marker/upload", request.URL.String())
		assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization"))
		assertMultipart(t, request, fixture.metadata, fixture.source)
		return jsonResponse(request, http.StatusOK, `{"format":"markdown","output":"{0}------------------------------------------------\n\n# First\n\n{1}------------------------------------------------\n\nSecond","images":{},"metadata":{"table_of_contents":[],"page_stats":[{"page_id":0,"text_extraction_method":"pdftext","block_counts":[],"block_metadata":{}},{"page_id":1,"text_extraction_method":"pdftext","block_counts":[],"block_metadata":{}}]},"success":true}`), nil
	}))

	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, int64(1), calls.Load())
	assert.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
	assert.Equal(t, document.EvidenceUnitPage, result.Evidence.UnitKind)
	require.Len(t, result.Evidence.Units, 2)
	assert.Equal(t, "# First", result.Evidence.Units[0].Text)
	assert.Equal(t, int64(1), result.Evidence.Units[1].Locator.Start)
	assert.Equal(t, fixture.metadata.SHA256, result.Receipt.SourceSHA256)
}

func TestDescriptorCoversMarkerConverterFamilies(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
	want := []document.RenditionFormatCapability{
		{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/webp", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/gif", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "word", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "ebook", MediaType: "application/epub+zip", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/html", InputKind: document.RenditionInputOriginalFile},
	}
	assert.ElementsMatch(t, want, fixture.descriptor.SupportedFormats)

	changed := fixture.profile
	changed.RuntimeFingerprint = strings.Repeat("9", 64)
	fingerprint, err := PolicyFingerprint(changed)
	require.NoError(t, err)
	assert.NotEqual(t, fixture.descriptor.PolicyFingerprint, fingerprint)
	changed = fixture.profile
	changed.SecretBinding = "other"
	fingerprint, err = PolicyFingerprint(changed)
	require.NoError(t, err)
	assert.NotEqual(t, fixture.descriptor.PolicyFingerprint, fingerprint)
}

func TestClientRequiresPinnedProfileAndOptionalCredentialPair(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
	changed := fixture.profile
	changed.RuntimeFingerprint = strings.Repeat("9", 64)
	_, err := New(changed, testSecrets{"marker-front": "secret"}, staticTransport(http.StatusOK, `{}`))
	require.ErrorContains(t, err, "policy fingerprint")

	withoutCredential := fixture.profile
	withoutCredential.SecretBinding = ""
	withoutCredential.Descriptor = descriptorFor(t, withoutCredential)
	_, err = New(withoutCredential, nil, staticTransport(http.StatusOK, `{}`))
	require.NoError(t, err)
	_, err = New(withoutCredential, testSecrets{}, staticTransport(http.StatusOK, `{}`))
	require.ErrorContains(t, err, "named binding")
}

func TestClientDegradesTransformedFamiliesWithoutInventingUnits(t *testing.T) {
	for _, input := range []struct{ family, mediaType, filename string }{
		{"word", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "notes.docx"},
		{"spreadsheet", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "book.xlsx"},
		{"presentation", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "deck.pptx"},
		{"ebook", "application/epub+zip", "book.epub"},
		{"text", "text/html", "page.html"},
		{"text", "text/html", "page.htm"},
	} {
		t.Run(input.family, func(t *testing.T) {
			fixture := newFixture(t, input.family, input.mediaType, input.filename, []byte("synthetic source"))
			client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, staticTransport(http.StatusOK,
				`{"format":"markdown","output":"{0}------------------------------------------------\n\nReadable","images":{},"metadata":{"table_of_contents":[],"page_stats":[{"page_id":0,"text_extraction_method":"pdftext","block_counts":[],"block_metadata":{}}]},"success":true}`))
			result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
			require.NoError(t, err)
			assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
			assert.Equal(t, document.EvidenceUnitGeneric, result.Evidence.UnitKind)
		})
	}
}

func TestClientRejectsUnsupportedFilenameIdentityAndBoundsBeforeEgress(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
	var calls atomic.Int64
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("unexpected") })
	client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, transport)

	wrong := fixture
	wrong.metadata.Filename = "report.txt"
	wrong.authorization.ProviderMetadataChecksum = wrong.metadata.ProviderMetadataChecksum
	_, err := client.Render(t.Context(), wrong.upload(), wrong.authorization)
	assertProviderCode(t, err, document.RenditionErrorUnsupportedInput)

	wrong = fixture
	wrong.metadata.SHA256 = strings.Repeat("0", 64)
	wrong.authorization.SourceSHA256 = wrong.metadata.SHA256
	_, err = client.Render(t.Context(), wrong.upload(), wrong.authorization)
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)

	tooLarge := fixture.profile
	tooLarge.MaxDocumentBytes = 2
	tooLarge.MaxRequestBytes = 1024
	tooLarge.Descriptor = descriptorFor(t, tooLarge)
	client = newClient(t, tooLarge, testSecrets{"marker-front": "secret"}, transport)
	tooLargeFixture := fixture.withDescriptor(tooLarge.Descriptor)
	_, err = client.Render(t.Context(), tooLargeFixture.upload(), tooLargeFixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
	assert.Zero(t, calls.Load())
}

func TestClientRejectsRedirectMalformedPartialAndOversizedResults(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
	for _, testCase := range []struct {
		name   string
		status int
		body   string
		want   document.RenditionErrorCode
	}{
		{"redirect", http.StatusFound, "", document.RenditionErrorMalformedEvidence},
		{"malformed", http.StatusOK, `{`, document.RenditionErrorMalformedEvidence},
		{"partial", http.StatusOK, `{"format":"markdown","output":"some","images":{},"metadata":{"table_of_contents":[],"page_stats":[]},"success":false,"error":"private"}`, document.RenditionErrorMalformedEvidence},
		{"schema drift", http.StatusOK, `{"format":"markdown","output":"some","images":{},"metadata":{"table_of_contents":[],"page_stats":[]},"success":true,"new_field":1}`, document.RenditionErrorMalformedEvidence},
		{"wrong format", http.StatusOK, `{"format":"html","output":"some","images":{},"metadata":{"table_of_contents":[],"page_stats":[]},"success":true}`, document.RenditionErrorMalformedEvidence},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, staticTransport(testCase.status, testCase.body))
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			assertProviderCode(t, err, testCase.want)
			assert.NotContains(t, err.Error(), "private")
		})
	}

	bounded := fixture.profile
	bounded.MaxResponseBytes = 64
	bounded.MaxMetadataBytes = 32
	bounded.Descriptor = descriptorFor(t, bounded)
	client := newClient(t, bounded, testSecrets{"marker-front": "secret"}, staticTransport(http.StatusOK, strings.Repeat("x", 65)))
	boundedFixture := fixture.withDescriptor(bounded.Descriptor)
	_, err := client.Render(t.Context(), boundedFixture.upload(), boundedFixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
}

func TestClientNeverFollowsRedirects(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
	var calls atomic.Int64
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		response := jsonResponse(request, http.StatusFound, "")
		response.Header.Set("Location", "https://attacker.invalid/result")
		return response, nil
	})
	client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, transport)
	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	assert.Equal(t, int64(1), calls.Load())
}

func TestClientRequiresLocalProofOfCompletePDFUnitsAndTotalResultBound(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(2))
	partial := `{"format":"markdown","output":"{0}------------------------------------------------\n\nOnly first","images":{},"metadata":{"table_of_contents":[],"page_stats":[{"page_id":0,"text_extraction_method":"pdftext","block_counts":[],"block_metadata":{}}]},"success":true}`
	client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, staticTransport(http.StatusOK, partial))
	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)

	fixture = newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
	fixture.authorization.MaxTotalResultBytes = 128
	complete := `{"format":"markdown","output":"{0}------------------------------------------------\n\nComplete","images":{},"metadata":{"table_of_contents":[],"page_stats":[{"page_id":0,"text_extraction_method":"pdftext","block_counts":[],"block_metadata":{}}]},"success":true}`
	client = newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, staticTransport(http.StatusOK, complete))
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
}

func TestClientProvesStillImageIdentityBeforeEgress(t *testing.T) {
	const complete = `{"format":"markdown","output":"{0}------------------------------------------------\n\nImage text","images":{},"metadata":{"table_of_contents":[],"page_stats":[{"page_id":0,"text_extraction_method":"surya","block_counts":[],"block_metadata":{}}]},"success":true}`
	still := newFixture(t, "image", "image/png", "scan.png", mediatest.PNG(4, 3, nil))
	client := newClient(t, still.profile, testSecrets{"marker-front": "secret"}, staticTransport(http.StatusOK, complete))
	result, err := document.RenderRendition(t.Context(), client, still.upload(), still.authorization)
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
	require.Len(t, result.Evidence.Units, 1)

	for _, testCase := range []struct {
		name      string
		mediaType string
		filename  string
		source    []byte
	}{
		{name: "malformed", mediaType: "image/png", filename: "scan.png", source: []byte("not a png")},
		{name: "mismatched", mediaType: "image/jpeg", filename: "scan.jpg", source: mediatest.PNG(4, 3, nil)},
		{name: "animated", mediaType: "image/gif", filename: "scan.gif", source: mediatest.GIF(2, 2, 2)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, "image", testCase.mediaType, testCase.filename, testCase.source)
			var calls atomic.Int64
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, errors.New("unexpected egress")
			})
			client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, transport)

			_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)

			assertProviderCode(t, err, document.RenditionErrorUnsupportedInput)
			assert.Zero(t, calls.Load())
		})
	}
}

func TestClientRechecksExpiryAndCancellationWhileReadingResponse(t *testing.T) {
	const complete = `{"format":"markdown","output":"{0}------------------------------------------------\n\nComplete","images":{},"metadata":{"table_of_contents":[],"page_stats":[{"page_id":0,"text_extraction_method":"pdftext","block_counts":[],"block_metadata":{}}]},"success":true}`

	t.Run("expiry after complete body", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
		expiresAt := time.Now().UTC().Add(30 * time.Millisecond)
		fixture.authorization.ExpiresAt = expiresAt.Format(timestampForm)
		body := &callbackReadCloser{reader: strings.NewReader(complete), before: func() {
			time.Sleep(time.Until(expiresAt) + 10*time.Millisecond)
		}}
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: request}, nil
		})
		client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, transport)

		_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)

		require.ErrorContains(t, err, "authorization is not current")
	})

	t.Run("cancellation after complete body", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
		ctx, cancel := context.WithCancel(t.Context())
		body := &callbackReadCloser{reader: strings.NewReader(complete), before: cancel}
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: request}, nil
		})
		client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, transport)

		_, err := document.RenderRendition(ctx, client, fixture.upload(), fixture.authorization)

		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("cancellation on body read error", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
		ctx, cancel := context.WithCancel(t.Context())
		body := &errorReadCloser{before: cancel, err: errors.New("private body failure")}
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: request}, nil
		})
		client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, transport)

		_, err := document.RenderRendition(ctx, client, fixture.upload(), fixture.authorization)

		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestClientClassifiesAuthCapacityTransportExpiryAndCancellation(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
	for _, testCase := range []struct {
		status int
		want   document.RenditionErrorCode
	}{
		{http.StatusUnauthorized, document.RenditionErrorAuthentication},
		{http.StatusTooManyRequests, document.RenditionErrorRateLimited},
		{http.StatusServiceUnavailable, document.RenditionErrorCapacity},
		{http.StatusUnsupportedMediaType, document.RenditionErrorUnsupportedInput},
	} {
		client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, staticTransport(testCase.status, "private"))
		_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
		assertProviderCode(t, err, testCase.want)
		assert.NotContains(t, err.Error(), "private")
	}
	client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private transport")
	}))
	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorAmbiguousSubmission)

	expired := fixture
	expired.authorization.ExpiresAt = time.Now().UTC().Add(-time.Second).Format(timestampForm)
	_, err = client.Render(t.Context(), expired.upload(), expired.authorization)
	require.Error(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.Render(ctx, fixture.upload(), fixture.authorization)
	assertProviderCode(t, err, document.RenditionErrorCanceled)
}

func TestClientBoundsImageAndMetadataPayloads(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
	for _, body := range []string{
		`{"format":"markdown","output":"safe","images":{"a":"c3ludGhldGljIGltYWdl"},"metadata":{"table_of_contents":[],"page_stats":[]},"success":true}`,
		`{"format":"markdown","output":"safe","images":{},"metadata":{"table_of_contents":"` + strings.Repeat("x", 257) + `","page_stats":[]},"success":true}`,
	} {
		client := newClient(t, fixture.profile, testSecrets{"marker-front": "secret"}, staticTransport(http.StatusOK, body))
		_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
		assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	}
}

type fixture struct {
	profile       Profile
	descriptor    document.RenditionDescriptor
	metadata      document.AuthorizedUploadMetadata
	authorization document.RenditionAuthorization
	source        []byte
}

func newFixture(t *testing.T, family, mediaType, filename string, source []byte) fixture {
	t.Helper()
	profile := Profile{Origin: "https://marker.internal", SecretBinding: "marker-front", Mode: "fast",
		DeploymentFingerprint: strings.Repeat("d", 64), RuntimeFingerprint: strings.Repeat("r", 64),
		RequestTimeout: time.Second, MaxDocumentBytes: 1 << 20, MaxRequestBytes: 2 << 20,
		MaxResponseBytes: 1 << 20, MaxMetadataBytes: 256, MaxImages: 1, MaxImageBytes: 8, MaxUnits: 32}
	// Runtime fingerprints are SHA-256 values, so use hexadecimal input.
	profile.RuntimeFingerprint = strings.Repeat("e", 64)
	profile.Descriptor = descriptorFor(t, profile)
	digest := sha256.Sum256(source)
	metadata := document.AuthorizedUploadMetadata{Filename: filename, MediaFamily: family, MediaType: mediaType,
		ByteLength: int64(len(source)), SHA256: hex.EncodeToString(digest[:]), CapabilityRecordChecksum: strings.Repeat("2", 64),
		ProviderMetadataChecksum: strings.Repeat("3", 64), InputKind: document.RenditionInputOriginalFile}
	started := time.Now().UTC().Add(-time.Minute)
	authorization := document.RenditionAuthorization{ProviderID: profile.Descriptor.ID, DescriptorFingerprint: profile.Descriptor.Fingerprint,
		PolicyFingerprint: profile.Descriptor.PolicyFingerprint, RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256: metadata.SHA256, SourceBytes: metadata.ByteLength, CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum, MediaFamily: family, MediaType: mediaType,
		InputKind: document.RenditionInputOriginalFile, DiscloseFilename: true,
		MaxProviderMarkdownBytes: 4096, MaxTotalResultBytes: 32768,
		AuthorizedAt: started.Format(timestampForm), ExpiresAt: started.Add(10 * time.Minute).Format(timestampForm)}
	return fixture{profile: profile, descriptor: profile.Descriptor, metadata: metadata, authorization: authorization, source: source}
}

func descriptorFor(t *testing.T, profile Profile) document.RenditionDescriptor {
	t.Helper()
	fingerprint, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{ID: providerID,
		ContractVersion: document.RenditionProviderContractVersion, PolicyFingerprint: fingerprint,
		TrustBoundary: document.RenditionTrustOperatorNetwork, SupportedFormats: SupportedFormats(),
		ReturnsMarkdown: true, ReturnsStructured: true})
	require.NoError(t, err)
	return descriptor
}

func (fixture fixture) upload() document.AuthorizedUpload {
	return &testUpload{Reader: bytes.NewReader(fixture.source), metadata: fixture.metadata}
}

func (fixture fixture) withDescriptor(descriptor document.RenditionDescriptor) fixture {
	fixture.descriptor, fixture.profile.Descriptor = descriptor, descriptor
	fixture.authorization.ProviderID = descriptor.ID
	fixture.authorization.DescriptorFingerprint = descriptor.Fingerprint
	fixture.authorization.PolicyFingerprint = descriptor.PolicyFingerprint
	return fixture
}

func newClient(t *testing.T, profile Profile, secrets SecretResolver, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := New(profile, secrets, transport)
	require.NoError(t, err)
	return client
}

func staticTransport(status int, body string) http.RoundTripper {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) { return jsonResponse(request, status, body), nil })
}

func jsonResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func assertMultipart(t *testing.T, request *http.Request, metadata document.AuthorizedUploadMetadata, source []byte) {
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
		} else {
			fields[part.FormName()] = string(value)
		}
	}
	assert.Equal(t, map[string]string{"mode": "fast", "force_ocr": "false", "paginate_output": "true", "output_format": "markdown"}, fields)
}

func assertProviderCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok, "%T: %v", err, err)
	assert.Equal(t, want, providerErr.Code())
}

func TestPolicyFingerprintIsCanonical(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", testPDF(1))
	first, err := PolicyFingerprint(fixture.profile)
	require.NoError(t, err)
	encoded, err := json.Marshal(map[string]string{"fingerprint": first}, json.Deterministic(true))
	require.NoError(t, err)
	assert.Contains(t, string(encoded), first)
}

func testPDF(pageCount int) []byte {
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n%synthetic-marker-fixture\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", pdfKids(pageCount), pageCount),
	}
	for index := range pageCount {
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R >>", pageCount+3+index))
	}
	for range pageCount {
		objects = append(objects, "<< /Length 0 >>\nstream\n\nendstream")
	}
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

func pdfKids(pageCount int) string {
	children := make([]string, pageCount)
	for index := range pageCount {
		children[index] = fmt.Sprintf("%d 0 R", index+3)
	}
	return strings.Join(children, " ")
}
