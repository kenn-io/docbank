// Package zeroentropyembed implements the fixed hosted ZeroEntropy zembed-1 contract.
package zeroentropyembed

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

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	ProviderID            = "zeroentropy.hosted.zembed-1-v1"
	Model                 = "zembed-1"
	DocumentFormatterV1   = "zeroentropy-zembed-1/document/v1"
	QueryFormatterV1      = "zeroentropy-zembed-1/query/v1"
	ScalarEncodingFloat32 = "float32"
	TransformNone         = "none"

	host                 = "api.zeroentropy.dev"
	origin               = "https://api.zeroentropy.dev"
	embedPath            = "/v1/models/embed"
	adapterContract      = "docbank-zeroentropy-zembed-1/v1"
	compatibilityID      = "zeroentropy/zembed-1/retrieval/v1"
	defaultTimeout       = 30 * time.Second
	maximumTimeout       = 5 * time.Minute
	defaultBatch         = 128
	maximumBatch         = 2048
	defaultItemBytes     = int64(1 << 20)
	defaultInputBytes    = int64(4 << 20)
	maximumInputBytes    = int64(5_000_000)
	defaultRequestBytes  = int64(8 << 20)
	maximumRequestBytes  = int64(16 << 20)
	defaultResponseBytes = int64(32 << 20)
	maximumResponseBytes = int64(128 << 20)
	maximumTokenBytes    = 128
)

var supportedDimensions = []int{40, 80, 160, 320, 640, 1280, 2560}

type EncodingFormat string

const (
	EncodingFloat  EncodingFormat = "float"
	EncodingBase64 EncodingFormat = "base64"
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
	Descriptor         document.EmbeddingDescriptor
	CompatibilityEpoch string
	SecretBinding      string
	EncodingFormat     EncodingFormat
	Latency            Latency
	ClientTransform    string
	RequestTimeout     time.Duration
	MaxBatchItems      int
	MaxInputItemBytes  int64
	MaxInputBytes      int64
	MaxRequestBytes    int64
	MaxResponseBytes   int64
	EgressPolicy       providerhttp.EgressPolicy
}

type Client struct {
	profile    Profile
	descriptor document.EmbeddingDescriptor
	secrets    SecretResolver
	http       *http.Client
}

type policyIdentity struct {
	AdapterContract    string                       `json:"adapter_contract"`
	Origin             string                       `json:"origin"`
	Route              string                       `json:"route"`
	Descriptor         document.EmbeddingDescriptor `json:"descriptor"`
	CompatibilityEpoch string                       `json:"compatibility_epoch"`
	SecretBinding      string                       `json:"secret_binding"`
	EncodingFormat     EncodingFormat               `json:"encoding_format"`
	Latency            Latency                      `json:"latency"`
	ClientTransform    string                       `json:"client_transform"`
	RequestTimeout     int64                        `json:"request_timeout_nanos"`
	MaxBatchItems      int                          `json:"max_batch_items"`
	MaxInputItemBytes  int64                        `json:"max_input_item_bytes"`
	MaxInputBytes      int64                        `json:"max_input_bytes"`
	MaxRequestBytes    int64                        `json:"max_request_bytes"`
	MaxResponseBytes   int64                        `json:"max_response_bytes"`
	Egress             egressIdentity               `json:"egress"`
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
	normalized, descriptor, err := normalizeProfile(profile)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(policyIdentity{
		AdapterContract: adapterContract, Origin: origin, Route: embedPath, Descriptor: descriptor,
		CompatibilityEpoch: normalized.CompatibilityEpoch, SecretBinding: normalized.SecretBinding,
		EncodingFormat: normalized.EncodingFormat, Latency: normalized.Latency,
		ClientTransform: normalized.ClientTransform, RequestTimeout: int64(normalized.RequestTimeout),
		MaxBatchItems: normalized.MaxBatchItems, MaxInputItemBytes: normalized.MaxInputItemBytes,
		MaxInputBytes: normalized.MaxInputBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes, Egress: profileEgressIdentity(normalized.EgressPolicy),
	}, json.Deterministic(true))
	if err != nil {
		return "", errors.New("zeroentropy embed: policy identity encoding failed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func New(profile Profile, secrets SecretResolver, resolver providerhttp.Resolver, supplied *http.Client) (*Client, error) {
	if supplied == nil {
		return nil, errors.New("zeroentropy embed: HTTP client settings source is required")
	}
	normalized, _, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		return nil, errors.New("zeroentropy embed: descriptor is not canonical")
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("zeroentropy embed: descriptor policy fingerprint does not match profile")
	}
	if nilInterface(secrets) {
		return nil, errors.New("zeroentropy embed: named API-key resolver is required")
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, errors.New("zeroentropy embed: sealed egress policy is invalid")
	}
	isolated := *supplied
	isolated.Transport = transport
	isolated.CheckRedirect = providerhttp.RefuseRedirects
	isolated.Jar = nil
	isolated.Timeout = 0
	normalized.Descriptor = cloneDescriptor(descriptor)
	return &Client{profile: normalized, descriptor: cloneDescriptor(descriptor), secrets: secrets, http: &isolated}, nil
}

func (client *Client) Descriptor() document.EmbeddingDescriptor {
	if client == nil {
		return document.EmbeddingDescriptor{}
	}
	return cloneDescriptor(client.descriptor)
}

func normalizeProfile(profile Profile) (Profile, document.EmbeddingDescriptor, error) {
	profile.EgressPolicy.AllowedCIDRs = slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	profile.EgressPolicy.TLS.SPKISHA256 = slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
	if profile.EncodingFormat == "" {
		profile.EncodingFormat = EncodingFloat
	}
	if profile.Latency == "" {
		profile.Latency = LatencyAuto
	}
	if profile.ClientTransform == "" {
		profile.ClientTransform = TransformNone
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = defaultTimeout
	}
	if profile.MaxBatchItems == 0 {
		profile.MaxBatchItems = defaultBatch
	}
	if profile.MaxInputItemBytes == 0 {
		profile.MaxInputItemBytes = defaultItemBytes
	}
	if profile.MaxInputBytes == 0 {
		profile.MaxInputBytes = defaultInputBytes
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = defaultRequestBytes
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = defaultResponseBytes
	}
	if !slices.Contains(supportedDimensions, profile.Descriptor.Dimension) ||
		(profile.EncodingFormat != EncodingFloat && profile.EncodingFormat != EncodingBase64) ||
		(profile.Latency != LatencyAuto && profile.Latency != LatencyFast && profile.Latency != LatencySlow) ||
		profile.ClientTransform != TransformNone || profile.RequestTimeout <= 0 || profile.RequestTimeout > maximumTimeout ||
		profile.MaxBatchItems < 1 || profile.MaxBatchItems > maximumBatch ||
		profile.MaxInputItemBytes < 1 || profile.MaxInputItemBytes > maximumInputBytes ||
		profile.MaxInputBytes < profile.MaxInputItemBytes || profile.MaxInputBytes > maximumInputBytes ||
		profile.MaxRequestBytes < 1 || profile.MaxRequestBytes > maximumRequestBytes ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maximumResponseBytes {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("zeroentropy embed: profile bounds or execution policy are invalid")
	}
	if !validToken(profile.CompatibilityEpoch) || profile.Descriptor.ModelRevision != profile.CompatibilityEpoch {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("zeroentropy embed: compatibility epoch must match descriptor revision")
	}
	if !validToken(profile.SecretBinding) {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("zeroentropy embed: named API-key binding is required")
	}
	if err := normalizeEgress(&profile.EgressPolicy); err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	descriptor := cloneDescriptor(profile.Descriptor)
	descriptor.PolicyFingerprint = strings.Repeat("0", sha256.Size*2)
	descriptor.Fingerprint = ""
	var err error
	descriptor, err = document.NewEmbeddingDescriptor(descriptor)
	if err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("zeroentropy embed: descriptor identity is invalid")
	}
	descriptor.PolicyFingerprint, descriptor.Fingerprint = "", ""
	if err := validateDescriptor(descriptor, profile.CompatibilityEpoch); err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	return profile, descriptor, nil
}

