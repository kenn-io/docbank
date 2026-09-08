package cohereapi

import (
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/docbank/document/providerhttp"
)

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

func NormalizeEgress(policy *providerhttp.EgressPolicy) error {
	if policy.ConnectTimeout == 0 {
		policy.ConnectTimeout = providerhttp.DefaultConnectTimeout
	}
	if policy.KeepAlive == 0 {
		policy.KeepAlive = providerhttp.DefaultKeepAlive
	}
	if policy.TLSHandshakeTimeout == 0 {
		policy.TLSHandshakeTimeout = providerhttp.DefaultTLSHandshakeTimeout
	}
	if policy.ProxyMode == "" {
		policy.ProxyMode = providerhttp.ProxyDisabled
	}
	if policy.Scheme != "https" || policy.Host != "api.cohere.com" || policy.Port != 443 ||
		policy.ProxyMode != providerhttp.ProxyDisabled || policy.TLS.RootCAs != nil {
		return errors.New("cohere: egress authority must be exactly api.cohere.com:443")
	}
	for index := range policy.AllowedCIDRs {
		policy.AllowedCIDRs[index] = policy.AllowedCIDRs[index].Masked()
	}
	slices.SortFunc(policy.AllowedCIDRs, func(left, right netip.Prefix) int { return strings.Compare(left.String(), right.String()) })
	for index := 1; index < len(policy.AllowedCIDRs); index++ {
		if policy.AllowedCIDRs[index] == policy.AllowedCIDRs[index-1] {
			return errors.New("cohere: egress policy has a duplicate CIDR")
		}
	}
	for index := range policy.TLS.SPKISHA256 {
		policy.TLS.SPKISHA256[index] = strings.ToLower(policy.TLS.SPKISHA256[index])
	}
	slices.Sort(policy.TLS.SPKISHA256)
	for index := 1; index < len(policy.TLS.SPKISHA256); index++ {
		if policy.TLS.SPKISHA256[index] == policy.TLS.SPKISHA256[index-1] {
			return errors.New("cohere: egress policy has a duplicate SPKI pin")
		}
	}
	if _, err := providerhttp.NewTransport(*policy, nil); err != nil {
		return errors.New("cohere: sealed egress policy is invalid")
	}
	return nil
}

func IdentifyEgress(policy providerhttp.EgressPolicy) EgressIdentity {
	cidrs := make([]string, len(policy.AllowedCIDRs))
	for index, prefix := range policy.AllowedCIDRs {
		cidrs[index] = prefix.String()
	}
	return EgressIdentity{Scheme: policy.Scheme, Host: policy.Host, Port: policy.Port,
		AllowedCIDRs: cidrs, ProxyMode: string(policy.ProxyMode), ConnectTimeout: int64(policy.ConnectTimeout),
		KeepAlive: int64(policy.KeepAlive), TLSHandshakeTimeout: int64(policy.TLSHandshakeTimeout),
		SPKISHA256: slices.Clone(policy.TLS.SPKISHA256)}
}

func IsNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
