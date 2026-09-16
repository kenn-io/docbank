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
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/openaicompat"
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

type environmentCredentialSecrets struct{ variables map[string]string }

type environmentEmbeddingSecrets = environmentCredentialSecrets

func (resolver environmentCredentialSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	variable, ok := resolver.variables[name]
	if !ok {
		return "", errors.New("credential binding is unavailable")
	}
	value, ok := os.LookupEnv(variable)
	if !ok || value == "" {
		return "", errors.New("credential environment variable is unavailable")
	}
	return value, nil
}

type embeddingRuntimeBundle struct {
	registry    *processing.EmbeddingRuntimeRegistry
	providers   map[string]document.EmbeddingProvider
	classifiers map[string]func(error) (processing.EmbeddingProviderFailure, time.Duration)
}

func configureEmbeddingRuntimeBundle(cfg config.Config, blobs embeddingRuntimeBlobStore,
	spoolDirectory string,
) (embeddingRuntimeBundle, error) {
	bundle := embeddingRuntimeBundle{registry: processing.NewEmbeddingRuntimeRegistry(),
		providers:   make(map[string]document.EmbeddingProvider),
		classifiers: make(map[string]func(error) (processing.EmbeddingProviderFailure, time.Duration))}
	registered := make(map[string]document.EmbeddingProvider)
	secrets := environmentCredentialSecrets{variables: make(map[string]string)}
	for name, binding := range cfg.CredentialBindings {
		portable := "credential:" + name
		secrets.variables[portable] = binding.EnvironmentVariable
		secrets.variables[name] = binding.EnvironmentVariable
	}
	for name, configured := range cfg.EmbeddingProfiles {
		if configured.Runtime == nil {
			continue
		}
		if _, ok := secrets.variables[configured.CredentialBinding]; !ok {
			return embeddingRuntimeBundle{}, errors.New("embedding credential binding is not configured")
		}
		modelInput, err := cfg.EmbeddingModelInput(name)
		if err != nil {
			return embeddingRuntimeBundle{}, err
		}
		descriptor := configuredEmbeddingDescriptor(configured, modelInput)
		var provider document.EmbeddingProvider
		var classify func(error) (processing.EmbeddingProviderFailure, time.Duration)
		switch configured.Runtime.AdapterContract {
		case openAIEmbeddingAdapter:
			profile := openaicompat.Profile{Origin: configured.Runtime.Endpoint, Descriptor: descriptor,
				ModelInput: modelInput, SecretBinding: configured.CredentialBinding,
				DeploymentEpoch:        configured.Runtime.DeploymentEpoch,
				ProviderRevisionHeader: configured.Runtime.ProviderRevisionHeader,
				RequestTimeout:         configured.Runtime.RequestTimeout.Std(), MaxBatchItems: configured.MaxBatchItems,
				MaxInputBytes: configured.MaxInputBytes, MaxRequestBytes: configured.Runtime.MaxRequestBytes,
				MaxResponseBytes: configured.MaxResponseBytes,
				EgressPolicy: providerEgressPolicy(configured.Runtime.Endpoint, configured.Runtime.AllowedCIDRs,
					configured.Runtime.SPKISHA256, configured.Runtime.ProxyMode, configured.Runtime.ConnectTimeout.Std(),
					configured.Runtime.KeepAlive.Std(), configured.Runtime.TLSHandshakeTimeout.Std())}
			descriptor, profile, err = finalizeOpenAIEmbeddingDescriptor(profile)
			if err == nil {
				profile.Descriptor = descriptor
				provider, err = openaicompat.New(profile, secrets, &http.Client{})
			}
			classify = classifyOpenAIEmbeddingError
		case voyageEmbeddingAdapter:
			provider, descriptor, err = configuredVoyageProvider(configured, modelInput, secrets)
			classify = classifyVoyageEmbeddingError
		default:
			err = errors.New("unsupported embedding runtime adapter")
		}
		if err != nil {
			return embeddingRuntimeBundle{}, fmt.Errorf("configuring embedding runtime %q: %w", name, err)
		}
		if descriptor.Fingerprint != configured.DescriptorFingerprint ||
			descriptor.ID != configured.DescriptorID || descriptor.ModelRevision != configured.Runtime.ModelRevision {
			return embeddingRuntimeBundle{}, fmt.Errorf("configuring embedding runtime %q: descriptor differs from portable binding", name)
		}
		// Every profile is validated above, including its complete provider policy.
		// Chunking and activation may differ without requiring another provider.
		if shared, exists := registered[descriptor.Fingerprint]; exists {
			bundle.providers[name] = shared
			bundle.classifiers[name] = classify
			continue
		}
		runtime, err := processing.NewProviderEmbeddingRuntime(provider, blobs, spoolDirectory, classify)
		if err != nil {
			return embeddingRuntimeBundle{}, err
		}
		if err := bundle.registry.Register(descriptor.Fingerprint, runtime); err != nil {
			return embeddingRuntimeBundle{}, err
		}
		registered[descriptor.Fingerprint] = provider
		bundle.providers[name] = provider
		bundle.classifiers[name] = classify
	}
	return bundle, nil
}

