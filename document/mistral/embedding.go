package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/manifestjson"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	EmbeddingProviderID          = "mistral.embeddings-v1"
	EmbeddingDocumentFormatterV1 = "mistral/document/v1"
	EmbeddingQueryFormatterV1    = "mistral/query/v1"
	EmbeddingScalarFloat32       = "float32"
	EmbeddingModel               = "mistral-embed"
	EmbeddingHostedAliasRevision = "mutable-alias-export-only"

	embeddingOrigin          = "https://api.mistral.ai"
	embeddingPath            = "/v1/embeddings"
	embeddingAdapterContract = "docbank-mistral-embeddings/v1"

	defaultEmbeddingTimeout          = 30 * time.Second
	defaultEmbeddingMaxBatchItems    = 128
	defaultEmbeddingMaxInputBytes    = int64(1 << 20)
	defaultEmbeddingMaxRequestBytes  = int64(2 << 20)
	defaultEmbeddingMaxResponseBytes = int64(32 << 20)
	maxEmbeddingSecretBytes          = 64 << 10
	embeddingUnitLengthTolerance     = 1e-4
)

type EmbeddingProfile struct {
	Endpoint         string
	EgressPolicy     providerhttp.EgressPolicy
	Descriptor       document.EmbeddingDescriptor
	ModelInput       document.ModelInputContract
	SecretBinding    string
	RequestTimeout   time.Duration
	MaxRetries       int
	MaxRetryDelay    time.Duration
	MaxBatchItems    int
	MaxInputBytes    int64
	MaxRequestBytes  int64
	MaxResponseBytes int64
}

type embeddingPolicyIdentity struct {
	AdapterContract   string                       `json:"adapter_contract"`
	Origin            string                       `json:"origin"`
	Route             string                       `json:"route"`
	Descriptor        document.EmbeddingDescriptor `json:"descriptor"`
	ModelInput        document.ModelInputContract  `json:"model_input"`
	CredentialBinding string                       `json:"credential_binding"`
	Egress            embeddingEgressIdentity      `json:"egress"`
	RequestTimeout    int64                        `json:"request_timeout_nanos"`
	MaxRetries        int                          `json:"max_retries"`
	MaxRetryDelay     int64                        `json:"max_retry_delay_nanos"`
	MaxBatchItems     int                          `json:"max_batch_items"`
	MaxInputBytes     int64                        `json:"max_input_bytes"`
	MaxRequestBytes   int64                        `json:"max_request_bytes"`
	MaxResponseBytes  int64                        `json:"max_response_bytes"`
}

type embeddingEgressIdentity struct {
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

var ErrEmbeddingCapacity = errors.New("mistral embedding capacity exceeded")

type EmbeddingClient struct {
	profile    EmbeddingProfile
	descriptor document.EmbeddingDescriptor
	secrets    SecretResolver
	http       *http.Client
}

var _ document.EmbeddingProvider = (*EmbeddingClient)(nil)

type embeddingWireRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	EncodingFormat string   `json:"encoding_format"`
}

type embeddingWireResponse struct {
	ID     string              `json:"id"`
	Object string              `json:"object"`
	Data   []embeddingWireItem `json:"data"`
	Model  string              `json:"model"`
	Usage  embeddingWireUsage  `json:"usage"`
}

type embeddingWireItem struct {
	Object    string    `json:"object"`
	Embedding []float32 `json:"embedding"`
	Index     *int      `json:"index"`
}

type embeddingWireUsage struct {
	PromptTokens        int64                  `json:"prompt_tokens"`
	CompletionTokens    int64                  `json:"completion_tokens"`
	TotalTokens         int64                  `json:"total_tokens"`
	PromptAudioSeconds  *float64               `json:"prompt_audio_seconds"`
	PromptTokensDetails *embeddingTokenDetails `json:"prompt_tokens_details,omitempty"`
	PromptTokenDetails  *embeddingTokenDetails `json:"prompt_token_details,omitempty"`
	NumCachedTokens     *int64                 `json:"num_cached_tokens,omitempty"`
	ServiceTier         string                 `json:"service_tier,omitempty"`
}

type embeddingTokenDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

