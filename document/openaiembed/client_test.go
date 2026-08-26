package openaiembed

import (
	"context"
	_ "embed"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

//go:embed testdata/success-indexed.json
var successIndexedResponse []byte

//go:embed testdata/schema-drift.json
var schemaDriftResponse []byte

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type testSecrets map[string]string

func (secrets testSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	value, ok := secrets[name]
	if !ok {
		return "", errors.New("missing synthetic secret")
	}
	return value, nil
}

type capturedRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	EncodingFormat string   `json:"encoding_format"`
}

func TestEmbedAppliesExplicitModelInputProfilesAndRestoresResponseIndices(t *testing.T) {
	tests := []struct {
		name     string
		config   document.ModelInputContractConfig
		expected []string
	}{
		{name: "nomic", config: document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}, expected: []string{"search_document: passage", "search_query: question"}},
		{name: "e5", config: document.ModelInputContractConfig{Profile: document.ModelInputProfileE5}, expected: []string{"passage: passage", "query: question"}},
		{name: "bge-m3", config: document.ModelInputContractConfig{Profile: document.ModelInputProfileBGEM3}, expected: []string{"passage", "question"}},
		{name: "gte", config: document.ModelInputContractConfig{Profile: document.ModelInputProfileGTE}, expected: []string{"passage", "question"}},
		{name: "qwen3 prefix free", config: document.ModelInputContractConfig{Profile: document.ModelInputProfileQwen3}, expected: []string{"passage", "question"}},
		{name: "qwen3 instruction", config: document.ModelInputContractConfig{Profile: document.ModelInputProfileQwen3, QueryInstruction: "Retrieve supporting passages"}, expected: []string{"passage", "Instruct: Retrieve supporting passages\nQuery:question"}},
		{name: "query-only instruction", config: document.ModelInputContractConfig{Profile: document.ModelInputProfileQueryInstruction, QueryInstruction: "Find exact evidence"}, expected: []string{"passage", "Instruct: Find exact evidence\nQuery:question"}},
		{name: "complete custom", config: document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic/custom-v1", Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "doc[{{content}}]"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query[{{content}}]"}}, expected: []string{"doc[passage]", "query[question]"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			contract, err := document.NewModelInputContract(testCase.config)
			require.NoError(t, err)
			profile := testProfile(t, contract)
			var seen capturedRequest
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				require.Equal(t, http.MethodPost, request.Method)
				assert.Equal(t, "/v1/embeddings", request.URL.Path)
				assert.Empty(t, request.URL.RawQuery)
				assert.Equal(t, "application/json", request.Header.Get("Content-Type"))
				assert.Equal(t, "application/json", request.Header.Get("Accept"))
				require.NoError(t, json.UnmarshalRead(request.Body, &seen, json.RejectUnknownMembers(true)))
				return jsonResponse(request, http.StatusOK, successIndexedResponse), nil
			}))

			result, err := document.ExecuteEmbedding(t.Context(), client, testInputs(), testAuthorization(profile.Descriptor))
			require.NoError(t, err)
			assert.Equal(t, testCase.expected, seen.Input)
			assert.Equal(t, "synthetic-model", seen.Model)
			assert.Equal(t, "float", seen.EncodingFormat)
			require.Len(t, result.Vectors, 2)
			assert.Equal(t, "document-1", result.Vectors[0].Key)
			assert.Equal(t, []float32{1, 0, 0}, result.Vectors[0].Values)
			assert.Nil(t, result.Vectors[0].Index)
			assert.Equal(t, "query-1", result.Vectors[1].Key)
			assert.Equal(t, []float32{0, 1, 0}, result.Vectors[1].Values)
			assert.Nil(t, result.Vectors[1].Index)
		})
	}
}

