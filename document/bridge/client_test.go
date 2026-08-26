package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

type testUpload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
}

func (upload *testUpload) Close() error                                { return nil }
func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

type testSecrets map[string]string

func (secrets testSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	value, ok := secrets[name]
	if !ok {
		return "", errors.New("missing test secret")
	}
	return value, nil
}

type secretResolverFunc func(context.Context, string) (string, error)

func (resolver secretResolverFunc) ResolveSecret(ctx context.Context, name string) (string, error) {
	return resolver(ctx, name)
}

//go:embed testdata/unknown-major.json
var unknownMajorResponse []byte

func TestBridgeContractSynchronousCompletion(t *testing.T) {
	fixture := newBridgeFixture(t)
	var idempotency string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !assert.Equal(t, jobsPath, request.URL.Path) ||
			!assert.Equal(t, http.MethodPost, request.Method) ||
			!assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization")) {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		idempotency = request.Header.Get("Idempotency-Key")
		assert.Len(t, idempotency, 64)
		assertMultipartRequest(t, request, fixture.authorization, fixture.source)
		writeBridgeJSON(t, response, http.StatusOK,
			completedEnvelope(t, fixture, "job-sync", nil))
	}))
	t.Cleanup(server.Close)

	client := newTestBridgeClient(t, server.URL, fixture.descriptor, testSecrets{"bridge-api": "synthetic-secret"})
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, "synthetic bridge evidence", result.Evidence.Units[0].Text)
	assert.Equal(t, "synthetic provider markdown\n", string(result.ProviderMarkdown))
	assert.Equal(t, "job-sync", result.Receipt.OperationID)
	assert.NotEmpty(t, idempotency)
}

func TestBridgeProfileCeilingsRejectAuthorizationBeforeEgress(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *bridgeFixture)
		ceiling func(*Profile)
	}{
		{
			name: "source bytes",
			ceiling: func(profile *Profile) {
				profile.MaxSourceBytes = int64(len([]byte("synthetic bridge source")) - 1)
			},
		},
		{
			name: "provider Markdown",
			ceiling: func(profile *Profile) {
				profile.MaxProviderMarkdownBytes = 4095
			},
		},
		{
			name: "artifact bytes",
			prepare: func(t *testing.T, fixture *bridgeFixture) {
				t.Helper()
				*fixture = fixture.withStructuredArtifact(t)
			},
			ceiling: func(profile *Profile) {
				profile.MaxArtifactBytes = 4095
			},
		},
		{
			name: "artifact count",
			prepare: func(t *testing.T, fixture *bridgeFixture) {
				t.Helper()
				*fixture = fixture.withArtifactAuthorization(t,
					[]document.EvidenceArtifactRole{
						document.EvidenceArtifactImage, document.EvidenceArtifactStructured,
					}, 2)
			},
			ceiling: func(profile *Profile) {
				profile.MaxArtifacts = 1
			},
		},
		{
			name: "total result bytes",
			ceiling: func(profile *Profile) {
				profile.MaxTotalResultBytes = 16383
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBridgeFixture(t)
			if test.prepare != nil {
				test.prepare(t, &fixture)
			}
			var requests atomic.Int64
			profile := testBridgeProfile("https://bridge.invalid", fixture.descriptor, nil)
			test.ceiling(&profile)
			client, err := New(profile, nil, &http.Client{Transport: roundTripFunc(
				func(*http.Request) (*http.Response, error) {
					requests.Add(1)
					return nil, errors.New("ceiling violation reached egress")
				},
			)})
			require.NoError(t, err)

			_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)

			assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
			assert.Zero(t, requests.Load())
		})
	}
}

func TestBridgeProfileCeilingsAllowResultWithinEveryLimit(t *testing.T) {
	fixture := newBridgeFixture(t).withStructuredArtifact(t)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeBridgeJSON(t, response, http.StatusOK, completedEnvelope(t, fixture,
			"job-within-ceilings", []map[string]any{
				inlineArtifact(document.EvidenceArtifactStructured, fixture.artifact),
			}))
	}))
	t.Cleanup(server.Close)
	profile := testBridgeProfile(server.URL, fixture.descriptor, nil)
	profile.MaxSourceBytes = fixture.metadata.ByteLength
	profile.MaxProviderMarkdownBytes = fixture.authorization.MaxProviderMarkdownBytes
	profile.MaxArtifactBytes = fixture.authorization.MaxArtifactBytes
	profile.MaxArtifacts = fixture.authorization.MaxArtifacts
	profile.MaxTotalResultBytes = fixture.authorization.MaxTotalResultBytes
	profile.MaxEvidenceUnits = 1
	client, err := New(profile, nil, http.DefaultClient)
	require.NoError(t, err)

	result, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)

	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 1)
	require.Len(t, result.Artifacts, 1)
	assert.Equal(t, fixture.artifact, result.Artifacts[0].Payload)
}

