package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/vision"
)

func TestImageUnderstandingRuntimeAllowsTextRendition(t *testing.T) {
	cfg := validRenditionRuntimeConfig(t)
	profile := cfg.RenditionProfiles["primary"]
	profile.AdapterContract = vision.UnderstandingAdapterContract
	profile.Model = "synthetic-vision"
	profile.MaxDocumentBytes = 16 << 20
	profile.MaxResponseBytes = 1 << 20
	profile.RequestedArtifacts = []string{"provider_markdown"}
	profile.DiscloseFilename = false
	profile.Runtime.RequestTimeout = Duration(time.Minute)
	descriptor, err := vision.Descriptor(vision.UnderstandingProviderID, profile.Model, document.RenditionTrustOperatorNetwork)
	require.NoError(t, err)
	profile.DescriptorID, profile.DescriptorFingerprint = descriptor.ID, descriptor.Fingerprint
	cfg.RenditionProfiles["primary"] = profile
	require.NoError(t, cfg.Validate())
	for _, change := range []func(*RenditionProfileConfig){
		func(p *RenditionProfileConfig) { p.RequestedArtifacts = []string{"markdown"} },
		func(p *RenditionProfileConfig) { p.DescriptorID = vision.ProviderID },
		func(p *RenditionProfileConfig) {
			p.RequestedArtifacts = []string{string(document.EvidenceArtifactStructured)}
		},
		func(p *RenditionProfileConfig) { p.DiscloseFilename = true },
		func(p *RenditionProfileConfig) { p.MaxDocumentBytes = 16<<20 + 1 },
	} {
		changed := profile
		change(&changed)
		cfg.RenditionProfiles["primary"] = changed
		require.Error(t, cfg.Validate())
	}
}
