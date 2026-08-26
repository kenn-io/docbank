package embeddingbridge_test

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
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/embeddingbridge"
	"go.kenn.io/docbank/document/providerhttp"
)

type secretMap map[string]string

func (values secretMap) ResolveSecret(_ context.Context, binding string) (string, error) {
	value, ok := values[binding]
	if !ok {
		return "", errors.New("synthetic resolver detail")
	}
	return value, nil
}

type fixedResolver struct{ address netip.Addr }

func (resolver fixedResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{resolver.address}, nil
}

type bridgeFixture struct {
	t          *testing.T
	server     *http.Server
	listener   net.Listener
	origin     string
	resolver   fixedResolver
	profile    embeddingbridge.Profile
	descriptor document.EmbeddingDescriptor
	client     *embeddingbridge.Client
}

func newBridgeFixture(t *testing.T, handler http.Handler) bridgeFixture {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		require.NoError(t, server.Shutdown(context.Background()))
	})
	tcpAddress, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := tcpAddress.Port
	origin := "http://embedding.invalid:" + strconv.Itoa(port)
	modelInput, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	require.NoError(t, err)
	descriptor := document.EmbeddingDescriptor{
		ID: "synthetic.embedding-bridge", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: strings.Repeat("0", 64), TrustBoundary: document.EmbeddingTrustOperatorNetwork,
		Model: "synthetic-embed", ModelRevision: "immutable-r1", Dimension: 2,
		Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone,
		ScalarEncoding: "float32", DocumentFormatter: "synthetic/document-v1",
		QueryFormatter:  "synthetic/query-v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk},
		CompatibilityID: modelInput.CompatibilityID, SupportsTextQuery: true, ModelInput: modelInput,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	}
	descriptor, err = document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
	profile := embeddingbridge.Profile{
		Origin: origin, Descriptor: descriptor, SecretBinding: "credential:synthetic-bridge",
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "http", Host: "embedding.invalid", Port: uint16(port),
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			ProxyMode:    providerhttp.ProxyDisabled,
		},
		RequestTimeout: time.Second, MaxBatchItems: 8, MaxInputBytes: 4096,
		MaxRequestBytes: 16 << 10, MaxResponseBytes: 16 << 10,
	}
	fingerprint, err := embeddingbridge.PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = fingerprint
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	descriptor = profile.Descriptor
	httpClient := &http.Client{}
	client, err := embeddingbridge.New(profile, secretMap{"credential:synthetic-bridge": "synthetic-secret"}, fixedResolver{netip.MustParseAddr("127.0.0.1")}, httpClient)
	require.NoError(t, err)
	return bridgeFixture{t: t, server: server, listener: listener, origin: origin, resolver: fixedResolver{netip.MustParseAddr("127.0.0.1")}, profile: profile, descriptor: descriptor, client: client}
}

func (fixture bridgeFixture) authorization(items int) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{
		ProviderID: fixture.descriptor.ID, DescriptorFingerprint: fixture.descriptor.Fingerprint,
		PolicyFingerprint: fixture.descriptor.PolicyFingerprint, MaxBatchItems: items,
		MaxInputBytes: 4096, MaxResponseBytes: 4096,
	}
}

type requestManifest struct {
	ContractVersion       string                          `json:"contract_version"`
	DescriptorFingerprint string                          `json:"descriptor_fingerprint"`
	PolicyFingerprint     string                          `json:"policy_fingerprint"`
	Authorization         document.EmbeddingAuthorization `json:"authorization"`
	Inputs                []manifestInput                 `json:"inputs"`
	RequestChecksum       string                          `json:"request_checksum,omitempty"`
}

type manifestInput struct {
	Index       int                                `json:"index"`
	Key         string                             `json:"key"`
	Role        document.EmbeddingRole             `json:"role"`
	Kind        document.EmbeddingInputKind        `json:"kind"`
	ByteLength  int64                              `json:"byte_length"`
	SHA256      string                             `json:"sha256"`
	Text        string                             `json:"text,omitempty"`
	HeadingPath []string                           `json:"heading_path,omitempty"`
	SourceSpans []manifestSpan                     `json:"source_spans,omitempty"`
	Upload      *document.AuthorizedUploadMetadata `json:"upload,omitempty"`
	FilePart    string                             `json:"file_part,omitempty"`
	FileIndex   *int                               `json:"file_index,omitempty"`
}

type manifestSpan struct {
	UnitIndex int `json:"unit_index"`
	CharStart int `json:"char_start"`
	CharEnd   int `json:"char_end"`
}

type responseEnvelope struct {
	ContractVersion       string                     `json:"contract_version"`
	DescriptorFingerprint string                     `json:"descriptor_fingerprint"`
	PolicyFingerprint     string                     `json:"policy_fingerprint"`
	RequestChecksum       string                     `json:"request_checksum"`
	Vectors               []document.EmbeddingVector `json:"vectors"`
}

