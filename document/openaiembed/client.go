// Package openaiembed implements one bounded OpenAI-compatible text embedding
// endpoint for operator-controlled local deployments.
package openaiembed

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
	// ProviderID is the fixed provider-neutral adapter identity.
	ProviderID = "openai-compatible.embeddings-v1"
	// DocumentFormatterV1 identifies the adapter's document formatting path.
	DocumentFormatterV1 = "openai-compatible/document/v1"
	// QueryFormatterV1 identifies the adapter's query formatting path.
	QueryFormatterV1 = "openai-compatible/query/v1"
	// ScalarEncodingFloat32 is the only scalar representation returned by the adapter.
	ScalarEncodingFloat32 = "float32"

	embeddingsPath  = "/v1/embeddings"
	adapterContract = "docbank-openai-compatible-embeddings/v1"

	defaultRequestTimeout   = 30 * time.Second
	defaultMaxBatchItems    = 128
	defaultMaxInputBytes    = int64(1 << 20)
	defaultMaxRequestBytes  = int64(2 << 20)
	defaultMaxResponseBytes = int64(32 << 20)

	maxRequestTimeout   = 5 * time.Minute
	maxBatchItems       = 10_000
	maxInputBytes       = int64(1 << 30)
	maxRequestBytes     = int64(1 << 30)
	maxResponseBytes    = int64(1 << 30)
	maxSecretBytes      = 64 << 10
	maxIdentityBytes    = 128
	unitLengthTolerance = 1e-4
)

var _ document.EmbeddingProvider = (*Client)(nil)

// SecretResolver resolves only the optional credential binding named by a
// profile. Secret values never enter profile or vector-space identity.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, name string) (string, error)
}

// Profile freezes one OpenAI-compatible endpoint, explicit model-input
// contract, immutable deployment identity, and all transport capacity bounds.
// Exactly one of DeploymentEpoch and ProviderRevisionHeader is required.
type Profile struct {
	Origin                 string
	Descriptor             document.EmbeddingDescriptor
	ModelInput             document.ModelInputContract
	SecretBinding          string
	DeploymentEpoch        string
	ProviderRevisionHeader string
	RequestTimeout         time.Duration
	MaxBatchItems          int
	MaxInputBytes          int64
	MaxRequestBytes        int64
	MaxResponseBytes       int64
}

type policyIdentity struct {
	AdapterContract        string                       `json:"adapter_contract"`
	Origin                 string                       `json:"origin"`
	Route                  string                       `json:"route"`
	Descriptor             document.EmbeddingDescriptor `json:"descriptor"`
	ModelInput             document.ModelInputContract  `json:"model_input"`
	CredentialBinding      string                       `json:"credential_binding"`
	DeploymentEpoch        string                       `json:"deployment_epoch,omitempty"`
	ProviderRevisionHeader string                       `json:"provider_revision_header,omitempty"`
	RequestTimeoutNanos    int64                        `json:"request_timeout_nanos"`
	MaxBatchItems          int                          `json:"max_batch_items"`
	MaxInputBytes          int64                        `json:"max_input_bytes"`
	MaxRequestBytes        int64                        `json:"max_request_bytes"`
	MaxResponseBytes       int64                        `json:"max_response_bytes"`
}

// Client calls exactly POST /v1/embeddings on the profile origin.
type Client struct {
	profile    Profile
	descriptor document.EmbeddingDescriptor
	secrets    SecretResolver
	http       *http.Client
}

type wireRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	EncodingFormat string   `json:"encoding_format"`
}

type wireResponse struct {
	Object string          `json:"object"`
	Data   []wireEmbedding `json:"data"`
	Model  string          `json:"model"`
	Usage  *wireUsage      `json:"usage,omitempty"`
}

type wireEmbedding struct {
	Object    string    `json:"object"`
	Embedding []float32 `json:"embedding"`
	Index     *int      `json:"index"`
}

type wireUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// PolicyFingerprint returns the canonical profile identity expected in the
// embedding descriptor. Credential values and HTTP client state are excluded.
func PolicyFingerprint(profile Profile) (string, error) {
	normalized, descriptorIdentity, err := normalizeProfile(profile)
	if err != nil {
		return "", err
	}
	identity := policyIdentity{
		AdapterContract: adapterContract, Origin: normalized.Origin, Route: embeddingsPath,
		Descriptor: descriptorIdentity, ModelInput: normalized.ModelInput,
		CredentialBinding: normalized.SecretBinding, DeploymentEpoch: normalized.DeploymentEpoch,
		ProviderRevisionHeader: normalized.ProviderRevisionHeader,
		RequestTimeoutNanos:    int64(normalized.RequestTimeout), MaxBatchItems: normalized.MaxBatchItems,
		MaxInputBytes: normalized.MaxInputBytes, MaxRequestBytes: normalized.MaxRequestBytes,
		MaxResponseBytes: normalized.MaxResponseBytes,
	}
	encoded, err := json.Marshal(identity, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("openaiembed: encode policy identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// New validates an immutable profile and isolates the supplied HTTP client
// from ambient cookies, timeouts, and redirect behavior. It performs no I/O.
func New(profile Profile, secrets SecretResolver, httpClient *http.Client) (*Client, error) {
	normalized, _, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		if err == nil {
			err = errors.New("descriptor is not canonical")
		}
		return nil, fmt.Errorf("openaiembed: invalid descriptor: %w", err)
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("openaiembed: descriptor policy fingerprint does not match profile")
	}
	if normalized.SecretBinding == "" {
		if !nilValue(secrets) {
			return nil, errors.New("openaiembed: secret resolver requires a named binding")
		}
	} else if nilValue(secrets) {
		return nil, errors.New("openaiembed: named secret binding requires a resolver")
	}
	if httpClient == nil {
		return nil, errors.New("openaiembed: HTTP client is required")
	}
	isolate := *httpClient
	isolate.CheckRedirect = providerhttp.RefuseRedirects
	isolate.Jar = nil
	isolate.Timeout = 0
	normalized.Descriptor = cloneDescriptor(descriptor)
	return &Client{profile: normalized, descriptor: cloneDescriptor(descriptor), secrets: secrets, http: &isolate}, nil
}

// Descriptor returns a defensive copy of the immutable provider contract.
func (client *Client) Descriptor() document.EmbeddingDescriptor {
	if client == nil {
		return document.EmbeddingDescriptor{}
	}
	return cloneDescriptor(client.descriptor)
}

// Embed validates and formats one text batch, performs one bounded request,
// and restores caller ordering from the response indices.
func (client *Client) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if client == nil {
		return document.EmbeddingResult{}, errors.New("openaiembed: client is required")
	}
	if ctx == nil {
		return document.EmbeddingResult{}, errors.New("openaiembed: context is required")
	}
	if err := document.ValidateEmbeddingProviderRequest(client, inputs, authorization); err != nil {
		return document.EmbeddingResult{}, err
	}
	if authorization.MaxBatchItems > client.profile.MaxBatchItems ||
		authorization.MaxInputBytes > client.profile.MaxInputBytes ||
		authorization.MaxResponseBytes > client.profile.MaxResponseBytes {
		return document.EmbeddingResult{}, errors.New("openaiembed: embedding authorization exceeds profile capacity")
	}
	if err := ctx.Err(); err != nil {
		return document.EmbeddingResult{}, fmt.Errorf("openaiembed: embedding canceled: %w", err)
	}

	rendered := make([]string, len(inputs))
	var renderedBytes int64
	for index, input := range inputs {
		switch {
		case input.Role == document.EmbeddingRoleDocument && input.Kind == document.EmbeddingInputRenditionChunk:
			rendered[index] = client.profile.ModelInput.EncodeDocument(input.Text)
		case input.Role == document.EmbeddingRoleQuery && input.Kind == document.EmbeddingInputQueryText:
			rendered[index] = client.profile.ModelInput.EncodeQuery(input.Text)
		default:
			return document.EmbeddingResult{}, errors.New("openaiembed: unsupported non-text embedding input or role")
		}
		if int64(len(rendered[index])) > client.profile.MaxInputBytes-renderedBytes {
			return document.EmbeddingResult{}, errors.New("openaiembed: embedding input exceeds profile byte capacity")
		}
		renderedBytes += int64(len(rendered[index]))
	}
	payload, err := json.Marshal(wireRequest{Input: rendered, Model: client.descriptor.Model, EncodingFormat: "float"})
	if err != nil {
		return document.EmbeddingResult{}, errors.New("openaiembed: could not encode embedding request")
	}
	if int64(len(payload)) > client.profile.MaxRequestBytes {
		return document.EmbeddingResult{}, errors.New("openaiembed: embedding request byte limit exceeded")
	}

	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, client.profile.Origin+embeddingsPath, bytes.NewReader(payload))
	if err != nil {
		return document.EmbeddingResult{}, errors.New("openaiembed: could not construct embedding request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if client.profile.SecretBinding != "" {
		secret, resolveErr := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
		if resolveErr != nil {
			if contextErr := requestCtx.Err(); contextErr != nil {
				return document.EmbeddingResult{}, fmt.Errorf("openaiembed: credential resolution canceled: %w", contextErr)
			}
			return document.EmbeddingResult{}, errors.New("openaiembed: could not resolve credential")
		}
		if !validSecret(secret) {
			return document.EmbeddingResult{}, errors.New("openaiembed: resolved credential is invalid")
		}
		request.Header.Set("Authorization", "Bearer "+secret)
	}

	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, fmt.Errorf("openaiembed: embedding request canceled: %w", contextErr)
		}
		return document.EmbeddingResult{}, errors.New("openaiembed: provider request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return document.EmbeddingResult{}, fmt.Errorf("openaiembed: provider redirect refused with HTTP status %d", response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return document.EmbeddingResult{}, fmt.Errorf("openaiembed: provider returned HTTP status %d", response.StatusCode)
	}
	if err := validateResponseContentType(response.Header.Get("Content-Type")); err != nil {
		return document.EmbeddingResult{}, err
	}
	if err := client.validateRevisionEcho(response.Header); err != nil {
		return document.EmbeddingResult{}, err
	}
	body, err := readBounded(requestCtx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		return document.EmbeddingResult{}, err
	}
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return document.EmbeddingResult{}, errors.New("openaiembed: provider response does not match the bounded embedding schema")
	}
	result, err := client.validateAndOrder(decoded, inputs)
	if err != nil {
		return document.EmbeddingResult{}, err
	}
	if err := document.ValidateEmbeddingProviderResult(client.descriptor, inputs, authorization, result); err != nil {
		return document.EmbeddingResult{}, err
	}
	return result, nil
}

