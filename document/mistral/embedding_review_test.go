package mistral

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

type mistralEmbeddingResolver struct {
	address     netip.Addr
	certificate *x509.Certificate
}

func (resolver mistralEmbeddingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{resolver.address}, nil
}

func mistralFixture(t *testing.T, handler http.Handler) (string, providerhttp.EgressPolicy, providerhttp.Resolver) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	_, portText, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	return server.URL, providerhttp.EgressPolicy{
		Scheme: "https", Host: parsed.Hostname(), Port: uint16(port),
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, ProxyMode: providerhttp.ProxyDisabled,
	}, mistralEmbeddingResolver{address: netip.MustParseAddr("127.0.0.1"), certificate: server.Certificate()}
}

func newMistralEmbeddingTestProvider(t *testing.T, profile EmbeddingProfile, secrets SecretResolver, resolver providerhttp.Resolver) (*EmbeddingClient, error) {
	t.Helper()
	provider, err := NewEmbeddingProvider(profile, secrets, resolver)
	if err != nil {
		return nil, err
	}
	if fixture, ok := resolver.(mistralEmbeddingResolver); ok {
		roots := x509.NewCertPool()
		roots.AddCert(fixture.certificate)
		transport, ok := provider.http.Transport.(*http.Transport)
		require.True(t, ok)
		// Trust only the fixture certificate after canonical profile validation.
		transport.TLSClientConfig.RootCAs = roots
	}
	t.Cleanup(provider.http.CloseIdleConnections)
	return provider, nil
}

func TestHostedMistralAliasIsAlwaysExportOnlyAndSealed(t *testing.T) {
	endpoint, egress, resolver := mistralFixture(t, http.NotFoundHandler())
	profile := testEmbeddingProfile(t)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile.Descriptor.SupportsTextQuery = true
	_, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "synthetic-secret"}, resolver)
	require.ErrorContains(t, err, "export-only")
}

type embeddingSecretFunc func(context.Context, string) (string, error)

func (resolve embeddingSecretFunc) ResolveSecret(ctx context.Context, binding string) (string, error) {
	return resolve(ctx, binding)
}

func TestMistralEmbeddingAttemptTimeouts(t *testing.T) {
	for _, stage := range []string{"credentials", "headers", "body"} {
		for _, outcome := range []string{"recover", "exhaust", "caller_cancel"} {
			t.Run(stage+"/"+outcome, func(t *testing.T) {
				// Virtual time keeps connection latency out of the attempt deadline.
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var requests atomic.Int32
					var resolutions int
					response := mistralEmbeddingBody(t, []int{0})
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
					profile := testEmbeddingProfile(t)
					profile.RequestTimeout, profile.MaxRetries = 100*time.Millisecond, 3
					profile.MaxRetryDelay = time.Nanosecond
					recomputeMistralEmbeddingProfile(t, &profile)
					// Reject network access if the in-memory client is ever bypassed.
					provider, err := newMistralEmbeddingTestProvider(t, profile, secrets, failingMistralEmbeddingResolver{})
					require.NoError(t, err)
					server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
							<-r.Context().Done()
							return
						}
						_, _ = w.Write(response)
					}))
					provider.http = server.Client()
					result, err := provider.Embed(ctx, []document.EmbeddingInput{{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "first"}}, embeddingAuthorization(profile.Descriptor))
					switch outcome {
					case "recover":
						require.NoError(t, err)
						require.Len(t, result.Vectors, 1)
						require.Equal(t, 3, resolutions)
					case "exhaust":
						require.ErrorIs(t, err, ErrTransientResponse)
						require.ErrorIs(t, err, context.DeadlineExceeded)
						require.Equal(t, 3, resolutions)
						require.Equal(t, 2, MetricsFromError(err).Retries)
					case "caller_cancel":
						require.ErrorIs(t, err, context.Canceled)
						require.NotErrorIs(t, err, ErrTransientResponse)
						require.Equal(t, 1, resolutions)
						require.Zero(t, MetricsFromError(err).Retries)
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
						require.Equal(t, wantRequests, MetricsFromError(err).Requests)
					}
				})
			})
		}
	}
}

func TestMistralEmbeddingAttemptTimeoutsThroughProductionTransport(t *testing.T) {
	for _, stage := range []string{"headers", "body"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			var reached atomic.Bool
			release := make(chan struct{})
			defer close(release)
			endpoint, egress, resolver := mistralFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				if stage == "body" {
					w.WriteHeader(http.StatusOK)
					assert.NoError(t, http.NewResponseController(w).Flush())
				}
				reached.Store(true)
				// Delay net/http's implicit response until the client assertions finish.
				<-release
			}))
			profile := testEmbeddingProfile(t)
			profile.Endpoint, profile.EgressPolicy = endpoint, egress
			profile.RequestTimeout, profile.MaxRetries = 2*time.Second, 1
			recomputeMistralEmbeddingProfile(t, &profile)
			provider, err := newMistralEmbeddingTestProvider(t, profile, embeddingSecretMap{"credential:mistral-embed": "synthetic-secret"}, resolver)
			require.NoError(t, err)
			var responseStarted atomic.Bool
			ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
				GotFirstResponseByte: func() { responseStarted.Store(true) },
			})
			_, err = provider.Embed(ctx, []document.EmbeddingInput{{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "first"}}, embeddingAuthorization(profile.Descriptor))
			require.True(t, reached.Load(), "attempt timed out before reaching the %s stage", stage)
			require.Equal(t, stage == "body", responseStarted.Load())
			require.ErrorIs(t, err, ErrTransientResponse)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Equal(t, RequestMetrics{Requests: 1}, withoutLatency(MetricsFromError(err)))
		})
	}
}

func TestMistralEmbeddingCapacityCeilings(t *testing.T) {
	for _, field := range []string{"input", "request", "response", "batch"} {
		for _, excess := range []int64{0, 1, math.MaxInt64} {
			t.Run(fmt.Sprintf("%s/%d", field, excess), func(t *testing.T) {
				profile := testEmbeddingProfile(t)
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
				_, err := EmbeddingPolicyFingerprint(profile)
				if excess == 0 {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}
