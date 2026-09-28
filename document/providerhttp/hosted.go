package providerhttp

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
)

// ErrHostedAuthority means the policy does not name the fixed HTTPS host with
// system roots and direct connections. Adapters supply their own diagnostic.
var ErrHostedAuthority = errors.New("hosted egress authority is invalid")

// ErrDuplicateCIDR means two allowed prefixes name the same masked network.
var ErrDuplicateCIDR = errors.New("egress policy has a duplicate CIDR")

// ErrDuplicateSPKI means two pins contain the same case-insensitive digest.
var ErrDuplicateSPKI = errors.New("egress policy has a duplicate SPKI pin")

// NormalizeHostedEgress defaults and canonicalizes a fixed hosted policy in
// place. Callers must clone slices first when retaining the supplied policy.
func NormalizeHostedEgress(policy *EgressPolicy, host string) error {
	if policy.ConnectTimeout == 0 {
		policy.ConnectTimeout = DefaultConnectTimeout
	}
	if policy.KeepAlive == 0 {
		policy.KeepAlive = DefaultKeepAlive
	}
	if policy.TLSHandshakeTimeout == 0 {
		policy.TLSHandshakeTimeout = DefaultTLSHandshakeTimeout
	}
	if policy.ProxyMode == "" {
		policy.ProxyMode = ProxyDisabled
	}
	if policy.Scheme != "https" || policy.Host != host || policy.Port != 443 ||
		policy.ProxyMode != ProxyDisabled || policy.TLS.RootCAs != nil {
		return ErrHostedAuthority
	}
	for index := range policy.AllowedCIDRs {
		policy.AllowedCIDRs[index] = policy.AllowedCIDRs[index].Masked()
	}
	slices.SortFunc(policy.AllowedCIDRs, func(left, right netip.Prefix) int { return strings.Compare(left.String(), right.String()) })
	for index := 1; index < len(policy.AllowedCIDRs); index++ {
		if policy.AllowedCIDRs[index] == policy.AllowedCIDRs[index-1] {
			return ErrDuplicateCIDR
		}
	}
	for index := range policy.TLS.SPKISHA256 {
		policy.TLS.SPKISHA256[index] = strings.ToLower(policy.TLS.SPKISHA256[index])
	}
	slices.Sort(policy.TLS.SPKISHA256)
	for index := 1; index < len(policy.TLS.SPKISHA256); index++ {
		if policy.TLS.SPKISHA256[index] == policy.TLS.SPKISHA256[index-1] {
			return ErrDuplicateSPKI
		}
	}
	if _, err := NewTransport(*policy, nil); err != nil {
		return errors.New("sealed egress policy is invalid")
	}
	return nil
}

// EgressIdentity is the canonical egress portion of a hosted profile identity.
type EgressIdentity struct {
	Scheme              string   `json:"scheme"`
	Host                string   `json:"host"`
	Port                uint16   `json:"port"`
	AllowedCIDRs        []string `json:"allowed_cidrs"`
	ProxyMode           string   `json:"proxy_mode"`
	ConnectTimeout      int64    `json:"connect_timeout_nanos"`
	KeepAlive           int64    `json:"keep_alive_nanos"`
	TLSHandshakeTimeout int64    `json:"tls_handshake_timeout_nanos"`
	SPKISHA256          []string `json:"spki_sha256,omitempty"`
}

// IdentifyEgress projects a normalized policy without retaining its slices.
func IdentifyEgress(policy EgressPolicy) EgressIdentity {
	cidrs := make([]string, len(policy.AllowedCIDRs))
	for index, prefix := range policy.AllowedCIDRs {
		cidrs[index] = prefix.String()
	}
	return EgressIdentity{Scheme: policy.Scheme, Host: policy.Host, Port: policy.Port,
		AllowedCIDRs: cidrs, ProxyMode: string(policy.ProxyMode), ConnectTimeout: int64(policy.ConnectTimeout),
		KeepAlive: int64(policy.KeepAlive), TLSHandshakeTimeout: int64(policy.TLSHandshakeTimeout),
		SPKISHA256: slices.Clone(policy.TLS.SPKISHA256)}
}
