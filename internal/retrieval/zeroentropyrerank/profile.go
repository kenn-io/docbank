// Package zeroentropyrerank implements the fixed hosted ZeroEntropy zerank-2 contract.
package zeroentropyrerank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document/providerhttp"
)

const (
	Model                = "zerank-2"
	host                 = "api.zeroentropy.dev"
	origin               = "https://api.zeroentropy.dev"
	rerankPath           = "/v1/models/rerank"
	adapterContract      = "docbank-zeroentropy-zerank-2/v1"
	maximumCandidates    = 2048
	maximumQueryBytes    = 64 << 10
	maximumExcerptBytes  = 1 << 20
	maximumTotalExcerpts = int64(5_000_000)
	maximumRequestBytes  = int64(16 << 20)
	maximumResponseBytes = int64(64 << 20)
	maximumTimeout       = 5 * time.Minute
	maximumTokenBytes    = 128
)

type Latency string

const (
	LatencyAuto Latency = "auto"
	LatencyFast Latency = "fast"
	LatencySlow Latency = "slow"
)

type SecretResolver interface {
	ResolveSecret(ctx context.Context, binding string) (string, error)
}

type Profile struct {
	ID                   string
	Model                string
	CompatibilityEpoch   string
	ModelRevision        string
	SecretBinding        string
	Latency              Latency
	RequestTimeout       time.Duration
	MaxCandidates        int
	MaxQueryBytes        int
	MaxExcerptBytes      int
	MaxTotalExcerptBytes int64
	MaxRequestBytes      int64
	MaxResponseBytes     int64
	EgressPolicy         providerhttp.EgressPolicy
}

type Client struct {
	profile     Profile
	fingerprint string
	secrets     SecretResolver
	http        *http.Client
}

type policyIdentity struct {
	AdapterContract      string         `json:"adapter_contract"`
	Origin               string         `json:"origin"`
	Route                string         `json:"route"`
	ID                   string         `json:"id"`
	Model                string         `json:"model"`
	CompatibilityEpoch   string         `json:"compatibility_epoch"`
	ModelRevision        string         `json:"model_revision"`
	SecretBinding        string         `json:"secret_binding"`
	Latency              Latency        `json:"latency"`
	RequestTimeout       int64          `json:"request_timeout_nanos"`
	MaxCandidates        int            `json:"max_candidates"`
	MaxQueryBytes        int            `json:"max_query_bytes"`
	MaxExcerptBytes      int            `json:"max_excerpt_bytes"`
	MaxTotalExcerptBytes int64          `json:"max_total_excerpt_bytes"`
	MaxRequestBytes      int64          `json:"max_request_bytes"`
	MaxResponseBytes     int64          `json:"max_response_bytes"`
	Egress               egressIdentity `json:"egress"`
}

