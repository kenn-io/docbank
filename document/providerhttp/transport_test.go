package providerhttp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/providerhttp"
)

func TestEgressDialsResolvedIPWithoutASecondLookup(t *testing.T) {
	var hostHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostHeader = r.Host
		_, _ = io.WriteString(w, "direct")
	}))
	defer server.Close()
	host, port := endpoint(t, server.Listener.Addr())
	resolver := &recordingResolver{answers: [][]netip.Addr{{netip.MustParseAddr(host)}}}
	transport := newHTTPTransport(t, port, resolver)
	client := &http.Client{Transport: transport, CheckRedirect: providerhttp.RefuseRedirects}

	response, err := client.Get("http://provider.invalid:" + strconv.Itoa(int(port)) + "/source")
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	assert.Equal(t, "direct", string(body))
	assert.Equal(t, "provider.invalid:"+strconv.Itoa(int(port)), hostHeader)
	assert.Equal(t, []string{"provider.invalid"}, resolver.hosts())
}

func TestEgressAcceptsAnEquivalentZeroPaddedPort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "provider")
	}))
	defer server.Close()
	host, port := endpoint(t, server.Listener.Addr())
	transport := newHTTPTransport(t, port, &recordingResolver{
		answers: [][]netip.Addr{{netip.MustParseAddr(host)}},
	})

	response, err := (&http.Client{Transport: transport}).Get(
		"http://provider.invalid:0" + strconv.Itoa(int(port)),
	)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, "provider", string(body))
}

func TestEgressIgnoresAmbientProxy(t *testing.T) {
	var proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyRequests.Add(1)
		http.Error(w, "proxy used", http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "origin")
	}))
	defer server.Close()
	host, port := endpoint(t, server.Listener.Addr())
	transport := newHTTPTransport(t, port, &recordingResolver{
		answers: [][]netip.Addr{{netip.MustParseAddr(host)}},
	})

	response, err := (&http.Client{Transport: transport}).Get(
		"http://provider.invalid:" + strconv.Itoa(int(port)),
	)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	assert.Equal(t, "origin", string(body))
	assert.Zero(t, proxyRequests.Load())
}

func TestEgressRejectsMixedAllowedAndDeniedDNSAnswers(t *testing.T) {
	resolver := &recordingResolver{answers: [][]netip.Addr{{
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("203.0.113.10"),
	}}}
	transport, err := providerhttp.NewTransport(providerhttp.EgressPolicy{
		Scheme: "http", Host: "provider.invalid", Port: 8080,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		ProxyMode:    providerhttp.ProxyDisabled,
	}, resolver)
	require.NoError(t, err)

	response, err := (&http.Client{Transport: transport}).Get("http://provider.invalid:8080/")
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorIs(t, err, providerhttp.ErrAddressDenied)
	assert.Equal(t, []string{"provider.invalid"}, resolver.hosts())
}

func TestEgressRevalidatesDNSOnEachConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "first")
	}))
	defer server.Close()
	host, port := endpoint(t, server.Listener.Addr())
	resolver := &recordingResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr(host)},
		{netip.MustParseAddr("203.0.113.10")},
	}}
	transport := newHTTPTransport(t, port, resolver)
	transport.DisableKeepAlives = true
	client := &http.Client{Transport: transport}
	url := "http://provider.invalid:" + strconv.Itoa(int(port)) + "/"

	first, err := client.Get(url)
	require.NoError(t, err)
	require.NoError(t, first.Body.Close())
	second, err := client.Get(url)
	if second != nil {
		require.NoError(t, second.Body.Close())
	}

	require.ErrorIs(t, err, providerhttp.ErrAddressDenied)
	assert.Equal(t, []string{"provider.invalid", "provider.invalid"}, resolver.hosts())
}

func TestEgressFallsBackAcrossApprovedDNSAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fallback")
	}))
	defer server.Close()
	_, port := endpoint(t, server.Listener.Addr())
	resolver := &recordingResolver{answers: [][]netip.Addr{{
		netip.MustParseAddr("127.0.0.2"),
		netip.MustParseAddr("127.0.0.1"),
	}}}
	transport := newHTTPTransport(t, port, resolver)

	response, err := (&http.Client{Transport: transport}).Get(
		"http://provider.invalid:" + strconv.Itoa(int(port)),
	)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, "fallback", string(body))
	assert.Equal(t, []string{"provider.invalid"}, resolver.hosts())
}

