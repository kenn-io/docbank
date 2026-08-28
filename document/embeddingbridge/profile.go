package embeddingbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	defaultRequestTimeout   = 30 * time.Second
	defaultMaxBatchItems    = 128
	defaultMaxInputBytes    = int64(16 << 20)
	defaultMaxRequestBytes  = int64(32 << 20)
	defaultMaxResponseBytes = int64(64 << 20)
	maxRequestTimeout       = 10 * time.Minute
	maxBatchItems           = 10_000
	maxInputBytes           = int64(1 << 30)
	maxRequestBytes         = int64(2 << 30)
	maxResponseBytes        = int64(1 << 30)
	maxSecretBytes          = 64 << 10
)

type policyIdentity struct {
	AdapterContract   string                       `json:"adapter_contract"`
	Origin            string                       `json:"origin"`
	Route             string                       `json:"route"`
	Descriptor        document.EmbeddingDescriptor `json:"descriptor"`
	ModelInput        document.ModelInputContract  `json:"model_input"`
	CredentialBinding string                       `json:"credential_binding"`
	Egress            egressIdentity               `json:"egress"`
	RequestTimeout    int64                        `json:"request_timeout_nanos"`
	MaxBatchItems     int                          `json:"max_batch_items"`
	MaxInputBytes     int64                        `json:"max_input_bytes"`
	MaxRequestBytes   int64                        `json:"max_request_bytes"`
	MaxResponseBytes  int64                        `json:"max_response_bytes"`
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

// PolicyFingerprint returns the canonical profile fingerprint. Credential
// values and supplied HTTP client state are deliberately excluded.
func PolicyFingerprint(profile Profile) (string, error) {
	normalized, descriptorIdentity, err := normalizeProfile(profile)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(policyIdentity{
		AdapterContract: adapterContract, Origin: normalized.Origin, Route: embeddingsPath,
		Descriptor: descriptorIdentity, ModelInput: descriptorIdentity.ModelInput,
		CredentialBinding: normalized.SecretBinding, Egress: profileEgressIdentity(normalized.EgressPolicy),
		RequestTimeout: int64(normalized.RequestTimeout), MaxBatchItems: normalized.MaxBatchItems,
		MaxInputBytes: normalized.MaxInputBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes,
	}, json.Deterministic(true))
	if err != nil {
		return "", errors.New("embedding bridge: profile identity encoding failed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// New validates a canonical profile and returns an isolated client using the
// repository egress transport. All mutable supplied client authority is removed.
func New(profile Profile, secrets SecretResolver, resolver providerhttp.Resolver, supplied *http.Client) (*Client, error) {
	if supplied == nil {
		return nil, errors.New("embedding bridge: HTTP client is required")
	}
	normalized, _, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		return nil, errors.New("embedding bridge: descriptor is not canonical")
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("embedding bridge: descriptor policy fingerprint does not match profile")
	}
	if normalized.SecretBinding == "" {
		if !nilInterface(secrets) {
			return nil, errors.New("embedding bridge: resolver is not allowed without a named secret binding")
		}
	} else if nilInterface(secrets) {
		return nil, errors.New("embedding bridge: named secret resolver is required")
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, errors.New("embedding bridge: sealed egress policy is invalid")
	}
	isolated := *supplied
	isolated.Transport = transport
	isolated.CheckRedirect = providerhttp.RefuseRedirects
	isolated.Jar = nil
	isolated.Timeout = 0
	return &Client{
		origin: normalized.Origin, descriptor: cloneDescriptor(descriptor),
		secretBinding: normalized.SecretBinding, secrets: secrets, http: &isolated,
		requestTimeout: normalized.RequestTimeout, maxBatchItems: normalized.MaxBatchItems,
		maxInputBytes: normalized.MaxInputBytes, maxRequestBytes: normalized.MaxRequestBytes,
		maxResponseBytes: normalized.MaxResponseBytes,
	}, nil
}

func normalizeProfile(profile Profile) (Profile, document.EmbeddingDescriptor, error) {
	profile.EgressPolicy.AllowedCIDRs = slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	profile.EgressPolicy.TLS.SPKISHA256 = slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = defaultRequestTimeout
	}
	if profile.MaxBatchItems == 0 {
		profile.MaxBatchItems = defaultMaxBatchItems
	}
	if profile.MaxInputBytes == 0 {
		profile.MaxInputBytes = defaultMaxInputBytes
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = defaultMaxRequestBytes
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = defaultMaxResponseBytes
	}
	if profile.RequestTimeout <= 0 || profile.RequestTimeout > maxRequestTimeout ||
		profile.MaxBatchItems < 1 || profile.MaxBatchItems > maxBatchItems ||
		profile.MaxInputBytes < 1 || profile.MaxInputBytes > maxInputBytes ||
		profile.MaxRequestBytes < 1 || profile.MaxRequestBytes > maxRequestBytes ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maxResponseBytes {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("embedding bridge: execution bounds are invalid")
	}
	if profile.SecretBinding != "" && !validBinding(profile.SecretBinding) {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("embedding bridge: secret binding is invalid")
	}
	if profile.SecretBinding == "" && profile.Descriptor.TrustBoundary != document.EmbeddingTrustOperatorNetwork {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("embedding bridge: anonymous access is operator-network only")
	}
	origin, err := normalizeOrigin(profile.Origin, profile.Descriptor.TrustBoundary)
	if err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	profile.Origin = origin
	if err := normalizeEgress(&profile); err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	descriptorIdentity := cloneDescriptor(profile.Descriptor)
	descriptorIdentity.PolicyFingerprint = strings.Repeat("0", sha256.Size*2)
	descriptorIdentity.Fingerprint = ""
	descriptorIdentity, err = document.NewEmbeddingDescriptor(descriptorIdentity)
	if err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("embedding bridge: descriptor identity is invalid")
	}
	descriptorIdentity.PolicyFingerprint = ""
	descriptorIdentity.Fingerprint = ""
	return profile, descriptorIdentity, nil
}

func normalizeOrigin(raw string, trust document.EmbeddingTrustBoundary) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Opaque != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("embedding bridge: origin must be one absolute origin without path, credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || trust != document.EmbeddingTrustOperatorNetwork) {
		return "", errors.New("embedding bridge: hosted origins require HTTPS; HTTP is operator-network only")
	}
	if trust != document.EmbeddingTrustOperatorNetwork && trust != document.EmbeddingTrustHostedProvider {
		return "", errors.New("embedding bridge: network origin requires an operator-network or hosted trust boundary")
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", errors.New("embedding bridge: origin port is invalid")
	}
	host := strings.ToLower(parsed.Hostname())
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if (parsed.Scheme != "https" || port != "443") && (parsed.Scheme != "http" || port != "80") {
		authority = net.JoinHostPort(host, port)
	}
	return parsed.Scheme + "://" + authority, nil
}