func TestBridgeProfileRejectsInvalidCeilings(t *testing.T) {
	fixture := newBridgeFixture(t)
	for _, mutate := range []func(*Profile){
		func(profile *Profile) { profile.MaxSourceBytes = -1 },
		func(profile *Profile) { profile.MaxProviderMarkdownBytes = -1 },
		func(profile *Profile) { profile.MaxArtifactBytes = -1 },
		func(profile *Profile) { profile.MaxArtifacts = -1 },
		func(profile *Profile) { profile.MaxTotalResultBytes = -1 },
		func(profile *Profile) { profile.MaxEvidenceUnits = -1 },
	} {
		profile := testBridgeProfile("https://bridge.invalid", fixture.descriptor, nil)
		mutate(&profile)
		_, err := New(profile, nil, http.DefaultClient)
		require.ErrorContains(t, err, "profile ceilings")
	}
}

func TestBridgeProfileCeilingsRejectDecodedResult(t *testing.T) {
	t.Run("evidence units", func(t *testing.T) {
		fixture := newBridgeFixture(t)
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			value := completedEnvelope(t, fixture, "job-evidence-ceiling", nil)
			evidence := document.SourceEvidenceV1{
				ContractVersion: document.SourceEvidenceContractV1,
				Completeness:    document.EvidenceComplete,
				Family:          "pdf", UnitKind: document.EvidenceUnitPage,
				Units: []document.SourceEvidenceUnitV1{
					{Order: 0, ProviderID: "page-0", Text: "first", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginZero, Start: 0, End: 0}},
					{Order: 1, ProviderID: "page-1", Text: "second", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginZero, Start: 1, End: 1}},
				},
			}
			setCompletedEvidence(t, value, evidence)
			writeBridgeJSON(t, response, http.StatusOK, value)
		}))
		t.Cleanup(server.Close)
		profile := testBridgeProfile(server.URL, fixture.descriptor, nil)
		profile.MaxEvidenceUnits = 1
		client, err := New(profile, nil, http.DefaultClient)
		require.NoError(t, err)

		_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)

		assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	})

	t.Run("artifact count", func(t *testing.T) {
		fixture := newBridgeFixture(t).withArtifactAuthorization(t,
			[]document.EvidenceArtifactRole{
				document.EvidenceArtifactImage, document.EvidenceArtifactStructured,
			}, 1)
		payload := []byte("{}")
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			writeBridgeJSON(t, response, http.StatusOK, completedEnvelope(t, fixture,
				"job-artifact-count", []map[string]any{
					inlineArtifact(document.EvidenceArtifactImage, payload),
					inlineArtifact(document.EvidenceArtifactStructured, payload),
				}))
		}))
		t.Cleanup(server.Close)
		profile := testBridgeProfile(server.URL, fixture.descriptor, nil)
		profile.MaxArtifacts = 1
		client, err := New(profile, nil, http.DefaultClient)
		require.NoError(t, err)

		_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)

		assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	})

	t.Run("cumulative bytes include fetched artifacts", func(t *testing.T) {
		fixture := newBridgeFixture(t).withStructuredArtifact(t)
		fixture.artifact = bytes.Repeat([]byte("x"), 1800)
		fixture.authorization.MaxArtifactBytes = 2000
		fixture.authorization.MaxTotalResultBytes = 2048
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case jobsPath:
				writeBridgeJSON(t, response, http.StatusOK, completedEnvelope(t, fixture,
					"job-total-ceiling", []map[string]any{{
						"role": string(document.EvidenceArtifactStructured), "media_type": "application/json",
						"byte_length": len(fixture.artifact), "sha256": sha256String(fixture.artifact),
						"location": "result", "artifact_id": "structured-1",
					}}))
			case jobsPath + "/job-total-ceiling/artifacts/structured-1":
				response.Header().Set("Content-Type", "application/json")
				_, err := response.Write(fixture.artifact)
				assert.NoError(t, err)
			default:
				http.NotFound(response, request)
			}
		}))
		t.Cleanup(server.Close)
		profile := testBridgeProfile(server.URL, fixture.descriptor, nil)
		profile.MaxArtifactBytes = 2000
		profile.MaxArtifacts = 1
		profile.MaxTotalResultBytes = 2048
		client, err := New(profile, nil, http.DefaultClient)
		require.NoError(t, err)

		_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)

		assertProviderCode(t, err, document.RenditionErrorMalformedEvidence)
	})
}

