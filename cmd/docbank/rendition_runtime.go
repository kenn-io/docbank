package main

import (
	"context"
	"errors"
	"fmt"
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
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

const (
	plaintextRenditionAdapter = "docbank-plaintext-rendition/v1"
	epubRenditionAdapter      = "docbank-epub-rendition/v1"
)

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
	pdfPasswordsFile         string
	pdfPasswords             bool
}

func (provider *configuredRenditionProvider) PDFPasswords() ([]string, bool, error) {
	if !provider.pdfPasswords {
		return nil, false, nil
	}
	if provider.pdfPasswordsFile == "" {
		return nil, true, nil
	}
	passwords, err := config.ReadPDFPasswords(provider.pdfPasswordsFile)
	return passwords, true, err
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

func (provider *configuredRenditionProvider) Probe(ctx context.Context) error {
	probe, ok := provider.RenditionProvider.(interface {
		Probe(ctx context.Context) error
	})
	if !ok {
		return errors.New("provider has no vision probe")
	}
	return probe.Probe(ctx)
}

func configureRenditionProviders(cfg config.Config) (
	map[string]document.RenditionProvider, map[string]processing.RuntimeDisclosure, error,
) {
	providers := make(map[string]document.RenditionProvider)
	disclosures := make(map[string]processing.RuntimeDisclosure)
	registered := make(map[string]registeredRenditionProvider)
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
		case vision.AdapterContract:
			provider, disclosures[name], err = configureVision(cfg, name, registered)
		case config.DoclingASRAdapterContract, config.DoclingDocumentAdapterContract, config.MistralOCRAdapterContract:
			provider, disclosures[name], err = configureExternalRendition(cfg, name, registered)
		default:
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("configuring rendition runtime %q: %w", name, err)
		}
		if provider == nil {
			continue
		}
		if configured.AdapterContract == plaintextRenditionAdapter || configured.AdapterContract == epubRenditionAdapter {
			descriptor := provider.Descriptor()
			if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint ||
				string(descriptor.TrustBoundary) != configured.TrustBoundary {
				return nil, nil, fmt.Errorf("configuring rendition runtime %q: descriptor differs from portable binding", name)
			}
		}
		providers[name] = provider
	}
	return providers, disclosures, nil
}

func renditionProviderClient(cfg config.Config, egress config.ProviderEgressConfig) (environmentCredentialSecrets, *http.Client, error) {
	secrets := environmentCredentialSecrets{variables: make(map[string]string)}
	for name, binding := range cfg.CredentialBindings {
		secrets.variables[name] = binding.EnvironmentVariable
	}
	transport, err := providerhttp.NewTransport(providerEgressPolicy(egress), nil)
	return secrets, &http.Client{Transport: transport}, err
}

func configureExternalRendition(cfg config.Config, name string,
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
	for _, binding := range []struct{ field, configured, expected string }{
		{"descriptor_id", configured.DescriptorID, descriptor.ID},
		{"descriptor_fingerprint", configured.DescriptorFingerprint, descriptor.Fingerprint},
		{"trust_boundary", configured.TrustBoundary, string(descriptor.TrustBoundary)},
	} {
		if binding.configured != binding.expected {
			return nil, disclosure, fmt.Errorf("descriptor differs from portable binding; expected %s = %q", binding.field, binding.expected)
		}
	}
	if configured.DisclosureFingerprint != adapter.disclosureFingerprint {
		return nil, disclosure, errors.New("disclosure fingerprint does not bind the runtime endpoint")
	}
	credential, ok := cfg.CredentialBindings[adapter.secretBinding]
	if !ok {
		return nil, disclosure, errors.New("rendition credential binding is not configured")
	}
	egress := providerEgressPolicy(configured.Runtime.ProviderEgressConfig)
	runtime := *configured.Runtime
	runtime.CapabilityManifest = "" // Descriptor identity covers the manifest contents.
	runtime.ProviderEgressConfig = config.ProviderEgressConfig{}
	inputs := renditionProviderInputs{runtime: runtime, egress: egress, adapterContract: configured.AdapterContract,
		maxTranscriptChars: configured.MaxTranscriptChars, maxDocumentBytes: configured.MaxDocumentBytes,
		maxResponseBytes: configured.MaxResponseBytes, credentialEnvironment: credential.EnvironmentVariable}
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
	secrets, client, err := renditionProviderClient(cfg, configured.Runtime.ProviderEgressConfig)
	if err != nil {
		return nil, disclosure, err
	}
	provider, err := adapter.newProvider(secrets, client)
	if err != nil {
		return nil, disclosure, err
	}
	bound := &configuredRenditionProvider{RenditionProvider: provider,
		allowedRenditionRequests: requestFingerprints, pdfPasswordsFile: cfg.Media.PDFPasswordsFile, pdfPasswords: adapter.pdfPasswords}
	registered[descriptor.Fingerprint] = registeredRenditionProvider{provider: bound, inputs: inputs}
	return bound, disclosure, nil
}

