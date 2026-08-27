package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/openaiembed"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/upload"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

const (
	openAIEmbeddingAdapter = "docbank-openai-compatible-embeddings/v1"
	voyageEmbeddingAdapter = "docbank-voyage-embeddings/v1"
)

func recoverEmbeddingRuntimeSpool(ctx context.Context, spoolDirectory string) error {
	_, err := upload.RecoverStale(ctx, spoolDirectory)
	return err
}

type environmentEmbeddingSecrets struct{ variables map[string]string }

func (resolver environmentEmbeddingSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	variable, ok := resolver.variables[name]
	if !ok {
		return "", errors.New("embedding credential binding is unavailable")
	}
	value, ok := os.LookupEnv(variable)
	if !ok || value == "" {
		return "", errors.New("embedding credential environment variable is unavailable")
	}
	return value, nil
}

func configureEmbeddingRuntimes(cfg config.Config, blobs embeddingRuntimeBlobStore,
	spoolDirectory string,
) (*processing.EmbeddingRuntimeRegistry, error) {
	registry := processing.NewEmbeddingRuntimeRegistry()
	secrets := environmentEmbeddingSecrets{variables: make(map[string]string)}
	for name, binding := range cfg.CredentialBindings {
		portable := "credential:" + name
		secrets.variables[portable] = binding.EnvironmentVariable
	}
	for name, configured := range cfg.EmbeddingProfiles {
		if configured.Runtime == nil {
			continue
		}
		variable, ok := secrets.variables[configured.CredentialBinding]
		if !ok {
			return nil, fmt.Errorf("embedding credential %q is not configured", configured.CredentialBinding)
		}
		if value, ok := os.LookupEnv(variable); !ok || value == "" {
			return nil, fmt.Errorf("embedding credential %q environment variable is missing", configured.CredentialBinding)
		}
		modelInput, err := cfg.EmbeddingModelInput(name)
		if err != nil {
			return nil, err
		}
		descriptor := configuredEmbeddingDescriptor(configured, modelInput)
		var provider document.EmbeddingProvider
		var classify func(error) (processing.EmbeddingProviderFailure, time.Duration)
		switch configured.Runtime.AdapterContract {
		case openAIEmbeddingAdapter:
			profile := openaiembed.Profile{Origin: configured.Runtime.Endpoint, Descriptor: descriptor,
				ModelInput: modelInput, SecretBinding: configured.CredentialBinding,
				DeploymentEpoch:        configured.Runtime.DeploymentEpoch,
				ProviderRevisionHeader: configured.Runtime.ProviderRevisionHeader,
				RequestTimeout:         configured.Runtime.RequestTimeout.Std(), MaxBatchItems: configured.MaxBatchItems,
				MaxInputBytes: configured.MaxInputBytes, MaxRequestBytes: configured.Runtime.MaxRequestBytes,
				MaxResponseBytes: configured.MaxResponseBytes,
				EgressPolicy:     configuredEmbeddingEgress(*configured.Runtime)}
			descriptor, profile, err = finalizeOpenAIEmbeddingDescriptor(profile)
			if err == nil {
				transport, transportErr := providerhttp.NewTransport(configuredEmbeddingEgress(*configured.Runtime), nil)
				if transportErr != nil {
					err = transportErr
				} else {
					profile.Descriptor = descriptor
					provider, err = openaiembed.New(profile, secrets, &http.Client{Transport: transport})
				}
			}
			classify = classifyOpenAIEmbeddingError
		case voyageEmbeddingAdapter:
			provider, descriptor, err = configuredVoyageProvider(configured, modelInput, secrets)
			classify = classifyVoyageEmbeddingError
		default:
			err = errors.New("unsupported embedding runtime adapter")
		}
		if err != nil {
			return nil, fmt.Errorf("configuring embedding runtime %q: %w", name, err)
		}
		if descriptor.Fingerprint != configured.DescriptorFingerprint ||
			descriptor.ID != configured.DescriptorID || descriptor.ModelRevision != configured.Runtime.ModelRevision {
			return nil, fmt.Errorf("configuring embedding runtime %q: descriptor differs from portable binding", name)
		}
		runtime, err := processing.NewProviderEmbeddingRuntime(provider, blobs, spoolDirectory, classify)
		if err != nil {
			return nil, err
		}
		if err := registry.Register(descriptor.Fingerprint, runtime); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

type embeddingRuntimeBlobStore interface {
	OpenContext(ctx context.Context, hash string) (io.ReadSeekCloser, error)
}

func configuredEmbeddingDescriptor(profile config.EmbeddingProfileConfig, modelInput document.ModelInputContract) document.EmbeddingDescriptor {
	runtime := profile.Runtime
	modes := []document.ModelInputMode{modelInput.Document.Mode}
	supportsQuery := runtime.AdapterContract == openAIEmbeddingAdapter
	if supportsQuery && modelInput.Query.Mode != modelInput.Document.Mode {
		modes = append(modes, modelInput.Query.Mode)
	}
	return document.EmbeddingDescriptor{ID: profile.DescriptorID,
		ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: strings.Repeat("0", 64),
		TrustBoundary: document.EmbeddingTrustBoundary(profile.TrustBoundary), Model: profile.Model,
		ModelRevision: runtime.ModelRevision, Dimension: profile.Dimensions, Metric: profile.Metric,
		Normalization: profile.Normalization, ScalarEncoding: profile.ScalarEncoding,
		DocumentFormatter: profile.DocumentFormatter, QueryFormatter: profile.QueryFormatter,
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputKind(profile.InputKind)},
		CompatibilityID: profile.CompatibilityID, SupportsTextQuery: supportsQuery,
		ModelInput: modelInput, SupportedRequestModes: modes}
}

func finalizeOpenAIEmbeddingDescriptor(profile openaiembed.Profile) (document.EmbeddingDescriptor, openaiembed.Profile, error) {
	temporary, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil {
		return document.EmbeddingDescriptor{}, profile, err
	}
	profile.Descriptor = temporary
	fingerprint, err := openaiembed.PolicyFingerprint(profile)
	if err != nil {
		return document.EmbeddingDescriptor{}, profile, err
	}
	temporary.PolicyFingerprint, temporary.Fingerprint = fingerprint, ""
	final, err := document.NewEmbeddingDescriptor(temporary)
	return final, profile, err
}

func configuredVoyageProvider(profile config.EmbeddingProfileConfig, modelInput document.ModelInputContract,
	secrets environmentEmbeddingSecrets,
) (document.EmbeddingProvider, document.EmbeddingDescriptor, error) {
	file, err := os.Open(profile.Runtime.CapabilityManifest)
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, errors.New("voyage capability manifest is unavailable")
	}
	manifest, decodeErr := voyage.DecodeCapabilityManifest(file)
	closeErr := file.Close()
	if decodeErr != nil || closeErr != nil {
		return nil, document.EmbeddingDescriptor{}, errors.Join(decodeErr, closeErr)
	}
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: profile.Model, Dimension: profile.Dimensions,
		Media:         media.Policy{MaxBytes: profile.MaxInputBytes, AllowStill: true, AllowVideo: true},
		MaxBatchItems: profile.MaxBatchItems, MaxRequestBytes: profile.Runtime.MaxRequestBytes,
		MaxResponseBytes: profile.MaxResponseBytes})
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	descriptor := configuredEmbeddingDescriptor(profile, modelInput)
	configured := voyage.EmbeddingProfile{Mode: voyage.EmbeddingModeDirectFile,
		Endpoint: profile.Runtime.Endpoint, EgressPolicy: configuredEmbeddingEgress(*profile.Runtime),
		Descriptor: descriptor, ModelInput: modelInput, SecretBinding: profile.CredentialBinding,
		RequestTimeout: profile.Runtime.RequestTimeout.Std(), MaxRetries: 1,
		MaxBatchItems: profile.MaxBatchItems, MaxInputBytes: profile.MaxInputBytes,
		MaxRequestBytes: profile.Runtime.MaxRequestBytes, MaxResponseBytes: profile.MaxResponseBytes,
		Policy: policy, CapabilityManifest: manifest}
	temporary, err := document.NewEmbeddingDescriptor(descriptor)
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	configured.Descriptor = temporary
	fingerprint, err := voyage.EmbeddingPolicyFingerprint(configured)
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	temporary.PolicyFingerprint, temporary.Fingerprint = fingerprint, ""
	final, err := document.NewEmbeddingDescriptor(temporary)
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	configured.Descriptor = final
	provider, err := voyage.NewEmbeddingProvider(configured, secrets, nil)
	return provider, final, err
}

