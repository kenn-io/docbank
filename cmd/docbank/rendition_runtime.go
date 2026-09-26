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
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

const (
	plaintextRenditionAdapter = "docbank-plaintext-rendition/v1"
	epubRenditionAdapter      = "docbank-epub-rendition/v1"
)

type renditionProviderInputs struct {
	profile               docling.ASRProfile
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
		case config.DoclingASRAdapterContract:
			provider, disclosures[name], err = configureDoclingASR(cfg, name, secrets, registered)
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
		if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint ||
			string(descriptor.TrustBoundary) != configured.TrustBoundary {
			return nil, nil, fmt.Errorf("configuring rendition runtime %q: descriptor differs from portable binding", name)
		}
		providers[name] = provider
	}
	return providers, disclosures, nil
}

func configureDoclingASR(cfg config.Config, name string, secrets environmentCredentialSecrets,
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
	profile, err := configuredDoclingASRProfile(configured)
	if err != nil {
		return nil, disclosure, err
	}
	expectedDisclosure := docling.ASRDisclosureFingerprint(profile.Descriptor, profile.Origin,
		configured.DeploymentFingerprint)
	if configured.DisclosureFingerprint != expectedDisclosure {
		return nil, disclosure, errors.New("disclosure fingerprint does not bind the runtime endpoint")
	}
	environmentVariable, ok := secrets.variables[profile.SecretBinding]
	if !ok {
		return nil, disclosure, errors.New("rendition credential binding is not configured")
	}
	egress := providerEgressPolicy(configured.Runtime.ProviderEgressConfig)
	inputs := renditionProviderInputs{profile: profile, egress: egress, credentialEnvironment: environmentVariable}
	disclosure = processing.RuntimeDisclosure{ImmediateProcessor: config.DoclingASRAdapterContract,
		UltimateProcessor: profile.Descriptor.ID, Endpoint: profile.Origin,
		Deployment: configured.DeploymentFingerprint}
	if existing, ok := registered[profile.Descriptor.Fingerprint]; ok {
		if !reflect.DeepEqual(existing.inputs, inputs) {
			return nil, disclosure, errors.New("conflicts with another profile's provider for the same descriptor")
		}
		for fingerprint := range requestFingerprints {
			existing.provider.allowedRenditionRequests[fingerprint] = struct{}{}
		}
		return existing.provider, disclosure, nil
	}
	transport, err := providerhttp.NewTransport(egress, nil)
	if err != nil {
		return nil, disclosure, err
	}
	provider, err := docling.NewASR(profile, secrets, &http.Client{Transport: transport})
	if err != nil {
		return nil, disclosure, err
	}
	bound := &configuredRenditionProvider{RenditionProvider: provider,
		allowedRenditionRequests: requestFingerprints}
	registered[profile.Descriptor.Fingerprint] = registeredRenditionProvider{provider: bound, inputs: inputs}
	return bound, disclosure, nil
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
	runtime := configured.Runtime
	return docling.ASRProfile{
		Origin: runtime.Endpoint, Descriptor: descriptor,
		SecretBinding:  strings.TrimPrefix(configured.CredentialBinding, "credential:"),
		RequestTimeout: runtime.RequestTimeout.Std(), TotalTimeout: runtime.TotalTimeout.Std(),
		PollInterval: runtime.PollInterval.Std(), MaxPollAttempts: runtime.MaxPollAttempts,
		MaxResponseBytes: configured.MaxResponseBytes, MaxDocumentBytes: configured.MaxDocumentBytes,
		MaxTranscriptChars: configured.MaxTranscriptChars,
	}, nil
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