func readBridgeRequest(t *testing.T, request *http.Request) (requestManifest, [][]byte) {
	t.Helper()
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(request.Body, parameters["boundary"])
	var manifest requestManifest
	var files [][]byte
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		payload, err := io.ReadAll(part)
		require.NoError(t, err)
		switch part.FormName() {
		case "manifest":
			require.Equal(t, "application/vnd.docbank.embedding-manifest+json;version=1", part.Header.Get("Content-Type"))
			require.NoError(t, json.Unmarshal(payload, &manifest, json.RejectUnknownMembers(true)))
		case "file":
			files = append(files, payload)
		default:
			t.Fatalf("unexpected multipart field %q", part.FormName())
		}
	}
	return manifest, files
}

func writeSuccess(t *testing.T, writer http.ResponseWriter, manifest requestManifest, vectors []document.EmbeddingVector) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/vnd.docbank.embedding-result+json;version=1")
	payload, err := json.Marshal(responseEnvelope{
		ContractVersion:       embeddingbridge.ContractVersion,
		DescriptorFingerprint: manifest.DescriptorFingerprint,
		PolicyFingerprint:     manifest.PolicyFingerprint,
		RequestChecksum:       manifest.RequestChecksum,
		Vectors:               vectors,
	})
	require.NoError(t, err)
	_, err = writer.Write(payload)
	require.NoError(t, err)
}

func TestTextDocumentAndQueryManifestBindsExactRenderedBytesAndChecksum(t *testing.T) {
	var captured requestManifest
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/docbank-embedding/v1/embeddings", request.URL.Path)
		assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization"))
		assert.Equal(t, request.Header.Get("Idempotency-Key"), request.Header.Get("Docbank-Request-Checksum"))
		manifest, files := readBridgeRequest(t, request)
		captured = manifest
		assert.Empty(t, files)
		zero, one := 0, 1
		writeSuccess(t, writer, manifest, []document.EmbeddingVector{
			{Key: "chunk-a", Index: &zero, Values: []float32{0.25, 0.5}},
			{Key: "query-a", Index: &one, Values: []float32{0.75, 1}},
		})
	}))
	inputs := []document.EmbeddingInput{
		{
			Key: "chunk-a", Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputRenditionChunk, Text: "alpha",
			HeadingPath: []string{"Overview"},
			SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 5}},
		},
		{Key: "query-a", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "beta"},
	}
	authorization := fixture.authorization(2)
	result, err := fixture.client.Embed(context.Background(), inputs, authorization)
	require.NoError(t, err)
	require.Len(t, result.Vectors, 2)
	assert.Equal(t, []float32{0.25, 0.5}, result.Vectors[0].Values)
	assert.Nil(t, result.Vectors[0].Index, "the provider result is normalized to request order")

	assert.Equal(t, embeddingbridge.ContractVersion, captured.ContractVersion)
	assert.Equal(t, fixture.descriptor.Fingerprint, captured.DescriptorFingerprint)
	assert.Equal(t, fixture.descriptor.PolicyFingerprint, captured.PolicyFingerprint)
	assert.Equal(t, authorization, captured.Authorization)
	require.Len(t, captured.Inputs, 2)
	assert.Equal(t, "search_document: alpha", captured.Inputs[0].Text)
	assert.Equal(t, int64(22), captured.Inputs[0].ByteLength)
	assert.Equal(t, sha256Text("search_document: alpha"), captured.Inputs[0].SHA256)
	assert.Equal(t, []string{"Overview"}, captured.Inputs[0].HeadingPath)
	assert.Equal(t, []manifestSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 5}}, captured.Inputs[0].SourceSpans)
	assert.Equal(t, "search_query: beta", captured.Inputs[1].Text)
	assert.Equal(t, int64(18), captured.Inputs[1].ByteLength)
	assert.Equal(t, sha256Text("search_query: beta"), captured.Inputs[1].SHA256)
	assert.Equal(t, exactManifestChecksum(t, captured), captured.RequestChecksum)
	assert.NotContains(t, mustJSON(t, captured), "node_id")
	assert.NotContains(t, mustJSON(t, captured), "tags")
	assert.NotContains(t, mustJSON(t, captured), "vault")
	assert.Contains(t, mustJSON(t, captured), `"unit_index":0`)
}

func TestExplicitNoAuthProfileUsesNoResolverOrAuthorizationHeader(t *testing.T) {
	var requests atomic.Int32
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		assert.Empty(t, request.Header.Get("Authorization"))
		manifest, _ := readBridgeRequest(t, request)
		zero := 0
		writeSuccess(t, writer, manifest, []document.EmbeddingVector{{Key: "first", Index: &zero, Values: []float32{1, 2}}})
	}))
	profile := fixture.profile
	profile.SecretBinding = ""
	fingerprint, err := embeddingbridge.PolicyFingerprint(profile)
	require.NoError(t, err)
	assert.NotEqual(t, fixture.descriptor.PolicyFingerprint, fingerprint)
	profile.Descriptor.PolicyFingerprint = fingerprint
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	client, err := embeddingbridge.New(profile, nil, fixture.resolver, &http.Client{})
	require.NoError(t, err)
	_, err = client.Embed(context.Background(), oneTextInput("alpha"), document.EmbeddingAuthorization{
		ProviderID: profile.Descriptor.ID, DescriptorFingerprint: profile.Descriptor.Fingerprint,
		PolicyFingerprint: profile.Descriptor.PolicyFingerprint, MaxBatchItems: 1,
		MaxInputBytes: 4096, MaxResponseBytes: 4096,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), requests.Load())

	_, err = embeddingbridge.New(profile, secretMap{}, fixture.resolver, &http.Client{})
	require.Error(t, err, "an empty binding cannot carry an ambient resolver")
	_, err = embeddingbridge.New(fixture.profile, nil, fixture.resolver, &http.Client{})
	require.Error(t, err, "a named binding requires its narrow resolver")
	hosted := profile
	hosted.Descriptor.TrustBoundary = document.EmbeddingTrustHostedProvider
	_, err = embeddingbridge.PolicyFingerprint(hosted)
	require.Error(t, err, "anonymous profiles are operator-network only")
}