func TestNewRequiresCanonicalProfileAndStableRevisionIdentity(t *testing.T) {
	contract := modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	base := testProfile(t, contract)

	for name, mutate := range map[string]func(Profile) Profile{
		"credentials in origin":         func(profile Profile) Profile { profile.Origin = "http://user@127.0.0.1:11434"; return profile },
		"query in origin":               func(profile Profile) Profile { profile.Origin += "?model=private"; return profile },
		"fragment in origin":            func(profile Profile) Profile { profile.Origin += "#fragment"; return profile },
		"non-root base path":            func(profile Profile) Profile { profile.Origin += "/api"; return profile },
		"unsupported scheme":            func(profile Profile) Profile { profile.Origin = "file:///tmp/provider"; return profile },
		"missing revision authority":    func(profile Profile) Profile { profile.DeploymentEpoch = ""; return profile },
		"two revision authorities":      func(profile Profile) Profile { profile.ProviderRevisionHeader = "X-Model-Revision"; return profile },
		"epoch differs from descriptor": func(profile Profile) Profile { profile.DeploymentEpoch = "epoch-other"; return profile },
		"non-canonical revision header": func(profile Profile) Profile {
			profile.DeploymentEpoch = ""
			profile.ProviderRevisionHeader = "x-model-revision"
			return profile
		},
		"unsafe revision header": func(profile Profile) Profile {
			profile.DeploymentEpoch = ""
			profile.ProviderRevisionHeader = "Authorization"
			return profile
		},
		"different explicit input contract": func(profile Profile) Profile {
			profile.ModelInput = modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileE5})
			return profile
		},
		"non-canonical descriptor": func(profile Profile) Profile {
			profile.Descriptor.Fingerprint = strings.Repeat("0", 64)
			return profile
		},
		"wrong trust boundary": func(profile Profile) Profile {
			profile.Descriptor = canonicalMutatedDescriptor(t, profile.Descriptor, func(descriptor *document.EmbeddingDescriptor) {
				descriptor.TrustBoundary = document.EmbeddingTrustHostedProvider
			})
			return profile
		},
		"direct-file capability": func(profile Profile) Profile {
			profile.Descriptor = canonicalMutatedDescriptor(t, profile.Descriptor, func(descriptor *document.EmbeddingDescriptor) {
				descriptor.InputKinds = append(descriptor.InputKinds, document.EmbeddingInputOriginalFile)
			})
			return profile
		},
		"missing text query": func(profile Profile) Profile {
			profile.Descriptor = canonicalMutatedDescriptor(t, profile.Descriptor, func(descriptor *document.EmbeddingDescriptor) { descriptor.SupportsTextQuery = false })
			return profile
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(mutate(base), nil, &http.Client{Transport: staticSuccessTransport()})
			require.Error(t, err)
		})
	}

	for _, unsupported := range []document.ModelInputProfile{
		document.ModelInputProfileOpenAICompatible, document.ModelInputProfileMistral,
	} {
		t.Run("unsupported input profile "+string(unsupported), func(t *testing.T) {
			profile := base
			profile.ModelInput = modelInput(t, document.ModelInputContractConfig{Profile: unsupported})
			profile.Descriptor = canonicalMutatedDescriptor(t, profile.Descriptor, func(descriptor *document.EmbeddingDescriptor) {
				descriptor.ModelInput = profile.ModelInput
				descriptor.CompatibilityID = profile.ModelInput.CompatibilityID
			})
			_, err := New(profile, nil, &http.Client{Transport: staticSuccessTransport()})
			require.ErrorContains(t, err, "model-input")
		})
	}

	t.Run("empty input profile", func(t *testing.T) {
		profile := base
		profile.ModelInput = modelInput(t, document.ModelInputContractConfig{})
		_, err := New(profile, nil, &http.Client{Transport: staticSuccessTransport()})
		require.ErrorContains(t, err, "model-input")
	})

	t.Run("voyage request modes", func(t *testing.T) {
		profile := base
		profile.ModelInput = modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileVoyage})
		profile.Descriptor = canonicalMutatedDescriptor(t, profile.Descriptor, func(descriptor *document.EmbeddingDescriptor) {
			descriptor.ModelInput = profile.ModelInput
			descriptor.CompatibilityID = profile.ModelInput.CompatibilityID
			descriptor.SupportedRequestModes = []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery}
		})
		_, err := New(profile, nil, &http.Client{Transport: staticSuccessTransport()})
		require.ErrorContains(t, err, "text-only")
	})

	_, err := New(base, nil, nil)
	require.ErrorContains(t, err, "HTTP client")
}