func EmbeddingPolicyFingerprint(profile EmbeddingProfile) (string, error) {
	normalized, descriptorIdentity, err := normalizeEmbeddingProfile(profile)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(embeddingPolicyIdentity{
		AdapterContract: embeddingAdapterContract, Origin: normalized.Endpoint, Route: embeddingPath,
		Descriptor: descriptorIdentity, ModelInput: normalized.ModelInput,
		CredentialBinding: normalized.SecretBinding, Egress: embeddingEgressPolicyIdentity(normalized.EgressPolicy),
		RequestTimeout: int64(normalized.RequestTimeout), MaxRetries: normalized.MaxRetries,
		MaxRetryDelay: int64(normalized.MaxRetryDelay), MaxBatchItems: normalized.MaxBatchItems,
		MaxInputBytes: normalized.MaxInputBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes,
	}, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("mistral embedding: encode policy identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func NewEmbeddingProvider(profile EmbeddingProfile, secrets SecretResolver, resolver providerhttp.Resolver) (*EmbeddingClient, error) {
	normalized, _, err := normalizeEmbeddingProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		if err == nil {
			err = errors.New("descriptor is not canonical")
		}
		return nil, fmt.Errorf("mistral embedding: invalid descriptor: %w", err)
	}
	fingerprint, err := EmbeddingPolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("mistral embedding: descriptor policy fingerprint does not match profile")
	}
	if descriptor.SupportsTextQuery {
		return nil, errors.New("mistral embedding: hosted mutable alias is export-only")
	}
	if normalized.SecretBinding == "" || nilValue(secrets) {
		return nil, errors.New("mistral embedding: named secret binding and resolver are required")
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, fmt.Errorf("mistral embedding: invalid sealed egress policy: %w", err)
	}
	normalized.Descriptor = cloneEmbeddingDescriptor(descriptor)
	return &EmbeddingClient{profile: normalized, descriptor: cloneEmbeddingDescriptor(descriptor), secrets: secrets, http: &http.Client{Transport: transport, CheckRedirect: providerhttp.RefuseRedirects}}, nil
}

func (client *EmbeddingClient) Descriptor() document.EmbeddingDescriptor {
	if client == nil {
		return document.EmbeddingDescriptor{}
	}
	return cloneEmbeddingDescriptor(client.descriptor)
}

func (client *EmbeddingClient) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if client == nil {
		return document.EmbeddingResult{}, errors.New("mistral embedding: client is required")
	}
	if ctx == nil {
		return document.EmbeddingResult{}, errors.New("mistral embedding: context is required")
	}
	if err := document.ValidateEmbeddingProviderRequest(client, inputs, authorization); err != nil {
		return document.EmbeddingResult{}, err
	}
	if authorization.MaxBatchItems > client.profile.MaxBatchItems || authorization.MaxInputBytes > client.profile.MaxInputBytes || authorization.MaxResponseBytes > client.profile.MaxResponseBytes {
		return document.EmbeddingResult{}, errors.New("mistral embedding: authorization exceeds profile capacity")
	}
	rendered := make([]string, len(inputs))
	for index, input := range inputs {
		if input.Role == document.EmbeddingRoleDocument {
			rendered[index] = client.profile.ModelInput.EncodeDocument(input.Text)
		} else {
			rendered[index] = client.profile.ModelInput.EncodeQuery(input.Text)
		}
	}
	payload, err := json.Marshal(embeddingWireRequest{Input: rendered, Model: client.descriptor.Model, EncodingFormat: "float"})
	if err != nil {
		return document.EmbeddingResult{}, errors.New("mistral embedding: could not encode request")
	}
	if int64(len(payload)) > client.profile.MaxRequestBytes {
		return document.EmbeddingResult{}, fmt.Errorf("%w: request byte limit", ErrEmbeddingCapacity)
	}
	started := time.Now()
	metrics := RequestMetrics{}
	for attempt := 1; ; attempt++ {
		result, retryHeader, retry, requested, attemptErr := client.embeddingAttempt(ctx, payload, inputs)
		if requested {
			metrics.Requests++
		}
		if attemptErr == nil {
			if err := document.ValidateEmbeddingProviderResult(client.descriptor, inputs, authorization, result); err != nil {
				return document.EmbeddingResult{}, err
			}
			return result, nil
		}
		if !retry || attempt >= client.profile.MaxRetries {
			metrics.Latency = time.Since(started)
			return document.EmbeddingResult{}, &processError{err: attemptErr, metrics: metrics}
		}
		metrics.Retries++
		if waitErr := waitContext(ctx, retryAfter(retryHeader, attempt, client.profile.MaxRetryDelay)); waitErr != nil {
			metrics.Latency = time.Since(started)
			return document.EmbeddingResult{}, &processError{err: fmt.Errorf("mistral embedding: request canceled: %w", waitErr), metrics: metrics}
		}
	}
}