func executableProcessingProfiles(cfg config.Config,
	bundle embeddingRuntimeBundle,
) (map[string]processing.ProfileConfig, error) {
	renditionProviders, renditionDisclosures, err := configureRenditionProviders(cfg)
	if err != nil {
		return nil, err
	}
	profiles := make(map[string]processing.ProfileConfig)
	names := make([]string, 0, len(cfg.ProcessingProfiles))
	for name := range cfg.ProcessingProfiles {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		resolved, err := cfg.ProcessingProfile(name)
		if err != nil {
			return nil, err
		}
		portable := resolved.Document
		if portable.Rendition == nil && len(portable.Embeddings) == 0 {
			continue
		}
		executable := true
		configured := processing.ProfileConfig{Profile: portable,
			EmbeddingProviders:   make(map[string]document.EmbeddingProvider),
			EmbeddingDisclosures: make(map[string]processing.RuntimeDisclosure),
			EmbeddingClassifiers: make(map[string]func(error) (processing.EmbeddingProviderFailure, time.Duration)),
			Tokenizers:           make(map[string]document.Tokenizer)}
		if portable.Rendition != nil {
			configured.RenditionProvider = renditionProviders[portable.Rendition.Name]
			if configured.RenditionProvider == nil {
				continue
			}
			configured.RenditionDisclosure = renditionDisclosures[portable.Rendition.Name]
		}
		for _, binding := range portable.Embeddings {
			provider := bundle.providers[binding.Name]
			classifier := bundle.classifiers[binding.Name]
			if provider == nil || classifier == nil {
				executable = false
				break
			}
			configured.EmbeddingProviders[binding.Name] = provider
			configured.EmbeddingClassifiers[binding.Name] = classifier
			if runtime := cfg.EmbeddingProfiles[binding.Name].Runtime; runtime != nil {
				deployment := runtime.DeploymentEpoch
				if deployment == "" {
					deployment = runtime.ModelRevision
				}
				configured.EmbeddingDisclosures[binding.Name] = processing.RuntimeDisclosure{
					ImmediateProcessor: runtime.AdapterContract,
					UltimateProcessor:  binding.Descriptor.ID,
					Endpoint:           runtime.Endpoint,
					Deployment:         deployment,
					Model:              binding.Model,
					ModelRevision:      runtime.ModelRevision,
				}
			}
			if binding.InputKind == document.EmbeddingInputRenditionChunk {
				tokenizer := configuredEmbeddingTokenizer(binding.Chunk.Tokenizer + "@" + binding.Chunk.TokenizerRevision)
				if tokenizer == nil {
					executable = false
					break
				}
				configured.Tokenizers[binding.Name] = tokenizer
			}
		}
		if executable {
			profiles[name] = configured
		}
	}
	return profiles, nil
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

func finalizeOpenAIEmbeddingDescriptor(profile openaicompat.Profile) (document.EmbeddingDescriptor, openaicompat.Profile, error) {
	temporary, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil {
		return document.EmbeddingDescriptor{}, profile, err
	}
	profile.Descriptor = temporary
	fingerprint, err := openaicompat.PolicyFingerprint(profile)
	if err != nil {
		return document.EmbeddingDescriptor{}, profile, err
	}
	temporary.PolicyFingerprint, temporary.Fingerprint = fingerprint, ""
	final, err := document.NewEmbeddingDescriptor(temporary)
	return final, profile, err
}

func configuredVoyageProvider(profile config.EmbeddingProfileConfig, modelInput document.ModelInputContract,
	secrets environmentCredentialSecrets,
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
		Endpoint: profile.Runtime.Endpoint, EgressPolicy: providerEgressPolicy(profile.Runtime.Endpoint,
			profile.Runtime.AllowedCIDRs, profile.Runtime.SPKISHA256, profile.Runtime.ProxyMode,
			profile.Runtime.ConnectTimeout.Std(), profile.Runtime.KeepAlive.Std(), profile.Runtime.TLSHandshakeTimeout.Std()),
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
	return providerEgressPolicy(runtime.Endpoint, runtime.AllowedCIDRs, runtime.SPKISHA256,
		runtime.ProxyMode, runtime.ConnectTimeout.Std(), runtime.KeepAlive.Std(), runtime.TLSHandshakeTimeout.Std())
}

func providerEgressPolicy(endpoint string, allowedCIDRs, spkiSHA256 []string, proxyMode string,
	connectTimeout, keepAlive, tlsHandshakeTimeout time.Duration,
) providerhttp.EgressPolicy {
	parsed, _ := url.Parse(endpoint)
	port := uint16(443)
	if parsed.Scheme == "http" {
		port = 80
	}
	if parsed.Port() != "" {
		value, _ := strconv.ParseUint(parsed.Port(), 10, 16)
		port = uint16(value)
	}
	prefixes := make([]netip.Prefix, 0, len(allowedCIDRs))
	for _, value := range allowedCIDRs {
		prefix, _ := netip.ParsePrefix(value)
		prefixes = append(prefixes, prefix)
	}
	return providerhttp.EgressPolicy{Scheme: parsed.Scheme, Host: parsed.Hostname(), Port: port,
		AllowedCIDRs: prefixes, ProxyMode: providerhttp.ProxyMode(proxyMode),
		ConnectTimeout: connectTimeout, KeepAlive: keepAlive, TLSHandshakeTimeout: tlsHandshakeTimeout,
		TLS: providerhttp.TLSPolicy{SPKISHA256: spkiSHA256}}
}

func classifyOpenAIEmbeddingError(err error) (processing.EmbeddingProviderFailure, time.Duration) {
	if errors.Is(err, openaicompat.ErrMalformedResponse) {
		return processing.EmbeddingProviderInvalidResponse, 0
	}
	if errors.Is(err, openaicompat.ErrUnauthorized) {
		return processing.EmbeddingProviderAuthorization, 0
	}
	if errors.Is(err, openaicompat.ErrCapacityResponse) {
		return processing.EmbeddingProviderCapacity, 0
	}
	if errors.Is(err, openaicompat.ErrTransientResponse) {
		delay, _ := openaicompat.RetryAfter(err)
		return processing.EmbeddingProviderTransient, delay
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return processing.EmbeddingProviderTransient, 0
	}
	return processing.EmbeddingProviderPermanent, 0
}

func classifyVoyageEmbeddingError(err error) (processing.EmbeddingProviderFailure, time.Duration) {
	if errors.Is(err, voyage.ErrUnauthorized) {
		return processing.EmbeddingProviderAuthorization, 0
	}
	if errors.Is(err, voyage.ErrBatchTooLarge) {
		return processing.EmbeddingProviderCapacity, 0
	}
	if errors.Is(err, voyage.ErrMalformedResponse) {
		return processing.EmbeddingProviderInvalidResponse, 0
	}
	if voyage.IsRetryable(err) {
		delay, _ := voyage.RetryAfter(err)
		return processing.EmbeddingProviderTransient, delay
	}
	return processing.EmbeddingProviderPermanent, 0
}