func TestProfilePolicyFingerprintCoversEndpointIdentityAndBounds(t *testing.T) {
	base := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	want, err := PolicyFingerprint(base)
	require.NoError(t, err)
	assert.Equal(t, want, base.Descriptor.PolicyFingerprint)

	mutations := map[string]func(*Profile){
		"origin": func(profile *Profile) { profile.Origin = "http://127.0.0.1:11435" },
		"deployment epoch": func(profile *Profile) {
			profile.DeploymentEpoch = "epoch-other"
			profile.Descriptor.ModelRevision = "epoch-other"
		},
		"credential binding": func(profile *Profile) { profile.SecretBinding = "other-secret" },
		"batch capacity":     func(profile *Profile) { profile.MaxBatchItems++ },
		"input capacity":     func(profile *Profile) { profile.MaxInputBytes++ },
		"request bound":      func(profile *Profile) { profile.MaxRequestBytes++ },
		"response bound":     func(profile *Profile) { profile.MaxResponseBytes++ },
		"timeout":            func(profile *Profile) { profile.RequestTimeout++ },
		"model":              func(profile *Profile) { profile.Descriptor.Model = "different-model" },
		"dimension":          func(profile *Profile) { profile.Descriptor.Dimension++ },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			fingerprint, err := PolicyFingerprint(changed)
			require.NoError(t, err)
			assert.NotEqual(t, want, fingerprint)
			changed.Descriptor = canonicalMutatedDescriptor(t, changed.Descriptor, func(*document.EmbeddingDescriptor) {})
			_, err = New(changed, nil, &http.Client{Transport: staticSuccessTransport()})
			require.ErrorContains(t, err, "policy fingerprint")
		})
	}
}

func TestProfilePolicyFingerprintCoversExactEgressAuthority(t *testing.T) {
	base := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	base.EgressPolicy = providerhttp.EgressPolicy{
		Scheme: "http", Host: "127.0.0.1", Port: 11434,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		ProxyMode:    providerhttp.ProxyDisabled,
	}
	want, err := PolicyFingerprint(base)
	require.NoError(t, err)
	changed := base
	changed.EgressPolicy.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	got, err := PolicyFingerprint(changed)
	require.NoError(t, err)
	assert.NotEqual(t, want, got)

	mismatched := base
	mismatched.EgressPolicy.Port++
	_, err = PolicyFingerprint(mismatched)
	require.ErrorContains(t, err, "origin")
}

func TestEmbedReturnsClassifiedProviderFailures(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	tests := []struct {
		name      string
		status    int
		want      error
		retry     string
		wantDelay time.Duration
	}{
		{name: "capacity", status: http.StatusRequestEntityTooLarge, want: ErrCapacityResponse},
		{name: "permanent", status: http.StatusBadRequest, want: ErrPermanentResponse},
		{name: "transient", status: http.StatusServiceUnavailable, want: ErrTransientResponse},
		{name: "retry after", status: http.StatusTooManyRequests, want: ErrTransientResponse, retry: "2", wantDelay: 2 * time.Second},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := jsonResponse(request, testCase.status, []byte(`{"error":"synthetic"}`))
				if testCase.retry != "" {
					response.Header.Set("Retry-After", testCase.retry)
				}
				return response, nil
			}))
			_, err := client.Embed(t.Context(), testInputs()[:1], testAuthorization(profile.Descriptor))
			require.ErrorIs(t, err, testCase.want)
			delay, set := RetryAfter(err)
			assert.Equal(t, testCase.retry != "", set)
			assert.Equal(t, testCase.wantDelay, delay)
		})
	}
}