func TestMixedTextAndDirectFileRequestStreamsOnlyAuthorizedBytes(t *testing.T) {
	source := []byte("synthetic direct file")
	metadata := uploadMetadata(source)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	parsed, err := url.Parse("http://embedding.invalid")
	require.NoError(t, err)
	jar.SetCookies(parsed, []*http.Cookie{{ //nolint:gosec // Deliberately ambient insecure test cookie; isolation must strip it.
		Name: "ambient", Value: "PRIVATE_COOKIE",
	}})
	var manifest requestManifest
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Empty(t, request.Header.Get("Cookie"))
		var files [][]byte
		manifest, files = readBridgeRequest(t, request)
		assert.Equal(t, [][]byte{source}, files)
		zero, one := 0, 1
		writeSuccess(t, writer, manifest, []document.EmbeddingVector{
			{Key: "source-a", Index: &zero, Values: []float32{1, 2}},
			{Key: "query-a", Index: &one, Values: []float32{3, 4}},
		})
	}))
	client, err := embeddingbridge.New(fixture.profile, secretMap{"credential:synthetic-bridge": "synthetic-secret"}, fixture.resolver, &http.Client{Jar: jar})
	require.NoError(t, err)
	upload := &testUpload{Reader: bytes.NewReader(source), metadata: metadata}
	result, err := client.Embed(context.Background(), []document.EmbeddingInput{
		{Key: "source-a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: upload},
		{Key: "query-a", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "needle"},
	}, fixture.authorization(2))
	require.NoError(t, err)
	require.Len(t, result.Vectors, 2)
	require.Len(t, manifest.Inputs, 2)
	assert.Equal(t, &metadata, manifest.Inputs[0].Upload)
	assert.Equal(t, "file", manifest.Inputs[0].FilePart)
	require.NotNil(t, manifest.Inputs[0].FileIndex)
	assert.Zero(t, *manifest.Inputs[0].FileIndex)
	assert.Empty(t, manifest.Inputs[0].Text)
	assert.Equal(t, int64(len(source)), manifest.Inputs[0].ByteLength)
	assert.Equal(t, metadata.SHA256, manifest.Inputs[0].SHA256)
}

func TestResponseContractFailsClosed(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        func(requestManifest) string
		category    embeddingbridge.ErrorCategory
	}{
		{name: "wrong media type", contentType: "application/json", body: validResponseBody, category: embeddingbridge.ErrorMalformedResponse},
		{name: "unknown major", contentType: "application/vnd.docbank.embedding-result+json;version=2", body: func(manifest requestManifest) string {
			return strings.Replace(validResponseBody(manifest), embeddingbridge.ContractVersion, "docbank-embedding/v2", 1)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "descriptor drift", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			manifest.DescriptorFingerprint = strings.Repeat("d", 64)
			return validResponseBody(manifest)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "policy drift", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			manifest.PolicyFingerprint = strings.Repeat("e", 64)
			return validResponseBody(manifest)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "request drift", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			manifest.RequestChecksum = strings.Repeat("f", 64)
			return validResponseBody(manifest)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "wrong order", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			return responseBody(manifest, `[{"key":"second","index":1,"values":[1,2]},{"key":"first","index":0,"values":[3,4]}]`)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "wrong index", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			return responseBody(manifest, `[{"key":"first","index":1,"values":[1,2]},{"key":"second","index":0,"values":[3,4]}]`)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "dimension", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			return responseBody(manifest, `[{"key":"first","index":0,"values":[1]},{"key":"second","index":1,"values":[3,4]}]`)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "non finite", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			return responseBody(manifest, `[{"key":"first","index":0,"values":[1e1000,2]},{"key":"second","index":1,"values":[3,4]}]`)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "duplicate key", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			return responseBody(manifest, `[{"key":"first","index":0,"values":[1,2]},{"key":"first","index":1,"values":[3,4]}]`)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "missing key", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			return responseBody(manifest, `[{"key":"first","index":0,"values":[1,2]}]`)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "extra key", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			return responseBody(manifest, `[{"key":"first","index":0,"values":[1,2]},{"key":"second","index":1,"values":[3,4]},{"key":"extra","index":2,"values":[5,6]}]`)
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "unknown field", contentType: responseMediaType(), body: func(manifest requestManifest) string {
			return strings.TrimSuffix(validResponseBody(manifest), "}") + `,"provider_body":"PRIVATE_RESPONSE"}`
		}, category: embeddingbridge.ErrorMalformedResponse},
		{name: "trailing JSON", contentType: responseMediaType(), body: func(manifest requestManifest) string { return validResponseBody(manifest) + `{}` }, category: embeddingbridge.ErrorMalformedResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				manifest, _ := readBridgeRequest(t, request)
				writer.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(writer, test.body(manifest))
			}))
			_, err := fixture.client.Embed(context.Background(), twoTextInputs(), fixture.authorization(2))
			require.Error(t, err)
			assert.Equal(t, test.category, embeddingbridge.Category(err))
			assert.NotContains(t, err.Error(), "PRIVATE_RESPONSE")
		})
	}
}