func (client *Client) validateRevisionEcho(header http.Header) error {
	if client.profile.ProviderRevisionHeader == "" {
		return nil
	}
	values := header.Values(client.profile.ProviderRevisionHeader)
	if len(values) != 1 || strings.TrimSpace(values[0]) != client.descriptor.ModelRevision {
		return errors.New("openaiembed: provider revision echo does not match profile")
	}
	return nil
}

func (client *Client) validateAndOrder(response wireResponse, inputs []document.EmbeddingInput) (document.EmbeddingResult, error) {
	if response.Object != "list" || response.Model != client.descriptor.Model {
		return document.EmbeddingResult{}, errors.New("openaiembed: provider model or response contract drifted")
	}
	if response.Usage != nil && (response.Usage.PromptTokens < 0 || response.Usage.TotalTokens < response.Usage.PromptTokens) {
		return document.EmbeddingResult{}, errors.New("openaiembed: provider usage is invalid")
	}
	if len(response.Data) != len(inputs) {
		return document.EmbeddingResult{}, errors.New("openaiembed: provider response has a missing vector")
	}
	vectors := make([]document.EmbeddingVector, len(inputs))
	seen := make([]bool, len(inputs))
	for _, item := range response.Data {
		if item.Object != "embedding" || item.Index == nil {
			return document.EmbeddingResult{}, errors.New("openaiembed: provider response item contract drifted")
		}
		index := *item.Index
		if index < 0 || index >= len(inputs) {
			return document.EmbeddingResult{}, errors.New("openaiembed: provider response index is outside request bounds")
		}
		if seen[index] {
			return document.EmbeddingResult{}, errors.New("openaiembed: provider response has a duplicate vector index")
		}
		seen[index] = true
		if err := client.validateVector(item.Embedding); err != nil {
			return document.EmbeddingResult{}, err
		}
		vectors[index] = document.EmbeddingVector{Key: inputs[index].Key, Values: slices.Clone(item.Embedding)}
	}
	if slices.Contains(seen, false) {
		return document.EmbeddingResult{}, errors.New("openaiembed: provider response has a missing vector index")
	}
	return document.EmbeddingResult{Vectors: vectors}, nil
}