func TestEmbedEnforcesProfileAuthorizationAndTextOnlyBoundsBeforeNetwork(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	profile.MaxBatchItems = 2
	profile.MaxInputBytes = 64
	profile.MaxRequestBytes = 256
	profile.MaxResponseBytes = 4096
	profile.Descriptor = descriptorFor(t, profile)
	var requests atomic.Int64
	client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		return jsonResponse(request, http.StatusOK, successIndexedResponse), nil
	}))

	tests := []struct {
		name          string
		inputs        []document.EmbeddingInput
		authorization document.EmbeddingAuthorization
	}{
		{name: "batch authority above profile", inputs: testInputs(), authorization: mutateAuthorization(testAuthorization(profile.Descriptor), func(value *document.EmbeddingAuthorization) { value.MaxBatchItems = 3 })},
		{name: "input authority above profile", inputs: testInputs(), authorization: mutateAuthorization(testAuthorization(profile.Descriptor), func(value *document.EmbeddingAuthorization) { value.MaxInputBytes = 65 })},
		{name: "response authority above profile", inputs: testInputs(), authorization: mutateAuthorization(testAuthorization(profile.Descriptor), func(value *document.EmbeddingAuthorization) { value.MaxResponseBytes = 4097 })},
		{name: "direct file", inputs: []document.EmbeddingInput{{Key: "file", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile}}, authorization: testAuthorization(profile.Descriptor)},
		{name: "too many items", inputs: append(testInputs(), document.EmbeddingInput{Key: "extra", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "extra"}), authorization: testAuthorization(profile.Descriptor)},
		{name: "rendered text too large", inputs: []document.EmbeddingInput{{Key: "large", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: strings.Repeat("x", 64)}}, authorization: testAuthorization(profile.Descriptor)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := client.Embed(t.Context(), testCase.inputs, testCase.authorization)
			require.Error(t, err)
		})
	}
	assert.Zero(t, requests.Load())
}

func TestEmbedUsesOptionalNamedBearerSecretWithoutAmbientCookies(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileGTE}))
	profile.SecretBinding = "local-embedding-key"
	profile.Descriptor = descriptorFor(t, profile)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	var seenAuthorization string
	base := &http.Client{Jar: jar, Timeout: time.Hour, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	base.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seenAuthorization = request.Header.Get("Authorization")
		return jsonResponse(request, http.StatusOK, successIndexedResponse), nil
	})
	client, err := New(profile, testSecrets{"local-embedding-key": "synthetic-secret"}, base)
	require.NoError(t, err)
	assert.Nil(t, client.http.Jar)
	assert.Zero(t, client.http.Timeout)
	_, err = client.Embed(t.Context(), testInputs(), testAuthorization(profile.Descriptor))
	require.NoError(t, err)
	assert.Equal(t, "Bearer synthetic-secret", seenAuthorization)

	_, err = New(profile, nil, base)
	require.ErrorContains(t, err, "resolver")
	withoutBinding := profile
	withoutBinding.SecretBinding = ""
	withoutBinding.Descriptor = descriptorFor(t, withoutBinding)
	_, err = New(withoutBinding, testSecrets{}, base)
	require.ErrorContains(t, err, "binding")

	for name, secret := range map[string]string{
		"line break":        "bad\r\nvalue",
		"space":             "bad value",
		"tab":               "bad\tvalue",
		"non-ASCII":         "bad-välue",
		"unsupported colon": "bad:value",
		"padding in middle": "bad=value",
		"padding only":      "==",
	} {
		t.Run(name, func(t *testing.T) {
			client = newTestClient(t, profile, testSecrets{"local-embedding-key": secret}, base.Transport)
			_, err = client.Embed(t.Context(), testInputs(), testAuthorization(profile.Descriptor))
			require.ErrorContains(t, err, "credential")
			assert.NotContains(t, err.Error(), "bad")
		})
	}

	for name, secret := range map[string]string{
		"minimal":                   "a",
		"full alphabet and padding": "aZ09-._~+/==",
	} {
		t.Run(name, func(t *testing.T) {
			client = newTestClient(t, profile, testSecrets{"local-embedding-key": secret}, base.Transport)
			_, err = client.Embed(t.Context(), testInputs(), testAuthorization(profile.Descriptor))
			require.NoError(t, err)
		})
	}
}

