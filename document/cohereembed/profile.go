// Package cohereembed implements the fixed hosted Cohere Embed v4 contract.
package cohereembed

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

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/cohereapi"
)

const (
	ProviderID            = "cohere.hosted.embed-v4-v1"
	Model                 = "embed-v4.0"
	DocumentFormatterV1   = "cohere-embed-v4/search-document/v1"
	QueryFormatterV1      = "cohere-embed-v4/search-query/v1"
	ScalarEncodingFloat32 = "float32"
	modelCompatibilityID  = "cohere/embed-v4/search/v1"
	host                  = "api.cohere.com"
	origin                = "https://api.cohere.com"
	embedPath             = "/v2/embed"
	adapterContract       = "docbank-cohere-embed-v4/v1"
	maximumBatch          = 96
	maximumImageBytes     = int64(20 << 20)
	maximumRequestBytes   = int64(64 << 20)
	maximumResponseBytes  = int64(64 << 20)
	maximumTimeout        = 5 * time.Minute
	maximumTokenBytes     = 128
	defaultTimeout        = 30 * time.Second
	defaultInputItemBytes = int64(1 << 20)
	defaultRequestBytes   = int64(32 << 20)
	defaultResponseBytes  = int64(32 << 20)
)

var supportedDimensions = []int{256, 512, 1024, 1536}

var acceptedImageFormats = []string{"image/gif", "image/jpeg", "image/png", "image/webp"}

type SecretResolver interface {
	ResolveSecret(ctx context.Context, binding string) (string, error)
}

type Profile struct {
	Descriptor         document.EmbeddingDescriptor
	CompatibilityEpoch string
	SecretBinding      string
	RequestTimeout     time.Duration
	MaxBatchItems      int
	MaxInputItemBytes  int64
	MaxInputBytes      int64
	MaxImageBytes      int64
	MaxRequestBytes    int64
	MaxResponseBytes   int64
	MediaPolicy        media.Policy
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
	RequestTimeout     int64                        `json:"request_timeout_nanos"`
	MaxBatchItems      int                          `json:"max_batch_items"`
	MaxInputItemBytes  int64                        `json:"max_input_item_bytes"`
	MaxInputBytes      int64                        `json:"max_input_bytes"`
	MaxImageBytes      int64                        `json:"max_image_bytes"`
	MaxRequestBytes    int64                        `json:"max_request_bytes"`
	MaxResponseBytes   int64                        `json:"max_response_bytes"`
	AcceptedImageTypes []string                     `json:"accepted_image_types"`
	MediaPolicy        media.Policy                 `json:"media_policy"`
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
		RequestTimeout: int64(normalized.RequestTimeout), MaxBatchItems: normalized.MaxBatchItems,
		MaxInputItemBytes: normalized.MaxInputItemBytes, MaxInputBytes: normalized.MaxInputBytes,
		MaxImageBytes: normalized.MaxImageBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes, AcceptedImageTypes: slices.Clone(acceptedImageFormats),
		MediaPolicy: normalized.MediaPolicy, Egress: egressPolicyIdentity(normalized.EgressPolicy),
	}, json.Deterministic(true))
	if err != nil {
		return "", errors.New("cohere embed: policy identity encoding failed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func New(profile Profile, secrets SecretResolver, resolver providerhttp.Resolver, supplied *http.Client) (*Client, error) {
	if supplied == nil {
		return nil, errors.New("cohere embed: HTTP client settings source is required")
	}
	normalized, _, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		return nil, errors.New("cohere embed: descriptor is not canonical")
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("cohere embed: descriptor policy fingerprint does not match profile")
	}
	if nilInterface(secrets) {
		return nil, errors.New("cohere embed: named API-key resolver is required")
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, errors.New("cohere embed: sealed egress policy is invalid")
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
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = defaultTimeout
	}
	if profile.MaxBatchItems == 0 {
		profile.MaxBatchItems = maximumBatch
	}
	if profile.MaxInputItemBytes == 0 {
		profile.MaxInputItemBytes = defaultInputItemBytes
	}
	if profile.MaxInputBytes == 0 {
		profile.MaxInputBytes = maximumImageBytes
	}
	if profile.MaxImageBytes == 0 {
		profile.MaxImageBytes = maximumImageBytes
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = defaultRequestBytes
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = defaultResponseBytes
	}
	if profile.RequestTimeout <= 0 || profile.RequestTimeout > maximumTimeout ||
		profile.MaxBatchItems < 1 || profile.MaxBatchItems > maximumBatch ||
		profile.MaxInputItemBytes < 1 || profile.MaxInputItemBytes > maximumImageBytes ||
		profile.MaxInputBytes < profile.MaxInputItemBytes || profile.MaxInputBytes > maximumImageBytes ||
		profile.MaxImageBytes < 1 || profile.MaxImageBytes > maximumImageBytes ||
		profile.MaxRequestBytes < 1 || profile.MaxRequestBytes > maximumRequestBytes ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maximumResponseBytes {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("cohere embed: execution bounds are invalid")
	}
	if !cohereapi.ValidToken(profile.CompatibilityEpoch, maximumTokenBytes) || profile.Descriptor.ModelRevision != profile.CompatibilityEpoch {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("cohere embed: compatibility epoch must match descriptor revision")
	}
	if !cohereapi.ValidToken(profile.SecretBinding, maximumTokenBytes) {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("cohere embed: named API-key binding is required")
	}
	profile.MediaPolicy = profile.MediaPolicy.Normalized()
	if err := profile.MediaPolicy.Validate(); err != nil || profile.MediaPolicy.MaxBytes > profile.MaxImageBytes ||
		profile.MediaPolicy.AllowVideo || !profile.MediaPolicy.AllowStill || !profile.MediaPolicy.AllowAnimated {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("cohere embed: media policy must admit bounded still and animated images only")
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
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("cohere embed: descriptor identity is invalid")
	}
	descriptor.PolicyFingerprint, descriptor.Fingerprint = "", ""
	if err := validateDescriptor(descriptor, profile.CompatibilityEpoch); err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	return profile, descriptor, nil
}

