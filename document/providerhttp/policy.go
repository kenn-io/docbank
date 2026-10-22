// Package providerhttp constructs HTTP transports whose network destination is
// bound to an explicit provider egress policy.
package providerhttp

import (
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

const (
	// DefaultConnectTimeout bounds one TCP connection attempt.
	DefaultConnectTimeout = 30 * time.Second
	// DefaultKeepAlive preserves the standard library's TCP keepalive cadence.
	DefaultKeepAlive = 30 * time.Second
	// DefaultTLSHandshakeTimeout bounds a provider TLS handshake.
	DefaultTLSHandshakeTimeout = 10 * time.Second
	maxTransportTimeout        = 5 * time.Minute
)

// ProxyMode describes whether a provider transport may use a proxy.
type ProxyMode string

const (
	// ProxyDisabled makes all connections directly to a policy-approved IP.
	// Environment proxy variables are never consulted.
	ProxyDisabled ProxyMode = "disabled"
)

// TLSPolicy contains optional certificate authority and SPKI pin restrictions.
// Normal hostname and chain verification always runs before a pin is checked.
type TLSPolicy struct {
	RootCAs    *x509.CertPool
	SPKISHA256 []string
}

// EgressPolicy authorizes one exact scheme, host, and port plus every network
// prefix to which that hostname is allowed to resolve.
type EgressPolicy struct {
	Scheme              string
	Host                string
	Port                uint16
	AllowedCIDRs        []netip.Prefix
	ProxyMode           ProxyMode
	ConnectTimeout      time.Duration
	KeepAlive           time.Duration
	TLSHandshakeTimeout time.Duration
	TLS                 TLSPolicy
}

type validatedPolicy struct {
	scheme       string
	host         string
	port         uint16
	allowedCIDRs []netip.Prefix
	rootCAs      *x509.CertPool
	spkiPins     [][32]byte
	connect      time.Duration
	keepAlive    time.Duration
	tlsHandshake time.Duration
}

func validatePolicy(policy EgressPolicy) (validatedPolicy, error) {
	if policy.Scheme != "http" && policy.Scheme != "https" {
		return validatedPolicy{}, errors.New("provider egress scheme must be http or https")
	}
	host, err := normalizeHost(policy.Host)
	if err != nil {
		return validatedPolicy{}, err
	}
	if policy.Port == 0 {
		return validatedPolicy{}, errors.New("provider egress port is required")
	}
	if policy.ProxyMode != "" && policy.ProxyMode != ProxyDisabled {
		return validatedPolicy{}, errors.New("provider egress supports only direct connections")
	}
	if policy.ConnectTimeout == 0 {
		policy.ConnectTimeout = DefaultConnectTimeout
	}
	if policy.KeepAlive == 0 {
		policy.KeepAlive = DefaultKeepAlive
	}
	if policy.TLSHandshakeTimeout == 0 {
		policy.TLSHandshakeTimeout = DefaultTLSHandshakeTimeout
	}
	if policy.ConnectTimeout < 0 || policy.ConnectTimeout > maxTransportTimeout ||
		policy.KeepAlive < 0 || policy.KeepAlive > maxTransportTimeout ||
		policy.TLSHandshakeTimeout < 0 || policy.TLSHandshakeTimeout > maxTransportTimeout {
		return validatedPolicy{}, errors.New("provider egress transport timeout is outside package bounds")
	}
	if len(policy.AllowedCIDRs) == 0 {
		return validatedPolicy{}, errors.New("provider egress requires at least one allowed CIDR")
	}
	allowed := make([]netip.Prefix, len(policy.AllowedCIDRs))
	for index, prefix := range policy.AllowedCIDRs {
		if !prefix.IsValid() || prefix.Addr().Zone() != "" {
			return validatedPolicy{}, fmt.Errorf("provider egress CIDR %d is invalid", index)
		}
		allowed[index] = prefix.Masked()
	}
	if policy.Scheme != "https" && (policy.TLS.RootCAs != nil || len(policy.TLS.SPKISHA256) != 0) {
		return validatedPolicy{}, errors.New("provider egress TLS policy requires an https scheme")
	}
	pins := make([][32]byte, len(policy.TLS.SPKISHA256))
	for index, encoded := range policy.TLS.SPKISHA256 {
		decoded, err := hex.DecodeString(encoded)
		if err != nil || len(decoded) != len(pins[index]) {
			return validatedPolicy{}, fmt.Errorf("provider egress SPKI pin %d must be a SHA-256 hex digest", index)
		}
		copy(pins[index][:], decoded)
	}
	var roots *x509.CertPool
	if policy.TLS.RootCAs != nil {
		roots = policy.TLS.RootCAs.Clone()
	}
	return validatedPolicy{
		scheme: policy.Scheme, host: host, port: policy.Port,
		allowedCIDRs: allowed, rootCAs: roots, spkiPins: pins,
		connect: policy.ConnectTimeout, keepAlive: policy.KeepAlive,
		tlsHandshake: policy.TLSHandshakeTimeout,
	}, nil
}

func normalizeHost(host string) (string, error) {
	if host == "" || host != strings.TrimSpace(host) {
		return "", errors.New("provider egress host is required without surrounding whitespace")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Zone() != "" {
			return "", errors.New("provider egress IP host cannot contain a zone")
		}
		return address.Unmap().String(), nil
	}
	if len(host) > 253 || strings.ContainsAny(host, ":/\\?#@[]") {
		return "", errors.New("provider egress host must be a bare DNS name or IP address")
	}
	host = strings.ToLower(host)
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("provider egress host is not a valid DNS name")
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') &&
				(character < '0' || character > '9') && character != '-' {
				return "", errors.New("provider egress host is not a valid ASCII DNS name")
			}
		}
	}
	return host, nil
}