func TestEmbedAcceptsJSONUTF8ContentType(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileGTE}))
	for _, contentType := range []string{"application/json; charset=utf-8", "application/json; charset=Utf-8"} {
		t.Run(contentType, func(t *testing.T) {
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := jsonResponse(request, http.StatusOK, singleVectorResponse("synthetic-model", []float32{1, 0, 0}))
				response.Header.Set("Content-Type", contentType)
				return response, nil
			}))
			_, err := client.Embed(t.Context(), testInputs()[:1], testAuthorization(profile.Descriptor))
			require.NoError(t, err)
		})
	}

	for _, contentType := range []string{
		"application/json; charset=iso-8859-1",
		"application/json; profile=embedding",
		"application/json; charset=utf-8; profile=embedding",
	} {
		t.Run(contentType, func(t *testing.T) {
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := jsonResponse(request, http.StatusOK, singleVectorResponse("synthetic-model", []float32{1, 0, 0}))
				response.Header.Set("Content-Type", contentType)
				return response, nil
			}))
			_, err := client.Embed(t.Context(), testInputs()[:1], testAuthorization(profile.Descriptor))
			require.ErrorContains(t, err, "content type")
		})
	}
}

func TestEmbedRequiresModelAndProviderRevisionEcho(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileBGEM3}))
	profile.DeploymentEpoch = ""
	profile.ProviderRevisionHeader = "X-Model-Revision"
	profile.Descriptor = descriptorFor(t, profile)

	for name, testCase := range map[string]struct {
		body      []byte
		revisions []string
	}{
		"missing revision":   {body: singleVectorResponse("synthetic-model", []float32{1, 0, 0})},
		"wrong revision":     {body: singleVectorResponse("synthetic-model", []float32{1, 0, 0}), revisions: []string{"revision-other"}},
		"multiple revisions": {body: singleVectorResponse("synthetic-model", []float32{1, 0, 0}), revisions: []string{"epoch-2026-08-26", "epoch-2026-08-26"}},
		"wrong model":        {body: singleVectorResponse("alias-drift", []float32{1, 0, 0}), revisions: []string{"epoch-2026-08-26"}},
	} {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := jsonResponse(request, http.StatusOK, testCase.body)
				if testCase.revisions != nil {
					response.Header["X-Model-Revision"] = testCase.revisions
				}
				return response, nil
			}))
			_, err := client.Embed(t.Context(), testInputs()[:1], testAuthorization(profile.Descriptor))
			require.Error(t, err)
		})
	}

	client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := jsonResponse(request, http.StatusOK, singleVectorResponse("synthetic-model", []float32{1, 0, 0}))
		response.Header.Set("X-Model-Revision", "  epoch-2026-08-26  ")
		return response, nil
	}))
	_, err := client.Embed(t.Context(), testInputs()[:1], testAuthorization(profile.Descriptor))
	require.NoError(t, err)
}