func (client *Client) validateVector(vector []float32) error {
	if len(vector) != client.descriptor.Dimension {
		return errors.New("openaiembed: provider vector dimension does not match profile")
	}
	var squaredNorm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return errors.New("openaiembed: provider vector contains a non-finite value")
		}
		squaredNorm += float64(value) * float64(value)
	}
	if squaredNorm == 0 {
		return errors.New("openaiembed: provider returned a zero vector")
	}
	if client.descriptor.Normalization == document.VectorNormalizationUnitLength && math.Abs(squaredNorm-1) > unitLengthTolerance {
		return errors.New("openaiembed: provider vector normalization does not match profile")
	}
	return nil
}

func normalizeProfile(profile Profile) (Profile, document.EmbeddingDescriptor, error) {
	origin, err := validateOrigin(profile.Origin)
	if err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	profile.Origin = origin
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
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("openaiembed: execution bounds are invalid")
	}
	if profile.SecretBinding != "" && !validIdentityToken(profile.SecretBinding) {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("openaiembed: secret binding is invalid")
	}

	descriptorIdentity := profile.Descriptor
	descriptorIdentity.PolicyFingerprint = strings.Repeat("0", sha256.Size*2)
	descriptorIdentity.Fingerprint = ""
	descriptorIdentity, err = document.NewEmbeddingDescriptor(descriptorIdentity)
	if err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, fmt.Errorf("openaiembed: invalid descriptor identity: %w", err)
	}
	descriptorIdentity.PolicyFingerprint = ""
	descriptorIdentity.Fingerprint = ""
	if err := validateDescriptorContract(descriptorIdentity, profile.ModelInput); err != nil {
		return Profile{}, document.EmbeddingDescriptor{}, err
	}
	if (profile.DeploymentEpoch == "") == (profile.ProviderRevisionHeader == "") {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("openaiembed: exactly one deployment epoch or provider revision header is required")
	}
	if profile.DeploymentEpoch != "" {
		if !validIdentityToken(profile.DeploymentEpoch) || profile.DeploymentEpoch != descriptorIdentity.ModelRevision {
			return Profile{}, document.EmbeddingDescriptor{}, errors.New("openaiembed: deployment epoch must exactly match descriptor model revision")
		}
	} else if !validRevisionHeader(profile.ProviderRevisionHeader) {
		return Profile{}, document.EmbeddingDescriptor{}, errors.New("openaiembed: provider revision header is not a canonical safe response header")
	}
	return profile, descriptorIdentity, nil
}

