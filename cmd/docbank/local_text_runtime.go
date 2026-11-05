package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/kit/atomicfile"
)

func readLocalText(root string) (config.LocalTextConfig, error) {
	var choice config.LocalTextConfig
	data, err := os.ReadFile(filepath.Join(root, "local-text.json"))
	if errors.Is(err, os.ErrNotExist) {
		return choice, nil
	}
	if err != nil {
		return choice, err
	}
	var version struct {
		Version int `json:"version"`
	}
	if err = json.Unmarshal(data, &version); err != nil {
		return choice, err
	}
	switch version.Version {
	case 1:
		var old struct {
			Version int    `json:"version"`
			Profile string `json:"profile"`
			Server  string `json:"server"`
		}
		err = json.Unmarshal(data, &old, json.RejectUnknownMembers(true))
		choice = config.LocalTextConfig{Version: 1, Profiles: []string{old.Profile}, Server: old.Server}
	case 2:
		err = json.Unmarshal(data, &choice, json.RejectUnknownMembers(true))
	default:
		err = fmt.Errorf("unsupported local-text format version %d", version.Version)
	}
	if err == nil && (len(choice.Profiles) == 0 || slices.Contains(choice.Profiles, "") || (choice.Server != "auto" && choice.Server != "local")) {
		err = errors.New("local-text selection requires profiles and auto or local server")
	}
	return choice, err
}

func saveLocalText(root string, choice config.LocalTextConfig) error {
	choice.Version = 2
	data, err := json.Marshal(choice)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(root, "local-text.json"), data)
}

// localTextCandidate moves compatible recipes, or supplies the plaintext fallback.
func localTextCandidate(cfg config.Config, mode string, names []string) (config.Config, error) {
	if mode != "auto" && mode != "local" {
		return cfg, processing.ErrProfileNotConfigured
	}
	candidate := cfg
	candidate.EmbeddingProfiles = maps.Clone(cfg.EmbeddingProfiles)
	candidate.RenditionProfiles = maps.Clone(cfg.RenditionProfiles)
	candidate.RetrievalProfiles = maps.Clone(cfg.RetrievalProfiles)
	if len(names) == 0 {
		names = []string{config.LocalTextProfile}
	}
	candidate.ProcessingProfiles = make(map[string]config.ProcessingProfileConfig, len(names))
	for _, name := range names {
		if _, configured := cfg.ProcessingProfiles[name]; name == config.LocalTextProfile && !configured {
			fallback, err := candidate.WithLocalText()
			if err != nil {
				return cfg, err
			}
			candidate = fallback
			profile := candidate.EmbeddingProfiles[name]
			profile.Runtime = new(*profile.Runtime)
			profile.Runtime.ServerMode = mode
			candidate.EmbeddingProfiles[name] = profile
			continue
		}
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
		candidate.ProcessingProfiles[name] = recipe
	}
	return candidate, nil
}

func localTextStartup(ctx context.Context, managed bool, _ string) error {
	if !managed {
		return nil
	}
	_, err := localEmbeddingOrigin(ctx)
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
	if cfg.EffectiveProfiles == nil {
		cfg.EffectiveProfiles = make(map[string]config.ResolvedProcessingProfile)
	}
	cfg.EffectiveProfiles[name] = resolved
	cfg.ProcessingProfiles = maps.Clone(cfg.ProcessingProfiles)
	cfg.ProcessingProfiles[name] = candidate.ProcessingProfiles[name]
	return cfg, nil
}
