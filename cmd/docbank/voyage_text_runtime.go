package main

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/internal/config"
)

func voyageTextRuntimeProfile(profile config.EmbeddingProfileConfig, modelInput document.ModelInputContract) voyage.EmbeddingProfile {
	return voyage.EmbeddingProfile{Mode: voyage.EmbeddingModeText,
		DeploymentEpoch: profile.Runtime.DeploymentEpoch,
		Endpoint:        profile.Runtime.Endpoint, EgressPolicy: providerEgressPolicy(profile.Runtime.ProviderEgressConfig),
		Descriptor: configuredEmbeddingDescriptor(profile, modelInput), ModelInput: modelInput, SecretBinding: profile.CredentialBinding,
		RequestTimeout: profile.Runtime.RequestTimeout.Std(), MaxRetries: 1,
		MaxBatchItems: profile.MaxBatchItems, MaxInputBytes: profile.MaxInputBytes,
		MaxRequestBytes: profile.Runtime.MaxRequestBytes, MaxResponseBytes: profile.MaxResponseBytes}
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

func configuredVoyageTextProvider(profile config.EmbeddingProfileConfig, modelInput document.ModelInputContract, secrets environmentCredentialSecrets) (document.EmbeddingProvider, document.EmbeddingDescriptor, error) {
	configured, err := finalizeVoyageEmbeddingDescriptor(voyageTextRuntimeProfile(profile, modelInput))
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	provider, err := voyage.NewEmbeddingProvider(configured, secrets, nil)
	return provider, configured.Descriptor, err
}