func validateDescriptorContract(descriptor document.EmbeddingDescriptor, modelInput document.ModelInputContract) error {
	if descriptor.ID != ProviderID || descriptor.TrustBoundary != document.EmbeddingTrustOperatorNetwork ||
		descriptor.ScalarEncoding != ScalarEncodingFloat32 || descriptor.DocumentFormatter != DocumentFormatterV1 ||
		descriptor.QueryFormatter != QueryFormatterV1 || !descriptor.SupportsTextQuery ||
		!slices.Equal(descriptor.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}) ||
		!slices.Equal(descriptor.SupportedRequestModes, []document.ModelInputMode{document.ModelInputModeText}) {
		return errors.New("openaiembed: descriptor does not match the text-only adapter contract")
	}
	if descriptor.ModelInput != modelInput || descriptor.CompatibilityID != modelInput.CompatibilityID {
		return errors.New("openaiembed: descriptor and explicit model-input contract differ")
	}
	switch modelInput.Profile {
	case document.ModelInputProfileNomic, document.ModelInputProfileE5,
		document.ModelInputProfileBGEM3, document.ModelInputProfileGTE,
		document.ModelInputProfileQwen3, document.ModelInputProfileQueryInstruction,
		document.ModelInputProfileCustom:
	default:
		return errors.New("openaiembed: model-input contract must use a reviewed embedding family or complete custom profile")
	}
	return nil
}

func validateOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Opaque != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("openaiembed: origin must be one HTTP(S) origin without credentials, non-root path, query, or fragment")
	}
	hostname := parsed.Hostname()
	if hostname == "" || !asciiHost(hostname) || strings.ToLower(hostname) != hostname {
		return "", errors.New("openaiembed: origin host is not canonical ASCII")
	}
	port := parsed.Port()
	if port != "" {
		value, parseErr := strconv.ParseUint(port, 10, 16)
		if parseErr != nil || value == 0 {
			return "", errors.New("openaiembed: origin port is invalid")
		}
	}
	authority := hostname
	if strings.Contains(hostname, ":") {
		authority = "[" + hostname + "]"
	}
	if port != "" {
		authority = net.JoinHostPort(hostname, port)
	}
	return parsed.Scheme + "://" + authority, nil
}

func asciiHost(host string) bool {
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Zone() == "" && address.String() == host
	}
	if len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func validIdentityToken(value string) bool {
	return value != "" && len(value) <= maxIdentityBytes && value == strings.TrimSpace(value) && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validRevisionHeader(name string) bool {
	if name == "" || len(name) > maxIdentityBytes || http.CanonicalHeaderKey(name) != name {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	switch name {
	case "Authorization", "Connection", "Content-Length", "Content-Type", "Cookie", "Date", "Location", "Server", "Set-Cookie", "Transfer-Encoding":
		return false
	default:
		return true
	}
}

func validSecret(secret string) bool {
	if secret == "" || len(secret) > maxSecretBytes {
		return false
	}
	padding := false
	content := 0
	for index := range len(secret) {
		character := secret[index]
		if character == '=' {
			padding = true
			continue
		}
		if padding || !validBearerCharacter(character) {
			return false
		}
		content++
	}
	return content > 0
}

func validBearerCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' || strings.ContainsRune("-._~+/", rune(character))
}

func validateResponseContentType(value string) error {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" {
		return errors.New("openaiembed: provider response content type is not application/json")
	}
	if len(parameters) == 0 {
		return nil
	}
	charset, ok := parameters["charset"]
	if len(parameters) != 1 || !ok || !strings.EqualFold(charset, "utf-8") {
		return errors.New("openaiembed: provider response content type has unsupported parameters")
	}
	return nil
}

func readBounded(ctx context.Context, reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("openaiembed: response read canceled: %w", contextErr)
		}
		return nil, errors.New("openaiembed: could not read provider response")
	}
	if int64(len(body)) > maximum {
		return nil, errors.New("openaiembed: provider response byte limit exceeded")
	}
	return body, nil
}

func cloneDescriptor(descriptor document.EmbeddingDescriptor) document.EmbeddingDescriptor {
	descriptor.InputKinds = slices.Clone(descriptor.InputKinds)
	descriptor.SupportedRequestModes = slices.Clone(descriptor.SupportedRequestModes)
	return descriptor
}

func nilValue(value any) bool {
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
