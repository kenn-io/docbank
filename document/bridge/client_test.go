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

func TestBridgeContractWithholdsFilenameWhenDisclosureIsDisabled(t *testing.T) {
	fixture := newBridgeFixture(t)
	fixture.authorization.DiscloseFilename = false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertMultipartRequest(t, request, fixture.authorization, fixture.source)
		writeBridgeJSON(t, response, http.StatusOK,
			completedEnvelope(t, fixture, "job-withheld-filename", nil))
	}))
	t.Cleanup(server.Close)

	client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
	_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
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

func TestBridgeContractHonorsRetryDelayFromPollingError(t *testing.T) {
	fixture := newBridgeFixture(t)
	const retryDelay = 40 * time.Millisecond
	var polls, firstPollAt, secondPollAt atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost:
			writeBridgeJSON(t, response, http.StatusAccepted,
				pendingEnvelope(fixture, "job-rate-limited", JobQueued))
		case request.Method == http.MethodGet && polls.Add(1) == 1:
			firstPollAt.Store(time.Now().UnixNano())
			value := pendingEnvelope(fixture, "job-rate-limited", JobRunning)
			value["error"] = map[string]any{
				"code": string(document.RenditionErrorRateLimited), "message": "retry later",
				"retry_after_millis": retryDelay.Milliseconds(),
			}
			writeBridgeJSON(t, response, http.StatusTooManyRequests, value)
		case request.Method == http.MethodGet:
			secondPollAt.Store(time.Now().UnixNano())
			writeBridgeJSON(t, response, http.StatusOK,
				completedEnvelope(t, fixture, "job-rate-limited", nil))
		default:
			response.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)

	client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
	client.pollInterval = time.Millisecond
	client.requestTimeout = 10 * time.Millisecond
	_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, int64(2), polls.Load())
	assert.GreaterOrEqual(t, time.Duration(secondPollAt.Load()-firstPollAt.Load()),
		retryDelay-5*time.Millisecond)
}

func TestBridgeContractRejectsAggregateArtifactLimitsBeforeFetching(t *testing.T) {
	tests := map[string]struct {
		mutate func(*bridgeFixture, []map[string]any) []map[string]any
		want   string
	}{
		"artifact count": {
			mutate: func(_ *bridgeFixture, artifacts []map[string]any) []map[string]any {
				return append(artifacts, artifacts[0])
			},
			want: "artifact count",
		},
		"artifact role": {
			mutate: func(_ *bridgeFixture, artifacts []map[string]any) []map[string]any {
				artifacts[0]["role"] = string(document.EvidenceArtifactImage)
				return artifacts
			},
			want: "artifact role",
		},
		"total result bytes": {
			mutate: func(fixture *bridgeFixture, artifacts []map[string]any) []map[string]any {
				fixture.authorization.MaxTotalResultBytes = 1
				return artifacts
			},
			want: "total result bytes",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newBridgeFixture(t).withStructuredArtifact(t)
			artifacts := []map[string]any{{
				"role": string(document.EvidenceArtifactStructured), "media_type": "application/json",
				"byte_length": len(fixture.artifact), "sha256": sha256String(fixture.artifact),
				"location": "result", "artifact_id": "structured-1",
			}}
			artifacts = test.mutate(&fixture, artifacts)
			var artifactRequests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == jobsPath {
					writeBridgeJSON(t, response, http.StatusOK,
						completedEnvelope(t, fixture, "job-limits", artifacts))
					return
				}
				if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/artifacts/") {
					artifactRequests.Add(1)
				}
				response.Header().Set("Content-Type", "application/json")
				_, err := response.Write(fixture.artifact)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)

			client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			requireBridgeErrorContains(t, err, test.want)
			assert.Zero(t, artifactRequests.Load(),
				"invalid aggregate metadata must fail before artifact fetches")
		})
	}
}

func TestBridgeContractRejectsArtifactAboveResponseLimitBeforeFetching(t *testing.T) {
	fixture := newBridgeFixture(t).withStructuredArtifact(t)
	fixture.artifact = bytes.Repeat([]byte("x"), 8<<10)
	fixture.authorization.MaxArtifactBytes = len(fixture.artifact)
	fixture.authorization.MaxTotalResultBytes = 1 << 20
	artifacts := []map[string]any{{
		"role": string(document.EvidenceArtifactStructured), "media_type": "application/json",
		"byte_length": len(fixture.artifact), "sha256": sha256String(fixture.artifact),
		"location": "result", "artifact_id": "structured-1",
	}}
	var artifactRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == jobsPath {
			writeBridgeJSON(t, response, http.StatusOK,
				completedEnvelope(t, fixture, "job-response-limit", artifacts))
			return
		}
		if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/artifacts/") {
			artifactRequests.Add(1)
		}
		response.Header().Set("Content-Type", "application/json")
		_, err := response.Write(fixture.artifact)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
	client.maxResponseBytes = 4 << 10
	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	requireBridgeErrorContains(t, err, "response byte limit")
	assert.Zero(t, artifactRequests.Load(), "oversized artifacts must fail before fetching")
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
		"whitespace-padded inline payload": {
			mutate: func(value map[string]any) {
				payload := []byte("x")
				completedResultMap(value)["provider_markdown"] = map[string]any{
					"media_type": "text/plain", "byte_length": len(payload), "sha256": sha256String(payload),
					"inline_base64": strings.Repeat("\n", 64<<10) + base64.StdEncoding.EncodeToString(payload),
				}
			},
			want: "provider Markdown",
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
			_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
			requireBridgeErrorContains(t, err, test.want)
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
	_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	requireBridgeErrorContains(t, err, "contract version")
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
			_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
			requireBridgeErrorContains(t, err, test.want)
		})
	}
}