type resolvedRenditionAdapter struct {
	pdfPasswords          bool
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
			pdfPasswords:          true,
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
			pdfPasswords:          true,
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
	return mistral.Profile{Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: strings.TrimPrefix(configured.CredentialBinding, "credential:"), Timeout: runtime.RequestTimeout.Std()}, nil
}

func configureVision(cfg config.Config, name string, registered map[string]registeredRenditionProvider) (document.RenditionProvider, processing.RuntimeDisclosure, error) {
	var disclosure processing.RuntimeDisclosure
	configured := cfg.RenditionProfiles[name]
	if configured.Runtime == nil {
		return nil, disclosure, nil
	}
	runtime := configured.Runtime
	profile := vision.Profile{Origin: runtime.Endpoint, Model: configured.Model, TrustBoundary: document.RenditionTrustBoundary(configured.TrustBoundary), SecretBinding: strings.TrimPrefix(configured.CredentialBinding, "credential:"), RequestTimeout: runtime.RequestTimeout.Std(), MaxResponseBytes: configured.MaxResponseBytes}
	credential, ok := cfg.CredentialBindings[profile.SecretBinding]
	if !ok {
		return nil, disclosure, errors.New("vision credential binding is not configured")
	}
	descriptor, err := vision.Descriptor(profile.Model, profile.TrustBoundary)
	if err != nil {
		return nil, disclosure, err
	}
	if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint || string(descriptor.TrustBoundary) != configured.TrustBoundary {
		return nil, disclosure, errors.New("descriptor differs from portable binding")
	}
	if configured.DisclosureFingerprint != vision.DisclosureFingerprint(descriptor, profile.Origin, configured.DeploymentFingerprint) {
		return nil, disclosure, errors.New("vision disclosure fingerprint does not bind runtime endpoint")
	}
	requestFingerprints, err := configuredRenditionRequestFingerprints(cfg, name)
	if err != nil {
		return nil, disclosure, err
	}
	egress := providerEgressPolicy(runtime.ProviderEgressConfig)
	storedRuntime := *configured.Runtime
	storedRuntime.CapabilityManifest = ""
	storedRuntime.ProviderEgressConfig = config.ProviderEgressConfig{}
	inputs := renditionProviderInputs{runtime: storedRuntime, egress: egress, adapterContract: vision.AdapterContract,
		maxResponseBytes: configured.MaxResponseBytes, credentialEnvironment: credential.EnvironmentVariable}
	disclosure = processing.RuntimeDisclosure{ImmediateProcessor: vision.AdapterContract, UltimateProcessor: descriptor.ID, Endpoint: profile.Origin, Deployment: configured.DeploymentFingerprint, Model: profile.Model}
	if existing, ok := registered[descriptor.Fingerprint]; ok {
		if !reflect.DeepEqual(existing.inputs, inputs) {
			return nil, disclosure, errors.New("conflicts with another profile's provider for the same descriptor")
		}
		for fingerprint := range requestFingerprints {
			existing.provider.allowedRenditionRequests[fingerprint] = struct{}{}
		}
		return existing.provider, disclosure, nil
	}
	secrets, client, err := renditionProviderClient(cfg, runtime.ProviderEgressConfig)
	if err != nil {
		return nil, disclosure, err
	}
	provider, err := vision.New(profile, secrets, client)
	if err != nil {
		return nil, disclosure, err
	}
	bound := &configuredRenditionProvider{RenditionProvider: provider, allowedRenditionRequests: requestFingerprints}
	registered[descriptor.Fingerprint] = registeredRenditionProvider{provider: bound, inputs: inputs}
	return bound, disclosure, nil
}