func TestResponseByteLimitIsEnforcedBeforeDecode(t *testing.T) {
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = readBridgeRequest(t, request)
		writer.Header().Set("Content-Type", responseMediaType())
		_, _ = writer.Write(bytes.Repeat([]byte("x"), 16<<10+1))
	}))
	_, err := fixture.client.Embed(context.Background(), twoTextInputs(), fixture.authorization(2))
	require.Error(t, err)
	assert.Equal(t, embeddingbridge.ErrorMalformedResponse, embeddingbridge.Category(err))
}

func TestProfileFingerprintFreezesDescriptorModelOriginEgressBindingAndBounds(t *testing.T) {
	fixture := newBridgeFixture(t, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
	}))
	mutations := []struct {
		name   string
		mutate func(*embeddingbridge.Profile)
	}{
		{"model revision", func(profile *embeddingbridge.Profile) { profile.Descriptor.ModelRevision = "immutable-r2" }},
		{"model input", func(profile *embeddingbridge.Profile) {
			profile.Descriptor.ModelInput.Query.Template = "query: {{content}}"
		}},
		{"compatibility", func(profile *embeddingbridge.Profile) { profile.Descriptor.CompatibilityID = "changed/v1" }},
		{"binding", func(profile *embeddingbridge.Profile) { profile.SecretBinding = "credential:other" }},
		{"origin", func(profile *embeddingbridge.Profile) { profile.Origin += "/other" }},
		{"egress", func(profile *embeddingbridge.Profile) {
			profile.EgressPolicy.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
		}},
		{"bounds", func(profile *embeddingbridge.Profile) { profile.MaxBatchItems-- }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			profile := fixture.profile
			profile.EgressPolicy.AllowedCIDRs = slices.Clone(profile.EgressPolicy.AllowedCIDRs)
			mutation.mutate(&profile)
			_, err := embeddingbridge.New(profile, secretMap{"credential:synthetic-bridge": "synthetic-secret"}, fixture.resolver, &http.Client{})
			require.Error(t, err)
		})
	}
}

func TestSecretResolutionRedirectCancellationAndAmbiguousSubmission(t *testing.T) {
	t.Run("secret resolution", func(t *testing.T) {
		var requests int
		fixture := newBridgeFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
		client, err := embeddingbridge.New(fixture.profile, secretMap{}, fixture.resolver, &http.Client{})
		require.NoError(t, err)
		_, err = client.Embed(context.Background(), oneTextInput("private input"), fixture.authorization(1))
		require.Error(t, err)
		assert.Equal(t, embeddingbridge.ErrorAuthentication, embeddingbridge.Category(err))
		assert.Zero(t, requests)
		assert.NotContains(t, err.Error(), "synthetic resolver detail")
		assert.NotContains(t, err.Error(), "private input")
	})
	t.Run("redirect", func(t *testing.T) {
		var redirected int
		mux := http.NewServeMux()
		mux.HandleFunc("/docbank-embedding/v1/embeddings", func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, "/redirected", http.StatusTemporaryRedirect)
		})
		mux.HandleFunc("/redirected", func(http.ResponseWriter, *http.Request) { redirected++ })
		fixture := newBridgeFixture(t, mux)
		_, err := fixture.client.Embed(context.Background(), oneTextInput("alpha"), fixture.authorization(1))
		require.Error(t, err)
		assert.Zero(t, redirected)
		assert.Equal(t, embeddingbridge.ErrorPermanent, embeddingbridge.Category(err))
	})
	t.Run("cancellation", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		fixture := newBridgeFixture(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
		}))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := fixture.client.Embed(ctx, oneTextInput("alpha"), fixture.authorization(1))
			done <- err
		}()
		<-started
		cancel()
		err := <-done
		close(release)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("ambiguous submission", func(t *testing.T) {
		fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_, _ = readBridgeRequest(t, request)
			hijacker, ok := writer.(http.Hijacker)
			if !assert.True(t, ok) {
				return
			}
			connection, _, err := hijacker.Hijack()
			if !assert.NoError(t, err) {
				return
			}
			_ = connection.Close()
		}))
		_, err := fixture.client.Embed(context.Background(), oneTextInput("alpha"), fixture.authorization(1))
		require.Error(t, err)
		assert.Equal(t, embeddingbridge.ErrorAmbiguousSubmission, embeddingbridge.Category(err))
		assert.True(t, embeddingbridge.IsRetryable(err))
	})
}