func TestBridgeContractIdempotencyReplayAndForwardCompatibleEnvelope(t *testing.T) {
	fixture := newBridgeFixture(t)
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		keys = append(keys, request.Header.Get("Idempotency-Key"))
		value := completedEnvelope(t, fixture, "job-replay", nil)
		value["future_minor_field"] = map[string]any{"ignored": true}
		writeBridgeJSON(t, response, http.StatusOK, value)
	}))
	t.Cleanup(server.Close)
	client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)

	for range 2 {
		_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
		require.NoError(t, err)
	}
	require.Len(t, keys, 2)
	assert.Equal(t, keys[0], keys[1], "an exact replay must retain its idempotency identity")
}

func TestBridgeContractAcceptsSchemaValidEvidenceEncoding(t *testing.T) {
	fixture := newBridgeFixture(t)
	value := completedEnvelope(t, fixture, "job-json-encoding", nil)
	evidence := completedEvidence(value)
	raw, ok := evidence["inline"].(jsontext.Value)
	require.True(t, ok)
	spaced := append([]byte("{\n  "), raw[1:]...)
	evidence["byte_length"] = len(spaced)
	evidence["sha256"] = sha256String(spaced)
	encoded, err := json.Marshal(value, json.Deterministic(true))
	require.NoError(t, err)
	encoded = bytes.Replace(encoded, raw, spaced, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", jobMediaType)
		_, writeErr := response.Write(encoded)
		assert.NoError(t, writeErr)
	}))
	t.Cleanup(server.Close)
	client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	require.NoError(t, err)
}

func TestBridgeContractPollsAndFetchesFixedRouteArtifact(t *testing.T) {
	fixture := newBridgeFixture(t)
	fixture = fixture.withStructuredArtifact(t)
	var polls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == jobsPath:
			writeBridgeJSON(t, response, http.StatusAccepted,
				pendingEnvelope(fixture, "job-async", JobQueued))
		case request.Method == http.MethodGet && request.URL.Path == jobsPath+"/job-async":
			if polls.Add(1) == 1 {
				writeBridgeJSON(t, response, http.StatusOK,
					pendingEnvelope(fixture, "job-async", JobRunning))
				return
			}
			writeBridgeJSON(t, response, http.StatusOK,
				completedEnvelope(t, fixture, "job-async", []map[string]any{{
					"role": string(document.EvidenceArtifactStructured), "media_type": "application/json",
					"byte_length": len(fixture.artifact), "sha256": sha256String(fixture.artifact),
					"location": "result", "artifact_id": "structured-1",
				}}))
		case request.Method == http.MethodGet && request.URL.Path == jobsPath+"/job-async/artifacts/structured-1":
			response.Header().Set("Content-Type", "application/json")
			_, err := response.Write(fixture.artifact)
			assert.NoError(t, err)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	require.Len(t, result.Artifacts, 1)
	assert.Equal(t, fixture.artifact, result.Artifacts[0].Payload)
	assert.Equal(t, int64(2), polls.Load())
}

