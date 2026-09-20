// Package typesafe implements the fixed hosted TypeSafe Jev reranking contract.
package typesafe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	ProviderName = "typesafe"
	ModelJev113  = "jev-1.13.0"

	host            = "api.typesafe.ai"
	origin          = "https://api.typesafe.ai"
	systemOnePath   = "/v1/systemone"
	adapterContract = "docbank-typesafe-jev-1.13.0/v1"

	defaultRequestTimeout    = 30 * time.Second
	maximumRequestTimeout    = 5 * time.Minute
	defaultMaxCandidates     = 100
	maximumMaxCandidates     = document.MaxRetrievalCandidateLimit
	defaultMaxQueryBytes     = 4096
	maximumMaxQueryBytes     = 64 << 10
	defaultMaxCandidateBytes = 4096
	maximumMaxCandidateBytes = 64 << 10
	defaultMaxRequestBytes   = int64(256 << 10)
	// Leave room for JSON escaping of a full batch at the default text limits.
	defaultMaxBatchedRequestBytes = int64(4 << 20)
	maximumMaxRequestBytes        = int64(8 << 20)
	defaultMaxResponseBytes       = int64(1 << 20)
	maximumMaxResponseBytes       = int64(8 << 20)
	defaultMaxConcurrent          = 8
	maximumMaxConcurrent          = 64
	maximumTokenBytes             = 128
)

// RequestShape selects how candidate texts reach the System One endpoint.
type RequestShape string

const (
	RequestShapePerCandidate RequestShape = "per_candidate"
	RequestShapeBatched      RequestShape = "batched"
)

// SecretResolver resolves the one profile-bound named credential.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, binding string) (string, error)
}

// Profile fixes the provider contract, request bounds, and egress policy.
type Profile struct {
	Model              string
	SecretBinding      string
	RequestShape       RequestShape
	RequestTimeout     time.Duration
	MaxCandidates      int
	MaxQueryBytes      int
	MaxCandidateBytes  int
	MaxRequestBytes    int64
	MaxResponseBytes   int64
	MaxConcurrentCalls int
	EgressPolicy       providerhttp.EgressPolicy
}

// Client is a bounded TypeSafe System One client.
type Client struct {
	profile     Profile
	fingerprint string
	secrets     SecretResolver
	http        *http.Client
}

