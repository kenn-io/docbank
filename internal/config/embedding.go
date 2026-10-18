package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"go.kenn.io/kit/embedconfig"
)

// Kit's embedder accepts these, but the pinned adapter has no request field
// for them; use model_input for document/query formatting.
var unsupportedEmbedderKeys = []string{"document_prefix", "document_suffix", "query_prefix", "query_suffix", "request_dimensions"}

func unsupportedEmbedderKey(md toml.MetaData) (string, bool) {
	for _, key := range md.Keys() {
		if len(key) == 4 && key[0] == "embedding_profiles" && key[2] == "embedder" && slices.Contains(unsupportedEmbedderKeys, key[3]) {
			return key.String(), true
		}
	}
	return "", false
}

// EmbeddingProfile resolves the preferred Kit configuration into the existing
// immutable binding. It never reads credentials or modifies the source config.
func (c Config) EmbeddingProfile(name string) (EmbeddingProfileConfig, error) {
	profile, ok := c.EmbeddingProfiles[name]
	if !ok {
		return profile, fmt.Errorf("embedding profile %q is not defined", name)
	}
	if profile.Embedder == nil {
		return profile, nil
	}
	if err := resolveEmbedder(&profile); err != nil {
		return EmbeddingProfileConfig{}, fmt.Errorf("[embedding_profiles.%s.embedder]: %w", name, err)
	}
	if binding, exists := c.CredentialBindings[strings.TrimPrefix(profile.CredentialBinding, "credential:")]; exists && !profile.Embedder.APIKey.IsZero() {
		key := profile.Embedder.APIKey
		if key.Env != binding.EnvironmentVariable || key.File != "" || key.Value != "" {
			return EmbeddingProfileConfig{}, fmt.Errorf("[embedding_profiles.%s.embedder] api_key conflicts with the legacy credential binding", name)
		}
	}
	for otherName, other := range c.EmbeddingProfiles {
		if otherName != name && other.CredentialBinding == profile.CredentialBinding && other.Embedder != nil &&
			!other.Embedder.APIKey.IsZero() && !profile.Embedder.APIKey.IsZero() && other.Embedder.APIKey != profile.Embedder.APIKey {
			return EmbeddingProfileConfig{}, fmt.Errorf("[embedding_profiles.%s.embedder] api_key conflicts with another profile using the same credential binding", name)
		}
	}
	return profile, nil
}

func resolveEmbedder(profile *EmbeddingProfileConfig) error {
	e := profile.Embedder
	if err := validateEmbedderOptions(e); err != nil {
		return err
	}
	if err := e.Validate(); err != nil {
		return fmt.Errorf("invalid Kit configuration: %w", err)
	}
	if !e.Enabled() || profile.Runtime == nil {
		return errors.New("base_url, model, dims and a runtime authority are required")
	}
	runtime := *profile.Runtime
	profile.Runtime = &runtime
	if runtime.AdapterContract == "" {
		runtime.AdapterContract = "docbank-openai-compatible-embeddings/v1"
	}
	if runtime.AdapterContract != "docbank-openai-compatible-embeddings/v1" {
		return errors.New("embedder is only supported for OpenAI-compatible text runtimes")
	}
	parts, err := e.Parts()
	if err != nil {
		return fmt.Errorf("prepare Kit configuration: %w", err)
	}
	// These require different wire roles or token packing. Neither is part of
	// the existing pinned adapter/input contract; never silently ignore them.
	if parts.Roles.InputType != embedconfig.InputTypeNone {
		return errors.New("input_type_mode must be none; use model_input for document/query formatting")
	}
	if e.ModelContextTokens != 0 || e.MaxBatchTokens != 0 {
		return errors.New("token batching is not supported by the pinned adapter; use the existing chunk and max_input_tokens policy")
	}
	// Keep the configured authority spelling, including an explicit default
	// port. Kit's canonical URL drops that port, but DocBank's stored policy
	// fingerprints include it. Validate above still enforces Kit admission.
	endpoint, err := url.Parse(strings.TrimRight(strings.TrimSpace(e.BaseURL), "/"))
	if err != nil {
		return fmt.Errorf("parse base_url: %w", err)
	}
	if endpoint.EscapedPath() != "/v1" && endpoint.EscapedPath() != "/v1/embeddings" {
		return errors.New("base_url must end in /v1 or /v1/embeddings for this adapter")
	}
	origin := endpoint.Scheme + "://" + endpoint.Host
	if strings.TrimSuffix(runtime.Endpoint, "/") == origin {
		origin = runtime.Endpoint
	}
	for _, err := range []error{
		mergeEmbeddingSetting(&profile.Model, parts.Model.Name, "model"),
		mergeEmbeddingSetting(&profile.Dimensions, parts.Model.Dimensions, "dims/dimensions"),
		mergeEmbeddingSetting(&profile.MaxBatchItems, parts.Batch.Items, "batch_size/max_batch_items"),
		mergeEmbeddingSetting(&runtime.Endpoint, origin, "base_url/endpoint"),
		mergeEmbeddingSetting(&runtime.RequestTimeout, Duration(parts.Transport.Timeout), "timeout_seconds/request_timeout"),
	} {
		if err != nil {
			return err
		}
	}
	if e.FingerprintSalt != "" {
		if err := mergeEmbeddingSetting(&runtime.ModelRevision, parts.Model.Revision, "fingerprint_salt/model_revision"); err != nil {
			return err
		}
	}
	if runtime.DeploymentEpoch == "" && runtime.ProviderRevisionHeader == "" {
		runtime.DeploymentEpoch = runtime.ModelRevision
	}
	return nil
}

// Docbank pins formatting in model_input and does not use Kit's request builder.
func validateEmbedderOptions(e *embedconfig.Embedder) error {
	for _, setting := range []struct{ name, value string }{
		{"document_prefix", e.DocumentPrefix}, {"document_suffix", e.DocumentSuffix},
		{"query_prefix", e.QueryPrefix}, {"query_suffix", e.QuerySuffix},
	} {
		if setting.value != "" {
			return fmt.Errorf("unsupported embedder setting %q; use model_input for document/query formatting", setting.name)
		}
	}
	if e.RequestDimensions {
		return errors.New("unsupported embedder setting request_dimensions; dims only declares the expected response width")
	}
	return nil
}

func mergeEmbeddingSetting[T comparable](legacy *T, shared T, field string) error {
	var zero T
	if *legacy != zero && *legacy != shared {
		return fmt.Errorf("%s conflicts with the legacy setting", field)
	}
	*legacy = shared
	return nil
}
