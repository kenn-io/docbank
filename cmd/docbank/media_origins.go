package main

import (
	"context"
	"fmt"

	"go.kenn.io/docbank/document/capselfhosted"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

func configureMediaOrigins(cfg config.Config) (
	map[string]processing.MediaOriginPolicy, map[string]processing.MediaOriginProbe, error,
) {
	policies := make(map[string]processing.MediaOriginPolicy, len(cfg.MediaOrigins))
	probes := make(map[string]processing.MediaOriginProbe, len(cfg.MediaOrigins))
	secrets := environmentCredentialSecrets{variables: make(map[string]string, len(cfg.CredentialBindings))}
	for name, binding := range cfg.CredentialBindings {
		secrets.variables[name] = binding.EnvironmentVariable
	}
	for name, entry := range cfg.MediaOrigins {
		origin, err := processing.CanonicalRemoteOrigin(entry.Endpoint)
		if err != nil {
			return nil, nil, fmt.Errorf("configuring media origin %q: %w", name, err)
		}
		egressConfig := entry.ProviderEgressConfig
		egressConfig.Endpoint = origin
		egress := providerEgressPolicy(egressConfig)
		deployment := capselfhosted.Deployment{Origin: origin, Egress: egress,
			CredentialBinding: entry.CredentialBinding, DeploymentRevision: entry.DeploymentRevision,
			ProbeTimeout: entry.ProbeTimeout.Std()}
		client, err := capselfhosted.NewClient(deployment, secrets, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("configuring media origin %q: %w", name, err)
		}
		fingerprint, err := deployment.Fingerprint()
		if err != nil {
			return nil, nil, fmt.Errorf("configuring media origin %q: %w", name, err)
		}
		policies[name] = processing.MediaOriginPolicy{
			OriginID: name, Provider: capselfhosted.Provider, ResolverFingerprint: fingerprint,
			IdentityFingerprint:   capselfhosted.ContractFingerprint(capselfhosted.IdentityContract),
			DisclosureFingerprint: capselfhosted.ContractFingerprint(capselfhosted.AdapterContract),
			InputClasses:          []string{"recording_reference"}, ExactOrigin: origin,
			CredentialBinding: entry.CredentialBinding, RecognizePath: capselfhosted.RecognizeSharePath,
			AcquisitionAvailable: false,
		}
		probes[name] = func(ctx context.Context) (processing.MediaOriginEvidence, error) {
			evidence, probeErr := client.Probe(ctx)
			return processing.MediaOriginEvidence{AdapterContract: evidence.AdapterContract,
				DeploymentRevision: evidence.DeploymentRevision, ProbeState: string(evidence.State)}, probeErr
		}
	}
	return policies, probes, nil
}
