package main

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedlocal"
)

func readLocalText(root string) (config.LocalTextConfig, error) {
	var choice config.LocalTextConfig
	data, err := os.ReadFile(filepath.Join(root, "local-text.json"))
	if errors.Is(err, os.ErrNotExist) {
		return choice, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &choice, json.RejectUnknownMembers(true))
		if err == nil && choice.Version != 1 {
			err = fmt.Errorf("unsupported local-text format version %d", choice.Version)
		}
		if err == nil && (choice.Profile == "" || (choice.Server != "auto" && choice.Server != "local") || ((choice.EmbeddingServer != "" || choice.EmbeddingModel != "" || choice.EmbeddingDimensions != 0) && (choice.EmbeddingServer == "" || choice.EmbeddingModel == "" || choice.EmbeddingDimensions <= 0 || len(choice.Profiles) == 0))) {
			err = errors.New("local-text selection requires a profile and auto or local server")
		}
	}
	return choice, err
}

func saveLocalText(root string, choice config.LocalTextConfig) error {
	choice.Version = 1
	data, err := json.Marshal(choice)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(root, "local-text.json"), data)
}

func localTextSave(root string, restartPending *atomic.Bool, save func(string, config.LocalTextConfig) error, logger *slog.Logger) func(string, string) error {
	return func(name, mode string) error {
		err := save(root, config.LocalTextConfig{Profile: name, Server: mode})
		if err == nil {
			if markerErr := embedlocal.RecordChoice(); markerErr != nil {
				logger.Warn("record shared search by meaning choice", "error", markerErr)
			}
		}
		if err == nil || errors.Is(err, atomicfile.ErrPublished) {
			restartPending.Store(true)
		}
		return err
	}
}

func localTextSaveTarget(root string, restartPending *atomic.Bool, save func(string, config.LocalTextConfig) error, logger *slog.Logger) func([]string, processing.LocalTextTarget) error {
	return func(names []string, target processing.LocalTextTarget) error {
		err := save(root, config.LocalTextConfig{Profile: names[0], Profiles: names, Server: target.Server, EmbeddingServer: target.EmbeddingServer, EmbeddingModel: target.EmbeddingModel, EmbeddingDimensions: target.EmbeddingDimensions})
		if err == nil || errors.Is(err, atomicfile.ErrPublished) {
			restartPending.Store(true)
		}
		return err
	}
}

func inheritedLocalText(profiles map[string]processing.ProfileConfig) (config.LocalTextConfig, error) {
	choice := config.LocalTextConfig{}
	for _, profile := range profiles {
		for _, binding := range profile.Profile.Embeddings {
			if provider := profile.EmbeddingProviders[binding.Name]; provider != nil && binding.InputKind != document.EmbeddingInputImage && provider.Descriptor().SupportsTextQuery {
				return choice, nil
			}
		}
	}
	recorded, err := embedlocal.ChoiceRecorded()
	if err != nil {
		return choice, fmt.Errorf("read shared search by meaning choice: %w", err)
	}
	if !recorded {
		return choice, nil
	}
	return config.LocalTextConfig{Profile: config.LocalTextProfile, Server: "local"}, nil
}