type egressIdentity struct {
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

func PolicyFingerprint(profile Profile) (string, error) {
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(policyIdentity{AdapterContract: adapterContract, Origin: origin, Route: rerankPath,
		ID: normalized.ID, Model: normalized.Model, CompatibilityEpoch: normalized.CompatibilityEpoch,
		ModelRevision: normalized.ModelRevision, SecretBinding: normalized.SecretBinding, Latency: normalized.Latency,
		RequestTimeout: int64(normalized.RequestTimeout), MaxCandidates: normalized.MaxCandidates,
		MaxQueryBytes: normalized.MaxQueryBytes, MaxExcerptBytes: normalized.MaxExcerptBytes,
		MaxTotalExcerptBytes: normalized.MaxTotalExcerptBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes, Egress: profileEgressIdentity(normalized.EgressPolicy)}, json.Deterministic(true))
	if err != nil {
		return "", errors.New("zeroentropy rerank: policy identity encoding failed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func New(profile Profile, secrets SecretResolver, resolver providerhttp.Resolver, supplied *http.Client) (*Client, error) {
	if supplied == nil {
		return nil, errors.New("zeroentropy rerank: HTTP client settings source is required")
	}
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	if nilInterface(secrets) {
		return nil, errors.New("zeroentropy rerank: named API-key resolver is required")
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, errors.New("zeroentropy rerank: sealed egress policy is invalid")
	}
	isolated := *supplied
	isolated.Transport = transport
	isolated.CheckRedirect = providerhttp.RefuseRedirects
	isolated.Jar = nil
	isolated.Timeout = 0
	return &Client{profile: normalized, fingerprint: fingerprint, secrets: secrets, http: &isolated}, nil
}

func (client *Client) ProfileID() string {
	if client == nil {
		return ""
	}
	return client.profile.ID
}
func (client *Client) Model() string {
	if client == nil {
		return ""
	}
	return client.profile.Model
}
func (client *Client) PolicyFingerprint() string {
	if client == nil {
		return ""
	}
	return client.fingerprint
}

func normalizeProfile(profile Profile) (Profile, error) {
	profile.EgressPolicy.AllowedCIDRs = slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	profile.EgressPolicy.TLS.SPKISHA256 = slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
	if profile.Latency == "" {
		profile.Latency = LatencyAuto
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = 30 * time.Second
	}
	if profile.MaxCandidates == 0 {
		profile.MaxCandidates = 100
	}
	if profile.MaxQueryBytes == 0 {
		profile.MaxQueryBytes = 4096
	}
	if profile.MaxExcerptBytes == 0 {
		profile.MaxExcerptBytes = 64 << 10
	}
	if profile.MaxTotalExcerptBytes == 0 {
		profile.MaxTotalExcerptBytes = 4 << 20
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = 8 << 20
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = 8 << 20
	}
	if !validToken(profile.ID) || profile.Model != Model || !validToken(profile.CompatibilityEpoch) ||
		profile.ModelRevision != profile.CompatibilityEpoch || !validToken(profile.SecretBinding) ||
		(profile.Latency != LatencyAuto && profile.Latency != LatencyFast && profile.Latency != LatencySlow) ||
		profile.RequestTimeout <= 0 || profile.RequestTimeout > maximumTimeout ||
		profile.MaxCandidates < 1 || profile.MaxCandidates > maximumCandidates ||
		profile.MaxQueryBytes < 1 || profile.MaxQueryBytes > maximumQueryBytes ||
		profile.MaxExcerptBytes < 1 || profile.MaxExcerptBytes > maximumExcerptBytes ||
		profile.MaxTotalExcerptBytes < int64(profile.MaxExcerptBytes) || profile.MaxTotalExcerptBytes > maximumTotalExcerpts ||
		profile.MaxRequestBytes < 1 || profile.MaxRequestBytes > maximumRequestBytes ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maximumResponseBytes {
		return Profile{}, errors.New("zeroentropy rerank: profile is invalid")
	}
	if err := normalizeEgress(&profile.EgressPolicy); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func normalizeEgress(policy *providerhttp.EgressPolicy) error {
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
	if policy.Scheme != "https" || policy.Host != host || policy.Port != 443 || policy.ProxyMode != providerhttp.ProxyDisabled || policy.TLS.RootCAs != nil {
		return errors.New("zeroentropy rerank: egress authority must be exactly api.zeroentropy.dev:443")
	}
	for index := range policy.AllowedCIDRs {
		policy.AllowedCIDRs[index] = policy.AllowedCIDRs[index].Masked()
	}
	slices.SortFunc(policy.AllowedCIDRs, func(left, right netip.Prefix) int { return strings.Compare(left.String(), right.String()) })
	for index := 1; index < len(policy.AllowedCIDRs); index++ {
		if policy.AllowedCIDRs[index] == policy.AllowedCIDRs[index-1] {
			return errors.New("zeroentropy rerank: duplicate egress CIDR")
		}
	}
	for index := range policy.TLS.SPKISHA256 {
		policy.TLS.SPKISHA256[index] = strings.ToLower(policy.TLS.SPKISHA256[index])
	}
	slices.Sort(policy.TLS.SPKISHA256)
	for index := 1; index < len(policy.TLS.SPKISHA256); index++ {
		if policy.TLS.SPKISHA256[index] == policy.TLS.SPKISHA256[index-1] {
			return errors.New("zeroentropy rerank: duplicate SPKI pin")
		}
	}
	if _, err := providerhttp.NewTransport(*policy, nil); err != nil {
		return errors.New("zeroentropy rerank: sealed egress policy is invalid")
	}
	return nil
}

func profileEgressIdentity(policy providerhttp.EgressPolicy) egressIdentity {
	cidrs := make([]string, len(policy.AllowedCIDRs))
	for index, prefix := range policy.AllowedCIDRs {
		cidrs[index] = prefix.String()
	}
	return egressIdentity{Scheme: policy.Scheme, Host: policy.Host, Port: policy.Port, AllowedCIDRs: cidrs,
		ProxyMode: string(policy.ProxyMode), ConnectTimeout: int64(policy.ConnectTimeout), KeepAlive: int64(policy.KeepAlive),
		TLSHandshakeTimeout: int64(policy.TLSHandshakeTimeout), SPKISHA256: slices.Clone(policy.TLS.SPKISHA256)}
}

func validToken(value string) bool {
	if value == "" || len(value) > maximumTokenBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return false
		}
	}
	return true
}

func nilInterface(value any) bool {
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