type policyIdentity struct {
	AdapterContract    string         `json:"adapter_contract"`
	Origin             string         `json:"origin"`
	Route              string         `json:"route"`
	Model              string         `json:"model"`
	SecretBinding      string         `json:"secret_binding"`
	RequestShape       RequestShape   `json:"request_shape"`
	RequestTimeout     int64          `json:"request_timeout_nanos"`
	MaxCandidates      int            `json:"max_candidates"`
	MaxQueryBytes      int            `json:"max_query_bytes"`
	MaxCandidateBytes  int            `json:"max_candidate_bytes"`
	MaxRequestBytes    int64          `json:"max_request_bytes"`
	MaxResponseBytes   int64          `json:"max_response_bytes"`
	MaxConcurrentCalls int            `json:"max_concurrent_calls"`
	Egress             egressIdentity `json:"egress"`
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

// PolicyFingerprint returns the canonical identity of the effective profile.
func PolicyFingerprint(profile Profile) (string, error) {
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(policyIdentity{
		AdapterContract: adapterContract, Origin: origin, Route: systemOnePath,
		Model: normalized.Model, SecretBinding: normalized.SecretBinding,
		RequestShape: normalized.RequestShape, RequestTimeout: int64(normalized.RequestTimeout),
		MaxCandidates: normalized.MaxCandidates, MaxQueryBytes: normalized.MaxQueryBytes,
		MaxCandidateBytes: normalized.MaxCandidateBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes, MaxConcurrentCalls: normalized.MaxConcurrentCalls,
		Egress: profileEgressIdentity(normalized.EgressPolicy),
	}, json.Deterministic(true))
	if err != nil {
		return "", errors.New("typesafe rerank: policy identity encoding failed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// New creates a client with a sealed destination and an isolated HTTP client.
func New(profile Profile, secrets SecretResolver, resolver providerhttp.Resolver, supplied *http.Client) (*Client, error) {
	if supplied == nil {
		return nil, errors.New("typesafe rerank: HTTP client settings source is required")
	}
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	if providerutil.IsNil(secrets) {
		return nil, errors.New("typesafe rerank: named API-key resolver is required")
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, errors.New("typesafe rerank: sealed egress policy is invalid")
	}
	isolated := *supplied
	isolated.Transport = transport
	isolated.CheckRedirect = providerhttp.RefuseRedirects
	isolated.Jar = nil
	isolated.Timeout = 0
	return &Client{profile: normalized, fingerprint: fingerprint, secrets: secrets, http: &isolated}, nil
}

// PolicyFingerprint returns the client's effective profile identity.
func (client *Client) PolicyFingerprint() string {
	if client == nil {
		return ""
	}
	return client.fingerprint
}

// Model returns the pinned model revision.
func (client *Client) Model() string {
	if client == nil {
		return ""
	}
	return client.profile.Model
}

// RequestShape returns the client's effective request shape.
func (client *Client) RequestShape() RequestShape {
	if client == nil {
		return ""
	}
	return client.profile.RequestShape
}

func normalizeProfile(profile Profile) (Profile, error) {
	profile.EgressPolicy.AllowedCIDRs = slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	profile.EgressPolicy.TLS.SPKISHA256 = slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
	if profile.Model == "" {
		profile.Model = ModelJev113
	}
	if profile.RequestShape == "" {
		profile.RequestShape = RequestShapePerCandidate
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = defaultRequestTimeout
	}
	if profile.MaxCandidates == 0 {
		profile.MaxCandidates = defaultMaxCandidates
	}
	if profile.MaxQueryBytes == 0 {
		profile.MaxQueryBytes = defaultMaxQueryBytes
	}
	if profile.MaxCandidateBytes == 0 {
		profile.MaxCandidateBytes = defaultMaxCandidateBytes
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = defaultMaxRequestBytes
		if profile.RequestShape == RequestShapeBatched {
			profile.MaxRequestBytes = defaultMaxBatchedRequestBytes
		}
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = defaultMaxResponseBytes
	}
	if profile.MaxConcurrentCalls == 0 {
		profile.MaxConcurrentCalls = defaultMaxConcurrent
	}
	if profile.Model != ModelJev113 || !validToken(profile.SecretBinding) ||
		(profile.RequestShape != RequestShapePerCandidate && profile.RequestShape != RequestShapeBatched) ||
		profile.RequestTimeout <= 0 || profile.RequestTimeout > maximumRequestTimeout ||
		profile.MaxCandidates < 1 || profile.MaxCandidates > maximumMaxCandidates ||
		profile.MaxQueryBytes < 1 || profile.MaxQueryBytes > maximumMaxQueryBytes ||
		profile.MaxCandidateBytes < 1 || profile.MaxCandidateBytes > maximumMaxCandidateBytes ||
		profile.MaxRequestBytes < 1 || profile.MaxRequestBytes > maximumMaxRequestBytes ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maximumMaxResponseBytes ||
		profile.MaxConcurrentCalls < 1 || profile.MaxConcurrentCalls > maximumMaxConcurrent {
		return Profile{}, errors.New("typesafe rerank: profile is invalid")
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
	if policy.Scheme != "https" || policy.Host != host || policy.Port != 443 ||
		policy.ProxyMode != providerhttp.ProxyDisabled || policy.TLS.RootCAs != nil {
		return errors.New("typesafe rerank: egress authority must be exactly api.typesafe.ai:443")
	}
	for index := range policy.AllowedCIDRs {
		policy.AllowedCIDRs[index] = policy.AllowedCIDRs[index].Masked()
	}
	slices.SortFunc(policy.AllowedCIDRs, func(left, right netip.Prefix) int {
		return strings.Compare(left.String(), right.String())
	})
	for index := 1; index < len(policy.AllowedCIDRs); index++ {
		if policy.AllowedCIDRs[index] == policy.AllowedCIDRs[index-1] {
			return errors.New("typesafe rerank: duplicate egress CIDR")
		}
	}
	for index := range policy.TLS.SPKISHA256 {
		policy.TLS.SPKISHA256[index] = strings.ToLower(policy.TLS.SPKISHA256[index])
	}
	slices.Sort(policy.TLS.SPKISHA256)
	for index := 1; index < len(policy.TLS.SPKISHA256); index++ {
		if policy.TLS.SPKISHA256[index] == policy.TLS.SPKISHA256[index-1] {
			return errors.New("typesafe rerank: duplicate SPKI pin")
		}
	}
	if _, err := providerhttp.NewTransport(*policy, nil); err != nil {
		return errors.New("typesafe rerank: sealed egress policy is invalid")
	}
	return nil
}

func profileEgressIdentity(policy providerhttp.EgressPolicy) egressIdentity {
	cidrs := make([]string, len(policy.AllowedCIDRs))
	for index, prefix := range policy.AllowedCIDRs {
		cidrs[index] = prefix.String()
	}
	return egressIdentity{
		Scheme: policy.Scheme, Host: policy.Host, Port: policy.Port, AllowedCIDRs: cidrs,
		ProxyMode: string(policy.ProxyMode), ConnectTimeout: int64(policy.ConnectTimeout),
		KeepAlive: int64(policy.KeepAlive), TLSHandshakeTimeout: int64(policy.TLSHandshakeTimeout),
		SPKISHA256: slices.Clone(policy.TLS.SPKISHA256),
	}
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