func TestEmbedRejectsSchemaVectorAndTransportDriftWithoutLeakingBodies(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	one := testInputs()[:1]
	tests := []struct {
		name        string
		status      int
		contentType string
		body        []byte
	}{
		{name: "unknown response member", status: http.StatusOK, contentType: "application/json", body: schemaDriftResponse},
		{name: "wrong media type", status: http.StatusOK, contentType: "text/plain", body: singleVectorResponse("synthetic-model", []float32{1, 0, 0})},
		{name: "provider error body", status: http.StatusTooManyRequests, contentType: "application/json", body: []byte(`{"error":"private request text and vector [9,8,7]"}`)},
		{name: "wrong list object", status: http.StatusOK, contentType: "application/json", body: []byte(`{"object":"embedding","data":[{"object":"embedding","embedding":[1,0,0],"index":0}],"model":"synthetic-model"}`)},
		{name: "wrong item object", status: http.StatusOK, contentType: "application/json", body: []byte(`{"object":"list","data":[{"object":"vector","embedding":[1,0,0],"index":0}],"model":"synthetic-model"}`)},
		{name: "duplicate index", status: http.StatusOK, contentType: "application/json", body: []byte(`{"object":"list","data":[{"object":"embedding","embedding":[1,0,0],"index":0},{"object":"embedding","embedding":[0,1,0],"index":0}],"model":"synthetic-model"}`)},
		{name: "missing vector", status: http.StatusOK, contentType: "application/json", body: []byte(`{"object":"list","data":[],"model":"synthetic-model"}`)},
		{name: "index outside batch", status: http.StatusOK, contentType: "application/json", body: []byte(`{"object":"list","data":[{"object":"embedding","embedding":[1,0,0],"index":2}],"model":"synthetic-model"}`)},
		{name: "dimension drift", status: http.StatusOK, contentType: "application/json", body: singleVectorResponse("synthetic-model", []float32{1, 0})},
		{name: "non-finite scalar", status: http.StatusOK, contentType: "application/json", body: []byte(`{"object":"list","data":[{"object":"embedding","embedding":[1e1000,0,0],"index":0}],"model":"synthetic-model"}`)},
		{name: "zero vector", status: http.StatusOK, contentType: "application/json", body: singleVectorResponse("synthetic-model", []float32{0, 0, 0})},
		{name: "normalization drift", status: http.StatusOK, contentType: "application/json", body: singleVectorResponse("synthetic-model", []float32{2, 0, 0})},
		{name: "negative usage", status: http.StatusOK, contentType: "application/json", body: []byte(`{"object":"list","data":[{"object":"embedding","embedding":[1,0,0],"index":0}],"model":"synthetic-model","usage":{"prompt_tokens":-1,"total_tokens":-1}}`)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := jsonResponse(request, testCase.status, testCase.body)
				response.Header.Set("Content-Type", testCase.contentType)
				return response, nil
			}))
			inputs := one
			if testCase.name == "duplicate index" {
				inputs = testInputs()
			}
			_, err := client.Embed(t.Context(), inputs, testAuthorization(profile.Descriptor))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private request text")
			assert.NotContains(t, err.Error(), "[9,8,7]")
		})
	}

	bounded := profile
	bounded.MaxResponseBytes = 64
	bounded.Descriptor = descriptorFor(t, bounded)
	client := newTestClient(t, bounded, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, http.StatusOK, []byte(strings.Repeat("x", 65))), nil
	}))
	_, err := client.Embed(t.Context(), one, mutateAuthorization(testAuthorization(bounded.Descriptor), func(value *document.EmbeddingAuthorization) { value.MaxResponseBytes = 64 }))
	require.ErrorContains(t, err, "response byte")
}

func TestEmbedRefusesRedirectsAndHonorsCancellation(t *testing.T) {
	contract := modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileGTE})
	var redirected atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/elsewhere" {
			redirected.Add(1)
		}
		http.Redirect(response, request, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	profile := testProfile(t, contract)
	profile.Origin = server.URL
	profile.Descriptor = descriptorFor(t, profile)
	client, err := New(profile, nil, server.Client())
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), testInputs(), testAuthorization(profile.Descriptor))
	require.ErrorContains(t, err, "redirect")
	assert.Zero(t, redirected.Load())

	started := make(chan struct{})
	canceledProfile := testProfile(t, contract)
	canceledClient := newTestClient(t, canceledProfile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, embedErr := canceledClient.Embed(ctx, testInputs(), testAuthorization(canceledProfile.Descriptor))
		done <- embedErr
	}()
	<-started
	cancel()
	err = <-done
	require.ErrorIs(t, err, context.Canceled)
}