func TestBridgeContractRejectsUnsafeOrCorruptResponses(t *testing.T) {
	tests := map[string]struct {
		mutate func(map[string]any)
		ctype  string
		want   string
	}{
		"source identity drift": {
			mutate: func(value map[string]any) { value["source_sha256"] = strings.Repeat("9", 64) },
			want:   "source identity",
		},
		"evidence checksum mismatch": {
			mutate: func(value map[string]any) {
				completedEvidence(value)["sha256"] = strings.Repeat("0", 64)
			},
			want: "evidence checksum",
		},
		"evidence length mismatch": {
			mutate: func(value map[string]any) {
				completedEvidence(value)["byte_length"] = 1
			},
			want: "evidence length",
		},
		"wrong response content type": {
			ctype: "application/json", want: "content type",
		},
		"Docbank frontmatter injection": {
			mutate: func(value map[string]any) {
				markdown := []byte("---\ncontract: docbank-sanitized-markdown/v1\n---\nsecret\n")
				completedResultMap(value)["provider_markdown"] = binaryPayload(markdown, "text/markdown")
			},
			want: "frontmatter",
		},
		"artifact URL escapes origin": {
			mutate: func(value map[string]any) {
				completedResultMap(value)["artifacts"] = []map[string]any{{
					"role": string(document.EvidenceArtifactStructured), "media_type": "application/json",
					"byte_length": 2, "sha256": sha256String([]byte("{}")),
					"location": "result", "artifact_id": "structured-1",
					"url": "https://attacker.invalid/secret",
				}}
			},
			want: "unknown member",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newBridgeFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				value := completedEnvelope(t, fixture, "job-corrupt", nil)
				if test.mutate != nil {
					test.mutate(value)
				}
				contentType := test.ctype
				if contentType == "" {
					contentType = jobMediaType
				}
				response.Header().Set("Content-Type", contentType)
				response.WriteHeader(http.StatusOK)
				assert.NoError(t, json.MarshalWrite(response, value, json.Deterministic(true)))
			}))
			t.Cleanup(server.Close)
			client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestBridgeContractRejectsRecordedUnknownMajorResponse(t *testing.T) {
	fixture := newBridgeFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", jobMediaType)
		_, err := response.Write(unknownMajorResponse)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	require.ErrorContains(t, err, "contract version")
}

func TestBridgeContractRejectsArtifactContentTypeLengthAndChecksumMismatch(t *testing.T) {
	tests := map[string]struct {
		mediaType     string
		declaredBytes int
		declaredHash  string
		want          string
	}{
		"content type": {mediaType: "text/plain", want: "content type"},
		"length":       {declaredBytes: 1, want: "HTTP length"},
		"checksum":     {declaredHash: strings.Repeat("0", 64), want: "checksum"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newBridgeFixture(t).withStructuredArtifact(t)
			declaredBytes := len(fixture.artifact)
			if test.declaredBytes != 0 {
				declaredBytes = test.declaredBytes
			}
			declaredHash := sha256String(fixture.artifact)
			if test.declaredHash != "" {
				declaredHash = test.declaredHash
			}
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == jobsPath {
					writeBridgeJSON(t, response, http.StatusOK,
						completedEnvelope(t, fixture, "job-artifact", []map[string]any{{
							"role": string(document.EvidenceArtifactStructured), "media_type": "application/json",
							"byte_length": declaredBytes, "sha256": declaredHash,
							"location": "result", "artifact_id": "structured-1",
						}}))
					return
				}
				contentType := test.mediaType
				if contentType == "" {
					contentType = "application/json"
				}
				response.Header().Set("Content-Type", contentType)
				_, err := response.Write(fixture.artifact)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestBridgeContractPreservesCredentialFailuresAndStripsAmbientCookies(t *testing.T) {
	t.Run("credential failure", func(t *testing.T) {
		fixture := newBridgeFixture(t)
		resolver := secretResolverFunc(func(context.Context, string) (string, error) {
			return "", errors.New("synthetic resolver failure")
		})
		client := newTestBridgeClientWithHTTP(t, "https://bridge.invalid", fixture.descriptor,
			resolver, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("request reached transport after credential failure")
				return nil, errors.New("unreachable test transport")
			})})
		baseline := runtime.NumGoroutine()
		for range 10 {
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			var providerError *document.RenditionProviderError
			require.ErrorAs(t, err, &providerError)
			assert.Equal(t, document.RenditionErrorAuthentication, providerError.Code())
		}
		require.Eventually(t, func() bool {
			return runtime.NumGoroutine() <= baseline+2
		}, time.Second, 10*time.Millisecond)
	})

	t.Run("ambient cookie jar", func(t *testing.T) {
		fixture := newBridgeFixture(t)
		server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			assert.Empty(t, request.Header.Get("Cookie"))
			writeBridgeJSON(t, response, http.StatusOK, completedEnvelope(t, fixture, "job-cookie", nil))
		}))
		t.Cleanup(server.Close)
		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		origin, err := url.Parse(server.URL)
		require.NoError(t, err)
		jar.SetCookies(origin, []*http.Cookie{{
			Name: "ambient", Value: "private", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		}})
		httpClient := server.Client()
		httpClient.Jar = jar
		client := newTestBridgeClientWithHTTP(t, server.URL, fixture.descriptor, nil, httpClient)
		_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
		require.NoError(t, err)
	})
}