func (client *EmbeddingClient) embeddingAttempt(ctx context.Context, payload []byte, inputs []document.EmbeddingInput) (document.EmbeddingResult, string, bool, bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	secret, err := client.secrets.ResolveSecret(attemptCtx, client.profile.SecretBinding)
	if err != nil || !validEmbeddingSecret(secret) {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, "", false, false, contextErr
		}
		return document.EmbeddingResult{}, "", false, false, fmt.Errorf("%w: credential unavailable", ErrPermanentResponse)
	}
	request, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, client.profile.Endpoint+embeddingPath, bytes.NewReader(payload))
	if err != nil {
		return document.EmbeddingResult{}, "", false, false, fmt.Errorf("%w: request construction", ErrPermanentResponse)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, "", false, true, contextErr
		}
		return document.EmbeddingResult{}, "", true, true, fmt.Errorf("%w: transport", ErrTransientResponse)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return document.EmbeddingResult{}, "", false, true, fmt.Errorf("%w: redirect HTTP %d", ErrPermanentResponse, response.StatusCode)
	}
	if response.StatusCode == http.StatusRequestEntityTooLarge {
		return document.EmbeddingResult{}, "", false, true, fmt.Errorf("%w: HTTP %d", ErrEmbeddingCapacity, response.StatusCode)
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return document.EmbeddingResult{}, response.Header.Get("Retry-After"), true, true, fmt.Errorf("%w: HTTP %d", ErrTransientResponse, response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return document.EmbeddingResult{}, "", false, true, fmt.Errorf("%w: HTTP %d", ErrPermanentResponse, response.StatusCode)
	}
	if err := validateEmbeddingContentType(response.Header.Get("Content-Type")); err != nil {
		return document.EmbeddingResult{}, "", false, true, fmt.Errorf("%w: response content type", ErrPermanentResponse)
	}
	body, err := readEmbeddingBody(attemptCtx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, "", false, true, contextErr
		}
		return document.EmbeddingResult{}, "", false, true, fmt.Errorf("%w: bounded response", ErrPermanentResponse)
	}
	if err := manifestjson.RejectDuplicateKeys(body, "mistral embedding response"); err != nil {
		return document.EmbeddingResult{}, "", false, true, fmt.Errorf("%w: duplicate response member", ErrPermanentResponse)
	}
	var decoded embeddingWireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return document.EmbeddingResult{}, "", false, true, fmt.Errorf("%w: malformed response", ErrPermanentResponse)
	}
	result, err := client.validateAndOrder(decoded, inputs)
	if err != nil {
		err = fmt.Errorf("%w: invalid response", ErrPermanentResponse)
	}
	return result, "", false, true, err
}

func (client *EmbeddingClient) validateAndOrder(response embeddingWireResponse, inputs []document.EmbeddingInput) (document.EmbeddingResult, error) {
	if response.ID == "" || response.Object != "list" || response.Model != client.descriptor.Model {
		return document.EmbeddingResult{}, errors.New("mistral embedding: provider model or response contract drifted")
	}
	if response.Usage.PromptTokens < 0 || response.Usage.CompletionTokens < 0 || response.Usage.TotalTokens < response.Usage.PromptTokens+response.Usage.CompletionTokens {
		return document.EmbeddingResult{}, errors.New("mistral embedding: provider usage is invalid")
	}
	for _, details := range []*embeddingTokenDetails{response.Usage.PromptTokensDetails, response.Usage.PromptTokenDetails} {
		if details != nil && (details.CachedTokens < 0 || details.CachedTokens > response.Usage.PromptTokens) {
			return document.EmbeddingResult{}, errors.New("mistral embedding: provider usage is invalid")
		}
	}
	if response.Usage.NumCachedTokens != nil && (*response.Usage.NumCachedTokens < 0 || *response.Usage.NumCachedTokens > response.Usage.PromptTokens) {
		return document.EmbeddingResult{}, errors.New("mistral embedding: provider usage is invalid")
	}
	if response.Usage.ServiceTier != "" && response.Usage.ServiceTier != "standard" && response.Usage.ServiceTier != "priority" {
		return document.EmbeddingResult{}, errors.New("mistral embedding: provider usage is invalid")
	}
	if len(response.Data) != len(inputs) {
		return document.EmbeddingResult{}, errors.New("mistral embedding: provider response has a missing vector")
	}
	vectors := make([]document.EmbeddingVector, len(inputs))
	seen := make([]bool, len(inputs))
	for _, item := range response.Data {
		if item.Object != "embedding" || item.Index == nil || *item.Index < 0 || *item.Index >= len(inputs) || seen[*item.Index] {
			return document.EmbeddingResult{}, errors.New("mistral embedding: provider response index contract drifted")
		}
		if err := client.validateVector(item.Embedding); err != nil {
			return document.EmbeddingResult{}, err
		}
		seen[*item.Index] = true
		vectors[*item.Index] = document.EmbeddingVector{Key: inputs[*item.Index].Key, Values: slices.Clone(item.Embedding)}
	}
	if slices.Contains(seen, false) {
		return document.EmbeddingResult{}, errors.New("mistral embedding: provider response has a missing vector index")
	}
	return document.EmbeddingResult{Vectors: vectors}, nil
}

