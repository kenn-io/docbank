package voyage_test

import (
	"context"
	"crypto/x509"
	json "encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
)

type embeddingResolver struct {
	address     netip.Addr
	certificate *x509.Certificate
}

func (resolver embeddingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{resolver.address}, nil
}

func voyageFixture(t *testing.T, handler http.Handler) (string, providerhttp.EgressPolicy, providerhttp.Resolver) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	_, portText, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	return server.URL + "/v1", providerhttp.EgressPolicy{
		Scheme: "https", Host: parsed.Hostname(), Port: uint16(port),
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		ProxyMode:    providerhttp.ProxyDisabled,
	}, embeddingResolver{address: netip.MustParseAddr("127.0.0.1"), certificate: server.Certificate()}
}

func TestContextualEmbeddingUsesDocumentedWireShape(t *testing.T) {
	var request map[string]any
	endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		assert.NoError(t, json.UnmarshalRead(incoming.Body, &request))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(contextualOfficialBody(t, []string{"document envelope: first", "document envelope: second"}, []int{1, 0}))
	}))
	profile := voyageTextProfile(t, voyage.EmbeddingModeContextual)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile = refingerprintVoyageProfile(t, profile)
	provider, err := newVoyageEmbeddingTestProvider(t, profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
	require.NoError(t, err)
	inputs := []document.EmbeddingInput{
		{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "first"},
		{Key: "second", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "second"},
	}
	result, err := provider.Embed(t.Context(), inputs, voyageAuthorization(profile.Descriptor))
	require.NoError(t, err)
	assert.NotContains(t, request, "truncation")
	assert.Equal(t, []any{[]any{"document envelope: first", "document envelope: second"}}, request["inputs"])
	assert.InDelta(t, float32(1), result.Vectors[0].Values[1], 0)
	assert.InDelta(t, float32(1), result.Vectors[1].Values[2], 0)
}

func TestContextualEmbeddingStrictlyRejectsDocumentedShapeDrift(t *testing.T) {
	texts := []string{"document envelope: first", "document envelope: second"}
	valid := contextualOfficialBody(t, texts, []int{0, 1})
	tests := []struct {
		name string
		body []byte
	}{
		{"unknown", []byte(strings.Replace(string(valid), `"chunker_version":`, `"unknown":"PRIVATE_RAW_BODY","chunker_version":`, 1))},
		{"duplicate", []byte(strings.Replace(string(valid), `"model":`, `"model":"duplicate","model":`, 1))},
		{"model drift", []byte(strings.Replace(string(valid), voyage.ContextualModel, "voyage-context-drift", 1))},
		{"chunker drift", []byte(strings.Replace(string(valid), `"1.0.0"`, `"2.0.0"`, 1))},
		{"text drift", []byte(strings.Replace(string(valid), texts[0], "PRIVATE_RETURNED_TEXT", 1))},
		{"partial indices", contextualOfficialBody(t, texts[:1], []int{0})},
		{"duplicate index", contextualOfficialBody(t, []string{texts[0], texts[0]}, []int{0, 0})},
	}
	inputs := []document.EmbeddingInput{
		{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "first"},
		{Key: "second", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "second"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write(test.body)
			}))
			profile := voyageTextProfile(t, voyage.EmbeddingModeContextual)
			profile.Endpoint, profile.EgressPolicy = endpoint, egress
			profile = refingerprintVoyageProfile(t, profile)
			provider, err := newVoyageEmbeddingTestProvider(t, profile, embeddingSecrets{"credential:voyage": "PRIVATE_SECRET"}, resolver)
			require.NoError(t, err)
			_, err = provider.Embed(t.Context(), inputs, voyageAuthorization(profile.Descriptor))
			require.ErrorIs(t, err, voyage.ErrMalformedResponse)
			assert.NotContains(t, err.Error(), "PRIVATE")
		})
	}
}

func TestHostedVoyageAliasIsAlwaysExportOnly(t *testing.T) {
	endpoint, egress, resolver := voyageFixture(t, http.NotFoundHandler())
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile.Descriptor.SupportsTextQuery = true
	_, err := newVoyageEmbeddingTestProvider(t, profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
	require.ErrorContains(t, err, "export-only")
}

func TestVoyageRoleContractMustMatchNativeModes(t *testing.T) {
	endpoint, egress, resolver := voyageFixture(t, http.NotFoundHandler())
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile.Descriptor.SupportedRequestModes = []document.ModelInputMode{document.ModelInputModeDocument}
	_, err := newVoyageEmbeddingTestProvider(t, profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
	require.ErrorContains(t, err, "native role")
}

func contextualOfficialBody(t *testing.T, texts []string, indices []int) []byte {
	t.Helper()
	items := make([]map[string]any, len(indices))
	for position, index := range indices {
		items[position] = map[string]any{"embedding": unitEmbedding(index + 1), "index": index, "text": texts[index]}
	}
	body, err := json.Marshal(map[string]any{
		"data": []any{map[string]any{"data": items, "index": 0}}, "model": voyage.ContextualModel,
		"usage": map[string]any{"total_tokens": 2}, "chunker_version": "1.0.0",
	})
	require.NoError(t, err)
	return body
}

func refingerprintVoyageProfile(t *testing.T, profile voyage.EmbeddingProfile) voyage.EmbeddingProfile {
	t.Helper()
	profile.Descriptor.PolicyFingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
	profile.Descriptor.Fingerprint = ""
	profile.Descriptor, _ = document.NewEmbeddingDescriptor(profile.Descriptor)
	fingerprint, err := voyage.EmbeddingPolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = fingerprint
	profile.Descriptor.Fingerprint = ""
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	return profile
}

func TestHostedVoyageEmbeddingRequiresHTTPS(t *testing.T) {
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	profile.Endpoint = "http://api.voyageai.com/v1"
	profile.EgressPolicy.Scheme, profile.EgressPolicy.Port = "http", 80
	_, err := voyage.EmbeddingPolicyFingerprint(profile)
	require.ErrorContains(t, err, "HTTPS")
	_, err = voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "secret"}, failingEmbeddingResolver{})
	require.ErrorContains(t, err, "HTTPS")
}