func TestBridgeContractClassifiesPerRequestTimeouts(t *testing.T) {
	t.Run("submission is ambiguous", func(t *testing.T) {
		fixture := newBridgeFixture(t)
		client := newTestBridgeClientWithHTTP(t, "https://bridge.invalid", fixture.descriptor, nil,
			&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			})})
		client.requestTimeout = 10 * time.Millisecond
		_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
		var providerError *document.RenditionProviderError
		require.ErrorAs(t, err, &providerError)
		assert.Equal(t, document.RenditionErrorAmbiguousSubmission, providerError.Code())
	})

	t.Run("poll is retried", func(t *testing.T) {
		fixture := newBridgeFixture(t)
		var polls atomic.Int64
		client := newTestBridgeClientWithHTTP(t, "https://bridge.invalid", fixture.descriptor, nil,
			&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodPost {
					return bridgeHTTPResponse(t, request, http.StatusAccepted,
						pendingEnvelope(fixture, "job-slow", JobQueued)), nil
				}
				if request.Method == http.MethodGet {
					polls.Add(1)
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: request}, nil
			})})
		client.requestTimeout = 10 * time.Millisecond
		_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
		var providerError *document.RenditionProviderError
		require.ErrorAs(t, err, &providerError)
		assert.Equal(t, document.RenditionErrorCapacity, providerError.Code())
		assert.Equal(t, int64(4), polls.Load())
	})
}

func TestBridgeContractRejectsUnboundedOrExtendedStableErrors(t *testing.T) {
	tests := map[string]map[string]any{
		"unknown member": {
			"code": string(document.RenditionErrorRateLimited), "message": "slow down", "internal": "private",
		},
		"unbounded retry": {
			"code": string(document.RenditionErrorRateLimited), "message": "slow down",
			"retry_after_millis": int64(maxBridgeTimeout/time.Millisecond) + 1,
		},
	}
	for name, stableError := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newBridgeFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				value := pendingEnvelope(fixture, "job-error", JobFailed)
				value["error"] = stableError
				writeBridgeJSON(t, response, http.StatusTooManyRequests, value)
			}))
			t.Cleanup(server.Close)
			client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			var providerError *document.RenditionProviderError
			require.ErrorAs(t, err, &providerError)
			assert.Equal(t, document.RenditionErrorMalformedEvidence, providerError.Code())
		})
	}
}

func TestBridgeContractClassifiesUnknownExpiredAndAmbiguousJobs(t *testing.T) {
	for name, status := range map[string]int{
		"unknown": http.StatusNotFound, "expired": http.StatusGone,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newBridgeFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost {
					writeBridgeJSON(t, response, http.StatusAccepted,
						pendingEnvelope(fixture, "job-missing", JobQueued))
					return
				}
				response.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			var providerError *document.RenditionProviderError
			require.ErrorAs(t, err, &providerError)
			assert.Equal(t, document.RenditionErrorUnknownJob, providerError.Code())
		})
	}

	t.Run("ambiguous submission", func(t *testing.T) {
		fixture := newBridgeFixture(t)
		client := newTestBridgeClientWithHTTP(t, "https://bridge.invalid", fixture.descriptor, nil,
			&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, io.ErrUnexpectedEOF
			})})
		_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
		var providerError *document.RenditionProviderError
		require.ErrorAs(t, err, &providerError)
		assert.Equal(t, document.RenditionErrorAmbiguousSubmission, providerError.Code())
	})

	t.Run("truncated submission response", func(t *testing.T) {
		fixture := newBridgeFixture(t)
		client := newTestBridgeClientWithHTTP(t, "https://bridge.invalid", fixture.descriptor, nil,
			&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusAccepted,
					Header:     http.Header{"Content-Type": []string{jobMediaType}},
					Body:       io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)),
					Request:    request,
				}, nil
			})})
		_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
		var providerError *document.RenditionProviderError
		require.ErrorAs(t, err, &providerError)
		assert.Equal(t, document.RenditionErrorAmbiguousSubmission, providerError.Code())
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})
}

