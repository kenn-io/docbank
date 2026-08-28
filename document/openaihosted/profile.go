// Package openaihosted implements the fixed hosted OpenAI embeddings contract.
package openaihosted

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
	// ProviderID is the fixed E1 adapter identity for hosted OpenAI embeddings.
	ProviderID = "openai.hosted.text-embedding-3-large-v1"
	// Model is the only currently documented hosted model alias admitted here.
	Model = "text-embedding-3-large"
	// DocumentFormatterV1 identifies the document rendering path.
	DocumentFormatterV1 = "openai-hosted/document/v1"
	// QueryFormatterV1 identifies the query rendering path.
	QueryFormatterV1 = "openai-hosted/query/v1"
	// ScalarEncodingFloat32 is the only response representation admitted here.
	ScalarEncodingFloat32 = "float32"

	host              = "api.openai.com"
	origin            = "https://api.openai.com"
	embeddingsPath    = "/v1/embeddings"
	adapterContract   = "docbank-openai-hosted-embeddings/v1"
	defaultTimeout    = 30 * time.Second
	defaultBatch      = 128
	defaultItemBytes  = int64(1 << 20)
	defaultInputBytes = int64(16 << 20)
	defaultRequest    = int64(32 << 20)
	defaultResponse   = int64(64 << 20)
	maximumTimeout    = 5 * time.Minute
	maximumBatch      = 2_048
	maximumBytes      = int64(1 << 30)
	maximumTokenBytes = 128
)

// SecretResolver resolves only the configured named OpenAI API-key binding.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, binding string) (string, error)
}

// Profile freezes the exact hosted contract and its execution bounds.
type Profile struct {
	Descriptor         document.EmbeddingDescriptor
	CompatibilityEpoch string
	SecretBinding      string
	RequestTimeout     time.Duration
	MaxBatchItems      int
	MaxInputItemBytes  int64
	MaxInputBytes      int64
	MaxRequestBytes    int64
	MaxResponseBytes   int64
	EgressPolicy       providerhttp.EgressPolicy
}

// Client calls only the fixed hosted OpenAI embeddings endpoint.
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

// PolicyFingerprint returns the canonical hosted profile identity.
func PolicyFingerprint(profile Profile) (string, error) {
	normalized, descriptorIdentity, err := normalizeProfile(profile)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(policyIdentity{
		AdapterContract: adapterContract, Origin: origin, Route: embeddingsPath,
		Descriptor: descriptorIdentity, CompatibilityEpoch: normalized.CompatibilityEpoch,
		SecretBinding: normalized.SecretBinding, RequestTimeout: int64(normalized.RequestTimeout),
		MaxBatchItems: normalized.MaxBatchItems, MaxInputItemBytes: normalized.MaxInputItemBytes,
		MaxInputBytes: normalized.MaxInputBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes, Egress: profileEgressIdentity(normalized.EgressPolicy),
	}, json.Deterministic(true))
	if err != nil {
		return "", errors.New("openaihosted: policy identity encoding failed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// New validates the fixed hosted profile and replaces every supplied transport
// authority with the sealed provider HTTP transport.
func New(profile Profile, secrets SecretResolver, resolver providerhttp.Resolver, supplied *http.Client) (*Client, error) {
	if supplied == nil {
		return nil, errors.New("openaihosted: HTTP client settings source is required")
	}
	normalized, _, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		return nil, errors.New("openaihosted: descriptor is not canonical")
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("openaihosted: descriptor policy fingerprint does not match profile")
	}
	if nilInterface(secrets) {
		return nil, errors.New("openaihosted: named API-key resolver is required")
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, errors.New("openaihosted: sealed egress policy is invalid")
	}
	isolated := *supplied
	isolated.Transport = transport
	isolated.CheckRedirect = providerhttp.RefuseRedirects
	isolated.Jar = nil
	isolated.Timeout = 0
	normalized.Descriptor = cloneDescriptor(descriptor)
	return &Client{profile: normalized, descriptor: cloneDescriptor(descriptor), secrets: secrets, http: &isolated}, nil
}

// Descriptor returns an immutable copy of the exact vector-space contract.
func (client *Client) Descriptor() document.EmbeddingDescriptor {
	if client == nil {
		return document.EmbeddingDescriptor{}
	}
	return cloneDescriptor(client.descriptor)
}

func normalizeProfile(profile Profile) (Profile, document.EmbeddingDescriptor, error) {
	profile.EgressPolicy.AllowedCIDRs = slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	profile.EgressPolicy.TLS.SPKISHA256 = slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
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
		profile.MaxRequestBytes = defaultRequest
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = defaultResponse
	}
	if profile.RequestTimeout <= 0 || profile.RequestTimeout > maximumTimeout ||
		profile.MaxBatchItems < 1 || profile.MaxBatchItems > maximumBatch ||
		profile.MaxInputItemBytes < 1 || profile.MaxInputItemBytes > maximumBytes ||
		profile.MaxInputBytes < profile.MaxInputItemBytes || profile.MaxInputBytes > maximumBytes ||
		profile.MaxRequestBytes < 1 || profile.MaxRequestBytes > maximumBytes ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maximumBytes {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("openaihosted: execution bounds are invalid")
	}
	if !validToken(profile.CompatibilityEpoch) || profile.Descriptor.ModelRevision != profile.CompatibilityEpoch {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("openaihosted: compatibility epoch must exactly match descriptor model revision")
	}
	if !validToken(profile.SecretBinding) {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("openaihosted: named API-key binding is required")
	}
	if err := normalizeEgress(&profile.EgressPolicy); err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	descriptorIdentity := cloneDescriptor(profile.Descriptor)
	descriptorIdentity.PolicyFingerprint = strings.Repeat("0", sha256.Size*2)
	descriptorIdentity.Fingerprint = ""
	var err error
	descriptorIdentity, err = document.NewEmbeddingDescriptor(descriptorIdentity)
	if err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("openaihosted: descriptor identity is invalid")
	}
	descriptorIdentity.PolicyFingerprint = ""
	descriptorIdentity.Fingerprint = ""
	if err := validateDescriptorContract(descriptorIdentity, profile.CompatibilityEpoch); err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	return profile, descriptorIdentity, nil
}

