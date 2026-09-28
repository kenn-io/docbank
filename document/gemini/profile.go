// Package gemini implements the fixed hosted Gemini Embedding 2 contract.
package gemini

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	ProviderID            = "gemini.hosted.embedding-2-v1"
	Model                 = "gemini-embedding-2"
	DocumentFormatterV1   = "gemini-embedding-2/search-document/v1"
	QueryFormatterV1      = "gemini-embedding-2/search-query/v1"
	ScalarEncodingFloat32 = "float32"

	host                = "generativelanguage.googleapis.com"
	origin              = "https://generativelanguage.googleapis.com"
	embedPath           = "/v1beta/models/gemini-embedding-2:embedContent"
	filesUploadPath     = "/upload/v1beta/files"
	filesPathPrefix     = "/v1beta/files/"
	adapterContract     = "docbank-gemini-embedding-2/v1"
	compatibilityID     = "gemini-embedding-2/search/v1"
	defaultTimeout      = 30 * time.Second
	defaultPoll         = 250 * time.Millisecond
	defaultPollAttempts = 120
	defaultCleanup      = 5 * time.Second
	retentionCeiling    = 48 * time.Hour
	defaultInputBytes   = int64(1 << 20)
	defaultRequest      = int64(2 << 20)
	defaultResponse     = int64(32 << 20)
	maximumTimeout      = 5 * time.Minute
	minimumPoll         = 10 * time.Millisecond
	maximumPoll         = 30 * time.Second
	maximumPollAttempts = 10_000
	maximumCleanup      = 30 * time.Second
	maximumInputBytes   = int64(100 << 20)
	maximumRequest      = int64(100 << 20)
	maximumResponse     = int64(100 << 20)
	maximumTokenBytes   = 128
)

type Transport string

const (
	TransportInline   Transport = "inline"
	TransportFilesAPI Transport = "files-api"
)

type SecretResolver interface {
	ResolveSecret(ctx context.Context, binding string) (string, error)
}

type Profile struct {
	Descriptor                   document.EmbeddingDescriptor
	CompatibilityEpoch           string
	SecretBinding                string
	Transport                    Transport
	CapabilityProfileFingerprint string
	DisclosureFingerprint        string
	RequestTimeout               time.Duration
	PollInterval                 time.Duration
	MaxPollAttempts              int
	CleanupTimeout               time.Duration
	MaxInputBytes                int64
	MaxRequestBytes              int64
	MaxResponseBytes             int64
	EgressPolicy                 providerhttp.EgressPolicy
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
	Transport          Transport                    `json:"transport"`
	CapabilityProfile  string                       `json:"capability_profile_fingerprint"`
	DisclosurePolicy   string                       `json:"disclosure_fingerprint"`
	RetentionCeiling   int64                        `json:"provider_retention_ceiling_nanos"`
	RequestTimeout     int64                        `json:"request_timeout_nanos"`
	PollInterval       int64                        `json:"poll_interval_nanos"`
	MaxPollAttempts    int                          `json:"max_poll_attempts"`
	CleanupTimeout     int64                        `json:"cleanup_timeout_nanos"`
	MaxInputBytes      int64                        `json:"max_input_bytes"`
	MaxRequestBytes    int64                        `json:"max_request_bytes"`
	MaxResponseBytes   int64                        `json:"max_response_bytes"`
	Egress             providerhttp.EgressIdentity  `json:"egress"`
}

