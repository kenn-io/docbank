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
	"go.kenn.io/docbank/document/embeddingbridge"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/upload"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/kit/secretref"
)

const (
	openAIEmbeddingAdapter   = "docbank-openai-compatible-embeddings/v1"
	voyageEmbeddingAdapter   = "docbank-voyage-embeddings/v1"
	bridgeEmbeddingAdapterV2 = "docbank-standard-embedding-bridge/v2"
)

func recoverEmbeddingRuntimeSpool(ctx context.Context, spoolDirectory string) error {
	_, err := upload.RecoverStale(ctx, spoolDirectory)
	return err
}

type environmentCredentialSecrets struct{ variables map[string]string }

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

// embeddingCredentialSecret resolves the configured Kit source for each call,
// so file/env rotation does not change the immutable credential binding.
type embeddingCredentialSecret struct {
	binding string
	ref     secretref.Ref
}

func (resolver embeddingCredentialSecret) ResolveSecret(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if name != resolver.binding {
		return "", errors.New("embedding credential binding is unavailable")
	}
	secret, err := resolver.ref.Resolve()
	if err != nil {
		return "", errors.New("embedding credential is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return secret.Value, nil
}

type embeddingRuntimeBundle struct {
	providers   map[string]document.EmbeddingProvider
	classifiers map[string]func(error) (processing.EmbeddingProviderFailure, time.Duration)
}

func configureEmbeddingRuntimeBundle(cfg config.Config) (embeddingRuntimeBundle, error) {
	bundle := embeddingRuntimeBundle{
		providers:   make(map[string]document.EmbeddingProvider),
		classifiers: make(map[string]func(error) (processing.EmbeddingProviderFailure, time.Duration))}
	registered := make(map[string]document.EmbeddingProvider)
	secrets := environmentCredentialSecrets{variables: make(map[string]string)}
	for name, binding := range cfg.CredentialBindings {
		portable := "credential:" + name
		secrets.variables[portable] = binding.EnvironmentVariable
	}
	for name := range cfg.EmbeddingProfiles {
		configured, err := cfg.EmbeddingProfile(name)
		if err != nil {
			return embeddingRuntimeBundle{}, err
		}
		if configured.Runtime == nil {
			continue
		}
		if _, ok := secrets.variables[configured.CredentialBinding]; !ok && configured.Embedder == nil && !config.ManagedEmbedding(configured) {
			return embeddingRuntimeBundle{}, errors.New("embedding credential binding is not configured")
		}
		modelInput, err := cfg.EmbeddingModelInput(name)
		if err != nil {
			return embeddingRuntimeBundle{}, err
		}
		descriptor := config.EmbeddingDescriptor(configured, modelInput)
		var provider document.EmbeddingProvider
		var classify func(error) (processing.EmbeddingProviderFailure, time.Duration)
		switch configured.Runtime.AdapterContract {
		case bridgeEmbeddingAdapterV2:
			profile := embeddingbridge.Profile{Origin: configured.Runtime.Endpoint, Descriptor: descriptor, SecretBinding: configured.CredentialBinding,
				EgressPolicy: providerEgressPolicy(configured.Runtime.ProviderEgressConfig), RequestTimeout: configured.Runtime.RequestTimeout.Std(),
				MaxBatchItems: configured.MaxBatchItems, MaxInputBytes: configured.MaxInputBytes, MaxRequestBytes: configured.Runtime.MaxRequestBytes, MaxResponseBytes: configured.MaxResponseBytes}
			descriptor.PolicyFingerprint, err = embeddingbridge.PolicyFingerprint(profile)
			if err == nil {
				descriptor, err = document.NewEmbeddingDescriptor(descriptor)
			}
			if err == nil {
				profile.Descriptor = descriptor
				provider, err = embeddingbridge.New(profile, secrets, nil, &http.Client{})
			}
			classify = classifyBridgeEmbeddingError
		case openAIEmbeddingAdapter:
			if config.ManagedEmbedding(configured) {
				profile, profileErr := config.ManagedEmbeddingProfile(configured, localEmbeddingOrigin)
				if profileErr != nil {
					return bundle, profileErr
				}
				descriptor = profile.Descriptor
				provider, err = openaicompat.New(profile, nil, &http.Client{})
				classify = classifyOpenAIEmbeddingError
				break
			}
			var resolver openaicompat.SecretResolver = secrets
			binding := configured.CredentialBinding
			if configured.Embedder != nil {
				if !configured.Embedder.APIKey.IsZero() {
					resolver = embeddingCredentialSecret{binding: binding, ref: configured.Embedder.APIKey}
				} else if _, exists := secrets.variables[binding]; !exists {
					return embeddingRuntimeBundle{}, fmt.Errorf("configuring embedding runtime %q: credential source is not configured", name)
				}
			}
			profile := openaicompat.Profile{Origin: configured.Runtime.Endpoint, Descriptor: descriptor,
				ModelInput: modelInput, SecretBinding: binding,
				DeploymentEpoch:        configured.Runtime.DeploymentEpoch,
				ProviderRevisionHeader: configured.Runtime.ProviderRevisionHeader,
				RequestTimeout:         configured.Runtime.RequestTimeout.Std(), MaxBatchItems: configured.MaxBatchItems,
				MaxInputBytes: configured.MaxInputBytes, MaxRequestBytes: configured.Runtime.MaxRequestBytes,
				MaxResponseBytes: configured.MaxResponseBytes,
				EgressPolicy:     providerEgressPolicy(configured.Runtime.ProviderEgressConfig)}
			descriptor, profile, err = finalizeOpenAIEmbeddingDescriptor(profile)
			if err == nil {
				profile.Descriptor = descriptor
				provider, err = openaicompat.New(profile, resolver, &http.Client{})
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
	rerankingRuntimes, err := configureRerankingProviders(cfg)
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
			EmbeddingSimilarityFloors: make(map[string]float64),
			EmbeddingProviders:        make(map[string]document.EmbeddingProvider),
			EmbeddingDisclosures:      make(map[string]processing.RuntimeDisclosure),
			EmbeddingClassifiers:      make(map[string]func(error) (processing.EmbeddingProviderFailure, time.Duration)),
			Tokenizers:                make(map[string]document.Tokenizer)}
		if runtime, ok := rerankingRuntimes[name]; ok {
			configured = applyRerankingRuntime(configured, runtime)
		}
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
			bindingConfig, err := cfg.EmbeddingProfile(binding.Name)
			if err != nil {
				return nil, err
			}
			if runtime := bindingConfig.Runtime; runtime != nil {
				if runtime.MinQuerySimilarity != nil {
					configured.EmbeddingSimilarityFloors[binding.Name] = *runtime.MinQuerySimilarity
				}
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
			if config.ManagedEmbedding(bindingConfig) {
				disclosure := configured.EmbeddingDisclosures[binding.Name]
				disclosure.Endpoint = "managed://kit-embed/" + bindingConfig.Runtime.ServerMode
				if bindingConfig.Runtime.ServerMode == "" {
					disclosure.Endpoint = "managed://kit-embed/auto"
				}
				configured.EmbeddingDisclosures[binding.Name] = disclosure
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
	configured := voyageRuntimeProfile(profile, modelInput)
	if profile.InputKind != string(document.EmbeddingInputRenditionChunk) {
		policy, manifest, err := voyageDirectFilePolicy(profile)
		if err != nil {
			return nil, document.EmbeddingDescriptor{}, err
		}
		configured.Mode, configured.Policy, configured.CapabilityManifest = voyage.EmbeddingModeDirectFile, policy, manifest
	}
	configured, err := finalizeVoyageEmbeddingDescriptor(configured)
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	provider, err := voyage.NewEmbeddingProvider(configured, secrets, nil)
	return provider, configured.Descriptor, err
}

// voyageRuntimeProfile builds the retained-text profile. Original-file
// profiles replace the mode and add their capability policy.
func voyageRuntimeProfile(profile config.EmbeddingProfileConfig, modelInput document.ModelInputContract) voyage.EmbeddingProfile {
	return voyage.EmbeddingProfile{Mode: voyage.EmbeddingModeText, DeploymentEpoch: profile.Runtime.DeploymentEpoch,
		Endpoint: profile.Runtime.Endpoint, EgressPolicy: providerEgressPolicy(profile.Runtime.ProviderEgressConfig),
		Descriptor: config.EmbeddingDescriptor(profile, modelInput), ModelInput: modelInput, SecretBinding: profile.CredentialBinding,
		RequestTimeout: profile.Runtime.RequestTimeout.Std(), MaxRetries: 1,
		MaxBatchItems: profile.MaxBatchItems, MaxInputBytes: profile.MaxInputBytes,
		MaxRequestBytes: profile.Runtime.MaxRequestBytes, MaxResponseBytes: profile.MaxResponseBytes}
}

func voyageDirectFilePolicy(profile config.EmbeddingProfileConfig) (voyage.Policy, voyage.CapabilityManifest, error) {
	manifest, err := loadCapabilityManifest(profile.Runtime.CapabilityManifest, "voyage", voyage.DecodeCapabilityManifest)
	if err != nil {
		return voyage.Policy{}, voyage.CapabilityManifest{}, err
	}
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: profile.Model, Dimension: profile.Dimensions,
		Media:         media.Policy{MaxBytes: profile.MaxInputBytes, AllowStill: true, AllowVideo: true},
		MaxBatchItems: profile.MaxBatchItems, MaxRequestBytes: profile.Runtime.MaxRequestBytes,
		MaxResponseBytes: profile.MaxResponseBytes})
	return policy, manifest, err
}

func loadCapabilityManifest[T any](path, provider string, decode func(io.Reader) (T, error)) (T, error) {
	var zero T
	file, err := os.Open(path)
	if err != nil {
		return zero, fmt.Errorf("%s capability manifest is unavailable", provider)
	}
	manifest, decodeErr := decode(file)
	closeErr := file.Close()
	if decodeErr != nil || closeErr != nil {
		return zero, errors.Join(decodeErr, closeErr)
	}
	return manifest, nil
}

func finalizeVoyageEmbeddingDescriptor(profile voyage.EmbeddingProfile) (voyage.EmbeddingProfile, error) {
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil {
		return profile, err
	}
	profile.Descriptor = descriptor
	fingerprint, err := voyage.EmbeddingPolicyFingerprint(profile)
	if err != nil {
		return profile, err
	}
	descriptor.PolicyFingerprint, descriptor.Fingerprint = fingerprint, ""
	profile.Descriptor, err = document.NewEmbeddingDescriptor(descriptor)
	return profile, err
}

// providerEgressPolicy converts config after Config.Validate has checked it.
func providerEgressPolicy(runtime config.ProviderEgressConfig) providerhttp.EgressPolicy {
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
		AllowedCIDRs: prefixes, ProxyMode: providerhttp.ProxyMode(runtime.ProxyMode),
		ConnectTimeout: runtime.ConnectTimeout.Std(), KeepAlive: runtime.KeepAlive.Std(), TLSHandshakeTimeout: runtime.TLSHandshakeTimeout.Std(),
		// Omitted pins and an explicit empty list both mean no pins.
		TLS: providerhttp.TLSPolicy{SPKISHA256: append([]string(nil), runtime.SPKISHA256...)}}
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

func localEmbeddingOrigin(ctx context.Context) (string, error) {
	opts := embedlocal.Options{SkipOllama: true}
	origin, err := embedlocal.Endpoint(ctx, opts)
	if err == nil {
		return strings.TrimSuffix(origin, "/v1"), nil
	}
	if !errors.Is(err, embedlocal.ErrNotFound) {
		return "", fmt.Errorf("discover local embeddings: %w", err)
	}
	dir, err := embedlocal.DefaultDir()
	if err != nil {
		return "", fmt.Errorf("find local embeddings directory: %w", err)
	}
	// No Confirm: Kit may reuse installed files, but cannot download here.
	installation, err := embedlocal.Install(ctx, embedlocal.InstallOptions{DataDir: dir})
	if err != nil {
		return "", fmt.Errorf("reuse local embeddings installation: %w", err)
	}
	opts.DataDir, opts.Executable = installation.DataDir, installation.Executable
	origin, err = embedlocal.Endpoint(ctx, opts)
	if err != nil {
		return "", fmt.Errorf("start local embeddings: %w", err)
	}
	return strings.TrimSuffix(origin, "/v1"), nil
}

func classifyBridgeEmbeddingError(err error) (processing.EmbeddingProviderFailure, time.Duration) {
	switch embeddingbridge.Category(err) {
	case embeddingbridge.ErrorAuthentication:
		return processing.EmbeddingProviderAuthorization, 0
	case embeddingbridge.ErrorCapacity:
		return processing.EmbeddingProviderCapacity, 0
	case embeddingbridge.ErrorMalformedResponse:
		return processing.EmbeddingProviderInvalidResponse, 0
	case embeddingbridge.ErrorTransient, embeddingbridge.ErrorAmbiguousSubmission:
		return processing.EmbeddingProviderTransient, 0
	default:
		return processing.EmbeddingProviderPermanent, 0
	}
}
