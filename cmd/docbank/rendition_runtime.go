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
	visionProfile         vision.Profile
	profile               docling.Profile
	adapterContract       string
	maxTranscriptChars    int
	egress                providerhttp.EgressPolicy
	credentialEnvironment string
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
	if err := provider.authorize(authorization); err != nil {
		return document.RenditionResult{}, err
	}
	return provider.RenditionProvider.Render(ctx, upload, authorization)
}

func (provider *configuredRenditionProvider) authorize(authorization document.RenditionAuthorization) error {
	if _, ok := provider.allowedRenditionRequests[authorization.RenditionRequestFingerprint]; !ok {
		failure, err := document.NewRenditionProviderError(document.RenditionErrorPolicyRejected, 0,
			document.ErrRenditionAuthorizationInvalid)
		if err != nil {
			return err
		}
		return failure
	}
	return nil
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
		case vision.AdapterContract:
			provider, disclosures[name], err = configureVision(cfg, name, secrets, registered)
		case config.DoclingASRAdapterContract, config.DoclingDocumentAdapterContract:
			provider, disclosures[name], err = configureDocling(cfg, name, secrets, registered)
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
		UltimateProcessor: profile.Descriptor.ID, Endpoint: profile.Origin,
		Deployment: configured.DeploymentFingerprint}
	provider, err := registerRenditionProvider(registered, profile.Descriptor.Fingerprint, inputs, requestFingerprints, func(client *http.Client) (document.RenditionProvider, error) {
		if configured.AdapterContract == config.DoclingASRAdapterContract {
			return docling.NewASR(docling.ASRProfile{Profile: profile, MaxTranscriptChars: configured.MaxTranscriptChars}, secrets, client)
		}
		return docling.New(profile, secrets, client)
	})
	return provider, disclosure, err
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

func configureVision(cfg config.Config, name string, secrets environmentCredentialSecrets, registered map[string]registeredRenditionProvider) (document.RenditionProvider, processing.RuntimeDisclosure, error) {
	configured := cfg.RenditionProfiles[name]
	if configured.Runtime == nil {
		return nil, processing.RuntimeDisclosure{}, nil
	}
	runtime := configured.Runtime
	profile := vision.Profile{Origin: runtime.Endpoint, Model: configured.Model, TrustBoundary: document.RenditionTrustBoundary(configured.TrustBoundary), SecretBinding: strings.TrimPrefix(configured.CredentialBinding, "credential:"), RequestTimeout: runtime.RequestTimeout.Std(), MaxResponseBytes: configured.MaxResponseBytes}
	environmentVariable, ok := secrets.variables[profile.SecretBinding]
	if !ok {
		return nil, processing.RuntimeDisclosure{}, errors.New("vision credential binding is not configured")
	}
	descriptor, err := vision.Descriptor(profile.Model, profile.TrustBoundary)
	if err != nil {
		return nil, processing.RuntimeDisclosure{}, err
	}
	if configured.DisclosureFingerprint != vision.DisclosureFingerprint(descriptor, profile.Origin, configured.DeploymentFingerprint) {
		return nil, processing.RuntimeDisclosure{}, errors.New("vision disclosure fingerprint does not bind runtime endpoint")
	}
	requests, err := configuredRenditionRequestFingerprints(cfg, name)
	if err != nil {
		return nil, processing.RuntimeDisclosure{}, err
	}
	egress := providerEgressPolicy(runtime.ProviderEgressConfig)
	inputs := renditionProviderInputs{visionProfile: profile, adapterContract: vision.AdapterContract, egress: egress, credentialEnvironment: environmentVariable}
	disclosure := processing.RuntimeDisclosure{ImmediateProcessor: vision.AdapterContract, UltimateProcessor: descriptor.ID, Endpoint: profile.Origin, Deployment: configured.DeploymentFingerprint, Model: profile.Model}
	provider, err := registerRenditionProvider(registered, descriptor.Fingerprint, inputs, requests, func(client *http.Client) (document.RenditionProvider, error) {
		return vision.New(profile, secrets, client)
	})
	return provider, disclosure, err
}

func registerRenditionProvider(registered map[string]registeredRenditionProvider, descriptor string, inputs renditionProviderInputs, requests map[string]struct{}, create func(*http.Client) (document.RenditionProvider, error)) (document.RenditionProvider, error) {
	if existing, ok := registered[descriptor]; ok {
		if !reflect.DeepEqual(existing.inputs, inputs) {
			return nil, errors.New("conflicts with another profile's provider for the same descriptor")
		}
		for fingerprint := range requests {
			existing.provider.allowedRenditionRequests[fingerprint] = struct{}{}
		}
		return existing.provider, nil
	}
	transport, err := providerhttp.NewTransport(inputs.egress, nil)
	if err != nil {
		return nil, err
	}
	provider, err := create(&http.Client{Transport: transport})
	if err != nil {
		return nil, err
	}
	bound := &configuredRenditionProvider{RenditionProvider: provider, allowedRenditionRequests: requests}
	registered[descriptor] = registeredRenditionProvider{provider: bound, inputs: inputs}
	return bound, nil
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
