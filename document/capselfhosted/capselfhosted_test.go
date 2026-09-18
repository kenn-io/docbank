package capselfhosted_test

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/capselfhosted"
	"go.kenn.io/docbank/document/providerhttp"
)

func TestRecognizeSharePath(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
		ok   bool
	}{
		{path: "/s/vid-1", want: "cap-self-hosted-video-id/v1:vid-1", ok: true},
		{path: "/embed/vid-1", want: "cap-self-hosted-video-id/v1:vid-1", ok: true},
		{path: "/share/vid-1"}, {path: "/s/"}, {path: "/s/a/b"},
		{path: "/s/a%2Fb"}, {path: "/s/.."}, {path: "/s/a.b"},
		{path: "/s/" + strings.Repeat("a", 129)},
	} {
		t.Run(test.path, func(t *testing.T) {
			got, ok := capselfhosted.RecognizeSharePath(test.path)
			assert.Equal(t, test.ok, ok)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestProbeClassifiesRegisteredDeployment(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     capselfhosted.ProbeState
		body      string
		code      int
		setup     func(*httptest.Server, *atomic.Int32)
		root      bool
		pin       string
		addr      string
		secret    string
		stallBody bool
		abortBody bool
		timeout   time.Duration
		wantAuth  bool
	}{
		{name: "verified", state: capselfhosted.ProbeVerified, body: `{"data":{}}`, wantAuth: true},
		{name: "credential_rejected", state: capselfhosted.ProbeCredentialRejected, code: http.StatusUnauthorized},
		{name: "redirect_unregistered", state: capselfhosted.ProbeRedirectUnregistered, code: http.StatusFound},
		{name: "dns_denied", state: capselfhosted.ProbeDNSDenied, addr: "10.0.0.1"},
		{name: "tls_pin_mismatch", state: capselfhosted.ProbeTLSPinMismatch, pin: strings.Repeat("0", sha256.Size*2)},
		{name: "tls_verification_failed", state: capselfhosted.ProbeTLSVerificationFailed, root: false},
		{name: "credential_missing", state: capselfhosted.ProbeCredentialMissing, secret: "missing"},
		{name: "provider_rate_limited", state: capselfhosted.ProbeProviderRateLimited, code: http.StatusTooManyRequests},
		{name: "provider_timeout", state: capselfhosted.ProbeProviderUnavailable, stallBody: true, timeout: 10 * time.Millisecond},
		{name: "provider_body_error", state: capselfhosted.ProbeProviderUnavailable, abortBody: true},
		{name: "contract_mismatch_status", state: capselfhosted.ProbeContractMismatch, code: http.StatusPaymentRequired},
		{name: "contract_mismatch_body", state: capselfhosted.ProbeContractMismatch, body: `{"data":[]}`},
		{name: "contract_mismatch_malformed", state: capselfhosted.ProbeContractMismatch, body: "not json"},
		{name: "contract_mismatch_oversized", state: capselfhosted.ProbeContractMismatch, body: `{"data":"` + strings.Repeat("x", 64<<10) + `"}`},
		{name: "provider_unavailable", state: capselfhosted.ProbeProviderUnavailable, code: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if test.wantAuth {
					assert.Equal(t, "/api/developer/v1/usage", r.URL.Path)
					assert.Equal(t, "Bearer synthetic-cap-key", r.Header.Get("Authorization"))
				}
				if test.name == "redirect_unregistered" {
					other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
						t.Fatal("redirect target received a request")
					}))
					defer other.Close()
					http.Redirect(w, r, other.URL, http.StatusFound)
					return
				}
				if test.code != 0 {
					w.WriteHeader(test.code)
					return
				}
				if test.stallBody {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"data":`))
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Errorf("flush response: %v", err)
						return
					}
					<-r.Context().Done()
					return
				}
				if test.abortBody {
					w.WriteHeader(http.StatusOK)
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Errorf("flush response: %v", err)
						return
					}
					panic(http.ErrAbortHandler)
				}
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			_, rawPort, err := net.SplitHostPort(server.Listener.Addr().String())
			require.NoError(t, err)
			port, err := strconv.ParseUint(rawPort, 10, 16)
			require.NoError(t, err)
			certificate := server.Certificate()
			roots := x509.NewCertPool()
			roots.AddCert(certificate)
			pinDigest := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
			pin := hex.EncodeToString(pinDigest[:])
			if test.pin != "" {
				pin = test.pin
			}
			rootCAs := roots
			if !test.root && test.name == "tls_verification_failed" {
				rootCAs = nil
			}
			address := test.addr
			if address == "" {
				address = "127.0.0.1"
			}
			resolver := syntheticResolver{answer: netip.MustParseAddr(address)}
			secret := capSecrets{value: "synthetic-cap-key"}
			if test.secret != "" {
				secret = capSecrets{err: errors.New("credential environment variable is unavailable")}
			}
			probeTimeout := test.timeout
			if probeTimeout == 0 {
				probeTimeout = time.Minute
			}
			client, err := capselfhosted.NewClient(capselfhosted.Deployment{
				Origin: "https://example.com:" + strconv.FormatUint(port, 10),
				Egress: providerhttp.EgressPolicy{Scheme: "https", Host: "example.com", Port: uint16(port),
					AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
					ProxyMode:    providerhttp.ProxyDisabled,
					TLS:          providerhttp.TLSPolicy{RootCAs: rootCAs, SPKISHA256: []string{pin}}},
				CredentialBinding: "credential:cap", DeploymentRevision: "v-synthetic", ProbeTimeout: probeTimeout,
			}, secret, resolver)
			require.NoError(t, err)
			evidence, err := client.Probe(t.Context())
			require.NoError(t, err)
			assert.Equal(t, test.state, evidence.State)
			assert.Equal(t, capselfhosted.AdapterContract, evidence.AdapterContract)
			assert.Equal(t, "v-synthetic", evidence.DeploymentRevision)
			assert.NotContains(t, fmt.Sprint(evidence), "synthetic-cap-key")
			if test.name == "dns_denied" || test.name == "credential_missing" {
				assert.Zero(t, requests.Load())
			}
		})
	}
}

func TestProbePreservesCancellationDuringBodyRead(t *testing.T) {
	headersSent := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush response: %v", err)
			return
		}
		close(headersSent)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	_, rawPort, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.ParseUint(rawPort, 10, 16)
	require.NoError(t, err)
	certificate := server.Certificate()
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	pinDigest := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	client, err := capselfhosted.NewClient(capselfhosted.Deployment{
		Origin: "https://example.com:" + strconv.FormatUint(port, 10),
		Egress: providerhttp.EgressPolicy{Scheme: "https", Host: "example.com", Port: uint16(port),
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			ProxyMode:    providerhttp.ProxyDisabled,
			TLS:          providerhttp.TLSPolicy{RootCAs: roots, SPKISHA256: []string{hex.EncodeToString(pinDigest[:])}}},
		CredentialBinding: "credential:cap", DeploymentRevision: "v-synthetic", ProbeTimeout: time.Minute,
	}, capSecrets{value: "synthetic-cap-key"}, syntheticResolver{answer: netip.MustParseAddr("127.0.0.1")})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, probeErr := client.Probe(ctx)
		done <- probeErr
	}()
	<-headersSent
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

type capSecrets struct {
	value string
	err   error
}

func (secrets capSecrets) ResolveSecret(context.Context, string) (string, error) {
	if secrets.err != nil {
		return "", secrets.err
	}
	return secrets.value, nil
}

type syntheticResolver struct {
	answer netip.Addr
}

func (resolver syntheticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{resolver.answer}, nil
}
