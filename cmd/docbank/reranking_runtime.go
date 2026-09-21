package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/retrieval/cohere"
	"go.kenn.io/docbank/internal/retrieval/zeroentropy"
)

type rerankingRuntime struct {
	provider      retrieval.RerankingProvider
	profile       retrieval.RerankingProfile
	disclosure    processing.RuntimeDisclosure
	deadline      time.Duration
	failurePolicy retrieval.ProviderFailurePolicy
}

func configureRerankingProviders(cfg config.Config) (map[string]rerankingRuntime, error) {
	runtimes := make(map[string]rerankingRuntime)
	secrets := environmentCredentialSecrets{variables: make(map[string]string, len(cfg.CredentialBindings))}
	for name, binding := range cfg.CredentialBindings {
		secrets.variables["credential:"+name] = binding.EnvironmentVariable
	}
	for name, processingProfile := range cfg.ProcessingProfiles {
		configured := processingProfile.Reranking
		if configured == nil {
			continue
		}
		if _, ok := secrets.variables[configured.CredentialBinding]; !ok {
			return nil, fmt.Errorf("reranking profile %q credential binding is not configured", name)
		}
		revision := configured.ModelRevision
		if revision == "" {
			revision = "v1"
		}
		epoch := configured.CompatibilityEpoch
		if epoch == "" {
			epoch = revision
		}
		maxTotalExcerptBytes := int64(configured.CandidateCount) * int64(configured.ExcerptBytes)
		base := retrieval.RerankingProfile{ID: name, MaxCandidates: configured.CandidateCount,
			MaxExcerptBytes: configured.ExcerptBytes}
		var provider retrieval.RerankingProvider
		var fingerprint string
		switch configured.Provider {
		case "zeroentropy":
			profile := zeroentropy.Profile{ID: name, Model: configured.Model,
				CompatibilityEpoch: epoch, ModelRevision: revision,
				SecretBinding: configured.CredentialBinding, Latency: zeroentropy.LatencyAuto,
				RequestTimeout: configured.Deadline.Std(), MaxCandidates: configured.CandidateCount,
				MaxExcerptBytes: configured.ExcerptBytes, MaxTotalExcerptBytes: maxTotalExcerptBytes,
				EgressPolicy: providerEgressPolicy(configured.ProviderEgressConfig)}
			client, err := zeroentropy.New(profile, secrets, nil, &http.Client{})
			if err != nil {
				return nil, fmt.Errorf("configuring reranking profile %q: %w", name, err)
			}
			provider, fingerprint = client, client.PolicyFingerprint()
		case "cohere":
			profile := cohere.Profile{ID: name, Model: cohere.Model(configured.Model),
				CompatibilityEpoch: epoch, ModelRevision: revision,
				SecretBinding: configured.CredentialBinding, RequestTimeout: configured.Deadline.Std(),
				MaxCandidates: configured.CandidateCount, MaxExcerptBytes: configured.ExcerptBytes,
				MaxTotalExcerptBytes: maxTotalExcerptBytes,
				EgressPolicy:         providerEgressPolicy(configured.ProviderEgressConfig)}
			client, err := cohere.New(profile, secrets, nil, &http.Client{})
			if err != nil {
				return nil, fmt.Errorf("configuring reranking profile %q: %w", name, err)
			}
			provider, fingerprint = client, client.PolicyFingerprint()
		default:
			return nil, errors.New("unsupported reranking provider")
		}
		runtimes[name] = rerankingRuntime{provider: provider, profile: base,
			deadline: configured.Deadline.Std(), failurePolicy: retrieval.ProviderFailurePolicy(configured.FailurePolicy),
			disclosure: processing.RuntimeDisclosure{
				ImmediateProcessor: "docbank-" + configured.Provider + "-rerank",
				UltimateProcessor:  configured.Provider,
				Endpoint:           configured.Endpoint,
				Deployment:         fingerprint,
				Model:              configured.Model,
				ModelRevision:      revision,
				MetadataClasses:    []string{"query_text", "candidate_excerpt", "candidate_identity", "evidence_reference"},
			}}
	}
	return runtimes, nil
}

func applyRerankingRuntime(profile processing.ProfileConfig, runtime rerankingRuntime) processing.ProfileConfig {
	profile.RerankingProvider = runtime.provider
	profile.RerankingProfile = runtime.profile
	profile.RerankingDisclosure = runtime.disclosure
	profile.RerankingDeadline = runtime.deadline
	profile.RerankingFailurePolicy = runtime.failurePolicy
	return profile
}