// localTextCandidate moves one compatible recipe, or supplies the plaintext fallback.
func localTextCandidate(cfg config.Config, mode, name string) (config.Config, error) {
	if mode != "auto" && mode != "local" {
		return cfg, processing.ErrProfileNotConfigured
	}
	if name == config.LocalTextProfile {
		name = ""
	}
	candidate := cfg
	candidate.EmbeddingProfiles = maps.Clone(cfg.EmbeddingProfiles)
	candidate.RenditionProfiles = maps.Clone(cfg.RenditionProfiles)
	candidate.RetrievalProfiles = maps.Clone(cfg.RetrievalProfiles)
	candidate.ProcessingProfiles = maps.Clone(cfg.ProcessingProfiles)
	if name != "" {
		recipe, ok := cfg.ProcessingProfiles[name]
		if !ok {
			return cfg, processing.ErrProfileNotConfigured
		}
		textBindings := 0
		for _, binding := range recipe.Embeddings {
			p, err := cfg.EmbeddingProfile(binding)
			if err != nil {
				return cfg, err
			}
			if p.InputKind != string(document.EmbeddingInputRenditionChunk) {
				continue
			}
			p, err = config.LocalTextEmbedding(p, mode)
			if err != nil {
				return cfg, processing.ErrProfileNotConfigured
			}
			candidate.EmbeddingProfiles[binding] = p
			textBindings++
		}
		if textBindings == 0 {
			return cfg, processing.ErrProfileNotConfigured
		}
		candidate.ProcessingProfiles = map[string]config.ProcessingProfileConfig{name: recipe}
		return candidate, nil
	}
	var err error
	candidate, err = candidate.WithLocalText()
	if err != nil {
		return cfg, err
	}
	candidate.ProcessingProfiles = map[string]config.ProcessingProfileConfig{config.LocalTextProfile: candidate.ProcessingProfiles[config.LocalTextProfile]}
	p := candidate.EmbeddingProfiles[config.LocalTextProfile]
	p.Runtime = new(*p.Runtime)
	p.Runtime.ServerMode = mode
	candidate.EmbeddingProfiles[config.LocalTextProfile] = p
	return candidate, nil
}

// localTextServerCandidate overlays one recipe with the reviewed server target.
func localTextServerCandidate(cfg config.Config, target processing.LocalTextTarget, name string) (config.Config, []string, error) {
	candidate := cfg
	candidate.EffectiveProfiles = nil
	candidate.EmbeddingProfiles = maps.Clone(cfg.EmbeddingProfiles)
	candidate.ProcessingProfiles = maps.Clone(cfg.ProcessingProfiles)
	recipe, ok := cfg.ProcessingProfiles[name]
	if !ok && name == config.LocalTextProfile {
		cfg.EmbeddingProfiles = maps.Clone(cfg.EmbeddingProfiles)
		cfg.RenditionProfiles = maps.Clone(cfg.RenditionProfiles)
		cfg.RetrievalProfiles = maps.Clone(cfg.RetrievalProfiles)
		cfg.ProcessingProfiles = maps.Clone(cfg.ProcessingProfiles)
		var err error
		cfg, err = cfg.WithLocalText()
		if err != nil {
			return cfg, nil, err
		}
		return localTextServerCandidate(cfg, target, name)
	}
	if !ok {
		return cfg, nil, processing.ErrProfileNotConfigured
	}
	recipe.Embeddings = append([]string(nil), recipe.Embeddings...)
	var retained []string
	moved := false
	for _, binding := range cfg.ProcessingProfiles[name].Embeddings {
		p, err := cfg.EmbeddingProfile(binding)
		if err != nil {
			return cfg, nil, err
		}
		if p.InputKind == string(document.EmbeddingInputRenditionChunk) && strings.HasPrefix(p.Model, "voyage-context-4") {
			retained = append(retained, name+": contextual text stays with its provider")
			continue
		}
		direct := p.InputKind == string(document.EmbeddingInputOriginalFile) && p.Runtime != nil && p.Runtime.AdapterContract == voyageEmbeddingAdapter
		if direct && !strings.HasPrefix(target.EmbeddingModel, "gemini-embedding-2") {
			retained = append(retained, name+": media stays with its provider for this text-only model")
			continue
		}
		if !direct && p.InputKind != string(document.EmbeddingInputRenditionChunk) {
			continue
		}
		outputName := binding
		if direct {
			outputName += "-kit"
			recipe.Embeddings = append(recipe.Embeddings, outputName)
			retained = append(retained, name+": media outside 20 MiB, six PDF pages, or two minutes stays with its provider")
		}
		p, err = serverEmbedding(p, target, direct, binding)
		if err != nil {
			return cfg, nil, err
		}
		candidate.EmbeddingProfiles[outputName] = p
		moved = true
	}
	if !moved {
		return cfg, retained, processing.ErrProfileNotConfigured
	}
	candidate.ProcessingProfiles = map[string]config.ProcessingProfileConfig{name: recipe}
	return candidate, retained, nil
}

