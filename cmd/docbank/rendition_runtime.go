package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/epub"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

const (
	plaintextRenditionAdapter = "docbank-plaintext-rendition/v1"
	epubRenditionAdapter      = "docbank-epub-rendition/v1"
)

// Only daemon tests set renditionTestEgress.
var renditionTestEgress struct {
	resolver    providerhttp.Resolver
	roots       *x509.CertPool
	dialContext func(context.Context, string, string) (net.Conn, error)
}

type renditionProviderInputs struct {
	adapterContract                    string
	maxTranscriptChars                 int
	maxDocumentBytes, maxResponseBytes int64
	credentialEnvironment              string
	runtime                            config.RenditionRuntimeConfig
	egress                             providerhttp.EgressPolicy
}

type registeredRenditionProvider struct {
	provider *configuredRenditionProvider
	inputs   renditionProviderInputs
}

type configuredRenditionProvider struct {
	document.RenditionProvider

	allowedRenditionRequests map[string]struct{}
}

func (provider *configuredRenditionProvider) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if _, ok := provider.allowedRenditionRequests[authorization.RenditionRequestFingerprint]; !ok {
		failure, err := document.NewRenditionProviderError(document.RenditionErrorPolicyRejected, 0,
			document.ErrRenditionAuthorizationInvalid)
		if err != nil {
			return document.RenditionResult{}, err
		}
		return document.RenditionResult{}, failure
	}
	return provider.RenditionProvider.Render(ctx, upload, authorization)
}

func configureRenditionProviders(cfg config.Config) (
	map[string]document.RenditionProvider, map[string]processing.RuntimeDisclosure, error,
) {
	providers := make(map[string]document.RenditionProvider)
	disclosures := make(map[string]processing.RuntimeDisclosure)
	registered := make(map[string]registeredRenditionProvider)
	secrets := environmentCredentialSecrets{variables: make(map[string]string)}
	for name, binding := range cfg.CredentialBindings {
		secrets.variables[name] = binding.EnvironmentVariable
	}
	names := make([]string, 0, len(cfg.RenditionProfiles))
	for name := range cfg.RenditionProfiles {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		configured := cfg.RenditionProfiles[name]
		var provider document.RenditionProvider
		var err error
		switch configured.AdapterContract {
		case plaintextRenditionAdapter:
			provider, err = plaintext.New(plaintext.Profile{MaxDocumentBytes: configured.MaxDocumentBytes})
		case epubRenditionAdapter:
			provider, err = epub.New(epub.Profile{MaxDocumentBytes: configured.MaxDocumentBytes, MaxUnits: int64(configured.MaxUnits)})
		case config.DoclingASRAdapterContract, config.DoclingDocumentAdapterContract, config.MistralOCRAdapterContract:
			provider, disclosures[name], err = configureExternalRendition(cfg, name, secrets, registered)
		default:
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("configuring rendition runtime %q: %w", name, err)
		}
		if provider == nil {
			continue
		}
		descriptor := provider.Descriptor()
		// Mistral validates the binding before checking its disclosure, with a fingerprint hint.
		if configured.AdapterContract != config.MistralOCRAdapterContract &&
			(descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint ||
				string(descriptor.TrustBoundary) != configured.TrustBoundary) {
			return nil, nil, fmt.Errorf("configuring rendition runtime %q: descriptor differs from portable binding", name)
		}
		providers[name] = provider
	}
	return providers, disclosures, nil
}