func TestBridgeContractBoundsPollingRetriesAndRefusesRedirects(t *testing.T) {
	for name, retryStatus := range map[string]int{
		"service unavailable": http.StatusServiceUnavailable,
		"internal error":      http.StatusInternalServerError,
	} {
		t.Run("bounded transient polling "+name, func(t *testing.T) {
			fixture := newBridgeFixture(t)
			var polls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost {
					writeBridgeJSON(t, response, http.StatusAccepted,
						pendingEnvelope(fixture, "job-retry", JobQueued))
					return
				}
				if request.Method == http.MethodGet {
					polls.Add(1)
					writeBridgeJSON(t, response, retryStatus, map[string]any{})
					return
				}
				response.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(server.Close)
			client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			var providerError *document.RenditionProviderError
			require.ErrorAs(t, err, &providerError)
			assert.Equal(t, document.RenditionErrorCapacity, providerError.Code())
			assert.Equal(t, int64(4), polls.Load())
		})
	}

	t.Run("redirect refused", func(t *testing.T) {
		fixture := newBridgeFixture(t)
		var escaped atomic.Int64
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			escaped.Add(1)
		}))
		t.Cleanup(target.Close)
		origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			http.Redirect(response, request, target.URL+jobsPath, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(origin.Close)
		client := newTestBridgeClient(t, origin.URL, fixture.descriptor, nil)
		_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
		require.Error(t, err)
		assert.Zero(t, escaped.Load())
	})
}

func TestBridgeContractCancelsRemoteJobWhenContextEnds(t *testing.T) {
	fixture := newBridgeFixture(t)
	var deletes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			writeBridgeJSON(t, response, http.StatusAccepted,
				pendingEnvelope(fixture, "job-cancel", JobQueued))
		case http.MethodGet:
			<-request.Context().Done()
		case http.MethodDelete:
			deletes.Add(1)
			response.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := client.Render(ctx, fixture.upload(), fixture.authorization)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Eventually(t, func() bool { return deletes.Load() == 1 }, time.Second, time.Millisecond)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type bridgeFixture struct {
	descriptor    document.RenditionDescriptor
	authorization document.RenditionAuthorization
	metadata      document.AuthorizedUploadMetadata
	source        []byte
	artifact      []byte
}

func newBridgeFixture(t *testing.T) bridgeFixture {
	t.Helper()
	source := []byte("synthetic bridge source")
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "bridge.synthetic", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("1", 64),
		TrustBoundary:     document.RenditionTrustOperatorNetwork,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "pdf", MediaType: "application/pdf",
			InputKind: document.RenditionInputOriginalFile,
		}},
		ReturnsMarkdown: true,
	})
	require.NoError(t, err)
	metadata := document.AuthorizedUploadMetadata{
		Filename: "document.pdf", MediaFamily: "pdf", MediaType: "application/pdf",
		ByteLength: int64(len(source)), SHA256: sha256String(source),
		CapabilityRecordChecksum: strings.Repeat("2", 64),
		ProviderMetadataChecksum: strings.Repeat("3", 64),
		InputKind:                document.RenditionInputOriginalFile,
	}
	authorizedAt := time.Now().UTC().Add(-time.Minute)
	authorization := document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType,
		InputKind: metadata.InputKind, MaxProviderMarkdownBytes: 4096,
		MaxTotalResultBytes: 16384,
		AuthorizedAt:        authorizedAt.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:           authorizedAt.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}
	return bridgeFixture{descriptor: descriptor, authorization: authorization, metadata: metadata, source: source}
}

func (fixture bridgeFixture) withStructuredArtifact(t *testing.T) bridgeFixture {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: fixture.descriptor.ID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: fixture.descriptor.PolicyFingerprint,
		TrustBoundary:     fixture.descriptor.TrustBoundary,
		SupportedFormats:  fixture.descriptor.SupportedFormats,
		ReturnsMarkdown:   true, ReturnsStructured: true,
		ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	require.NoError(t, err)
	fixture.descriptor = descriptor
	fixture.authorization.DescriptorFingerprint = descriptor.Fingerprint
	fixture.authorization.AllowedArtifactRoles = []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}
	fixture.authorization.MaxArtifactBytes = 4096
	fixture.authorization.MaxArtifacts = 1
	fixture.artifact = []byte(`{"synthetic":"value"}`)
	return fixture
}

