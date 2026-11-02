package embedconfig

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kit/secretref"
)

// Embedder is the standard configuration-file shape for one embedding
// endpoint. Applications decode it wherever they keep an embedder, such as a
// [search.embeddings] table, so every application reads the same keys the
// same way. An application with several embedders uses one Embedder each.
//
// The zero value is disabled. BaseURL, Model, and Dims are set together.
type Embedder struct {
	// BaseURL is the OpenAI-compatible endpoint base. The client appends
	// /embeddings unless the path already ends with it.
	BaseURL string `toml:"base_url"`
	// Model is the provider model name.
	Model string `toml:"model"`
	// Dims is the vector width the provider returns.
	Dims int `toml:"dims"`
	// RequestDimensions sends Dims as the dimensions field on each request.
	// Set it only when the endpoint supports requesting that width. False
	// leaves the provider default; Dims still validates the returned width.
	// The client never truncates vectors to fit Dims.
	RequestDimensions bool `toml:"request_dimensions"`
	// APIKey is the bearer token: the token itself as a string, or a table
	// naming its source, such as { env = "NAME" } or { file = "PATH" }.
	// Leave it unset for an endpoint that needs no authentication. The
	// sensitive tag marks it for redaction by applications that display
	// configuration.
	APIKey secretref.Ref `toml:"api_key" sensitive:"true"`
	// FingerprintSalt marks a different vector space for the same model name,
	// such as retrained weights. Changing it starts a new generation.
	FingerprintSalt string `toml:"fingerprint_salt"`
	// InputTypeMode is "none" (the default) or "retrieval". Retrieval sends
	// input_type document or query on each request.
	InputTypeMode string `toml:"input_type_mode"`
	// DocumentPrefix, DocumentSuffix, QueryPrefix, and QuerySuffix are
	// literal role affixes. Whitespace, including trailing spaces, is kept.
	// They change vector identity and are applied to raw text by EmbedTexts
	// and Embed; EncodeFunc expects text that is already formatted.
	DocumentPrefix string `toml:"document_prefix"`
	DocumentSuffix string `toml:"document_suffix"`
	QueryPrefix    string `toml:"query_prefix"`
	QuerySuffix    string `toml:"query_suffix"`
	// BatchSize caps inputs per request. Zero uses DefaultBatchItems.
	BatchSize int `toml:"batch_size"`
	// ModelContextTokens is the most tokens one input can hold, and
	// MaxBatchTokens is the provider's aggregate input-token cap per request.
	// Set both to batch by tokens, or leave both zero to batch by count.
	ModelContextTokens int `toml:"model_context_tokens"`
	MaxBatchTokens     int `toml:"max_batch_tokens"`
	// TimeoutSeconds is the per-request timeout. Zero uses DefaultTimeout.
	TimeoutSeconds int `toml:"timeout_seconds"`
	// TrustPrivateNetwork allows plaintext HTTP to a private-network endpoint.
	TrustPrivateNetwork bool `toml:"trust_private_network"`
}

// Parts are the kit settings an Embedder describes. The model is cosine and
// L2-normalized, the only storable vector space. Deployment.PinEndpoint is
// left false; an application that keys vectors by endpoint sets it.
type Parts struct {
	Model      Model
	Roles      Roles
	Deployment Deployment
	Batch      Batch
	Transport  Transport
}

// Enabled reports whether any endpoint setting is present. Validate rejects a
// partial configuration.
func (e Embedder) Enabled() bool {
	return strings.TrimSpace(e.BaseURL) != "" || strings.TrimSpace(e.Model) != "" || e.Dims != 0
}

// Validate checks the configuration without resolving secrets. A disabled
// Embedder is valid only when no other key is set.
func (e Embedder) Validate() error {
	if !e.Enabled() {
		if e != (Embedder{}) {
			return errors.New("embed base_url, model, and dims are required with other embedder settings")
		}
		return nil
	}
	if strings.TrimSpace(e.BaseURL) == "" || strings.TrimSpace(e.Model) == "" || e.Dims <= 0 {
		return errors.New("embed base_url, model, and positive dims must be configured together")
	}
	if err := e.APIKey.Validate(); err != nil {
		return fmt.Errorf("embed api_key: %w", err)
	}
	if e.BatchSize < 0 || e.ModelContextTokens < 0 || e.MaxBatchTokens < 0 || e.TimeoutSeconds < 0 {
		return errors.New("embed batch_size, model_context_tokens, max_batch_tokens, and timeout_seconds must not be negative")
	}
	_, err := e.Parts()
	return err
}

// Parts converts the configuration into validated kit settings. It fills
// only operational defaults: batch size and timeout.
func (e Embedder) Parts() (Parts, error) {
	model, err := Model{
		Name:              e.Model,
		Revision:          e.FingerprintSalt,
		Dimensions:        e.Dims,
		Metric:            MetricCosine,
		Normalization:     NormalizationL2,
		RequestDimensions: e.RequestDimensions,
	}.Prepared()
	if err != nil {
		return Parts{}, err
	}
	roles, err := Roles{
		InputType:      InputType(strings.TrimSpace(e.InputTypeMode)),
		DocumentPrefix: e.DocumentPrefix,
		DocumentSuffix: e.DocumentSuffix,
		QueryPrefix:    e.QueryPrefix,
		QuerySuffix:    e.QuerySuffix,
	}.Prepared()
	if err != nil {
		return Parts{}, err
	}
	deployment, err := Deployment{BaseURL: e.BaseURL, TrustPrivateNetwork: e.TrustPrivateNetwork}.Prepared()
	if err != nil {
		return Parts{}, err
	}
	batch, err := Batch{
		Items: e.BatchSize, MaxTokens: e.MaxBatchTokens, InputTokenUpperBound: e.ModelContextTokens,
	}.Prepared()
	if err != nil {
		return Parts{}, err
	}
	transport, err := Transport{Timeout: time.Duration(e.TimeoutSeconds) * time.Second}.Prepared()
	if err != nil {
		return Parts{}, err
	}
	return Parts{Model: model, Roles: roles, Deployment: deployment, Batch: batch, Transport: transport}, nil
}

// ResolveAPIKey reads the API key. An unset key resolves to an empty Secret.
// A configured environment or file source that cannot provide a key
// returns an error.
func (e Embedder) ResolveAPIKey() (secretref.Secret, error) {
	secret, err := e.APIKey.Resolve()
	if err != nil {
		return secretref.Secret{}, fmt.Errorf("embed api_key: %w", err)
	}
	return secret, nil
}