func serverEmbedding(p config.EmbeddingProfileConfig, target processing.LocalTextTarget, direct bool, fallback string) (config.EmbeddingProfileConfig, error) {
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(target.EmbeddingServer+"\x00"+target.EmbeddingModel+"\x00"+fmt.Sprint(target.EmbeddingDimensions))))
	compatibility := target.EmbeddingModel + "/search/v1"
	input := config.EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileCustom), CompatibilityID: compatibility,
		Document: config.EmbeddingModelInputEncoderConfig{Mode: "text", Template: "{{content}}"}, Query: config.EmbeddingModelInputEncoderConfig{Mode: "text", Template: "{{content}}"}}
	if strings.HasPrefix(target.EmbeddingModel, "gemini-embedding-2") {
		input.Document.Template = "title: none | text: {{content}}"
		input.Query.Template = "task: search result | query: {{content}}"
	}
	if target.EmbeddingModel == "nomic-embed-text" {
		input = config.EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileNomic)}
		compatibility = "nomic/search/v1"
	}
	p.Model, p.Dimensions, p.CompatibilityID, p.ModelInput = "", 0, compatibility, input
	p.DescriptorID, p.DocumentFormatter, p.QueryFormatter = openaicompat.ProviderID, openaicompat.DocumentFormatterV1, openaicompat.QueryFormatterV1
	p.Metric, p.Normalization, p.ScalarEncoding = "cosine", "unit_length", "float32"
	p.AuthorizationFingerprint, p.DisclosureFingerprint = identity, identity
	p.CredentialBinding, p.TrustBoundary = "credential:embedding-server", string(document.EmbeddingTrustOperatorNetwork)
	p.MaxBatchItems = 0
	p.Embedder = &embedconfig.Embedder{API: embedconfig.APIKit, BaseURL: target.EmbeddingServer, Model: target.EmbeddingModel, Dims: target.EmbeddingDimensions, FingerprintSalt: identity, TimeoutSeconds: 120, TrustPrivateNetwork: true}
	p.Runtime = &config.EmbeddingRuntimeConfig{AdapterContract: openAIEmbeddingAdapter, Anonymous: true, MaxRequestBytes: 28 << 20,
		ProviderEgressConfig: config.ProviderEgressConfig{AllowedCIDRs: []string{"0.0.0.0/0", "::/0"}, ProxyMode: "disabled", ConnectTimeout: config.Duration(10 * time.Second), KeepAlive: config.Duration(30 * time.Second), TLSHandshakeTimeout: config.Duration(10 * time.Second)}}
	if direct {
		p.InputKind = string(document.EmbeddingInputOriginalFile)
		p.MaxInputBytes = 20 << 20
		p.MaxInputTokens = 0
		p.Chunk = config.EmbeddingChunkConfig{}
		p.Runtime.MediaFallbackBinding = fallback
	}
	c := config.Config{EmbeddingProfiles: map[string]config.EmbeddingProfileConfig{"target": p}}
	resolved, err := c.EmbeddingProfile("target")
	if err != nil {
		return p, err
	}
	c.EmbeddingProfiles["target"] = resolved
	modelInput, err := c.EmbeddingModelInput("target")
	if err != nil {
		return p, err
	}
	profile := openaicompat.Profile{API: embedconfig.APIKit, Origin: resolved.Runtime.Endpoint, Descriptor: config.EmbeddingDescriptor(resolved, modelInput), ModelInput: modelInput, DeploymentEpoch: resolved.Runtime.DeploymentEpoch, RequestTimeout: resolved.Runtime.RequestTimeout.Std(), MaxBatchItems: resolved.MaxBatchItems, MaxInputBytes: resolved.MaxInputBytes, MaxRequestBytes: resolved.Runtime.MaxRequestBytes, MaxResponseBytes: resolved.MaxResponseBytes, EgressPolicy: providerEgressPolicy(resolved.Runtime.ProviderEgressConfig)}
	descriptor, _, err := finalizeOpenAIEmbeddingDescriptor(profile)
	if err != nil {
		return p, err
	}
	// Retain the original source form so resolution can derive all Kit settings again.
	p.DescriptorFingerprint = descriptor.Fingerprint
	return p, nil
}

