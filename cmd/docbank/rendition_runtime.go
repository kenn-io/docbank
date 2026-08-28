package main

import (
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/config"
)

const plaintextRenditionAdapter = "docbank-plaintext-rendition/v1"

func configureRenditionProviders(cfg config.Config) (map[string]document.RenditionProvider, error) {
	providers := make(map[string]document.RenditionProvider)
	for name, configured := range cfg.RenditionProfiles {
		if configured.AdapterContract != plaintextRenditionAdapter {
			continue
		}
		provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: configured.MaxDocumentBytes})
		if err != nil {
			return nil, fmt.Errorf("configuring rendition runtime %q: %w", name, err)
		}
		descriptor := provider.Descriptor()
		if descriptor.ID != configured.DescriptorID || descriptor.Fingerprint != configured.DescriptorFingerprint ||
			string(descriptor.TrustBoundary) != configured.TrustBoundary {
			return nil, fmt.Errorf("configuring rendition runtime %q: descriptor differs from portable binding", name)
		}
		providers[name] = provider
	}
	return providers, nil
}
