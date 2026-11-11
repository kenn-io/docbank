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
	t.Run("image larger than provider limit", func(t *testing.T) {
		tooLarge := profile
		tooLarge.MaxDocumentBytes = 16<<20 + 1
		cfg.RenditionProfiles["primary"] = tooLarge
		require.Error(t, cfg.Validate())
		cfg.RenditionProfiles["primary"] = profile
	})
	profile.DescriptorID = vision.ProviderID
	cfg.RenditionProfiles["primary"] = profile
	require.ErrorContains(t, cfg.Validate(), "image understanding requires its own task")
}
