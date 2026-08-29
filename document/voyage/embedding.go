package voyage

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
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/manifestjson"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	EmbeddingProviderID          = "voyage.embeddings-v1"
	EmbeddingDocumentFormatterV1 = "voyage/document/v1"
	EmbeddingQueryFormatterV1    = "voyage/query/v1"
	EmbeddingScalarFloat32       = "float32"
	TextModel                    = "voyage-4"
	ContextualModel              = "voyage-context-4"
	HostedAliasRevision          = "mutable-alias-export-only"

	textEmbeddingsPath       = "/embeddings"
	contextualEmbeddingsPath = "/contextualizedembeddings"
	embeddingAdapterContract = "docbank-voyage-embeddings/v1"

	defaultEmbeddingMaxBatchItems    = 128
	defaultEmbeddingMaxInputBytes    = int64(1 << 20)
	defaultEmbeddingMaxRequestBytes  = int64(2 << 20)
	defaultEmbeddingMaxResponseBytes = int64(32 << 20)
	maxEmbeddingSecretBytes          = 64 << 10
	unitLengthTolerance              = 1e-4
)

type EmbeddingMode string

const (
	EmbeddingModeText       EmbeddingMode = "text"
	EmbeddingModeContextual EmbeddingMode = "contextual"
	EmbeddingModeDirectFile EmbeddingMode = "direct_file"
)

type SecretResolver interface {
	ResolveSecret(ctx context.Context, name string) (string, error)
}

type EmbeddingProfile struct {
	Mode               EmbeddingMode
	Endpoint           string
	EgressPolicy       providerhttp.EgressPolicy
	Descriptor         document.EmbeddingDescriptor
	ModelInput         document.ModelInputContract
	SecretBinding      string
	ChunkerVersion     string
	RequestTimeout     time.Duration
	MaxRetries         int
	RetryBaseDelay     time.Duration
	MaxBatchItems      int
	MaxInputBytes      int64
	MaxRequestBytes    int64
	MaxResponseBytes   int64
	Policy             Policy
	CapabilityManifest CapabilityManifest
}

type embeddingPolicyIdentity struct {
	AdapterContract   string                       `json:"adapter_contract"`
	Endpoint          string                       `json:"endpoint"`
	Route             string                       `json:"route"`
	Mode              EmbeddingMode                `json:"mode"`
	Descriptor        document.EmbeddingDescriptor `json:"descriptor"`
	ModelInput        document.ModelInputContract  `json:"model_input"`
	CredentialBinding string                       `json:"credential_binding"`
	Egress            embeddingEgressIdentity      `json:"egress"`
	ChunkerVersion    string                       `json:"chunker_version,omitempty"`
	RequestTimeout    int64                        `json:"request_timeout_nanos"`
	MaxRetries        int                          `json:"max_retries"`
	RetryBaseDelay    int64                        `json:"retry_base_delay_nanos"`
	MaxBatchItems     int                          `json:"max_batch_items"`
	MaxInputBytes     int64                        `json:"max_input_bytes"`
	MaxRequestBytes   int64                        `json:"max_request_bytes"`
	MaxResponseBytes  int64                        `json:"max_response_bytes"`
	CapabilityPolicy  string                       `json:"capability_policy,omitempty"`
}

type EmbeddingClient struct {
	profile    EmbeddingProfile
	descriptor document.EmbeddingDescriptor
	secrets    SecretResolver
	http       *http.Client
	now        func() time.Time
}

var _ document.EmbeddingProvider = (*EmbeddingClient)(nil)

type embeddingWireRequest struct {
	Input           []string   `json:"input,omitempty"`
	Inputs          [][]string `json:"inputs,omitempty"`
	Model           string     `json:"model"`
	InputType       string     `json:"input_type"`
	Truncation      bool       `json:"truncation"`
	OutputDimension int        `json:"output_dimension"`
	OutputDType     string     `json:"output_dtype"`
}

type contextualWireRequest struct {
	Inputs          [][]string `json:"inputs"`
	Model           string     `json:"model"`
	InputType       string     `json:"input_type"`
	OutputDimension int        `json:"output_dimension"`
	OutputDType     string     `json:"output_dtype"`
}