func normalizeEgress(profile *Profile) error {
	policy := &profile.EgressPolicy
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
	if policy.TLS.RootCAs != nil {
		return errors.New("embedding bridge: custom egress roots cannot enter canonical identity")
	}
	parsed, err := url.Parse(profile.Origin)
	if err != nil {
		return errors.New("embedding bridge: origin is invalid")
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if parsed.Scheme != policy.Scheme || !strings.EqualFold(parsed.Hostname(), policy.Host) ||
		port != strconv.FormatUint(uint64(policy.Port), 10) {
		return errors.New("embedding bridge: origin and egress authority differ")
	}
	policy.Host = strings.ToLower(policy.Host)
	for index := range policy.AllowedCIDRs {
		policy.AllowedCIDRs[index] = policy.AllowedCIDRs[index].Masked()
	}
	slices.SortFunc(policy.AllowedCIDRs, func(left, right netip.Prefix) int {
		return strings.Compare(left.String(), right.String())
	})
	for index := 1; index < len(policy.AllowedCIDRs); index++ {
		if policy.AllowedCIDRs[index] == policy.AllowedCIDRs[index-1] {
			return errors.New("embedding bridge: egress policy has a duplicate CIDR")
		}
	}
	for index := range policy.TLS.SPKISHA256 {
		policy.TLS.SPKISHA256[index] = strings.ToLower(policy.TLS.SPKISHA256[index])
	}
	slices.Sort(policy.TLS.SPKISHA256)
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

func validBinding(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && value == strings.TrimSpace(value) &&
		strings.IndexFunc(value, func(character rune) bool { return unicode.IsControl(character) || unicode.IsSpace(character) }) < 0
}

func validSecret(value string) bool {
	return value != "" && len(value) <= maxSecretBytes && strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character) || unicode.IsSpace(character)
	}) < 0
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

func cloneDescriptor(value document.EmbeddingDescriptor) document.EmbeddingDescriptor {
	value.InputKinds = slices.Clone(value.InputKinds)
	value.SupportedRequestModes = slices.Clone(value.SupportedRequestModes)
	return value
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
