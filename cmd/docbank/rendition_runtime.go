package main

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

const plaintextRenditionAdapter = "docbank-plaintext-rendition/v1"

type renditionProviderInputs struct {
	profile               docling.ASRProfile
	egress                providerhttp.EgressPolicy
	credentialEnvironment string
}

type registeredRenditionProvider struct {
	provider document.RenditionProvider
	inputs   renditionProviderInputs
}

func configureRenditionProviders(cfg config.Config) (
	map[string]document.RenditionProvider, map[string]processing.RuntimeDisclosure, error,
) {
	providers := make(map[string]document.RenditionProvider)
	disclosures := make(map[string]processing.RuntimeDisclosure)
	registered := make(map[string]registeredRenditionProvider)
	secrets := environmentCredentialSecrets{variables: make(map[string]string)}
	for name, binding := range cfg.CredentialBindings {
		secrets.variables["credential:"+name] = binding.EnvironmentVariable
		secrets.variables[name] = binding.EnvironmentVariable
	}
	names := make([]string, 0, len(cfg.RenditionProfiles))
	for name := range cfg.RenditionProfiles {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		configured := cfg.RenditionProfiles[name]
		if configured.AdapterContract != plaintextRenditionAdapter {
			if configured.AdapterContract != config.DoclingASRAdapterContract || configured.Runtime == nil {
				continue
			}
			transcriptChars, err := cfg.RenditionTranscriptChars(name)
			if err != nil {
				return nil, nil, err
			}
			if transcriptChars == 0 {
				continue
			}
			policyFingerprint, err := docling.ASRPolicyFingerprint(transcriptChars)
			if err != nil {
				return nil, nil, fmt.Errorf("configuring rendition runtime %q: %w", name, err)
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
				return nil, nil, fmt.Errorf("configuring rendition runtime %q: %w", name, err)
			}
			if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint ||
				string(descriptor.TrustBoundary) != configured.TrustBoundary {
				return nil, nil, fmt.Errorf("configuring rendition runtime %q: descriptor differs from portable binding", name)
			}
			secretBinding := strings.TrimPrefix(configured.CredentialBinding, "credential:")
			environmentVariable, ok := secrets.variables[secretBinding]
			if !ok {
				return nil, nil, errors.New("rendition credential binding is not configured")
			}
			egress := providerEgressPolicy(configured.Runtime.Endpoint, configured.Runtime.AllowedCIDRs,
				configured.Runtime.SPKISHA256, configured.Runtime.ProxyMode, configured.Runtime.ConnectTimeout.Std(),
				configured.Runtime.KeepAlive.Std(), configured.Runtime.TLSHandshakeTimeout.Std())
			inputs := renditionProviderInputs{profile: docling.ASRProfile{
				Profile: docling.Profile{Origin: configured.Runtime.Endpoint, Descriptor: descriptor,
					SecretBinding: secretBinding, RequestTimeout: configured.Runtime.RequestTimeout.Std(),
					TotalTimeout: configured.Runtime.TotalTimeout.Std(), PollInterval: configured.Runtime.PollInterval.Std(),
					MaxPollAttempts: configured.Runtime.MaxPollAttempts, MaxResponseBytes: configured.MaxResponseBytes,
					MaxDocumentBytes: configured.MaxDocumentBytes},
				MaxTranscriptChars: transcriptChars}, egress: egress,
				credentialEnvironment: environmentVariable}
			disclosure := processing.RuntimeDisclosure{ImmediateProcessor: config.DoclingASRAdapterContract,
				UltimateProcessor: descriptor.ID, Endpoint: configured.Runtime.Endpoint,
				Deployment: configured.DeploymentFingerprint}
			if existing, ok := registered[descriptor.Fingerprint]; ok {
				if !sameRenditionProviderInputs(existing.inputs, inputs) {
					return nil, nil, fmt.Errorf("configuring rendition runtime %q conflicts with another profile's provider for the same descriptor", name)
				}
				providers[name] = existing.provider
				disclosures[name] = disclosure
				continue
			}
			transport, err := providerhttp.NewTransport(egress, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("configuring rendition runtime %q: %w", name, err)
			}
			provider, err := docling.NewASR(inputs.profile, secrets, &http.Client{Transport: transport})
			if err != nil {
				return nil, nil, fmt.Errorf("configuring rendition runtime %q: %w", name, err)
			}
			if provider.Descriptor().ID != descriptor.ID || provider.Descriptor().Fingerprint != descriptor.Fingerprint ||
				provider.Descriptor().TrustBoundary != descriptor.TrustBoundary {
				return nil, nil, fmt.Errorf("configuring rendition runtime %q: provider descriptor differs from portable binding", name)
			}
			registered[descriptor.Fingerprint] = registeredRenditionProvider{provider: provider, inputs: inputs}
			providers[name] = provider
			disclosures[name] = disclosure
			continue
		}
		if configured.AdapterContract != plaintextRenditionAdapter {
			continue
		}
		provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: configured.MaxDocumentBytes})
		if err != nil {
			return nil, nil, fmt.Errorf("configuring rendition runtime %q: %w", name, err)
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

func sameRenditionProviderInputs(left, right renditionProviderInputs) bool {
	return left.credentialEnvironment == right.credentialEnvironment && reflect.DeepEqual(left.profile, right.profile) &&
		left.egress.Scheme == right.egress.Scheme && left.egress.Host == right.egress.Host &&
		left.egress.Port == right.egress.Port && slices.Equal(left.egress.AllowedCIDRs, right.egress.AllowedCIDRs) &&
		left.egress.ProxyMode == right.egress.ProxyMode && left.egress.ConnectTimeout == right.egress.ConnectTimeout &&
		left.egress.KeepAlive == right.egress.KeepAlive && left.egress.TLSHandshakeTimeout == right.egress.TLSHandshakeTimeout &&
		slices.Equal(left.egress.TLS.SPKISHA256, right.egress.TLS.SPKISHA256)
}