func validateDescriptor(descriptor document.EmbeddingDescriptor, epoch string) error {
	expected, err := modelInputContract()
	if err != nil {
		return errors.New("zeroentropy embed: fixed model-input contract is invalid")
	}
	if descriptor.ID != ProviderID || descriptor.ContractVersion != document.EmbeddingProviderContractVersion ||
		descriptor.TrustBoundary != document.EmbeddingTrustHostedProvider || descriptor.Model != Model ||
		descriptor.ModelRevision != epoch || !slices.Contains(supportedDimensions, descriptor.Dimension) ||
		descriptor.Metric != document.VectorMetricCosine || descriptor.Normalization != document.VectorNormalizationNone ||
		descriptor.ScalarEncoding != ScalarEncodingFloat32 || descriptor.DocumentFormatter != DocumentFormatterV1 ||
		descriptor.QueryFormatter != QueryFormatterV1 || !descriptor.SupportsTextQuery ||
		!reflect.DeepEqual(descriptor.ModelInput, expected) || descriptor.CompatibilityID != compatibilityID ||
		!slices.Equal(descriptor.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}) ||
		!slices.Equal(descriptor.SupportedRequestModes, []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery}) {
		return errors.New("zeroentropy embed: descriptor does not match the fixed hosted zembed-1 contract")
	}
	return nil
}

func modelInputContract() (document.ModelInputContract, error) {
	return document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: compatibilityID,
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeDocument, Template: "{{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeQuery, Template: "{{content}}"},
	})
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
		return errors.New("zeroentropy embed: egress authority must be exactly api.zeroentropy.dev:443")
	}
	for index := range policy.AllowedCIDRs {
		policy.AllowedCIDRs[index] = policy.AllowedCIDRs[index].Masked()
	}
	slices.SortFunc(policy.AllowedCIDRs, func(left, right netip.Prefix) int { return strings.Compare(left.String(), right.String()) })
	for index := 1; index < len(policy.AllowedCIDRs); index++ {
		if policy.AllowedCIDRs[index] == policy.AllowedCIDRs[index-1] {
			return errors.New("zeroentropy embed: duplicate egress CIDR")
		}
	}
	for index := range policy.TLS.SPKISHA256 {
		policy.TLS.SPKISHA256[index] = strings.ToLower(policy.TLS.SPKISHA256[index])
	}
	slices.Sort(policy.TLS.SPKISHA256)
	for index := 1; index < len(policy.TLS.SPKISHA256); index++ {
		if policy.TLS.SPKISHA256[index] == policy.TLS.SPKISHA256[index-1] {
			return errors.New("zeroentropy embed: duplicate SPKI pin")
		}
	}
	if _, err := providerhttp.NewTransport(*policy, nil); err != nil {
		return errors.New("zeroentropy embed: sealed egress policy is invalid")
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

func cloneDescriptor(descriptor document.EmbeddingDescriptor) document.EmbeddingDescriptor {
	descriptor.InputKinds = slices.Clone(descriptor.InputKinds)
	descriptor.SupportedRequestModes = slices.Clone(descriptor.SupportedRequestModes)
	return descriptor
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
