package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/vision"
)

func TestExecutableProcessingProfilesRegistersImageUnderstanding(t *testing.T) {
	cfg := visionProcessingConfig(t, "http://127.0.0.1:5001", vision.ProviderID)
	binding := cfg.RenditionProfiles["vision"]
	descriptor, err := vision.Descriptor(vision.UnderstandingProviderID, binding.Model, document.RenditionTrustOperatorNetwork)
	require.NoError(t, err)
	binding.AdapterContract = vision.UnderstandingAdapterContract
	binding.DescriptorID, binding.DescriptorFingerprint = descriptor.ID, descriptor.Fingerprint
	binding.DisclosureFingerprint = vision.DisclosureFingerprint(descriptor, binding.Runtime.Endpoint, binding.DeploymentFingerprint)
	binding.RequestedArtifacts = []string{string(document.EvidenceArtifactStructured)}
	cfg.RenditionProfiles["vision"] = binding
	profile := cfg.ProcessingProfiles["vision"]
	profile.RetainSanitizedMarkdown = true
	cfg.ProcessingProfiles["vision"] = profile
	require.NoError(t, cfg.Validate())
	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	require.Equal(t, vision.UnderstandingProviderID, profiles["vision"].RenditionProvider.Descriptor().ID)
}
