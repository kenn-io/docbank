package providerhttp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
)

var (
	// ErrDestinationDenied means the requested scheme, host, or port was not
	// the exact destination authorized by the egress policy.
	ErrDestinationDenied = errors.New("provider egress destination denied")
	// ErrAddressDenied means DNS returned at least one address outside every
	// network prefix authorized by the egress policy.
	ErrAddressDenied = errors.New("provider egress address denied")
	// ErrCertificatePin means normal TLS verification succeeded but the leaf
	// certificate did not match an authorized SHA-256 SPKI digest.
	ErrCertificatePin = errors.New("provider egress certificate pin mismatch")
)

// Resolver performs the one DNS lookup allowed for each connection attempt.
type Resolver interface {
	LookupNetIP(ctx context.Context, network string, host string) ([]netip.Addr, error)
}

// RefuseRedirects is an http.Client CheckRedirect function that prevents a
// request body or credential from being replayed, including to the same host.
func RefuseRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// NewTransport returns a transport that resolves once per connection attempt,
// validates every answer, and connects to one exact allowed IP without an
// ambient proxy or a second hostname lookup.
func NewTransport(policy EgressPolicy, resolver Resolver) (*http.Transport, error) {
	validated, err := validatePolicy(policy)
	if err != nil {
		return nil, err
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialer := &policyDialer{
		policy: validated, resolver: resolver,
		dialer: net.Dialer{Timeout: validated.connect, KeepAlive: validated.keepAlive},
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    validated.rootCAs,
		ServerName: validated.host,
	}
	if len(validated.spkiPins) != 0 {
		pins := append([][32]byte(nil), validated.spkiPins...)
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return ErrCertificatePin
			}
			digest := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
			for _, pin := range pins {
				if subtle.ConstantTimeCompare(digest[:], pin[:]) == 1 {
					return nil
				}
			}
			return ErrCertificatePin
		}
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport has an unsupported implementation")
	}
	transport := defaultTransport.Clone()
	transport.Proxy = func(request *http.Request) (*url.URL, error) {
		if err := validated.validateRequest(request); err != nil {
			return nil, err
		}
		return nil, nil //nolint:nilnil // nil is the http.Transport contract for a direct connection.
	}
	transport.TLSClientConfig = tlsConfig
	transport.TLSHandshakeTimeout = validated.tlsHandshake
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if validated.scheme != "http" {
			return nil, fmt.Errorf("%w: plaintext connection", ErrDestinationDenied)
		}
		return dialer.dial(ctx, network, address)
	}
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if validated.scheme != "https" {
			return nil, fmt.Errorf("%w: TLS connection", ErrDestinationDenied)
		}
		connection, err := dialer.dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tlsConnection := tls.Client(connection, tlsConfig.Clone())
		handshakeContext, cancel := context.WithTimeout(ctx, validated.tlsHandshake)
		defer cancel()
		if err := tlsConnection.HandshakeContext(handshakeContext); err != nil {
			handshakeErr := fmt.Errorf("handshake with provider: %w", err)
			if closeErr := connection.Close(); closeErr != nil {
				return nil, errors.Join(handshakeErr, fmt.Errorf("close failed provider connection: %w", closeErr))
			}
			return nil, handshakeErr
		}
		return tlsConnection, nil
	}
	return transport, nil
}

func (policy validatedPolicy) validateRequest(request *http.Request) error {
	if request == nil || request.URL == nil || request.URL.Scheme != policy.scheme ||
		request.URL.User != nil {
		return fmt.Errorf("%w: request scheme or authority", ErrDestinationDenied)
	}
	host, port, err := parseAuthority(request.URL.Host, policy.scheme)
	if err != nil || host != policy.host || port != policy.port {
		return fmt.Errorf("%w: request host or port", ErrDestinationDenied)
	}
	if request.Host != "" {
		host, port, err = parseAuthority(request.Host, policy.scheme)
		if err != nil || host != policy.host || port != policy.port {
			return fmt.Errorf("%w: Host header", ErrDestinationDenied)
		}
	}
	return nil
}

func parseAuthority(authority, scheme string) (string, uint16, error) {
	parsed, err := url.Parse(scheme + "://" + authority)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", 0, errors.New("invalid provider egress authority")
	}
	host, err := normalizeHost(parsed.Hostname())
	if err != nil {
		return "", 0, err
	}
	port := parsed.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", 0, errors.New("invalid provider egress scheme")
		}
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return "", 0, errors.New("invalid provider egress port")
	}
	return host, uint16(parsedPort), nil
}

type policyDialer struct {
	policy   validatedPolicy
	resolver Resolver
	dialer   net.Dialer
}

func (dialer *policyDialer) dial(
	ctx context.Context,
	network string,
	address string,
) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("%w: unsupported network", ErrDestinationDenied)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed address", ErrDestinationDenied)
	}
	host, err = normalizeHost(host)
	parsedPort, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || portErr != nil || parsedPort == 0 ||
		host != dialer.policy.host || uint16(parsedPort) != dialer.policy.port {
		return nil, fmt.Errorf("%w: host or port", ErrDestinationDenied)
	}
	port = strconv.Itoa(int(parsedPort))
	addresses, err := dialer.resolve(ctx)
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, selected := range addresses {
		exactNetwork := "tcp6"
		if selected.Is4() {
			exactNetwork = "tcp4"
		}
		connection, dialErr := dialer.dialer.DialContext(
			ctx, exactNetwork, net.JoinHostPort(selected.String(), port),
		)
		if dialErr == nil {
			return connection, nil
		}
		failures = append(failures, dialErr)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("dial provider IPs: %w", errors.Join(failures...))
}

func (dialer *policyDialer) resolve(ctx context.Context) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(dialer.policy.host); err == nil {
		addresses := []netip.Addr{address.Unmap()}
		if err := dialer.validateAddresses(addresses); err != nil {
			return nil, err
		}
		return addresses, nil
	}
	addresses, err := dialer.resolver.LookupNetIP(ctx, "ip", dialer.policy.host)
	if err != nil {
		return nil, fmt.Errorf("resolve provider egress host: %w", err)
	}
	if err := dialer.validateAddresses(addresses); err != nil {
		return nil, err
	}
	result := make([]netip.Addr, len(addresses))
	for index, address := range addresses {
		result[index] = address.Unmap()
	}
	return result, nil
}

func (dialer *policyDialer) validateAddresses(addresses []netip.Addr) error {
	if len(addresses) == 0 {
		return errors.New("provider egress DNS returned no addresses")
	}
	for _, address := range addresses {
		if !address.IsValid() || address.Zone() != "" {
			return fmt.Errorf("%w: invalid DNS answer", ErrAddressDenied)
		}
		address = address.Unmap()
		allowed := false
		for _, prefix := range dialer.policy.allowedCIDRs {
			if prefix.Contains(address) {
				allowed = true
				break
			}
		}
		if !allowed {
			return ErrAddressDenied
		}
	}
	return nil
}