func validateDescriptor(descriptor document.EmbeddingDescriptor, epoch string) error {
	expectedInput, err := modelInputContract()
	if err != nil {
		return errors.New("cohere embed: fixed model-input contract is invalid")
	}
	if descriptor.ID != ProviderID || descriptor.ContractVersion != document.EmbeddingProviderContractVersion ||
		descriptor.TrustBoundary != document.EmbeddingTrustHostedProvider || descriptor.Model != Model ||
		descriptor.ModelRevision != epoch || !slices.Contains(supportedDimensions, descriptor.Dimension) ||
		descriptor.Metric != document.VectorMetricCosine || descriptor.Normalization != document.VectorNormalizationNone ||
		descriptor.ScalarEncoding != ScalarEncodingFloat32 || descriptor.DocumentFormatter != DocumentFormatterV1 ||
		descriptor.QueryFormatter != QueryFormatterV1 || !descriptor.SupportsTextQuery ||
		!reflect.DeepEqual(descriptor.ModelInput, expectedInput) || descriptor.CompatibilityID != modelCompatibilityID ||
		!slices.Equal(descriptor.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk}) ||
		!slices.Equal(descriptor.SupportedRequestModes, []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery}) {
		return errors.New("cohere embed: descriptor does not match the fixed hosted multimodal contract")
	}
	return nil
}

func modelInputContract() (document.ModelInputContract, error) {
	return document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: modelCompatibilityID,
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
		return errors.New("cohere embed: egress authority must be exactly api.cohere.com:443")
	}
	for index := range policy.AllowedCIDRs {
		policy.AllowedCIDRs[index] = policy.AllowedCIDRs[index].Masked()
	}
	slices.SortFunc(policy.AllowedCIDRs, func(left, right netip.Prefix) int { return strings.Compare(left.String(), right.String()) })
	for index := 1; index < len(policy.AllowedCIDRs); index++ {
		if policy.AllowedCIDRs[index] == policy.AllowedCIDRs[index-1] {
			return errors.New("cohere embed: egress policy has a duplicate CIDR")
		}
	}
	for index := range policy.TLS.SPKISHA256 {
		policy.TLS.SPKISHA256[index] = strings.ToLower(policy.TLS.SPKISHA256[index])
	}
	slices.Sort(policy.TLS.SPKISHA256)
	for index := 1; index < len(policy.TLS.SPKISHA256); index++ {
		if policy.TLS.SPKISHA256[index] == policy.TLS.SPKISHA256[index-1] {
			return errors.New("cohere embed: egress policy has a duplicate SPKI pin")
		}
	}
	if _, err := providerhttp.NewTransport(*policy, nil); err != nil {
		return errors.New("cohere embed: sealed egress policy is invalid")
	}
	return nil
}

func egressPolicyIdentity(policy providerhttp.EgressPolicy) egressIdentity {
	cidrs := make([]string, len(policy.AllowedCIDRs))
	for index, prefix := range policy.AllowedCIDRs {
		cidrs[index] = prefix.String()
	}
	return egressIdentity{Scheme: policy.Scheme, Host: policy.Host, Port: policy.Port,
		AllowedCIDRs: cidrs, ProxyMode: string(policy.ProxyMode), ConnectTimeout: int64(policy.ConnectTimeout),
		KeepAlive: int64(policy.KeepAlive), TLSHandshakeTimeout: int64(policy.TLSHandshakeTimeout),
		SPKISHA256: slices.Clone(policy.TLS.SPKISHA256)}
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