func TestEgressRejectsUnapprovedDestinationBeforeResolution(t *testing.T) {
	resolver := &recordingResolver{answers: [][]netip.Addr{{netip.MustParseAddr("127.0.0.1")}}}
	transport, err := providerhttp.NewTransport(providerhttp.EgressPolicy{
		Scheme: "http", Host: "provider.invalid", Port: 8080,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}, resolver)
	require.NoError(t, err)
	client := &http.Client{Transport: transport}

	for _, rawURL := range []string{
		"http://other.invalid:8080/",
		"http://provider.invalid:8081/",
		"https://provider.invalid:8080/",
	} {
		response, err := client.Get(rawURL)
		if response != nil {
			require.NoError(t, response.Body.Close())
		}
		require.ErrorIs(t, err, providerhttp.ErrDestinationDenied, rawURL)
	}
	request, err := http.NewRequest(http.MethodGet, "http://provider.invalid:8080/", nil)
	require.NoError(t, err)
	request.Host = "other.invalid:8080"
	response, err := client.Do(request)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorIs(t, err, providerhttp.ErrDestinationDenied)
	assert.Empty(t, resolver.hosts())
}

func TestEgressUsesLiteralIPWithoutDNS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "literal")
	}))
	defer server.Close()
	host, port := endpoint(t, server.Listener.Addr())
	resolver := &recordingResolver{}
	transport, err := providerhttp.NewTransport(providerhttp.EgressPolicy{
		Scheme: "http", Host: host, Port: port,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}, resolver)
	require.NoError(t, err)

	response, err := (&http.Client{Transport: transport}).Get(server.URL)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Empty(t, resolver.hosts())
}

func TestEgressPreservesTLSIdentityAndEnforcesSPKIPin(t *testing.T) {
	certificate, roots, pin := providerCertificate(t, "provider.invalid")
	var sni string
	var hostHeader string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostHeader = r.Host
		_, _ = io.WriteString(w, "secure")
	}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = hello.ServerName
			return nil, nil //nolint:nilnil // nil keeps this server's configured certificate.
		},
	}
	server.StartTLS()
	defer server.Close()
	host, port := endpoint(t, server.Listener.Addr())
	resolver := &recordingResolver{answers: [][]netip.Addr{{netip.MustParseAddr(host)}}}
	policy := providerhttp.EgressPolicy{
		Scheme: "https", Host: "provider.invalid", Port: port,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		TLS:          providerhttp.TLSPolicy{RootCAs: roots, SPKISHA256: []string{pin}},
	}
	transport, err := providerhttp.NewTransport(policy, resolver)
	require.NoError(t, err)
	client := &http.Client{Transport: transport}
	url := "https://provider.invalid:" + strconv.Itoa(int(port)) + "/result"

	response, err := client.Get(url)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, "provider.invalid", sni)
	assert.Equal(t, "provider.invalid:"+strconv.Itoa(int(port)), hostHeader)

	policy.TLS.SPKISHA256 = []string{hex.EncodeToString(make([]byte, sha256.Size))}
	transport, err = providerhttp.NewTransport(policy, resolver)
	require.NoError(t, err)
	response, err = (&http.Client{Transport: transport}).Get(url)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorIs(t, err, providerhttp.ErrCertificatePin)
}

func TestEgressBoundsTLSHandshake(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	_, port := endpoint(t, listener.Addr())
	transport, err := providerhttp.NewTransport(providerhttp.EgressPolicy{
		Scheme: "https", Host: "127.0.0.1", Port: port,
		AllowedCIDRs:        []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		TLSHandshakeTimeout: 25 * time.Millisecond,
	}, &recordingResolver{})
	require.NoError(t, err)
	started := time.Now()

	response, err := (&http.Client{Transport: transport}).Get(
		"https://127.0.0.1:" + strconv.Itoa(int(port)),
	)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
	select {
	case connection := <-accepted:
		require.NoError(t, connection.Close())
	case <-time.After(time.Second):
		require.Fail(t, "provider connection was not accepted")
	}
}