func localTextStartup(ctx context.Context, managed bool, _ string) error {
	if !managed {
		return nil
	}
	_, err := localEmbeddingOrigin(ctx, embedlocal.TextModel, func(context.Context, int64) (bool, error) { return true, nil })
	return err
}

func projectLocalTextProfile(cfg config.Config, profiles map[string]processing.ProfileConfig, candidate config.Config, name string, selected processing.ProfileConfig) (config.Config, error) {
	// Registries share identical descriptors, so retain unchanged provider objects.
	for _, existing := range profiles {
		if selected.RenditionProvider != nil && existing.RenditionProvider != nil && selected.RenditionProvider.Descriptor().Fingerprint == existing.RenditionProvider.Descriptor().Fingerprint {
			selected.RenditionProvider = existing.RenditionProvider
		}
		for binding, provider := range selected.EmbeddingProviders {
			for _, original := range existing.EmbeddingProviders {
				if provider.Descriptor().Fingerprint == original.Descriptor().Fingerprint {
					selected.EmbeddingProviders[binding] = original
				}
			}
		}
	}
	resolved, err := candidate.ProcessingProfile(name)
	if err != nil {
		return cfg, err
	}
	profiles[name] = selected
	cfg.EffectiveProfiles = maps.Clone(cfg.EffectiveProfiles)
	if cfg.EffectiveProfiles == nil {
		cfg.EffectiveProfiles = make(map[string]config.ResolvedProcessingProfile)
	}
	cfg.EffectiveProfiles[name] = resolved
	cfg.ProcessingProfiles = maps.Clone(cfg.ProcessingProfiles)
	cfg.ProcessingProfiles[name] = candidate.ProcessingProfiles[name]
	return cfg, nil
}

func runLocalText(ctx context.Context, setup *processing.LocalTextSetup, inherited bool, ready *atomic.Bool, logger *slog.Logger) error {
	if inherited {
		active, err := setup.InitializeInherited(ctx)
		if err != nil || !active {
			return err
		}
		ready.Store(true)
	}
	return setup.Backfill(ctx, logger)
}

func localTextStatus(ctx context.Context, profiles map[string]processing.ProfileConfig, service *processing.Service, restartPending, inherited, inheritedReady bool) (api.LocalTextStatus, error) {
	status := api.LocalTextStatus{RestartRequired: restartPending}
	for _, profile := range service.Profiles() {
		if len(profile.QueryEmbeddingBindings) == 0 {
			continue
		}
		if inherited && profile.Name == config.LocalTextProfile {
			active, err := service.ProfileConsentActive(ctx, profile.Name)
			if err != nil {
				return status, err
			}
			if !active || !inheritedReady {
				continue
			}
		}
		binding := profiles[profile.Name].EmbeddingDisclosures[profile.QueryEmbeddingBindings[0]]
		status.Active, status.Provider = true, "configured"
		if endpoint, err := url.Parse(binding.Endpoint); err == nil {
			if endpoint.Scheme == "managed" {
				status.Provider = "local"
			} else if endpoint.Port() == "11434" {
				status.Provider = "ollama"
			}
		}
		return status, nil
	}
	files, err := embedlocal.Inspect(ctx, embedlocal.Options{SkipOllama: true})
	if err != nil {
		return status, fmt.Errorf("inspect local embeddings: %w", err)
	}
	status.DownloadBytes = files.DownloadBytes
	return status, nil
}