func (client *EmbeddingClient) validateVector(vector []float32) error {
	if len(vector) != client.descriptor.Dimension {
		return errors.New("mistral embedding: provider vector dimension does not match profile")
	}
	var norm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return errors.New("mistral embedding: provider vector contains a non-finite value")
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return errors.New("mistral embedding: provider returned a zero vector")
	}
	if client.descriptor.Normalization == document.VectorNormalizationUnitLength && math.Abs(norm-1) > embeddingUnitLengthTolerance {
		return errors.New("mistral embedding: provider vector normalization does not match profile")
	}
	return nil
}

func normalizeEmbeddingProfile(profile EmbeddingProfile) (EmbeddingProfile, document.EmbeddingDescriptor, error) {
	if profile.Endpoint == "" {
		profile.Endpoint = embeddingOrigin
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = defaultEmbeddingTimeout
	}
	if profile.MaxRetries == 0 {
		profile.MaxRetries = DefaultMaxRetries
	}
	if profile.MaxRetryDelay == 0 {
		profile.MaxRetryDelay = DefaultMaxRetryDelay
	}
	if profile.MaxBatchItems == 0 {
		profile.MaxBatchItems = defaultEmbeddingMaxBatchItems
	}
	if profile.MaxInputBytes == 0 {
		profile.MaxInputBytes = defaultEmbeddingMaxInputBytes
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = defaultEmbeddingMaxRequestBytes
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = defaultEmbeddingMaxResponseBytes
	}
	if profile.RequestTimeout <= 0 || profile.RequestTimeout > MaxTimeout || profile.MaxRetries < 1 || profile.MaxRetries > MaxRetries || profile.MaxRetryDelay <= 0 || profile.MaxRetryDelay > MaxRetryDelay || profile.MaxBatchItems < 1 || profile.MaxBatchItems > 1000 || profile.MaxInputBytes < 1 || profile.MaxRequestBytes < 1 || profile.MaxResponseBytes < 1 {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, errors.New("mistral embedding: execution bounds are invalid")
	}
	if !validEmbeddingBinding(profile.SecretBinding) {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, errors.New("mistral embedding: secret binding is invalid")
	}
	if err := normalizeAndValidateEmbeddingEgress(&profile); err != nil {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, err
	}
	descriptorIdentity := profile.Descriptor
	descriptorIdentity.PolicyFingerprint = strings.Repeat("0", sha256.Size*2)
	descriptorIdentity.Fingerprint = ""
	var err error
	descriptorIdentity, err = document.NewEmbeddingDescriptor(descriptorIdentity)
	if err != nil {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, fmt.Errorf("mistral embedding: invalid descriptor identity: %w", err)
	}
	descriptorIdentity.PolicyFingerprint = ""
	descriptorIdentity.Fingerprint = ""
	if descriptorIdentity.SupportsTextQuery {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, errors.New("mistral embedding: hosted mutable alias is export-only")
	}
	if descriptorIdentity.ID != EmbeddingProviderID || descriptorIdentity.TrustBoundary != document.EmbeddingTrustHostedProvider || descriptorIdentity.Model != EmbeddingModel || descriptorIdentity.ModelRevision != EmbeddingHostedAliasRevision || descriptorIdentity.Dimension != 1024 || descriptorIdentity.ScalarEncoding != EmbeddingScalarFloat32 || descriptorIdentity.DocumentFormatter != EmbeddingDocumentFormatterV1 || descriptorIdentity.QueryFormatter != EmbeddingQueryFormatterV1 || !slices.Equal(descriptorIdentity.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}) || !slices.Equal(descriptorIdentity.SupportedRequestModes, []document.ModelInputMode{document.ModelInputModeText}) || descriptorIdentity.ModelInput != profile.ModelInput || descriptorIdentity.CompatibilityID != profile.ModelInput.CompatibilityID || profile.ModelInput.Document.Mode != document.ModelInputModeText || profile.ModelInput.Query.Mode != document.ModelInputModeText {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, errors.New("mistral embedding: descriptor does not match text adapter contract")
	}
	return profile, descriptorIdentity, nil
}