func (fixture bridgeFixture) withArtifactAuthorization(
	t *testing.T, roles []document.EvidenceArtifactRole, maxArtifacts int,
) bridgeFixture {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: fixture.descriptor.ID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: fixture.descriptor.PolicyFingerprint,
		TrustBoundary:     fixture.descriptor.TrustBoundary,
		SupportedFormats:  fixture.descriptor.SupportedFormats,
		ReturnsMarkdown:   true, ReturnsStructured: true,
		ArtifactRoles: slices.Clone(roles),
	})
	require.NoError(t, err)
	fixture.descriptor = descriptor
	fixture.authorization.DescriptorFingerprint = descriptor.Fingerprint
	fixture.authorization.AllowedArtifactRoles = slices.Clone(roles)
	fixture.authorization.MaxArtifactBytes = 4096
	fixture.authorization.MaxArtifacts = maxArtifacts
	return fixture
}

func (fixture bridgeFixture) upload() document.AuthorizedUpload {
	return &testUpload{Reader: bytes.NewReader(fixture.source), metadata: fixture.metadata}
}

func newTestBridgeClient(
	t *testing.T, origin string, descriptor document.RenditionDescriptor, secrets SecretResolver,
) *Client {
	t.Helper()
	return newTestBridgeClientWithHTTP(t, origin, descriptor, secrets, http.DefaultClient)
}

func newTestBridgeClientWithHTTP(
	t *testing.T, origin string, descriptor document.RenditionDescriptor,
	secrets SecretResolver, httpClient *http.Client,
) *Client {
	t.Helper()
	client, err := New(testBridgeProfile(origin, descriptor, secrets), secrets, httpClient)
	require.NoError(t, err)
	return client
}

func testBridgeProfile(
	origin string, descriptor document.RenditionDescriptor, secrets SecretResolver,
) Profile {
	secretBinding := ""
	if secrets != nil {
		secretBinding = "bridge-api"
	}
	return Profile{
		Origin: origin, Descriptor: descriptor, SecretBinding: secretBinding,
		RequestTimeout: time.Second, TotalTimeout: 2 * time.Second,
		PollInterval: time.Millisecond, MaxPollAttempts: 4, MaxResponseBytes: 1 << 20,
	}
}

func completedEnvelope(
	t *testing.T, fixture bridgeFixture, jobID string, artifacts []map[string]any,
) map[string]any {
	t.Helper()
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceDegradedProvenance,
		Family:          fixture.authorization.MediaFamily, UnitKind: document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "natural_provenance",
			Reason: "synthetic bridge evidence has generic provenance",
		}},
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, Text: "synthetic bridge evidence",
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
			},
		}},
	}
	if len(artifacts) != 0 {
		evidence.Artifacts = []document.SourceEvidenceArtifactV1{{
			ProviderID: "structured-1", Pointer: "provider/structured.json",
			Role: document.EvidenceArtifactStructured, SHA256: sha256String(fixture.artifact),
		}}
	}
	evidenceJSON, err := json.Marshal(evidence, json.Deterministic(true))
	require.NoError(t, err)
	markdown := []byte("synthetic provider markdown\n")
	authorizedAt, err := time.Parse("2006-01-02T15:04:05.000000000Z", fixture.authorization.AuthorizedAt)
	require.NoError(t, err)
	return map[string]any{
		"contract_version": ContractVersion, "status": JobCompleted, "job_id": jobID,
		"source_sha256":          fixture.authorization.SourceSHA256,
		"adapter_id":             fixture.descriptor.ID,
		"descriptor_fingerprint": fixture.descriptor.Fingerprint,
		"policy_fingerprint":     fixture.descriptor.PolicyFingerprint,
		"result": map[string]any{
			"evidence": map[string]any{
				"media_type": evidenceMediaType, "byte_length": len(evidenceJSON),
				"sha256": sha256String(evidenceJSON), "inline": jsontext.Value(evidenceJSON),
			},
			"provider_markdown": binaryPayload(markdown, "text/markdown"),
			"artifacts":         artifacts,
			"receipt": document.RenditionReceipt{
				ProviderID:            fixture.descriptor.ID,
				DescriptorFingerprint: fixture.descriptor.Fingerprint,
				PolicyFingerprint:     fixture.descriptor.PolicyFingerprint,
				SourceSHA256:          fixture.authorization.SourceSHA256, OperationID: jobID,
				StartedAt:   authorizedAt.Add(time.Second).Format("2006-01-02T15:04:05.000000000Z"),
				CompletedAt: authorizedAt.Add(2 * time.Second).Format("2006-01-02T15:04:05.000000000Z"),
				Usage:       document.RenditionUsage{Requests: 1, InputBytes: fixture.authorization.SourceBytes},
			},
		},
	}
}

