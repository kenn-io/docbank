package docling

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mediatranscript"
)

// The dedicated remote CI lane runs a real pinned Docling Serve ASR deployment.
// Normal unit suites use protocol fixtures and explicitly skip this live probe.
func TestDoclingASRRealAudioQualification(t *testing.T) {
	endpoint := os.Getenv("DOCBANK_CI_ASR_URL")
	if endpoint == "" {
		t.Skip("requires isolated remote CI Docling ASR service")
	}
	source, err := os.ReadFile("../../.superpowers/asr-qualification/asr-spoken.m4a")
	require.NoError(t, err)
	for _, test := range []struct {
		name, mediaType, filename, artifactName string
		source                                  []byte
	}{
		{name: "M4A AAC", mediaType: "audio/mp4", filename: "asr-spoken.m4a", artifactName: "media-transcript.json", source: source},
		{name: "MP3", mediaType: "audio/mpeg", filename: "asr-spoken.mp3", artifactName: "mp3-transcript.json", source: asrSpokenMP3},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newASRFixture(t, test.mediaType, test.filename, test.source)
			client, err := NewASR(ASRProfile{
				Origin: endpoint, Descriptor: fixture.descriptor, RequestTimeout: 30 * time.Second, TotalTimeout: 5 * time.Minute,
				PollInterval: time.Second, MaxPollAttempts: 300, MaxResponseBytes: 1 << 20, MaxDocumentBytes: 1 << 20,
				MaxTranscriptChars: 100_000, AllowM4A: test.mediaType == "audio/mp4"}, nil, http.DefaultClient)
			require.NoError(t, err)
			result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
			require.NoError(t, err, "synthetic ASR cause: %v", errors.Unwrap(err))
			require.Len(t, result.Artifacts, 1)
			artifact, err := mediatranscript.Unmarshal(result.Artifacts[0].Payload)
			require.NoError(t, err)
			var text strings.Builder
			for _, segment := range artifact.Segments {
				text.WriteString(segment.Text)
				text.WriteByte(' ')
			}
			// ASR is uncertain: the service default tiny model hears the first noun
			// imperfectly. Check several known speech terms without claiming verbatim accuracy.
			require.Contains(t, strings.ToLower(text.String()), "delivery")
			require.Contains(t, strings.ToLower(text.String()), "arrives")
			require.Contains(t, strings.ToLower(text.String()), "friday")
			require.NoError(t, os.WriteFile("../../.superpowers/asr-qualification/"+test.artifactName, result.Artifacts[0].Payload, 0600))
		})
	}
}