func TestDirectFileTerminalLengthAndChecksumAreProven(t *testing.T) {
	expected := []byte("12345")
	tests := []struct {
		name     string
		actual   []byte
		metadata document.AuthorizedUploadMetadata
	}{
		{name: "short", actual: []byte("1234"), metadata: uploadMetadata(expected)},
		{name: "long", actual: []byte("123456"), metadata: uploadMetadata(expected)},
		{name: "substituted", actual: []byte("abcde"), metadata: uploadMetadata(expected)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				writer.Header().Set("Content-Type", responseMediaType())
				_, _ = io.WriteString(writer, `{}`)
			}))
			upload := &testUpload{Reader: bytes.NewReader(test.actual), metadata: test.metadata}
			_, err := fixture.client.Embed(context.Background(), []document.EmbeddingInput{{
				Key: "source", Role: document.EmbeddingRoleDocument,
				Kind: document.EmbeddingInputOriginalFile, Source: upload,
			}}, fixture.authorization(1))
			require.Error(t, err)
			assert.Equal(t, embeddingbridge.ErrorSourceChanged, embeddingbridge.Category(err))
			assert.NotContains(t, err.Error(), string(test.actual))
		})
	}
}

func TestUploadMetadataIsFrozenOnceBeforeValidationAndTransmission(t *testing.T) {
	source := []byte("immutable synthetic source")
	upload := &countingUpload{Reader: bytes.NewReader(source), metadata: uploadMetadata(source)}
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		manifest, files := readBridgeRequest(t, request)
		assert.Equal(t, [][]byte{source}, files)
		zero := 0
		writeSuccess(t, writer, manifest, []document.EmbeddingVector{{Key: "source", Index: &zero, Values: []float32{1, 2}}})
	}))
	_, err := fixture.client.Embed(context.Background(), []document.EmbeddingInput{{
		Key: "source", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputOriginalFile, Source: upload,
	}}, fixture.authorization(1))
	require.NoError(t, err)
	assert.Equal(t, int32(1), upload.calls.Load())
}

func TestDirectFileRejectsHeaderUnsafeFilenameBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	source := []byte("synthetic source")
	metadata := uploadMetadata(source)
	metadata.Filename = "synthetic.bin\r\nX-Injected: private"
	fixture := newBridgeFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	_, err := fixture.client.Embed(context.Background(), []document.EmbeddingInput{{
		Key: "source", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile,
		Source: &testUpload{Reader: bytes.NewReader(source), metadata: metadata},
	}}, fixture.authorization(1))
	require.Error(t, err)
	assert.Equal(t, embeddingbridge.ErrorPermanent, embeddingbridge.Category(err))
	assert.Zero(t, requests.Load())
	assert.NotContains(t, err.Error(), "X-Injected")
}

func TestCancellationClosesBlockedDirectFileAndReturns(t *testing.T) {
	source := []byte("synthetic blocked source")
	upload := newBlockingUpload(uploadMetadata(source))
	fixture := newBridgeFixture(t, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fixture.client.Embed(ctx, []document.EmbeddingInput{{
			Key: "source", Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputOriginalFile, Source: upload,
		}}, fixture.authorization(1))
		done <- err
	}()
	<-upload.started
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(250 * time.Millisecond):
		_ = upload.Close()
		err := <-done
		assert.Fail(t, "embedding bridge did not close the blocked authorized upload", "eventual error: %v", err)
	}
	assert.Positive(t, upload.closes.Load())
}

func TestCancellationBeforeSecretResolutionLeavesUnreadSourceOpen(t *testing.T) {
	source := []byte("synthetic unread source")
	upload := newObservedUpload(source, uploadMetadata(source))
	resolver := &cancelBlockingSecretResolver{started: make(chan struct{})}
	fixture := newBridgeFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("request reached server before secret resolution completed")
	}))
	client, err := embeddingbridge.New(fixture.profile, resolver, fixture.resolver, &http.Client{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, embedErr := client.Embed(ctx, []document.EmbeddingInput{{
			Key: "unread", Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputOriginalFile, Source: upload,
		}}, fixture.authorization(1))
		done <- embedErr
	}()
	<-resolver.started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Zero(t, upload.reads.Load())
	assert.Zero(t, upload.closes.Load(), "the worker retains ownership until a source read begins")
}

func TestMultiFileCancellationClosesOnlyActiveSource(t *testing.T) {
	firstBytes := []byte("synthetic first source")
	secondBytes := []byte("synthetic blocked second source")
	thirdBytes := []byte("synthetic unread third source")
	first := newObservedUpload(firstBytes, uploadMetadata(firstBytes))
	second := newBlockingUpload(uploadMetadata(secondBytes))
	third := newObservedUpload(thirdBytes, uploadMetadata(thirdBytes))
	fixture := newBridgeFixture(t, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, embedErr := fixture.client.Embed(ctx, []document.EmbeddingInput{
			{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: first},
			{Key: "second", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: second},
			{Key: "third", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: third},
		}, fixture.authorization(3))
		done <- embedErr
	}()
	<-second.started
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(250 * time.Millisecond):
		_ = second.Close()
		err := <-done
		assert.Fail(t, "embedding bridge did not close the active source", "eventual error: %v", err)
	}
	assert.Zero(t, first.closes.Load(), "a completed source is no longer owned by the bridge")
	assert.Positive(t, second.closes.Load(), "the active blocked source must be closed")
	assert.Zero(t, third.reads.Load(), "later sources must remain unread")
	assert.Zero(t, third.closes.Load(), "later unread sources remain worker-owned")
}

