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
	profile.MaxTranscriptChars = 0
	profile.Runtime.TotalTimeout = 0
	profile.Runtime.PollInterval = 0
	profile.Runtime.MaxPollAttempts = 0
	profile.RequestedArtifacts = []string{"structured_evidence"}
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
	for _, tc := range []struct {
		name   string
		change func(*RenditionProfileConfig)
		want   string
	}{
		{"descriptor", func(p *RenditionProfileConfig) { p.DescriptorID = vision.ProviderID }, "image understanding requires its own task"},
		{"artifact", func(p *RenditionProfileConfig) {
			p.RequestedArtifacts = []string{string(document.EvidenceArtifactMarkdown)}
		}, "vision requires a withheld filename"},
		{"input limit", func(p *RenditionProfileConfig) { p.MaxDocumentBytes = 16<<20 + 1 }, "image understanding requires its own task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := profile
			tc.change(&changed)
			cfg.RenditionProfiles["primary"] = changed
			require.ErrorContains(t, cfg.Validate(), tc.want)
		})
	}
}