func PolicyFingerprint(profile Profile) (string, error) {
	normalized, descriptor, err := normalizeProfile(profile)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(policyIdentity{
		AdapterContract: adapterContract, Origin: origin, Route: embedPath, Descriptor: descriptor,
		CompatibilityEpoch: normalized.CompatibilityEpoch, SecretBinding: normalized.SecretBinding,
		Transport: normalized.Transport, CapabilityProfile: normalized.CapabilityProfileFingerprint,
		DisclosurePolicy: normalized.DisclosureFingerprint, RetentionCeiling: int64(profileRetention(normalized.Transport)),
		RequestTimeout: int64(normalized.RequestTimeout), PollInterval: int64(normalized.PollInterval),
		MaxPollAttempts: normalized.MaxPollAttempts,
		CleanupTimeout:  int64(normalized.CleanupTimeout),
		MaxInputBytes:   normalized.MaxInputBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes, Egress: providerhttp.IdentifyEgress(normalized.EgressPolicy),
	}, json.Deterministic(true))
	if err != nil {
		return "", errors.New("gemini embed: policy identity encoding failed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func New(profile Profile, secrets SecretResolver, resolver providerhttp.Resolver, supplied *http.Client) (*Client, error) {
	if supplied == nil {
		return nil, errors.New("gemini embed: HTTP client settings source is required")
	}
	normalized, _, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		return nil, errors.New("gemini embed: descriptor is not canonical")
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("gemini embed: descriptor policy fingerprint does not match profile")
	}
	if nilInterface(secrets) {
		return nil, errors.New("gemini embed: named API-key resolver is required")
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, errors.New("gemini embed: sealed egress policy is invalid")
	}
	isolate := *supplied
	isolate.Transport = transport
	isolate.CheckRedirect = providerhttp.RefuseRedirects
	isolate.Jar = nil
	isolate.Timeout = 0
	normalized.Descriptor = cloneDescriptor(descriptor)
	return &Client{profile: normalized, descriptor: cloneDescriptor(descriptor), secrets: secrets, http: &isolate}, nil
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
	if profile.PollInterval == 0 {
		profile.PollInterval = defaultPoll
	}
	if profile.MaxPollAttempts == 0 {
		profile.MaxPollAttempts = defaultPollAttempts
	}
	if profile.CleanupTimeout == 0 {
		profile.CleanupTimeout = defaultCleanup
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
		profile.PollInterval < minimumPoll || profile.PollInterval > maximumPoll ||
		profile.MaxPollAttempts < 1 || profile.MaxPollAttempts > maximumPollAttempts ||
		profile.CleanupTimeout <= 0 || profile.CleanupTimeout > maximumCleanup ||
		profile.MaxInputBytes < 1 || profile.MaxInputBytes > maximumInputBytes ||
		profile.MaxRequestBytes < 1 || profile.MaxRequestBytes > maximumRequest ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maximumResponse {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("gemini embed: execution bounds are invalid")
	}
	if profile.Transport != TransportInline && profile.Transport != TransportFilesAPI {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("gemini embed: transport is invalid")
	}
	if profile.CompatibilityEpoch != Model || profile.Descriptor.ModelRevision != Model {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("gemini embed: stable model revision must be gemini-embedding-2")
	}
	if !validToken(profile.SecretBinding) {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("gemini embed: named API-key binding is required")
	}
	if !validFingerprint(profile.CapabilityProfileFingerprint) || !validFingerprint(profile.DisclosureFingerprint) {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("gemini embed: capability profile and disclosure identities are required")
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
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("gemini embed: descriptor identity is invalid")
	}
	descriptor.PolicyFingerprint, descriptor.Fingerprint = "", ""
	if err := validateDescriptor(descriptor); err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	if profile.Transport == TransportFilesAPI {
		minimum, capacityErr := minimumFilesAPIRequestCapacity(descriptor.Dimension)
		if capacityErr != nil || profile.MaxRequestBytes < minimum {
			return Profile{}, document.EmbeddingDescriptor{}, errors.New("gemini embed: Files API request byte capacity is too small")
		}
	}
	return profile, descriptor, nil
}

func minimumFilesAPIRequestCapacity(dimension int) (int64, error) {
	start, err := json.Marshal(wireStartUpload{})
	if err != nil {
		return 0, err
	}
	part := wirePart{FileData: &wireFileData{
		MIMEType: "video/quicktime", FileURI: maximumProviderFileURI(),
	}}
	fileData, err := json.Marshal(wireRequest{Model: "models/" + Model,
		Content: wireContent{Parts: []wirePart{part}}, OutputDimensionality: dimension})
	if err != nil {
		return 0, err
	}
	return int64(max(len(start), len(fileData))), nil
}

func maximumProviderFileURI() string {
	return origin + "/v1beta/files/" + strings.Repeat("a", 40)
}

func validateDescriptor(descriptor document.EmbeddingDescriptor) error {
	contract, err := modelInputContract()
	if err != nil {
		return errors.New("gemini embed: fixed model-input contract is invalid")
	}
	if descriptor.ID != ProviderID || descriptor.ContractVersion != document.EmbeddingProviderContractVersion ||
		descriptor.TrustBoundary != document.EmbeddingTrustHostedProvider || descriptor.Model != Model ||
		descriptor.ModelRevision != Model || descriptor.Dimension < 128 || descriptor.Dimension > 3072 ||
		descriptor.Metric != document.VectorMetricCosine || descriptor.Normalization != document.VectorNormalizationUnitLength ||
		descriptor.ScalarEncoding != ScalarEncodingFloat32 || descriptor.DocumentFormatter != DocumentFormatterV1 ||
		descriptor.QueryFormatter != QueryFormatterV1 || !descriptor.SupportsTextQuery ||
		!reflect.DeepEqual(descriptor.ModelInput, contract) || descriptor.CompatibilityID != compatibilityID ||
		!slices.Equal(descriptor.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk}) ||
		!slices.Equal(descriptor.SupportedRequestModes, []document.ModelInputMode{document.ModelInputModeText}) {
		return errors.New("gemini embed: descriptor does not match the fixed hosted multimodal contract")
	}
	return nil
}

func modelInputContract() (document.ModelInputContract, error) {
	return document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: compatibilityID,
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "title: none | text: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "task: search result | query: {{content}}"},
	})
}

func normalizeEgress(policy *providerhttp.EgressPolicy) error {
	if err := providerhttp.NormalizeHostedEgress(policy, host); err != nil {
		if errors.Is(err, providerhttp.ErrHostedAuthority) {
			return errors.New("gemini embed: egress authority must be exactly generativelanguage.googleapis.com:443")
		}
		return fmt.Errorf("gemini embed: %s", err.Error())
	}
	return nil
}

func cloneDescriptor(value document.EmbeddingDescriptor) document.EmbeddingDescriptor {
	value.InputKinds = slices.Clone(value.InputKinds)
	value.SupportedRequestModes = slices.Clone(value.SupportedRequestModes)
	return value
}

func validToken(value string) bool {
	if value == "" || len(value) > maximumTokenBytes || !utf8.ValidString(value) {
		return false
	}
	for _, runeValue := range value {
		if runeValue < 0x21 || runeValue > 0x7e {
			return false
		}
	}
	return true
}

func validFingerprint(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func profileRetention(transport Transport) time.Duration {
	if transport == TransportFilesAPI {
		return retentionCeiling
	}
	return 0
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