func TestCancellationWithNonComparableActiveSourceDoesNotPanic(t *testing.T) {
	source := []byte("synthetic non-comparable source")
	state := newBlockingUpload(uploadMetadata(source))
	upload := nonComparableUpload{state}
	fixture := newBridgeFixture(t, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, embedErr := fixture.client.Embed(ctx, []document.EmbeddingInput{{
			Key: "source", Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputOriginalFile, Source: upload,
		}}, fixture.authorization(1))
		done <- embedErr
	}()
	<-state.started
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(250 * time.Millisecond):
		_ = state.Close()
		err := <-done
		assert.Fail(t, "embedding bridge did not close the active non-comparable source", "eventual error: %v", err)
	}
}

func TestPolicyFingerprintDoesNotMutateCallerEgressOrder(t *testing.T) {
	fixture := newBridgeFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	profile := fixture.profile
	profile.EgressPolicy.AllowedCIDRs = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.2/32"),
		netip.MustParsePrefix("127.0.0.0/24"),
	}
	original := slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	fingerprint, err := embeddingbridge.PolicyFingerprint(profile)
	require.NoError(t, err)
	assert.Equal(t, original, profile.EgressPolicy.AllowedCIDRs)
	profile.Descriptor.PolicyFingerprint = fingerprint
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	_, err = embeddingbridge.New(profile, secretMap{"credential:synthetic-bridge": "synthetic-secret"}, fixture.resolver, &http.Client{})
	require.NoError(t, err)
	assert.Equal(t, original, profile.EgressPolicy.AllowedCIDRs)
}

func TestHTTPFailuresAreClassifiedWithoutProviderBodiesOrSecrets(t *testing.T) {
	tests := []struct {
		status   int
		category embeddingbridge.ErrorCategory
		retry    bool
	}{
		{http.StatusUnauthorized, embeddingbridge.ErrorAuthentication, false},
		{http.StatusRequestEntityTooLarge, embeddingbridge.ErrorCapacity, false},
		{http.StatusTooManyRequests, embeddingbridge.ErrorTransient, true},
		{http.StatusServiceUnavailable, embeddingbridge.ErrorTransient, true},
		{http.StatusBadRequest, embeddingbridge.ErrorPermanent, false},
	}
	for _, test := range tests {
		t.Run(strconv.Itoa(test.status), func(t *testing.T) {
			fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = readBridgeRequest(t, request)
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, "PRIVATE_RESPONSE synthetic-secret private-input")
			}))
			_, err := fixture.client.Embed(context.Background(), oneTextInput("private-input"), fixture.authorization(1))
			require.Error(t, err)
			assert.Equal(t, test.category, embeddingbridge.Category(err))
			assert.Equal(t, test.retry, embeddingbridge.IsRetryable(err))
			assert.NotContains(t, err.Error(), "PRIVATE_RESPONSE")
			assert.NotContains(t, err.Error(), "synthetic-secret")
			assert.NotContains(t, err.Error(), "private-input")
		})
	}
}

func TestEarlyHTTPRejectionDoesNotReadDirectFileAndKeepsStatusClassification(t *testing.T) {
	source := []byte("synthetic early rejection")
	upload := newBlockingUpload(uploadMetadata(source))
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "100-continue", request.Header.Get("Expect"))
		writer.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	done := make(chan error, 1)
	go func() {
		_, err := fixture.client.Embed(context.Background(), []document.EmbeddingInput{{
			Key: "source", Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputOriginalFile, Source: upload,
		}}, fixture.authorization(1))
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.Equal(t, embeddingbridge.ErrorCapacity, embeddingbridge.Category(err))
	case <-time.After(time.Second):
		_ = upload.Close()
		<-done
		assert.Fail(t, "early rejection did not return within the request bound")
	}
	assert.Zero(t, upload.closes.Load(), "the worker retains ownership when no source read starts")
	select {
	case <-upload.started:
		assert.Fail(t, "early rejection read the direct-file source")
	default:
	}
}

func TestDirectFileConnectionLossAfterPartHeaderIsAmbiguous(t *testing.T) {
	source := []byte("synthetic ambiguous source")
	upload := newGatedUpload(source, uploadMetadata(source))
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer upload.Release()
		mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if !assert.NoError(t, err) || !assert.Equal(t, "multipart/form-data", mediaType) {
			return
		}
		reader := multipart.NewReader(request.Body, parameters["boundary"])
		manifestPart, err := reader.NextPart()
		if !assert.NoError(t, err) {
			return
		}
		_, err = io.Copy(io.Discard, manifestPart)
		if !assert.NoError(t, err) {
			return
		}
		filePart, err := reader.NextPart()
		if !assert.NoError(t, err) || !assert.Equal(t, "file", filePart.FormName()) {
			return
		}
		hijacker, ok := writer.(http.Hijacker)
		if !assert.True(t, ok) {
			return
		}
		connection, _, err := hijacker.Hijack()
		if !assert.NoError(t, err) {
			return
		}
		_ = connection.Close()
	}))
	_, err := fixture.client.Embed(context.Background(), []document.EmbeddingInput{{
		Key: "source", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputOriginalFile, Source: upload,
	}}, fixture.authorization(1))
	require.Error(t, err)
	assert.Equal(t, embeddingbridge.ErrorAmbiguousSubmission, embeddingbridge.Category(err))
	assert.True(t, embeddingbridge.IsRetryable(err))
}