type embeddingWireItem struct {
	Object    string    `json:"object"`
	Embedding []float32 `json:"embedding"`
	Index     *int      `json:"index"`
}

type embeddingWireResponse struct {
	Object string              `json:"object"`
	Data   []embeddingWireItem `json:"data"`
	Model  string              `json:"model"`
	Usage  *struct {
		TotalTokens int64 `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

type contextualWireGroup struct {
	Data  []contextualWireItem `json:"data"`
	Index *int                 `json:"index"`
}

type contextualWireItem struct {
	Embedding []float32 `json:"embedding"`
	Index     *int      `json:"index"`
	Text      string    `json:"text"`
}

type contextualWireResponse struct {
	Data           []contextualWireGroup `json:"data"`
	Model          string                `json:"model"`
	ChunkerVersion string                `json:"chunker_version"`
	Usage          *struct {
		TotalTokens int64 `json:"total_tokens"`
	} `json:"usage,omitempty"`
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

func EmbeddingPolicyFingerprint(profile EmbeddingProfile) (string, error) {
	normalized, descriptorIdentity, route, err := normalizeEmbeddingProfile(profile)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(embeddingPolicyIdentity{
		AdapterContract: embeddingAdapterContract, Endpoint: normalized.Endpoint, Route: route,
		Mode: normalized.Mode, Descriptor: descriptorIdentity, ModelInput: normalized.ModelInput,
		CredentialBinding: normalized.SecretBinding, Egress: embeddingEgressPolicyIdentity(normalized.EgressPolicy), ChunkerVersion: normalized.ChunkerVersion,
		RequestTimeout: int64(normalized.RequestTimeout), MaxRetries: normalized.MaxRetries,
		RetryBaseDelay: int64(normalized.RetryBaseDelay), MaxBatchItems: normalized.MaxBatchItems,
		MaxInputBytes: normalized.MaxInputBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes,
		CapabilityPolicy: directCapabilityFingerprint(normalized),
	}, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("voyage embedding: encode policy identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func NewEmbeddingProvider(profile EmbeddingProfile, secrets SecretResolver, resolver providerhttp.Resolver) (*EmbeddingClient, error) {
	normalized, _, _, err := normalizeEmbeddingProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		if err == nil {
			err = errors.New("descriptor is not canonical")
		}
		return nil, fmt.Errorf("voyage embedding: invalid descriptor: %w", err)
	}
	fingerprint, err := EmbeddingPolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("voyage embedding: descriptor policy fingerprint does not match profile")
	}
	if descriptor.SupportsTextQuery {
		return nil, errors.New("voyage embedding: hosted mutable alias is export-only")
	}
	if normalized.SecretBinding == "" || nilEmbeddingValue(secrets) {
		return nil, errors.New("voyage embedding: named secret binding and resolver are required")
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, fmt.Errorf("voyage embedding: invalid sealed egress policy: %w", err)
	}
	normalized.Descriptor = cloneEmbeddingDescriptor(descriptor)
	return &EmbeddingClient{profile: normalized, descriptor: cloneEmbeddingDescriptor(descriptor), secrets: secrets, http: &http.Client{Transport: transport, CheckRedirect: providerhttp.RefuseRedirects}, now: time.Now}, nil
}

func (client *EmbeddingClient) Descriptor() document.EmbeddingDescriptor {
	if client == nil {
		return document.EmbeddingDescriptor{}
	}
	return cloneEmbeddingDescriptor(client.descriptor)
}

func (client *EmbeddingClient) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if client == nil {
		return document.EmbeddingResult{}, errors.New("voyage embedding: client is required")
	}
	if ctx == nil {
		return document.EmbeddingResult{}, errors.New("voyage embedding: context is required")
	}
	if err := document.ValidateEmbeddingProviderRequest(client, inputs, authorization); err != nil {
		return document.EmbeddingResult{}, err
	}
	if authorization.MaxBatchItems > client.profile.MaxBatchItems || authorization.MaxInputBytes > client.profile.MaxInputBytes || authorization.MaxResponseBytes > client.profile.MaxResponseBytes {
		return document.EmbeddingResult{}, errors.New("voyage embedding: authorization exceeds profile capacity")
	}
	if client.profile.Mode == EmbeddingModeDirectFile {
		return client.embedDirectFiles(ctx, inputs, authorization)
	}
	result := document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, len(inputs))}
	for _, role := range []document.EmbeddingRole{document.EmbeddingRoleDocument, document.EmbeddingRoleQuery} {
		positions := make([]int, 0, len(inputs))
		rendered := make([]string, 0, len(inputs))
		for index, input := range inputs {
			if input.Role != role {
				continue
			}
			positions = append(positions, index)
			if role == document.EmbeddingRoleDocument {
				rendered = append(rendered, client.profile.ModelInput.EncodeDocument(input.Text))
			} else {
				rendered = append(rendered, client.profile.ModelInput.EncodeQuery(input.Text))
			}
		}
		if len(positions) == 0 {
			continue
		}
		vectors, err := client.embedTextRole(ctx, rendered, string(role))
		if err != nil {
			return document.EmbeddingResult{}, err
		}
		for local, global := range positions {
			result.Vectors[global] = document.EmbeddingVector{Key: inputs[global].Key, Values: vectors[local]}
		}
	}
	if err := document.ValidateEmbeddingProviderResult(client.descriptor, inputs, authorization, result); err != nil {
		return document.EmbeddingResult{}, err
	}
	return result, nil
}

func (client *EmbeddingClient) embedDirectFiles(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	secret, err := client.secrets.ResolveSecret(ctx, client.profile.SecretBinding)
	if err != nil || !validEmbeddingSecret(secret) {
		if contextErr := ctx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, fmt.Errorf("voyage embedding: credential resolution canceled: %w", contextErr)
		}
		return document.EmbeddingResult{}, errors.New("voyage embedding: credential is unavailable")
	}
	legacy, err := NewClient(client.profile.Policy, ClientConfig{
		APIKey: secret, Timeout: client.profile.RequestTimeout, MaxRetries: client.profile.MaxRetries,
		RetryBaseDelay: client.profile.RetryBaseDelay, HTTPClient: client.http,
	})
	if err != nil {
		return document.EmbeddingResult{}, errors.New("voyage embedding: direct-file client configuration failed")
	}
	authorities, err := client.profile.Policy.AuthorizeAll(client.profile.CapabilityManifest)
	if err != nil {
		return document.EmbeddingResult{}, errors.New("voyage embedding: direct-file capability authority is invalid")
	}
	direct := make([]Input, len(inputs))
	for index, input := range inputs {
		metadata := input.Source.Metadata()
		data, readErr := io.ReadAll(io.LimitReader(input.Source, metadata.ByteLength+1))
		closeErr := input.Source.Close()
		if readErr != nil || closeErr != nil || int64(len(data)) != metadata.ByteLength {
			clear(data)
			return document.EmbeddingResult{}, errors.New("voyage embedding: direct-file source could not be read exactly")
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != metadata.SHA256 {
			clear(data)
			return document.EmbeddingResult{}, errors.New("voyage embedding: direct-file source identity changed")
		}
		detected, detectErr := media.DetectBytes(data, metadata.MediaType)
		if detectErr != nil || string(detected.Kind) != metadata.MediaFamily || detected.MediaType != metadata.MediaType {
			clear(data)
			return document.EmbeddingResult{}, errors.New("voyage embedding: direct-file media identity could not be verified")
		}
		direct[index] = Input{Parts: []Part{{Media: &Media{Metadata: detected, Bytes: data}}}}
	}
	defer func() {
		for index := range direct {
			clear(direct[index].Parts[0].Media.Bytes)
		}
	}()
	providerResult, err := legacy.EmbedDocuments(ctx, direct, authorities)
	if err != nil {
		return document.EmbeddingResult{}, fmt.Errorf("voyage embedding: direct-file provider request failed: %w", err)
	}
	result := document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, len(inputs))}
	for index, vector := range providerResult.Vectors {
		result.Vectors[index] = document.EmbeddingVector{Key: inputs[index].Key, Values: slices.Clone(vector)}
	}
	if err := document.ValidateEmbeddingProviderResult(client.descriptor, inputs, authorization, result); err != nil {
		return document.EmbeddingResult{}, err
	}
	return result, nil
}

func (client *EmbeddingClient) embedTextRole(ctx context.Context, rendered []string, inputType string) ([][]float32, error) {
	route := textEmbeddingsPath
	var payload []byte
	var err error
	if client.profile.Mode == EmbeddingModeContextual {
		route = contextualEmbeddingsPath
		payload, err = json.Marshal(contextualWireRequest{Inputs: [][]string{rendered}, Model: client.descriptor.Model, InputType: inputType, OutputDimension: client.descriptor.Dimension, OutputDType: "float"})
	} else {
		payload, err = json.Marshal(embeddingWireRequest{Input: rendered, Model: client.descriptor.Model, InputType: inputType, Truncation: false, OutputDimension: client.descriptor.Dimension, OutputDType: "float"})
	}
	if err != nil {
		return nil, errors.New("voyage embedding: could not encode request")
	}
	if int64(len(payload)) > client.profile.MaxRequestBytes {
		return nil, &ProviderError{Kind: ErrBatchTooLarge}
	}
	started := time.Now()
	metrics := RequestMetrics{}
	for attempt := 1; ; attempt++ {
		vectors, retryAfter, retry, requested, err := client.embeddingAttempt(ctx, route, payload, rendered)
		if requested {
			metrics.Requests++
		}
		if err == nil {
			return vectors, nil
		}
		if !retry || attempt >= client.profile.MaxRetries {
			if providerErr, ok := errors.AsType[*ProviderError](err); ok {
				providerErr.Metrics = metrics
				providerErr.Metrics.Latency = time.Since(started)
			}
			return nil, err
		}
		metrics.Retries++
		delay := retryBackoffDelay(client.profile.RetryBaseDelay, attempt)
		if retryAfter >= 0 {
			delay = retryAfter
		}
		if waitErr := sleepContext(ctx, delay); waitErr != nil {
			return nil, &ProviderError{Kind: waitErr, cause: waitErr, Metrics: RequestMetrics{Requests: metrics.Requests, Retries: metrics.Retries, Latency: time.Since(started)}}
		}
	}
}

func (client *EmbeddingClient) embeddingAttempt(ctx context.Context, route string, payload []byte, rendered []string) ([][]float32, time.Duration, bool, bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	secret, err := client.secrets.ResolveSecret(attemptCtx, client.profile.SecretBinding)
	if err != nil || !validEmbeddingSecret(secret) {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return nil, -1, false, false, &ProviderError{Kind: contextErr, cause: contextErr}
		}
		return nil, -1, false, false, &ProviderError{Kind: ErrPermanentResponse}
	}
	request, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, client.profile.Endpoint+route, bytes.NewReader(payload))
	if err != nil {
		return nil, -1, false, false, &ProviderError{Kind: ErrPermanentResponse}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return nil, -1, false, true, &ProviderError{Kind: contextErr, cause: contextErr}
		}
		return nil, -1, true, true, &ProviderError{Kind: ErrTransientResponse, cause: err}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, -1, false, true, &ProviderError{Kind: ErrPermanentResponse, StatusCode: response.StatusCode}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, -1, false, true, &ProviderError{Kind: ErrUnauthorized, StatusCode: response.StatusCode}
	}
	if response.StatusCode == http.StatusRequestEntityTooLarge {
		return nil, -1, false, true, &ProviderError{Kind: ErrBatchTooLarge, StatusCode: response.StatusCode}
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		delay, set := parseRetryAfter(response.Header.Get("Retry-After"), client.now())
		if !set {
			delay = -1
		}
		providerErr := &ProviderError{Kind: ErrTransientResponse, StatusCode: response.StatusCode, RetryAfter: delay, RetrySet: delay >= 0}
		return nil, delay, true, true, providerErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, -1, false, true, &ProviderError{Kind: ErrPermanentResponse, StatusCode: response.StatusCode}
	}
	if err := validateEmbeddingContentType(response.Header.Get("Content-Type")); err != nil {
		return nil, -1, false, true, &ProviderError{Kind: ErrMalformedResponse}
	}
	body, err := readEmbeddingBody(attemptCtx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		return nil, -1, false, true, &ProviderError{Kind: ErrMalformedResponse, cause: err}
	}
	if err := manifestjson.RejectDuplicateKeys(body, "voyage embedding response"); err != nil {
		return nil, -1, false, true, &ProviderError{Kind: ErrMalformedResponse}
	}
	if client.profile.Mode == EmbeddingModeContextual {
		vectors, decodeErr := client.decodeContextual(body, rendered)
		return vectors, -1, false, true, decodeErr
	}
	vectors, decodeErr := client.decodeText(body, len(rendered))
	return vectors, -1, false, true, decodeErr
}

func (client *EmbeddingClient) decodeText(body []byte, want int) ([][]float32, error) {
	var response embeddingWireResponse
	if err := json.Unmarshal(body, &response, json.RejectUnknownMembers(true)); err != nil {
		return nil, &ProviderError{Kind: ErrMalformedResponse}
	}
	vectors, err := client.orderItems(response.Object, response.Model, response.Data, want)
	if err != nil {
		err = &ProviderError{Kind: ErrMalformedResponse, cause: err}
	}
	return vectors, err
}

func (client *EmbeddingClient) decodeContextual(body []byte, rendered []string) ([][]float32, error) {
	var response contextualWireResponse
	if err := json.Unmarshal(body, &response, json.RejectUnknownMembers(true)); err != nil || response.Model != client.descriptor.Model || response.ChunkerVersion != client.profile.ChunkerVersion {
		return nil, &ProviderError{Kind: ErrMalformedResponse}
	}
	if len(response.Data) != 1 || response.Data[0].Index == nil || *response.Data[0].Index != 0 || len(response.Data[0].Data) != len(rendered) {
		return nil, &ProviderError{Kind: ErrMalformedResponse}
	}
	vectors := make([][]float32, len(rendered))
	seen := make([]bool, len(rendered))
	for _, item := range response.Data[0].Data {
		if item.Index == nil || *item.Index < 0 || *item.Index >= len(rendered) || seen[*item.Index] || item.Text != rendered[*item.Index] || client.validateEmbeddingVector(item.Embedding) != nil {
			return nil, &ProviderError{Kind: ErrMalformedResponse}
		}
		seen[*item.Index] = true
		vectors[*item.Index] = slices.Clone(item.Embedding)
	}
	if slices.Contains(seen, false) {
		return nil, &ProviderError{Kind: ErrMalformedResponse}
	}
	return vectors, nil
}

func (client *EmbeddingClient) orderItems(object, model string, items []embeddingWireItem, want int) ([][]float32, error) {
	if object != "list" || model != client.descriptor.Model {
		return nil, errors.New("voyage embedding: provider model or response contract drifted")
	}
	if len(items) != want {
		return nil, errors.New("voyage embedding: provider response has a missing vector")
	}
	vectors := make([][]float32, want)
	seen := make([]bool, want)
	for _, item := range items {
		if item.Object != "embedding" || item.Index == nil || *item.Index < 0 || *item.Index >= want || seen[*item.Index] {
			return nil, errors.New("voyage embedding: provider response index contract drifted")
		}
		if err := client.validateEmbeddingVector(item.Embedding); err != nil {
			return nil, err
		}
		seen[*item.Index] = true
		vectors[*item.Index] = slices.Clone(item.Embedding)
	}
	if slices.Contains(seen, false) {
		return nil, errors.New("voyage embedding: provider response has a missing vector index")
	}
	return vectors, nil
}

func (client *EmbeddingClient) validateEmbeddingVector(vector []float32) error {
	if len(vector) != client.descriptor.Dimension {
		return errors.New("voyage embedding: provider vector dimension does not match profile")
	}
	var norm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return errors.New("voyage embedding: provider vector contains a non-finite value")
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return errors.New("voyage embedding: provider returned a zero vector")
	}
	if client.descriptor.Normalization == document.VectorNormalizationUnitLength && math.Abs(norm-1) > unitLengthTolerance {
		return errors.New("voyage embedding: provider vector normalization does not match profile")
	}
	return nil
}

func normalizeEmbeddingProfile(profile EmbeddingProfile) (EmbeddingProfile, document.EmbeddingDescriptor, string, error) {
	if profile.Endpoint == "" {
		profile.Endpoint = DefaultEndpoint
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = DefaultTimeout
	}
	if profile.MaxRetries == 0 {
		profile.MaxRetries = DefaultMaxRetries
	}
	if profile.RetryBaseDelay == 0 {
		profile.RetryBaseDelay = defaultRetryBaseDelay
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
	if profile.RequestTimeout <= 0 || profile.RequestTimeout > MaxTimeout || profile.MaxRetries < 1 || profile.MaxRetries > MaxRetries || profile.RetryBaseDelay < 0 || profile.RetryBaseDelay > maxRetryAfter || profile.MaxBatchItems < 1 || profile.MaxBatchItems > 1000 || profile.MaxInputBytes < 1 || profile.MaxRequestBytes < 1 || profile.MaxResponseBytes < 1 {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: execution bounds are invalid")
	}
	if !validEmbeddingToken(profile.SecretBinding) {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: binding is invalid")
	}
	if err := normalizeAndValidateEmbeddingEgress(&profile); err != nil {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", err
	}
	descriptorIdentity := profile.Descriptor
	descriptorIdentity.PolicyFingerprint = strings.Repeat("0", sha256.Size*2)
	descriptorIdentity.Fingerprint = ""
	var err error
	descriptorIdentity, err = document.NewEmbeddingDescriptor(descriptorIdentity)
	if err != nil {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", fmt.Errorf("voyage embedding: invalid descriptor identity: %w", err)
	}
	descriptorIdentity.PolicyFingerprint = ""
	descriptorIdentity.Fingerprint = ""
	if descriptorIdentity.ID != EmbeddingProviderID || descriptorIdentity.TrustBoundary != document.EmbeddingTrustHostedProvider || descriptorIdentity.ScalarEncoding != EmbeddingScalarFloat32 || descriptorIdentity.DocumentFormatter != EmbeddingDocumentFormatterV1 || descriptorIdentity.QueryFormatter != EmbeddingQueryFormatterV1 || descriptorIdentity.ModelInput != profile.ModelInput || descriptorIdentity.CompatibilityID != profile.ModelInput.CompatibilityID {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: descriptor does not match adapter contract")
	}
	if profile.Mode != EmbeddingModeDirectFile && descriptorIdentity.SupportsTextQuery {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: hosted mutable alias is export-only")
	}
	if profile.Mode != EmbeddingModeDirectFile && (!slices.Equal(descriptorIdentity.SupportedRequestModes, []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery}) || profile.ModelInput.Document.Mode != document.ModelInputModeDocument || profile.ModelInput.Query.Mode != document.ModelInputModeQuery) {
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: descriptor native role modes do not match document/query behavior")
	}
	route := textEmbeddingsPath
	switch profile.Mode {
	case EmbeddingModeText:
		if !slices.Contains([]string{"voyage-4-large", TextModel, "voyage-4-lite"}, descriptorIdentity.Model) || descriptorIdentity.ModelRevision != HostedAliasRevision || descriptorIdentity.SupportsTextQuery || !slices.Contains([]int{2048, 1024, 512, 256}, descriptorIdentity.Dimension) || !slices.Equal(descriptorIdentity.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}) || !slices.Equal(descriptorIdentity.SupportedRequestModes, []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery}) || profile.ModelInput.Document.Mode != document.ModelInputModeDocument || profile.ModelInput.Query.Mode != document.ModelInputModeQuery {
			return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: descriptor is not a pinned Voyage 4 text profile")
		}
	case EmbeddingModeContextual:
		route = contextualEmbeddingsPath
		if !validEmbeddingToken(profile.ChunkerVersion) || descriptorIdentity.Model != ContextualModel || descriptorIdentity.ModelRevision != HostedAliasRevision || descriptorIdentity.SupportsTextQuery || !slices.Contains([]int{2048, 1024, 512, 256}, descriptorIdentity.Dimension) || !slices.Equal(descriptorIdentity.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}) || !slices.Equal(descriptorIdentity.SupportedRequestModes, []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery}) || profile.ModelInput.Document.Mode != document.ModelInputModeDocument || profile.ModelInput.Query.Mode != document.ModelInputModeQuery {
			return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: descriptor is not a pinned contextual profile")
		}
	case EmbeddingModeDirectFile:
		route = embeddingsPath
		if !profile.Policy.valid() {
			return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: direct-file policy is invalid")
		}
		if profile.Endpoint != profile.Policy.values.Endpoint {
			return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: direct-file endpoint differs from capability policy")
		}
		if err := profile.CapabilityManifest.ValidateComplete(); err != nil {
			return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", fmt.Errorf("voyage embedding: direct-file capability evidence: %w", err)
		}
		capabilityFingerprint, err := profile.Policy.Fingerprint(profile.CapabilityManifest)
		if err != nil {
			return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", fmt.Errorf("voyage embedding: direct-file capability policy: %w", err)
		}
		expectedRevision := "capability-" + capabilityFingerprint[:32]
		if descriptorIdentity.Model != profile.Policy.values.Model || descriptorIdentity.Dimension != profile.Policy.values.Dimension || descriptorIdentity.ModelRevision != expectedRevision || descriptorIdentity.SupportsTextQuery || !slices.Equal(descriptorIdentity.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}) || !slices.Contains(descriptorIdentity.SupportedRequestModes, document.ModelInputModeDocument) {
			return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: direct-file descriptor is not the exact capability-attested export profile")
		}
	default:
		return EmbeddingProfile{}, document.EmbeddingDescriptor{}, "", errors.New("voyage embedding: mode is invalid")
	}
	return profile, descriptorIdentity, route, nil
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
		return errors.New("voyage embedding: custom egress roots cannot enter canonical identity")
	}
	parsed, err := url.Parse(profile.Endpoint)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.TrimSuffix(parsed.Path, "/") != "/v1" {
		return errors.New("voyage embedding: endpoint must be an exact /v1 provider root")
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
		return errors.New("voyage embedding: endpoint and egress authority differ")
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

func DirectFileModelRevision(policy Policy, manifest CapabilityManifest) (string, error) {
	fingerprint, err := policy.Fingerprint(manifest)
	if err != nil {
		return "", err
	}
	return "capability-" + fingerprint[:32], nil
}

func directCapabilityFingerprint(profile EmbeddingProfile) string {
	if profile.Mode != EmbeddingModeDirectFile {
		return ""
	}
	fingerprint, _ := profile.Policy.Fingerprint(profile.CapabilityManifest)
	return fingerprint
}

func readEmbeddingBody(ctx context.Context, reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("voyage embedding: response read canceled: %w", contextErr)
		}
		return nil, errors.New("voyage embedding: could not read provider response")
	}
	if int64(len(body)) > maximum {
		return nil, errors.New("voyage embedding: provider response byte limit exceeded")
	}
	return body, nil
}

func validateEmbeddingContentType(value string) error {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" || (len(parameters) != 0 && (len(parameters) != 1 || !strings.EqualFold(parameters["charset"], "utf-8"))) {
		return errors.New("voyage embedding: provider response content type is invalid")
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

func validEmbeddingToken(value string) bool {
	return value != "" && len(value) <= 128 && value == strings.TrimSpace(value) && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func cloneEmbeddingDescriptor(value document.EmbeddingDescriptor) document.EmbeddingDescriptor {
	value.InputKinds = slices.Clone(value.InputKinds)
	value.SupportedRequestModes = slices.Clone(value.SupportedRequestModes)
	return value
}

func nilEmbeddingValue(value any) bool {
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