func TestDirectFileEmbeddingRejectsNativeRoleMismatch(t *testing.T) {
	policy := testPolicy(t)
	manifest, err := voyagetest.SyntheticManifest(policy)
	require.NoError(t, err)
	profile := voyageDirectFileProfile(t, policy, manifest)
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileMistral})
	require.NoError(t, err)
	profile.ModelInput, profile.Descriptor.ModelInput = contract, contract
	profile.Descriptor.CompatibilityID = contract.CompatibilityID
	profile.Descriptor.SupportedRequestModes = []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeText}
	_, err = voyage.EmbeddingPolicyFingerprint(profile)
	require.ErrorContains(t, err, "native role")
}

func TestVoyageEmbeddingRetriesMalformedResponseOnce(t *testing.T) {
	for _, mode := range []voyage.EmbeddingMode{voyage.EmbeddingModeText, voyage.EmbeddingModeContextual} {
		for _, testCase := range []struct {
			name            string
			recoverResponse bool
			attempts        int
			wantRequests    int32
		}{
			{"recovered", true, 3, 2}, {"malformed_twice", false, 3, 2}, {"attempt_limit", true, 1, 1},
		} {
			t.Run(fmt.Sprintf("%s/%s", mode, testCase.name), func(t *testing.T) {
				var calls atomic.Int32
				endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					count := calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if count == 1 || !testCase.recoverResponse {
						_, _ = w.Write([]byte(`{"data":`))
						return
					}
					if mode == voyage.EmbeddingModeContextual {
						_, _ = w.Write(contextualOfficialBody(t, []string{"document envelope: passage"}, []int{0}))
					} else {
						_, _ = w.Write(voyageTextBody(t, voyage.TextModel, []int{0}, []int{1}))
					}
				}))
				profile := voyageTextProfile(t, mode)
				profile.Endpoint, profile.EgressPolicy = endpoint, egress
				profile.MaxRetries, profile.RetryBaseDelay = testCase.attempts, time.Millisecond
				profile = refingerprintVoyageProfile(t, profile)
				provider, err := newVoyageEmbeddingTestProvider(t, profile, embeddingSecrets{"credential:voyage": "secret"}, resolver)
				require.NoError(t, err)
				result, err := document.ExecuteEmbedding(t.Context(), provider, []document.EmbeddingInput{{Key: "chunk", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"}}, voyageAuthorization(profile.Descriptor))
				require.Equal(t, testCase.wantRequests, calls.Load())
				if testCase.recoverResponse && testCase.attempts > 1 {
					require.NoError(t, err)
					require.Len(t, result.Vectors, 1)
					assert.Equal(t, "chunk", result.Vectors[0].Key)
				} else {
					require.ErrorIs(t, err, voyage.ErrMalformedResponse)
					assert.Equal(t, int(testCase.wantRequests)-1, voyage.MetricsFromError(err).Retries)
				}
			})
		}
	}
}

type blockingEmbeddingUpload struct {
	*io.PipeReader

	started chan struct{}
	once    sync.Once
}

func (upload *blockingEmbeddingUpload) Read(p []byte) (int, error) {
	upload.once.Do(func() { close(upload.started) })
	return upload.PipeReader.Read(p)
}
func (upload *blockingEmbeddingUpload) Metadata() document.AuthorizedUploadMetadata {
	return document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/png", ByteLength: 5, SHA256: strings.Repeat("a", 64), CapabilityRecordChecksum: strings.Repeat("b", 64), ProviderMetadataChecksum: strings.Repeat("c", 64), InputKind: document.RenditionInputOriginalFile}
}