func TestDirectFileConnectionLossDuringBlockedReadIsAmbiguous(t *testing.T) {
	source := []byte("synthetic blocked connection-loss source")
	upload := newBlockingUpload(uploadMetadata(source))
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if !assert.NoError(t, err) || !assert.Equal(t, "multipart/form-data", mediaType) {
			return
		}
		reader := multipart.NewReader(request.Body, parameters["boundary"])
		manifestPart, err := reader.NextPart()
		if !assert.NoError(t, err) {
			return
		}
		_, err = io.Copy(io.Discard, manifestPart)
		if !assert.NoError(t, err) {
			return
		}
		filePart, err := reader.NextPart()
		if !assert.NoError(t, err) || !assert.Equal(t, "file", filePart.FormName()) {
			return
		}
		<-upload.started
		writer.Header().Set("Content-Type", responseMediaType())
		writer.Header().Set("Content-Length", "64")
		writer.WriteHeader(http.StatusOK)
		flusher, ok := writer.(http.Flusher)
		if !assert.True(t, ok) {
			return
		}
		flusher.Flush()
		hijacker, ok := writer.(http.Hijacker)
		if !assert.True(t, ok) {
			return
		}
		connection, _, err := hijacker.Hijack()
		if !assert.NoError(t, err) {
			return
		}
		_ = connection.Close()
	}))
	_, err := fixture.client.Embed(context.Background(), []document.EmbeddingInput{{
		Key: "source", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputOriginalFile, Source: upload,
	}}, fixture.authorization(1))
	require.Error(t, err)
	assert.Equal(t, embeddingbridge.ErrorAmbiguousSubmission, embeddingbridge.Category(err))
	assert.True(t, embeddingbridge.IsRetryable(err))
	assert.NotContains(t, err.Error(), "synthetic blocked reader closed")
}

func TestKnownHTTPStatusDuringBlockedUploadWinsWriterError(t *testing.T) {
	source := []byte("synthetic blocked rejected source")
	upload := newBlockingUpload(uploadMetadata(source))
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if !assert.NoError(t, err) || !assert.Equal(t, "multipart/form-data", mediaType) {
			return
		}
		reader := multipart.NewReader(request.Body, parameters["boundary"])
		manifestPart, err := reader.NextPart()
		if !assert.NoError(t, err) {
			return
		}
		_, err = io.Copy(io.Discard, manifestPart)
		if !assert.NoError(t, err) {
			return
		}
		filePart, err := reader.NextPart()
		if !assert.NoError(t, err) || !assert.Equal(t, "file", filePart.FormName()) {
			return
		}
		<-upload.started
		writer.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	_, err := fixture.client.Embed(context.Background(), []document.EmbeddingInput{{
		Key: "source", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputOriginalFile, Source: upload,
	}}, fixture.authorization(1))
	require.Error(t, err)
	assert.Equal(t, embeddingbridge.ErrorCapacity, embeddingbridge.Category(err))
	assert.False(t, embeddingbridge.IsRetryable(err))
	assert.NotContains(t, err.Error(), "synthetic blocked reader closed")
	assert.Positive(t, upload.closes.Load())
}

func TestConcurrentRequestsDoNotShareSecretsOrState(t *testing.T) {
	var mu sync.Mutex
	checksums := make(map[string]bool)
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		manifest, _ := readBridgeRequest(t, request)
		mu.Lock()
		checksums[manifest.RequestChecksum] = true
		mu.Unlock()
		zero := 0
		writeSuccess(t, writer, manifest, []document.EmbeddingVector{{Key: manifest.Inputs[0].Key, Index: &zero, Values: []float32{1, 2}}})
	}))
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for _, value := range []string{"alpha", "beta"} {
		wait.Go(func() {
			_, err := fixture.client.Embed(context.Background(), oneTextInput(value), fixture.authorization(1))
			errorsSeen <- err
		})
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		require.NoError(t, err)
	}
	assert.Len(t, checksums, 2)
}

type testUpload struct {
	io.Reader

	metadata document.AuthorizedUploadMetadata
}

func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

func (upload *testUpload) Close() error { return nil }

type countingUpload struct {
	io.Reader

	metadata document.AuthorizedUploadMetadata
	calls    atomic.Int32
}

func (upload *countingUpload) Metadata() document.AuthorizedUploadMetadata {
	upload.calls.Add(1)
	return upload.metadata
}

func (upload *countingUpload) Close() error { return nil }