func TestEgressPolicyIsImmutableAfterTransportCreation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "immutable")
	}))
	defer server.Close()
	host, port := endpoint(t, server.Listener.Addr())
	allowed := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	policy := providerhttp.EgressPolicy{
		Scheme: "http", Host: "provider.invalid", Port: port, AllowedCIDRs: allowed,
	}
	transport, err := providerhttp.NewTransport(policy, &recordingResolver{
		answers: [][]netip.Addr{{netip.MustParseAddr(host)}},
	})
	require.NoError(t, err)
	allowed[0] = netip.MustParsePrefix("203.0.113.0/24")

	response, err := (&http.Client{Transport: transport}).Get(
		"http://provider.invalid:" + strconv.Itoa(int(port)),
	)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
}

func TestEgressRefusesRedirectReplay(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirected.Add(1)
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	host, port := endpoint(t, server.Listener.Addr())
	transport := newHTTPTransport(t, port, &recordingResolver{
		answers: [][]netip.Addr{{netip.MustParseAddr(host)}},
	})
	client := &http.Client{Transport: transport, CheckRedirect: providerhttp.RefuseRedirects}

	response, err := client.Get("http://provider.invalid:" + strconv.Itoa(int(port)))
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	assert.Zero(t, redirected.Load())
}

func TestEgressValidatesPolicy(t *testing.T) {
	valid := providerhttp.EgressPolicy{
		Scheme: "https", Host: "provider.invalid", Port: 443,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		ProxyMode:    providerhttp.ProxyDisabled,
	}
	for _, test := range []struct {
		name   string
		mutate func(*providerhttp.EgressPolicy)
	}{
		{name: "scheme", mutate: func(policy *providerhttp.EgressPolicy) { policy.Scheme = "ftp" }},
		{name: "host", mutate: func(policy *providerhttp.EgressPolicy) { policy.Host = "" }},
		{name: "host with port", mutate: func(policy *providerhttp.EgressPolicy) { policy.Host = "provider.invalid:443" }},
		{name: "port", mutate: func(policy *providerhttp.EgressPolicy) { policy.Port = 0 }},
		{name: "CIDRs", mutate: func(policy *providerhttp.EgressPolicy) { policy.AllowedCIDRs = nil }},
		{name: "proxy", mutate: func(policy *providerhttp.EgressPolicy) { policy.ProxyMode = "environment" }},
		{name: "pin", mutate: func(policy *providerhttp.EgressPolicy) { policy.TLS.SPKISHA256 = []string{"bad"} }},
		{name: "connect timeout", mutate: func(policy *providerhttp.EgressPolicy) { policy.ConnectTimeout = -time.Second }},
		{name: "keepalive", mutate: func(policy *providerhttp.EgressPolicy) { policy.KeepAlive = -time.Second }},
		{name: "TLS handshake timeout", mutate: func(policy *providerhttp.EgressPolicy) { policy.TLSHandshakeTimeout = -time.Second }},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := valid
			test.mutate(&policy)
			_, err := providerhttp.NewTransport(policy, &recordingResolver{})
			require.Error(t, err)
		})
	}
}

func newHTTPTransport(
	t *testing.T,
	port uint16,
	resolver providerhttp.Resolver,
) *http.Transport {
	t.Helper()
	transport, err := providerhttp.NewTransport(providerhttp.EgressPolicy{
		Scheme: "http", Host: "provider.invalid", Port: port,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		ProxyMode:    providerhttp.ProxyDisabled,
	}, resolver)
	require.NoError(t, err)
	return transport
}

func endpoint(t *testing.T, address net.Addr) (string, uint16) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(address.String())
	require.NoError(t, err)
	port, err := strconv.ParseUint(rawPort, 10, 16)
	require.NoError(t, err)
	return host, uint16(port)
}

type recordingResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	calls   []string
}

func (resolver *recordingResolver) LookupNetIP(
	_ context.Context,
	network string,
	host string,
) ([]netip.Addr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.calls = append(resolver.calls, host)
	if network != "ip" {
		return nil, assert.AnError
	}
	if len(resolver.answers) == 0 {
		return nil, assert.AnError
	}
	answer := resolver.answers[0]
	if len(resolver.answers) > 1 {
		resolver.answers = resolver.answers[1:]
	}
	return append([]netip.Addr(nil), answer...), nil
}

func (resolver *recordingResolver) hosts() []string {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return append([]string(nil), resolver.calls...)
}

func providerCertificate(t *testing.T, hostname string) (tls.Certificate, *x509.CertPool, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	digest := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	return certificate, roots, hex.EncodeToString(digest[:])
}