func configuredEmbeddingEgress(runtime config.EmbeddingRuntimeConfig) providerhttp.EgressPolicy {
	parsed, _ := url.Parse(runtime.Endpoint)
	port := uint16(443)
	if parsed.Scheme == "http" {
		port = 80
	}
	if parsed.Port() != "" {
		value, _ := strconv.ParseUint(parsed.Port(), 10, 16)
		port = uint16(value)
	}
	prefixes := make([]netip.Prefix, 0, len(runtime.AllowedCIDRs))
	for _, value := range runtime.AllowedCIDRs {
		prefix, _ := netip.ParsePrefix(value)
		prefixes = append(prefixes, prefix)
	}
	return providerhttp.EgressPolicy{Scheme: parsed.Scheme, Host: parsed.Hostname(), Port: port,
		AllowedCIDRs: prefixes, ProxyMode: providerhttp.ProxyDisabled,
		ConnectTimeout: runtime.ConnectTimeout.Std(), KeepAlive: runtime.KeepAlive.Std(),
		TLSHandshakeTimeout: runtime.TLSHandshakeTimeout.Std(),
		TLS:                 providerhttp.TLSPolicy{SPKISHA256: runtime.SPKISHA256}}
}

func classifyOpenAIEmbeddingError(err error) (processing.EmbeddingProviderFailure, time.Duration) {
	if errors.Is(err, openaiembed.ErrCapacityResponse) {
		return processing.EmbeddingProviderCapacity, 0
	}
	if errors.Is(err, openaiembed.ErrTransientResponse) {
		delay, _ := openaiembed.RetryAfter(err)
		return processing.EmbeddingProviderTransient, delay
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return processing.EmbeddingProviderTransient, 0
	}
	return processing.EmbeddingProviderPermanent, 0
}

func classifyVoyageEmbeddingError(err error) (processing.EmbeddingProviderFailure, time.Duration) {
	if errors.Is(err, voyage.ErrBatchTooLarge) {
		return processing.EmbeddingProviderCapacity, 0
	}
	if voyage.IsRetryable(err) || errors.Is(err, voyage.ErrMalformedResponse) {
		delay, _ := voyage.RetryAfter(err)
		return processing.EmbeddingProviderTransient, delay
	}
	return processing.EmbeddingProviderPermanent, 0
}