func validateDescriptorContract(descriptor document.EmbeddingDescriptor, epoch string) error {
	if descriptor.ID != ProviderID || descriptor.ContractVersion != document.EmbeddingProviderContractVersion ||
		descriptor.TrustBoundary != document.EmbeddingTrustHostedProvider || descriptor.Model != Model ||
		descriptor.ModelRevision != epoch || descriptor.Dimension < 1 || descriptor.Metric != document.VectorMetricCosine ||
		descriptor.Normalization != document.VectorNormalizationUnitLength || descriptor.ScalarEncoding != ScalarEncodingFloat32 ||
		descriptor.DocumentFormatter != DocumentFormatterV1 || descriptor.QueryFormatter != QueryFormatterV1 ||
		!descriptor.SupportsTextQuery || !slices.Equal(descriptor.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}) ||
		!slices.Equal(descriptor.SupportedRequestModes, []document.ModelInputMode{document.ModelInputModeText}) ||
		descriptor.ModelInput.Document.Mode != document.ModelInputModeText || descriptor.ModelInput.Query.Mode != document.ModelInputModeText ||
		descriptor.CompatibilityID != descriptor.ModelInput.CompatibilityID {
		return errors.New("openaihosted: descriptor does not match the fixed hosted text contract")
	}
	return nil
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
		return errors.New("openaihosted: egress authority must be exactly api.openai.com:443 with system roots and no proxy")
	}
	for index := range policy.AllowedCIDRs {
		policy.AllowedCIDRs[index] = policy.AllowedCIDRs[index].Masked()
	}
	slices.SortFunc(policy.AllowedCIDRs, func(left, right netip.Prefix) int {
		return strings.Compare(left.String(), right.String())
	})
	for index := 1; index < len(policy.AllowedCIDRs); index++ {
		if policy.AllowedCIDRs[index] == policy.AllowedCIDRs[index-1] {
			return errors.New("openaihosted: egress policy has a duplicate CIDR")
		}
	}
	for index := range policy.TLS.SPKISHA256 {
		policy.TLS.SPKISHA256[index] = strings.ToLower(policy.TLS.SPKISHA256[index])
	}
	slices.Sort(policy.TLS.SPKISHA256)
	for index := 1; index < len(policy.TLS.SPKISHA256); index++ {
		if policy.TLS.SPKISHA256[index] == policy.TLS.SPKISHA256[index-1] {
			return errors.New("openaihosted: egress policy has a duplicate SPKI pin")
		}
	}
	if _, err := providerhttp.NewTransport(*policy, nil); err != nil {
		return errors.New("openaihosted: sealed egress policy is invalid")
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
	return value != "" && len(value) <= maximumTokenBytes && utf8.ValidString(value) && value == strings.TrimSpace(value) &&
		strings.IndexFunc(value, func(character rune) bool { return unicode.IsControl(character) || unicode.IsSpace(character) }) < 0
}

func cloneDescriptor(value document.EmbeddingDescriptor) document.EmbeddingDescriptor {
	value.InputKinds = slices.Clone(value.InputKinds)
	value.SupportedRequestModes = slices.Clone(value.SupportedRequestModes)
	return value
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