func TestBridgeContractClassifiesArtifactReadFailure(t *testing.T) {
	fixture := newBridgeFixture(t).withStructuredArtifact(t)
	client := newTestBridgeClientWithHTTP(t, "https://bridge.invalid", fixture.descriptor, nil,
		&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, ContentLength: -1,
				Header:  http.Header{"Content-Type": []string{"application/json"}},
				Body:    io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)),
				Request: request,
			}, nil
		})})

	_, err := client.fetchArtifact(t.Context(), "job-read-error", artifactPayload{
		MediaType: "application/json", ByteLength: int64(len(fixture.artifact)),
		SHA256: sha256String(fixture.artifact), ArtifactID: "structured-1",
	})
	var providerError *document.RenditionProviderError
	require.ErrorAs(t, err, &providerError)
	assert.Equal(t, document.RenditionErrorTransient, providerError.Code())
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestBridgeContractClassifiesArtifactReadTimeout(t *testing.T) {
	fixture := newBridgeFixture(t).withStructuredArtifact(t)
	client := newTestBridgeClientWithHTTP(t, "https://bridge.invalid", fixture.descriptor, nil,
		&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body, writer := io.Pipe()
			go func() {
				<-request.Context().Done()
				_ = writer.CloseWithError(request.Context().Err())
			}()
			return &http.Response{
				StatusCode: http.StatusOK, ContentLength: -1,
				Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: request,
			}, nil
		})})
	client.requestTimeout = 10 * time.Millisecond

	_, err := client.fetchArtifact(t.Context(), "job-read-timeout", artifactPayload{
		MediaType: "application/json", ByteLength: int64(len(fixture.artifact)),
		SHA256: sha256String(fixture.artifact), ArtifactID: "structured-1",
	})
	var providerError *document.RenditionProviderError
	require.ErrorAs(t, err, &providerError)
	assert.Equal(t, document.RenditionErrorTransient, providerError.Code())
	assert.ErrorIs(t, err, context.DeadlineExceeded)
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
		ReturnsMarkdown: true, ReturnsStructured: true,
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
		DiscloseFilename:    true,
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
	secretBinding := ""
	if secrets != nil {
		secretBinding = "bridge-api"
	}
	client, err := New(Profile{
		Origin: origin, Descriptor: descriptor, SecretBinding: secretBinding,
		RequestTimeout: time.Second, TotalTimeout: 2 * time.Second,
		PollInterval: time.Millisecond, MaxPollAttempts: 4, MaxResponseBytes: 1 << 20,
	}, secrets, httpClient)
	require.NoError(t, err)
	return client
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
	authorizationFingerprint, err := fixture.authorization.Fingerprint()
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
				ProviderID:                  fixture.descriptor.ID,
				DescriptorFingerprint:       fixture.descriptor.Fingerprint,
				PolicyFingerprint:           fixture.descriptor.PolicyFingerprint,
				RenditionRequestFingerprint: fixture.authorization.RenditionRequestFingerprint,
				AuthorizationFingerprint:    authorizationFingerprint,
				SourceSHA256:                fixture.authorization.SourceSHA256, OperationID: jobID,
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
	assert.Equal(t, authorization.DiscloseFilename, manifest.Authorization.DiscloseFilename)
	expectedFilename := "document.pdf"
	if !authorization.DiscloseFilename {
		expectedFilename = ""
	}
	assert.Equal(t, expectedFilename, manifest.Source.Filename)
	sourcePart, err := reader.NextPart()
	require.NoError(t, err)
	assert.Equal(t, sourcePartName, sourcePart.FormName())
	assert.Equal(t, expectedFilename, sourcePart.FileName())
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

func requireBridgeErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	var providerError *document.RenditionProviderError
	require.ErrorAs(t, err, &providerError)
	require.ErrorContains(t, errors.Unwrap(providerError), want)
}