type blockingUpload struct {
	metadata document.AuthorizedUploadMetadata
	started  chan struct{}
	closed   chan struct{}
	start    sync.Once
	close    sync.Once
	closes   atomic.Int32
}

type observedUpload struct {
	reader   *bytes.Reader
	metadata document.AuthorizedUploadMetadata
	reads    atomic.Int32
	closes   atomic.Int32
}

type cancelBlockingSecretResolver struct {
	started chan struct{}
	once    sync.Once
}

type nonComparableUpload []*blockingUpload

func (upload nonComparableUpload) Read(value []byte) (int, error) {
	return upload[0].Read(value)
}

func (upload nonComparableUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload[0].Metadata()
}

func (upload nonComparableUpload) Close() error { return upload[0].Close() }

type gatedUpload struct {
	reader      *bytes.Reader
	metadata    document.AuthorizedUploadMetadata
	release     chan struct{}
	releaseOnce sync.Once
	waitOnce    sync.Once
}

func newGatedUpload(source []byte, metadata document.AuthorizedUploadMetadata) *gatedUpload {
	return &gatedUpload{reader: bytes.NewReader(source), metadata: metadata, release: make(chan struct{})}
}

func (upload *gatedUpload) Read(value []byte) (int, error) {
	upload.waitOnce.Do(func() { <-upload.release })
	read, err := upload.reader.Read(value)
	if errors.Is(err, io.EOF) {
		return read, io.EOF
	}
	if err != nil {
		return read, fmt.Errorf("gated upload read: %w", err)
	}
	return read, nil
}

func (upload *gatedUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

func (upload *gatedUpload) Release() { upload.releaseOnce.Do(func() { close(upload.release) }) }

func (upload *gatedUpload) Close() error {
	upload.Release()
	return nil
}

func newBlockingUpload(metadata document.AuthorizedUploadMetadata) *blockingUpload {
	return &blockingUpload{metadata: metadata, started: make(chan struct{}), closed: make(chan struct{})}
}

func newObservedUpload(source []byte, metadata document.AuthorizedUploadMetadata) *observedUpload {
	return &observedUpload{reader: bytes.NewReader(source), metadata: metadata}
}

func (upload *observedUpload) Read(value []byte) (int, error) {
	upload.reads.Add(1)
	read, err := upload.reader.Read(value)
	if errors.Is(err, io.EOF) {
		return read, io.EOF
	}
	if err != nil {
		return read, fmt.Errorf("observed upload read: %w", err)
	}
	return read, nil
}

func (upload *observedUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

func (upload *observedUpload) Close() error {
	upload.closes.Add(1)
	return nil
}

func (resolver *cancelBlockingSecretResolver) ResolveSecret(ctx context.Context, _ string) (string, error) {
	resolver.once.Do(func() { close(resolver.started) })
	<-ctx.Done()
	return "", ctx.Err()
}

func (upload *blockingUpload) Read([]byte) (int, error) {
	upload.start.Do(func() { close(upload.started) })
	<-upload.closed
	return 0, errors.New("synthetic blocked reader closed")
}

func (upload *blockingUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

func (upload *blockingUpload) Close() error {
	upload.closes.Add(1)
	upload.close.Do(func() { close(upload.closed) })
	return nil
}

func uploadMetadata(source []byte) document.AuthorizedUploadMetadata {
	return document.AuthorizedUploadMetadata{
		Filename: "synthetic.bin", MediaFamily: "binary", MediaType: "application/octet-stream",
		ByteLength: int64(len(source)), SHA256: sha256Bytes(source),
		CapabilityRecordChecksum: strings.Repeat("a", 64), ProviderMetadataChecksum: strings.Repeat("b", 64),
		InputKind: document.RenditionInputOriginalFile,
	}
}

func oneTextInput(text string) []document.EmbeddingInput {
	return []document.EmbeddingInput{{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: text}}
}

func twoTextInputs() []document.EmbeddingInput {
	return []document.EmbeddingInput{
		{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha"},
		{Key: "second", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "beta"},
	}
}

func validResponseBody(manifest requestManifest) string {
	return responseBody(manifest, `[{"key":"first","index":0,"values":[1,2]},{"key":"second","index":1,"values":[3,4]}]`)
}

func responseBody(manifest requestManifest, vectors string) string {
	return fmt.Sprintf(`{"contract_version":%q,"descriptor_fingerprint":%q,"policy_fingerprint":%q,"request_checksum":%q,"vectors":%s}`,
		embeddingbridge.ContractVersion, manifest.DescriptorFingerprint, manifest.PolicyFingerprint, manifest.RequestChecksum, vectors)
}

func responseMediaType() string { return "application/vnd.docbank.embedding-result+json;version=1" }

func exactManifestChecksum(t *testing.T, manifest requestManifest) string {
	t.Helper()
	manifest.RequestChecksum = ""
	encoded, err := json.Marshal(manifest, json.Deterministic(true))
	require.NoError(t, err)
	return sha256Bytes(encoded)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value, json.Deterministic(true))
	require.NoError(t, err)
	return string(encoded)
}

func sha256Text(value string) string { return sha256Bytes([]byte(value)) }

func sha256Bytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