func configureExternalRendition(cfg config.Config, name string, secrets environmentCredentialSecrets,
	registered map[string]registeredRenditionProvider,
) (document.RenditionProvider, processing.RuntimeDisclosure, error) {
	var disclosure processing.RuntimeDisclosure
	configured := cfg.RenditionProfiles[name]
	if configured.Runtime == nil {
		return nil, disclosure, nil
	}
	requestFingerprints, err := configuredRenditionRequestFingerprints(cfg, name)
	if err != nil || len(requestFingerprints) == 0 {
		return nil, disclosure, err
	}
	adapter, err := resolveRenditionAdapter(configured)
	if err != nil {
		return nil, disclosure, err
	}
	descriptor := adapter.descriptor
	expectedDisclosure := adapter.disclosureFingerprint
	if configured.DisclosureFingerprint != expectedDisclosure {
		return nil, disclosure, errors.New("disclosure fingerprint does not bind the runtime endpoint")
	}
	environmentVariable, ok := secrets.variables[adapter.secretBinding]
	if !ok {
		return nil, disclosure, errors.New("rendition credential binding is not configured")
	}
	egress := providerEgressPolicy(configured.Runtime.ProviderEgressConfig)
	runtime := *configured.Runtime
	runtime.CapabilityManifest = "" // Descriptor identity covers the manifest contents.
	runtime.ProviderEgressConfig = config.ProviderEgressConfig{}
	inputs := renditionProviderInputs{runtime: runtime, egress: egress, adapterContract: configured.AdapterContract,
		maxTranscriptChars: configured.MaxTranscriptChars, maxDocumentBytes: configured.MaxDocumentBytes,
		maxResponseBytes: configured.MaxResponseBytes, credentialEnvironment: environmentVariable}
	disclosure = processing.RuntimeDisclosure{ImmediateProcessor: configured.AdapterContract,
		UltimateProcessor: descriptor.ID, Endpoint: configured.Runtime.Endpoint,
		Deployment: configured.DeploymentFingerprint, Model: adapter.model}
	if existing, ok := registered[descriptor.Fingerprint]; ok {
		if !reflect.DeepEqual(existing.inputs, inputs) {
			return nil, disclosure, errors.New("conflicts with another profile's provider for the same descriptor")
		}
		for fingerprint := range requestFingerprints {
			existing.provider.allowedRenditionRequests[fingerprint] = struct{}{}
		}
		return existing.provider, disclosure, nil
	}
	egress.TLS.RootCAs = renditionTestEgress.roots
	egress.DialContext = renditionTestEgress.dialContext
	transport, err := providerhttp.NewTransport(egress, renditionTestEgress.resolver)
	if err != nil {
		return nil, disclosure, err
	}
	provider, err := adapter.newProvider(secrets, &http.Client{Transport: transport})
	if err != nil {
		return nil, disclosure, err
	}
	bound := &configuredRenditionProvider{RenditionProvider: provider,
		allowedRenditionRequests: requestFingerprints}
	registered[descriptor.Fingerprint] = registeredRenditionProvider{provider: bound, inputs: inputs}
	return bound, disclosure, nil
}

type resolvedRenditionAdapter struct {
	descriptor            document.RenditionDescriptor
	secretBinding         string
	model                 string
	disclosureFingerprint string
	newProvider           func(environmentCredentialSecrets, *http.Client) (document.RenditionProvider, error)
}

func resolveRenditionAdapter(configured config.RenditionProfileConfig) (resolvedRenditionAdapter, error) {
	runtime := configured.Runtime
	switch configured.AdapterContract {
	case config.MistralOCRAdapterContract:
		profile, err := configuredMistralProfile(configured)
		if err != nil {
			return resolvedRenditionAdapter{}, err
		}
		return resolvedRenditionAdapter{descriptor: profile.Descriptor, secretBinding: profile.SecretBinding,
			model:                 mistral.DefaultModel,
			disclosureFingerprint: mistral.DisclosureFingerprint(profile.Descriptor, runtime.Endpoint, configured.DeploymentFingerprint),
			newProvider: func(secrets environmentCredentialSecrets, client *http.Client) (document.RenditionProvider, error) {
				return mistral.NewRenditionProvider(profile, secrets, client)
			}}, nil
	case config.DoclingASRAdapterContract:
		profile, err := configuredDoclingASRProfile(configured)
		if err != nil {
			return resolvedRenditionAdapter{}, err
		}
		return resolvedRenditionAdapter{descriptor: profile.Descriptor, secretBinding: profile.SecretBinding,
			disclosureFingerprint: docling.ASRDisclosureFingerprint(profile.Descriptor, runtime.Endpoint, configured.DeploymentFingerprint),
			newProvider: func(secrets environmentCredentialSecrets, client *http.Client) (document.RenditionProvider, error) {
				return docling.NewASR(profile, secrets, client)
			}}, nil
	default:
		profile, err := configuredDoclingProfile(configured)
		if err != nil {
			return resolvedRenditionAdapter{}, err
		}
		return resolvedRenditionAdapter{descriptor: profile.Descriptor, secretBinding: profile.SecretBinding,
			disclosureFingerprint: docling.DocumentDisclosureFingerprint(profile.Descriptor, runtime.Endpoint, configured.DeploymentFingerprint),
			newProvider: func(secrets environmentCredentialSecrets, client *http.Client) (document.RenditionProvider, error) {
				return docling.New(profile, secrets, client)
			}}, nil
	}
}