func TestEmbedBoundsRequestBodyAndPreservesDescriptorSnapshot(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileE5}))
	profile.MaxRequestBytes = 128
	profile.Descriptor = descriptorFor(t, profile)
	var requests atomic.Int64
	client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		return jsonResponse(request, http.StatusOK, singleVectorResponse("synthetic-model", []float32{1, 0, 0})), nil
	}))
	input := []document.EmbeddingInput{{Key: "large", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: strings.Repeat("x", 100)}}
	authorization := mutateAuthorization(testAuthorization(profile.Descriptor), func(value *document.EmbeddingAuthorization) { value.MaxInputBytes = 200 })
	_, err := client.Embed(t.Context(), input, authorization)
	require.ErrorIs(t, err, ErrCapacityResponse)
	require.ErrorContains(t, err, "request byte")
	assert.Zero(t, requests.Load())

	descriptor := client.Descriptor()
	descriptor.InputKinds[0] = document.EmbeddingInputOriginalFile
	descriptor.SupportedRequestModes[0] = document.ModelInputModeQuery
	assert.Equal(t, []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, client.Descriptor().InputKinds)
	assert.Equal(t, []document.ModelInputMode{document.ModelInputModeText}, client.Descriptor().SupportedRequestModes)
}

func testProfile(t *testing.T, contract document.ModelInputContract) Profile {
	t.Helper()
	profile := Profile{
		Origin: "http://127.0.0.1:11434", ModelInput: contract, DeploymentEpoch: "epoch-2026-08-26",
		RequestTimeout: time.Second, MaxBatchItems: 8, MaxInputBytes: 4096,
		MaxRequestBytes: 8192, MaxResponseBytes: 16384,
		Descriptor: document.EmbeddingDescriptor{
			ID: ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			TrustBoundary: document.EmbeddingTrustOperatorNetwork, Model: "synthetic-model",
			ModelRevision: "epoch-2026-08-26", Dimension: 3, Metric: document.VectorMetricCosine,
			Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: ScalarEncodingFloat32,
			DocumentFormatter: DocumentFormatterV1, QueryFormatter: QueryFormatterV1,
			InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk},
			CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
		},
	}
	profile.Descriptor = descriptorFor(t, profile)
	return profile
}

func descriptorFor(t *testing.T, profile Profile) document.EmbeddingDescriptor {
	t.Helper()
	profile.Descriptor.PolicyFingerprint = ""
	profile.Descriptor.Fingerprint = ""
	fingerprint, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = fingerprint
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	return descriptor
}

func canonicalMutatedDescriptor(t *testing.T, descriptor document.EmbeddingDescriptor, mutate func(*document.EmbeddingDescriptor)) document.EmbeddingDescriptor {
	t.Helper()
	descriptor.Fingerprint = ""
	mutate(&descriptor)
	canonical, err := document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
	return canonical
}

func modelInput(t *testing.T, config document.ModelInputContractConfig) document.ModelInputContract {
	t.Helper()
	contract, err := document.NewModelInputContract(config)
	require.NoError(t, err)
	return contract
}

func testInputs() []document.EmbeddingInput {
	return []document.EmbeddingInput{
		{Key: "document-1", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"},
		{Key: "query-1", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "question"},
	}
}

func testAuthorization(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 2, MaxInputBytes: 64, MaxResponseBytes: 4096}
}

func mutateAuthorization(value document.EmbeddingAuthorization, mutate func(*document.EmbeddingAuthorization)) document.EmbeddingAuthorization {
	mutate(&value)
	return value
}

func newTestClient(t *testing.T, profile Profile, secrets SecretResolver, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := New(profile, secrets, &http.Client{Transport: transport})
	require.NoError(t, err)
	return client
}

func staticSuccessTransport() http.RoundTripper {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, http.StatusOK, successIndexedResponse), nil
	})
}

func jsonResponse(request *http.Request, status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}
}

func singleVectorResponse(model string, vector []float32) []byte {
	body, err := json.Marshal(map[string]any{"object": "list", "data": []map[string]any{{"object": "embedding", "embedding": vector, "index": 0}}, "model": model})
	if err != nil {
		panic(err)
	}
	return body
}
