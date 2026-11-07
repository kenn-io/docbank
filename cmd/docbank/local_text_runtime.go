package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/kit/atomicfile"
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
		if err == nil && (choice.Profile == "" || (choice.Server != "auto" && choice.Server != "local")) {
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

func localTextStartup(ctx context.Context, managed bool, _ string) error {
	if !managed {
		return nil
	}
	_, err := localEmbeddingOrigin(ctx, embedlocal.TextModel)
	if err != nil {
		return fmt.Errorf("embeddings server is not ready; run docbank processing local to install and start it: %w", err)
	}
	return nil
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
	cfg.EffectiveProfiles = map[string]config.ResolvedProcessingProfile{name: resolved}
	cfg.ProcessingProfiles = maps.Clone(cfg.ProcessingProfiles)
	cfg.ProcessingProfiles[name] = candidate.ProcessingProfiles[name]
	return cfg, nil
}