func configuredDoclingProfile(configured config.RenditionProfileConfig) (docling.Profile, error) {
	descriptor, err := docling.DocumentDescriptor(document.RenditionTrustBoundary(configured.TrustBoundary))
	if err != nil {
		return docling.Profile{}, err
	}
	if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint {
		return docling.Profile{}, errors.New("descriptor differs from portable binding")
	}
	return configuredDoclingTransportProfile(configured, descriptor), nil
}

func configuredDoclingTransportProfile(configured config.RenditionProfileConfig, descriptor document.RenditionDescriptor) docling.Profile {
	runtime := configured.Runtime
	return docling.Profile{
		Origin: runtime.Endpoint, Descriptor: descriptor,
		SecretBinding:  strings.TrimPrefix(configured.CredentialBinding, "credential:"),
		RequestTimeout: runtime.RequestTimeout.Std(), TotalTimeout: runtime.TotalTimeout.Std(),
		PollInterval: runtime.PollInterval.Std(), MaxPollAttempts: runtime.MaxPollAttempts,
		MaxResponseBytes: configured.MaxResponseBytes, MaxDocumentBytes: configured.MaxDocumentBytes,
	}
}

func configuredDoclingASRProfile(configured config.RenditionProfileConfig) (docling.ASRProfile, error) {
	policyFingerprint, err := docling.ASRPolicyFingerprint(configured.MaxTranscriptChars)
	if err != nil {
		return docling.ASRProfile{}, err
	}
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "docling.serve-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: policyFingerprint,
		TrustBoundary:     document.RenditionTrustBoundary(configured.TrustBoundary),
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "audio", MediaType: "audio/mpeg", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
	})
	if err != nil {
		return docling.ASRProfile{}, err
	}
	if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint ||
		string(descriptor.TrustBoundary) != configured.TrustBoundary {
		return docling.ASRProfile{}, errors.New("descriptor differs from portable binding")
	}
	return docling.ASRProfile{Profile: configuredDoclingTransportProfile(configured, descriptor),
		MaxTranscriptChars: configured.MaxTranscriptChars}, nil
}

func configuredRenditionRequestFingerprints(cfg config.Config, renditionName string) (map[string]struct{}, error) {
	fingerprints := make(map[string]struct{})
	for name, configured := range cfg.ProcessingProfiles {
		if configured.Rendition != renditionName {
			continue
		}
		resolved, err := cfg.ProcessingProfile(name)
		if err != nil {
			return nil, err
		}
		_, derived, err := document.CanonicalProfile(resolved.Document)
		if err != nil {
			return nil, fmt.Errorf("processing profile %q: %w", name, err)
		}
		fingerprints[derived.RenditionRequest] = struct{}{}
	}
	return fingerprints, nil
}

func configuredMistralProfile(configured config.RenditionProfileConfig) (mistral.Profile, error) {
	runtime := configured.Runtime
	manifest, err := loadCapabilityManifest(runtime.CapabilityManifest, "mistral", mistral.DecodeCapabilityManifest)
	if err != nil {
		return mistral.Profile{}, err
	}
	policy, err := configured.MistralPolicy()
	if err != nil {
		return mistral.Profile{}, err
	}
	descriptor, err := mistral.DocumentDescriptor(policy, manifest)
	if err != nil {
		return mistral.Profile{}, err
	}
	if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint ||
		string(descriptor.TrustBoundary) != configured.TrustBoundary {
		return mistral.Profile{}, fmt.Errorf("descriptor differs from portable binding; expected descriptor_fingerprint = %q", descriptor.Fingerprint)
	}
	return mistral.Profile{Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: strings.TrimPrefix(configured.CredentialBinding, "credential:"), Timeout: runtime.RequestTimeout.Std()}, nil
}
