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
	profile.MaxUnits = 4
	profile.RequestedArtifacts = []string{"provider_markdown"}
	profile.DiscloseFilename = false
	profile.Runtime.RequestTimeout = Duration(time.Minute)
	descriptor, err := vision.Descriptor(vision.UnderstandingProviderID, profile.Model, document.RenditionTrustOperatorNetwork)
	require.NoError(t, err)
	profile.DescriptorID, profile.DescriptorFingerprint = descriptor.ID, descriptor.Fingerprint
	cfg.RenditionProfiles["primary"] = profile
	embedding := cfg.EmbeddingProfiles["semantic"]
	embedding.Chunk.Formatter = document.RenditionChunkHeadingsFormatter
	cfg.EmbeddingProfiles["semantic"] = embedding
	require.NoError(t, cfg.Validate())
	withoutRuntime := profile
	withoutRuntime.Runtime = nil
	withoutRuntime.MaxUnits = 3
	cfg.RenditionProfiles["primary"] = withoutRuntime
	require.ErrorContains(t, cfg.Validate(), "at least four units")
	cfg.RenditionProfiles["primary"] = profile
	embedding.Chunk.Formatter = "chunk/v1"
	cfg.EmbeddingProfiles["semantic"] = embedding
	require.ErrorContains(t, cfg.Validate(), "headings formatter")
	embedding.Chunk.Formatter = document.RenditionChunkHeadingsFormatter
	cfg.EmbeddingProfiles["semantic"] = embedding
	for _, change := range []func(*RenditionProfileConfig){
		func(p *RenditionProfileConfig) { p.RequestedArtifacts = []string{"markdown"} },
		func(p *RenditionProfileConfig) { p.DescriptorID = vision.ProviderID },
		func(p *RenditionProfileConfig) {
			p.RequestedArtifacts = []string{string(document.EvidenceArtifactStructured)}
		},
		func(p *RenditionProfileConfig) { p.DiscloseFilename = true },
		func(p *RenditionProfileConfig) { p.MaxDocumentBytes = 16<<20 + 1 },
		func(p *RenditionProfileConfig) { p.MaxUnits = 1 },
		func(p *RenditionProfileConfig) { p.MaxUnits = 2 },
		func(p *RenditionProfileConfig) { p.MaxUnits = 3 },
	} {
		changed := profile
		change(&changed)
		cfg.RenditionProfiles["primary"] = changed
		require.Error(t, cfg.Validate())
	}
}
