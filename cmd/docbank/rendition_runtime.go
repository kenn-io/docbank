package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
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

type renditionProviderInputs struct {
	profile               any
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
		if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint ||
			string(descriptor.TrustBoundary) != configured.TrustBoundary {
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
	var profile any
	var mistralProfile mistral.Profile
	var doclingProfile docling.Profile
	var descriptor document.RenditionDescriptor
	var secretBinding, model string
	if configured.AdapterContract == config.MistralOCRAdapterContract {
		mistralProfile, err = configuredMistralProfile(configured)
		if err != nil {
			return nil, disclosure, err
		}
		profile, descriptor, secretBinding = mistralProfile, mistralProfile.Descriptor, mistralProfile.SecretBinding
		model = mistral.DefaultModel
	} else {
		doclingProfile, err = configuredDoclingProfile(configured)
		if err != nil {
			return nil, disclosure, err
		}
		profile, descriptor, secretBinding = doclingProfile, doclingProfile.Descriptor, doclingProfile.SecretBinding
	}
	expectedDisclosure := renditionDisclosureFingerprint(configured.AdapterContract, descriptor, configured.Runtime.Endpoint, configured.DeploymentFingerprint)
	if configured.DisclosureFingerprint != expectedDisclosure {
		return nil, disclosure, errors.New("disclosure fingerprint does not bind the runtime endpoint")
	}
	environmentVariable, ok := secrets.variables[secretBinding]
	if !ok {
		return nil, disclosure, errors.New("rendition credential binding is not configured")
	}
	egress := providerEgressPolicy(configured.Runtime.ProviderEgressConfig)
	inputs := renditionProviderInputs{profile: profile, adapterContract: configured.AdapterContract,
		maxTranscriptChars: configured.MaxTranscriptChars, egress: egress, credentialEnvironment: environmentVariable}
	disclosure = processing.RuntimeDisclosure{ImmediateProcessor: configured.AdapterContract,
		UltimateProcessor: descriptor.ID, Endpoint: configured.Runtime.Endpoint,
		Deployment: configured.DeploymentFingerprint, Model: model}
	if existing, ok := registered[descriptor.Fingerprint]; ok {
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
	var provider document.RenditionProvider
	switch configured.AdapterContract {
	case config.MistralOCRAdapterContract:
		provider, err = mistral.NewRenditionProvider(mistralProfile, secrets, &http.Client{Transport: transport})
	case config.DoclingASRAdapterContract:
		provider, err = docling.NewASR(docling.ASRProfile{Profile: doclingProfile, MaxTranscriptChars: configured.MaxTranscriptChars}, secrets, &http.Client{Transport: transport})
	default:
		provider, err = docling.New(doclingProfile, secrets, &http.Client{Transport: transport})
	}
	if err != nil {
		return nil, disclosure, err
	}
	bound := &configuredRenditionProvider{RenditionProvider: provider,
		allowedRenditionRequests: requestFingerprints}
	registered[descriptor.Fingerprint] = registeredRenditionProvider{provider: bound, inputs: inputs}
	return bound, disclosure, nil
}

func configuredDoclingProfile(configured config.RenditionProfileConfig) (docling.Profile, error) {
	if configured.AdapterContract == config.DoclingASRAdapterContract {
		profile, err := configuredDoclingASRProfile(configured)
		return profile.Profile, err
	}
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

func renditionDisclosureFingerprint(contract string, descriptor document.RenditionDescriptor, endpoint, deployment string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{contract, descriptor.ID, descriptor.Fingerprint, endpoint, deployment}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func configuredMistralProfile(configured config.RenditionProfileConfig) (mistral.Profile, error) {
	runtime := configured.Runtime
	file, err := os.Open(runtime.CapabilityManifest)
	if err != nil {
		return mistral.Profile{}, errors.New("mistral capability manifest is unavailable")
	}
	manifest, decodeErr := mistral.DecodeCapabilityManifest(file)
	closeErr := file.Close()
	if decodeErr != nil || closeErr != nil {
		return mistral.Profile{}, errors.Join(decodeErr, closeErr)
	}
	normalize, err := document.NewNormalizePolicy(25_000_000)
	if err != nil {
		return mistral.Profile{}, err
	}
	policy, err := mistral.NewPolicy(mistral.PolicyConfig{Region: mistral.RegionEU, Model: mistral.DefaultModel,
		Retention: runtime.Retention, Training: runtime.Training, MaxDocumentBytes: configured.MaxDocumentBytes,
		MaxResponseBytes: configured.MaxResponseBytes, MaxUnits: configured.MaxUnits,
		ExtractHeader: true, ExtractFooter: true, NormalizePolicy: normalize})
	if err != nil {
		return mistral.Profile{}, err
	}
	if runtime.Endpoint != strings.TrimSuffix(policy.Values().Endpoint, "/v1/ocr") {
		return mistral.Profile{}, errors.New("mistral runtime endpoint differs from policy origin")
	}
	fingerprint, err := policy.Fingerprint(manifest)
	if err != nil {
		return mistral.Profile{}, err
	}
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "mistral.ocr-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: fingerprint, TrustBoundary: document.RenditionTrustHostedProvider,
		SupportedFormats: []document.RenditionFormatCapability{{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile}},
		ReturnsMarkdown:  true, ReturnsStructured: true,
	})
	if err != nil {
		return mistral.Profile{}, err
	}
	if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint {
		return mistral.Profile{}, fmt.Errorf("descriptor differs from portable binding; expected descriptor_fingerprint = %q", descriptor.Fingerprint)
	}
	return mistral.Profile{Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: strings.TrimPrefix(configured.CredentialBinding, "credential:"), Timeout: runtime.RequestTimeout.Std()}, nil
}