func pendingEnvelope(fixture bridgeFixture, jobID string, status JobStatus) map[string]any {
	return map[string]any{
		"contract_version": ContractVersion, "status": status, "job_id": jobID,
		"source_sha256":          fixture.authorization.SourceSHA256,
		"adapter_id":             fixture.descriptor.ID,
		"descriptor_fingerprint": fixture.descriptor.Fingerprint,
		"policy_fingerprint":     fixture.descriptor.PolicyFingerprint,
	}
}

func binaryPayload(payload []byte, mediaType string) map[string]any {
	return map[string]any{
		"media_type": mediaType, "byte_length": len(payload), "sha256": sha256String(payload),
		"inline_base64": base64.StdEncoding.EncodeToString(payload),
	}
}

func completedResultMap(value map[string]any) map[string]any {
	result, ok := value["result"].(map[string]any)
	if !ok {
		panic("test completed envelope lacks a result map")
	}
	return result
}

func completedEvidence(value map[string]any) map[string]any {
	evidence, ok := completedResultMap(value)["evidence"].(map[string]any)
	if !ok {
		panic("test completed envelope lacks an evidence map")
	}
	return evidence
}

func setCompletedEvidence(t *testing.T, value map[string]any, evidence document.SourceEvidenceV1) {
	t.Helper()
	encoded, err := json.Marshal(evidence, json.Deterministic(true))
	require.NoError(t, err)
	payload := completedEvidence(value)
	payload["inline"] = jsontext.Value(encoded)
	payload["byte_length"] = len(encoded)
	payload["sha256"] = sha256String(encoded)
}

func inlineArtifact(role document.EvidenceArtifactRole, payload []byte) map[string]any {
	return map[string]any{
		"role": string(role), "media_type": "application/json",
		"byte_length": len(payload), "sha256": sha256String(payload),
		"location": "inline", "inline_base64": base64.StdEncoding.EncodeToString(payload),
	}
}

func assertProviderCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	var providerError *document.RenditionProviderError
	require.ErrorAs(t, err, &providerError)
	assert.Equal(t, want, providerError.Code())
}

func assertMultipartRequest(
	t *testing.T, request *http.Request, authorization document.RenditionAuthorization, source []byte,
) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(request.Body, params["boundary"])
	manifestPart, err := reader.NextPart()
	require.NoError(t, err)
	assert.Equal(t, authorizationPartName, manifestPart.FormName())
	manifestBytes, err := io.ReadAll(manifestPart)
	require.NoError(t, err)
	var manifest AuthorizationManifest
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	assert.Equal(t, authorization.SourceSHA256, manifest.Authorization.SourceSHA256)
	sourcePart, err := reader.NextPart()
	require.NoError(t, err)
	assert.Equal(t, sourcePartName, sourcePart.FormName())
	assert.Equal(t, "document.pdf", sourcePart.FileName())
	gotSource, err := io.ReadAll(sourcePart)
	require.NoError(t, err)
	assert.Equal(t, source, gotSource)
}

func writeBridgeJSON(t *testing.T, response http.ResponseWriter, status int, value any) {
	t.Helper()
	response.Header().Set("Content-Type", jobMediaType)
	response.WriteHeader(status)
	require.NoError(t, json.MarshalWrite(response, value, json.Deterministic(true)))
}

func bridgeHTTPResponse(
	t *testing.T, request *http.Request, status int, value any,
) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(value, json.Deterministic(true))
	require.NoError(t, err)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{jobMediaType}},
		Body:       io.NopCloser(bytes.NewReader(encoded)),
		Request:    request,
	}
}

func sha256String(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
