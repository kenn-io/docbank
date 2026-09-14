package media_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
)

func TestTranscriptQualificationRequiresMoreThanContainerMagic(t *testing.T) {
	q := media.TranscriptQualification{Container: "mp4", Codec: "unknown", MediaType: "video/mp4", ProviderBound: true}
	require.Equal(t, "unqualified", media.TranscriptState(q))

	q = media.TranscriptQualification{Container: "wav", Codec: "pcm_s16le", MediaType: "audio/wav",
		Bounded: true, Evidence: "TestMediaWAVTranscriptEndToEnd"}
	require.Equal(t, "provider_required", media.TranscriptState(q))

	q.ProviderBound = true
	require.Equal(t, "qualified", media.TranscriptState(q))

	q.Evidence = ""
	require.Equal(t, "unqualified", media.TranscriptState(q))
}