func normalizeAndValidateEmbeddingEgress(profile *EmbeddingProfile) error {
	if profile.EgressPolicy.ConnectTimeout == 0 {
		profile.EgressPolicy.ConnectTimeout = providerhttp.DefaultConnectTimeout
	}
	if profile.EgressPolicy.KeepAlive == 0 {
		profile.EgressPolicy.KeepAlive = providerhttp.DefaultKeepAlive
	}
	if profile.EgressPolicy.TLSHandshakeTimeout == 0 {
		profile.EgressPolicy.TLSHandshakeTimeout = providerhttp.DefaultTLSHandshakeTimeout
	}
	if profile.EgressPolicy.ProxyMode == "" {
		profile.EgressPolicy.ProxyMode = providerhttp.ProxyDisabled
	}
	if profile.EgressPolicy.TLS.RootCAs != nil {
		return errors.New("mistral embedding: custom egress roots cannot enter canonical identity")
	}
	parsed, err := url.Parse(profile.Endpoint)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("mistral embedding: endpoint must be an exact provider origin")
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	if parsed.Scheme != profile.EgressPolicy.Scheme || !strings.EqualFold(parsed.Hostname(), profile.EgressPolicy.Host) || port != strconv.FormatUint(uint64(profile.EgressPolicy.Port), 10) {
		return errors.New("mistral embedding: endpoint and egress authority differ")
	}
	profile.Endpoint = strings.TrimSuffix(profile.Endpoint, "/")
	slices.SortFunc(profile.EgressPolicy.AllowedCIDRs, func(a, b netip.Prefix) int { return strings.Compare(a.Masked().String(), b.Masked().String()) })
	slices.Sort(profile.EgressPolicy.TLS.SPKISHA256)
	return nil
}

func embeddingEgressPolicyIdentity(policy providerhttp.EgressPolicy) embeddingEgressIdentity {
	cidrs := make([]string, len(policy.AllowedCIDRs))
	for index, prefix := range policy.AllowedCIDRs {
		cidrs[index] = prefix.Masked().String()
	}
	return embeddingEgressIdentity{Scheme: policy.Scheme, Host: strings.ToLower(policy.Host), Port: policy.Port, AllowedCIDRs: cidrs, ProxyMode: string(policy.ProxyMode), ConnectTimeout: int64(policy.ConnectTimeout), KeepAlive: int64(policy.KeepAlive), TLSHandshakeTimeout: int64(policy.TLSHandshakeTimeout), SPKISHA256: slices.Clone(policy.TLS.SPKISHA256)}
}

func readEmbeddingBody(ctx context.Context, reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("mistral embedding: response read canceled: %w", contextErr)
		}
		return nil, errors.New("mistral embedding: could not read provider response")
	}
	if int64(len(body)) > maximum {
		return nil, errors.New("mistral embedding: provider response byte limit exceeded")
	}
	return body, nil
}

func validateEmbeddingContentType(value string) error {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" || (len(parameters) != 0 && (len(parameters) != 1 || !strings.EqualFold(parameters["charset"], "utf-8"))) {
		return errors.New("mistral embedding: provider response content type is invalid")
	}
	return nil
}

func validEmbeddingSecret(secret string) bool {
	if secret == "" || len(secret) > maxEmbeddingSecretBytes {
		return false
	}
	for _, character := range secret {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func validEmbeddingBinding(value string) bool {
	return value != "" && len(value) <= 128 && value == strings.TrimSpace(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func cloneEmbeddingDescriptor(value document.EmbeddingDescriptor) document.EmbeddingDescriptor {
	value.InputKinds = slices.Clone(value.InputKinds)
	value.SupportedRequestModes = slices.Clone(value.SupportedRequestModes)
	return value
}
