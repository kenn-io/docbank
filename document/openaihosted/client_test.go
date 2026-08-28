package openaihosted

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestEmbedRejectsStrictHostedResponseDrift(t *testing.T) {
	valid := `{"object":"list","data":[{"object":"embedding","embedding":[1,0,0],"index":0},{"object":"embedding","embedding":[0,1,0],"index":1}],"model":"text-embedding-3-large","usage":{"prompt_tokens":2,"total_tokens":2}}`
	tests := map[string]string{
		"root object":          strings.Replace(valid, `"object":"list"`, `"object":"collection"`, 1),
		"item object":          strings.Replace(valid, `"object":"embedding"`, `"object":"vector"`, 1),
		"model echo":           strings.Replace(valid, Model, "text-embedding-3-small", 1),
		"usage missing":        strings.Replace(valid, `,"usage":{"prompt_tokens":2,"total_tokens":2}`, "", 1),
		"prompt usage missing": strings.Replace(valid, `"prompt_tokens":2,`, "", 1),
		"total usage missing":  strings.Replace(valid, `,"total_tokens":2`, "", 1),
		"usage negative":       strings.Replace(valid, `"prompt_tokens":2`, `"prompt_tokens":-1`, 1),
		"usage inconsistent":   strings.Replace(valid, `"total_tokens":2`, `"total_tokens":1`, 1),
		"missing index":        strings.Replace(valid, `,"index":0`, "", 1),
		"duplicate index":      strings.Replace(valid, `"index":1`, `"index":0`, 1),
		"outside index":        strings.Replace(valid, `"index":1`, `"index":2`, 1),
		"missing vector":       strings.Replace(valid, `,{"object":"embedding","embedding":[0,1,0],"index":1}`, "", 1),
		"extra vector":         strings.Replace(valid, `],"model"`, `,{"object":"embedding","embedding":[0,0,1],"index":2}],"model"`, 1),
		"wrong dimension":      strings.Replace(valid, `[1,0,0]`, `[1,0]`, 1),
		"non finite":           strings.Replace(valid, `[1,0,0]`, `[1e1000,0,0]`, 1),
		"base64 vector":        strings.Replace(valid, `[1,0,0]`, `"AQID"`, 1),
		"unknown root":         strings.Replace(valid, `"usage":`, `"future":true,"usage":`, 1),
		"unknown item":         strings.Replace(valid, `"index":0`, `"future":true,"index":0`, 1),
		"unknown usage":        strings.Replace(valid, `"prompt_tokens":2`, `"future":true,"prompt_tokens":2`, 1),
		"duplicate member":     strings.Replace(valid, `"object":"list"`, `"object":"list","object":"list"`, 1),
		"trailing JSON":        valid + `{}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			client := hostedClientReturning(t, http.StatusOK, "application/json", body)
			_, err := client.Embed(context.Background(), twoHostedInputs(), hostedAuthorization(client.descriptor, 2))
			require.Error(t, err)
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.NotContains(t, err.Error(), "alpha")
			assert.NotContains(t, err.Error(), "AQID")
		})
	}

	client := hostedClientReturning(t, http.StatusOK, "text/plain", valid)
	_, err := client.Embed(context.Background(), twoHostedInputs(), hostedAuthorization(client.descriptor, 2))
	require.ErrorIs(t, err, ErrPermanentResponse)
}

func TestEmbedEnforcesItemTotalRequestResponseAndBatchBounds(t *testing.T) {
	tests := []struct {
		name          string
		mutateProfile func(*Profile)
		inputs        []document.EmbeddingInput
		authorization func(document.EmbeddingDescriptor) document.EmbeddingAuthorization
		response      string
		wantRequest   bool
	}{
		{
			name: "per item", mutateProfile: func(profile *Profile) { profile.MaxInputItemBytes = 10 },
			inputs: []document.EmbeddingInput{{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "xx"}},
		},
		{
			name: "total input", mutateProfile: func(profile *Profile) { profile.MaxInputItemBytes = 10; profile.MaxInputBytes = 15 },
			inputs: []document.EmbeddingInput{
				{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "a"},
				{Key: "second", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "b"},
			},
		},
		{
			name: "request bytes", mutateProfile: func(profile *Profile) { profile.MaxRequestBytes = 64 },
			inputs: []document.EmbeddingInput{{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "a"}},
		},
		{
			name: "batch", mutateProfile: func(profile *Profile) { profile.MaxBatchItems = 1 },
			inputs: twoHostedInputs(),
		},
		{
			name: "response bytes", mutateProfile: func(profile *Profile) { profile.MaxResponseBytes = 64 },
			inputs: []document.EmbeddingInput{{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "a"}},
			authorization: func(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
				authorization := hostedAuthorization(descriptor, 1)
				authorization.MaxResponseBytes = 64
				return authorization
			},
			response: strings.Repeat("x", 65), wantRequest: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := hostedTestProfile(t)
			test.mutateProfile(&profile)
			profile.Descriptor = hostedDescriptorFor(t, profile)
			var requests atomic.Int32
			client := newHostedTestClient(t, profile, hostedSecrets{"secret:openai": "sk-synthetic"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				return hostedJSONResponse(request, http.StatusOK, test.response), nil
			}))
			authorization := hostedAuthorization(client.descriptor, len(test.inputs))
			if test.authorization != nil {
				authorization = test.authorization(client.descriptor)
			}
			_, err := client.Embed(context.Background(), test.inputs, authorization)
			require.Error(t, err)
			if test.name == "response bytes" {
				require.ErrorIs(t, err, ErrCapacityResponse)
			}
			if test.wantRequest {
				assert.Equal(t, int32(1), requests.Load())
			} else {
				assert.Zero(t, requests.Load())
			}
		})
	}
}

func TestEmbedRequiresValidNamedAPIKeyWithoutLeakingResolverDetails(t *testing.T) {
	profile := hostedTestProfile(t)
	tests := map[string]SecretResolver{
		"empty":         hostedSecrets{"secret:openai": ""},
		"whitespace":    hostedSecrets{"secret:openai": "sk synthetic"},
		"control":       hostedSecrets{"secret:openai": "sk-synthetic\nPRIVATE"},
		"invalid UTF-8": hostedSecrets{"secret:openai": string([]byte{'s', 'k', '-', 0xff})},
		"resolver":      failingSecrets{err: errors.New("PRIVATE_RESOLVER_DETAIL")},
	}
	for name, resolver := range tests {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			client := newHostedTestClient(t, profile, resolver, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				return hostedJSONResponse(request, http.StatusInternalServerError, "PRIVATE_BODY"), nil
			}))
			_, err := client.Embed(context.Background(), oneHostedInput(), hostedAuthorization(client.descriptor, 1))
			require.Error(t, err)
			assert.Zero(t, requests.Load())
			assert.NotContains(t, err.Error(), "PRIVATE")
			assert.NotContains(t, err.Error(), "synthetic")
		})
	}
}

func TestEmbedClassifiesHTTPAndTransportFailuresWithoutLeakingBodies(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter string
		want       error
		delay      time.Duration
		delaySet   bool
	}{
		{name: "request timeout", status: http.StatusRequestTimeout, want: ErrTransientResponse},
		{name: "rate limit", status: http.StatusTooManyRequests, retryAfter: "7200", want: ErrTransientResponse, delay: time.Hour, delaySet: true},
		{name: "server", status: http.StatusInternalServerError, want: ErrTransientResponse},
		{name: "capacity", status: http.StatusRequestEntityTooLarge, want: ErrCapacityResponse},
		{name: "permanent", status: http.StatusBadRequest, want: ErrPermanentResponse},
		{name: "redirect", status: http.StatusTemporaryRedirect, want: ErrPermanentResponse},
		{name: "nonstandard 600", status: 600, want: ErrPermanentResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := hostedTestProfile(t)
			client := newHostedTestClient(t, profile, hostedSecrets{"secret:openai": "sk-PRIVATE"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := hostedJSONResponse(request, test.status, "PRIVATE_PROVIDER_BODY")
				response.Header.Set("Retry-After", test.retryAfter)
				if test.status == http.StatusTemporaryRedirect {
					response.Header.Set("Location", "https://example.com/steal")
				}
				return response, nil
			}))
			_, err := client.Embed(context.Background(), oneHostedInput(), hostedAuthorization(client.descriptor, 1))
			require.ErrorIs(t, err, test.want)
			assert.NotContains(t, err.Error(), "PRIVATE")
			delay, set := RetryAfter(err)
			assert.Equal(t, test.delaySet, set)
			assert.Equal(t, test.delay, delay)
		})
	}

	transportErr := errors.New("PRIVATE_TRANSPORT_DETAIL")
	client := newHostedTestClient(t, hostedTestProfile(t), hostedSecrets{"secret:openai": "sk-PRIVATE"}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportErr
	}))
	_, err := client.Embed(context.Background(), oneHostedInput(), hostedAuthorization(client.descriptor, 1))
	require.ErrorIs(t, err, ErrTransientResponse)
	assert.NotContains(t, err.Error(), "PRIVATE")

	client = newHostedTestClient(t, hostedTestProfile(t), hostedSecrets{"secret:openai": "sk-PRIVATE"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: &failingBody{err: errors.New("PRIVATE_RESPONSE_READ_DETAIL")}, Request: request,
		}, nil
	}))
	_, err = client.Embed(context.Background(), oneHostedInput(), hostedAuthorization(client.descriptor, 1))
	require.ErrorIs(t, err, ErrTransientResponse)
	assert.NotContains(t, err.Error(), "PRIVATE")
}

func TestEmbedPreservesCancellationAndRefusesRedirectReplay(t *testing.T) {
	started := make(chan struct{})
	client := newHostedTestClient(t, hostedTestProfile(t), hostedSecrets{"secret:openai": "sk-synthetic"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Embed(ctx, oneHostedInput(), hostedAuthorization(client.descriptor, 1))
		done <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	var calls atomic.Int32
	client = newHostedTestClient(t, hostedTestProfile(t), hostedSecrets{"secret:openai": "sk-synthetic"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": []string{"https://example.com/steal"}},
			Body:       io.NopCloser(strings.NewReader("PRIVATE_REDIRECT_BODY")), Request: request,
		}, nil
	}))
	_, err := client.Embed(context.Background(), oneHostedInput(), hostedAuthorization(client.descriptor, 1))
	require.ErrorIs(t, err, ErrPermanentResponse)
	assert.Equal(t, int32(1), calls.Load())
	assert.NotContains(t, err.Error(), "PRIVATE")
}

func hostedClientReturning(t *testing.T, status int, mediaType, body string) *Client {
	t.Helper()
	return newHostedTestClient(t, hostedTestProfile(t), hostedSecrets{"secret:openai": "sk-synthetic"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := hostedJSONResponse(request, status, body)
		response.Header.Set("Content-Type", mediaType)
		return response, nil
	}))
}

func oneHostedInput() []document.EmbeddingInput {
	return []document.EmbeddingInput{{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha"}}
}

func twoHostedInputs() []document.EmbeddingInput {
	return []document.EmbeddingInput{
		{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "alpha"},
		{Key: "second", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "beta"},
	}
}

type failingSecrets struct{ err error }

func (resolver failingSecrets) ResolveSecret(context.Context, string) (string, error) {
	return "", resolver.err
}

type failingBody struct{ err error }

func (body *failingBody) Read([]byte) (int, error) { return 0, body.err }

func (*failingBody) Close() error { return nil }