func TestDirectFileEmbeddingCancellationInterruptsUpload(t *testing.T) {
	for _, core := range []bool{false, true} {
		t.Run(fmt.Sprintf("core_%t", core), func(t *testing.T) {
			policy := testPolicy(t)
			manifest, err := voyagetest.SyntheticManifest(policy)
			require.NoError(t, err)
			profile := voyageDirectFileProfile(t, policy, manifest)
			provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "secret"}, failingEmbeddingResolver{})
			require.NoError(t, err)
			reader, writer := io.Pipe()
			defer func() { _ = reader.Close() }()
			defer func() { _ = writer.Close() }()
			source := &blockingEmbeddingUpload{PipeReader: reader, started: make(chan struct{})}
			inputs := []document.EmbeddingInput{{Key: "file", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: source}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if core {
					_, err = document.ExecuteEmbedding(ctx, provider, inputs, voyageAuthorization(profile.Descriptor))
				} else {
					_, err = provider.Embed(ctx, inputs, voyageAuthorization(profile.Descriptor))
				}
				done <- err
			}()
			<-source.started
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				_ = reader.Close()
				<-done
				t.Fatal("embedding did not interrupt its blocked upload after cancellation")
			}
		})
	}
}

func newVoyageEmbeddingTestProvider(t *testing.T, profile voyage.EmbeddingProfile, secrets voyage.SecretResolver, resolver providerhttp.Resolver) (*voyage.EmbeddingClient, error) {
	t.Helper()
	provider, err := voyage.NewEmbeddingProvider(profile, secrets, resolver)
	if err != nil {
		return nil, err
	}
	if fixture, ok := resolver.(embeddingResolver); ok && fixture.certificate != nil {
		voyage.TrustEmbeddingTestCertificate(t, provider, fixture.certificate)
	}
	return provider, nil
}

func TestVoyageEmbeddingAttemptTimeouts(t *testing.T) {
	for _, stage := range []string{"credentials", "headers", "body"} {
		for _, outcome := range []string{"recover", "exhaust", "caller_cancel"} {
			t.Run(stage+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var requests atomic.Int32
				var resolutions int
				response := voyageTextBody(t, voyage.TextModel, []int{0}, []int{1})
				endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					count := requests.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					if stage != "credentials" && (outcome != "recover" || count < 3) {
						if stage == "body" {
							w.WriteHeader(http.StatusOK)
							assert.NoError(t, http.NewResponseController(w).Flush())
						}
						if outcome == "caller_cancel" {
							cancel()
						}
						select {
						case <-r.Context().Done():
						case <-time.After(2 * time.Second):
						}
						return
					}
					_, _ = w.Write(response)
				}))
				secrets := embeddingSecretFunc(func(attemptCtx context.Context, _ string) (string, error) {
					resolutions++
					if stage == "credentials" && (outcome != "recover" || resolutions < 3) {
						if outcome == "caller_cancel" {
							cancel()
						}
						<-attemptCtx.Done()
						return "", attemptCtx.Err()
					}
					return "synthetic-secret", nil
				})
				profile := voyageTextProfile(t, voyage.EmbeddingModeText)
				profile.Endpoint, profile.EgressPolicy = endpoint, egress
				profile.RequestTimeout, profile.MaxRetries = 100*time.Millisecond, 3
				profile.RetryBaseDelay = time.Nanosecond
				profile = refingerprintVoyageProfile(t, profile)
				provider, err := newVoyageEmbeddingTestProvider(t, profile, secrets, resolver)
				require.NoError(t, err)
				result, err := provider.Embed(ctx, []document.EmbeddingInput{{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "first"}}, voyageAuthorization(profile.Descriptor))
				switch outcome {
				case "recover":
					require.NoError(t, err)
					require.Len(t, result.Vectors, 1)
					require.Equal(t, 3, resolutions)
					require.NoError(t, ctx.Err())
				case "exhaust":
					require.ErrorIs(t, err, voyage.ErrTransientResponse)
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.NoError(t, ctx.Err())
					require.Equal(t, 3, resolutions)
					require.Equal(t, 2, voyage.MetricsFromError(err).Retries)
				case "caller_cancel":
					require.ErrorIs(t, err, context.Canceled)
					require.NotErrorIs(t, err, voyage.ErrTransientResponse)
					require.Equal(t, 1, resolutions)
					require.Zero(t, voyage.MetricsFromError(err).Retries)
				}
				wantRequests := resolutions
				if stage == "credentials" {
					wantRequests = 0
					if outcome == "recover" {
						wantRequests = 1
					}
				}
				require.EqualValues(t, wantRequests, requests.Load())
				if err != nil {
					require.Equal(t, wantRequests, voyage.MetricsFromError(err).Requests)
				}
			})
		}
	}
}

func TestVoyageEmbeddingCapacityCeilings(t *testing.T) {
	for _, field := range []string{"input", "request", "response", "batch"} {
		for _, excess := range []int64{0, 1, math.MaxInt64} {
			t.Run(fmt.Sprintf("%s/%d", field, excess), func(t *testing.T) {
				profile := voyageTextProfile(t, voyage.EmbeddingModeText)
				limit := int64(1 << 30)
				if field == "batch" {
					limit = 1000
				}
				value := excess
				if excess != math.MaxInt64 {
					value += limit
				}
				switch field {
				case "input":
					profile.MaxInputBytes = value
				case "request":
					profile.MaxRequestBytes = value
				case "response":
					profile.MaxResponseBytes = value
				case "batch":
					profile.MaxBatchItems = int(value)
				}
				_, err := voyage.EmbeddingPolicyFingerprint(profile)
				if excess == 0 {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}
