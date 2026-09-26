package daemonconn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/docbank/internal/api"
)

const (
	// RemoteAPIVersion is the capability contract supported by this client.
	RemoteAPIVersion             = api.RemoteAPIVersion
	maxCapabilitiesResponseBytes = 64 << 10
)

var (
	// ErrRemoteUnavailable means an explicit target did not return a usable
	// capability response. Callers must not fall back to a local daemon.
	ErrRemoteUnavailable = errors.New("remote daemon unavailable")
	// ErrRemoteUnauthorized means the resolved credential was rejected.
	ErrRemoteUnauthorized = errors.New("remote daemon credential rejected")
	// ErrRemoteCredentialUnavailable means the named private credential could
	// not be resolved to a non-empty value.
	ErrRemoteCredentialUnavailable = errors.New("remote daemon credential unavailable")
	// ErrRemoteIdentityMismatch means the target answered for another vault.
	ErrRemoteIdentityMismatch = errors.New("remote vault identity mismatch")
	// ErrRemoteAPIIncompatible means the target does not implement this remote
	// capability contract.
	ErrRemoteAPIIncompatible = errors.New("remote daemon API incompatible")
)

// RemoteCapabilities is the negotiated, credential-scoped remote contract.
type RemoteCapabilities = api.Capabilities

// RemoteTLSPolicy narrows trust for an explicit remote daemon. Normal hostname
// and certificate-chain verification always runs before optional SPKI pins.
type RemoteTLSPolicy struct {
	RootCAs    *x509.CertPool
	SPKISHA256 []string
}

// RemoteTarget names exactly one daemon and its expected persistent identity.
// CredentialRef is a private credential reference, never a credential value.
type RemoteTarget struct {
	URL              string
	ExpectedVaultUID string
	CredentialRef    string
	TLSPolicy        RemoteTLSPolicy
}

// CredentialResolver resolves one configured reference inside the child
// client which will use it.
type CredentialResolver func(context.Context, string) (string, error)

// ConnectTargetOptions holds process-local seams. EnsureLocal is consulted
// only when target is nil; an explicit remote target is always authoritative.
type ConnectTargetOptions struct {
	ResolveCredential CredentialResolver
	EnsureLocal       func(context.Context) (*Connection, error)
}

// ValidateRemoteURL accepts only credential-free HTTPS endpoints. A path is
// allowed so a remote daemon can live behind a reverse-proxy base path.
func ValidateRemoteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" ||
		strings.ContainsAny(u.Path, "\\\r\n\t") {
		return errors.New("remote target requires an HTTPS origin without credentials, query or fragment")
	}
	return nil
}

// ConnectTarget selects a local daemon only when target is nil. For an
// explicit target it resolves that target's private credential, negotiates the
// capability contract, and installs mutation-time identity checks on the
// returned connection.
func ConnectTarget(
	ctx context.Context, target *RemoteTarget, options ConnectTargetOptions,
) (*Connection, RemoteCapabilities, error) {
	if target == nil {
		ensure := options.EnsureLocal
		if ensure == nil {
			ensure = Ensure
		}
		connection, err := ensure(ctx)
		return connection, RemoteCapabilities{}, err
	}
	if err := ValidateRemoteURL(target.URL); err != nil ||
		target.ExpectedVaultUID == "" || target.CredentialRef == "" {
		return nil, RemoteCapabilities{}, errors.New("invalid remote target configuration")
	}
	if options.ResolveCredential == nil {
		return nil, RemoteCapabilities{}, ErrRemoteCredentialUnavailable
	}
	credential, err := options.ResolveCredential(ctx, target.CredentialRef)
	if err != nil || credential == "" {
		return nil, RemoteCapabilities{}, ErrRemoteCredentialUnavailable
	}

	transport, err := newRemoteTransport(target.TLSPolicy)
	if err != nil {
		return nil, RemoteCapabilities{}, ErrRemoteUnavailable
	}
	baseURL := strings.TrimRight(target.URL, "/")
	capabilities, err := fetchRemoteCapabilities(
		ctx, transport, baseURL, credential, target.ExpectedVaultUID)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, RemoteCapabilities{}, err
	}
	connection := New(baseURL, credential)
	connection.hc = &http.Client{
		Transport: &remoteIdentityTransport{
			next: transport, baseURL: baseURL, credential: credential,
			expectedVaultUID: target.ExpectedVaultUID,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return connection, capabilities, nil
}

func newRemoteTransport(policy RemoteTLSPolicy) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("unsupported default HTTP transport")
	}
	transport := base.Clone()
	transport.Proxy = nil
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if policy.RootCAs != nil {
		tlsConfig.RootCAs = policy.RootCAs.Clone()
	}
	pins := make([][sha256.Size]byte, len(policy.SPKISHA256))
	for index, encoded := range policy.SPKISHA256 {
		decoded, err := hex.DecodeString(encoded)
		if err != nil || len(decoded) != sha256.Size {
			return nil, errors.New("invalid remote TLS policy")
		}
		copy(pins[index][:], decoded)
	}
	if len(pins) != 0 {
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("remote certificate pin mismatch")
			}
			digest := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
			for _, pin := range pins {
				if subtle.ConstantTimeCompare(digest[:], pin[:]) == 1 {
					return nil
				}
			}
			return errors.New("remote certificate pin mismatch")
		}
	}
	transport.TLSClientConfig = tlsConfig
	return transport, nil
}

type remoteIdentityTransport struct {
	next             http.RoundTripper
	baseURL          string
	credential       string
	expectedVaultUID string
}

func (t *remoteIdentityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead &&
		request.Method != http.MethodOptions {
		if _, err := fetchRemoteCapabilities(
			request.Context(), t.next, t.baseURL, t.credential, t.expectedVaultUID,
		); err != nil {
			return nil, err
		}
	}
	return t.next.RoundTrip(request)
}

func (t *remoteIdentityTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func fetchRemoteCapabilities(
	ctx context.Context, transport http.RoundTripper, baseURL, credential, expectedVaultUID string,
) (RemoteCapabilities, error) {
	probe := New(baseURL, credential)
	probe.hc = &http.Client{
		Transport: capabilityResponseTransport{next: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	capabilities, err := probe.API().ReadCapabilities(ctx)
	if err != nil {
		if status, ok := responseStatus(err); ok &&
			(status == http.StatusUnauthorized || status == http.StatusForbidden) {
			return RemoteCapabilities{}, ErrRemoteUnauthorized
		}
		if errors.Is(err, ErrRemoteAPIIncompatible) || IsResponseDecodeError(err) {
			return RemoteCapabilities{}, ErrRemoteAPIIncompatible
		}
		return RemoteCapabilities{}, ErrRemoteUnavailable
	}
	if capabilities.VaultUID == "" || capabilities.APIVersion != RemoteAPIVersion ||
		capabilities.Operations == nil || capabilities.Limits == nil {
		return RemoteCapabilities{}, ErrRemoteAPIIncompatible
	}
	if capabilities.VaultUID != expectedVaultUID {
		return RemoteCapabilities{}, ErrRemoteIdentityMismatch
	}
	return *capabilities, nil
}

// capabilityResponseTransport bounds the generated client's allocation and
// removes remote response text from errors. Status and typed decoding remain
// owned by the generated operation and apiTransport.
type capabilityResponseTransport struct{ next http.RoundTripper }

func (t capabilityResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxCapabilitiesResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || len(body) > maxCapabilitiesResponseBytes {
		return nil, ErrRemoteAPIIncompatible
	}
	if response.StatusCode < http.StatusOK || response.StatusCode > 299 {
		body = nil
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	return response, nil
}
